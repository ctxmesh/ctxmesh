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

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
)

// The console ToolRegistry CRUD endpoints are Postgres-authoritative (ADR 0044):
// the CRD is retired, so these tests seed a memstore (via wireTRStore) and gate
// RBAC through the control-plane SSAR authorizer, not the caller-scoped fake
// client. callerClient still authenticates the request (token → 401), but the
// data + RBAC live in the store path.

// trNS is the namespace used in ToolRegistry tests.
const trNS = "team-tools"

// --- fixture helpers --------------------------------------------------------

// mockToolRegistry builds a minimal ToolRegistry with one approved curated entry.
func mockToolRegistry(name, ns string) *agentsv1alpha1.ToolRegistry {
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.ToolRegistrySpec{
			Tools: []agentsv1alpha1.ToolEntry{
				{
					Name:           "search",
					Description:    "web search",
					Source:         agentsv1alpha1.SourceCurated,
					ApprovalStatus: agentsv1alpha1.ApprovalApproved,
				},
			},
		},
	}
}

// mockToolRegistryWithPending builds a ToolRegistry with one pending entry.
func mockToolRegistryWithPending(name, ns string) *agentsv1alpha1.ToolRegistry {
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.ToolRegistrySpec{
			Tools: []agentsv1alpha1.ToolEntry{
				{
					Name:           "byo-tool",
					Description:    "BYO MCP tool",
					Source:         agentsv1alpha1.SourceUserAdded,
					ApprovalStatus: agentsv1alpha1.ApprovalPending,
				},
			},
		},
	}
}

// newTRConsoleServer builds a caller-scoped BFF server with a memstore wired as
// the ToolRegistry source, seeded with regs and gated by auth (nil ⇒ a permissive
// recordingAuthorizer). It returns the server, the caller-client factory (so token
// routing can be asserted) and the store (for post-request state assertions).
func newTRConsoleServer(t *testing.T, auth authz.Authorizer, regs ...*agentsv1alpha1.ToolRegistry) (*Server, *fakeCallerClientFactory) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)
	wireTRStore(t, s, auth, regs...)
	return s, factory
}

// --- request helpers --------------------------------------------------------

func getToolRegistries(t *testing.T, s *Server, rawQuery string) (ToolRegistryListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	url := "/api/toolregistries"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body ToolRegistryListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

func getToolRegistry(t *testing.T, s *Server, name string) (*ToolRegistryDetail, int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/toolregistries/"+trNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail ToolRegistryDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func createToolRegistry(t *testing.T, s *Server, reqBody ToolRegistryCreateRequest) (*ToolRegistryDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/toolregistries", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var detail ToolRegistryDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

//nolint:unparam
func putToolRegistry(t *testing.T, s *Server, name string, reqBody ToolRegistryUpdateRequest) (*ToolRegistryDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/toolregistries/"+trNS+"/"+name, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail ToolRegistryDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func deleteToolRegistry(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/toolregistries/"+trNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// =============================================================================
// GET /api/toolregistries — list contract
// =============================================================================

// TestListToolRegistriesEmpty proves an empty store yields [] not null.
func TestListToolRegistriesEmpty(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil)

	body, code := getToolRegistries(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items, "items must be [] not null")
	assert.Empty(t, body.Items)
	assert.Empty(t, body.NextCursor)
}

// TestListToolRegistriesReturnsItems proves seeded ToolRegistries appear in the
// response with the correct projections.
func TestListToolRegistriesReturnsItems(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil,
		mockToolRegistry("catalog-a", trNS),
		mockToolRegistry("catalog-b", trNS),
	)

	body, code := getToolRegistries(t, s, "namespace="+trNS)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
		assert.Equal(t, trNS, item.Namespace)
		// Tools must be [] not nil.
		assert.NotNil(t, item.Tools, "tools must be [] not null")
	}
	assert.True(t, names["catalog-a"])
	assert.True(t, names["catalog-b"])
}

