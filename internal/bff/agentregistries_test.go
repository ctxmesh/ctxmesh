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

// arNS is the namespace used in AgentRegistry tests.
const arNS = "team-reg"

// --- fixture helpers --------------------------------------------------------

// mockAgentRegistry builds a minimal AgentRegistry with the given registryId
// and an empty memberSelector. Safe for direct create in the fake store.
func mockAgentRegistry(name, ns, registryId string) *agentsv1alpha1.AgentRegistry {
	return &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			RegistryId:     registryId,
			MemberSelector: metav1.LabelSelector{},
		},
	}
}

// fullAgentRegistry builds an AgentRegistry with all editable spec fields set.
// name is kept as a parameter for future multi-name tests — the unparam linter
// is suppressed because the signature should not be over-specialised.
//
//nolint:unparam
func fullAgentRegistry(name, ns, registryId string) *agentsv1alpha1.AgentRegistry {
	return &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			RegistryId: registryId,
			MemberSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"registry": registryId},
			},
			Guards: &agentsv1alpha1.RegistryGuards{
				MaxDepth:  4,
				HopBudget: 16,
			},
			Roles: []string{"orchestrator", "reviewer"},
		},
	}
}

// readyAR sets the Ready condition on an AgentRegistry (simulates a reconciled
// object where the controller has resolved memberSelector and injected guard defaults).
func readyAR(ar *agentsv1alpha1.AgentRegistry) *agentsv1alpha1.AgentRegistry {
	ar.Status.Conditions = []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciled",
			Message:            "member agents annotated and guards injected",
			LastTransitionTime: metav1.Now(),
		},
	}
	return ar
}

// withMembers sets the status.members on an AgentRegistry (simulates the
// controller having resolved memberSelector to a concrete member list).
func withMembers(ar *agentsv1alpha1.AgentRegistry, members ...string) *agentsv1alpha1.AgentRegistry {
	ar.Status.Members = members
	return ar
}

// --- request helpers --------------------------------------------------------

