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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// recordCapableAgent is a ready agent marked spec.record=true (record-capable).
func recordCapableAgent(name, namespace, url string) *agentsv1alpha1.AgentDeployment {
	a := readyAgent(name, namespace, url)
	a.Spec.Record = true
	return a
}

// TestCreateRun_Record_FailsClosedOnNonRecordCapableAgent proves the C2 fail-closed gate (M78, ADR
// 0071 §1): asking to record a run against an agent that is NOT record-capable (spec.record unset)
// is REFUSED with a clear 400 — never a silently-record-nothing run.
func TestCreateRun_Record_FailsClosedOnNonRecordCapableAgent(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local") // NOT record-capable
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"ok","consent_required":[]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	raw, _ := json.Marshal(InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`), Record: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "record on a non-record-capable agent must fail closed")
	assert.Contains(t, rec.Body.String(), "record-capable", "the error must name the record-capability gap")
}

// TestCreateRun_Record_AcceptedOnRecordCapableAgent proves a record run against a record-capable
// agent is accepted and the run carries Record=true (the per-run capture toggle propagates).
func TestCreateRun_Record_AcceptedOnRecordCapableAgent(t *testing.T) {
	agent := recordCapableAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "tr", resp: []byte(`{"output":"ok","consent_required":[]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`), Record: true})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	assert.True(t, got.Record, "a recorded run must persist Record=true")
	assert.Equal(t, run.StatusSucceeded, got.Status)
}

// TestCreateRun_NoRecord_UnaffectedByCapability proves a NON-record run is byte-for-byte unchanged
// whether or not the agent is record-capable (record is strictly opt-in).
func TestCreateRun_NoRecord_UnaffectedByCapability(t *testing.T) {
	agent := recordCapableAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "tr", resp: []byte(`{"output":"ok","consent_required":[]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	assert.False(t, got.Record, "a run that did not opt in must not be recorded")
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

// TestResumeRun_Unauthorized403 is the M113 caller-scoped resume gate (closing the pre-V15 hole Fable
// flagged): a caller who is NEITHER the run's creator NOR RBAC-authorized on its backing agent cannot
// approve/deny it — the deny path must not cancel another tenant's paused run on a bare bearer.
func TestResumeRun_Unauthorized403(t *testing.T) {
	agent := readyAgent("sk", "prod", "http://sk.prod.svc.cluster.local")
	// The caller's RBAC FORBIDS reading the backing agent (and the SSR identity won't match the run's
	// creator), so authorizeRunAccess denies. (Get is forbidden only for the AgentDeployment so the run
	// store + other reads still work.)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*agentsv1alpha1.AgentDeployment); ok {
					return apierrors.NewForbidden(
						k8sschema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"},
						key.Name, errors.New("forbidden"))
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	s := newInvokeServer(t, newFakeFactory(c), &fakeInvokeAdapter{})

	// Seed a paused mid-run approval run owned by ANOTHER principal, directly (create would hit the
	// forbidden Get). This is exactly the deny-path target Fable flagged.
	rn := run.New("run-victim", "prod", "sk", json.RawMessage(`{}`), "", time.Now())
	rn.CallerUsername = "alice@example.com"
	require.NoError(t, s.runStore.Create(rn))
	_, err := s.runStore.Update("run-victim", func(x *run.Run) error {
		if err := x.Transition(run.StatusRunning, time.Now()); err != nil {
			return err
		}
		x.RequiresAction = &run.Action{Kind: run.ActionApproval, Key: "tool:x", Message: "approve the step"}
		return x.Transition(run.StatusRequiresAction, time.Now())
	})
	require.NoError(t, err)

	// A different caller tries to DENY (cancel) it → forbidden, and the run is UNCHANGED.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/run-victim/resume", bytes.NewReader([]byte(`{"decision":"deny"}`)))
	req.Header.Set("Authorization", "Bearer intruder-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, "a non-owner without RBAC on the backing agent cannot resume/deny")

	got, gErr := s.runStore.Get("run-victim")
	require.NoError(t, gErr)
	assert.Equal(t, run.StatusRequiresAction, got.Status, "the unauthorized deny did NOT cancel the run")
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
	assert.Equal(t, "approval denied", got.Error, "no reason ⇒ the bare base error (backward-compatible)")
}

// TestResumeRun_ApprovalDenied_WithReason is V16 (m115.4): an optional reason on the deny is stored on the
// run so the denial is explainable on the run detail (not just the bare "approval denied").
func TestResumeRun_ApprovalDenied_WithReason(t *testing.T) {
	agent := readyAgent("mailer", "prod", "http://mailer.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	s := newInvokeServer(t, newFakeFactory(c), approvalInvokeAdapter{})

	created := createRun(t, s, InvokeRequest{Agent: "mailer", Namespace: "prod", Input: json.RawMessage(`{"input":"email the customer"}`)})
	pollRun(t, s, created.ID, func(st run.Status) bool {
		return st != run.StatusQueued && st != run.StatusRunning
	})

	rec := resumeRun(t, s, created.ID, `{"decision":"deny","reason":"not authorized to email customers"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusCancelled, got.Status)
	assert.Equal(t, "approval denied: not authorized to email customers", got.Error,
		"the human-supplied reason is appended to the stored error")
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
// emits the canned tokens + step frames then returns the final envelope, so the run executor streams
// token + step events (M78, ADR 0071 §4).
type fakeStreamingInvokeAdapter struct {
	traceID string
	tokens  []string
	// steps are canned `step` metadata JSON payloads streamed to onStep (M78) — the raw JSON the
	// run store persists as EventStep and the console renders as live step-visibility.
	steps []string
	final []byte
	err   error
}

func (f *fakeStreamingInvokeAdapter) Invoke(context.Context, string, []byte) ([]byte, string, error) {
	return f.final, f.traceID, f.err
}

func (f *fakeStreamingInvokeAdapter) InvokeStream(_ context.Context, _ string, _ []byte, onToken func(string), onStep func(string)) ([]byte, string, error) {
	if f.err != nil {
		return nil, f.traceID, f.err
	}
	for _, tok := range f.tokens {
		onToken(tok)
	}
	for _, st := range f.steps {
		onStep(st)
	}
	return f.final, f.traceID, nil
}

func TestCreateRun_StreamsTokens(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	// The agent streams a model step frame + a tool step frame (M78, ADR 0071 §4). The BFF must
	// republish each verbatim as an EventStep on the run stream (live step-visibility).
	stepModel := `{"type":"step","step":1,"kind":"model","tokens":{"prompt":11,"completion":7},"ref":null}`
	stepTool := `{"type":"step","step":1,"kind":"tool","tool":"echo_tool","tokens":{"prompt":0,"completion":0},"ref":null}`
	inv := &fakeStreamingInvokeAdapter{
		traceID: "t",
		tokens:  []string{"Hel", "lo", " there"},
		steps:   []string{stepModel, stepTool},
		final:   []byte(`{"output":"Hello there","consent_required":[]}`),
	}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`)})
	final := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })
	assert.Equal(t, run.StatusSucceeded, final.Status)
	assert.Equal(t, "Hello there", final.Messages[0].Content)

	// The event stream carried the token deltas as they arrived, the step metadata frames, then
	// the message + state.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+created.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	assert.Contains(t, body, "event: token")
	assert.Contains(t, body, "Hel")
	assert.Contains(t, body, "lo")
	// The step frames were republished as EventStep, their Data the verbatim step-metadata JSON
	// (JSON-escaped inside the run Event's `data` field on the wire).
	assert.Contains(t, body, "event: step")
	assert.Contains(t, body, `\"kind\":\"model\"`)
	assert.Contains(t, body, `\"tool\":\"echo_tool\"`)
	assert.Contains(t, body, "event: message")
	assert.Contains(t, body, "event: state")
	assert.Contains(t, body, string(run.StatusSucceeded))
}

// agentWithSchema builds a ready "typed" AgentDeployment whose spec.runtime.outputSchema is set to
// the given raw JSON Schema, so the create path can pin it onto the run (m65.3). Only the schema
// varies across tests; the identity is fixed.
func agentWithSchema(schema string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "typed", Namespace: "prod"},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "echo:1",
			Runtime: &agentsv1alpha1.RuntimeSpec{
				OutputSchema: &k8sruntime.RawExtension{Raw: []byte(schema)},
			},
		},
		Status: agentsv1alpha1.AgentDeploymentStatus{URL: "http://typed.prod.svc.cluster.local"},
	}
}

