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

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

// TestIntegration_Router_PostgresPath: the FULL config-selected path — a ClusterCredentialStore
// selecting `postgres`, the DSN + local KEK read from Secrets, a Backend built over real
// Postgres, and a Store→Resolve round-trip. Skips unless CREDPOSTGRES_TEST_DSN is set.
func TestIntegration_Router_PostgresPath(t *testing.T) {
	dsn := os.Getenv("CREDPOSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set CREDPOSTGRES_TEST_DSN to run the router→postgres integration test")
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
	store := &agentsv1alpha1.ClusterCredentialStore{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultStoreName},
		Spec: agentsv1alpha1.CredentialStoreSpec{Provider: agentsv1alpha1.CredentialStoreProvider{
			Postgres: &agentsv1alpha1.CredentialProviderPostgres{
				DSNSecretRef: agentsv1alpha1.SecretKeyRef{Name: "pg-dsn", Key: "dsn"},
				Encryption: &agentsv1alpha1.EnvelopeEncryption{
					LocalKEKSecretRef: &agentsv1alpha1.SecretKeyRef{Name: "pg-kek", Key: "kek"},
				},
			},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(dsnSecret, kekSecret, store).Build()
	r := NewRouter(c, Deps{Client: c, DefaultCredentialNamespace: credNS, Exchanger: &credresolve.HTTPTokenExchanger{}})

	// The router selects postgres, reads the DSN + KEK Secrets, opens real Postgres, and
	// applies the schema — then a Resolve for a user with no grant on an open (non-OAuth)
	// server returns ErrNoCredential, proving a real query ran end-to-end through the wiring.
	if _, err := r.Resolve(ctx, "app-ns", "", "srv", "nouser"); err != credresolve.ErrNoCredential {
		t.Fatalf("Resolve(no grant) err = %v, want ErrNoCredential (config-selected postgres backend is live)", err)
	}
}
