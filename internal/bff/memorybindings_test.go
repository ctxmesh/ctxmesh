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

// mbNS is the namespace used in MemoryBinding tests.
const mbNS = "team-memory"

// --- fixture helpers --------------------------------------------------------

// mockMemoryBinding builds a minimal MemoryBinding (session scope).
func mockMemoryBinding(name, ns, agentRef string) *agentsv1alpha1.MemoryBinding {
	return &agentsv1alpha1.MemoryBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.MemoryBindingSpec{
			AgentRef: agentRef,
			Scope:    "session",
		},
	}
}

// mockMemoryBindingWithBackend builds a MemoryBinding with a custom Valkey addr.
func mockMemoryBindingWithBackend(name, ns, agentRef, addr string) *agentsv1alpha1.MemoryBinding {
	mb := mockMemoryBinding(name, ns, agentRef)
	mb.Spec.Backend = &agentsv1alpha1.MemoryBackend{Addr: addr}
	return mb
}

// readyMB sets the Ready condition on a MemoryBinding.
func readyMB(mb *agentsv1alpha1.MemoryBinding) *agentsv1alpha1.MemoryBinding {
	mb.Status.Conditions = []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciled",
			Message:            "MEMORY_BACKEND_ADDR injected",
			LastTransitionTime: metav1.Now(),
		},
	}
	return mb
}

// --- request helpers --------------------------------------------------------

