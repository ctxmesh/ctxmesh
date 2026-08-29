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

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

func smReq(t *testing.T, s *Server, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/agents/default/a1/sessionmemory", nil)
	} else {
		r = httptest.NewRequest(method, "/api/agents/default/a1/sessionmemory", bytes.NewReader([]byte(body)))
	}
	r.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// The session-memory console surface (M137/EU1d): GET reads spec.sessionMemory; PUT toggles perUser by
// setting/clearing the folded field, preserving a configured Backend and normalizing perUser off for the
// shared scope.
func TestSessionMemory_EnableToggleDisable(t *testing.T) {
	backend := &agentsv1alpha1.MemoryBackend{}
	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "default"},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "img", ExecutionModel: "serving",
			// Start enabled with a configured backend so we can prove PUT preserves it.
			SessionMemory: &agentsv1alpha1.SessionMemorySpec{Scope: "session", Backend: backend},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// GET: enabled, session scope, perUser off (the CRD default).
	rec := smReq(t, s, http.MethodGet, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var cfg SessionMemoryConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))
	assert.True(t, cfg.Enabled)
	assert.Equal(t, "session", cfg.Scope)
	assert.False(t, cfg.PerUser)

	// PUT: turn perUser ON (session scope) → the folded field carries it AND keeps the backend.
	rec = smReq(t, s, http.MethodPut, `{"enabled":true,"scope":"session","perUser":true}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var live agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "a1"}, &live))
	require.NotNil(t, live.Spec.SessionMemory)
	assert.True(t, live.Spec.SessionMemory.PerUser)
	assert.NotNil(t, live.Spec.SessionMemory.Backend, "a configured backend must be preserved across a toggle")

	// PUT: perUser is normalized OFF for the shared scope (the CRD ignores it there — keep the spec honest).
	rec = smReq(t, s, http.MethodPut, `{"enabled":true,"scope":"shared","perUser":true}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "a1"}, &live))
	require.NotNil(t, live.Spec.SessionMemory)
	assert.Equal(t, "shared", live.Spec.SessionMemory.Scope)
	assert.False(t, live.Spec.SessionMemory.PerUser, "perUser is meaningless for the shared scope")

	// PUT disable → the folded field is cleared (nil).
	rec = smReq(t, s, http.MethodPut, `{"enabled":false}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "a1"}, &live))
	assert.Nil(t, live.Spec.SessionMemory)
}
