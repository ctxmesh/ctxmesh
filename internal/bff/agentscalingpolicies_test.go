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

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// aspNS is the namespace used in AgentScalingPolicy tests.
const aspNS = "team-scaling"

// --- fixture helpers --------------------------------------------------------

// mockASP builds a minimal AgentScalingPolicy with request-rate trigger.
func mockASP(name, ns, agentRef string) *agentsv1alpha1.AgentScalingPolicy {
	return &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.AgentScalingPolicySpec{
			AgentRef: agentRef,
			Trigger:  "request-rate",
			Min:      0,
			Max:      5,
		},
	}
}

// mockASPSchedule builds an AgentScalingPolicy with schedule trigger.
func mockASPSchedule(name, ns, agentRef, schedule string) *agentsv1alpha1.AgentScalingPolicy {
	return &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.AgentScalingPolicySpec{
			AgentRef: agentRef,
			Trigger:  "schedule",
			Min:      1,
			Max:      3,
			Schedule: schedule,
		},
	}
}

// readyASP sets the Ready condition and backend on an AgentScalingPolicy.
func readyASP(p *agentsv1alpha1.AgentScalingPolicy) *agentsv1alpha1.AgentScalingPolicy {
	p.Status.Backend = "knative-annotations"
	p.Status.Conditions = []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciled",
			Message:            "backend configured",
			LastTransitionTime: metav1.Now(),
		},
	}
	return p
}

// --- request helpers --------------------------------------------------------

