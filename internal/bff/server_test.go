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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, agentsv1alpha1.AddToScheme(s))
	return s
}

func newTestServer(t *testing.T, reader AgentReader) *Server {
	t.Helper()
	return NewServer(Options{
		Reader:  reader,
		Auth:    AllowAll{}, // auth is exercised separately; here we test handlers.
		Version: "test-1.2.3",
		Log:     logr.Discard(),
	})
}

func TestHealthEndpoint(t *testing.T) {
	s := newTestServer(t, fake.NewClientBuilder().WithScheme(testScheme(t)).Build())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var body HealthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ok", body.Status)
	assert.Equal(t, "test-1.2.3", body.Version)
}

func TestListAgentsEmpty(t *testing.T) {
	// No agents in the fake cluster → the SPA must receive [] (not null).
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newTestServer(t, c)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"agents":[]}`, rec.Body.String())
}

func TestListAgentsProjection(t *testing.T) {
	ready := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "prod"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "echo:1.2.3"},
		Status: agentsv1alpha1.AgentDeploymentStatus{
			Conditions: []metav1.Condition{{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "ServiceReady",
			}},
		},
	}
	pending := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "wip", Namespace: "staging"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "wip:latest"},
		// No conditions → Pending.
	}
	notReady := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: "staging"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "broken:0"},
		Status: agentsv1alpha1.AgentDeploymentStatus{
			Conditions: []metav1.Condition{{
				Type:   "Ready",
				Status: metav1.ConditionFalse,
				Reason: "RevisionFailed",
			}},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(ready, pending, notReady).
		Build()
	s := newTestServer(t, c)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body AgentListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Agents, 3)

	byName := map[string]AgentSummary{}
	for _, a := range body.Agents {
		byName[a.Name] = a
	}

	assert.Equal(t, AgentSummary{
		Name: "echo", Namespace: "prod", Image: "echo:1.2.3", Phase: "Ready", Ready: true,
	}, byName["echo"])
	assert.Equal(t, "Pending", byName["wip"].Phase)
	assert.False(t, byName["wip"].Ready)
	assert.Equal(t, "NotReady", byName["broken"].Phase)
	assert.False(t, byName["broken"].Ready)
}

// erroringReader forces the List seam to fail so we exercise the 500 path.
type erroringReader struct{ err error }

func (e erroringReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return e.err
}

func TestListAgentsClientError(t *testing.T) {
	s := newTestServer(t, erroringReader{err: assert.AnError})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