// getAgentRegistries drives GET /api/agentregistries with a caller token and the
// given raw query string. Returns the decoded response and the HTTP status code.
func getAgentRegistries(t *testing.T, s *Server, rawQuery string) (AgentRegistryListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	url := "/api/agentregistries"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body AgentRegistryListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

// getAgentRegistry drives GET /api/agentregistries/{arNS}/{name} with a caller
// token. Returns the decoded detail DTO, the HTTP status, and the raw body string.
func getAgentRegistry(t *testing.T, s *Server, name string) (*AgentRegistryDetail, int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agentregistries/"+arNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail AgentRegistryDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// createAgentRegistry drives POST /api/agentregistries with the given request body.
func createAgentRegistry(t *testing.T, s *Server, reqBody AgentRegistryCreateRequest) (*AgentRegistryDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agentregistries", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var detail AgentRegistryDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// putAgentRegistry drives PUT /api/agentregistries/arNS/{name} with the given
// request body. The name parameter is kept for future multi-name tests —
// the unparam linter is suppressed because the signature should not be over-specialised.
//
//nolint:unparam
func putAgentRegistry(t *testing.T, s *Server, name string, reqBody AgentRegistryUpdateRequest) (*AgentRegistryDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/agentregistries/"+arNS+"/"+name, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail AgentRegistryDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// deleteAgentRegistry drives DELETE /api/agentregistries/arNS/{name} with a
// caller token. Returns the raw response recorder.
func deleteAgentRegistry(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/agentregistries/"+arNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// =============================================================================
// GET /api/agentregistries — list contract
// =============================================================================

// TestListAgentRegistriesEmpty proves an empty cluster yields
// {"items":[],"nextCursor":""} — never null slices.
func TestListAgentRegistriesEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getAgentRegistries(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items, "items must be [] not null")
	assert.Empty(t, body.Items)
	assert.Empty(t, body.NextCursor)
}

// TestListAgentRegistriesReturnsItems proves seeded AgentRegistries appear in the
// response with the correct projections (registryId, memberSelector, guards, roles).
func TestListAgentRegistriesReturnsItems(t *testing.T) {
	objs := []client.Object{
		mockAgentRegistry("registry-a", arNS, "reg-a"),
		mockAgentRegistry("registry-b", arNS, "reg-b"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getAgentRegistries(t, s, "namespace="+arNS)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
		assert.Equal(t, arNS, item.Namespace)
		// Roles must be [] not nil.
		assert.NotNil(t, item.Roles, "roles must be [] not null")
	}
	assert.True(t, names["registry-a"])
	assert.True(t, names["registry-b"])
}

// TestListAgentRegistriesQFilter proves ?q is a case-insensitive windowed
// substring filter on the registry name.
func TestListAgentRegistriesQFilter(t *testing.T) {
	objs := []client.Object{
		mockAgentRegistry("prod-registry", arNS, "prod"),
		mockAgentRegistry("PROD-staging", arNS, "prod-staging"),
		mockAgentRegistry("dev-registry", arNS, "dev"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getAgentRegistries(t, s, "q=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2, "q must match both prod variants case-insensitively")
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
	}
	assert.True(t, names["prod-registry"])
	assert.True(t, names["PROD-staging"])
	assert.False(t, names["dev-registry"])

	// No match → [] not null.
	body, code = getAgentRegistries(t, s, "q=zzz-nomatch")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items)
	assert.Empty(t, body.Items)
}

// TestListAgentRegistriesNamespaceScoping proves ?namespace scopes the list and
// an absent ?namespace returns all namespaces.
func TestListAgentRegistriesNamespaceScoping(t *testing.T) {
	objs := []client.Object{
		mockAgentRegistry("prod-reg", "prod", "prod"),
		mockAgentRegistry("dev-reg", "dev", "dev"),
		mockAgentRegistry("dev-reg2", "dev", "dev2"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Scoped.
	body, code := getAgentRegistries(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "prod", body.Items[0].Namespace)

	// Unscoped → all.
	body, code = getAgentRegistries(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, body.Items, 3)
}

// TestListAgentRegistriesLimitAndCursor proves limit/cursor paging works with
// the list contract (same pattern as ModelRoute and SecretBinding tests).
func TestListAgentRegistriesLimitAndCursor(t *testing.T) {
	all := []*agentsv1alpha1.AgentRegistry{
		mockAgentRegistry("reg-000", arNS, "r000"),
		mockAgentRegistry("reg-001", arNS, "r001"),
		mockAgentRegistry("reg-002", arNS, "r002"),
		mockAgentRegistry("reg-003", arNS, "r003"),
		mockAgentRegistry("reg-004", arNS, "r004"),
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
			arList, ok := list.(*agentsv1alpha1.AgentRegistryList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			for _, ar := range all[start:end] {
				arList.Items = append(arList.Items, *ar)
			}
			if end < len(all) {
				arList.Continue = strconv.Itoa(end)
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
	page1, code := getAgentRegistries(t, s, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 2)
	require.NotEmpty(t, page1.NextCursor, "a non-exhausted list must expose a nextCursor")

	// Page 2 via cursor round-trip.
	page2, code := getAgentRegistries(t, s, "limit=2&cursor="+page1.NextCursor)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page2.Items, 2)
	assert.NotEqual(t, page1.Items[0].Name, page2.Items[0].Name, "page 2 must be a different window")

	// Drain to exhaustion.
	seen := len(page1.Items) + len(page2.Items)
	cursor := page2.NextCursor
	for cursor != "" {
		next, code := getAgentRegistries(t, s, "limit=2&cursor="+cursor)
		require.Equal(t, http.StatusOK, code)
		seen += len(next.Items)
		cursor = next.NextCursor
	}
	assert.Equal(t, 5, seen, "paging must visit every registry exactly once")
}

// TestListAgentRegistriesForbiddenIs403 proves a Forbidden on the list surfaces
// as 403, not an empty [].
func TestListAgentRegistriesForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "agentregistries"},
					"", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code := getAgentRegistries(t, s, "")
	require.Equal(t, http.StatusForbidden, code)
}

// TestListAgentRegistriesNoEgressField is THE NO-EGRESS ASSERTION for the list
// response. It proves that none of the list items contain an egress/allowlist
// field — the console cannot alter the controller-owned NetworkPolicy through
// this surface (M6 whitelist + M11 default-deny preserved by construction).
func TestListAgentRegistriesNoEgressField(t *testing.T) {
	ar := fullAgentRegistry("my-registry", arNS, "my-reg")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ar).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agentregistries?namespace="+arNS, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	items, ok := raw["items"].([]any)
	require.True(t, ok, "response must have an items array")
	require.NotEmpty(t, items)

	// THE NO-EGRESS PROPERTY: none of these keys may appear in any list item.
	forbiddenKeys := []string{"egress", "allowlist", "networkPolicy", "egressPolicy", "egressRules"}
	for _, itemAny := range items {
		item, ok := itemAny.(map[string]any)
		require.True(t, ok)
		for _, k := range forbiddenKeys {
			_, present := item[k]
			assert.False(t, present, "list item must NOT contain field %q (egress is controller-owned)", k)
		}
	}
}

// =============================================================================
// GET /api/agentregistries/{ns}/{name} — detail
// =============================================================================

// TestGetAgentRegistryReturnsDetail proves a seeded AgentRegistry is returned
// with all editable spec fields projected correctly: registryId, memberSelector,
// guards, roles, and status (members, phase, ready).
func TestGetAgentRegistryReturnsDetail(t *testing.T) {
	ar := readyAR(withMembers(fullAgentRegistry("my-registry", arNS, "my-reg"), "agent-a", "agent-b"))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ar).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getAgentRegistry(t, s, "my-registry")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-registry", detail.Name)
	assert.Equal(t, arNS, detail.Namespace)
	assert.Equal(t, "my-reg", detail.RegistryId)
	assert.Equal(t, map[string]string{"registry": "my-reg"}, detail.MemberSelector.MatchLabels)
	require.NotNil(t, detail.Guards)
	assert.Equal(t, int32(4), detail.Guards.MaxDepth)
	assert.Equal(t, int32(16), detail.Guards.HopBudget)
	assert.Equal(t, []string{"orchestrator", "reviewer"}, detail.Roles)
	// Status.
	assert.True(t, detail.Status.Ready)
	assert.Equal(t, phaseReady, detail.Status.Phase)
	assert.Equal(t, []string{"agent-a", "agent-b"}, detail.Status.Members)
}

// TestGetAgentRegistryNoEgressField is THE NO-EGRESS ASSERTION for the detail
// response. It proves the detail DTO has NO egress/allowlist field — the console
// cannot alter the controller-owned NetworkPolicy through this surface.
func TestGetAgentRegistryNoEgressField(t *testing.T) {
	ar := fullAgentRegistry("my-registry", arNS, "my-reg")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ar).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agentregistries/"+arNS+"/my-registry", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw), "response must be valid JSON")

	// THE NO-EGRESS PROPERTY: none of these keys may appear in the detail response.
	forbiddenKeys := []string{"egress", "allowlist", "networkPolicy", "egressPolicy", "egressRules"}
	for _, k := range forbiddenKeys {
		_, present := raw[k]
		assert.False(t, present, "detail response must NOT contain field %q (egress is controller-owned)", k)
	}
}

// TestGetAgentRegistryNotFoundIs404 proves a missing AgentRegistry yields 404.
func TestGetAgentRegistryNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getAgentRegistry(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// TestGetAgentRegistryForbiddenIs403 proves a caller denied Get sees an honest 403.
func TestGetAgentRegistryForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "agentregistries"},
					"my-registry", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getAgentRegistry(t, s, "my-registry")
	require.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "forbidden")
}

// =============================================================================
// POST /api/agentregistries — create
// =============================================================================

// TestCreateAgentRegistrySucceeds proves a valid AgentRegistry create returns
// 201 with the full detail DTO — including registryId, memberSelector, guards,
// roles, and an empty members list (controller hasn't reconciled yet).
func TestCreateAgentRegistrySucceeds(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryCreateRequest{
		Name:       "new-registry",
		Namespace:  arNS,
		RegistryId: "new-reg",
		MemberSelector: LabelSelectorDTO{
			MatchLabels: map[string]string{"registry": "new-reg"},
		},
		Guards: &RegistryGuardsDTO{MaxDepth: 6, HopBudget: 24},
		Roles:  []string{"orchestrator", "worker"},
	}
	detail, code, body := createAgentRegistry(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "new-registry", detail.Name)
	assert.Equal(t, arNS, detail.Namespace)
	assert.Equal(t, "new-reg", detail.RegistryId)
	assert.Equal(t, map[string]string{"registry": "new-reg"}, detail.MemberSelector.MatchLabels)
	require.NotNil(t, detail.Guards)
	assert.Equal(t, int32(6), detail.Guards.MaxDepth)
	assert.Equal(t, int32(24), detail.Guards.HopBudget)
	assert.Equal(t, []string{"orchestrator", "worker"}, detail.Roles)
	// Members is [] not nil before the controller reconciles.
	assert.NotNil(t, detail.Status.Members, "status.members must be [] not null")

	// Confirm it landed in the fake store.
	var got agentsv1alpha1.AgentRegistry
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: arNS, Name: "new-registry"}, &got))
	assert.Equal(t, "new-reg", got.Spec.RegistryId)
	assert.Equal(t, map[string]string{"registry": "new-reg"}, got.Spec.MemberSelector.MatchLabels)
	require.NotNil(t, got.Spec.Guards)
	assert.Equal(t, int32(6), got.Spec.Guards.MaxDepth)
	assert.Equal(t, []string{"orchestrator", "worker"}, got.Spec.Roles)
}

