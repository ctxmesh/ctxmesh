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
	"io"
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

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/runcap"
)

// fakeInvokeAdapter is the test double for InvokeAdapter. It records the endpoint
// + body it was handed and returns a preset traceId/response/err, so the handler
// tests prove the caller-scoped resolve → invoke → traceId hand-off WITHOUT a
// real agent or trace backend (tier0 determinism).
type fakeInvokeAdapter struct {
	traceID     string
	resp        []byte
	err         error
	gotEndpoint string
	gotBody     []byte
	gotCtx      context.Context
	called      bool
}

func (f *fakeInvokeAdapter) Invoke(ctx context.Context, endpoint string, body []byte) ([]byte, string, error) {
	f.called = true
	f.gotEndpoint = endpoint
	f.gotBody = body
	f.gotCtx = ctx
	return f.resp, f.traceID, f.err
}

// newInvokeServer builds a Server whose /api/invoke runs through the fake caller
// factory + the given fake InvokeAdapter. AllowAll edge auth so the factory/handler
// drive the token flow.
func newInvokeServer(t *testing.T, factory CallerClientFactory, inv InvokeAdapter) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients: factory,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Invoke: inv},
		Version:       "test",
		Log:           logr.Discard(),
	})
}

// readyAgent is an AgentDeployment with an assigned endpoint (status.url) — the
// happy-path fixture the Playground resolves and dispatches to.
func readyAgent(name, namespace, url string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "echo:1"},
		Status:     agentsv1alpha1.AgentDeploymentStatus{URL: url},
	}
}

// TestInvokeReturnsTraceID proves the happy path: the caller-scoped handler
// resolves the agent's endpoint, invokes it via the adapter, and returns the
// run's traceId + response. The adapter is handed the resolved endpoint (proving
// the caller-scoped CRD read fed the dispatch) and the verbatim input body.
func TestInvokeReturnsTraceID(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "abc123trace", resp: []byte(`{"answer":"MOCK_OK"}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	reqBody, _ := json.Marshal(InvokeRequest{
		Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{"prompt":"hi"}`),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body InvokeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "abc123trace", body.TraceID, "the run's traceId is the hand-off")
	assert.Contains(t, body.Response, "MOCK_OK")

	// The adapter was driven with the CALLER-SCOPED-resolved endpoint + verbatim input.
	assert.True(t, inv.called)
	assert.Equal(t, "http://echo.prod.svc.cluster.local", inv.gotEndpoint)
	assert.JSONEq(t, `{"prompt":"hi"}`, string(inv.gotBody))
}

// TestInvokeSurfacesConsentRequired proves /invoke lifts the agent's structured
// consent_required (m25.9) onto the response DTO so the console can render a CTA.
func TestInvokeSurfacesConsentRequired(t *testing.T) {
	agent := readyAgent("sk", "prod", "http://sk.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{
		traceID: "t",
		resp:    []byte(`{"output":"connect your account","tools_called":[],"consent_required":["scalekit-mcp-server"]}`),
	}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	reqBody, _ := json.Marshal(InvokeRequest{Agent: "sk", Namespace: "prod", Input: json.RawMessage(`{}`)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body InvokeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, []string{"scalekit-mcp-server"}, body.ConsentRequired)

	// A plain result carries no consent_required (no CTA).
	assert.Nil(t, parseConsentRequired([]byte(`{"output":"done","tools_called":["x"]}`)))
	assert.Nil(t, parseConsentRequired([]byte(`not json`)))
}

// TestInvokeMintsAndAttachesRunCapability proves the authenticated /invoke mints the
// invoking user's run capability (runcap, ADR 0030 §2) and carries it on the adapter's
// context: it verifies under the platform public key with the caller's hashed identity as
// `sub` and the agent as the actor — never a value the agent supplied.
func TestInvokeMintsAndAttachesRunCapability(t *testing.T) {
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)

	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(agent).
		WithInterceptorFuncs(ssrInterceptor("alice@example.com", nil)).
		Build()
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{}`)}
	s := NewServer(Options{
		CallerClients:               newFakeFactory(c),
		Scheme:                      testScheme(t),
		Auth:                        AllowAll{},
		Adapters:                    Adapters{Invoke: inv},
		Version:                     "test",
		MCPEnabled:                  true,
		MCPCapabilityPrivateSeedB64: runcap.EncodePrivateSeed(priv),
		MCPCapabilityAudience:       "test-plane",
		Log:                         logr.Discard(),
	})

	reqBody, _ := json.Marshal(InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{"prompt":"hi"}`)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	require.NotNil(t, inv.gotCtx)
	token := runCapabilityFromContext(inv.gotCtx)
	require.NotEmpty(t, token, "an authenticated /invoke with a signer must mint + attach a capability")

	capb, err := runcap.NewVerifier(pub, "test-plane", nil).Verify(token)
	require.NoError(t, err, "the minted capability must verify under the platform public key")
	assert.Equal(t, userGrantHash("alice@example.com"), capb.User, "sub is the caller's HASHED identity")
	assert.Equal(t, "prod/echo", capb.Agent, "the act claim is the invoked agent")
	assert.NotEmpty(t, capb.RunID)
}

