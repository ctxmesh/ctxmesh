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

// esNS is the namespace used in EvalSuite tests.
const esNS = "team-evals"

// --- fixture helpers --------------------------------------------------------

// mockEvalSuite builds a minimal EvalSuite with one mock scorer.
func mockEvalSuite(name, ns string) *agentsv1alpha1.EvalSuite {
	return &agentsv1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.EvalSuiteSpec{
			Dataset:   agentsv1alpha1.DatasetRef{Ref: "my-dataset"},
			Scorers:   []agentsv1alpha1.ScorerSpec{{Name: "quality", Type: "mock", Weight: 1}},
			Threshold: "0.80",
			Gate:      "block",
		},
	}
}

// gatedEvalSuite sets status conditions (gate outcome) on an EvalSuite.
func gatedEvalSuite(es *agentsv1alpha1.EvalSuite, passed bool) *agentsv1alpha1.EvalSuite {
	status := metav1.ConditionTrue
	reason := "GatePassed"
	msg := "suite score 0.92 >= threshold 0.80"
	if !passed {
		status = metav1.ConditionFalse
		reason = "GateBlocked"
		msg = "suite score 0.65 < threshold 0.80"
	}
	es.Status.Conditions = []metav1.Condition{
		{
			Type:               "GatePassed",
			Status:             status,
			Reason:             reason,
			Message:            msg,
			LastTransitionTime: metav1.Now(),
		},
	}
	return es
}

// --- request helpers --------------------------------------------------------

