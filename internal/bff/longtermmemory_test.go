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

func ltmReq(t *testing.T, s *Server, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/agents/default/a1/longtermmemory", nil)
	} else {
		r = httptest.NewRequest(method, "/api/agents/default/a1/longtermmemory", bytes.NewReader([]byte(body)))
	}
	r.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// The long-term-memory ENABLE surface (m49.3): GET reads the folded capability; PUT enables/disables it by
// setting/clearing spec.longTermMemory on the AgentDeployment.
func TestLongTermMemory_EnableConfigureDisable(t *testing.T) {
	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "default"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img", ExecutionModel: "serving"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// GET: initially disabled.
	rec := ltmReq(t, s, http.MethodGet, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var cfg LongTermMemoryConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))
	assert.False(t, cfg.Enabled)

	// PUT: enable per-user with an embedding route → the folded field is set.
	rec = ltmReq(t, s, http.MethodPut, `{"enabled":true,"perUser":true,"embeddingRoute":"emb"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var live agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "a1"}, &live))
	require.NotNil(t, live.Spec.LongTermMemory)
	assert.True(t, live.Spec.LongTermMemory.Enabled)
	assert.True(t, live.Spec.LongTermMemory.PerUser)
	assert.Equal(t, "emb", live.Spec.LongTermMemory.EmbeddingRoute)

	// PUT disable → the folded field is cleared (nil).
	rec = ltmReq(t, s, http.MethodPut, `{"enabled":false}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "a1"}, &live))
	assert.Nil(t, live.Spec.LongTermMemory)
}
