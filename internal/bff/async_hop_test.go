package bff

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/asyncbus"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentcapability"
	"github.com/ctxmesh/ctxmesh/internal/run"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// recordingBus captures what the publish edge hands to the async backend.
type recordingBus struct {
	mu       sync.Mutex
	messages []asyncbus.Message
	err      error
}

func (b *recordingBus) Publish(_ context.Context, msg asyncbus.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.messages = append(b.messages, msg)
	return nil
}

func (b *recordingBus) Close() error { return nil }

func (b *recordingBus) published() []asyncbus.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]asyncbus.Message(nil), b.messages...)
}

// newAsyncServer builds the publish edge + dispatcher over a seeded capability registry.
func newAsyncServer(
	t *testing.T, caller *run.Run, rows ...agentcapability.AgentCapability,
) (*Server, *runcap.Signer, *recordingBus) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := runcap.NewSigner(priv, spawnAud, nil)

	runs := run.NewMemStore()
	if caller != nil {
		require.NoError(t, runs.Create(caller))
	}
	caps := agentcapability.NewMemStore()
	for _, row := range rows {
		require.NoError(t, caps.Set(context.Background(), row))
	}
	bus := &recordingBus{}
	s := &Server{
		capabilitySigner:  signer,
		runStore:          runs,
		agentCapabilities: caps,
		asyncPublisher:    bus,
		log:               logr.Discard(),
	}
	return s, signer, bus
}

func postAsyncPublish(t *testing.T, s *Server, capToken string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/internal/async/publish", bytes.NewReader([]byte(body)))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func ceHeaders() map[string]string {
	return map[string]string{
		"Ce-Id":          "msg-1",
		"Ce-Type":        "reviewer", // the CloudEvent type IS the receiver agent
		"Ce-Source":      "planner",
		"Ce-Specversion": "1.0",
		"Content-Type":   "application/json",
	}
}

// A published hop is durably handed to the bus, routed by the SENDER's registry, with the CloudEvent
// moved through verbatim.
func TestAsyncPublish_EnqueuesOnTheSendersRegistrySubject(t *testing.T) {
	caller := mkParentRun("run-async-1")
	s, signer, bus := newAsyncServer(t, caller,
		cap0("team-ns", "supervisor", "reg-a", "Coordinates work."),
	)

	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), ceHeaders(), `{"messageId":"msg-1"}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	sent := bus.published()
	require.Len(t, sent, 1)
	assert.Equal(t, "msg-1", sent[0].ID, "the CloudEvent id is the idempotency key")
	assert.Equal(t, asyncbus.Subject("reg-a"), sent[0].Subject,
		"routed by the registry the CONTROL PLANE says the sender is in")
	assert.Equal(t, `{"messageId":"msg-1"}`, string(sent[0].Data), "the CloudEvent body is moved verbatim")
	assert.Equal(t, "reviewer", sent[0].Headers["Ce-Type"], "the binding headers survive the hop")
	assert.Equal(t, "team-ns", sent[0].Headers[headerAsyncNamespace])
	assert.Equal(t, "reg-a", sent[0].Headers[headerAsyncRegistry])
}

// THE SECURITY PROPERTY: a pod cannot choose its own routing context. Caller-supplied platform headers
// are discarded and re-stamped from the verified run, so it cannot publish into someone else's registry.
func TestAsyncPublish_CallerCannotForgeItsRegistryOrNamespace(t *testing.T) {
	caller := mkParentRun("run-async-2")
	s, signer, bus := newAsyncServer(t, caller,
		cap0("team-ns", "supervisor", "reg-a", "Coordinates work."),
	)

	headers := ceHeaders()
	headers[headerAsyncRegistry] = "reg-victim"
	headers[headerAsyncNamespace] = "victim-ns"

	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), headers, `{"messageId":"msg-1"}`)
	require.Equal(t, http.StatusAccepted, rec.Code)

	sent := bus.published()
	require.Len(t, sent, 1)
	assert.Equal(t, asyncbus.Subject("reg-a"), sent[0].Subject, "the forged registry is ignored")
	assert.Equal(t, "reg-a", sent[0].Headers[headerAsyncRegistry], "the platform re-stamps its own view")
	assert.Equal(t, "team-ns", sent[0].Headers[headerAsyncNamespace])
}

// The run capability must never ride onto the bus — it is a bearer credential, and the message outlives
// the request.
func TestAsyncPublish_DoesNotLeakTheRunCapabilityOntoTheBus(t *testing.T) {
	caller := mkParentRun("run-async-3")
	s, signer, bus := newAsyncServer(t, caller, cap0("team-ns", "supervisor", "reg-a", "Coordinates."))

	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), ceHeaders(), `{"m":1}`)
	require.Equal(t, http.StatusAccepted, rec.Code)

	for k := range bus.published()[0].Headers {
		assert.NotEqual(t, http.CanonicalHeaderKey(runcap.HeaderName), http.CanonicalHeaderKey(k),
			"a bearer credential must not be persisted into a durable message")
	}
}

// An agent in no registry has no async peers: refused, rather than published into a subject nobody
// serves — which would look like success and lose the hop.
func TestAsyncPublish_UnscopedAgentIsRefused(t *testing.T) {
	caller := mkParentRun("run-async-4")
	s, signer, bus := newAsyncServer(t, caller, cap0("team-ns", "supervisor", "", "Coordinates."))

	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), ceHeaders(), `{"m":1}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, bus.published())
}

