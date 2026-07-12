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

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
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
	called      bool
}

func (f *fakeInvokeAdapter) Invoke(_ context.Context, endpoint string, body []byte) ([]byte, string, error) {
	f.called = true
	f.gotEndpoint = endpoint
	f.gotBody = body
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