// TestCreateAgentRegistryNoEgressField proves the create response has NO
// egress/allowlist field — the BFF does not write a NetworkPolicy and never
// exposes egress through the API surface.
func TestCreateAgentRegistryNoEgressField(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryCreateRequest{
		Name:       "egress-test",
		Namespace:  arNS,
		RegistryId: "egress-test",
	}
	rec := httptest.NewRecorder()
	b, _ := json.Marshal(req)
	httpReq := httptest.NewRequest(http.MethodPost, "/api/agentregistries", bytes.NewReader(b))
	httpReq.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, httpReq)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	forbiddenKeys := []string{"egress", "allowlist", "networkPolicy", "egressPolicy", "egressRules"}
	for _, k := range forbiddenKeys {
		_, present := raw[k]
		assert.False(t, present, "create response must NOT contain field %q", k)
	}
}

// TestCreateAgentRegistryMissingNameIs400 proves a missing name yields 400.
func TestCreateAgentRegistryMissingNameIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryCreateRequest{
		Namespace:  arNS,
		RegistryId: "my-reg",
	}
	_, code, body := createAgentRegistry(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "name")
}

// TestCreateAgentRegistryMissingRegistryIdIs400 proves a missing registryId yields 400.
func TestCreateAgentRegistryMissingRegistryIdIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryCreateRequest{
		Name:      "my-registry",
		Namespace: arNS,
		// RegistryId is empty.
	}
	_, code, body := createAgentRegistry(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "registryId")
}