func getAgentScalingPolicies(t *testing.T, s *Server, rawQuery string) (AgentScalingPolicyListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	url := "/api/agentscalingpolicies"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body AgentScalingPolicyListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

func getAgentScalingPolicy(t *testing.T, s *Server, ns, name string) (*AgentScalingPolicyDetail, int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agentscalingpolicies/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail AgentScalingPolicyDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func createAgentScalingPolicy(t *testing.T, s *Server, reqBody AgentScalingPolicyCreateRequest) (*AgentScalingPolicyDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agentscalingpolicies", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var detail AgentScalingPolicyDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// putAgentScalingPolicy drives PUT /api/agentscalingpolicies/{ns}/{name}.
// ns is always aspNS in this test package — unparam is suppressed for the same
// reason as the analogous helper in modelroutes_test.go (shared test helper).
func putAgentScalingPolicy(t *testing.T, s *Server, ns, name string, reqBody AgentScalingPolicyUpdateRequest) (*AgentScalingPolicyDetail, int, string) { //nolint:unparam
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/agentscalingpolicies/"+ns+"/"+name, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail AgentScalingPolicyDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func deleteAgentScalingPolicy(t *testing.T, s *Server, ns, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/agentscalingpolicies/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// =============================================================================
// GET /api/agentscalingpolicies — list contract
// =============================================================================

// TestListAgentScalingPoliciesEmpty proves an empty cluster yields
// {"items":[],"nextCursor":""} — never null slices.
func TestListAgentScalingPoliciesEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getAgentScalingPolicies(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items, "items must be [] not null")
	assert.Empty(t, body.Items)
	assert.Empty(t, body.NextCursor)
}

// TestListAgentScalingPoliciesReturnsItems proves seeded policies appear with
// the correct projections.
func TestListAgentScalingPoliciesReturnsItems(t *testing.T) {
	objs := []client.Object{
		mockASP("policy-a", aspNS, "agent-a"),
		mockASP("policy-b", aspNS, "agent-b"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getAgentScalingPolicies(t, s, "namespace="+aspNS)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	agentRefs := map[string]bool{}
	for _, item := range body.Items {
		agentRefs[item.AgentRef] = true
		assert.Equal(t, aspNS, item.Namespace)
		assert.Equal(t, "request-rate", item.Trigger)
		assert.Equal(t, int32(5), item.Max)
	}
	assert.True(t, agentRefs["agent-a"])
	assert.True(t, agentRefs["agent-b"])
}

// TestListAgentScalingPoliciesLimitAndCursor proves limit/cursor pagination.
func TestListAgentScalingPoliciesLimitAndCursor(t *testing.T) {
	all := []*agentsv1alpha1.AgentScalingPolicy{
		mockASP("asp-000", aspNS, "agent-0"),
		mockASP("asp-001", aspNS, "agent-1"),
		mockASP("asp-002", aspNS, "agent-2"),
		mockASP("asp-003", aspNS, "agent-3"),
		mockASP("asp-004", aspNS, "agent-4"),
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
			aspList, ok := list.(*agentsv1alpha1.AgentScalingPolicyList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			for _, p := range all[start:end] {
				aspList.Items = append(aspList.Items, *p)
			}
			if end < len(all) {
				aspList.Continue = strconv.Itoa(end)
			}
			return nil
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(pagingFn).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	page1, code := getAgentScalingPolicies(t, s, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 2)
	require.NotEmpty(t, page1.NextCursor)

	page2, code := getAgentScalingPolicies(t, s, "limit=2&cursor="+page1.NextCursor)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page2.Items, 2)
	assert.NotEqual(t, page1.Items[0].Name, page2.Items[0].Name)

	seen := len(page1.Items) + len(page2.Items)
	cursor := page2.NextCursor
	for cursor != "" {
		next, c2 := getAgentScalingPolicies(t, s, "limit=2&cursor="+cursor)
		require.Equal(t, http.StatusOK, c2)
		seen += len(next.Items)
		cursor = next.NextCursor
	}
	assert.Equal(t, 5, seen, "paging must visit every policy exactly once")
}

// TestListAgentScalingPoliciesQFilter proves ?q filters by name substring.
func TestListAgentScalingPoliciesQFilter(t *testing.T) {
	objs := []client.Object{
		mockASP("scale-prod", aspNS, "agent-prod"),
		mockASP("SCALE-dev", aspNS, "agent-dev"),
		mockASP("other-policy", aspNS, "agent-other"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getAgentScalingPolicies(t, s, "q=scale")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)

	body, code = getAgentScalingPolicies(t, s, "q=zzz-nomatch")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items)
	assert.Empty(t, body.Items)
}

// TestListAgentScalingPoliciesNamespaceScoping proves ?namespace scoping.
func TestListAgentScalingPoliciesNamespaceScoping(t *testing.T) {
	objs := []client.Object{
		mockASP("asp-prod", "prod", "agent-prod"),
		mockASP("asp-dev", "dev", "agent-dev"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getAgentScalingPolicies(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "prod", body.Items[0].Namespace)
}

// TestListAgentScalingPoliciesForbiddenIs403 proves Forbidden on list → 403.
func TestListAgentScalingPoliciesForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "agentscalingpolicies"},
					"", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code := getAgentScalingPolicies(t, s, "")
	require.Equal(t, http.StatusForbidden, code)
}

// =============================================================================
// GET /api/agentscalingpolicies/{ns}/{name} — detail
// =============================================================================

// TestGetAgentScalingPolicyReturnsDetail proves the detail DTO includes backend
// from status and all spec fields.
func TestGetAgentScalingPolicyReturnsDetail(t *testing.T) {
	p := readyASP(mockASPSchedule("my-asp", aspNS, "my-agent", "*/5 * * * *"))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(p).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getAgentScalingPolicy(t, s, aspNS, "my-asp")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-asp", detail.Name)
	assert.Equal(t, aspNS, detail.Namespace)
	assert.Equal(t, "my-agent", detail.AgentRef)
	assert.Equal(t, "schedule", detail.Trigger)
	assert.Equal(t, "*/5 * * * *", detail.Schedule)
	assert.Equal(t, int32(1), detail.Min)
	assert.Equal(t, int32(3), detail.Max)
	// Backend comes from the controller-owned status.
	assert.Equal(t, "knative-annotations", detail.Backend)
	assert.True(t, detail.Ready)
	assert.Equal(t, phaseReady, detail.Phase)
}

// TestGetAgentScalingPolicyNotFoundIs404 proves a missing policy yields 404.
func TestGetAgentScalingPolicyNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getAgentScalingPolicy(t, s, aspNS, "ghost")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// TestGetAgentScalingPolicyForbiddenIs403 proves a denied Get → 403.
func TestGetAgentScalingPolicyForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "agentscalingpolicies"},
					"my-asp", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getAgentScalingPolicy(t, s, aspNS, "my-asp")
	require.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "forbidden")
}

// =============================================================================
// POST /api/agentscalingpolicies — create
// =============================================================================

// TestCreateAgentScalingPolicyBasic proves a valid create returns 201.
func TestCreateAgentScalingPolicyBasic(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentScalingPolicyCreateRequest{
		Name:      "my-asp",
		Namespace: aspNS,
		AgentRef:  "my-agent",
		Trigger:   "request-rate",
		Min:       0,
		Max:       10,
	}
	detail, code, body := createAgentScalingPolicy(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-asp", detail.Name)
	assert.Equal(t, "my-agent", detail.AgentRef)
	assert.Equal(t, "request-rate", detail.Trigger)
	assert.Equal(t, int32(10), detail.Max)

	// Confirm it landed in the fake store.
	var got agentsv1alpha1.AgentScalingPolicy
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: aspNS, Name: "my-asp"}, &got))
	assert.Equal(t, "my-agent", got.Spec.AgentRef)
}

// TestCreateAgentScalingPolicyScheduleTrigger proves a schedule-trigger policy
// with a schedule field is accepted.
func TestCreateAgentScalingPolicyScheduleTrigger(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentScalingPolicyCreateRequest{
		Name:      "cron-asp",
		Namespace: aspNS,
		AgentRef:  "batch-agent",
		Trigger:   "schedule",
		Min:       1,
		Max:       3,
		Schedule:  "0 * * * *",
	}
	detail, code, body := createAgentScalingPolicy(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "0 * * * *", detail.Schedule)
}

// TestCreateAgentScalingPolicyMaxLessThanMinIs422 proves that when the API server
// enforces the CRD XValidation (max >= min), the BFF surfaces the rejection as
// honest 422 — never a 500.
//
// Implementation note: the tier1 envtest exercises the REAL CRD rule with a live
// API server. Unit tests simulate the API-server Invalid error via an interceptor
// (the fake client does not enforce CRD XValidations). This pattern mirrors
// TestCreateModelRouteAPIServerRejectionSurfaces4xx.
func TestCreateAgentScalingPolicyMaxLessThanMinIs422(t *testing.T) {
	// Intercept the Create call and return the same error the real API server
	// would return when max < min (XValidation: max must be >= min).
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: agentScalingPolicyKind},
					obj.GetName(),
					nil, // field errors (abbreviated for the unit test)
				)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Submit max=1 < min=5 — our pre-flight does NOT re-implement this check;
	// the interceptor simulates the API-server XValidation rejection.
	req := AgentScalingPolicyCreateRequest{
		Name:      "bad-asp",
		Namespace: aspNS,
		AgentRef:  "agent-x",
		Trigger:   "request-rate",
		Min:       5,
		Max:       1, // violates max >= min
	}
	_, code, body := createAgentScalingPolicy(t, s, req)
	// Must be 422 (IsInvalid → StatusUnprocessableEntity), never a 5xx.
	assert.Equal(t, http.StatusUnprocessableEntity, code, "max<min must surface as 422, got %d: %s", code, body)
}

