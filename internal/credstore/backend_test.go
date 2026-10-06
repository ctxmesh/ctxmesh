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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	return s
}

func testDeps(c client.Client) Deps {
	return Deps{Client: c, DefaultCredentialNamespace: "cred-system", Exchanger: &credresolve.HTTPTokenExchanger{}}
}

func kubernetesConfig() Config {
	return Config{Provider: Provider{Kubernetes: &KubernetesProvider{}}}
}

// TestBackendFor_FailsClosed: kubernetes builds a real resolver; a remote provider without mtls and a
// postgres provider without encryption fail closed (never a plaintext dial, never plaintext storage);
// a config with no provider is an error, never a default.
func TestBackendFor_FailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	deps := testDeps(c)

	b, err := BackendFor(ctx, kubernetesConfig(), deps)
	if err != nil || b == nil {
		t.Fatalf("kubernetes BackendFor = (%v, %v), want a backend", b, err)
	}

	_, err = BackendFor(ctx, Config{Provider: Provider{Remote: &RemoteProvider{Endpoint: "https://x:8443"}}}, deps)
	if err == nil || !strings.Contains(err.Error(), "mtls") {
		t.Errorf("remote-without-mtls err = %v, want an mtls-required error", err)
	}

	_, err = BackendFor(ctx, Config{Provider: Provider{Postgres: &PostgresProvider{
		DSNSecretRef: SecretKeyRef{Name: "p", Key: "dsn"},
	}}}, deps)
	if err == nil || !strings.Contains(err.Error(), "encryption") {
		t.Errorf("postgres-without-encryption err = %v, want an encryption-required error", err)
	}

	if _, err = BackendFor(ctx, Config{}, deps); err == nil {
		t.Error("an empty config built a backend; it must be an error")
	}
}

// TestBackendFor_KubernetesWriteReadRoundTrip: the SPI write path — StoreGrant on the configured
// kubernetes backend persists a grant Secret in the credential namespace, and the same backend
// resolves it back.
func TestBackendFor_KubernetesWriteReadRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	b, err := BackendFor(ctx, kubernetesConfig(), testDeps(c))
	if err != nil {
		t.Fatalf("BackendFor: %v", err)
	}
	w, ok := b.(credresolve.GrantWriter)
	if !ok {
		t.Fatal("the kubernetes backend must support writes")
	}

	g := credresolve.Grant{
		Tokens:    credresolve.Tokens{AccessToken: "tok-1", ExpiresAt: time.Now().Add(time.Hour)},
		Config:    credresolve.OAuthConfig{TokenEndpoint: "https://as/token", ClientID: "cid"},
		ServerURL: "https://mcp.example/mcp",
	}
	if err := w.StoreGrant(ctx, "app-ns", "", "srv", "uh", g); err != nil {
		t.Fatalf("StoreGrant: %v", err)
	}
	gns, gname := credresolve.SecretCoordinates("cred-system", "app-ns", "srv", "uh", "")
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: gns, Name: gname}, &sec); err != nil {
		t.Fatalf("grant Secret not written: %v", err)
	}
	cred, err := b.Resolve(ctx, "app-ns", "", "srv", "uh")
	if err != nil || cred.Value != "tok-1" {
		t.Fatalf("Resolve after StoreGrant = (%+v, %v), want tok-1", cred, err)
	}
}