// TestCreateAgentRegistryAlreadyExistsIs409 proves a duplicate create yields 409.
func TestCreateAgentRegistryAlreadyExistsIs409(t *testing.T) {
	existing := mockAgentRegistry("my-registry", arNS, "my-reg")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryCreateRequest{
		Name:       "my-registry",
		Namespace:  arNS,
		RegistryId: "my-reg",
	}
	_, code, body := createAgentRegistry(t, s, req)
	assert.Equal(t, http.StatusConflict, code, "body: %s", body)
	assert.Contains(t, body, "already exists")
}

// TestCreateAgentRegistryAPIServerRejectionSurfaces4xx proves that when the API
// server rejects a create (e.g. a CRD XValidation fires), the BFF surfaces the
// rejection as an honest 4xx (422), never a 500. Simulated via an interceptor
// returning apierrors.Invalid (same pattern as modelroutes_test.go).
func TestCreateAgentRegistryAPIServerRejectionSurfaces4xx(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: agentRegistryKind},
					obj.GetName(),
					nil,
				)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryCreateRequest{
		Name:       "bad-registry",
		Namespace:  arNS,
		RegistryId: "bad-reg",
	}
	_, code, body := createAgentRegistry(t, s, req)
	assert.True(t, code >= 400 && code < 500, "API server rejection must surface as 4xx, got %d: %s", code, body)
}

