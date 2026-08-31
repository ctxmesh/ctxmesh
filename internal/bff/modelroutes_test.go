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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// mrNS is the namespace used in ModelRoute tests.
const mrNS = "team-a"

// --- fixture helpers --------------------------------------------------------

// mockModelRoute builds a ModelRoute with a single mock provider (no
// secretBindingRef required). Safe for direct create in the fake store.
func mockModelRoute(name, ns string) *agentsv1alpha1.ModelRoute {
	return &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{Provider: "mock", Model: "mock-default", Priority: 1},
			},
		},
	}
}

// apiBaseModelRoute builds a ModelRoute with a non-mock provider that uses
// apiBase instead of secretBindingRef (the m14.12b golden-path field — no key
// required when apiBase is set).
func apiBaseModelRoute(name, ns string) *agentsv1alpha1.ModelRoute {
	return &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{
					Provider: "openai",
					Model:    "gpt-4o",
					Priority: 1,
					APIBase:  "http://tool-mock.ns.svc.cluster.local:9099/v1",
				},
			},
		},
	}
}

// secretBindingModelRoute builds a ModelRoute with a non-mock provider that has
// a secretBindingRef (the typical production path).
func secretBindingModelRoute(name, ns, secretBindingRef string) *agentsv1alpha1.ModelRoute {
	return &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{
				{
					Provider:         "anthropic",
					Model:            "claude-sonnet-4-6",
					Priority:         1,
					SecretBindingRef: secretBindingRef,
				},
			},
		},
	}
}

// readyMR sets the Ready condition on a ModelRoute (simulates a reconciled object).
func readyMR(mr *agentsv1alpha1.ModelRoute) *agentsv1alpha1.ModelRoute {
	mr.Status.Conditions = []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciled",
			Message:            "gateway config updated",
			LastTransitionTime: metav1.Now(),
		},
	}
	return mr
}

// --- request helpers --------------------------------------------------------