// TestCreateAgentScalingPolicyScheduleMissingIs422 proves that a schedule-trigger
// without a schedule field surfaces as 422 when the API server rejects it.
func TestCreateAgentScalingPolicyScheduleMissingIs422(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				// Simulate: "schedule is required when trigger is 'schedule'".
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: agentScalingPolicyKind},
					obj.GetName(),
					nil,
				)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentScalingPolicyCreateRequest{
		Name:      "sched-asp",
		Namespace: aspNS,
		AgentRef:  "batch-agent",
		Trigger:   "schedule",
		Min:       0,
		Max:       1,
		// Schedule intentionally omitted — violates the CRD XValidation.
	}
	_, code, body := createAgentScalingPolicy(t, s, req)
	assert.Equal(t, http.StatusUnprocessableEntity, code, "schedule-missing must surface as 422, got %d: %s", code, body)
}

// TestCreateAgentScalingPolicyMissingNameIs400 proves a missing name yields 400.
func TestCreateAgentScalingPolicyMissingNameIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentScalingPolicyCreateRequest{AgentRef: "agent-x", Trigger: "request-rate", Max: 5}
	_, code, body := createAgentScalingPolicy(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "name")
}

// TestCreateAgentScalingPolicyMissingAgentRefIs400 proves a missing agentRef
// yields 400.
func TestCreateAgentScalingPolicyMissingAgentRefIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentScalingPolicyCreateRequest{Name: "my-asp", Namespace: aspNS, Trigger: "request-rate", Max: 5}
	_, code, body := createAgentScalingPolicy(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "agentRef")
}

