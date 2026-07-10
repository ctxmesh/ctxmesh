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

package main

// Unit tests for the async A2A path (cloudevent.go + async.go). Two surfaces:
//
//  1. envelope↔CloudEvent round-trip (the M6 envelope carried as a CloudEvent):
//     id=messageId, type=receiverAgentId, source=senderAgentId, data=envelope
//     JSON — a producer's encode and a consumer's decode reconstruct the exact
//     envelope, with the routing attributes mirroring the fields the m7.5 Trigger
//     filters on.
//
//  2. the consumer dedupe (messageId idempotency, M11 fail-closed): first-seen
//     invokes the agent; a duplicate within the TTL is acked WITHOUT re-invoking;
//     an unreachable seen-set FAILS CLOSED (NACK — not processed, broker retries);
//     a transient error followed by a successful MarkSeen → the message is
//     processed exactly once (no double-process).
//
// Run with -race. No real Valkey / broker: the SeenSet and the invoker are stubs.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"go.opentelemetry.io/otel/trace/noop"
)

// sampleEnvelope builds a representative platform envelope with a nested opaque
// payload for the round-trip / consume tests.
func sampleEnvelope(msgID, receiver string) envelope {
	return envelope{
		TraceID:         "0123456789abcdef0123456789abcdef",
		RegistryID:      "research-team",
		ConversationID:  "conv-42",
		MessageID:       msgID,
		SenderAgentID:   "orchestrator",
		ReceiverAgentID: receiver,
		Role:            roleOrchestrator,
		Depth:           2,
		Path:            []string{"root", "orchestrator"},
		BudgetRemaining: 30,
		Payload:         json.RawMessage(`{"task":"summarize","doc":"x"}`),
	}
}

// ── CloudEvent round-trip ─────────────────────────────────────────────────────

func TestEnvelopeCloudEventRoundTrip(t *testing.T) {
	t.Parallel()
	in := sampleEnvelope("msg-abc-123", "worker-agent")

	evt, err := envelopeToCloudEvent(in)
	if err != nil {
		t.Fatalf("envelopeToCloudEvent: %v", err)
	}

	// Routing attributes mirror the envelope (specs/eventing-scaling.md §12.6).
	if got := evt.ID(); got != in.MessageID {
		t.Errorf("CloudEvent id = %q, want messageId %q", got, in.MessageID)
	}
	if got := evt.Type(); got != in.ReceiverAgentID {
		t.Errorf("CloudEvent type = %q, want receiverAgentId %q (m7.5 Trigger filters on type)", got, in.ReceiverAgentID)
	}
	if got := evt.Source(); got != in.SenderAgentID {
		t.Errorf("CloudEvent source = %q, want senderAgentId %q", got, in.SenderAgentID)
	}
	if got, ok := evt.Extensions()[ceExtensionRegistryID]; !ok || got != in.RegistryID {
		t.Errorf("CloudEvent %s extension = %v, want registryId %q", ceExtensionRegistryID, got, in.RegistryID)
	}
	if ct := evt.DataContentType(); ct != cloudevents.ApplicationJSON {
		t.Errorf("CloudEvent datacontenttype = %q, want %q", ct, cloudevents.ApplicationJSON)
	}

	// Decode reconstructs the exact envelope, payload byte-for-byte.
	out, err := cloudEventToEnvelope(evt)
	if err != nil {
		t.Fatalf("cloudEventToEnvelope: %v", err)
	}
	assertEnvelopeEqual(t, in, out)
}

// TestCloudEventRoundTrip_OverHTTP proves the mapping survives the CloudEvents
// HTTP binding both ways: publishEnvelope writes a real HTTP request (binary
// content mode); NewEventFromHTTPRequest parses it back; the envelope decodes
// identically. This is the exact wire path a producer→broker→consumer takes.
func TestCloudEventRoundTrip_OverHTTP(t *testing.T) {
	t.Parallel()
	in := sampleEnvelope("msg-http-1", "worker-agent")

	// Capture what the "broker" receives.
	var gotEvent *cloudevents.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, err := cehttp.NewEventFromHTTPRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotEvent = e
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	if err := publishEnvelope(context.Background(), srv.Client(), srv.URL, in, nil); err != nil {
		t.Fatalf("publishEnvelope: %v", err)
	}
	if gotEvent == nil {
		t.Fatal("broker did not receive a CloudEvent")
	}
	if gotEvent.ID() != in.MessageID || gotEvent.Type() != in.ReceiverAgentID {
		t.Errorf("received event id/type = %q/%q, want %q/%q",
			gotEvent.ID(), gotEvent.Type(), in.MessageID, in.ReceiverAgentID)
	}
	out, err := cloudEventToEnvelope(*gotEvent)
	if err != nil {
		t.Fatalf("cloudEventToEnvelope after HTTP: %v", err)
	}
	assertEnvelopeEqual(t, in, out)
}