// TestListToolRegistriesQFilter proves ?q is a case-insensitive windowed
// substring filter on the registry name (pushed down to the store).
func TestListToolRegistriesQFilter(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil,
		mockToolRegistry("prod-catalog", trNS),
		mockToolRegistry("PROD-staging", trNS),
		mockToolRegistry("dev-catalog", trNS),
	)

	body, code := getToolRegistries(t, s, "q=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
	}
	assert.True(t, names["prod-catalog"])
	assert.True(t, names["PROD-staging"])
	assert.False(t, names["dev-catalog"])

	// No match → [] not null.
	body, code = getToolRegistries(t, s, "q=zzz-nomatch")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items)
	assert.Empty(t, body.Items)
}

// TestListToolRegistriesNamespaceScoping proves ?namespace scopes the list.
func TestListToolRegistriesNamespaceScoping(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil,
		mockToolRegistry("prod-tr", "prod"),
		mockToolRegistry("dev-tr", "dev"),
	)

	body, code := getToolRegistries(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "prod", body.Items[0].Namespace)

	body, code = getToolRegistries(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, body.Items, 2)
}

// TestListToolRegistriesLimitAndCursor proves limit/cursor paging works through
// the store's offset pagination.
func TestListToolRegistriesLimitAndCursor(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil,
		mockToolRegistry("tr-000", trNS),
		mockToolRegistry("tr-001", trNS),
		mockToolRegistry("tr-002", trNS),
		mockToolRegistry("tr-003", trNS),
		mockToolRegistry("tr-004", trNS),
	)

	page1, code := getToolRegistries(t, s, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 2)
	require.NotEmpty(t, page1.NextCursor)

	page2, code := getToolRegistries(t, s, "limit=2&cursor="+page1.NextCursor)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page2.Items, 2)
	assert.NotEqual(t, page1.Items[0].Name, page2.Items[0].Name)

	seen := len(page1.Items) + len(page2.Items)
	cursor := page2.NextCursor
	for cursor != "" {
		next, code := getToolRegistries(t, s, "limit=2&cursor="+cursor)
		require.Equal(t, http.StatusOK, code)
		seen += len(next.Items)
		cursor = next.NextCursor
	}
	assert.Equal(t, 5, seen, "paging must visit every registry exactly once")
}

// TestListToolRegistriesForbiddenIs403 proves a denied SSAR on the list → 403.
func TestListToolRegistriesForbiddenIs403(t *testing.T) {
	s, _ := newTRConsoleServer(t, &recordingAuthorizer{err: authz.ErrForbidden},
		mockToolRegistry("secret-catalog", trNS))

	body, code := getToolRegistries(t, s, "")
	require.Equal(t, http.StatusForbidden, code)
	assert.Empty(t, body.Items, "a denied read must not leak store rows")
}

// =============================================================================
// GET /api/toolregistries/{ns}/{name} — detail
// =============================================================================

// TestGetToolRegistryReturnsDetail proves a seeded ToolRegistry is returned with
// correct projection including the tool entries and approval status. A store-backed
// registry is always Ready (ADR 0044): the controller reconcile loop is retired.
func TestGetToolRegistryReturnsDetail(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil, mockToolRegistry("my-catalog", trNS))

	detail, code, body := getToolRegistry(t, s, "my-catalog")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-catalog", detail.Name)
	assert.Equal(t, trNS, detail.Namespace)
	assert.True(t, detail.Ready)
	assert.Equal(t, phaseReady, detail.Phase)
	require.Len(t, detail.Tools, 1)
	assert.Equal(t, "search", detail.Tools[0].Name)
	assert.Equal(t, agentsv1alpha1.ApprovalApproved, detail.Tools[0].ApprovalStatus)
}

// TestGetToolRegistryNotFoundIs404 proves a missing ToolRegistry yields 404.
func TestGetToolRegistryNotFoundIs404(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil)

	_, code, body := getToolRegistry(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// TestGetToolRegistryForbiddenIs403 proves a caller denied Get sees 403.
func TestGetToolRegistryForbiddenIs403(t *testing.T) {
	s, _ := newTRConsoleServer(t, &recordingAuthorizer{err: authz.ErrForbidden},
		mockToolRegistry("my-catalog", trNS))

	_, code, body := getToolRegistry(t, s, "my-catalog")
	require.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "permission")
}

