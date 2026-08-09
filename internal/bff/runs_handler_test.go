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
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
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

func TestResumeRun(t *testing.T) {
	agent := readyAgent("sk", "prod", "http://sk.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	// First invoke returns consent_required → requires_action; the resume re-invokes and (with the
	// now-connected credential, simulated by flipping the adapter) succeeds.
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"connect","consent_required":["scalekit-mcp-server"]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "sk", Namespace: "prod", Input: json.RawMessage(`{"input":"go"}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool {
		return st != run.StatusQueued && st != run.StatusRunning
	})
	require.Equal(t, run.StatusRequiresAction, got.Status)

	// The user connected → the agent now returns a real answer. Resume.
	inv.resp = []byte(`{"output":"Here are your environments.","consent_required":[]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+created.ID+"/resume", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	final := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusSucceeded, final.Status)
	require.Len(t, final.Messages, 1)
	assert.Equal(t, "Here are your environments.", final.Messages[0].Content)
}

func TestResumeRun_NotAwaitingAction(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"done","consent_required":[]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)
	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`)})
	pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	// Resuming a succeeded run is a 409 (nothing to resume).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+created.ID+"/resume", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

// approvalInvokeAdapter models a human-in-the-loop agent (m32.4): the first invoke pauses for
// approval; once the re-invoke carries the granted approval key in its `approvals`, it succeeds.
type approvalInvokeAdapter struct{}

func (approvalInvokeAdapter) Invoke(_ context.Context, _ string, body []byte) ([]byte, string, error) {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(body, &m)
	if _, approved := m["approvals"]; approved {
		return []byte(`{"output":"email sent","consent_required":[]}`), "tr-appr", nil
	}
	return []byte(`{"output":"awaiting approval",` +
		`"approval_required":{"key":"send-email","summary":"Send the email to the customer?"},` +
		`"consent_required":[]}`), "tr-appr", nil
}

func resumeRun(t *testing.T, s *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+id+"/resume", rdr)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestResumeRun_Approval(t *testing.T) {
	agent := readyAgent("mailer", "prod", "http://mailer.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := newInvokeServer(t, newFakeFactory(c), approvalInvokeAdapter{})

	created := createRun(t, s, InvokeRequest{Agent: "mailer", Namespace: "prod", Input: json.RawMessage(`{"input":"email the customer"}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool {
		return st != run.StatusQueued && st != run.StatusRunning
	})
	require.Equal(t, run.StatusRequiresAction, got.Status)
	require.NotNil(t, got.RequiresAction)
	assert.Equal(t, run.ActionApproval, got.RequiresAction.Kind)
	assert.Equal(t, "send-email", got.RequiresAction.Key)
	assert.Equal(t, "Send the email to the customer?", got.RequiresAction.Message)

	// Approve → the run re-invokes with the key granted and succeeds.
	require.Equal(t, http.StatusAccepted, resumeRun(t, s, created.ID, `{"decision":"approve"}`).Code)
	final := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusSucceeded, final.Status)
	require.NotEmpty(t, final.Messages)
	assert.Equal(t, "email sent", final.Messages[len(final.Messages)-1].Content)
}