func TestCloudEventToEnvelope_Errors(t *testing.T) {
	t.Parallel()

	t.Run("no data", func(t *testing.T) {
		t.Parallel()
		evt := cloudevents.NewEvent()
		evt.SetID("id-1")
		evt.SetType("worker")
		evt.SetSource("sender")
		if _, err := cloudEventToEnvelope(evt); err == nil {
			t.Fatal("expected error for CloudEvent with no data")
		}
	})

	t.Run("non-JSON data", func(t *testing.T) {
		t.Parallel()
		evt := cloudevents.NewEvent()
		evt.SetID("id-2")
		evt.SetType("worker")
		evt.SetSource("sender")
		_ = evt.SetData("text/plain", []byte("not json"))
		if _, err := cloudEventToEnvelope(evt); err == nil {
			t.Fatal("expected error for non-JSON CloudEvent data")
		}
	})

	t.Run("messageId falls back to CloudEvent id", func(t *testing.T) {
		t.Parallel()
		// An envelope whose own messageId is empty must inherit the CE id so it
		// still has a dedupe key (they are equal by construction).
		env := sampleEnvelope("", "worker")
		evt := cloudevents.NewEvent()
		evt.SetID("ce-fallback-id")
		evt.SetType("worker")
		evt.SetSource("sender")
		data, _ := json.Marshal(env)
		_ = evt.SetData(cloudevents.ApplicationJSON, json.RawMessage(data))
		out, err := cloudEventToEnvelope(evt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.MessageID != "ce-fallback-id" {
			t.Errorf("messageId = %q, want fallback to CE id %q", out.MessageID, "ce-fallback-id")
		}
	})
}

// ── dedupe / consume ──────────────────────────────────────────────────────────

// fakeSeenSet is an in-memory SeenSet stub. When failWith is set, every MarkSeen
// returns it (to exercise fail-closed NACK behaviour); otherwise it tracks seen
// ids like Valkey SetNX (first sighting → true, subsequent → false). failAfter,
// when positive, injects failWith for that many calls and then clears it so
// subsequent calls succeed — used to simulate a transient store error.
type fakeSeenSet struct {
	mu        sync.Mutex
	seen      map[string]bool
	failWith  error
	failAfter int // if > 0: fail this many calls, then clear failWith
	calls     atomic.Int32
}

func newFakeSeenSet() *fakeSeenSet { return &fakeSeenSet{seen: map[string]bool{}} }

func (f *fakeSeenSet) MarkSeen(_ context.Context, id string, _ time.Duration) (bool, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		err := f.failWith
		if f.failAfter > 0 {
			f.failAfter--
			if f.failAfter == 0 {
				f.failWith = nil // transient: clear after exhausting the fail budget
			}
		}
		return false, err
	}
	if f.seen[id] {
		return false, nil // duplicate
	}
	f.seen[id] = true
	return true, nil // first-seen
}

// newTestConsumer builds an asyncConsumer with a counting invoker. invokeCount
// records how many times the agent was actually invoked.
func newTestConsumer(seen SeenSet, invokeStatus int, invokeErr error) (*asyncConsumer, *int32) {
	var invokeCount int32
	c := &asyncConsumer{
		cfg:    asyncConfig{SelfName: "worker-agent"},
		seen:   seen,
		tracer: noop.NewTracerProvider().Tracer("test"),
		invoke: func(_ context.Context, _ envelope) (int, error) {
			atomic.AddInt32(&invokeCount, 1)
			return invokeStatus, invokeErr
		},
	}
	return c, &invokeCount
}

