/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package storage

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// postgresLocalKEKConfig selects postgres with its DSN in Secret pg-dsn/dsn and a local KEK in
// pg-kek/kek.
func postgresLocalKEKConfig() Config {
	return Config{Provider: Provider{Postgres: &PostgresProvider{
		DSNSecretRef: SecretKeyRef{Name: "pg-dsn", Key: "dsn"},
		Encryption:   &EnvelopeEncryption{LocalKEKSecretRef: &SecretKeyRef{Name: "pg-kek", Key: "kek"}},
	}}}
}

// postgresDeps serves the DSN and KEK Secrets (kekKey names the KEK's key) and counts start-up retries.
func postgresDeps(t *testing.T, dsn, kekKey string, retries *atomic.Int32) Deps {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-dsn", Namespace: "cred-system"},
			Data:       map[string][]byte{"dsn": []byte(dsn)},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-kek", Namespace: "cred-system"},
			Data:       map[string][]byte{kekKey: bytes.Repeat([]byte{0x22}, 32)},
		},
	).Build()
	deps := testDeps(c)
	deps.StoreRetry = func(error, time.Duration) { retries.Add(1) }
	return deps
}

// closedPortDSN points at a local port nothing listens on, so every connection is refused.
func closedPortDSN(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return fmt.Sprintf("postgres://u:p@127.0.0.1:%d/grants?sslmode=disable&connect_timeout=1", port)
}

// TestPostgresBackend_WaitsForAnUnreachableDatabase: a credential Postgres that does not answer yet is
// retried within the start-up budget rather than failing the token service on the first attempt.
func TestPostgresBackend_WaitsForAnUnreachableDatabase(t *testing.T) {
	t.Setenv("CONTROLPLANE_STARTUP_TIMEOUT", "2500ms")
	var retries atomic.Int32
	deps := postgresDeps(t, closedPortDSN(t), "kek", &retries)

	_, err := BackendFor(context.Background(), postgresLocalKEKConfig(), deps)
	if err == nil {
		t.Fatal("an unreachable database built a backend")
	}
	if retries.Load() < 1 {
		t.Fatalf("the unreachable database was not retried (err: %v)", err)
	}
	if !strings.Contains(err.Error(), "still unreachable after 2.5s") {
		t.Fatalf("err = %v, want the bounded wait to say it gave up", err)
	}
}

// TestPostgresBackend_ConfigurationErrorsRefuseAtOnce: a missing Secret key or a malformed DSN is
// configuration, not a slow dependency, so it refuses without a single retry, and the error names the
// Secret to fix.
func TestPostgresBackend_ConfigurationErrorsRefuseAtOnce(t *testing.T) {
	t.Setenv("CONTROLPLANE_STARTUP_TIMEOUT", "1m")
	missingDSNSecret := postgresLocalKEKConfig()
	missingDSNSecret.Provider.Postgres.DSNSecretRef.Name = "absent"
	cases := map[string]struct {
		cfg               Config
		dsn, kekKey, want string
	}{
		"DSN Secret missing":         {missingDSNSecret, closedPortDSN(t), "kek", "get secret cred-system/absent"},
		"KEK Secret without its key": {postgresLocalKEKConfig(), closedPortDSN(t), "other", `secret cred-system/pg-kek has no key "kek"`},
		"malformed DSN":              {postgresLocalKEKConfig(), "host=127.0.0.1 port=notaport", "kek", "cred-system/pg-dsn is not usable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var retries atomic.Int32
			start := time.Now()
			_, err := BackendFor(context.Background(), tc.cfg, postgresDeps(t, tc.dsn, tc.kekKey, &retries))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if n := retries.Load(); n != 0 {
				t.Fatalf("a configuration error was retried %d times", n)
			}
			if el := time.Since(start); el > 5*time.Second {
				t.Fatalf("a configuration error took %s to refuse", el)
			}
		})
	}
}
