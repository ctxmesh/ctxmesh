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

// Tests for GET /api/templates (m74.2, ADR 0068 §2/§3).
//
// What we test:
//   (a) memstore ListTemplates predicate — org/public/team/private visibility, member vs non-member,
//       tombstoned never returned, latest-version-wins per origin
//   (b) handler — authz gate (denied SSAR → 403), namespace default, recipes always included,
//       published entries from store, tenant fail-closed, nil store degrades gracefully

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

	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/agent-engine/internal/controlplane/publishedartifact"
)

// --- (a) memstore ListTemplates predicate tests --------------------------------

func TestListTemplates_OrgInMemberNS(t *testing.T) {
	store := publishedartifact.NewMemStore()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind:            kindAgent,
		OriginNamespace: "ns-a",
		OriginName:      "shared-agent",
		SpecJSON:        json.RawMessage(`{"name":"shared-agent"}`),
		Visibility:      "org",
		ContentHash:     "h1",
	})
	require.NoError(t, err)

	rows, err := store.ListTemplates(context.Background(), "ns-caller", []string{"ns-caller", "ns-a"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "shared-agent", rows[0].OriginName)
}

func TestListTemplates_OrgInNonMemberNS(t *testing.T) {
	store := publishedartifact.NewMemStore()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind:            kindAgent,
		OriginNamespace: "ns-other",
		OriginName:      "others-agent",
		SpecJSON:        json.RawMessage(`{"name":"others-agent"}`),
		Visibility:      "org",
		ContentHash:     "h1",
	})
	require.NoError(t, err)

	// ns-other is NOT in members — must not leak.
	rows, err := store.ListTemplates(context.Background(), "ns-caller", []string{"ns-caller", "ns-a"})
	require.NoError(t, err)
	assert.Empty(t, rows, "org artifact in a non-member namespace must not leak")
}

func TestListTemplates_PublicAnyNS(t *testing.T) {
	store := publishedartifact.NewMemStore()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind:            kindAgent,
		OriginNamespace: "ns-unrelated",
		OriginName:      "open-agent",
		SpecJSON:        json.RawMessage(`{"name":"open-agent"}`),
		Visibility:      "public",
		ContentHash:     "h1",
	})
	require.NoError(t, err)

	rows, err := store.ListTemplates(context.Background(), "ns-caller", []string{"ns-caller"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "open-agent", rows[0].OriginName)
}

func TestListTemplates_TeamOwnNS(t *testing.T) {
	store := publishedartifact.NewMemStore()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind:            kindAgent,
		OriginNamespace: "ns-caller",
		OriginName:      "team-agent",
		SpecJSON:        json.RawMessage(`{"name":"team-agent"}`),
		Visibility:      "team",
		ContentHash:     "h1",
	})
	require.NoError(t, err)

	rows, err := store.ListTemplates(context.Background(), "ns-caller", []string{"ns-caller"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "team-agent", rows[0].OriginName)
}

func TestListTemplates_TeamOtherNS(t *testing.T) {
	store := publishedartifact.NewMemStore()
	// team artifact in ns-a (a member), but NOT callerNS — must NOT appear (team is within-namespace only).
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind:            kindAgent,
		OriginNamespace: "ns-a",
		OriginName:      "team-in-other",
		SpecJSON:        json.RawMessage(`{"name":"team-in-other"}`),
		Visibility:      "team",
		ContentHash:     "h1",
	})
	require.NoError(t, err)

	rows, err := store.ListTemplates(context.Background(), "ns-caller", []string{"ns-caller", "ns-a"})
	require.NoError(t, err)
	assert.Empty(t, rows, "team artifact in a sibling namespace must not be returned")
}

