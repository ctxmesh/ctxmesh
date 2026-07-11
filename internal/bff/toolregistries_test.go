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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

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

// readyTR sets the Ready condition on a ToolRegistry.
func readyTR(tr *agentsv1alpha1.ToolRegistry) *agentsv1alpha1.ToolRegistry {
	tr.Status.Conditions = []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciled",
			LastTransitionTime: metav1.Now(),
		},
	}
	return tr
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

// TestListToolRegistriesEmpty proves an empty cluster yields [] not null.
func TestListToolRegistriesEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getToolRegistries(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items, "items must be [] not null")
	assert.Empty(t, body.Items)
	assert.Empty(t, body.NextCursor)
}

// TestListToolRegistriesReturnsItems proves seeded ToolRegistries appear in the
// response with the correct projections.
func TestListToolRegistriesReturnsItems(t *testing.T) {
	objs := []client.Object{
		mockToolRegistry("catalog-a", trNS),
		mockToolRegistry("catalog-b", trNS),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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
// substring filter on the registry name.
func TestListToolRegistriesQFilter(t *testing.T) {
	objs := []client.Object{
		mockToolRegistry("prod-catalog", trNS),
		mockToolRegistry("PROD-staging", trNS),
		mockToolRegistry("dev-catalog", trNS),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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
	objs := []client.Object{
		mockToolRegistry("prod-tr", "prod"),
		mockToolRegistry("dev-tr", "dev"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getToolRegistries(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "prod", body.Items[0].Namespace)

	body, code = getToolRegistries(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, body.Items, 2)
}

// TestListToolRegistriesLimitAndCursor proves limit/cursor paging works.
func TestListToolRegistriesLimitAndCursor(t *testing.T) {
	all := []*agentsv1alpha1.ToolRegistry{
		mockToolRegistry("tr-000", trNS),
		mockToolRegistry("tr-001", trNS),
		mockToolRegistry("tr-002", trNS),
		mockToolRegistry("tr-003", trNS),
		mockToolRegistry("tr-004", trNS),
	}

	pagingFn := interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var lo client.ListOptions
			lo.ApplyOptions(opts)
			start := 0
			if lo.Continue != "" {
				n, err := strconv.Atoi(lo.Continue)
				if err != nil {
					return fmt.Errorf("bad continue token %q", lo.Continue)
				}
				start = n
			}
			end := len(all)
			if lo.Limit > 0 && start+int(lo.Limit) < end {
				end = start + int(lo.Limit)
			}
			trList, ok := list.(*agentsv1alpha1.ToolRegistryList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			for _, tr := range all[start:end] {
				trList.Items = append(trList.Items, *tr)
			}
			if end < len(all) {
				trList.Continue = strconv.Itoa(end)
			}
			return nil
		},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(pagingFn).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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

// TestListToolRegistriesForbiddenIs403 proves a Forbidden on the list surfaces as 403.
func TestListToolRegistriesForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "toolregistries"},
					"", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code := getToolRegistries(t, s, "")
	require.Equal(t, http.StatusForbidden, code)
}

// =============================================================================
// GET /api/toolregistries/{ns}/{name} — detail
// =============================================================================

// TestGetToolRegistryReturnsDetail proves a seeded ToolRegistry is returned with
// correct projection including the tool entries and approval status.
func TestGetToolRegistryReturnsDetail(t *testing.T) {
	tr := readyTR(mockToolRegistry("my-catalog", trNS))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(tr).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getToolRegistry(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// TestGetToolRegistryForbiddenIs403 proves a caller denied Get sees 403.
func TestGetToolRegistryForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "toolregistries"},
					"my-catalog", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getToolRegistry(t, s, "my-catalog")
	require.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "forbidden")
}

// =============================================================================
// POST /api/toolregistries — create
// =============================================================================

// TestCreateToolRegistrySucceeds proves a valid ToolRegistry create returns 201.
func TestCreateToolRegistrySucceeds(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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

	// Confirm it landed in the fake store.
	var got agentsv1alpha1.ToolRegistry
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: trNS, Name: "new-catalog"}, &got))
	assert.Len(t, got.Spec.Tools, 1)
	assert.Equal(t, "web-search", got.Spec.Tools[0].Name)
}