func getEvalSuites(t *testing.T, s *Server, rawQuery string) (EvalSuiteListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	url := "/api/evalsuites"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body EvalSuiteListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

//nolint:unparam
func getEvalSuite(t *testing.T, s *Server, ns, name string) (*EvalSuiteDetail, int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/evalsuites/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail EvalSuiteDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func createEvalSuite(t *testing.T, s *Server, reqBody EvalSuiteCreateRequest) (*EvalSuiteDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/evalsuites", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var detail EvalSuiteDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

//nolint:unparam
func putEvalSuite(t *testing.T, s *Server, ns, name string, reqBody EvalSuiteUpdateRequest) (*EvalSuiteDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/evalsuites/"+ns+"/"+name, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail EvalSuiteDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func deleteEvalSuite(t *testing.T, s *Server, ns, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/evalsuites/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

//nolint:unparam
func getEvalSuiteResults(t *testing.T, s *Server, ns, name, traceID string) (*EvalSuiteResultsResponse, int, string) {
	t.Helper()
	url := "/api/evalsuites/" + ns + "/" + name + "/results"
	if traceID != "" {
		url += "?traceId=" + traceID
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var body EvalSuiteResultsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return &body, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// =============================================================================
// GET /api/evalsuites — list contract
// =============================================================================

// TestListEvalSuitesEmpty proves an empty cluster yields [] not null.
func TestListEvalSuitesEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getEvalSuites(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items, "items must be [] not null")
	assert.Empty(t, body.Items)
	assert.Empty(t, body.NextCursor)
}

// TestListEvalSuitesReturnsItems proves seeded EvalSuites appear in the response.
func TestListEvalSuitesReturnsItems(t *testing.T) {
	objs := []client.Object{
		mockEvalSuite("suite-a", esNS),
		mockEvalSuite("suite-b", esNS),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getEvalSuites(t, s, "namespace="+esNS)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
		assert.Equal(t, esNS, item.Namespace)
		assert.Equal(t, "my-dataset", item.DatasetRef)
		assert.Equal(t, "0.80", item.Threshold)
		assert.Equal(t, "block", item.Gate)
	}
	assert.True(t, names["suite-a"])
	assert.True(t, names["suite-b"])
}

// TestListEvalSuitesQFilter proves ?q is a case-insensitive windowed substring filter.
func TestListEvalSuitesQFilter(t *testing.T) {
	objs := []client.Object{
		mockEvalSuite("prod-suite", esNS),
		mockEvalSuite("PROD-staging-suite", esNS),
		mockEvalSuite("dev-suite", esNS),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getEvalSuites(t, s, "q=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
	}
	assert.True(t, names["prod-suite"])
	assert.True(t, names["PROD-staging-suite"])
	assert.False(t, names["dev-suite"])

	// No match → [] not null.
	body, code = getEvalSuites(t, s, "q=zzz-nomatch")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items)
	assert.Empty(t, body.Items)
}

// TestListEvalSuitesNamespaceScoping proves ?namespace scopes the list.
func TestListEvalSuitesNamespaceScoping(t *testing.T) {
	objs := []client.Object{
		mockEvalSuite("prod-es", "prod"),
		mockEvalSuite("dev-es", "dev"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getEvalSuites(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "prod", body.Items[0].Namespace)
}

// TestListEvalSuitesLimitAndCursor proves limit/cursor paging works.
func TestListEvalSuitesLimitAndCursor(t *testing.T) {
	all := []*agentsv1alpha1.EvalSuite{
		mockEvalSuite("es-000", esNS),
		mockEvalSuite("es-001", esNS),
		mockEvalSuite("es-002", esNS),
		mockEvalSuite("es-003", esNS),
		mockEvalSuite("es-004", esNS),
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
			esList, ok := list.(*agentsv1alpha1.EvalSuiteList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			for _, es := range all[start:end] {
				esList.Items = append(esList.Items, *es)
			}
			if end < len(all) {
				esList.Continue = strconv.Itoa(end)
			}
			return nil
		},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(pagingFn).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	page1, code := getEvalSuites(t, s, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 2)
	require.NotEmpty(t, page1.NextCursor)

	seen := len(page1.Items)
	cursor := page1.NextCursor
	for cursor != "" {
		next, code := getEvalSuites(t, s, "limit=2&cursor="+cursor)
		require.Equal(t, http.StatusOK, code)
		seen += len(next.Items)
		cursor = next.NextCursor
	}
	assert.Equal(t, 5, seen, "paging must visit every eval suite exactly once")
}

// TestListEvalSuitesForbiddenIs403 proves a Forbidden on the list surfaces as 403.
func TestListEvalSuitesForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "evalsuites"},
					"", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code := getEvalSuites(t, s, "")
	require.Equal(t, http.StatusForbidden, code)
}

// =============================================================================
// GET /api/evalsuites/{ns}/{name} — detail
// =============================================================================

// TestGetEvalSuiteReturnsDetail proves a seeded EvalSuite is returned with correct
// projection including scorers, gate, threshold, and status conditions.
func TestGetEvalSuiteReturnsDetail(t *testing.T) {
	es := gatedEvalSuite(mockEvalSuite("my-suite", esNS), true)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(es).WithObjects(es).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getEvalSuite(t, s, esNS, "my-suite")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-suite", detail.Name)
	assert.Equal(t, esNS, detail.Namespace)
	assert.Equal(t, "my-dataset", detail.DatasetRef)
	assert.Equal(t, "block", detail.Gate)
	assert.Equal(t, "0.80", detail.Threshold)
	// Scorers must be projected (never nil).
	assert.NotNil(t, detail.Scorers)
	require.Len(t, detail.Scorers, 1)
	assert.Equal(t, "quality", detail.Scorers[0].Name)
	assert.Equal(t, "mock", detail.Scorers[0].Type)
	// Conditions from status must be projected (the gate outcome).
	assert.NotNil(t, detail.Conditions, "conditions must be [] not null")
	require.Len(t, detail.Conditions, 1)
	assert.Equal(t, "GatePassed", detail.Conditions[0].Type)
	assert.Equal(t, "True", detail.Conditions[0].Status)
}

// TestGetEvalSuiteProjectsEmptyConditions proves conditions are [] not null
// when status.conditions has not been written yet (no reconciliation occurred).
func TestGetEvalSuiteProjectsEmptyConditions(t *testing.T) {
	es := mockEvalSuite("new-suite", esNS) // no conditions set
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(es).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getEvalSuite(t, s, esNS, "new-suite")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	assert.NotNil(t, detail.Conditions, "conditions must be [] not null even when no controller run yet")
	assert.Empty(t, detail.Conditions)
}

// TestGetEvalSuiteNotFoundIs404 proves a missing EvalSuite yields 404.
func TestGetEvalSuiteNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getEvalSuite(t, s, esNS, "ghost")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// TestGetEvalSuiteForbiddenIs403 proves a caller denied Get sees 403.
func TestGetEvalSuiteForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "evalsuites"},
					"my-suite", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getEvalSuite(t, s, esNS, "my-suite")
	require.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "forbidden")
}

// =============================================================================
// POST /api/evalsuites — create
// =============================================================================

// TestCreateEvalSuiteSucceeds proves a valid EvalSuite create returns 201.
func TestCreateEvalSuiteSucceeds(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := EvalSuiteCreateRequest{
		Name:       "new-suite",
		Namespace:  esNS,
		DatasetRef: "ci-dataset",
		Scorers:    []ScorerSpecDTO{{Name: "quality", Type: "mock", Weight: 1}},
		Threshold:  "0.75",
		Gate:       "block",
	}
	detail, code, body := createEvalSuite(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "new-suite", detail.Name)
	assert.Equal(t, esNS, detail.Namespace)
	assert.Equal(t, "ci-dataset", detail.DatasetRef)
	assert.Equal(t, "0.75", detail.Threshold)
	assert.NotNil(t, detail.Scorers)
	require.Len(t, detail.Scorers, 1)
	assert.Equal(t, "quality", detail.Scorers[0].Name)

	// Confirm it landed in the fake store.
	var got agentsv1alpha1.EvalSuite
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: esNS, Name: "new-suite"}, &got))
	assert.Equal(t, "ci-dataset", got.Spec.Dataset.Ref)
	assert.Len(t, got.Spec.Scorers, 1)
}