// getModelRoutes drives GET /api/modelroutes with a caller token and the given
// raw query string. Returns the decoded response and the HTTP status code.
func getModelRoutes(t *testing.T, s *Server, rawQuery string) (ModelRouteListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	url := "/api/modelroutes"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body ModelRouteListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

// getModelRoute drives GET /api/modelroutes/{ns}/{name} with a caller token.
// ns is always mrNS in this test package; the parameter is kept for future
// multi-namespace tests — the unparam linter is suppressed here because the
// function is a shared test helper whose signature should not be over-specialised.
func getModelRoute(t *testing.T, s *Server, name string) (*ModelRouteDetail, int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/modelroutes/"+mrNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail ModelRouteDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// createModelRoute drives POST /api/modelroutes with the given request body.
func createModelRoute(t *testing.T, s *Server, reqBody ModelRouteCreateRequest) (*ModelRouteDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/modelroutes", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var detail ModelRouteDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// putModelRoute drives PUT /api/modelroutes/mrNS/{name} with the given request body.
func putModelRoute(t *testing.T, s *Server, name string, reqBody ModelRouteUpdateRequest) (*ModelRouteDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/modelroutes/"+mrNS+"/"+name, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail ModelRouteDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// deleteModelRoute drives DELETE /api/modelroutes/mrNS/{name} with a caller token.
func deleteModelRoute(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/modelroutes/"+mrNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// =============================================================================
// GET /api/modelroutes — list contract
// =============================================================================

// TestListModelRoutesEmpty proves an empty cluster yields {"items":[],"nextCursor":""}
// — never null slices.
func TestListModelRoutesEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getModelRoutes(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items, "items must be [] not null")
	assert.Empty(t, body.Items)
	assert.Empty(t, body.NextCursor)
}

// TestListModelRoutesReturnsItems proves seeded ModelRoutes appear in the
// response with the correct projections.
func TestListModelRoutesReturnsItems(t *testing.T) {
	objs := []client.Object{
		mockModelRoute("route-a", mrNS),
		mockModelRoute("route-b", mrNS),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getModelRoutes(t, s, "namespace="+mrNS)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
		assert.Equal(t, mrNS, item.Namespace)
		require.Len(t, item.Providers, 1)
		assert.Equal(t, "mock", item.Providers[0].Provider)
	}
	assert.True(t, names["route-a"])
	assert.True(t, names["route-b"])
}

// TestListModelRoutesLimitAndCursor proves limit/cursor are honored (the same
// contract as GET /api/agents). We simulate paging with an interceptor, just
// like the agent list contract tests.
func TestListModelRoutesLimitAndCursor(t *testing.T) {
	// Build a paging interceptor for ModelRouteList (mirrors pagingListInterceptor
	// but for ModelRoute), standing in for the K8s API server pagination.
	all := []*agentsv1alpha1.ModelRoute{
		mockModelRoute("route-000", mrNS),
		mockModelRoute("route-001", mrNS),
		mockModelRoute("route-002", mrNS),
		mockModelRoute("route-003", mrNS),
		mockModelRoute("route-004", mrNS),
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
			mrList, ok := list.(*agentsv1alpha1.ModelRouteList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			for _, mr := range all[start:end] {
				mrList.Items = append(mrList.Items, *mr)
			}
			if end < len(all) {
				mrList.Continue = strconv.Itoa(end)
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
	page1, code := getModelRoutes(t, s, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 2)
	require.NotEmpty(t, page1.NextCursor, "a non-exhausted list must expose a nextCursor")

	// Page 2 via cursor round-trip.
	page2, code := getModelRoutes(t, s, "limit=2&cursor="+page1.NextCursor)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page2.Items, 2)
	assert.NotEqual(t, page1.Items[0].Name, page2.Items[0].Name, "page 2 must be a different window")

	// Drain to exhaustion.
	seen := len(page1.Items) + len(page2.Items)
	cursor := page2.NextCursor
	for cursor != "" {
		next, code := getModelRoutes(t, s, "limit=2&cursor="+cursor)
		require.Equal(t, http.StatusOK, code)
		seen += len(next.Items)
		cursor = next.NextCursor
	}
	assert.Equal(t, 5, seen, "paging must visit every route exactly once")
}

// TestListModelRoutesQFilter proves ?q is a case-insensitive windowed substring
// filter on the route name.
func TestListModelRoutesQFilter(t *testing.T) {
	objs := []client.Object{
		mockModelRoute("anthropic-prod", mrNS),
		mockModelRoute("ANTHROPIC-dev", mrNS),
		mockModelRoute("openai-prod", mrNS),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getModelRoutes(t, s, "q=anthropic")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2, "q must match both anthropic variants case-insensitively")
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
	}
	assert.True(t, names["anthropic-prod"])
	assert.True(t, names["ANTHROPIC-dev"])
	assert.False(t, names["openai-prod"])

	// No match → [] not null.
	body, code = getModelRoutes(t, s, "q=zzz-nomatch")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items)
	assert.Empty(t, body.Items)
}

// TestListModelRoutesNamespaceScoping proves ?namespace scopes the list to one
// namespace and an absent ?namespace returns all namespaces.
func TestListModelRoutesNamespaceScoping(t *testing.T) {
	objs := []client.Object{
		mockModelRoute("prod-route", "prod"),
		mockModelRoute("dev-route", "dev"),
		mockModelRoute("dev-route2", "dev"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Scoped.
	body, code := getModelRoutes(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "prod", body.Items[0].Namespace)

	// Unscoped → all.
	body, code = getModelRoutes(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, body.Items, 3)
}

// TestListModelRoutesForbiddenIs403 proves a Forbidden on the list surfaces as
// 403, not an empty [].
func TestListModelRoutesForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "modelroutes"},
					"", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code := getModelRoutes(t, s, "")
	require.Equal(t, http.StatusForbidden, code)
}

// =============================================================================
// GET /api/modelroutes/{ns}/{name} — detail
// =============================================================================

// TestGetModelRouteReturnsDetail proves a seeded ModelRoute is returned with
// all fields projected correctly, including phase and ready from conditions.
func TestGetModelRouteReturnsDetail(t *testing.T) {
	mr := readyMR(secretBindingModelRoute("my-route", mrNS, "my-secret-binding"))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mr).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getModelRoute(t, s, "my-route")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-route", detail.Name)
	assert.Equal(t, mrNS, detail.Namespace)
	require.Len(t, detail.Providers, 1)
	assert.Equal(t, "anthropic", detail.Providers[0].Provider)
	assert.Equal(t, "claude-sonnet-4-6", detail.Providers[0].Model)
	assert.Equal(t, int32(1), detail.Providers[0].Priority)
	assert.Equal(t, "my-secret-binding", detail.Providers[0].SecretBindingRef)
	assert.True(t, detail.Ready)
	assert.Equal(t, phaseReady, detail.Phase)
}

// TestGetModelRouteAPIBaseField proves the apiBase field is preserved in the
// detail DTO (m14.12b requirement — never stripped).
func TestGetModelRouteAPIBaseField(t *testing.T) {
	mr := apiBaseModelRoute("base-route", mrNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mr).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getModelRoute(t, s, "base-route")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	require.Len(t, detail.Providers, 1)
	assert.Equal(t, "http://tool-mock.ns.svc.cluster.local:9099/v1", detail.Providers[0].APIBase)
	assert.Empty(t, detail.Providers[0].SecretBindingRef)
}

// TestGetModelRouteNotFoundIs404 proves a missing ModelRoute yields 404.
func TestGetModelRouteNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getModelRoute(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// TestGetModelRouteForbiddenIs403 proves a caller denied Get on a ModelRoute
// sees an honest 403.
func TestGetModelRouteForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "modelroutes"},
					"my-route", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getModelRoute(t, s, "my-route")
	require.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "forbidden")
}

// =============================================================================
// POST /api/modelroutes — create
// =============================================================================

// TestCreateModelRouteMock proves a valid mock-provider ModelRoute (no
// secretBindingRef needed) is created and the detail DTO is returned.
func TestCreateModelRouteMock(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ModelRouteCreateRequest{
		Name:      "mock-route",
		Namespace: mrNS,
		Providers: []ModelRouteProviderDTO{
			{Provider: "mock", Model: "mock-default", Priority: 1},
		},
	}
	detail, code, body := createModelRoute(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "mock-route", detail.Name)
	assert.Equal(t, mrNS, detail.Namespace)
	require.Len(t, detail.Providers, 1)
	assert.Equal(t, "mock", detail.Providers[0].Provider)

	// Confirm it landed in the fake store.
	var got agentsv1alpha1.ModelRoute
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: mrNS, Name: "mock-route"}, &got))
	assert.Equal(t, "mock", got.Spec.Providers[0].Provider)
}

