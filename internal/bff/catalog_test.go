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

package bff

// Tests for GET /api/catalog (m73.4, ADR 0067 §6).
//
// What we test:
//   (a) memstore ListCatalog predicate — org/public/team/private visibility, member vs non-member
//   (b) handler — unmapped namespace fails closed (own-ns + public); denied SSAR → 403;
//       DTO has no SecretName; members deduplication

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// --- (a) memstore ListCatalog predicate tests --------------------------------

func TestListCatalog_OrgInMemberNS(t *testing.T) {
	store := toolregistry.NewMemStore()
	// org server in member namespace ns-a — should be returned
	_, err := store.Upsert(context.Background(), makeCatalogRegistry("ns-a", "shared-mcp", visibilityOrg))
	require.NoError(t, err)

	rows, err := store.ListCatalog(context.Background(), "ns-caller", []string{"ns-caller", "ns-a"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "shared-mcp", rows[0].Name)
	assert.Equal(t, "ns-a", rows[0].Namespace)
}

func TestListCatalog_OrgInNonMemberNS(t *testing.T) {
	store := toolregistry.NewMemStore()
	// org server in ns-other, which is NOT in members — must NOT be returned
	_, err := store.Upsert(context.Background(), makeCatalogRegistry("ns-other", "others-mcp", visibilityOrg))
	require.NoError(t, err)

	rows, err := store.ListCatalog(context.Background(), "ns-caller", []string{"ns-caller", "ns-a"})
	require.NoError(t, err)
	assert.Empty(t, rows, "org server in a non-member namespace must not leak")
}

func TestListCatalog_PublicAnyNS(t *testing.T) {
	store := toolregistry.NewMemStore()
	// public server in a completely unrelated namespace — still visible
	_, err := store.Upsert(context.Background(), makeCatalogRegistry("ns-unrelated", "open-mcp", visibilityPublic))
	require.NoError(t, err)

	rows, err := store.ListCatalog(context.Background(), "ns-caller", []string{"ns-caller"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "open-mcp", rows[0].Name)
}

func TestListCatalog_TeamOwnNS(t *testing.T) {
	store := toolregistry.NewMemStore()
	// team server in callerNS — should be returned
	_, err := store.Upsert(context.Background(), makeCatalogRegistry("ns-caller", "team-mcp", visibilityTeam))
	require.NoError(t, err)

	rows, err := store.ListCatalog(context.Background(), "ns-caller", []string{"ns-caller"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "team-mcp", rows[0].Name)
}

func TestListCatalog_TeamOtherNS(t *testing.T) {
	store := toolregistry.NewMemStore()
	// team server in ns-a (a member), but NOT callerNS — must NOT be returned (team is within-namespace)
	_, err := store.Upsert(context.Background(), makeCatalogRegistry("ns-a", "team-in-other", visibilityTeam))
	require.NoError(t, err)

	rows, err := store.ListCatalog(context.Background(), "ns-caller", []string{"ns-caller", "ns-a"})
	require.NoError(t, err)
	assert.Empty(t, rows, "team server in a sibling namespace must not be returned")
}

func TestListCatalog_PrivateNeverReturned(t *testing.T) {
	store := toolregistry.NewMemStore()
	// private server in callerNS itself — must NEVER be returned by ListCatalog
	_, err := store.Upsert(context.Background(), makeCatalogRegistry("ns-caller", "private-mcp", visibilityPrivate))
	require.NoError(t, err)

	rows, err := store.ListCatalog(context.Background(), "ns-caller", []string{"ns-caller"})
	require.NoError(t, err)
	assert.Empty(t, rows, "private server must never appear in the catalog (leak-safe)")
}

func TestListCatalog_ManagedByFilter(t *testing.T) {
	store := toolregistry.NewMemStore()
	// org server but WITHOUT the managed-by=ctxmesh-mcp label — must not appear
	_, err := store.Upsert(context.Background(), toolregistry.ToolRegistry{
		Namespace: "ns-caller",
		Name:      "curated-no-managed-by",
		Labels:    map[string]string{labelMCPVisibility: visibilityOrg}, // no managed-by label
		Tools:     []toolregistry.ToolEntry{{Name: "some-tool"}},
	})
	require.NoError(t, err)

	rows, err := store.ListCatalog(context.Background(), "ns-caller", []string{"ns-caller"})
	require.NoError(t, err)
	assert.Empty(t, rows, "non-managed-by-mcp registries must not appear in the catalog")
}

// --- (b) handler tests -------------------------------------------------------

// newCatalogServer wires a BFF Server with mcpEnabled, a fake caller-client factory,
// the given toolRegistryStore, and an optional namespaceTenantStore.
func newCatalogServer(t *testing.T, trStore toolregistry.Store, nsStore namespacetenant.Store, auth authz.Authorizer) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := NewServer(Options{
		CallerClients:        factory,
		Scheme:               testScheme(t),
		Auth:                 AllowAll{},
		MCPEnabled:           true,
		ToolRegistryStore:    trStore,
		NamespaceTenantStore: nsStore,
		Version:              "test",
		Log:                  logr.Discard(),
	})
	if auth != nil {
		s.authorizer = auth
	} else {
		s.authorizer = &recordingAuthorizer{} // permissive by default
	}
	return s
}

// catalogRequest returns a GET /api/catalog?namespace=ns-caller request for the test namespace.
func catalogRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/catalog?namespace=ns-caller", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

// TestHandleCatalog_UnmappedNSFailsClosed: when the namespace has no tenant
// mapping, the catalog returns only own-ns servers (public + own-ns team).
func TestHandleCatalog_UnmappedNSFailsClosed(t *testing.T) {
	store := toolregistry.NewMemStore()
	// A public server in an unrelated namespace — visible even without a tenant.
	_, err := store.Upsert(context.Background(), makeCatalogRegistry("ns-other", "public-mcp", visibilityPublic))
	require.NoError(t, err)
	// An org server in a sibling namespace — must NOT appear (no tenant mapping).
	_, err = store.Upsert(context.Background(), makeCatalogRegistry("ns-other", "org-mcp", visibilityOrg))
	require.NoError(t, err)
	// A team server in callerNS — must appear.
	_, err = store.Upsert(context.Background(), makeCatalogRegistry("ns-caller", "my-team-mcp", visibilityTeam))
	require.NoError(t, err)

	// namespacetenant memstore with no entries → TenantOf returns ok=false
	nsStore := namespacetenant.NewMemStore()
	s := newCatalogServer(t, store, nsStore, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, catalogRequest())
	require.Equal(t, http.StatusOK, rec.Code)

	var resp CatalogResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	names := make([]string, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		names = append(names, e.Name)
	}
	assert.Contains(t, names, "public-mcp", "public server must be returned (no tenant needed)")
	assert.Contains(t, names, "my-team-mcp", "own-ns team server must be returned")
	assert.NotContains(t, names, "org-mcp", "org server in sibling ns must NOT appear (fail-closed: no tenant)")
}

// TestHandleCatalog_ForbiddenSSAR403: denied SSAR → honest 403.
func TestHandleCatalog_ForbiddenSSAR403(t *testing.T) {
	store := toolregistry.NewMemStore()
	s := newCatalogServer(t, store, namespacetenant.NewMemStore(), &recordingAuthorizer{err: authz.ErrForbidden})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, catalogRequest())
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestHandleCatalog_NilStoreIs501: if toolRegistryStore is nil → 501.
func TestHandleCatalog_NilStoreIs501(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		CallerClients: &fakeCallerClientFactory{client: c},
		Auth:          AllowAll{},
		MCPEnabled:    true,
		Version:       "test",
		Log:           logr.Discard(),
	})
	s.authorizer = &recordingAuthorizer{}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, catalogRequest())
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// TestHandleCatalog_NoSecretName: DTO must not contain any SecretName field.
func TestHandleCatalog_NoSecretName(t *testing.T) {
	store := toolregistry.NewMemStore()
	// A server with a secret annotation (the secret name is stored as an annotation).
	reg := makeCatalogRegistry("ns-caller", "keyed-mcp", visibilityTeam)
	reg.Annotations[annMCPSecret] = "keyed-mcp-secret"
	_, err := store.Upsert(context.Background(), reg)
	require.NoError(t, err)

	s := newCatalogServer(t, store, namespacetenant.NewMemStore(), nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, catalogRequest())
	require.Equal(t, http.StatusOK, rec.Code)

	// The raw JSON must not contain "secretName" — the catalog DTO deliberately omits it.
	assert.NotContains(t, rec.Body.String(), "secretName", "catalog DTO must not leak secretName")
	assert.NotContains(t, rec.Body.String(), "keyed-mcp-secret", "catalog DTO must not leak the secret reference")
}