func TestListTemplates_PrivateNeverReturned(t *testing.T) {
	store := publishedartifact.NewMemStore()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind:            kindAgent,
		OriginNamespace: "ns-caller",
		OriginName:      "private-agent",
		SpecJSON:        json.RawMessage(`{"name":"private-agent"}`),
		Visibility:      "private",
		ContentHash:     "h1",
	})
	require.NoError(t, err)

	rows, err := store.ListTemplates(context.Background(), "ns-caller", []string{"ns-caller"})
	require.NoError(t, err)
	assert.Empty(t, rows, "private artifact must never appear in the template gallery (leak-safe)")
}

func TestListTemplates_TombstonedNotReturned(t *testing.T) {
	store := publishedartifact.NewMemStore()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind:            kindAgent,
		OriginNamespace: "ns-caller",
		OriginName:      "dead-agent",
		SpecJSON:        json.RawMessage(`{"name":"dead-agent"}`),
		Visibility:      "public",
		ContentHash:     "h1",
	})
	require.NoError(t, err)
	require.NoError(t, store.Tombstone(context.Background(), kindAgent, "ns-caller", "dead-agent"))

	rows, err := store.ListTemplates(context.Background(), "ns-caller", []string{"ns-caller"})
	require.NoError(t, err)
	assert.Empty(t, rows, "tombstoned artifact must not appear in the gallery")
}

func TestListTemplates_LatestVersionWins(t *testing.T) {
	store := publishedartifact.NewMemStore()
	// Publish v1 then v2 for the same origin — only v2 must appear.
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: "ns-a", OriginName: "evolving",
		SpecJSON: json.RawMessage(`{"v":1}`), Visibility: "org", ContentHash: "h1",
	})
	require.NoError(t, err)
	_, err = store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: "ns-a", OriginName: "evolving",
		SpecJSON: json.RawMessage(`{"v":2}`), Visibility: "org", ContentHash: "h2",
	})
	require.NoError(t, err)

	rows, err := store.ListTemplates(context.Background(), "ns-caller", []string{"ns-caller", "ns-a"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "exactly one row per origin (latest version)")
	assert.Equal(t, 2, rows[0].Version, "latest version must win")
	assert.JSONEq(t, `{"v":2}`, string(rows[0].SpecJSON))
}

// --- (b) handler tests -------------------------------------------------------

// newTemplatesListServer wires a Server for the GET /api/templates handler tests.
func newTemplatesListServer(t *testing.T, paStore publishedartifact.Store, nsStore namespacetenant.Store, auth authz.Authorizer) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := NewServer(Options{
		CallerClients:          factory,
		Scheme:                 testScheme(t),
		Auth:                   AllowAll{},
		PublishedArtifactStore: paStore,
		NamespaceTenantStore:   nsStore,
		Version:                "test",
		Log:                    logr.Discard(),
	})
	if auth != nil {
		s.authorizer = auth
	} else {
		s.authorizer = &recordingAuthorizer{} // permissive by default
	}
	return s
}

// templatesRequest returns a GET /api/templates?namespace=ns-caller request.
func templatesRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/templates?namespace=ns-caller", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

// TestHandleTemplates_ForbiddenSSAR403: denied SSAR → honest 403.
func TestHandleTemplates_ForbiddenSSAR403(t *testing.T) {
	store := publishedartifact.NewMemStore()
	s := newTemplatesListServer(t, store, namespacetenant.NewMemStore(), &recordingAuthorizer{err: authz.ErrForbidden})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, templatesRequest())
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestHandleTemplates_SSARResourceIsAgentDeployments: the authz gate uses resourceAgentDeployments.
func TestHandleTemplates_SSARResourceIsAgentDeployments(t *testing.T) {
	store := publishedartifact.NewMemStore()
	auth := &recordingAuthorizer{}
	s := newTemplatesListServer(t, store, namespacetenant.NewMemStore(), auth)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, templatesRequest())
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, resourceAgentDeployments, auth.last.Resource, "SSAR resource must be agentdeployments")
	assert.Equal(t, authz.VerbList, auth.last.Verb, "SSAR verb must be list")
	assert.Equal(t, "ns-caller", auth.last.Namespace, "SSAR namespace must be the caller's namespace")
}

