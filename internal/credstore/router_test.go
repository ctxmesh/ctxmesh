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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add agents scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	return s
}

func k8sStore(name, ns string) client.Object {
	spec := agentsv1alpha1.CredentialStoreSpec{
		Provider: agentsv1alpha1.CredentialStoreProvider{
			Kubernetes: &agentsv1alpha1.CredentialProviderKubernetes{},
		},
	}
	if ns == "" {
		return &agentsv1alpha1.ClusterCredentialStore{
			ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec,
		}
	}
	return &agentsv1alpha1.CredentialStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec,
	}
}

func testDeps(c client.Client) Deps {
	return Deps{Client: c, DefaultCredentialNamespace: "cred-system", Exchanger: &credresolve.HTTPTokenExchanger{}}
}

// TestSelectSpec_Precedence: a namespaced CredentialStore overrides the cluster default,
// which overrides the implicit kubernetes fallback.
func TestSelectSpec_Precedence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// No CRDs → implicit kubernetes (existing installs unchanged).
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	spec, source, err := SelectSpec(ctx, c, "team-a")
	if err != nil {
		t.Fatalf("SelectSpec: %v", err)
	}
	if spec.Provider.Kubernetes == nil || source != "implicit-kubernetes" {
		t.Fatalf("no-CRD case = %q, want implicit-kubernetes kubernetes provider", source)
	}

	// Cluster default present → used for a namespace with no override.
	c = fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(k8sStore(DefaultStoreName, "")).Build()
	_, source, err = SelectSpec(ctx, c, "team-a")
	if err != nil {
		t.Fatalf("SelectSpec: %v", err)
	}
	if source != "clustercredentialstore/default" {
		t.Fatalf("cluster-default case = %q, want clustercredentialstore/default", source)
	}

	// Namespaced override present → wins over the cluster default in that namespace.
	c = fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(k8sStore(DefaultStoreName, ""), k8sStore(DefaultStoreName, "team-a")).Build()
	_, source, err = SelectSpec(ctx, c, "team-a")
	if err != nil {
		t.Fatalf("SelectSpec: %v", err)
	}
	if source != "credentialstore/team-a/default" {
		t.Fatalf("override case = %q, want credentialstore/team-a/default", source)
	}
	// A different namespace still falls through to the cluster default.
	_, source, _ = SelectSpec(ctx, c, "team-b")
	if source != "clustercredentialstore/default" {
		t.Fatalf("team-b = %q, want the cluster default (override is per-namespace)", source)
	}
}

// TestBackendFor_KubernetesAndUnimplemented: kubernetes builds a real resolver; the
// not-yet-built providers fail closed with ErrProviderNotImplemented (never a wrong backend);
// a remote provider without mtls fails closed with an mtls-required error (never a plaintext dial).
func TestBackendFor_KubernetesAndUnimplemented(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	deps := testDeps(c)

	b, err := BackendFor(ctx, ImplicitKubernetesSpec(), deps)
	if err != nil || b == nil {
		t.Fatalf("kubernetes BackendFor = (%v, %v), want a backend", b, err)
	}

	// openbao is not yet built → fail closed with ErrProviderNotImplemented.
	_, err = BackendFor(ctx, agentsv1alpha1.CredentialStoreSpec{Provider: agentsv1alpha1.CredentialStoreProvider{
		OpenBao: &agentsv1alpha1.CredentialProviderOpenBao{Address: "https://x:8200"},
	}}, deps)
	if !errors.Is(err, ErrProviderNotImplemented) {
		t.Errorf("openbao BackendFor err = %v, want ErrProviderNotImplemented (fail closed)", err)
	}

	// remote WITHOUT mtls → fail closed on an mtls-required error (not a plaintext dial).
	_, err = BackendFor(ctx, agentsv1alpha1.CredentialStoreSpec{Provider: agentsv1alpha1.CredentialStoreProvider{
		Remote: &agentsv1alpha1.CredentialProviderRemote{Endpoint: "https://x:8443"},
	}}, deps)
	if err == nil || !strings.Contains(err.Error(), "mtls") {
		t.Errorf("remote-without-mtls err = %v, want an mtls-required error", err)
	}

	// postgres WITHOUT encryption → fail closed (a Postgres store must not persist plaintext).
	_, err = BackendFor(ctx, agentsv1alpha1.CredentialStoreSpec{Provider: agentsv1alpha1.CredentialStoreProvider{
		Postgres: &agentsv1alpha1.CredentialProviderPostgres{DSNSecretRef: agentsv1alpha1.SecretKeyRef{Name: "p", Key: "dsn"}},
	}}, deps)
	if err == nil || !strings.Contains(err.Error(), "encryption") {
		t.Errorf("postgres-without-encryption err = %v, want an encryption-required error", err)
	}
}