// TestCreateEvalSuiteMissingNameIs400 proves a missing name yields 400.
func TestCreateEvalSuiteMissingNameIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := EvalSuiteCreateRequest{
		Namespace:  esNS,
		DatasetRef: "ci-dataset",
		Scorers:    []ScorerSpecDTO{{Name: "q", Type: "mock"}},
		Threshold:  "0.80",
	}
	_, code, body := createEvalSuite(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "name")
}

// TestCreateEvalSuiteEmptyScorersIs400 proves an empty scorers list yields 400.
func TestCreateEvalSuiteEmptyScorersIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := EvalSuiteCreateRequest{
		Name:       "bad-suite",
		Namespace:  esNS,
		DatasetRef: "ci-dataset",
		Scorers:    []ScorerSpecDTO{},
		Threshold:  "0.80",
	}
	_, code, body := createEvalSuite(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
}

// TestCreateEvalSuiteMissingDatasetRefIs400 proves a missing datasetRef yields 400.
func TestCreateEvalSuiteMissingDatasetRefIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := EvalSuiteCreateRequest{
		Name:      "bad-suite",
		Namespace: esNS,
		Scorers:   []ScorerSpecDTO{{Name: "q", Type: "mock"}},
		Threshold: "0.80",
	}
	_, code, body := createEvalSuite(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "datasetRef")
}

// TestCreateEvalSuiteAlreadyExistsIs409 proves a duplicate create yields 409.
func TestCreateEvalSuiteAlreadyExistsIs409(t *testing.T) {
	existing := mockEvalSuite("my-suite", esNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := EvalSuiteCreateRequest{
		Name:       "my-suite",
		Namespace:  esNS,
		DatasetRef: "ci-dataset",
		Scorers:    []ScorerSpecDTO{{Name: "q", Type: "mock"}},
		Threshold:  "0.80",
	}
	_, code, body := createEvalSuite(t, s, req)
	assert.Equal(t, http.StatusConflict, code, "body: %s", body)
	assert.Contains(t, body, "already exists")
}

// TestCreateEvalSuiteAPIServerRejectionSurfaces422 proves API server Invalid
// surfaces as 422 (CRD XValidation rejection).
func TestCreateEvalSuiteAPIServerRejectionSurfaces422(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: evalSuiteKind},
					obj.GetName(), nil)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := EvalSuiteCreateRequest{
		Name:       "bad-suite",
		Namespace:  esNS,
		DatasetRef: "ci-dataset",
		Scorers:    []ScorerSpecDTO{{Name: "q", Type: "mock"}},
		Threshold:  "0.80",
	}
	_, code, body := createEvalSuite(t, s, req)
	assert.True(t, code >= 400 && code < 500, "API server rejection must surface as 4xx, got %d: %s", code, body)
	assert.Equal(t, http.StatusUnprocessableEntity, code, "CRD XValidation rejection must be 422, got %d: %s", code, body)
}