// TestCreateModelRouteWithAPIBase proves a non-mock provider with apiBase set
// is accepted (no secretBindingRef required when apiBase is present).
func TestCreateModelRouteWithAPIBase(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ModelRouteCreateRequest{
		Name:      "api-base-route",
		Namespace: mrNS,
		Providers: []ModelRouteProviderDTO{
			{
				Provider: "openai",
				Model:    "gpt-4o",
				Priority: 1,
				APIBase:  "http://tool-mock.ns.svc.cluster.local:9099/v1",
			},
		},
	}
	detail, code, body := createModelRoute(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "http://tool-mock.ns.svc.cluster.local:9099/v1", detail.Providers[0].APIBase)
}

// TestCreateModelRouteWithSecretBindingRef proves a non-mock provider with a
// secretBindingRef is accepted.
func TestCreateModelRouteWithSecretBindingRef(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ModelRouteCreateRequest{
		Name:      "anthropic-route",
		Namespace: mrNS,
		Providers: []ModelRouteProviderDTO{
			{
				Provider:         "anthropic",
				Model:            "claude-sonnet-4-6",
				Priority:         1,
				SecretBindingRef: "my-secret",
			},
		},
	}
	detail, code, body := createModelRoute(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-secret", detail.Providers[0].SecretBindingRef)
}

// TestCreateModelRouteInvalidBodyIs400 proves a missing body yields 400.
func TestCreateModelRouteInvalidBodyIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Empty providers list → our pre-validation returns 400 before hitting the API server.
	req := ModelRouteCreateRequest{Name: "bad-route", Namespace: mrNS}
	_, code, body := createModelRoute(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "providers")
}

// TestCreateModelRouteMissingNameIs400 proves a missing name yields 400.
func TestCreateModelRouteMissingNameIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ModelRouteCreateRequest{
		// Name is empty.
		Namespace: mrNS,
		Providers: []ModelRouteProviderDTO{
			{Provider: "mock", Model: "mock-default", Priority: 1},
		},
	}
	_, code, body := createModelRoute(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "name")
}