// TestInvokeWithoutSignerAttachesNoCapability proves the default (no platform capability
// key) mints nothing — the run proceeds without a capability (unattended / org-public).
func TestInvokeWithoutSignerAttachesNoCapability(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(agent).
		WithInterceptorFuncs(ssrInterceptor("alice@example.com", nil)).
		Build()
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	reqBody, _ := json.Marshal(InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, runCapabilityFromContext(inv.gotCtx), "no signer ⇒ no capability minted")
}

// TestInvokeAdapterAttachesCapabilityHeader proves the pure-HTTP adapter attaches the
// run-capability header (from the request context) on the outbound /invoke — the hop that
// carries the capability from the BFF, through the launcher, to the agent.
func TestInvokeAdapterAttachesCapabilityHeader(t *testing.T) {
	var gotCap string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCap = r.Header.Get(runcap.HeaderName)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	adapter := NewInvokeAdapter(InvokeAdapterConfig{HTTPClient: srv.Client()})

	_, _, err := adapter.Invoke(contextWithRunCapability(context.Background(), "cap-token-xyz"), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "cap-token-xyz", gotCap, "the adapter attaches the capability from the context")

	gotCap = ""
	_, _, err = adapter.Invoke(context.Background(), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.Empty(t, gotCap, "no capability on the context ⇒ no header")
}

// TestInvokeAdapterAttachesConversationHeader proves the adapter forwards a conversation id
// carried on the context as X-Conversation-Id — the header the memory-aware agent reads to
// scope its thread — and attaches NO header when the run is single-shot (no id).
func TestInvokeAdapterAttachesConversationHeader(t *testing.T) {
	var gotConv string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotConv = r.Header.Get(hdrConversationID)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	adapter := NewInvokeAdapter(InvokeAdapterConfig{HTTPClient: srv.Client()})

	_, _, err := adapter.Invoke(contextWithConversationID(context.Background(), "chat-42"), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "chat-42", gotConv, "the adapter attaches the conversation id from the context")

	gotConv = ""
	_, _, err = adapter.Invoke(context.Background(), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.Empty(t, gotConv, "no conversation id on the context ⇒ no header (single-shot run)")
}

// TestInvokeAdapterAttachesRecordHeader proves the adapter stamps the per-run record-mode capture
// toggle (M78, ADR 0071 §1) as X-Ctxmesh-Record: <runId> when the run is being recorded, and
// attaches NO header for a normal (non-recorded) run.
func TestInvokeAdapterAttachesRecordHeader(t *testing.T) {
	var gotRecord string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRecord = r.Header.Get(hdrRecord)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	adapter := NewInvokeAdapter(InvokeAdapterConfig{HTTPClient: srv.Client()})

	_, _, err := adapter.Invoke(contextWithRecord(context.Background(), "run-rec-1"), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "run-rec-1", gotRecord, "the adapter stamps X-Ctxmesh-Record from the context for a recorded run")

	gotRecord = ""
	_, _, err = adapter.Invoke(context.Background(), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.Empty(t, gotRecord, "no record id on the context ⇒ no header (non-recorded run)")
}

// TestInvokeAdapterAttachesIncludeHistoryHeader proves the adapter stamps the handoff INPUT FILTER
// (m83.6) as X-Ctxmesh-Include-History: false ONLY on a handoff target's transfer turn (the context
// carries skip=true), and attaches NO header otherwise (a normal run / a default handoff replays).
func TestInvokeAdapterAttachesIncludeHistoryHeader(t *testing.T) {
	var gotHdr string
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHdr = r.Header.Get(hdrIncludeHistory)
		_, present = r.Header[http.CanonicalHeaderKey(hdrIncludeHistory)]
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	adapter := NewInvokeAdapter(InvokeAdapterConfig{HTTPClient: srv.Client()})

	_, _, err := adapter.Invoke(contextWithSkipHistoryReplay(context.Background(), true), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "false", gotHdr, "skip=true ⇒ the adapter stamps X-Ctxmesh-Include-History: false (B skips replay)")

	gotHdr, present = "", true
	_, _, err = adapter.Invoke(context.Background(), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.False(t, present, "no skip signal ⇒ no header (default handoff / normal run replays as today)")

	// An explicit skip=false is also the default — no header (the run-worker only sets the ctx when true).
	present = true
	_, _, err = adapter.Invoke(contextWithSkipHistoryReplay(context.Background(), false), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.False(t, present, "skip=false ⇒ no header (replay as today)")
}

// TestInvokeAdapterAttachesSpawnHeaders proves the adapter forwards a run's spawn-tree position
// (M64) as X-Ctxmesh-Spawn-Root/Depth when present, and attaches none for a plain (non-spawn) run.
func TestInvokeAdapterAttachesSpawnHeaders(t *testing.T) {
	var gotRoot, gotDepth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRoot = r.Header.Get(hdrSpawnRoot)
		gotDepth = r.Header.Get(hdrSpawnDepth)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	adapter := NewInvokeAdapter(InvokeAdapterConfig{HTTPClient: srv.Client()})

	_, _, err := adapter.Invoke(contextWithSpawnContext(context.Background(), "root-1", 2), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "root-1", gotRoot, "the adapter stamps the spawn-tree root")
	assert.Equal(t, "2", gotDepth, "the adapter stamps this run's spawn depth")

	gotRoot, gotDepth = "", ""
	_, _, err = adapter.Invoke(context.Background(), srv.URL, []byte(`{}`))
	require.NoError(t, err)
	assert.Empty(t, gotRoot, "no spawn context ⇒ no headers (a plain run)")
	assert.Empty(t, gotDepth)
}

// TestInvokeThreadsConversationID proves the caller-scoped handler carries a body-supplied
// conversationId onto the adapter's context (→ X-Conversation-Id), and rejects a malformed
// id with a 400 at the console boundary rather than forwarding a memory-key-breaking value.
func TestInvokeThreadsConversationID(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{}`)}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	reqBody, _ := json.Marshal(InvokeRequest{
		Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{"input":"hi"}`), ConversationID: "chat-42",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, inv.called)
	assert.Equal(t, "chat-42", conversationIDFromContext(inv.gotCtx), "the handler threads the id onto the adapter ctx")

	// A malformed id (a memory-key separator) is rejected at the boundary — the run never fires.
	bad := &fakeInvokeAdapter{traceID: "t", resp: []byte(`{}`)}
	s2 := newInvokeServer(t, newFakeFactory(c), bad)
	badBody, _ := json.Marshal(InvokeRequest{
		Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`), ConversationID: "bad:id",
	})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(badBody))
	req2.Header.Set("Authorization", "Bearer developer-persona-token")
	s2.Handler().ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusBadRequest, rec2.Code)
	assert.False(t, bad.called, "a malformed conversationId fails fast — the agent is never invoked")
}

// newDevInvokeServer builds the dev-mode substrate (ADR 0021): NO cluster
// (CallerClients nil), DevMode on, and a fixed devInvokeEndpoint — so POST /api/invoke
// targets the single local Compose agent directly through the given fake adapter.
func newDevInvokeServer(t *testing.T, endpoint string, inv InvokeAdapter) *Server {
	t.Helper()
	return NewServer(Options{
		Scheme:            testScheme(t),
		Auth:              AllowAll{},
		DevMode:           true,
		DevInvokeEndpoint: endpoint,
		Adapters:          Adapters{Invoke: inv},
		Version:           "dev",
		Log:               logr.Discard(),
	})
}

// TestDevInvokeRunsLocalAgentNoCluster proves the dev Playground run (ADR 0021): with
// no cluster, POST /api/invoke targets the fixed local endpoint directly — the agent/
// namespace in the request are ignored (one local agent) and only Input flows through.
func TestDevInvokeRunsLocalAgentNoCluster(t *testing.T) {
	inv := &fakeInvokeAdapter{traceID: "devtrace", resp: []byte(`{"answer":"MOCK_OK"}`)}
	s := newDevInvokeServer(t, "http://localhost:8080", inv)

	reqBody, _ := json.Marshal(InvokeRequest{
		Agent: "ignored-in-dev", Input: json.RawMessage(`{"prompt":"hi"}`),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	// AllowAll edge auth — dev mode is a single local developer, no token needed.
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body InvokeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "devtrace", body.TraceID)
	assert.Contains(t, body.Response, "MOCK_OK")
	// The run targeted the fixed LOCAL endpoint — no cluster resolution happened.
	assert.True(t, inv.called)
	assert.Equal(t, "http://localhost:8080", inv.gotEndpoint)
	assert.JSONEq(t, `{"prompt":"hi"}`, string(inv.gotBody))
}

// TestDevInvokeNon2xxIs502 proves the dev path degrades honestly: a non-2xx from the
// local agent is a 502 with the real reason + the traceId, never a fake success.
func TestDevInvokeNon2xxIs502(t *testing.T) {
	inv := &fakeInvokeAdapter{
		traceID: "devtrace",
		err:     &invokeError{status: http.StatusInternalServerError, body: []byte("boom")},
	}
	s := newDevInvokeServer(t, "http://localhost:8080", inv)

	reqBody, _ := json.Marshal(InvokeRequest{Input: json.RawMessage(`{}`)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	var body InvokeErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "devtrace", body.TraceID)
	assert.Contains(t, body.Error, "500")
}

// TestDevInvokeWithoutEndpointIs501 proves the honest degrade: DevMode with no local
// endpoint wired (and no cluster) serves 501, never a crash or a fabricated run.
func TestDevInvokeWithoutEndpointIs501(t *testing.T) {
	s := NewServer(Options{
		Scheme:   testScheme(t),
		Auth:     AllowAll{},
		DevMode:  true,
		Adapters: Adapters{Invoke: &fakeInvokeAdapter{}},
		Log:      logr.Discard(),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader([]byte(`{}`)))
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotImplemented, rec.Code)
}

// TestInvokeForbiddenIs403 proves a Forbidden on the caller-scoped agent lookup
// surfaces as 403 and the agent is NEVER invoked — a viewer with no invoke rights
// is denied by the API server (as the caller), not by an SA fallback.
func TestInvokeForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"},
					"echo", assert.AnError,
				)
			},
		}).
		Build()
	inv := &fakeInvokeAdapter{traceID: "unused"}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	reqBody, _ := json.Marshal(InvokeRequest{Agent: "echo", Namespace: "prod"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, inv.called, "a Forbidden lookup must NOT reach the agent")
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.NotEmpty(t, errBody.Error)
}

// TestInvokeWithoutTokenIs401 proves a token-less request is rejected with 401
// BEFORE any K8s call or invoke — an anonymous caller never runs an agent.
func TestInvokeWithoutTokenIs401(t *testing.T) {
	getCalled := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				getCalled = true
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	inv := &fakeInvokeAdapter{traceID: "unused"}
	s := newInvokeServer(t, &fakeCallerClientFactory{client: c, requireToken: true}, inv)

	reqBody, _ := json.Marshal(InvokeRequest{Agent: "echo", Namespace: "prod"})
	rec := httptest.NewRecorder()
	// No Authorization header.
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, getCalled, "no K8s read for a token-less request")
	assert.False(t, inv.called, "no invoke for a token-less request")
}

// TestInvokeNotReadyIs409 proves an agent with no assigned endpoint (not Ready)
// yields a 409 — never a fake success — and the agent is not dispatched.
func TestInvokeNotReadyIs409(t *testing.T) {
	agent := readyAgent("wip", "prod", "") // no status.url
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "unused"}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	reqBody, _ := json.Marshal(InvokeRequest{Agent: "wip", Namespace: "prod"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.False(t, inv.called)
}

// TestInvokeJobAgentIsAccurate409 proves a job agent (no serving endpoint by design)
// gets an ACCURATE conflict — not the misleading "not ready (no endpoint assigned
// yet)", which would imply waiting helps (m25 S4).
func TestInvokeJobAgentIsAccurate409(t *testing.T) {
	agent := readyAgent("batch", "jobs", "") // no status.url; distinct ns
	agent.Spec.ExecutionModel = "job"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{traceID: "unused"}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	reqBody, _ := json.Marshal(InvokeRequest{Agent: "batch", Namespace: "jobs"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "job agent", "a job agent gets an accurate message")
	assert.NotContains(t, rec.Body.String(), "no endpoint assigned yet")
	assert.False(t, inv.called)
}

// TestInvokeAgentNotFoundIs404 proves a missing agent yields 404.
func TestInvokeAgentNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	inv := &fakeInvokeAdapter{traceID: "unused"}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	reqBody, _ := json.Marshal(InvokeRequest{Agent: "ghost", Namespace: "prod"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.False(t, inv.called)
}

// TestInvokeUpstreamFailureIs502 proves an agent that answers non-2xx is surfaced
// as a 502 with the honest error AND the traceId (the failed run is still traced),
// never a swallowed 200.
func TestInvokeUpstreamFailureIs502(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &fakeInvokeAdapter{
		traceID: "failtrace",
		resp:    []byte("boom"),
		err:     &invokeError{status: http.StatusInternalServerError, body: []byte("boom")},
	}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	reqBody, _ := json.Marshal(InvokeRequest{Agent: "echo", Namespace: "prod"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	var body InvokeErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "failtrace", body.TraceID, "a failed run still hands back its traceId")
	assert.Contains(t, body.Error, "500")
}

// TestInvokeNilAdapterIs501 proves that when the InvokeAdapter is not wired the
// route serves an honest 501 (the seam is discoverable but not implemented).
func TestInvokeNilAdapterIs501(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	// Server with a caller factory but NO Invoke adapter.
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{}, // Invoke nil
		Version:       "test",
		Log:           logr.Discard(),
	})

	reqBody, _ := json.Marshal(InvokeRequest{Agent: "echo", Namespace: "prod"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/invoke", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotImplemented, rec.Code)
}

// --- the concrete HTTP invoke adapter (trace hand-off) -----------------------

// TestHTTPInvokeAdapterInjectsTraceparentAndReturnsTraceID proves the adapter
// POSTs /invoke to the resolved endpoint, injects a W3C traceparent carrying the
// minted trace id, and returns that SAME id — the launcher's span continues this
// trace, so the returned id IS the run's Langfuse trace id.
func TestHTTPInvokeAdapterInjectsTraceparentAndReturnsTraceID(t *testing.T) {
	var gotPath, gotTraceparent, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTraceparent = r.Header.Get("traceparent")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"answer":"MOCK_OK"}`))
	}))
	defer srv.Close()

	a := NewInvokeAdapter(InvokeAdapterConfig{HTTPClient: srv.Client()})
	resp, traceID, err := a.Invoke(context.Background(), srv.URL, []byte(`{"prompt":"hi"}`))
	require.NoError(t, err)

	assert.Equal(t, "/invoke", gotPath)
	assert.Equal(t, `{"prompt":"hi"}`, gotBody)
	assert.Contains(t, string(resp), "MOCK_OK")

	// traceparent is "00-<32hex>-<16hex>-01" and its trace-id segment equals the
	// returned traceID (the id fed to /api/traces/{id}).
	require.NotEmpty(t, traceID)
	assert.Len(t, traceID, 32, "trace id is 16 bytes / 32 hex chars")
	assert.Contains(t, gotTraceparent, traceID, "the returned traceId is what was injected as traceparent")
	assert.True(t, len(gotTraceparent) > 0 && gotTraceparent[:3] == "00-", "W3C traceparent version 00")
}