// TestHandleTemplates_RecipesAlwaysIncluded: the handler always includes embedded recipes
// in the response regardless of what the store has.
func TestHandleTemplates_RecipesAlwaysIncluded(t *testing.T) {
	store := publishedartifact.NewMemStore() // empty — no published artifacts
	s := newTemplatesListServer(t, store, namespacetenant.NewMemStore(), nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, templatesRequest())
	require.Equal(t, http.StatusOK, rec.Code)

	var resp TemplateListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// At least one recipe must be present (the real embedded recipes). Source == "recipe".
	recipeCount := 0
	for _, e := range resp.Templates {
		if e.Source == templateSourceRecipe {
			recipeCount++
			assert.Equal(t, kindAgent, e.Kind)
			assert.Equal(t, visibilityPublic, e.Visibility)
			assert.Nil(t, e.Provenance, "builtin recipes have no provenance")
		}
	}
	assert.Greater(t, recipeCount, 0, "at least one embedded recipe must be returned")
}

// TestHandleTemplates_PublishedEntriesIncluded: published artifacts from the store appear in the response.
func TestHandleTemplates_PublishedEntriesIncluded(t *testing.T) {
	store := publishedartifact.NewMemStore()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind:            kindAgent,
		OriginNamespace: "ns-caller",
		OriginName:      "my-agent",
		SpecJSON:        json.RawMessage(`{"name":"my-agent"}`),
		Visibility:      "team",
		ContentHash:     "h1",
	})
	require.NoError(t, err)

	s := newTemplatesListServer(t, store, namespacetenant.NewMemStore(), nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, templatesRequest())
	require.Equal(t, http.StatusOK, rec.Code)

	var resp TemplateListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	published := make([]TemplateEntry, 0)
	for _, e := range resp.Templates {
		if e.Source == templateSourcePublished {
			published = append(published, e)
		}
	}
	require.Len(t, published, 1, "exactly one published entry must appear")
	assert.Equal(t, "my-agent", published[0].Name)
	assert.Equal(t, "team", published[0].Visibility)
	require.NotNil(t, published[0].Provenance)
	assert.Equal(t, "ns-caller", published[0].Provenance.OriginNamespace)
	assert.Equal(t, "my-agent", published[0].Provenance.OriginName)
	assert.Equal(t, 1, published[0].Provenance.Version)
}

// TestHandleTemplates_UnmappedNSFailsClosed: when the namespace has no tenant mapping, the handler
// returns only own-ns + public rows (no cross-tenant org leakage).
func TestHandleTemplates_UnmappedNSFailsClosed(t *testing.T) {
	store := publishedartifact.NewMemStore()
	// org artifact in ns-other — must NOT appear (no tenant mapping → fail-closed).
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: "ns-other", OriginName: "org-agent",
		SpecJSON: json.RawMessage(`{"name":"org-agent"}`), Visibility: "org", ContentHash: "h1",
	})
	require.NoError(t, err)
	// public artifact — always visible.
	_, err = store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: "ns-other", OriginName: "public-agent",
		SpecJSON: json.RawMessage(`{"name":"public-agent"}`), Visibility: "public", ContentHash: "h2",
	})
	require.NoError(t, err)

	// namespacetenant memstore with no entries → TenantOf returns ok=false.
	nsStore := namespacetenant.NewMemStore()
	s := newTemplatesListServer(t, store, nsStore, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, templatesRequest())
	require.Equal(t, http.StatusOK, rec.Code)

	var resp TemplateListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	publishedNames := make([]string, 0)
	for _, e := range resp.Templates {
		if e.Source == templateSourcePublished {
			publishedNames = append(publishedNames, e.Name)
		}
	}
	assert.Contains(t, publishedNames, "public-agent", "public artifact must always be returned")
	assert.NotContains(t, publishedNames, "org-agent", "org artifact in sibling ns must NOT appear (fail-closed)")
}

