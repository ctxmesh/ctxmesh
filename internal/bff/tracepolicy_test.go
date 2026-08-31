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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

func tracePolicyServer(t *testing.T, objs ...client.Object) (*Server, client.Client) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return newCallerServer(t, &fakeCallerClientFactory{client: c}), c
}

// TestTracePolicy_GetAndUpdate proves the redaction editor round-trips: an agent
// with no policy reads []; a PUT sets the custom detectors on the CRD; and a PUT
// with an empty list clears the policy block (m18.13).
func TestTracePolicy_GetAndUpdate(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "team-a"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "i:1"},
	}
	s, c := tracePolicyServer(t, ad)

	// GET on an agent with no policy → [] (not null).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents/team-a/echo/tracepolicy", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"customDetectors":[]}`, rec.Body.String())

	// PUT sets a custom detector.
	body, _ := json.Marshal(TracePolicyResponse{CustomDetectors: []CustomDetectorDTO{{Name: "badge", Pattern: "BADGE-[0-9]+"}}})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/agents/team-a/echo/tracepolicy", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// The CRD now carries the detector.
	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "echo", Namespace: "team-a"}, &got))
	require.NotNil(t, got.Spec.TracePolicy)
	require.Len(t, got.Spec.TracePolicy.CustomDetectors, 1)
	assert.Equal(t, "badge", got.Spec.TracePolicy.CustomDetectors[0].Name)
	assert.Equal(t, "BADGE-[0-9]+", got.Spec.TracePolicy.CustomDetectors[0].Pattern)

	// PUT with an empty list clears the policy block.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/agents/team-a/echo/tracepolicy",
		bytes.NewReader([]byte(`{"customDetectors":[]}`)))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "echo", Namespace: "team-a"}, &got))
	assert.Nil(t, got.Spec.TracePolicy, "clearing the last detector removes the block")
}

// TestTracePolicy_NotFound proves a missing agent surfaces as 404, not a 500.
func TestTracePolicy_NotFound(t *testing.T) {
	s, _ := tracePolicyServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents/team-a/ghost/tracepolicy", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