// TestCreateToolRegistryMissingNameIs400 proves a missing name yields 400.
func TestCreateToolRegistryMissingNameIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ToolRegistryCreateRequest{
		Name:      "bad-catalog",
		Namespace: trNS,
		Tools:     []ToolEntryCreateDTO{},
	}
	_, code, body := createToolRegistry(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
}

// TestCreateToolRegistryAlreadyExistsIs409 proves a duplicate create yields 409.
func TestCreateToolRegistryAlreadyExistsIs409(t *testing.T) {
	existing := mockToolRegistry("my-catalog", trNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ToolRegistryCreateRequest{
		Name:      "my-catalog",
		Namespace: trNS,
		Tools:     []ToolEntryCreateDTO{{Name: "search"}},
	}
	_, code, body := createToolRegistry(t, s, req)
	assert.Equal(t, http.StatusConflict, code, "body: %s", body)
	assert.Contains(t, body, "already exists")
}

// TestCreateToolRegistryAPIServerRejectionSurfaces4xx proves API server Invalid
// surfaces as 4xx (422).
func TestCreateToolRegistryAPIServerRejectionSurfaces4xx(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: toolRegistryKind},
					obj.GetName(), nil)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ToolRegistryCreateRequest{
		Name:      "bad-catalog",
		Namespace: trNS,
		Tools:     []ToolEntryCreateDTO{{Name: "search"}},
	}
	_, code, body := createToolRegistry(t, s, req)
	assert.True(t, code >= 400 && code < 500, "API server rejection must surface as 4xx, got %d: %s", code, body)
}