// TestCreateModelRouteAPIServerRejectionSurfaces4xx proves that when the API
// server rejects a create (e.g. invalid spec the CRD XValidation catches), the
// BFF surfaces the rejection as an honest 4xx (422/400), never a 500. We
// simulate the API-server rejection via an interceptor that returns apierrors.Invalid.
func TestCreateModelRouteAPIServerRejectionSurfaces4xx(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				// Simulate the CRD XValidation: secretBindingRef required for non-mock
				// provider unless apiBase is set.
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: modelRouteKind},
					obj.GetName(),
					nil,
				)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// A non-mock provider with neither secretBindingRef nor apiBase: our pre-flight
	// validation passes (we don't replicate CRD logic), so the interceptor's
	// rejection is what surfaces the 4xx.
	req := ModelRouteCreateRequest{
		Name:      "bad-route",
		Namespace: mrNS,
		Providers: []ModelRouteProviderDTO{
			// Mock so our pre-flight doesn't reject it; the interceptor simulates
			// the XValidation-based API server rejection.
			{Provider: "mock", Model: "mock-default", Priority: 1},
		},
	}
	_, code, body := createModelRoute(t, s, req)
	// Must be a 4xx, never 5xx.
	assert.True(t, code >= 400 && code < 500, "API server rejection must surface as 4xx, got %d: %s", code, body)
}

