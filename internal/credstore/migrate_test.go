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
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/credpostgres"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

func legacyGrantSecret(credNS, ns, server, userHash, token string) client.Object {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        credresolve.SecretName(server, userHash, ""),
			Namespace:   credNS,
			Labels:      credresolve.SecretLabels(server, userHash, ns, ""),
			Annotations: map[string]string{credresolve.AnnGrantServerURL: "https://mcp/" + server},
		},
		Data: credresolve.SecretData(
			credresolve.OAuthConfig{TokenEndpoint: "https://as/token", ClientID: "cid"},
			credresolve.Tokens{AccessToken: token, RefreshToken: "r-" + token, ExpiresAt: time.Now().Add(time.Hour)},
		),
	}
}

// TestMigrate_K8sToPostgres: a backfill lifts existing k8s-Secret grants into the Postgres
// backend, and each resolves back through the target — no connected account is lost.
func TestMigrate_K8sToPostgres(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const credNS = "cred-system"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		legacyGrantSecret(credNS, "team-a", "srv", "uhA", "tok-a"),
		legacyGrantSecret(credNS, "team-b", "srv", "uhB", "tok-b"),
	).Build()

	source := credresolve.NewK8sBackend(credresolve.K8sBackendConfig{Client: c, CredentialNamespace: credNS})
	sealer, _ := credpostgres.NewLocalSealer(make([]byte, 32))
	target, _ := credpostgres.NewBackend(credpostgres.BackendConfig{Storage: credpostgres.NewMemStore(), Sealer: sealer})

	n, err := Migrate(ctx, source, target)
	if err != nil || n != 2 {
		t.Fatalf("Migrate = (%d, %v), want 2 migrated", n, err)
	}
	for _, tc := range []struct{ ns, uh, want string }{{"team-a", "uhA", "tok-a"}, {"team-b", "uhB", "tok-b"}} {
		cred, err := target.Resolve(ctx, tc.ns, "", "srv", tc.uh)
		if err != nil || cred.Value != tc.want {
			t.Fatalf("post-migrate Resolve %s = (%+v, %v), want %s", tc.ns, cred, err, tc.want)
		}
	}
}

type stubResolver struct {
	cred        credresolve.Credential
	err         error
	revokeCalls int
}

func (s *stubResolver) Resolve(context.Context, string, string, string, string) (credresolve.Credential, error) {
	return s.cred, s.err
}

func (s *stubResolver) Revoke(context.Context, string, string, string, string) error {
	s.revokeCalls++
	return nil
}

// TestDualRead_FallbackOnMiss: a resolve during migration finds a grant wherever it lives —
// primary hit wins; a primary not-found falls back to the legacy backend; revoke hits both.
func TestDualRead_FallbackOnMiss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Primary MISS (consent-required) + fallback HIT → the fallback's token, no consent error.
	primary := &stubResolver{err: credresolve.ErrConsentRequired}
	fallback := &stubResolver{cred: credresolve.Credential{Kind: credresolve.KindBearer, Value: "legacy-tok"}}
	dr := NewDualRead(primary, fallback)
	if cred, err := dr.Resolve(ctx, "ns", "", "srv", "uh"); err != nil || cred.Value != "legacy-tok" {
		t.Fatalf("miss→fallback Resolve = (%+v, %v), want legacy-tok", cred, err)
	}

	// Primary HIT → the primary wins, fallback untouched.
	primary2 := &stubResolver{cred: credresolve.Credential{Kind: credresolve.KindBearer, Value: "new-tok"}}
	fallback2 := &stubResolver{err: fmt.Errorf("should not be called")}
	if cred, err := NewDualRead(primary2, fallback2).Resolve(ctx, "ns", "", "srv", "uh"); err != nil || cred.Value != "new-tok" {
		t.Fatalf("primary-hit Resolve = (%+v, %v), want new-tok", cred, err)
	}

	// Revoke hits BOTH backends.
	if err := dr.Revoke(ctx, "ns", "", "srv", "uh"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if primary.revokeCalls != 1 || fallback.revokeCalls != 1 {
		t.Fatalf("revoke reached primary=%d fallback=%d, want 1 each", primary.revokeCalls, fallback.revokeCalls)
	}
}