// TestHandleTemplates_WithTenant: a mapped namespace returns the full tenant member set (cross-tenant org).
func TestHandleTemplates_WithTenant(t *testing.T) {
	store := publishedartifact.NewMemStore()
	// org artifact in ns-b (a tenant member) — should appear.
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: "ns-b", OriginName: "shared-agent",
		SpecJSON: json.RawMessage(`{"name":"shared-agent"}`), Visibility: "org", ContentHash: "h1",
	})
	require.NoError(t, err)
	// org artifact in ns-other (NOT a tenant member) — must NOT appear.
	_, err = store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: "ns-other", OriginName: "outsider-agent",
		SpecJSON: json.RawMessage(`{"name":"outsider-agent"}`), Visibility: "org", ContentHash: "h2",
	})
	require.NoError(t, err)

	nsStore := namespacetenant.NewMemStore()
	require.NoError(t, nsStore.SetMembers(context.Background(), "acme-tenant", []string{"ns-caller", "ns-b"}))

	s := newTemplatesListServer(t, store, nsStore, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, templatesRequest())
	require.Equal(t, http.StatusOK, rec.Code)

	var resp TemplateListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	publishedNames := make([]string, 0)
	for _, e := range resp.Templates {
		if e.Source == templateSourcePublished {
			publishedNames = append(publishedNames, e.Name)
		}
	}
	assert.Contains(t, publishedNames, "shared-agent", "org artifact from tenant member must appear")
	assert.NotContains(t, publishedNames, "outsider-agent", "org artifact from non-member must not appear")
}

// TestHandleTemplates_NilStoreReturnsRecipes: nil publishedArtifactStore → no 501, just recipes.
func TestHandleTemplates_NilStoreReturnsRecipes(t *testing.T) {
	// No PublishedArtifactStore injected — handler must still return recipes (graceful degrade).
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		CallerClients: &fakeCallerClientFactory{client: c},
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Version:       "test",
		Log:           logr.Discard(),
	})
	s.authorizer = &recordingAuthorizer{}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, templatesRequest())
	require.Equal(t, http.StatusOK, rec.Code, "nil store must not cause 501")

	var resp TemplateListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	hasRecipe := false
	for _, e := range resp.Templates {
		if e.Source == templateSourceRecipe {
			hasRecipe = true
		}
	}
	assert.True(t, hasRecipe, "recipes must be returned even when the artifact store is nil")
}

// TestHandleTemplates_DefaultNamespace: missing ?namespace= defaults to "default".
func TestHandleTemplates_DefaultNamespace(t *testing.T) {
	store := publishedartifact.NewMemStore()
	auth := &recordingAuthorizer{}
	s := newTemplatesListServer(t, store, namespacetenant.NewMemStore(), auth)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/templates", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, defaultCreateNamespace, auth.last.Namespace, "namespace must default to defaultCreateNamespace")
}

// TestHandleTemplates_PrivateNeverLeaked: private artifact in own namespace must not appear even
// when the caller is in that namespace.
func TestHandleTemplates_PrivateNeverLeaked(t *testing.T) {
	store := publishedartifact.NewMemStore()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: "ns-caller", OriginName: "private-agent",
		SpecJSON: json.RawMessage(`{"name":"private-agent"}`), Visibility: "private", ContentHash: "h1",
	})
	require.NoError(t, err)

	nsStore := namespacetenant.NewMemStore()
	require.NoError(t, nsStore.SetMembers(context.Background(), "acme", []string{"ns-caller"}))

	s := newTemplatesListServer(t, store, nsStore, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, templatesRequest())
	require.Equal(t, http.StatusOK, rec.Code)

	var resp TemplateListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	for _, e := range resp.Templates {
		if e.Source == templateSourcePublished {
			assert.NotEqual(t, "private-agent", e.Name, "private artifact must never leak in the gallery")
		}
	}
}