// TestCreateRun_PinsOutputSchema proves that a run created for an agent with
// spec.runtime.outputSchema has its OutputSchema field set to the raw JSON Schema
// text of the agent at create time (m65.3, ADR 0058).
func TestCreateRun_PinsOutputSchema(t *testing.T) {
	schema := `{"type":"object","properties":{"answer":{"type":"string"}}}`
	agent := agentWithSchema(schema)
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

// TestCreateRun_RoutesToActiveAgentAfterHandoff — active-agent routing (m67.6, ADR 0060 §5): when a run
// is created for a conversation that a prior handoff transferred to agent B AND no explicit agent is
// given, the run routes to B (the active agent), not a 400.
func TestCreateRun_RoutesToActiveAgentAfterHandoff(t *testing.T) {
	// B is a deployed, ready agent the conversation was handed off to.
	agentB := readyAgent("billing-agent", "prod", "http://billing-agent.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agentB).Build()
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"how can I help with billing?","consent_required":[]}`)}

	store := run.NewMemStore()
	conv := run.NewMemConversationStore()
	require.NoError(t, conv.SetActiveAgent("chat-handed-off", "prod", "billing-agent", "A-1"))
	s := NewServer(Options{
		CallerClients:     newFakeFactory(c),
		Scheme:            testScheme(t),
		Auth:              AllowAll{},
		Adapters:          Adapters{Invoke: inv},
		Version:           "test",
		Log:               logr.Discard(),
		RunStore:          store,
		ConvStore:         conv,
		RunWorkerDispatch: true, // keep queued so we can read the routed agent before execution
	})

	// No explicit agent — only the conversationId. Routing resolves it to the active agent B.
	created := createRun(t, s, InvokeRequest{ConversationID: "chat-handed-off", Input: json.RawMessage(`{}`)})
	rn, err := store.Get(created.ID)
	require.NoError(t, err)
	assert.Equal(t, "billing-agent", rn.Agent, "an agent-less run on a handed-off conversation routes to the active agent B")
	assert.Equal(t, "chat-handed-off", rn.ConversationID)
}

// TestCreateRun_ExplicitAgentOverridesActivePointer — an explicit agent always wins over the pointer.
func TestCreateRun_ExplicitAgentOverridesActivePointer(t *testing.T) {
	agentEcho := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agentEcho).Build()
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"ok","consent_required":[]}`)}

	store := run.NewMemStore()
	conv := run.NewMemConversationStore()
	require.NoError(t, conv.SetActiveAgent("chat-x", "prod", "billing-agent", "A-1"))
	s := NewServer(Options{
		CallerClients:     newFakeFactory(c),
		Scheme:            testScheme(t),
		Auth:              AllowAll{},
		Adapters:          Adapters{Invoke: inv},
		Version:           "test",
		Log:               logr.Discard(),
		RunStore:          store,
		ConvStore:         conv,
		RunWorkerDispatch: true,
	})

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", ConversationID: "chat-x", Input: json.RawMessage(`{}`)})
	rn, err := store.Get(created.ID)
	require.NoError(t, err)
	assert.Equal(t, "echo", rn.Agent, "an EXPLICIT agent overrides the active-agent pointer")
}