// TestHTTPInvokeStreamParsesTokenAndStepFrames proves InvokeStream forwards each SSE `token`
// frame to onToken and each `step` metadata frame's RAW JSON payload verbatim to onStep (M78,
// ADR 0071 §4 — live step-visibility), then returns the terminal `done` envelope. The adapter
// does not re-parse the step shape — the exact bytes the agent streamed reach onStep.
func TestHTTPInvokeStreamParsesTokenAndStepFrames(t *testing.T) {
	stepModel := `{"type":"step","step":1,"kind":"model","tokens":{"prompt":11,"completion":7},"ref":null}`
	stepTool := `{"type":"step","step":1,"kind":"tool","tool":"echo_tool","tokens":{"prompt":0,"completion":0},"ref":{"channel":"tool","index":0}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A model token, the two step frames, another token, then the terminal done envelope.
		_, _ = w.Write([]byte("data: {\"type\":\"token\",\"text\":\"Hel\"}\n\n"))
		_, _ = w.Write([]byte("data: " + stepModel + "\n\n"))
		_, _ = w.Write([]byte("data: " + stepTool + "\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"token\",\"text\":\"lo\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"output\":\"Hello\"}\n\n"))
	}))
	defer srv.Close()

	a := NewInvokeAdapter(InvokeAdapterConfig{HTTPClient: srv.Client()})
	sa, ok := a.(StreamingInvokeAdapter)
	require.True(t, ok, "the http invoke adapter implements StreamingInvokeAdapter")

	var tokens, steps []string
	final, traceID, err := sa.InvokeStream(context.Background(), srv.URL, []byte(`{"input":"hi"}`),
		func(text string) { tokens = append(tokens, text) },
		func(stepJSON string) { steps = append(steps, stepJSON) },
	)
	require.NoError(t, err)

	assert.Equal(t, []string{"Hel", "lo"}, tokens, "each token frame's text reached onToken")
	// onStep received each step frame's raw JSON payload VERBATIM (the BFF does not re-parse it).
	require.Equal(t, []string{stepModel, stepTool}, steps)
	assert.Contains(t, string(final), "Hello", "the terminal done envelope is returned")
	assert.NotEmpty(t, traceID)
}

// TestHTTPInvokeStreamToleratesNilOnStep proves a streaming caller that passes a nil onStep (a
// path that does not want step events) does not panic when the agent streams `step` frames.
func TestHTTPInvokeStreamToleratesNilOnStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"step\",\"step\":1,\"kind\":\"model\",\"ref\":null}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\",\"output\":\"ok\"}\n\n"))
	}))
	defer srv.Close()

	a := NewInvokeAdapter(InvokeAdapterConfig{HTTPClient: srv.Client()})
	sa := a.(StreamingInvokeAdapter)
	final, _, err := sa.InvokeStream(context.Background(), srv.URL, []byte(`{}`), nil, nil)
	require.NoError(t, err)
	assert.Contains(t, string(final), "ok")
}

// TestHTTPInvokeAdapterSurfacesNon2xx proves a non-2xx agent response is returned
// as an *invokeError (the handler maps it to a 502) with the traceId still set —
// a failed run is never a silent success.
func TestHTTPInvokeAdapterSurfacesNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("agent blew up"))
	}))
	defer srv.Close()

	a := NewInvokeAdapter(InvokeAdapterConfig{HTTPClient: srv.Client()})
	_, traceID, err := a.Invoke(context.Background(), srv.URL, []byte(`{}`))
	require.Error(t, err)
	var ie *invokeError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, http.StatusInternalServerError, ie.status)
	assert.NotEmpty(t, traceID, "a failed run still has a traceId")
}

// TestHTTPInvokeAdapterEmptyEndpoint proves an empty endpoint is a clear error,
// not a nil-deref or a request to a bad URL.
func TestHTTPInvokeAdapterEmptyEndpoint(t *testing.T) {
	a := NewInvokeAdapter(InvokeAdapterConfig{})
	_, _, err := a.Invoke(context.Background(), "  ", []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty agent endpoint")
}