// The edge is capability-authenticated and fail-closed, and it insists on the CloudEvent attributes the
// rest of the path depends on.
func TestAsyncPublish_RejectsBadRequests(t *testing.T) {
	caller := mkParentRun("run-async-5")
	s, signer, _ := newAsyncServer(t, caller, cap0("team-ns", "supervisor", "reg-a", "Coordinates."))
	tok := mintCap(t, signer, caller.ID)

	assert.Equal(t, http.StatusUnauthorized,
		postAsyncPublish(t, s, "", ceHeaders(), `{"m":1}`).Code, "no capability")

	noID := ceHeaders()
	delete(noID, "Ce-Id")
	assert.Equal(t, http.StatusBadRequest, postAsyncPublish(t, s, tok, noID, `{"m":1}`).Code,
		"no ce-id ⇒ no idempotency key")

	noType := ceHeaders()
	delete(noType, "Ce-Type")
	assert.Equal(t, http.StatusBadRequest, postAsyncPublish(t, s, tok, noType, `{"m":1}`).Code,
		"no ce-type ⇒ no receiver")

	assert.Equal(t, http.StatusBadRequest, postAsyncPublish(t, s, tok, ceHeaders(), "").Code,
		"an empty body carries no envelope")
}

// A backend that refuses is reported honestly — the producer must learn its hop was NOT durable.
func TestAsyncPublish_BackendFailureIsReported(t *testing.T) {
	caller := mkParentRun("run-async-6")
	s, signer, bus := newAsyncServer(t, caller, cap0("team-ns", "supervisor", "reg-a", "Coordinates."))
	bus.err = errors.New("broker unreachable")

	rec := postAsyncPublish(t, s, mintCap(t, signer, caller.ID), ceHeaders(), `{"m":1}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code,
		"a publish that did not durably land must not answer 202")
}

// ── dispatcher ────────────────────────────────────────────────────────────────

// The dispatcher delivers a hop to its target agent as a CloudEvent, unchanged — which is what keeps the
// launcher's existing consumer (dedupe + invoke) working across a backend swap.
func TestAsyncDispatch_DeliversTheCloudEventToTheTarget(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer agent.Close()

	s, _, _ := newAsyncServer(t, nil, cap0("team-ns", "reviewer", "reg-a", "Reviews things."))
	s.agentURL = func(string, string) string { return agent.URL }

	err := s.deliverAsyncHop(context.Background(), agent.Client(), asyncbus.Message{
		ID: "msg-1", Subject: asyncbus.Subject("reg-a"), Data: []byte(`{"messageId":"msg-1"}`),
		Headers: map[string]string{
			"Ce-Id": "msg-1", "Ce-Type": "reviewer", "Content-Type": "application/json",
			headerAsyncNamespace: "team-ns", headerAsyncRegistry: "reg-a",
		},
	})
	require.NoError(t, err, "a 2xx from the callee acks the message")

	assert.Equal(t, `{"messageId":"msg-1"}`, string(gotBody), "the CloudEvent body is delivered verbatim")
	assert.Equal(t, "reviewer", gotHeaders.Get("Ce-Type"), "the binding headers reach the agent")
	assert.Empty(t, gotHeaders.Get(headerAsyncNamespace),
		"the platform's routing headers are internal — the agent never sees them")
	assert.Empty(t, gotHeaders.Get(headerAsyncRegistry))
}

// THE SECOND SECURITY PROPERTY: the dispatcher refuses to cross a registry boundary. The callee's own
// layer-1 check reads the envelope's registryId — which the PRODUCER wrote — so a crafted envelope could
// otherwise claim a registry its sender is not in. Checking the target's REAL registry against the one
// the message was published into (stamped by the edge) closes that.
func TestAsyncDispatch_RefusesToCrossARegistryBoundary(t *testing.T) {
	delivered := false
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer agent.Close()

	// The target lives in reg-b; the message was published from reg-a.
	s, _, _ := newAsyncServer(t, nil, cap0("team-ns", "reviewer", "reg-b", "Reviews things."))
	s.agentURL = func(string, string) string { return agent.URL }

	err := s.deliverAsyncHop(context.Background(), agent.Client(), asyncbus.Message{
		ID: "msg-1", Data: []byte(`{}`),
		Headers: map[string]string{
			"Ce-Type": "reviewer", headerAsyncNamespace: "team-ns", headerAsyncRegistry: "reg-a",
		},
	})
	require.NoError(t, err, "un-deliverable forever ⇒ acked, not retried into the poison cap")
	assert.False(t, delivered, "a hop must never reach an agent outside the sender's registry")
}

// A callee that is briefly unavailable NACKs (retry); a callee that REFUSES acks (its mind will not
// change). Getting this backwards either drops recoverable work or burns the delivery budget on a no.
func TestAsyncDispatch_RetriesServerErrorsButNotRefusals(t *testing.T) {
	status := http.StatusServiceUnavailable
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	defer agent.Close()

	s, _, _ := newAsyncServer(t, nil, cap0("team-ns", "reviewer", "reg-a", "Reviews."))
	s.agentURL = func(string, string) string { return agent.URL }

	msg := asyncbus.Message{
		ID: "msg-1", Data: []byte(`{}`),
		Headers: map[string]string{
			"Ce-Type": "reviewer", headerAsyncNamespace: "team-ns", headerAsyncRegistry: "reg-a",
		},
	}

	require.Error(t, s.deliverAsyncHop(context.Background(), agent.Client(), msg),
		"a 5xx is transient — nack so the backoff redelivers it")

	status = http.StatusForbidden
	require.NoError(t, s.deliverAsyncHop(context.Background(), agent.Client(), msg),
		"a 4xx is the callee's own refusal — ack rather than retry a settled no")
}

// A message with no routing context can never be delivered, so it is acked with a reason rather than
// redelivered until the cap.
func TestAsyncDispatch_UnroutableMessageIsAckedNotRetried(t *testing.T) {
	s, _, _ := newAsyncServer(t, nil)
	require.NoError(t, s.deliverAsyncHop(context.Background(), http.DefaultClient, asyncbus.Message{
		ID: "msg-1", Data: []byte(`{}`), Headers: map[string]string{headerAsyncNamespace: "team-ns"},
	}), "no receiver ⇒ acked")
}
