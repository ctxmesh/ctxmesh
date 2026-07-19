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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/run"
)

// createRun POSTs /api/runs and returns the created run id.
func createRun(t *testing.T, s *Server, body InvokeRequest) CreateRunResponse {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code, "create run should 202")
	var out CreateRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotEmpty(t, out.ID)
	assert.Equal(t, string(run.StatusQueued), out.Status)
	return out
}

// pollRun polls GET /api/runs/{id} until stopWhen(status) or a timeout.
func pollRun(t *testing.T, s *Server, id string, stopWhen func(run.Status) bool) run.Run {
	t.Helper()
	var got run.Run
	require.Eventually(t, func() bool {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/runs/"+id, nil)
		req.Header.Set("Authorization", "Bearer developer-persona-token")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			return false
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return stopWhen(got.Status)
	}, 3*time.Second, 10*time.Millisecond)
	return got
}

func TestCreateRun_Succeeds(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "tr-run", resp: []byte(`{"output":"Order shipped.","consent_required":[]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{
		Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{"input":"hi"}`), ConversationID: "chat-1",
	})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	assert.Equal(t, run.StatusSucceeded, got.Status)
	assert.Equal(t, "tr-run", got.TraceID)
	assert.Equal(t, "chat-1", got.ConversationID)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "assistant", got.Messages[0].Role)
	assert.Equal(t, "Order shipped.", got.Messages[0].Content, "the envelope's output is unwrapped")
}

func TestCreateRun_ConsentRequiresAction(t *testing.T) {
	agent := readyAgent("sk", "prod", "http://sk.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"connect your account","consent_required":["scalekit-mcp-server"]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "sk", Namespace: "prod", Input: json.RawMessage(`{}`)})
	// requires_action is NOT terminal (it can resume) — stop once we leave queued/running.
	got := pollRun(t, s, created.ID, func(st run.Status) bool {
		return st != run.StatusQueued && st != run.StatusRunning
	})

	assert.Equal(t, run.StatusRequiresAction, got.Status)
	require.NotNil(t, got.RequiresAction)
	assert.Equal(t, run.ActionConsentRequired, got.RequiresAction.Kind)
	assert.Equal(t, []string{"scalekit-mcp-server"}, got.RequiresAction.Servers)
}

func TestCreateRun_AgentFailureIsFailed(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "t", err: &invokeError{status: 502, body: []byte("upstream boom")}}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	assert.Equal(t, run.StatusFailed, got.Status, "an agent failure is an honest failed run, never a swallowed success")
	assert.NotEmpty(t, got.Error)
}

func TestRunEvents_SSE(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"Hi there.","consent_required":[]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`)})
	// Wait until terminal so the stream has the full backlog and closes cleanly (no hang).
	pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+created.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	body := rec.Body.String()
	// The stream carried the state transitions + the assistant message.
	assert.Contains(t, body, "event: state")
	assert.Contains(t, body, string(run.StatusRunning))
	assert.Contains(t, body, string(run.StatusSucceeded))
	assert.Contains(t, body, "event: message")
	assert.Contains(t, body, "Hi there.")
}

func TestGetRun_NotFound(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := newInvokeServer(t, newFakeFactory(c), &fakeInvokeAdapter{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs/nope", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