func TestResumeRun_ApprovalDenied(t *testing.T) {
	agent := readyAgent("mailer", "prod", "http://mailer.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := newInvokeServer(t, newFakeFactory(c), approvalInvokeAdapter{})

	created := createRun(t, s, InvokeRequest{Agent: "mailer", Namespace: "prod", Input: json.RawMessage(`{"input":"email the customer"}`)})
	pollRun(t, s, created.ID, func(st run.Status) bool {
		return st != run.StatusQueued && st != run.StatusRunning
	})

	// Deny → the run is cancelled (terminal), never re-invoked.
	rec := resumeRun(t, s, created.ID, `{"decision":"deny"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusCancelled, got.Status, "a denied approval cancels the run")
}

func TestCancelRun(t *testing.T) {
	agent := readyAgent("sk", "prod", "http://sk.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	// Pause the run in requires_action so it is non-terminal + stable to cancel deterministically.
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"connect","consent_required":["scalekit-mcp-server"]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "sk", Namespace: "prod", Input: json.RawMessage(`{}`)})
	pollRun(t, s, created.ID, func(st run.Status) bool {
		return st != run.StatusQueued && st != run.StatusRunning
	})

	// Cancel → 200 cancelled.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+created.ID+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusCancelled, got.Status)

	// Cancelling a terminal run → 409.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/runs/"+created.ID+"/cancel", nil)
	req2.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusConflict, rec2.Code, "a terminal run cannot be cancelled")
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

// fakeStreamingInvokeAdapter implements BOTH InvokeAdapter and StreamingInvokeAdapter: InvokeStream
// emits the canned tokens then returns the final envelope, so the run executor streams token events.
type fakeStreamingInvokeAdapter struct {
	traceID string
	tokens  []string
	final   []byte
	err     error
}

func (f *fakeStreamingInvokeAdapter) Invoke(context.Context, string, []byte) ([]byte, string, error) {
	return f.final, f.traceID, f.err
}

func (f *fakeStreamingInvokeAdapter) InvokeStream(_ context.Context, _ string, _ []byte, onToken func(string)) ([]byte, string, error) {
	if f.err != nil {
		return nil, f.traceID, f.err
	}
	for _, tok := range f.tokens {
		onToken(tok)
	}
	return f.final, f.traceID, nil
}

func TestCreateRun_StreamsTokens(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeStreamingInvokeAdapter{
		traceID: "t",
		tokens:  []string{"Hel", "lo", " there"},
		final:   []byte(`{"output":"Hello there","consent_required":[]}`),
	}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`)})
	final := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusSucceeded, final.Status)
	assert.Equal(t, "Hello there", final.Messages[0].Content)

	// The event stream carried the token deltas as they arrived, then the message + state.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+created.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	assert.Contains(t, body, "event: token")
	assert.Contains(t, body, "Hel")
	assert.Contains(t, body, "lo")
	assert.Contains(t, body, "event: message")
	assert.Contains(t, body, "event: state")
	assert.Contains(t, body, string(run.StatusSucceeded))
}

// agentWithSchema builds a ready AgentDeployment whose spec.runtime.outputSchema is set to
// the given raw JSON Schema, so the create path can pin it onto the run (m65.3).
func agentWithSchema(name, namespace, url, schema string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "echo:1",
			Runtime: &agentsv1alpha1.RuntimeSpec{
				OutputSchema: &k8sruntime.RawExtension{Raw: []byte(schema)},
			},
		},
		Status: agentsv1alpha1.AgentDeploymentStatus{URL: url},
	}
}

// TestCreateRun_PinsOutputSchema proves that a run created for an agent with
// spec.runtime.outputSchema has its OutputSchema field set to the raw JSON Schema
// text of the agent at create time (m65.3, ADR 0058).
func TestCreateRun_PinsOutputSchema(t *testing.T) {
	schema := `{"type":"object","properties":{"answer":{"type":"string"}}}`
	agent := agentWithSchema("typed", "prod", "http://typed.prod.svc.cluster.local", schema)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"done","consent_required":[]}`)}

	store := run.NewMemStore()
	s := NewServer(Options{
		CallerClients:     newFakeFactory(c),
		Scheme:            testScheme(t),
		Auth:              AllowAll{},
		Adapters:          Adapters{Invoke: inv},
		Version:           "test",
		Log:               logr.Discard(),
		RunStore:          store,
		RunWorkerDispatch: true, // keep queued so we can read OutputSchema before execution mutates it
	})

	created := createRun(t, s, InvokeRequest{Agent: "typed", Namespace: "prod", Input: json.RawMessage(`{}`)})
	rn, err := store.Get(created.ID)
	require.NoError(t, err)
	assert.Equal(t, schema, rn.OutputSchema,
		"OutputSchema must be pinned to the agent's spec.runtime.outputSchema at create time")
}

// TestCreateRun_NoRuntimeOutputSchemaIsEmpty proves that a run created for an agent with
// no spec.runtime gets OutputSchema == "" (backward-compat: run-create must not fail m65.3).
func TestCreateRun_NoRuntimeOutputSchemaIsEmpty(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"done","consent_required":[]}`)}

	store := run.NewMemStore()
	s := NewServer(Options{
		CallerClients:     newFakeFactory(c),
		Scheme:            testScheme(t),
		Auth:              AllowAll{},
		Adapters:          Adapters{Invoke: inv},
		Version:           "test",
		Log:               logr.Discard(),
		RunStore:          store,
		RunWorkerDispatch: true,
	})

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`)})
	rn, err := store.Get(created.ID)
	require.NoError(t, err)
	assert.Equal(t, "", rn.OutputSchema,
		"a run for an agent with no runtime must have OutputSchema == \"\" (no validation, backward-compat)")
}