// TestHandleCatalog_WithTenant: a mapped namespace returns the full tenant member set.
func TestHandleCatalog_WithTenant(t *testing.T) {
	store := toolregistry.NewMemStore()
	// org server in ns-b (a tenant member) — should appear.
	_, err := store.Upsert(context.Background(), makeCatalogRegistry("ns-b", "shared-mcp", visibilityOrg))
	require.NoError(t, err)
	// org server in ns-other (NOT a tenant member) — must NOT appear.
	_, err = store.Upsert(context.Background(), makeCatalogRegistry("ns-other", "outsider-mcp", visibilityOrg))
	require.NoError(t, err)

	nsStore := namespacetenant.NewMemStore()
	require.NoError(t, nsStore.SetMembers(context.Background(), "acme-tenant", []string{"ns-caller", "ns-b"}))

	s := newCatalogServer(t, store, nsStore, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, catalogRequest())
	require.Equal(t, http.StatusOK, rec.Code)

	var resp CatalogResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	names := make([]string, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		names = append(names, e.Name)
	}
	assert.Contains(t, names, "shared-mcp")
	assert.NotContains(t, names, "outsider-mcp")
}

// TestHandleCatalog_PrivateNeverLeaked: private server in own namespace must not
// appear in the catalog even when the caller is in the same namespace.
func TestHandleCatalog_PrivateNeverLeaked(t *testing.T) {
	store := toolregistry.NewMemStore()
	_, err := store.Upsert(context.Background(), makeCatalogRegistry("ns-caller", "my-private-mcp", visibilityPrivate))
	require.NoError(t, err)

	nsStore := namespacetenant.NewMemStore()
	require.NoError(t, nsStore.SetMembers(context.Background(), "acme-tenant", []string{"ns-caller"}))

	s := newCatalogServer(t, store, nsStore, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, catalogRequest())
	require.Equal(t, http.StatusOK, rec.Code)

	var resp CatalogResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Entries, "private server must never appear in the cross-tenant catalog")
}

// TestHandleCatalog_DefaultNamespace: missing ?namespace= defaults to "default".
func TestHandleCatalog_DefaultNamespace(t *testing.T) {
	store := toolregistry.NewMemStore()
	// A public server — will be returned regardless of namespace.
	_, err := store.Upsert(context.Background(), makeCatalogRegistry("any-ns", "public-mcp", visibilityPublic))
	require.NoError(t, err)

	s := newCatalogServer(t, store, namespacetenant.NewMemStore(), nil)

	// No ?namespace= query param.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/catalog", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp CatalogResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// Public server must appear (namespace defaults to "default" which is always in members for unmapped ns).
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, "public-mcp", resp.Entries[0].Name)
}