// TestCreateAgentRegistryForbiddenIs403 proves a viewer's create surfaces the
// API server's real 403.
func TestCreateAgentRegistryForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "agentregistries"},
					obj.GetName(), errors.New("viewer cannot create"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := AgentRegistryCreateRequest{
		Name:       "no-perm-registry",
		Namespace:  arNS,
		RegistryId: "no-perm",
	}
	_, code, body := createAgentRegistry(t, s, req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestCreateAgentRegistryWithoutTokenIs401 proves a token-less POST is rejected
// 401 before any K8s call.
func TestCreateAgentRegistryWithoutTokenIs401(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				createCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(AgentRegistryCreateRequest{
		Name:       "registry",
		Namespace:  arNS,
		RegistryId: "reg",
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agentregistries", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, createCalled, "no K8s create must run for a token-less request")
}

// =============================================================================
// PUT /api/agentregistries/{ns}/{name} — update via SSA
// =============================================================================

// TestUpdateAgentRegistryEditsRoles proves a PUT updates roles via SSA and the
// changed value is visible in the fake store.
func TestUpdateAgentRegistryEditsRoles(t *testing.T) {
	existing := fullAgentRegistry("my-registry", arNS, "my-reg")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryUpdateRequest{
		Name: "my-registry",
		MemberSelector: LabelSelectorDTO{
			MatchLabels: map[string]string{"registry": "my-reg"},
		},
		Roles: []string{"orchestrator", "worker", "reviewer"},
	}
	detail, code, body := putAgentRegistry(t, s, "my-registry", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, []string{"orchestrator", "worker", "reviewer"}, detail.Roles)

	// Confirm the change landed.
	var got agentsv1alpha1.AgentRegistry
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: arNS, Name: "my-registry"}, &got))
	assert.Equal(t, []string{"orchestrator", "worker", "reviewer"}, got.Spec.Roles)
}

// TestUpdateAgentRegistryEditsGuards proves a PUT updates the guards spec via
// SSA and the changed value is visible in the fake store.
func TestUpdateAgentRegistryEditsGuards(t *testing.T) {
	existing := fullAgentRegistry("my-registry", arNS, "my-reg")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryUpdateRequest{
		MemberSelector: LabelSelectorDTO{
			MatchLabels: map[string]string{"registry": "my-reg"},
		},
		Guards: &RegistryGuardsDTO{MaxDepth: 10, HopBudget: 48},
	}
	detail, code, body := putAgentRegistry(t, s, "my-registry", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	require.NotNil(t, detail.Guards)
	assert.Equal(t, int32(10), detail.Guards.MaxDepth)
	assert.Equal(t, int32(48), detail.Guards.HopBudget)
}

// TestUpdateAgentRegistryPreservesRegistryId proves that a PUT does NOT change
// the registryId — it is read from the live object and re-applied unchanged.
// This ensures the immutability constraint is respected by construction (the
// caller cannot supply a new registryId in the update DTO).
func TestUpdateAgentRegistryPreservesRegistryId(t *testing.T) {
	existing := mockAgentRegistry("my-registry", arNS, "immutable-id")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryUpdateRequest{
		MemberSelector: LabelSelectorDTO{
			MatchLabels: map[string]string{"team": "alpha"},
		},
	}
	detail, code, body := putAgentRegistry(t, s, "my-registry", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	// registryId must be unchanged — it was not in the update request body.
	assert.Equal(t, "immutable-id", detail.RegistryId)
}

// TestUpdateAgentRegistryInvalidWriteSurfaces422 proves the error-MAPPING: when the
// API server rejects an SSA apply with an Invalid error, the BFF surfaces it as an
// honest 422 (never a 500). This exercises classifyAgentRegistryWriteError, NOT a
// registryId change — registryId cannot change through this path (the update DTO has
// no such field; the live value is always re-sent), so a registryId edit is a
// silent-preserve 200, not an Invalid. See TestUpdateAgentRegistryPreservesRegistryId
// for that behavior. Here we simulate a generic Invalid via an interceptor.
func TestUpdateAgentRegistryInvalidWriteSurfaces422(t *testing.T) {
	existing := mockAgentRegistry("my-registry", arNS, "original-id")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				// Simulate any API-server Invalid rejection (e.g. a CRD XValidation).
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: agentRegistryKind},
					obj.GetName(),
					nil,
				)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryUpdateRequest{
		MemberSelector: LabelSelectorDTO{},
	}
	_, code, body := putAgentRegistry(t, s, "my-registry", req)
	// An API-server Invalid must surface as 422 (Unprocessable Entity), never a 500.
	assert.Equal(t, http.StatusUnprocessableEntity, code, "API-server Invalid must surface as 422, got %d: %s", code, body)
}

// TestUpdateAgentRegistryRenameGuardIs400 proves a spec name that does not match
// the URL name is rejected 400 (a PUT is not a rename).
func TestUpdateAgentRegistryRenameGuardIs400(t *testing.T) {
	existing := mockAgentRegistry("my-registry", arNS, "my-reg")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryUpdateRequest{
		Name:           "different-name", // mismatch
		MemberSelector: LabelSelectorDTO{},
	}
	_, code, body := putAgentRegistry(t, s, "my-registry", req)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "rename")
}