// TestCreateEvalSuiteForbiddenIs403 proves a viewer's create returns 403.
func TestCreateEvalSuiteForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "evalsuites"},
					obj.GetName(), errors.New("viewer cannot create"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := EvalSuiteCreateRequest{
		Name:       "no-perm",
		Namespace:  esNS,
		DatasetRef: "ci-dataset",
		Scorers:    []ScorerSpecDTO{{Name: "q", Type: "mock"}},
		Threshold:  "0.80",
	}
	_, code, body := createEvalSuite(t, s, req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestCreateEvalSuiteWithoutTokenIs401 proves a token-less POST is rejected 401.
func TestCreateEvalSuiteWithoutTokenIs401(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				createCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(EvalSuiteCreateRequest{
		Name:       "suite",
		Namespace:  esNS,
		DatasetRef: "ci-dataset",
		Scorers:    []ScorerSpecDTO{{Name: "q", Type: "mock"}},
		Threshold:  "0.80",
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/evalsuites", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, createCalled, "no K8s create must run for a token-less request")
}

// =============================================================================
// PUT /api/evalsuites/{ns}/{name} — update (SSA) + rename guard
// =============================================================================

// TestUpdateEvalSuiteEditsSpec proves a PUT updates the spec via SSA.
func TestUpdateEvalSuiteEditsSpec(t *testing.T) {
	existing := mockEvalSuite("my-suite", esNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := EvalSuiteUpdateRequest{
		Name:       "my-suite",
		DatasetRef: "updated-dataset",
		Scorers: []ScorerSpecDTO{
			{Name: "quality", Type: "mock", Weight: 2},
			{Name: "accuracy", Type: "llm-judge", Weight: 1},
		},
		Threshold: "0.90",
		Gate:      "warn",
	}
	detail, code, body := putEvalSuite(t, s, esNS, "my-suite", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "updated-dataset", detail.DatasetRef)
	assert.Equal(t, "0.90", detail.Threshold)
	assert.Equal(t, "warn", detail.Gate)
	require.Len(t, detail.Scorers, 2)
}

// TestUpdateEvalSuiteRenameGuardIs400 proves a name mismatch yields 400.
func TestUpdateEvalSuiteRenameGuardIs400(t *testing.T) {
	existing := mockEvalSuite("my-suite", esNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := EvalSuiteUpdateRequest{
		Name:       "different-name", // mismatch
		DatasetRef: "ci-dataset",
		Scorers:    []ScorerSpecDTO{{Name: "q", Type: "mock"}},
		Threshold:  "0.80",
	}
	_, code, body := putEvalSuite(t, s, esNS, "my-suite", req)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "rename")
}

// TestUpdateEvalSuiteAbsentNameInBodyIsOK proves omitting Name does not trigger
// the rename guard.
func TestUpdateEvalSuiteAbsentNameInBodyIsOK(t *testing.T) {
	existing := mockEvalSuite("my-suite", esNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := EvalSuiteUpdateRequest{
		DatasetRef: "ci-dataset-v2",
		Scorers:    []ScorerSpecDTO{{Name: "quality", Type: "mock"}},
		Threshold:  "0.85",
	}
	detail, code, body := putEvalSuite(t, s, esNS, "my-suite", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-suite", detail.Name)
}

// TestUpdateEvalSuiteForbiddenIs403 proves a viewer's PUT returns 403.
func TestUpdateEvalSuiteForbiddenIs403(t *testing.T) {
	existing := mockEvalSuite("my-suite", esNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "evalsuites"},
					obj.GetName(), errors.New("viewer cannot update"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	req := EvalSuiteUpdateRequest{
		DatasetRef: "ci-dataset",
		Scorers:    []ScorerSpecDTO{{Name: "q", Type: "mock"}},
		Threshold:  "0.80",
	}
	_, code, body := putEvalSuite(t, s, esNS, "my-suite", req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
}

// TestUpdateEvalSuiteXValidationRejectionIs422 proves API server Invalid → 422.
func TestUpdateEvalSuiteXValidationRejectionIs422(t *testing.T) {
	existing := mockEvalSuite("my-suite", esNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: evalSuiteKind},
					obj.GetName(), nil)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := EvalSuiteUpdateRequest{
		DatasetRef: "ci-dataset",
		Scorers:    []ScorerSpecDTO{{Name: "q", Type: "mock"}},
		Threshold:  "0.80",
	}
	_, code, body := putEvalSuite(t, s, esNS, "my-suite", req)
	assert.Equal(t, http.StatusUnprocessableEntity, code, "API-server Invalid must surface as 422, got %d: %s", code, body)
}

// TestUpdateEvalSuiteWithoutTokenIs401 proves a token-less PUT is rejected 401.
func TestUpdateEvalSuiteWithoutTokenIs401(t *testing.T) {
	patchCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patchCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(EvalSuiteUpdateRequest{
		DatasetRef: "ci-dataset",
		Scorers:    []ScorerSpecDTO{{Name: "q", Type: "mock"}},
		Threshold:  "0.80",
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/evalsuites/"+esNS+"/my-suite", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, patchCalled, "no K8s patch must run for a token-less request")
}

// =============================================================================
// DELETE /api/evalsuites/{ns}/{name} — delete
// =============================================================================

// TestDeleteEvalSuiteRemovesObject proves a DELETE succeeds (204).
func TestDeleteEvalSuiteRemovesObject(t *testing.T) {
	es := mockEvalSuite("my-suite", esNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(es).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteEvalSuite(t, s, esNS, "my-suite")
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	var got agentsv1alpha1.EvalSuite
	err := c.Get(context.Background(), client.ObjectKey{Namespace: esNS, Name: "my-suite"}, &got)
	require.True(t, apierrors.IsNotFound(err), "EvalSuite must be gone after a successful DELETE")
}

// TestDeleteEvalSuiteNotFoundIs404 proves deleting a missing EvalSuite yields 404.
func TestDeleteEvalSuiteNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteEvalSuite(t, s, esNS, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestDeleteEvalSuiteForbiddenIs403 proves a viewer's DELETE returns 403.
func TestDeleteEvalSuiteForbiddenIs403(t *testing.T) {
	es := mockEvalSuite("my-suite", esNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(es).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "evalsuites"},
					obj.GetName(), errors.New("viewer cannot delete"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := deleteEvalSuite(t, s, esNS, "my-suite")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestDeleteEvalSuiteWithoutTokenIs401 proves a token-less DELETE is rejected 401.
func TestDeleteEvalSuiteWithoutTokenIs401(t *testing.T) {
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
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/evalsuites/"+esNS+"/my-suite", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, deleteCalled, "no K8s delete must run for a token-less request")
}

// =============================================================================
// GET /api/evalsuites/{ns}/{name}/results — merged CRD status + Langfuse scores
// =============================================================================

// TestEvalSuiteResultsReturnsCRDConditions proves the results endpoint returns
// the gate outcome from status.conditions — the REAL CRD status, never fabricated.
func TestEvalSuiteResultsReturnsCRDConditions(t *testing.T) {
	es := gatedEvalSuite(mockEvalSuite("my-suite", esNS), true) // gate passed
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(es).WithObjects(es).Build()
	// Wire Langfuse but no traceId → scoresAvailable:false
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c}, Adapters{
		Langfuse: fakeLangfuseAdapter{},
	})

	body, code, raw := getEvalSuiteResults(t, s, esNS, "my-suite", "")
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.NotNil(t, body)
	// CRD conditions must be projected.
	require.Len(t, body.Conditions, 1)
	assert.Equal(t, "GatePassed", body.Conditions[0].Type)
	assert.Equal(t, "True", body.Conditions[0].Status)
	assert.Equal(t, "GatePassed", body.Conditions[0].Reason)
	// Scores not available because traceId not supplied.
	assert.False(t, body.ScoresAvailable)
	assert.NotNil(t, body.Scores, "scores must be [] not null")
	assert.Empty(t, body.Scores)
	assert.Equal(t, "traceId not supplied", body.ScoresUnavailableReason)
}

// TestEvalSuiteResultsGateResultsAggregatesFromAgents proves the read-time projection
// (ADR 0094, m121.1): the offline gate outcome lives per-agent on AgentDeployment
// status.gate, and /results aggregates it caller-scoped for every agent gating on this
// suite — a gated agent surfaces its real score, an un-gated one is pending, and an
// agent on a DIFFERENT suite is excluded. GateResultsAvailable=true on a successful list.
func TestEvalSuiteResultsGateResultsAggregatesFromAgents(t *testing.T) {
	es := mockEvalSuite("my-suite", esNS)
	gated := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-gated", Namespace: esNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{EvalSuiteRef: "my-suite"},
		Status: agentsv1alpha1.AgentDeploymentStatus{Gate: &agentsv1alpha1.GateStatus{
			Decision: "promoted", Phase: "awaiting-promotion", Reason: "AwaitingHumanPromotion",
			Score: "0.9182", ScoredRevision: "agent-gated-abc", Threshold: "0.0000",
		}},
	}
	pending := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-pending", Namespace: esNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{EvalSuiteRef: "my-suite"},
		// no status.gate yet
	}
	other := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-other", Namespace: esNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{EvalSuiteRef: "different-suite"},
		Status: agentsv1alpha1.AgentDeploymentStatus{Gate: &agentsv1alpha1.GateStatus{
			Score: "0.5000", Decision: "blocked",
		}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(es).WithObjects(es, gated, pending, other).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code, raw := getEvalSuiteResults(t, s, esNS, "my-suite", "")
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.NotNil(t, body)
	assert.True(t, body.GateResultsAvailable, "list succeeded ⇒ available")
	assert.Empty(t, body.GateResultsUnavailableReason)
	require.Len(t, body.GateResults, 2, "only the two agents on my-suite (not different-suite)")

	byAgent := map[string]GateResultDTO{}
	for _, g := range body.GateResults {
		byAgent[g.Agent] = g
	}
	g := byAgent["agent-gated"]
	assert.Equal(t, "0.9182", g.Score)
	assert.Equal(t, "promoted", g.Decision)
	assert.Equal(t, "agent-gated-abc", g.ScoredRevision)
	assert.Equal(t, "0.0000", g.Threshold)
	assert.False(t, g.Pending)
	p := byAgent["agent-pending"]
	assert.True(t, p.Pending, "an agent with no gate run yet is pending")
	assert.Empty(t, p.Score, "pending ⇒ no fake 0.0 score")
	_, hasOther := byAgent["agent-other"]
	assert.False(t, hasOther, "an agent gating on a different suite must be excluded")
}

// TestEvalSuiteResultsWithLangfuseScores proves that when Langfuse is wired AND
// traceId is supplied, real Langfuse scores appear in the response alongside the
// CRD conditions. Never fabricated — scoresAvailable:true only when real scores exist.
func TestEvalSuiteResultsWithLangfuseScores(t *testing.T) {
	es := gatedEvalSuite(mockEvalSuite("my-suite", esNS), true)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(es).WithObjects(es).Build()

	fakeScores := []FeedbackScore{
		{ID: "sc-1", TraceID: "trace-eval-abc", Name: "quality", DataType: "NUMERIC", Value: 0.92, Source: "API", CreatedAt: "2026-07-04T00:00:00Z"},
	}
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c}, Adapters{
		Langfuse: fakeLangfuseAdapter{scores: fakeScores},
	})

	body, code, raw := getEvalSuiteResults(t, s, esNS, "my-suite", "trace-eval-abc")
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.NotNil(t, body)
	// CRD conditions.
	require.Len(t, body.Conditions, 1)
	assert.Equal(t, "GatePassed", body.Conditions[0].Type)
	// Langfuse scores present.
	assert.True(t, body.ScoresAvailable, "scores must be available when Langfuse wired + traceId supplied")
	assert.Empty(t, body.ScoresUnavailableReason, "unavailable reason must be empty when scores are available")
	require.Len(t, body.Scores, 1)
	assert.Equal(t, "sc-1", body.Scores[0].ID)
	assert.InDelta(t, 0.92, body.Scores[0].Value, 1e-9)
}

// TestEvalSuiteResultsLangfuseAbsentDegradeHonestly proves that when the Langfuse
// adapter is nil, the results endpoint still returns 200 with the CRD conditions
// and scoresAvailable:false + the documented reason — never a 501 or fabricated scores.
func TestEvalSuiteResultsLangfuseAbsentDegradeHonestly(t *testing.T) {
	es := gatedEvalSuite(mockEvalSuite("my-suite", esNS), false) // gate blocked
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(es).WithObjects(es).Build()
	// No Langfuse adapter wired.
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c}, Adapters{})

	body, code, raw := getEvalSuiteResults(t, s, esNS, "my-suite", "trace-eval-xyz")
	require.Equal(t, http.StatusOK, code, "Langfuse absent must return 200 with degrade, not 501, body: %s", raw)
	require.NotNil(t, body)
	// CRD conditions still available even without Langfuse.
	require.Len(t, body.Conditions, 1)
	assert.Equal(t, "GatePassed", body.Conditions[0].Type)
	assert.Equal(t, "False", body.Conditions[0].Status)
	// Scores unavailable with honest reason.
	assert.False(t, body.ScoresAvailable)
	assert.NotNil(t, body.Scores, "scores must be [] not null")
	assert.Empty(t, body.Scores)
	assert.Equal(t, "langfuse not configured", body.ScoresUnavailableReason)
}

// TestEvalSuiteResultsNoTraceIDDegradeHonestly proves that when traceId is absent,
// the results still return the CRD conditions with scoresAvailable:false and the
// reason "traceId not supplied" — never fabricated scores.
func TestEvalSuiteResultsNoTraceIDDegradeHonestly(t *testing.T) {
	es := gatedEvalSuite(mockEvalSuite("my-suite", esNS), true)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(es).WithObjects(es).Build()
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c}, Adapters{
		Langfuse: fakeLangfuseAdapter{},
	})

	body, code, raw := getEvalSuiteResults(t, s, esNS, "my-suite", "" /* no traceId */)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.NotNil(t, body)
	assert.NotEmpty(t, body.Conditions)
	assert.False(t, body.ScoresAvailable)
	assert.Equal(t, "traceId not supplied", body.ScoresUnavailableReason)
	assert.Empty(t, body.Scores)
}

// TestEvalSuiteResultsLangfuseErrorDegradeHonestly proves that a Langfuse upstream
// error degrades honestly: still 200 with CRD conditions and scoresAvailable:false
// + a reason containing the error detail — never a 502 that hides the conditions,
// never fabricated scores.
func TestEvalSuiteResultsLangfuseErrorDegradeHonestly(t *testing.T) {
	es := gatedEvalSuite(mockEvalSuite("my-suite", esNS), true)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(es).WithObjects(es).Build()
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c}, Adapters{
		Langfuse: fakeLangfuseAdapter{scoresErr: errors.New("upstream timeout")},
	})

	body, code, raw := getEvalSuiteResults(t, s, esNS, "my-suite", "trace-eval-123")
	require.Equal(t, http.StatusOK, code, "Langfuse error must return 200 with degrade, not 502, body: %s", raw)
	require.NotNil(t, body)
	// CRD conditions still available.
	assert.NotEmpty(t, body.Conditions)
	// Scores unavailable with honest reason containing the error detail.
	assert.False(t, body.ScoresAvailable)
	assert.Contains(t, body.ScoresUnavailableReason, "langfuse error")
	assert.Contains(t, body.ScoresUnavailableReason, "upstream timeout")
	assert.Empty(t, body.Scores)
}

// TestEvalSuiteResultsNotFoundIs404 proves a missing EvalSuite returns 404.
func TestEvalSuiteResultsNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getEvalSuiteResults(t, s, esNS, "ghost", "")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// TestEvalSuiteResultsForbiddenIs403 proves a caller denied Get returns 403.
func TestEvalSuiteResultsForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "evalsuites"},
					"my-suite", errors.New("viewer denied"))
			},
		}).Build()
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c}, Adapters{
		Langfuse: fakeLangfuseAdapter{},
	})

	_, code, body := getEvalSuiteResults(t, s, esNS, "my-suite", "trace-xyz")
	require.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "forbidden")
}

