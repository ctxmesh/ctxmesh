package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// asyncEdge stands in for the platform's async publish edge, capturing what the launcher publishes.
type asyncEdge struct {
	body    []byte
	headers http.Header
	status  int
	srv     *httptest.Server
}

func newAsyncEdge(t *testing.T, status int) *asyncEdge {
	t.Helper()
	e := &asyncEdge{status: status}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.body, _ = io.ReadAll(r.Body)
		e.headers = r.Header.Clone()
		w.WriteHeader(e.status)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// newAsyncA2A builds an A2A server whose async mode targets the fake edge.
func newAsyncA2A(t *testing.T, edge *asyncEdge) *a2aServer {
	t.Helper()
	cfg := baseCfg()
	if edge != nil {
		cfg.AsyncPublishURL = edge.srv.URL
	}
	return newA2AServer(cfg, noop.NewTracerProvider().Tracer("x"), propagation.TraceContext{}, nil)
}

// asyncTarget is the peer every case in this file hops to — one name keeps the assertions about the
// async MODE rather than about routing, which a2a_test.go already covers.
const asyncTarget = "reviewer"

// asyncCapToken is the invoking user's run capability, relayed on every hop — the edge derives the
// sender's registry from the run it names, so a call without one has no routing context at all.
const asyncCapToken = "cap-token"

func postA2A(t *testing.T, s *a2aServer, query, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/a2a/"+asyncTarget+query, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(runcap.HeaderName, asyncCapToken)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

// mode=async publishes the SAME envelope the synchronous path would send — as a CloudEvent, to the
// platform's publish edge, with the run capability relayed so the edge can derive the sender's registry.
func TestA2AAsync_PublishesTheEnvelopeAsACloudEvent(t *testing.T) {
	edge := newAsyncEdge(t, http.StatusAccepted)
	s := newAsyncA2A(t, edge)

	rec := postA2A(t, s, "?mode=async", `{"task":"review this"}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// The caller gets "durably accepted" + the messageId — never an answer: the callee may not even be
	// running yet, which is the whole point of the async hop.
	var out struct {
		Accepted  bool   `json:"accepted"`
		MessageID string `json:"messageId"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.True(t, out.Accepted)
	assert.NotEmpty(t, out.MessageID)

	assert.Equal(t, "reviewer", edge.headers.Get("Ce-Type"), "the CloudEvent type names the receiver")
	assert.Equal(t, out.MessageID, edge.headers.Get("Ce-Id"), "ce-id is the envelope's messageId")
	assert.Equal(t, "orchestrator", edge.headers.Get("Ce-Source"))
	assert.Equal(t, asyncCapToken, edge.headers.Get(runcap.HeaderName),
		"the run capability is relayed so the edge can derive the sender's registry from the control plane")

	var env envelope
	require.NoError(t, json.Unmarshal(edge.body, &env))
	assert.Equal(t, "research-team", env.RegistryID)
	assert.Equal(t, "orchestrator", env.SenderAgentID)
	assert.Equal(t, "reviewer", env.ReceiverAgentID)
	assert.Equal(t, 1, env.Depth, "a first hop, exactly as the synchronous path builds it")
}

// THE PROPERTY THAT MATTERS: async is not a way around the conversation guards. A hop that the
// synchronous path refuses must be refused here too — otherwise `?mode=async` would launder it.
func TestA2AAsync_IsSubjectToTheSameConversationGuards(t *testing.T) {
	edge := newAsyncEdge(t, http.StatusAccepted)
	s := newAsyncA2A(t, edge)

	// An incoming envelope already at the depth limit: the next hop trips maxDepth.
	deep := envelope{
		RegistryID: "research-team", ConversationID: "c1", MessageID: "m1",
		SenderAgentID: "someone", ReceiverAgentID: "orchestrator", Role: "worker",
		Depth: defaultMaxDepth, Path: []string{"someone"}, BudgetRemaining: 5,
	}
	raw, err := json.Marshal(deep)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/a2a/reviewer?mode=async", strings.NewReader(`{"t":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(a2aEnvelopeHeader, string(raw))
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, "the depth guard refuses the async hop too")
	assert.Nil(t, edge.body, "nothing is published when a guard trips")
}

// Without an async backend configured the mode says so honestly rather than silently falling back to a
// synchronous call the caller did not ask for.
func TestA2AAsync_UnconfiguredIs501(t *testing.T) {
	s := newAsyncA2A(t, nil)
	rec := postA2A(t, s, "?mode=async", `{"t":1}`)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// A publish the edge refuses is reported as a failure — an agent that believes it dispatched work when
// it did not is the worst outcome available here.
func TestA2AAsync_PublishFailureIsSurfaced(t *testing.T) {
	edge := newAsyncEdge(t, http.StatusServiceUnavailable)
	s := newAsyncA2A(t, edge)

	rec := postA2A(t, s, "?mode=async", `{"t":1}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code, "a failed publish must never look like acceptance")
}

// The default path is untouched: no mode ⇒ the synchronous forward, unchanged.
func TestA2AAsync_DefaultModeStaysSynchronous(t *testing.T) {
	edge := newAsyncEdge(t, http.StatusAccepted)
	s, _, _, peerCh := newTestA2AServer(t, baseCfg(), http.StatusOK, `{"ok":true}`)
	s.cfg.AsyncPublishURL = edge.srv.URL

	rec := postA2A(t, s, "", `{"t":1}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Nil(t, edge.body, "a call with no mode never touches the async edge")
	select {
	case <-peerCh:
	default:
		t.Fatal("the synchronous path should have forwarded to the peer")
	}
}