// TestCreateAgentScalingPolicyAlreadyExistsIs409 proves a duplicate → 409.
func TestCreateAgentScalingPolicyAlreadyExistsIs409(t *testing.T) {
	existing := mockASP("my-asp", aspNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentScalingPolicyCreateRequest{
		Name: "my-asp", Namespace: aspNS, AgentRef: "agent-x", Trigger: "request-rate", Max: 5,
	}
	_, code, body := createAgentScalingPolicy(t, s, req)
	assert.Equal(t, http.StatusConflict, code, "body: %s", body)
	assert.Contains(t, body, "already exists")
}

// TestCreateAgentScalingPolicyForbiddenIs403 proves a viewer's create → 403.
func TestCreateAgentScalingPolicyForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "agentscalingpolicies"},
					obj.GetName(), errors.New("viewer cannot create"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := AgentScalingPolicyCreateRequest{
		Name: "my-asp", Namespace: aspNS, AgentRef: "agent-x", Trigger: "request-rate", Max: 5,
	}
	_, code, body := createAgentScalingPolicy(t, s, req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestCreateAgentScalingPolicyWithoutTokenIs401 proves a token-less POST → 401.
func TestCreateAgentScalingPolicyWithoutTokenIs401(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				createCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(AgentScalingPolicyCreateRequest{
		Name: "asp", Namespace: aspNS, AgentRef: "agent-x", Trigger: "request-rate", Max: 5,
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agentscalingpolicies", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, createCalled, "no K8s create must run for a token-less request")
}

// =============================================================================
// PUT /api/agentscalingpolicies/{ns}/{name} — SSA edit
// =============================================================================

// TestUpdateAgentScalingPolicyEditsMax proves a PUT edits the max field via SSA.
func TestUpdateAgentScalingPolicyEditsMax(t *testing.T) {
	existing := mockASP("my-asp", aspNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentScalingPolicyUpdateRequest{
		AgentRef: "agent-x",
		Trigger:  "request-rate",
		Min:      0,
		Max:      20,
	}
	detail, code, body := putAgentScalingPolicy(t, s, aspNS, "my-asp", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, int32(20), detail.Max)

	// Confirm the change landed.
	var got agentsv1alpha1.AgentScalingPolicy
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: aspNS, Name: "my-asp"}, &got))
	assert.Equal(t, int32(20), got.Spec.Max)
}

// TestUpdateAgentScalingPolicyAgentRefIsMutable proves a PUT that changes agentRef
// is accepted and applied (agentRef is NOT CRD-immutable — no oldSelf XValidation).
func TestUpdateAgentScalingPolicyAgentRefIsMutable(t *testing.T) {
	existing := mockASP("my-asp", aspNS, "original-agent")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentScalingPolicyUpdateRequest{
		AgentRef: "new-agent",
		Trigger:  "request-rate",
		Min:      0,
		Max:      5,
	}
	detail, code, body := putAgentScalingPolicy(t, s, aspNS, "my-asp", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	// agentRef is mutable — new value applied.
	assert.Equal(t, "new-agent", detail.AgentRef)
}

// TestUpdateAgentScalingPolicyXValidationRejectionIs422 proves that when the API
// server enforces a CRD XValidation rule (e.g. max<min) on a PUT/SSA, the BFF
// surfaces the rejection as 422.
func TestUpdateAgentScalingPolicyXValidationRejectionIs422(t *testing.T) {
	existing := mockASP("my-asp", aspNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				// Simulate the API server rejecting max<min.
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: agentScalingPolicyKind},
					obj.GetName(), nil)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentScalingPolicyUpdateRequest{
		AgentRef: "agent-x",
		Trigger:  "request-rate",
		Min:      10,
		Max:      1, // violates max >= min
	}
	_, code, body := putAgentScalingPolicy(t, s, aspNS, "my-asp", req)
	assert.Equal(t, http.StatusUnprocessableEntity, code, "XValidation rejection must surface as 422, got %d: %s", code, body)
}

// TestUpdateAgentScalingPolicyRenameGuardIs400 proves name mismatch → 400.
func TestUpdateAgentScalingPolicyRenameGuardIs400(t *testing.T) {
	existing := mockASP("my-asp", aspNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := AgentScalingPolicyUpdateRequest{
		Name:     "different-name", // mismatch
		AgentRef: "agent-x",
		Trigger:  "request-rate",
		Max:      5,
	}
	_, code, body := putAgentScalingPolicy(t, s, aspNS, "my-asp", req)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "rename")
}

// TestUpdateAgentScalingPolicyForbiddenIs403 proves a viewer's PUT → 403.
func TestUpdateAgentScalingPolicyForbiddenIs403(t *testing.T) {
	existing := mockASP("my-asp", aspNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "agentscalingpolicies"},
					obj.GetName(), errors.New("viewer cannot update"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := AgentScalingPolicyUpdateRequest{AgentRef: "agent-x", Trigger: "request-rate", Max: 5}
	_, code, body := putAgentScalingPolicy(t, s, aspNS, "my-asp", req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestUpdateAgentScalingPolicyWithoutTokenIs401 proves a token-less PUT → 401.
func TestUpdateAgentScalingPolicyWithoutTokenIs401(t *testing.T) {
	patchCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patchCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(AgentScalingPolicyUpdateRequest{AgentRef: "agent-x", Trigger: "request-rate", Max: 5})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/agentscalingpolicies/"+aspNS+"/my-asp", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, patchCalled, "no K8s patch must run for a token-less request")
}

// =============================================================================
// DELETE /api/agentscalingpolicies/{ns}/{name} — delete
// =============================================================================

// TestDeleteAgentScalingPolicyRemovesObject proves DELETE succeeds (204) and
// the policy is gone.
func TestDeleteAgentScalingPolicyRemovesObject(t *testing.T) {
	p := mockASP("my-asp", aspNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(p).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteAgentScalingPolicy(t, s, aspNS, "my-asp")
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	var got agentsv1alpha1.AgentScalingPolicy
	err := c.Get(context.Background(), client.ObjectKey{Namespace: aspNS, Name: "my-asp"}, &got)
	require.True(t, apierrors.IsNotFound(err), "AgentScalingPolicy must be gone after DELETE")
}

// TestDeleteAgentScalingPolicyNotFoundIs404 proves deleting a missing policy → 404.
func TestDeleteAgentScalingPolicyNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteAgentScalingPolicy(t, s, aspNS, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestDeleteAgentScalingPolicyForbiddenIs403 proves a viewer's DELETE → 403.
func TestDeleteAgentScalingPolicyForbiddenIs403(t *testing.T) {
	p := mockASP("my-asp", aspNS, "agent-x")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(p).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "agentscalingpolicies"},
					obj.GetName(), errors.New("viewer cannot delete"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := deleteAgentScalingPolicy(t, s, aspNS, "my-asp")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestDeleteAgentScalingPolicyWithoutTokenIs401 proves a token-less DELETE → 401.
func TestDeleteAgentScalingPolicyWithoutTokenIs401(t *testing.T) {
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
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/agentscalingpolicies/"+aspNS+"/my-asp", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, deleteCalled, "no K8s delete must run for a token-less request")
}
