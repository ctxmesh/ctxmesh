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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/controlplane/agentmemory"
)

func memoryConsoleServer(t *testing.T, factory *fakeCallerClientFactory, store agentmemory.Store) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients:    factory,
		AgentMemoryStore: store,
		Scheme:           testScheme(t),
		Auth:             AllowAll{},
		Adapters:         Adapters{Expand: NewExpandAdapter()},
		Version:          "test",
		Log:              logr.Discard(),
	})
}

func getAgentMemory(t *testing.T, s *Server) (*httptest.ResponseRecorder, AgentMemoryResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents/prod/asst/memory", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var resp AgentMemoryResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec, resp
}

func seedMem(t *testing.T, store agentmemory.Store, scope, subject, content string) {
	t.Helper()
	_, err := store.Remember(context.Background(), agentmemory.AgentMemory{
		Namespace: "prod", AgentName: "asst", Scope: scope, Subject: subject,
		Content: content, EmbeddingModel: "m", EmbeddingDim: 2, Embedding: []float32{1, 0},
	})
	require.NoError(t, err)
}

// The console lists an agent's AGENT-WIDE memories only — per-user memories are excluded (privacy).
func TestHandleAgentMemory_ListsAgentWideOnly(t *testing.T) {
	store := agentmemory.NewMemStore()
	seedMem(t, store, agentmemory.ScopeAgent, "", "the team prefers metric units")
	seedMem(t, store, agentmemory.ScopeAgentUser, "hash-alice", "alice's private secret")

	agent := &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "asst", Namespace: "prod"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := memoryConsoleServer(t, &fakeCallerClientFactory{client: c}, store)

	rec, resp := getAgentMemory(t, s)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, resp.Items, 1, "only agent-wide memory is listed, never a user's private memory")
	assert.Equal(t, "the team prefers metric units", resp.Items[0].Content)
	assert.NotContains(t, rec.Body.String(), "alice", "no per-user content or hashed identity leaks")
}

// splitTraceTag lifts the traceId tag into the typed field and removes it from the
// tag chips, without mutating the input (m54.3).
func TestSplitTraceTag(t *testing.T) {
	orig := map[string]string{"traceId": "abc123", "topic": "prefs"}
	traceID, rest := splitTraceTag(orig)
	assert.Equal(t, "abc123", traceID)
	assert.Equal(t, map[string]string{"topic": "prefs"}, rest, "traceId is removed from the chips")
	assert.Contains(t, orig, "traceId", "the input map must not be mutated")

	// No traceId tag ⇒ empty id, tags returned as-is.
	id2, rest2 := splitTraceTag(map[string]string{"topic": "prefs"})
	assert.Equal(t, "", id2)
	assert.Equal(t, map[string]string{"topic": "prefs"}, rest2)

	// Nil tags ⇒ no panic, empty id.
	id3, _ := splitTraceTag(nil)
	assert.Equal(t, "", id3)
}

// A missing agent is a 404 (the caller-scoped existence gate), not an empty 200.
func TestHandleAgentMemory_MissingAgentIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := memoryConsoleServer(t, &fakeCallerClientFactory{client: c}, agentmemory.NewMemStore())
	rec, _ := getAgentMemory(t, s)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// No store wired ⇒ an honest 501, never a 500.
func TestHandleAgentMemory_NoStoreIs501(t *testing.T) {
	agent := &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "asst", Namespace: "prod"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := memoryConsoleServer(t, &fakeCallerClientFactory{client: c}, nil)
	rec, _ := getAgentMemory(t, s)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