// TestHandoffMarkerPresent — the executeRun guard (m67.6) suppresses termination ONLY for a SUCCESSFUL
// transfer (ok:true, the edge already terminated the run). A refused handoff (ok:false) or an absent
// marker must return false so executeRun terminates the still-running source run (the stranded-run fix).
func TestHandoffMarkerPresent(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"successful transfer", `{"output":"","handoff":{"ok":"true","targetAgent":"billing"}}`, true},
		{"refused handoff", `{"output":"","handoff":{"ok":"false","error":"not a member"}}`, false},
		{"empty handoff object", `{"output":"x","handoff":{}}`, false},
		{"null handoff", `{"output":"x","handoff":null}`, false},
		{"no handoff key", `{"output":"a normal answer"}`, false},
		{"not json", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, handoffMarkerPresent([]byte(tc.body)))
		})
	}
}

// m65.4OutputSchema is the schema pinned onto runs in the m65.4 executeRun integration tests: an
// object with a required string "answer".
const m65_4OutputSchema = `{
	"type": "object",
	"properties": {"answer": {"type": "string"}},
	"required": ["answer"],
	"additionalProperties": false
}`

// runEventsBody returns the run's full SSE event backlog (the run must already be terminal so the
// stream closes cleanly). Used to assert which stream events did — and did NOT — surface.
func runEventsBody(t *testing.T, s *Server, id string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+id+"/events", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	return rec.Body.String()
}