// =============================================================================
// POST /api/toolregistries — create
// =============================================================================

// TestCreateToolRegistrySucceeds proves a valid ToolRegistry create returns 201
// and lands in the store.
func TestCreateToolRegistrySucceeds(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil)
	store := s.toolRegistryStore

	req := ToolRegistryCreateRequest{
		Name:      "new-catalog",
		Namespace: trNS,
		Tools: []ToolEntryCreateDTO{
			{Name: "web-search", Description: "search the web", Source: agentsv1alpha1.SourceCurated},
		},
	}
	detail, code, body := createToolRegistry(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "new-catalog", detail.Name)
	assert.Equal(t, trNS, detail.Namespace)
	require.Len(t, detail.Tools, 1)
	assert.Equal(t, "web-search", detail.Tools[0].Name)

	// Confirm it landed in the store.
	got, err := store.Get(context.Background(), trNS, "new-catalog")
	require.NoError(t, err)
	require.Len(t, got.Tools, 1)
	assert.Equal(t, "web-search", got.Tools[0].Name)
}

// TestCreateToolRegistryMissingNameIs400 proves a missing name yields 400.
func TestCreateToolRegistryMissingNameIs400(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil)

	req := ToolRegistryCreateRequest{
		Namespace: trNS,
		Tools:     []ToolEntryCreateDTO{{Name: "search"}},
	}
	_, code, body := createToolRegistry(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "name")
}

// TestCreateToolRegistryEmptyToolsIs400 proves an empty tools list yields 400.
func TestCreateToolRegistryEmptyToolsIs400(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil)

	req := ToolRegistryCreateRequest{
		Name:      "bad-catalog",
		Namespace: trNS,
		Tools:     []ToolEntryCreateDTO{},
	}
	_, code, body := createToolRegistry(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
}

// TestCreateToolRegistryAlreadyExistsIs409 proves a duplicate create yields 409
// via the store's atomic Create.
func TestCreateToolRegistryAlreadyExistsIs409(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil, mockToolRegistry("my-catalog", trNS))

	req := ToolRegistryCreateRequest{
		Name:      "my-catalog",
		Namespace: trNS,
		Tools:     []ToolEntryCreateDTO{{Name: "search"}},
	}
	_, code, body := createToolRegistry(t, s, req)
	assert.Equal(t, http.StatusConflict, code, "body: %s", body)
	assert.Contains(t, body, "already exists")
}

// TestCreateToolRegistryInvalidSpecSurfaces422 proves an in-app validation failure
// (duplicate tool names) surfaces as 422 — the store-path replacement for the CRD's
// API-server schema rejection (ADR 0044).
func TestCreateToolRegistryInvalidSpecSurfaces422(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil)

	req := ToolRegistryCreateRequest{
		Name:      "bad-catalog",
		Namespace: trNS,
		Tools: []ToolEntryCreateDTO{
			{Name: "search"},
			{Name: "search"}, // duplicate name ⇒ Validate rejects
		},
	}
	_, code, body := createToolRegistry(t, s, req)
	assert.Equal(t, http.StatusUnprocessableEntity, code, "in-app validation must surface as 422, got %d: %s", code, body)
}

