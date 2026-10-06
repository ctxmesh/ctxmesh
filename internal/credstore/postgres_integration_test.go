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

package credstore

import (
	"bytes"
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

// TestIntegration_BackendFor_PostgresPath: the FULL configured path — CREDENTIAL_BACKEND=postgres
// parsed from the environment, the DSN + local KEK read from Secrets, a Backend built over real
// Postgres, and a Resolve against it. Skips unless CREDPOSTGRES_TEST_DSN is set.
func TestIntegration_BackendFor_PostgresPath(t *testing.T) {
	dsn := os.Getenv("CREDPOSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set CREDPOSTGRES_TEST_DSN to run the configured-postgres integration test")
	}
	ctx := context.Background()
	const credNS = "cred-system"

	dsnSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-dsn", Namespace: credNS},
		Data:       map[string][]byte{"dsn": []byte(dsn)},
	}
	kekSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-kek", Namespace: credNS},
		Data:       map[string][]byte{"kek": bytes.Repeat([]byte{0x22}, 32)},
	}
	cfg, err := ConfigFromEnv([]string{
		EnvBackend + "=postgres",
		EnvPostgresDSNSecretName + "=pg-dsn",
		EnvPostgresDSNSecretKey + "=dsn",
		EnvPostgresLocalKEKSecretName + "=pg-kek",
		EnvPostgresLocalKEKSecretKey + "=kek",
	})
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(dsnSecret, kekSecret).Build()
	b, err := BackendFor(ctx, cfg, Deps{Client: c, DefaultCredentialNamespace: credNS, Exchanger: &credresolve.HTTPTokenExchanger{}})
	if err != nil {
		t.Fatalf("BackendFor: %v", err)
	}

	// The configured backend read the DSN + KEK Secrets, opened real Postgres and applied the
	// schema — a Resolve for a user with no grant on an open (non-OAuth) server returns
	// ErrNoCredential, proving a real query ran end-to-end through the wiring.
	if _, err := b.Resolve(ctx, "app-ns", "", "srv", "nouser"); err != credresolve.ErrNoCredential {
		t.Fatalf("Resolve(no grant) err = %v, want ErrNoCredential (config-selected postgres backend is live)", err)
	}
}