// TestUpdateAgentRegistryAbsentNameInBodyIsOK proves omitting Name in the body
// does not trigger the rename guard — the URL is authoritative.
func TestUpdateAgentRegistryAbsentNameInBodyIsOK(t *testing.T) {
	existing := mockAgentRegistry("my-registry", arNS, "my-reg")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentRegistryUpdateRequest{
		// Name is empty — URL is authoritative.
		MemberSelector: LabelSelectorDTO{
			MatchLabels: map[string]string{"updated": "true"},
		},
	}
	detail, code, body := putAgentRegistry(t, s, "my-registry", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-registry", detail.Name)
}

// TestUpdateAgentRegistryForbiddenIs403 proves a viewer's PUT surfaces the API
// server's real 403 — the BFF never pre-empts the decision (ADR 0011).
func TestUpdateAgentRegistryForbiddenIs403(t *testing.T) {
	existing := mockAgentRegistry("my-registry", arNS, "my-reg")
	patchForbidden := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patchForbidden = true
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "agentregistries"},
					obj.GetName(), errors.New("viewer cannot update"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := AgentRegistryUpdateRequest{
		MemberSelector: LabelSelectorDTO{},
	}
	_, code, body := putAgentRegistry(t, s, "my-registry", req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
	assert.True(t, patchForbidden, "patch must have been attempted with the caller's client")
}

// TestUpdateAgentRegistryWithoutTokenIs401 proves a token-less PUT is rejected
// 401 before any K8s call.
func TestUpdateAgentRegistryWithoutTokenIs401(t *testing.T) {
	patchCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patchCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(AgentRegistryUpdateRequest{MemberSelector: LabelSelectorDTO{}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/agentregistries/"+arNS+"/my-registry", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, patchCalled, "no K8s patch must run for a token-less request")
}

// =============================================================================
// DELETE /api/agentregistries/{ns}/{name} — delete
// =============================================================================

// TestDeleteAgentRegistryRemovesObject proves a DELETE succeeds (204) and the
// AgentRegistry is gone from the fake store.
func TestDeleteAgentRegistryRemovesObject(t *testing.T) {
	ar := mockAgentRegistry("my-registry", arNS, "my-reg")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ar).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteAgentRegistry(t, s, "my-registry")
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	var got agentsv1alpha1.AgentRegistry
	err := c.Get(context.Background(), client.ObjectKey{Namespace: arNS, Name: "my-registry"}, &got)
	require.True(t, apierrors.IsNotFound(err), "AgentRegistry must be gone after a successful DELETE")
}

// TestDeleteAgentRegistryNotFoundIs404 proves deleting a missing AgentRegistry
// yields 404.
func TestDeleteAgentRegistryNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteAgentRegistry(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestDeleteAgentRegistryForbiddenIs403 proves a viewer's DELETE surfaces the
// API server's real 403 — the BFF never pre-empts the decision (ADR 0011).
func TestDeleteAgentRegistryForbiddenIs403(t *testing.T) {
	ar := mockAgentRegistry("my-registry", arNS, "my-reg")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ar).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "agentregistries"},
					obj.GetName(), errors.New("viewer cannot delete"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := deleteAgentRegistry(t, s, "my-registry")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestDeleteAgentRegistryWithoutTokenIs401 proves a token-less DELETE is
// rejected 401 before any K8s call.
func TestDeleteAgentRegistryWithoutTokenIs401(t *testing.T) {
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
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/agentregistries/"+arNS+"/my-registry", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, deleteCalled, "no K8s delete must run for a token-less request")
}