// TestRouter_SharesBackendAcrossNamespaces: two namespaces on the same resolved config get
// the SAME backend instance — so the global cache + singleflight (ADR 0030 §1) is preserved,
// not fragmented per namespace.
func TestRouter_SharesBackendAcrossNamespaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build() // no CRD → both implicit kubernetes
	r := NewRouter(c, testDeps(c))

	a, err := r.backendFor(ctx, "team-a")
	if err != nil {
		t.Fatalf("backendFor a: %v", err)
	}
	b, err := r.backendFor(ctx, "team-b")
	if err != nil {
		t.Fatalf("backendFor b: %v", err)
	}
	if a != b {
		t.Fatal("two namespaces on the same kubernetes config got DIFFERENT backends — cache/singleflight would fragment")
	}
}

// TestRouter_UnimplementedProviderFailsClosed: a namespace whose CredentialStore names an
// unbuilt provider yields no credential (ErrProviderNotImplemented) rather than silently
// resolving via a wrong backend.
func TestRouter_UnimplementedProviderFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ob := &agentsv1alpha1.CredentialStore{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultStoreName, Namespace: "team-ob"},
		Spec: agentsv1alpha1.CredentialStoreSpec{Provider: agentsv1alpha1.CredentialStoreProvider{
			OpenBao: &agentsv1alpha1.CredentialProviderOpenBao{Address: "https://x:8200"},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ob).Build()
	r := NewRouter(c, testDeps(c))

	_, err := r.Resolve(ctx, "team-ob", "", "srv", "userhash")
	if !errors.Is(err, ErrProviderNotImplemented) {
		t.Fatalf("Resolve on unbuilt provider = %v, want ErrProviderNotImplemented (fail closed)", err)
	}
}

// TestRouter_StoreGrant_Kubernetes: the SPI write path — StoreGrant routes to the selected
// (default kubernetes) backend, which persists a grant Secret; the same Router then resolves
// it. Proves the write path lands in the config-selected backend and round-trips.
func TestRouter_StoreGrant_Kubernetes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build() // no CRD → kubernetes default
	r := NewRouter(c, testDeps(c))

	g := credresolve.Grant{
		Tokens:    credresolve.Tokens{AccessToken: "tok-1", ExpiresAt: time.Now().Add(time.Hour)},
		Config:    credresolve.OAuthConfig{TokenEndpoint: "https://as/token", ClientID: "cid"},
		ServerURL: "https://mcp.example/mcp",
	}
	if err := r.StoreGrant(ctx, "app-ns", "", "srv", "uh", g); err != nil {
		t.Fatalf("StoreGrant: %v", err)
	}
	// A grant Secret now exists in the credential namespace at the derived coordinates.
	gns, gname := credresolve.SecretCoordinates("cred-system", "app-ns", "srv", "uh", "")
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: gns, Name: gname}, &sec); err != nil {
		t.Fatalf("grant Secret not written: %v", err)
	}
	// And it resolves back through the same Router (write→read round-trip).
	cred, err := r.Resolve(ctx, "app-ns", "", "srv", "uh")
	if err != nil || cred.Value != "tok-1" {
		t.Fatalf("Resolve after StoreGrant = (%+v, %v), want tok-1", cred, err)
	}
}

// TestRouter_SelectionCacheTTL: the namespace→backend selection is cached; after the TTL
// elapses it is re-resolved (so a store change propagates).
func TestRouter_SelectionCacheTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	now := time.Unix(0, 0)
	r := NewRouter(c, testDeps(c))
	r.now = func() time.Time { return now }
	r.ttl = 30 * time.Second

	if _, err := r.backendFor(ctx, "team-a"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, ok := r.nsCache["team-a"]; !ok {
		t.Fatal("selection not cached after first resolve")
	}
	// Within TTL: cache entry is reused (expiry unchanged).
	firstExpiry := r.nsCache["team-a"].expires
	now = now.Add(10 * time.Second)
	if _, err := r.backendFor(ctx, "team-a"); err != nil {
		t.Fatalf("within-ttl: %v", err)
	}
	if r.nsCache["team-a"].expires != firstExpiry {
		t.Fatal("selection re-resolved within TTL — the lookup should stay off the hot path")
	}
	// After TTL: re-resolved (expiry advances).
	now = now.Add(30 * time.Second)
	if _, err := r.backendFor(ctx, "team-a"); err != nil {
		t.Fatalf("post-ttl: %v", err)
	}
	if !r.nsCache["team-a"].expires.After(firstExpiry) {
		t.Fatal("selection not re-resolved after TTL expiry")
	}
}