// TestCreateToolRegistryForbiddenIs403 proves a denied SSAR on create returns 403,
// and the caller's token still reached the client factory (authenticated first).
func TestCreateToolRegistryForbiddenIs403(t *testing.T) {
	s, factory := newTRConsoleServer(t, &recordingAuthorizer{err: authz.ErrForbidden})

	req := ToolRegistryCreateRequest{
		Name:      "no-perm",
		Namespace: trNS,
		Tools:     []ToolEntryCreateDTO{{Name: "search"}},
	}
	_, code, body := createToolRegistry(t, s, req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "permission")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestCreateToolRegistryWithoutTokenIs401 proves a token-less POST is rejected 401
// before any store write.
func TestCreateToolRegistryWithoutTokenIs401(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})
	store := wireTRStore(t, s, nil)

	b, _ := json.Marshal(ToolRegistryCreateRequest{
		Name:      "catalog",
		Namespace: trNS,
		Tools:     []ToolEntryCreateDTO{{Name: "search"}},
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/toolregistries", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	page, err := store.List(context.Background(), controlplane.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, page.Items, "no store write must run for a token-less request")
}

// =============================================================================
// PUT /api/toolregistries/{ns}/{name} — update + don't-flip-approval test
// =============================================================================

// TestUpdateToolRegistryEditsTools proves a PUT updates tool entries.
func TestUpdateToolRegistryEditsTools(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil, mockToolRegistry("my-catalog", trNS))

	req := ToolRegistryUpdateRequest{
		Name: "my-catalog",
		Tools: []ToolEntryCreateDTO{
			{Name: "search", Description: "updated desc"},
			{Name: "code-exec", Description: "execute code"},
		},
	}
	detail, code, body := putToolRegistry(t, s, "my-catalog", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	require.Len(t, detail.Tools, 2)
}

// TestUpdateToolRegistryDoesNotFlipApprovalStatus is THE APPROVAL-PRESERVATION
// TEST. It proves that a PUT cannot change a tool entry's approvalStatus — the
// approval state is controller/approval-owned. When the live entry has "pending",
// the updated entry must still be "pending" after the PUT.
func TestUpdateToolRegistryDoesNotFlipApprovalStatus(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil, mockToolRegistryWithPending("my-catalog", trNS))
	store := s.toolRegistryStore

	// PUT with a curated-field edit — no approvalStatus field in the request.
	// The ToolEntryCreateDTO has no approvalStatus field, so even if a crafty
	// caller injected one in raw JSON, the Go decoder would ignore it.
	req := ToolRegistryUpdateRequest{
		Tools: []ToolEntryCreateDTO{
			{Name: "byo-tool", Description: "updated description"},
		},
	}
	detail, code, body := putToolRegistry(t, s, "my-catalog", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	require.Len(t, detail.Tools, 1)

	// THE APPROVAL-PRESERVATION PROPERTY: the tool's approvalStatus must still be
	// "pending" — the PUT must not have flipped it to "approved".
	assert.Equal(t, agentsv1alpha1.ApprovalPending, detail.Tools[0].ApprovalStatus,
		"PUT must NOT flip a tool entry's approvalStatus — approval is controller-owned")

	// Also verify in the store.
	got, err := store.Get(context.Background(), trNS, "my-catalog")
	require.NoError(t, err)
	require.Len(t, got.Tools, 1)
	assert.Equal(t, agentsv1alpha1.ApprovalPending, got.Tools[0].ApprovalStatus,
		"stored approvalStatus must be preserved, not overwritten by the PUT")
}

// TestUpdateToolRegistryRenameGuardIs400 proves a name mismatch yields 400.
func TestUpdateToolRegistryRenameGuardIs400(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil, mockToolRegistry("my-catalog", trNS))

	req := ToolRegistryUpdateRequest{
		Name:  "different-name", // mismatch
		Tools: []ToolEntryCreateDTO{{Name: "search"}},
	}
	_, code, body := putToolRegistry(t, s, "my-catalog", req)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "rename")
}

// TestUpdateToolRegistryAbsentNameInBodyIsOK proves omitting Name does not
// trigger the rename guard.
func TestUpdateToolRegistryAbsentNameInBodyIsOK(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil, mockToolRegistry("my-catalog", trNS))

	req := ToolRegistryUpdateRequest{
		Tools: []ToolEntryCreateDTO{{Name: "search", Description: "updated"}},
	}
	detail, code, body := putToolRegistry(t, s, "my-catalog", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-catalog", detail.Name)
}

// TestUpdateToolRegistryNotFoundIs404 proves a PUT to a missing registry is a 404
// — the store PUT edits, it does not create.
func TestUpdateToolRegistryNotFoundIs404(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil)

	req := ToolRegistryUpdateRequest{Tools: []ToolEntryCreateDTO{{Name: "search"}}}
	_, code, body := putToolRegistry(t, s, "ghost", req)
	assert.Equal(t, http.StatusNotFound, code, "body: %s", body)
}