func getMemoryBindings(t *testing.T, s *Server, rawQuery string) (MemoryBindingListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	url := "/api/memorybindings"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body MemoryBindingListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

// getMemoryBinding drives GET /api/memorybindings/{ns}/{name} with a caller token.
// ns is always mbNS in this test package; the parameter is kept for future
// multi-namespace tests — the unparam linter is suppressed here because the
// function is a shared test helper whose signature should not be over-specialised.
func getMemoryBinding(t *testing.T, s *Server, ns, name string) (*MemoryBindingDetail, int, string) { //nolint:unparam
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/memorybindings/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail MemoryBindingDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func createMemoryBinding(t *testing.T, s *Server, reqBody MemoryBindingCreateRequest) (*MemoryBindingDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/memorybindings", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var detail MemoryBindingDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// putMemoryBinding drives PUT /api/memorybindings/{ns}/{name} with a caller token.
// ns is always mbNS in this test package — unparam is suppressed for the same
// reason as getMemoryBinding (shared helper).
func putMemoryBinding(t *testing.T, s *Server, ns, name string, reqBody MemoryBindingUpdateRequest) (*MemoryBindingDetail, int, string) { //nolint:unparam
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/memorybindings/"+ns+"/"+name, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail MemoryBindingDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func deleteMemoryBinding(t *testing.T, s *Server, ns, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/memorybindings/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// =============================================================================
// GET /api/memorybindings — list contract
// =============================================================================

// TestListMemoryBindingsEmpty proves an empty cluster yields {"items":[],"nextCursor":""}
// — never null slices.
func TestListMemoryBindingsEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getMemoryBindings(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items, "items must be [] not null")
	assert.Empty(t, body.Items)
	assert.Empty(t, body.NextCursor)
}

// TestListMemoryBindingsReturnsItems proves seeded MemoryBindings appear in the
// response with the correct projections.
func TestListMemoryBindingsReturnsItems(t *testing.T) {
	objs := []client.Object{
		mockMemoryBinding("mb-a", mbNS, "agent-a"),
		mockMemoryBinding("mb-b", mbNS, "agent-b"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getMemoryBindings(t, s, "namespace="+mbNS)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	agentRefs := map[string]bool{}
	for _, item := range body.Items {
		agentRefs[item.AgentRef] = true
		assert.Equal(t, mbNS, item.Namespace)
		assert.Equal(t, "session", item.Scope)
	}
	assert.True(t, agentRefs["agent-a"])
	assert.True(t, agentRefs["agent-b"])
}

// TestListMemoryBindingsLimitAndCursor proves limit/cursor pagination is honored.
func TestListMemoryBindingsLimitAndCursor(t *testing.T) {
	all := []*agentsv1alpha1.MemoryBinding{
		mockMemoryBinding("mb-000", mbNS, "agent-0"),
		mockMemoryBinding("mb-001", mbNS, "agent-1"),
		mockMemoryBinding("mb-002", mbNS, "agent-2"),
		mockMemoryBinding("mb-003", mbNS, "agent-3"),
		mockMemoryBinding("mb-004", mbNS, "agent-4"),
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
			mbList, ok := list.(*agentsv1alpha1.MemoryBindingList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			for _, mb := range all[start:end] {
				mbList.Items = append(mbList.Items, *mb)
			}
			if end < len(all) {
				mbList.Continue = strconv.Itoa(end)
			}
			return nil
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(pagingFn).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Page 1: limit=2 → 2 items + nextCursor.
	page1, code := getMemoryBindings(t, s, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 2)
	require.NotEmpty(t, page1.NextCursor)

	// Page 2 via cursor round-trip.
	page2, code := getMemoryBindings(t, s, "limit=2&cursor="+page1.NextCursor)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page2.Items, 2)
	assert.NotEqual(t, page1.Items[0].Name, page2.Items[0].Name)

	// Drain to exhaustion.
	seen := len(page1.Items) + len(page2.Items)
	cursor := page2.NextCursor
	for cursor != "" {
		next, c2 := getMemoryBindings(t, s, "limit=2&cursor="+cursor)
		require.Equal(t, http.StatusOK, c2)
		seen += len(next.Items)
		cursor = next.NextCursor
	}
	assert.Equal(t, 5, seen, "paging must visit every binding exactly once")
}

// TestListMemoryBindingsQFilter proves ?q is a case-insensitive windowed substring
// filter on the name.
func TestListMemoryBindingsQFilter(t *testing.T) {
	objs := []client.Object{
		mockMemoryBinding("session-mem-prod", mbNS, "agent-prod"),
		mockMemoryBinding("SESSION-MEM-dev", mbNS, "agent-dev"),
		mockMemoryBinding("other-binding", mbNS, "agent-other"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getMemoryBindings(t, s, "q=session-mem")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)

	// No match → [] not null.
	body, code = getMemoryBindings(t, s, "q=zzz-nomatch")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items)
	assert.Empty(t, body.Items)
}

// TestListMemoryBindingsNamespaceScoping proves ?namespace scopes the list.
func TestListMemoryBindingsNamespaceScoping(t *testing.T) {
	objs := []client.Object{
		mockMemoryBinding("mb-prod", "prod", "agent-prod"),
		mockMemoryBinding("mb-dev", "dev", "agent-dev"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getMemoryBindings(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "prod", body.Items[0].Namespace)

	body, code = getMemoryBindings(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, body.Items, 2)
}

// TestListMemoryBindingsForbiddenIs403 proves a Forbidden on the list surfaces
// as 403, not an empty [].
func TestListMemoryBindingsForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "memorybindings"},
					"", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code := getMemoryBindings(t, s, "")
	require.Equal(t, http.StatusForbidden, code)
}

// =============================================================================
// GET /api/memorybindings/{ns}/{name} — detail
// =============================================================================

// TestGetMemoryBindingReturnsDetail proves a seeded MemoryBinding returns with
// all fields projected, including backend addr and Ready status.
func TestGetMemoryBindingReturnsDetail(t *testing.T) {
	mb := readyMB(mockMemoryBindingWithBackend("my-mb", mbNS, "my-agent", "valkey.ns.svc:6379"))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mb).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getMemoryBinding(t, s, mbNS, "my-mb")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-mb", detail.Name)
	assert.Equal(t, mbNS, detail.Namespace)
	assert.Equal(t, "my-agent", detail.AgentRef)
	assert.Equal(t, "session", detail.Scope)
	require.NotNil(t, detail.Backend)
	assert.Equal(t, "valkey.ns.svc:6379", detail.Backend.Addr)
	assert.True(t, detail.Ready)
	assert.Equal(t, phaseReady, detail.Phase)
}

// TestGetMemoryBindingNoBackendOmitsField proves a binding without a backend has
// a nil Backend field in the DTO.
func TestGetMemoryBindingNoBackendOmitsField(t *testing.T) {
	mb := mockMemoryBinding("no-backend", mbNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mb).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getMemoryBinding(t, s, mbNS, "no-backend")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Nil(t, detail.Backend)
}

// TestGetMemoryBindingNotFoundIs404 proves a missing MemoryBinding yields 404.
func TestGetMemoryBindingNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getMemoryBinding(t, s, mbNS, "ghost")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// TestGetMemoryBindingForbiddenIs403 proves a caller denied Get sees 403.
func TestGetMemoryBindingForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "memorybindings"},
					"my-mb", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getMemoryBinding(t, s, mbNS, "my-mb")
	require.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "forbidden")
}

// =============================================================================
// POST /api/memorybindings — create
// =============================================================================

// TestCreateMemoryBindingBasic proves a valid create returns 201 and the detail DTO.
func TestCreateMemoryBindingBasic(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := MemoryBindingCreateRequest{
		Name:      "my-mb",
		Namespace: mbNS,
		AgentRef:  "my-agent",
		Scope:     "session",
	}
	detail, code, body := createMemoryBinding(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-mb", detail.Name)
	assert.Equal(t, mbNS, detail.Namespace)
	assert.Equal(t, "my-agent", detail.AgentRef)

	// Confirm it landed in the fake store.
	var got agentsv1alpha1.MemoryBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: mbNS, Name: "my-mb"}, &got))
	assert.Equal(t, "my-agent", got.Spec.AgentRef)
}