// TestExecuteRun_SchemaConformingSucceeds proves the authoritative gate (m65.4, ADR 0058) passes a
// terminal answer that is valid JSON conforming to the pinned outputSchema: the run succeeds, the
// answer is preserved, and the assistant message is emitted — byte-for-byte the pre-m65.4 success.
func TestExecuteRun_SchemaConformingSucceeds(t *testing.T) {
	agent := agentWithSchema(m65_4OutputSchema)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "tr", resp: []byte(`{"output":"{\"answer\":\"shipped\"}","consent_required":[]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "typed", Namespace: "prod", Input: json.RawMessage(`{}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	assert.Equal(t, run.StatusSucceeded, got.Status, "conforming JSON must succeed unchanged")
	assert.Empty(t, got.Error)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, `{"answer":"shipped"}`, got.Messages[0].Content, "the conforming answer is preserved verbatim")

	body := runEventsBody(t, s, created.ID)
	assert.Contains(t, body, "event: message", "a successful run emits its assistant message")
	assert.Contains(t, body, string(run.StatusSucceeded))
}

// TestExecuteRun_NonConformingFailsClosed proves fail-closed on a valid-JSON answer that VIOLATES
// the schema (missing the required "answer"): the run is an honest `failed` with a schema/validation
// error, no assistant message is surfaced, and there is NO `event: message` on the stream.
func TestExecuteRun_NonConformingFailsClosed(t *testing.T) {
	agent := agentWithSchema(m65_4OutputSchema)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	// Valid JSON, but missing the required "answer" property → violates the schema.
	inv := &fakeInvokeAdapter{traceID: "tr", resp: []byte(`{"output":"{\"note\":\"nope\"}","consent_required":[]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "typed", Namespace: "prod", Input: json.RawMessage(`{}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	assert.Equal(t, run.StatusFailed, got.Status,
		"a schema-violating answer is an honest failed run, never a swallowed success")
	assert.NotEmpty(t, got.Error)
	assert.Contains(t, got.Error, "schema", "the failure explains the validation problem")
	assert.Empty(t, got.Messages, "a rejected answer must NOT be persisted as an assistant message")

	body := runEventsBody(t, s, created.ID)
	assert.NotContains(t, body, "event: message",
		"a rejected answer must NOT be surfaced as a successful assistant message on the stream")
	assert.Contains(t, body, string(run.StatusFailed))
}

// TestExecuteRun_NonJSONFailsClosed proves fail-closed when a schema is set but the terminal answer
// is not JSON at all (plain prose): honest `failed`, no message surfaced.
func TestExecuteRun_NonJSONFailsClosed(t *testing.T) {
	agent := agentWithSchema(m65_4OutputSchema)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "tr", resp: []byte(`{"output":"shipped, as prose","consent_required":[]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "typed", Namespace: "prod", Input: json.RawMessage(`{}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	assert.Equal(t, run.StatusFailed, got.Status, "non-JSON against a schema is a failed run")
	assert.NotEmpty(t, got.Error)
	assert.Empty(t, got.Messages)
	assert.NotContains(t, runEventsBody(t, s, created.ID), "event: message")
}

// TestExecuteRun_NoSchemaUnchanged proves the regression guard: a run with NO pinned schema is the
// exact pre-m65.4 path — any answer (here, plain prose) succeeds and is surfaced unchanged.
func TestExecuteRun_NoSchemaUnchanged(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "tr", resp: []byte(`{"output":"just prose, no schema","consent_required":[]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	assert.Equal(t, run.StatusSucceeded, got.Status, "no schema => no validation, unchanged success")
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "just prose, no schema", got.Messages[0].Content)
}

// TestExecuteRun_MalformedSchemaFailsClosed proves that a pinned schema that does not compile as a
// JSON Schema (possible because the CRD stores it preserve-unknown / unvalidated) DENIES: an
// unenforceable governance control fails the run closed rather than silently passing the answer.
func TestExecuteRun_MalformedSchemaFailsClosed(t *testing.T) {
	// A structurally invalid schema ("type" must be a string/array of strings, not a number).
	agent := agentWithSchema(`{"type": 123}`)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "tr", resp: []byte(`{"output":"{\"answer\":\"shipped\"}","consent_required":[]}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "typed", Namespace: "prod", Input: json.RawMessage(`{}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	assert.Equal(t, run.StatusFailed, got.Status, "an uncompilable schema must fail closed, not pass the answer")
	assert.Contains(t, got.Error, "not a valid JSON Schema")
	assert.Empty(t, got.Messages)
}

// TestWithApprovals covers the durable-run HITL-approve grant injection (m120.2). The critical case
// is a NON-object input (the console sends `input` as a plain string, so rn.Input is the JSON scalar
// "…"): before m120.2 that no-op'd and the run re-paused on every resume. withApprovals must WRAP it
// so the granted key rides the re-invoke and the SDK's pause_for_approval(key) proceeds.
func TestWithApprovals(t *testing.T) {
	keys := []string{"tool:echo"}

	t.Run("string input is wrapped so the grant rides the re-invoke", func(t *testing.T) {
		// rn.Input for a console prompt is the JSON scalar "hello there" (json.RawMessage).
		out := withApprovals([]byte(`"hello there"`), keys)
		var got struct {
			Input     string   `json:"input"`
			Approvals []string `json:"approvals"`
		}
		require.NoError(t, json.Unmarshal(out, &got))
		assert.Equal(t, "hello there", got.Input, "the original prompt must survive the wrap")
		assert.Equal(t, keys, got.Approvals, "the granted approval key must be present")
	})

	t.Run("object input merges approvals as a sibling key", func(t *testing.T) {
		out := withApprovals([]byte(`{"input":"hi","conversation_id":"c1"}`), keys)
		var got map[string]any
		require.NoError(t, json.Unmarshal(out, &got))
		assert.Equal(t, "hi", got["input"])
		assert.Equal(t, "c1", got["conversation_id"], "existing fields are preserved")
		assert.Equal(t, []any{"tool:echo"}, got["approvals"])
	})

	t.Run("plain (non-JSON) text is quoted into a JSON string input", func(t *testing.T) {
		out := withApprovals([]byte(`plain prompt`), keys)
		var got struct {
			Input     string   `json:"input"`
			Approvals []string `json:"approvals"`
		}
		require.NoError(t, json.Unmarshal(out, &got))
		assert.Equal(t, "plain prompt", got.Input)
		assert.Equal(t, keys, got.Approvals)
	})
}