// TestCreateModelRouteAlreadyExistsIs409 proves a second create with the same
// name surfaces as 409 Conflict.
func TestCreateModelRouteAlreadyExistsIs409(t *testing.T) {
	existing := mockModelRoute("my-route", mrNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ModelRouteCreateRequest{
		Name:      "my-route",
		Namespace: mrNS,
		Providers: []ModelRouteProviderDTO{
			{Provider: "mock", Model: "mock-default", Priority: 1},
		},
	}
	_, code, body := createModelRoute(t, s, req)
	assert.Equal(t, http.StatusConflict, code, "body: %s", body)
	assert.Contains(t, body, "already exists")
}

// TestCreateModelRouteForbiddenIs403 proves a viewer's create surfaces the
// API server's real 403.
func TestCreateModelRouteForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "modelroutes"},
					obj.GetName(), errors.New("viewer cannot create"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := ModelRouteCreateRequest{
		Name:      "no-perm-route",
		Namespace: mrNS,
		Providers: []ModelRouteProviderDTO{
			{Provider: "mock", Model: "mock-default", Priority: 1},
		},
	}
	_, code, body := createModelRoute(t, s, req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	// The CALLER'S token reached the factory.
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestCreateModelRouteWithoutTokenIs401 proves a token-less POST is rejected
// 401 before any K8s call.
func TestCreateModelRouteWithoutTokenIs401(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				createCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(ModelRouteCreateRequest{
		Name:      "route",
		Namespace: mrNS,
		Providers: []ModelRouteProviderDTO{{Provider: "mock", Model: "m", Priority: 1}},
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/modelroutes", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, createCalled, "no K8s create must run for a token-less request")
}

// =============================================================================
// PUT /api/modelroutes/{ns}/{name} — update via SSA
// =============================================================================

// TestUpdateModelRouteEditsField proves a PUT edits a field via SSA and the
// changed value is visible in the fake store.
func TestUpdateModelRouteEditsField(t *testing.T) {
	existing := mockModelRoute("my-route", mrNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Change the model name on the mock provider.
	req := ModelRouteUpdateRequest{
		Name: "my-route",
		Providers: []ModelRouteProviderDTO{
			{Provider: "mock", Model: "mock-v2", Priority: 1},
		},
	}
	detail, code, body := putModelRoute(t, s, "my-route", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "mock-v2", detail.Providers[0].Model)

	// Confirm the change landed.
	var got agentsv1alpha1.ModelRoute
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: mrNS, Name: "my-route"}, &got))
	assert.Equal(t, "mock-v2", got.Spec.Providers[0].Model)
}

// TestUpdateModelRoutePreservesAPIBase proves PUT preserves the apiBase field
// when included in the update (m14.12b — never silently stripped).
func TestUpdateModelRoutePreservesAPIBase(t *testing.T) {
	existing := apiBaseModelRoute("api-route", mrNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ModelRouteUpdateRequest{
		Providers: []ModelRouteProviderDTO{
			{
				Provider: "openai",
				Model:    "gpt-4o-mini",
				Priority: 1,
				APIBase:  "http://tool-mock.ns.svc.cluster.local:9099/v1",
			},
		},
	}
	detail, code, body := putModelRoute(t, s, "api-route", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "http://tool-mock.ns.svc.cluster.local:9099/v1", detail.Providers[0].APIBase)
	assert.Equal(t, "gpt-4o-mini", detail.Providers[0].Model)
}

// TestUpdateModelRouteRenameGuardIs400 proves a spec name that does not match
// the URL name is rejected 400 (a PUT is not a rename).
func TestUpdateModelRouteRenameGuardIs400(t *testing.T) {
	existing := mockModelRoute("my-route", mrNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ModelRouteUpdateRequest{
		Name: "different-name", // mismatch
		Providers: []ModelRouteProviderDTO{
			{Provider: "mock", Model: "mock-default", Priority: 1},
		},
	}
	_, code, body := putModelRoute(t, s, "my-route", req)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, strings.ToLower(body), "rename")
}

// TestUpdateModelRouteAbsentNameInBodyIsOK proves that omitting the Name in the
// body does not trigger the rename guard — the URL is authoritative.
func TestUpdateModelRouteAbsentNameInBodyIsOK(t *testing.T) {
	existing := mockModelRoute("my-route", mrNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := ModelRouteUpdateRequest{
		// Name is empty — URL name is authoritative, no rename guard triggered.
		Providers: []ModelRouteProviderDTO{
			{Provider: "mock", Model: "mock-updated", Priority: 1},
		},
	}
	detail, code, body := putModelRoute(t, s, "my-route", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "mock-updated", detail.Providers[0].Model)
}

// TestUpdateModelRouteForbiddenIs403 proves a viewer's PUT surfaces the API
// server's real 403 — the BFF never pre-empts the decision (ADR 0011).
func TestUpdateModelRouteForbiddenIs403(t *testing.T) {
	existing := mockModelRoute("my-route", mrNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "modelroutes"},
					obj.GetName(), errors.New("viewer cannot update"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := ModelRouteUpdateRequest{
		Providers: []ModelRouteProviderDTO{
			{Provider: "mock", Model: "mock-default", Priority: 1},
		},
	}
	_, code, body := putModelRoute(t, s, "my-route", req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestUpdateModelRouteWithoutTokenIs401 proves a token-less PUT is rejected
// 401 before any K8s call.
func TestUpdateModelRouteWithoutTokenIs401(t *testing.T) {
	patchCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patchCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(ModelRouteUpdateRequest{
		Providers: []ModelRouteProviderDTO{{Provider: "mock", Model: "m", Priority: 1}},
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/modelroutes/"+mrNS+"/my-route", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, patchCalled, "no K8s patch must run for a token-less request")
}

// =============================================================================
// DELETE /api/modelroutes/{ns}/{name} — delete
// =============================================================================

// TestDeleteModelRouteRemovesObject proves a DELETE succeeds (204) and the
// ModelRoute is gone from the fake store.
func TestDeleteModelRouteRemovesObject(t *testing.T) {
	mr := mockModelRoute("my-route", mrNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mr).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteModelRoute(t, s, "my-route")
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	var got agentsv1alpha1.ModelRoute
	err := c.Get(context.Background(), client.ObjectKey{Namespace: mrNS, Name: "my-route"}, &got)
	require.True(t, apierrors.IsNotFound(err), "ModelRoute must be gone after a successful DELETE")
}

// TestDeleteModelRouteNotFoundIs404 proves deleting a missing ModelRoute yields 404.
func TestDeleteModelRouteNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteModelRoute(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestDeleteModelRouteForbiddenIs403 proves a viewer's DELETE surfaces the API
// server's real 403 — the BFF never pre-empts the decision (ADR 0011).
func TestDeleteModelRouteForbiddenIs403(t *testing.T) {
	mr := mockModelRoute("my-route", mrNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mr).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "modelroutes"},
					obj.GetName(), errors.New("viewer cannot delete"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := deleteModelRoute(t, s, "my-route")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestDeleteModelRouteWithoutTokenIs401 proves a token-less DELETE is rejected
// 401 before any K8s call.
func TestDeleteModelRouteWithoutTokenIs401(t *testing.T) {
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
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/modelroutes/"+mrNS+"/my-route", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, deleteCalled, "no K8s delete must run for a token-less request")
}