// TestUpdateToolRegistryForbiddenIs403 proves a denied SSAR on update returns 403.
func TestUpdateToolRegistryForbiddenIs403(t *testing.T) {
	s, factory := newTRConsoleServer(t, &recordingAuthorizer{err: authz.ErrForbidden},
		mockToolRegistry("my-catalog", trNS))

	req := ToolRegistryUpdateRequest{
		Tools: []ToolEntryCreateDTO{{Name: "search"}},
	}
	_, code, body := putToolRegistry(t, s, "my-catalog", req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "permission")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestUpdateToolRegistryInvalidSpecSurfaces422 proves an in-app validation failure
// (duplicate tool names) on update surfaces as 422 (ADR 0044).
func TestUpdateToolRegistryInvalidSpecSurfaces422(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil, mockToolRegistry("my-catalog", trNS))

	req := ToolRegistryUpdateRequest{
		Tools: []ToolEntryCreateDTO{
			{Name: "search"},
			{Name: "search"}, // duplicate ⇒ Validate rejects
		},
	}
	_, code, body := putToolRegistry(t, s, "my-catalog", req)
	assert.Equal(t, http.StatusUnprocessableEntity, code, "in-app validation must surface as 422, got %d: %s", code, body)
}

// TestUpdateToolRegistryWithoutTokenIs401 proves a token-less PUT is rejected 401
// before any store write.
func TestUpdateToolRegistryWithoutTokenIs401(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})
	store := wireTRStore(t, s, nil, mockToolRegistry("my-catalog", trNS))

	b, _ := json.Marshal(ToolRegistryUpdateRequest{Tools: []ToolEntryCreateDTO{{Name: "renamed"}}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/toolregistries/"+trNS+"/my-catalog", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	got, err := store.Get(context.Background(), trNS, "my-catalog")
	require.NoError(t, err)
	require.Len(t, got.Tools, 1)
	assert.Equal(t, "search", got.Tools[0].Name, "no store write must run for a token-less request")
}

// =============================================================================
// DELETE /api/toolregistries/{ns}/{name} — delete
// =============================================================================

// TestDeleteToolRegistryRemovesObject proves a DELETE succeeds (204) and removes
// the store row.
func TestDeleteToolRegistryRemovesObject(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil, mockToolRegistry("my-catalog", trNS))
	store := s.toolRegistryStore

	rec := deleteToolRegistry(t, s, "my-catalog")
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	_, err := store.Get(context.Background(), trNS, "my-catalog")
	assert.ErrorIs(t, err, controlplane.ErrNotFound, "ToolRegistry must be gone after a successful DELETE")
}

// TestDeleteToolRegistryNotFoundIs404 proves deleting a missing ToolRegistry yields 404.
func TestDeleteToolRegistryNotFoundIs404(t *testing.T) {
	s, _ := newTRConsoleServer(t, nil)

	rec := deleteToolRegistry(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestDeleteToolRegistryForbiddenIs403 proves a denied SSAR on delete returns 403,
// and the store row survives.
func TestDeleteToolRegistryForbiddenIs403(t *testing.T) {
	s, factory := newTRConsoleServer(t, &recordingAuthorizer{err: authz.ErrForbidden},
		mockToolRegistry("my-catalog", trNS))
	store := s.toolRegistryStore

	rec := deleteToolRegistry(t, s, "my-catalog")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "permission")
	assert.Equal(t, "caller-token", factory.gotToken)
	_, err := store.Get(context.Background(), trNS, "my-catalog")
	assert.NoError(t, err, "a denied delete leaves the store row intact")
}

// TestDeleteToolRegistryWithoutTokenIs401 proves a token-less DELETE is rejected 401
// before any store write.
func TestDeleteToolRegistryWithoutTokenIs401(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})
	store := wireTRStore(t, s, nil, mockToolRegistry("my-catalog", trNS))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/toolregistries/"+trNS+"/my-catalog", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	_, err := store.Get(context.Background(), trNS, "my-catalog")
	assert.NoError(t, err, "no store delete must run for a token-less request")
}