// TestCreateToolRegistryForbiddenIs403 proves a viewer's create returns 403.
func TestCreateToolRegistryForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "toolregistries"},
					obj.GetName(), errors.New("viewer cannot create"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := ToolRegistryCreateRequest{
		Name:      "no-perm",
		Namespace: trNS,
		Tools:     []ToolEntryCreateDTO{{Name: "search"}},
	}
	_, code, body := createToolRegistry(t, s, req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestCreateToolRegistryWithoutTokenIs401 proves a token-less POST is rejected 401.
func TestCreateToolRegistryWithoutTokenIs401(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				createCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(ToolRegistryCreateRequest{
		Name:      "catalog",
		Namespace: trNS,
		Tools:     []ToolEntryCreateDTO{{Name: "search"}},
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/toolregistries", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, createCalled, "no K8s create must run for a token-less request")
}

// =============================================================================
// PUT /api/toolregistries/{ns}/{name} — update (SSA) + don't-flip-approval test
// =============================================================================

// TestUpdateToolRegistryEditsTools proves a PUT updates tool entries via SSA.
func TestUpdateToolRegistryEditsTools(t *testing.T) {
	existing := mockToolRegistry("my-catalog", trNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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
// approval state is controller/approval-owned. When the live entry has
// "pending", the updated entry must still be "pending" after the PUT, even if
// the caller tries to sneak an approvalStatus field in the JSON body (which is
// silently ignored by the update request DTO — ToolEntryCreateDTO has no
// approvalStatus field). This is the "don't-break-approval/register property"
// from the task description.
func TestUpdateToolRegistryDoesNotFlipApprovalStatus(t *testing.T) {
	// Seed with a pending-approved user-added tool.
	existing := mockToolRegistryWithPending("my-catalog", trNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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

	// Also verify in the fake store.
	var got agentsv1alpha1.ToolRegistry
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: trNS, Name: "my-catalog"}, &got))
	require.Len(t, got.Spec.Tools, 1)
	assert.Equal(t, agentsv1alpha1.ApprovalPending, got.Spec.Tools[0].ApprovalStatus,
		"stored approvalStatus must be preserved, not overwritten by the PUT")
}

// TestUpdateToolRegistryRenameGuardIs400 proves a name mismatch yields 400.
func TestUpdateToolRegistryRenameGuardIs400(t *testing.T) {
	existing := mockToolRegistry("my-catalog", trNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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
	existing := mockToolRegistry("my-catalog", trNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ToolRegistryUpdateRequest{
		Tools: []ToolEntryCreateDTO{{Name: "search", Description: "updated"}},
	}
	detail, code, body := putToolRegistry(t, s, "my-catalog", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-catalog", detail.Name)
}

// TestUpdateToolRegistryForbiddenIs403 proves a viewer's PUT returns 403.
func TestUpdateToolRegistryForbiddenIs403(t *testing.T) {
	existing := mockToolRegistry("my-catalog", trNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "toolregistries"},
					obj.GetName(), errors.New("viewer cannot update"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := ToolRegistryUpdateRequest{
		Tools: []ToolEntryCreateDTO{{Name: "search"}},
	}
	_, code, body := putToolRegistry(t, s, "my-catalog", req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
}

// TestUpdateToolRegistryInvalidWriteSurfaces422 proves API server Invalid → 422.
func TestUpdateToolRegistryInvalidWriteSurfaces422(t *testing.T) {
	existing := mockToolRegistry("my-catalog", trNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: toolRegistryKind},
					obj.GetName(), nil)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ToolRegistryUpdateRequest{
		Tools: []ToolEntryCreateDTO{{Name: "search"}},
	}
	_, code, body := putToolRegistry(t, s, "my-catalog", req)
	assert.Equal(t, http.StatusUnprocessableEntity, code, "API-server Invalid must surface as 422, got %d: %s", code, body)
}

// TestUpdateToolRegistryWithoutTokenIs401 proves a token-less PUT is rejected 401.
func TestUpdateToolRegistryWithoutTokenIs401(t *testing.T) {
	patchCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patchCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(ToolRegistryUpdateRequest{Tools: []ToolEntryCreateDTO{{Name: "search"}}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/toolregistries/"+trNS+"/my-catalog", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, patchCalled, "no K8s patch must run for a token-less request")
}

// =============================================================================
// DELETE /api/toolregistries/{ns}/{name} — delete
// =============================================================================

// TestDeleteToolRegistryRemovesObject proves a DELETE succeeds (204).
func TestDeleteToolRegistryRemovesObject(t *testing.T) {
	tr := mockToolRegistry("my-catalog", trNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(tr).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteToolRegistry(t, s, "my-catalog")
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	var got agentsv1alpha1.ToolRegistry
	err := c.Get(context.Background(), client.ObjectKey{Namespace: trNS, Name: "my-catalog"}, &got)
	require.True(t, apierrors.IsNotFound(err), "ToolRegistry must be gone after a successful DELETE")
}

// TestDeleteToolRegistryNotFoundIs404 proves deleting a missing ToolRegistry yields 404.
func TestDeleteToolRegistryNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteToolRegistry(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestDeleteToolRegistryForbiddenIs403 proves a viewer's DELETE returns 403.
func TestDeleteToolRegistryForbiddenIs403(t *testing.T) {
	tr := mockToolRegistry("my-catalog", trNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(tr).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "toolregistries"},
					obj.GetName(), errors.New("viewer cannot delete"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := deleteToolRegistry(t, s, "my-catalog")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestDeleteToolRegistryWithoutTokenIs401 proves a token-less DELETE is rejected 401.
func TestDeleteToolRegistryWithoutTokenIs401(t *testing.T) {
	deleteCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				deleteCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/toolregistries/"+trNS+"/my-catalog", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, deleteCalled, "no K8s delete must run for a token-less request")
}