// TestEvalSuiteResultsEmptyConditionsIsHonest proves that when status.conditions
// is empty (controller never ran), results returns [] conditions, not fabricated
// outcomes.
func TestEvalSuiteResultsEmptyConditionsIsHonest(t *testing.T) {
	es := mockEvalSuite("new-suite", esNS) // no conditions
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(es).Build()
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c}, Adapters{
		Langfuse: fakeLangfuseAdapter{},
	})

	body, code, raw := getEvalSuiteResults(t, s, esNS, "new-suite", "")
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.NotNil(t, body)
	// Empty conditions is HONEST: the controller hasn't run yet. NOT fabricated.
	assert.NotNil(t, body.Conditions, "conditions must be [] not null")
	assert.Empty(t, body.Conditions, "no conditions when controller hasn't run — must not fabricate")
}

// TestEvalSuiteResultsScoresNonNilWhenEmpty proves scores is always [] not null
// on the wire, even when scoresAvailable is false.
func TestEvalSuiteResultsScoresNonNilWhenEmpty(t *testing.T) {
	es := mockEvalSuite("my-suite", esNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(es).Build()
	// No Langfuse.
	s := serverWithCallerAndAdapters(t, &fakeCallerClientFactory{client: c}, Adapters{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/evalsuites/"+esNS+"/my-suite/results", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"scores":[]`, "scores must serialize as [] not null")
	assert.Contains(t, rec.Body.String(), `"conditions":[]`, "conditions must serialize as [] not null")
}
