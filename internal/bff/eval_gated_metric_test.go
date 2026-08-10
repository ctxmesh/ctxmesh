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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
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

// agentDeploy is a minimal AgentDeployment builder for eval-gated tests.
func agentDeploy(name, ns, evalSuiteRef string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:        "ghcr.io/test/agent:latest",
			EvalSuiteRef: evalSuiteRef,
		},
	}
}

// newEvalGatedServer builds a minimal Server for the eval-gated handler tests.
func newEvalGatedServer(t *testing.T, factory CallerClientFactory) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients: factory,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Version:       "test",
		Log:           logr.Discard(),
	})
}

// evalGatedGET fires GET /api/metrics/eval-gated and returns the recorder.
func evalGatedGET(t *testing.T, s *Server, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

// TestEvalGatedMetric_counts proves the handler correctly counts gated vs total
// from a fake set of AgentDeployments (some with evalSuiteRef, some without).
func TestEvalGatedMetric_counts(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		agentDeploy("agent-a", "default", "my-suite"),    // gated
		agentDeploy("agent-b", "default", "other-suite"), // gated
		agentDeploy("agent-c", "default", ""),            // NOT gated
		agentDeploy("agent-d", "other", "suite-x"),       // gated, different ns
	).Build()

	s := newEvalGatedServer(t, newFakeFactory(c))
	rec := evalGatedGET(t, s, "/api/metrics/eval-gated")

	require.Equal(t, http.StatusOK, rec.Code)
	var body EvalGatedMetricResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Equal(t, 4, body.Total, "total must count all AgentDeployments")
	assert.Equal(t, 3, body.Gated, "gated must count only those with non-empty evalSuiteRef")
	// 3/4 = 75.0
	assert.InDelta(t, 75.0, body.Percent, 0.05, "percent must be gated/total*100")
}

// TestEvalGatedMetric_allGated proves a cluster where every deployment is eval-gated
// returns 100%.
func TestEvalGatedMetric_allGated(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		agentDeploy("agent-a", "default", "suite-a"),
		agentDeploy("agent-b", "default", "suite-b"),
	).Build()

	s := newEvalGatedServer(t, newFakeFactory(c))
	rec := evalGatedGET(t, s, "/api/metrics/eval-gated")

	require.Equal(t, http.StatusOK, rec.Code)
	var body EvalGatedMetricResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Equal(t, 2, body.Total)
	assert.Equal(t, 2, body.Gated)
	assert.InDelta(t, 100.0, body.Percent, 0.05)
}

// TestEvalGatedMetric_zeroTotal proves the handler returns 0% without dividing
// by zero when no AgentDeployments exist.
func TestEvalGatedMetric_zeroTotal(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	s := newEvalGatedServer(t, newFakeFactory(c))
	rec := evalGatedGET(t, s, "/api/metrics/eval-gated")

	require.Equal(t, http.StatusOK, rec.Code)
	var body EvalGatedMetricResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Equal(t, 0, body.Total)
	assert.Equal(t, 0, body.Gated)
	assert.Equal(t, 0.0, body.Percent, "percent must be 0 when total==0, not a divide-by-zero")
}

// TestEvalGatedMetric_noneGated proves the handler correctly returns 0% when no
// deployment references an EvalSuite.
func TestEvalGatedMetric_noneGated(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		agentDeploy("agent-a", "default", ""),
		agentDeploy("agent-b", "default", ""),
	).Build()

	s := newEvalGatedServer(t, newFakeFactory(c))
	rec := evalGatedGET(t, s, "/api/metrics/eval-gated")

	require.Equal(t, http.StatusOK, rec.Code)
	var body EvalGatedMetricResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Equal(t, 2, body.Total)
	assert.Equal(t, 0, body.Gated)
	assert.Equal(t, 0.0, body.Percent)
}

// TestEvalGatedMetric_namespaceScoped proves the ?namespace param narrows the
// count to one namespace (caller-scoped, matching GET /api/agents behaviour).
func TestEvalGatedMetric_namespaceScoped(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		agentDeploy("agent-a", "default", "suite-a"), // gated in "default"
		agentDeploy("agent-b", "default", ""),        // NOT gated in "default"
		agentDeploy("agent-c", "other", "suite-b"),   // gated in "other" — excluded
	).Build()

	s := newEvalGatedServer(t, newFakeFactory(c))
	rec := evalGatedGET(t, s, "/api/metrics/eval-gated?namespace=default")

	require.Equal(t, http.StatusOK, rec.Code)
	var body EvalGatedMetricResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Equal(t, 2, body.Total, "only the 'default' namespace must be counted")
	assert.Equal(t, 1, body.Gated)
	assert.InDelta(t, 50.0, body.Percent, 0.05)
}

// TestEvalGatedMetric_callerScoped proves the handler is caller-scoped (ADR 0011):
// a Forbidden on the AgentDeployment list surfaces as 403, not a fabricated 200.
func TestEvalGatedMetric_callerScoped(t *testing.T) {
	forbidden := interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return apierrors.NewForbidden(
				schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"},
				"", assert.AnError,
			)
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(forbidden).Build()

	s := newEvalGatedServer(t, newFakeFactory(c))
	rec := evalGatedGET(t, s, "/api/metrics/eval-gated")

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a caller-scoped read denial must surface as 403 — never a fabricated 0/0 success")
}
