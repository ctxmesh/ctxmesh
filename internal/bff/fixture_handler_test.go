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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agentry/internal/objectstore"
	"github.com/ctxmesh/agentry/internal/replay"
	"github.com/ctxmesh/agentry/internal/run"
)

// fixtureRunServer mirrors authzRunServer but wires the DocStore the fixture endpoint reads.
func fixtureRunServer(
	t *testing.T, username string, rs run.Store, docStore objectstore.ObjectStore,
) *Server {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(ssrInterceptor(username, nil)).
		Build()
	return NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Invoke: &fakeInvokeAdapter{}},
		RunStore:      rs,
		DocStore:      docStore,
		Version:       "test",
		Log:           logr.Discard(),
	})
}

func stepFrameJSON(t *testing.T, kind, tool string) string {
	t.Helper()
	m := map[string]any{"step": 1, "kind": kind, "tokens": map[string]int{"prompt": 1, "completion": 1}}
	if tool != "" {
		m["tool"] = tool
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return string(b)
}

func getFixture(t *testing.T, s *Server, runID string) (*httptest.ResponseRecorder, RunFixtureDTO) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/"+runID+"/fixture", nil))
	var dto RunFixtureDTO
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dto))
	}
	return rec, dto
}

// TestGetRunFixture_RecordedRun: an owner reads a recorded run's fixture — the step timeline joins to
// the recorded wire-exact I/O (model bytes incl. framing; a tool by name).
func TestGetRunFixture_RecordedRun(t *testing.T) {
	rs := run.NewMemStore()
	seedOwnedRun(t, rs, "r1", "team", "assistant", "alice@example.com", "", "hi")
	require.NoError(t, rs.AppendEvent("r1", run.EventStep, stepFrameJSON(t, "model", "")))
	require.NoError(t, rs.AppendEvent("r1", run.EventStep, stepFrameJSON(t, "tool", "search")))
	require.NoError(t, rs.AppendEvent("r1", run.EventStep, stepFrameJSON(t, "model", "")))
	// A non-frame EventStep (a workflow plan-approval label) must be ignored by the descriptor parse.
	require.NoError(t, rs.AppendEvent("r1", run.EventStep, "plan approved"))

	docStore := objectstore.NewMemObjectStore()
	fs, err := replay.NewFixtureStore(docStore)
	require.NoError(t, err)
	fx := replay.NewFixture("r1", "team/assistant")
	fx.AppendModel([]byte(`{"m":0}`), []byte("data: hello\n\n"), "text/event-stream", 200)
	fx.AppendTool("c1", "search", []byte(`{"q":"x"}`), []byte(`{"hits":1}`), "application/json")
	fx.AppendModel([]byte(`{"m":1}`), []byte(`{"answer":"done"}`), "application/json", 200)
	_, err = fs.Put(context.Background(), fx)
	require.NoError(t, err)

	s := fixtureRunServer(t, "alice@example.com", rs, docStore)
	rec, dto := getFixture(t, s, "r1")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, dto.Recorded)
	require.Len(t, dto.Steps, 3)
	assert.True(t, dto.Steps[0].Recorded)
	assert.Equal(t, "text/event-stream", dto.Steps[0].ContentType)
	assert.Equal(t, "data: hello\n\n", dto.Steps[0].Response, "SSE framing byte-exact")
	assert.Equal(t, "search", dto.Steps[1].ToolName)
	assert.Equal(t, "c1", dto.Steps[1].CallID)
	assert.Equal(t, `{"answer":"done"}`, dto.Steps[2].Response)
}

// TestGetRunFixture_NotRecorded: a run with no fixture is an honest recorded:false (200) — the
// timeline still renders (every step a gap), never a 5xx.
func TestGetRunFixture_NotRecorded(t *testing.T) {
	rs := run.NewMemStore()
	seedOwnedRun(t, rs, "r2", "team", "assistant", "alice@example.com", "", "hi")
	require.NoError(t, rs.AppendEvent("r2", run.EventStep, stepFrameJSON(t, "model", "")))

	s := fixtureRunServer(t, "alice@example.com", rs, objectstore.NewMemObjectStore())
	rec, dto := getFixture(t, s, "r2")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, dto.Recorded)
	require.Len(t, dto.Steps, 1)
	assert.False(t, dto.Steps[0].Recorded, "a not-recorded run shows the step as a gap, not fabricated I/O")
}

// TestGetRunFixture_NoObjectStore: no object store configured ⇒ honest 501 (the deploy needs
// OBJECT_STORE_* on the BFF — m109.3), never a 500 or a fabricated result.
func TestGetRunFixture_NoObjectStore(t *testing.T) {
	rs := run.NewMemStore()
	seedOwnedRun(t, rs, "r3", "team", "assistant", "alice@example.com", "", "hi")

	s := fixtureRunServer(t, "alice@example.com", rs, nil)
	rec, _ := getFixture(t, s, "r3")

	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// TestGetRunFixture_CrossTenantIs403AndNoBytes: a non-owner without agent RBAC is denied BEFORE any
// fixture bytes are read — a fixture (full prompts + tool I/O) is never a cross-tenant read oracle.
func TestGetRunFixture_CrossTenantIs403AndNoBytes(t *testing.T) {
	rs := run.NewMemStore()
	seedOwnedRun(t, rs, "r4", "victim-ns", "victim-agent", "victim@example.com", "", "topsecret-xyz")
	require.NoError(t, rs.AppendEvent("r4", run.EventStep, stepFrameJSON(t, "model", "")))

	docStore := objectstore.NewMemObjectStore()
	fs, err := replay.NewFixtureStore(docStore)
	require.NoError(t, err)
	fx := replay.NewFixture("r4", "victim-ns/victim-agent")
	fx.AppendModel([]byte(`{"secret":"topsecret-xyz"}`), []byte("resp"), "application/json", 200)
	_, err = fs.Put(context.Background(), fx)
	require.NoError(t, err)

	// A caller who is neither the owner nor RBAC-authorized on the run's agent is DENIED before any
	// fixture bytes are read (403 forbidden or 404 no-oracle — both are a no-leak deny; the fake
	// caller has no agents, so authorizeRunAccess resolves to a not-found agent → 404).
	s := fixtureRunServer(t, "attacker@example.com", rs, docStore)
	rec, _ := getFixture(t, s, "r4")

	assert.NotEqual(t, http.StatusOK, rec.Code, "a non-owner without access must be denied, never served")
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code)
	assert.NotContains(t, rec.Body.String(), "topsecret-xyz", "a denied response must not leak fixture bytes")
}