// TestCreateMemoryBindingWithBackend proves the backend addr is stored.
func TestCreateMemoryBindingWithBackend(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := MemoryBindingCreateRequest{
		Name:      "mb-custom",
		Namespace: mbNS,
		AgentRef:  "agent-x",
		Backend:   &MemoryBackendDTO{Addr: "valkey.custom.svc:6379"},
	}
	detail, code, body := createMemoryBinding(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail.Backend)
	assert.Equal(t, "valkey.custom.svc:6379", detail.Backend.Addr)
}

// TestCreateMemoryBindingMissingNameIs400 proves a missing name yields 400.
func TestCreateMemoryBindingMissingNameIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := MemoryBindingCreateRequest{AgentRef: "agent-x", Namespace: mbNS}
	_, code, body := createMemoryBinding(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "name")
}

// TestCreateMemoryBindingMissingAgentRefIs400 proves a missing agentRef yields 400.
func TestCreateMemoryBindingMissingAgentRefIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := MemoryBindingCreateRequest{Name: "mb-x", Namespace: mbNS}
	_, code, body := createMemoryBinding(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "agentRef")
}

// TestCreateMemoryBindingAlreadyExistsIs409 proves a duplicate create yields 409.
func TestCreateMemoryBindingAlreadyExistsIs409(t *testing.T) {
	existing := mockMemoryBinding("my-mb", mbNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := MemoryBindingCreateRequest{Name: "my-mb", Namespace: mbNS, AgentRef: "agent-x"}
	_, code, body := createMemoryBinding(t, s, req)
	assert.Equal(t, http.StatusConflict, code, "body: %s", body)
	assert.Contains(t, body, "already exists")
}

// TestCreateMemoryBindingForbiddenIs403 proves a viewer's create surfaces 403.
func TestCreateMemoryBindingForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "memorybindings"},
					obj.GetName(), errors.New("viewer cannot create"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := MemoryBindingCreateRequest{Name: "my-mb", Namespace: mbNS, AgentRef: "agent-x"}
	_, code, body := createMemoryBinding(t, s, req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestCreateMemoryBindingWithoutTokenIs401 proves a token-less POST is rejected
// 401 before any K8s call.
func TestCreateMemoryBindingWithoutTokenIs401(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				createCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(MemoryBindingCreateRequest{Name: "mb", Namespace: mbNS, AgentRef: "agent-x"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/memorybindings", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, createCalled, "no K8s create must run for a token-less request")
}

// =============================================================================
// PUT /api/memorybindings/{ns}/{name} — SSA edit
// =============================================================================

// TestUpdateMemoryBindingEditsScope proves a PUT changes the scope via SSA.
func TestUpdateMemoryBindingEditsScope(t *testing.T) {
	existing := mockMemoryBinding("my-mb", mbNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := MemoryBindingUpdateRequest{
		AgentRef: "agent-x",
		Scope:    "session",
		Backend:  &MemoryBackendDTO{Addr: "valkey.new.svc:6379"},
	}
	detail, code, body := putMemoryBinding(t, s, mbNS, "my-mb", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	require.NotNil(t, detail.Backend)
	assert.Equal(t, "valkey.new.svc:6379", detail.Backend.Addr)
}

// TestUpdateMemoryBindingAgentRefIsMutable proves a PUT that changes agentRef is
// accepted and applied (agentRef is NOT CRD-immutable — no oldSelf XValidation).
// This is the ACTUAL behavior: agentRef is mutable at the API level.
func TestUpdateMemoryBindingAgentRefIsMutable(t *testing.T) {
	existing := mockMemoryBinding("my-mb", mbNS, "original-agent")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Change agentRef — the API server (and the fake) accepts this because the
	// CRD has no oldSelf immutability XValidation on agentRef.
	req := MemoryBindingUpdateRequest{
		AgentRef: "new-agent",
		Scope:    "session",
	}
	detail, code, body := putMemoryBinding(t, s, mbNS, "my-mb", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	// The new agentRef is applied (mutable, no CRD immutability enforcement).
	assert.Equal(t, "new-agent", detail.AgentRef)

	// Confirm the change landed in the fake store.
	var got agentsv1alpha1.MemoryBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: mbNS, Name: "my-mb"}, &got))
	assert.Equal(t, "new-agent", got.Spec.AgentRef)
}

// TestUpdateMemoryBindingRenameGuardIs400 proves a spec name mismatch is rejected 400.
func TestUpdateMemoryBindingRenameGuardIs400(t *testing.T) {
	existing := mockMemoryBinding("my-mb", mbNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := MemoryBindingUpdateRequest{
		Name:     "different-name", // mismatch
		AgentRef: "agent-x",
	}
	_, code, body := putMemoryBinding(t, s, mbNS, "my-mb", req)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "rename")
}

// TestUpdateMemoryBindingForbiddenIs403 proves a viewer's PUT surfaces 403.
func TestUpdateMemoryBindingForbiddenIs403(t *testing.T) {
	existing := mockMemoryBinding("my-mb", mbNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "memorybindings"},
					obj.GetName(), errors.New("viewer cannot update"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := MemoryBindingUpdateRequest{AgentRef: "agent-x"}
	_, code, body := putMemoryBinding(t, s, mbNS, "my-mb", req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestUpdateMemoryBindingWithoutTokenIs401 proves a token-less PUT is rejected
// 401 before any K8s call.
func TestUpdateMemoryBindingWithoutTokenIs401(t *testing.T) {
	patchCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patchCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(MemoryBindingUpdateRequest{AgentRef: "agent-x"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/memorybindings/"+mbNS+"/my-mb", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, patchCalled, "no K8s patch must run for a token-less request")
}

// =============================================================================
// DELETE /api/memorybindings/{ns}/{name} — delete
// =============================================================================

// TestDeleteMemoryBindingRemovesObject proves a DELETE succeeds (204) and the
// binding is gone.
func TestDeleteMemoryBindingRemovesObject(t *testing.T) {
	mb := mockMemoryBinding("my-mb", mbNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mb).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteMemoryBinding(t, s, mbNS, "my-mb")
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	var got agentsv1alpha1.MemoryBinding
	err := c.Get(context.Background(), client.ObjectKey{Namespace: mbNS, Name: "my-mb"}, &got)
	require.True(t, apierrors.IsNotFound(err), "MemoryBinding must be gone after DELETE")
}

// TestDeleteMemoryBindingNotFoundIs404 proves deleting a missing binding yields 404.
func TestDeleteMemoryBindingNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteMemoryBinding(t, s, mbNS, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestDeleteMemoryBindingForbiddenIs403 proves a viewer's DELETE surfaces 403.
func TestDeleteMemoryBindingForbiddenIs403(t *testing.T) {
	mb := mockMemoryBinding("my-mb", mbNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mb).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "memorybindings"},
					obj.GetName(), errors.New("viewer cannot delete"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := deleteMemoryBinding(t, s, mbNS, "my-mb")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestDeleteMemoryBindingWithoutTokenIs401 proves a token-less DELETE is rejected
// 401 before any K8s call.
func TestDeleteMemoryBindingWithoutTokenIs401(t *testing.T) {
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
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/memorybindings/"+mbNS+"/my-mb", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, deleteCalled, "no K8s delete must run for a token-less request")
}