// cloudEventRequest builds an HTTP request carrying env as a CloudEvent (binary
// content mode), as a Trigger would deliver it.
func cloudEventRequest(t *testing.T, env envelope) *http.Request {
	t.Helper()
	evt, err := envelopeToCloudEvent(env)
	if err != nil {
		t.Fatalf("envelopeToCloudEvent: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if err := cehttp.WriteRequest(context.Background(), cloudevents.ToMessage(&evt), req); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	return req
}

func TestConsume_FirstSeen_Invokes(t *testing.T) {
	t.Parallel()
	c, invokeCount := newTestConsumer(newFakeSeenSet(), http.StatusOK, nil)

	rr := httptest.NewRecorder()
	c.consume(rr, cloudEventRequest(t, sampleEnvelope("msg-first", "worker-agent")))

	if rr.Code != http.StatusNoContent {
		t.Errorf("first-seen: status = %d, want 204", rr.Code)
	}
	if got := atomic.LoadInt32(invokeCount); got != 1 {
		t.Errorf("first-seen: agent invoked %d times, want 1", got)
	}
}

func TestConsume_Duplicate_AckedNotReinvoked(t *testing.T) {
	t.Parallel()
	seen := newFakeSeenSet()
	c, invokeCount := newTestConsumer(seen, http.StatusOK, nil)
	env := sampleEnvelope("msg-dup", "worker-agent")

	// First delivery: invoked.
	rr1 := httptest.NewRecorder()
	c.consume(rr1, cloudEventRequest(t, env))
	if rr1.Code != http.StatusNoContent {
		t.Fatalf("first delivery status = %d, want 204", rr1.Code)
	}

	// Redelivery of the SAME messageId within the TTL: acked, NOT re-invoked.
	rr2 := httptest.NewRecorder()
	c.consume(rr2, cloudEventRequest(t, env))
	if rr2.Code != http.StatusNoContent {
		t.Errorf("duplicate: status = %d, want 204 (acked)", rr2.Code)
	}
	if got := atomic.LoadInt32(invokeCount); got != 1 {
		t.Errorf("duplicate: agent invoked %d times, want 1 (dedup must skip re-invoke)", got)
	}
}

func TestConsume_FailClosed_WhenSeenSetErrors(t *testing.T) {
	t.Parallel()
	// A seen-set that always errors (Valkey persistently unreachable): the
	// consumer must NACK (503) and NOT invoke the agent — fail-closed (M11).
	// The broker retries; after the retry budget it DLQs (M7 machinery).
	seen := &fakeSeenSet{seen: map[string]bool{}, failWith: errors.New("valkey down")}
	c, invokeCount := newTestConsumer(seen, http.StatusOK, nil)

	env := sampleEnvelope("msg-failclosed", "worker-agent")
	for i := range 2 {
		rr := httptest.NewRecorder()
		c.consume(rr, cloudEventRequest(t, env))
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("fail-closed delivery %d: status = %d, want 503 (NACK)", i, rr.Code)
		}
	}
	if got := atomic.LoadInt32(invokeCount); got != 0 {
		t.Errorf("fail-closed: agent invoked %d times, want 0 (must NOT process on store error)", got)
	}
}

func TestConsume_TransientSeenSetError_ExactlyOnce(t *testing.T) {
	t.Parallel()
	// Simulates a transient dedupe-store blip: the first delivery fails (NACK),
	// the broker retries, and the second delivery succeeds → agent is invoked
	// exactly once (no double-process). This is the key fail-closed invariant:
	// transient blip → broker retry → dedupe succeeds → exactly-once.
	seen := &fakeSeenSet{
		seen:      map[string]bool{},
		failWith:  errors.New("valkey transient"),
		failAfter: 1, // fail once, then succeed
	}
	c, invokeCount := newTestConsumer(seen, http.StatusOK, nil)
	env := sampleEnvelope("msg-transient", "worker-agent")

	// First attempt: store errors → NACK (fail-closed, no invoke).
	rr1 := httptest.NewRecorder()
	c.consume(rr1, cloudEventRequest(t, env))
	if rr1.Code != http.StatusServiceUnavailable {
		t.Errorf("first (transient error) delivery: status = %d, want 503 (NACK)", rr1.Code)
	}
	if got := atomic.LoadInt32(invokeCount); got != 0 {
		t.Errorf("after transient error: agent invoked %d times, want 0", got)
	}

	// Second attempt (broker retry): store is healthy → first-seen → invoke.
	rr2 := httptest.NewRecorder()
	c.consume(rr2, cloudEventRequest(t, env))
	if rr2.Code != http.StatusNoContent {
		t.Errorf("second (retry) delivery: status = %d, want 204 (acked)", rr2.Code)
	}
	if got := atomic.LoadInt32(invokeCount); got != 1 {
		t.Errorf("after retry: agent invoked %d times, want exactly 1", got)
	}

	// Third attempt (another broker retry of the same message id): store is
	// healthy, id is now seen → duplicate → acked, NOT re-invoked.
	rr3 := httptest.NewRecorder()
	c.consume(rr3, cloudEventRequest(t, env))
	if rr3.Code != http.StatusNoContent {
		t.Errorf("third (duplicate) delivery: status = %d, want 204 (acked, no re-invoke)", rr3.Code)
	}
	if got := atomic.LoadInt32(invokeCount); got != 1 {
		t.Errorf("after duplicate: agent invoked %d times, want still 1 (no double-process)", got)
	}
}

func TestConsume_NilSeenSet_DedupeDisabled(t *testing.T) {
	t.Parallel()
	// No seen-set (MEMORY_BACKEND_ADDR absent): dedupe disabled ⇒ always
	// first-seen (fail-open by construction).
	c, invokeCount := newTestConsumer(nil, http.StatusOK, nil)
	env := sampleEnvelope("msg-nostore", "worker-agent")
	for i := range 2 {
		rr := httptest.NewRecorder()
		c.consume(rr, cloudEventRequest(t, env))
		if rr.Code != http.StatusNoContent {
			t.Errorf("nil-seenset delivery %d: status = %d, want 204", i, rr.Code)
		}
	}
	if got := atomic.LoadInt32(invokeCount); got != 2 {
		t.Errorf("nil seen-set: agent invoked %d times, want 2 (no dedupe store)", got)
	}
}

func TestConsume_AgentUnreachable_Nacks(t *testing.T) {
	t.Parallel()
	// The agent invoke errors (agent down): NACK (502) so the broker retries/DLQs.
	c, _ := newTestConsumer(newFakeSeenSet(), 0, errors.New("connection refused"))
	rr := httptest.NewRecorder()
	c.consume(rr, cloudEventRequest(t, sampleEnvelope("msg-nack", "worker-agent")))
	if rr.Code != http.StatusBadGateway {
		t.Errorf("agent unreachable: status = %d, want 502 (NACK → retry/DLQ)", rr.Code)
	}
}

func TestConsume_AgentPoison_Nacks(t *testing.T) {
	t.Parallel()
	// The agent processes but returns 5xx (a poison message): NACK so the broker
	// retries and eventually DLQs it (the 🧪). Not acked.
	c, _ := newTestConsumer(newFakeSeenSet(), http.StatusInternalServerError, nil)
	rr := httptest.NewRecorder()
	c.consume(rr, cloudEventRequest(t, sampleEnvelope("msg-poison", "worker-agent")))
	if rr.Code != http.StatusBadGateway {
		t.Errorf("poison message: status = %d, want 502 (NACK → retry/DLQ)", rr.Code)
	}
}

func TestConsume_BadCloudEvent_400(t *testing.T) {
	t.Parallel()
	c, invokeCount := newTestConsumer(newFakeSeenSet(), http.StatusOK, nil)
	// A POST with a ce-id header but a body that is not a valid envelope: the
	// event decodes but the envelope does not — a 400 the broker should not retry
	// forever (it DLQs a persistently malformed event).
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	evt := cloudevents.NewEvent()
	evt.SetID("bad-1")
	evt.SetType("worker-agent")
	evt.SetSource("sender")
	_ = evt.SetData(cloudevents.ApplicationJSON, json.RawMessage(`"not an envelope object"`))
	if err := cehttp.WriteRequest(context.Background(), cloudevents.ToMessage(&evt), req); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	rr := httptest.NewRecorder()
	c.consume(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad envelope: status = %d, want 400", rr.Code)
	}
	if got := atomic.LoadInt32(invokeCount); got != 0 {
		t.Errorf("bad envelope: agent invoked %d times, want 0", got)
	}
}

// ── real redisSeenSet (miniredis) ─────────────────────────────────────────────

// TestRedisSeenSet_SetNXSemantics exercises the production redisSeenSet against
// a real (mini) Valkey: the first MarkSeen of a messageId is first-seen (true),
// a second is a duplicate (false), the key carries the dedupe TTL, and once it
// expires the id is first-seen again (the dedupe window is bounded).
func TestRedisSeenSet_SetNXSemantics(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	s := newRedisSeenSet(mr.Addr())
	ctx := context.Background()

	first, err := s.MarkSeen(ctx, "id-1", dedupeTTL)
	if err != nil {
		t.Fatalf("first MarkSeen: %v", err)
	}
	if !first {
		t.Error("first sighting must be first-seen (true)")
	}

	dup, err := s.MarkSeen(ctx, "id-1", dedupeTTL)
	if err != nil {
		t.Fatalf("second MarkSeen: %v", err)
	}
	if dup {
		t.Error("second sighting must be a duplicate (false)")
	}

	// The key carries a TTL near dedupeTTL (bounded dedupe window).
	ttl := mr.TTL(dedupeKeyPrefix + "id-1")
	if ttl <= 0 || ttl > dedupeTTL {
		t.Errorf("dedupe key TTL = %v, want in (0, %v]", ttl, dedupeTTL)
	}

	// Advance past the TTL: the id is first-seen again (window expired).
	mr.FastForward(dedupeTTL + time.Second)
	reSeen, err := s.MarkSeen(ctx, "id-1", dedupeTTL)
	if err != nil {
		t.Fatalf("MarkSeen after expiry: %v", err)
	}
	if !reSeen {
		t.Error("after the TTL expires the id must be first-seen again")
	}
}

// TestRedisSeenSet_BackendDown_Errors confirms an unreachable Valkey surfaces an
// error from MarkSeen (which the consumer then fails CLOSED on — covered by
// TestConsume_FailClosed_WhenSeenSetErrors with the stub; here we prove the real
// store reports the error rather than hanging).
func TestRedisSeenSet_BackendDown_Errors(t *testing.T) {
	t.Parallel()
	// A closed miniredis address: dial fails fast (bounded by dedupeOpTimeout).
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()

	s := newRedisSeenSet(addr)
	ctx, cancel := context.WithTimeout(context.Background(), dedupeOpTimeout+time.Second)
	defer cancel()
	if _, err := s.MarkSeen(ctx, "id-x", dedupeTTL); err == nil {
		t.Fatal("expected an error from an unreachable Valkey")
	}
}

// ── request recognition ───────────────────────────────────────────────────────

func TestIsCloudEventRequest(t *testing.T) {
	t.Parallel()

	binary := cloudEventRequest(t, sampleEnvelope("m", "worker"))
	if !isCloudEventRequest(binary) {
		t.Error("binary-mode CloudEvent (Ce-Id header) must be recognised")
	}

	structured := httptest.NewRequest(http.MethodPost, "/", nil)
	structured.Header.Set("Content-Type", "application/cloudevents+json; charset=utf-8")
	if !isCloudEventRequest(structured) {
		t.Error("structured-mode CloudEvent (application/cloudevents+json) must be recognised")
	}

	plain := httptest.NewRequest(http.MethodPost, "/invoke", nil)
	plain.Header.Set("Content-Type", "application/json")
	if isCloudEventRequest(plain) {
		t.Error("an ordinary /invoke JSON POST must NOT be treated as a CloudEvent")
	}

	getReq := httptest.NewRequest(http.MethodGet, "/", nil)
	getReq.Header.Set("Ce-Id", "x")
	if isCloudEventRequest(getReq) {
		t.Error("a non-POST must not be treated as a CloudEvent delivery")
	}
}

// assertEnvelopeEqual compares two envelopes field-by-field (payload compared as
// canonicalised JSON so formatting differences do not cause spurious failures).
func assertEnvelopeEqual(t *testing.T, want, got envelope) {
	t.Helper()
	if got.TraceID != want.TraceID ||
		got.RegistryID != want.RegistryID ||
		got.ConversationID != want.ConversationID ||
		got.MessageID != want.MessageID ||
		got.SenderAgentID != want.SenderAgentID ||
		got.ReceiverAgentID != want.ReceiverAgentID ||
		got.Role != want.Role ||
		got.Depth != want.Depth ||
		got.BudgetRemaining != want.BudgetRemaining {
		t.Errorf("envelope scalar fields differ:\n want %+v\n  got %+v", want, got)
	}
	if len(got.Path) != len(want.Path) {
		t.Fatalf("path length differs: want %v, got %v", want.Path, got.Path)
	}
	for i := range want.Path {
		if got.Path[i] != want.Path[i] {
			t.Errorf("path[%d] = %q, want %q", i, got.Path[i], want.Path[i])
		}
	}
	if !jsonEqual(t, want.Payload, got.Payload) {
		t.Errorf("payload differs: want %s, got %s", want.Payload, got.Payload)
	}
}

// jsonEqual reports whether two JSON blobs are semantically equal.
func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(ab) == string(bb)
}
