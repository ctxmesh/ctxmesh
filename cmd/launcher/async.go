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

// The async A2A consumer + publisher (M7, specs/eventing-scaling.md §"Async A2A
// envelope", §"Idempotency (launcher consumer)"). An eventing-model agent's
// Knative Trigger (m7.5) delivers a CloudEvent to the agent's ksvc; the launcher
// fronting the ksvc:
//
//  1. recognises a CloudEvent inbound (a POST carrying the CloudEvent HTTP
//     binding headers, structured or binary content mode);
//  2. decodes the platform envelope from it (cloudevent.go);
//  3. dedupes on the envelope's messageId against a short-TTL Valkey seen-set —
//     a redelivery of a messageId already seen inside the window is ACKED
//     without re-invoking the agent (at-least-once + idempotency, §12.6);
//  4. invokes the agent (the user container) with the envelope's payload;
//  5. emits an a2a.async.consume span recording the dedupe hit/miss.
//
// FAIL-CLOSED (M11, resolves M7 deferral — specs/trace-governance-security.md
// §"Eventing dedupe fail-open → fail-closed"): if the dedupe store is
// unreachable or the per-op timeout fires, the message is NOT processed — the
// consumer NACKs (non-2xx) so the broker retries. A transient Valkey blip means
// the retry's dedupe check succeeds and the message processes exactly once; a
// persistent outage lets the broker's bounded retry schedule exhaust and land the
// message in the per-registry DLQ (M7 machinery, no new loop here). This
// prevents a dedupe-store blip from causing double-processing, which is the M11
// security posture: dedupe uncertainty → reject/retry, not process.
//
// The publisher (publishEnvelope) is the producer side: it encodes an envelope
// as a CloudEvent and POSTs it to the registry broker — enough for the e2e (and
// a future producer example, m7.7) to emit an async A2A event.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

const (
	// dedupeTTL is the lifetime of a messageId in the seen-set. It bounds the
	// window in which a redelivery is recognised as a duplicate: long enough to
	// cover a broker's retry/backoff schedule (retry×exponential backoff) and a
	// pod roll within it (the set is in Valkey, so it survives a consumer
	// restart), short enough that the set does not grow unbounded. Revisit with
	// the M11 retention work (specs/eventing-scaling.md §"Open questions":
	// dedupe-window TTL tuning per registry).
	dedupeTTL = 10 * time.Minute

	// dedupeOpTimeout bounds a single seen-set round-trip. A slow/hung Valkey
	// must never wedge the consume path beyond this — on timeout we fail CLOSED
	// (NACK) rather than block, consistent with the M11 fail-closed posture.
	dedupeOpTimeout = 2 * time.Second

	// dedupeKeyPrefix namespaces the seen-set keys so they never collide with the
	// M5 memory keys (mem:...) in a shared Valkey. One key per messageId.
	dedupeKeyPrefix = "a2a:seen:"

	// maxAsyncBody caps the inbound CloudEvent body (and the agent's response we
	// relay), matching the sync A2A / memory 1MiB bound. Oversize async payloads
	// are the blob-offload concern (m7.6b), not this path.
	maxAsyncBody = 1 << 20
)

// SeenSet is the minimal dedupe-store surface the async consumer needs. It is an
// interface (not *redis.Client) so unit tests drive the consumer against a fake
// — including one that ERRORS, to prove the fail-open behaviour — without a real
// Valkey.
type SeenSet interface {
	// MarkSeen atomically records messageID as seen with the given TTL and
	// reports whether this was the FIRST sighting: true ⇒ first-seen (process),
	// false ⇒ already seen within the window (duplicate — ack without
	// re-invoking). An error means the store is unreachable; the caller MUST
	// fail closed (NACK — do not process, let the broker retry).
	MarkSeen(ctx context.Context, messageID string, ttl time.Duration) (bool, error)
}

// redisSeenSet is the production SeenSet backed by the same go-redis client the
// M5 memory endpoint uses (MEMORY_BACKEND_ADDR / the injected Valkey). SetNX is
// a single atomic op: it sets the key iff absent and returns whether it did, so
// first-seen vs duplicate is one round-trip with no read-modify-write race.
type redisSeenSet struct {
	rdb *redis.Client
}

func newRedisSeenSet(addr string) *redisSeenSet {
	return &redisSeenSet{
		rdb: redis.NewClient(&redis.Options{
			Addr:         addr,
			DialTimeout:  dedupeOpTimeout,
			ReadTimeout:  dedupeOpTimeout,
			WriteTimeout: dedupeOpTimeout,
		}),
	}
}

func (s *redisSeenSet) MarkSeen(ctx context.Context, messageID string, ttl time.Duration) (bool, error) {
	// SetNX: set key=1 iff absent, with a TTL, atomically. Result() is true when
	// the key was newly set (first-seen), false when it already existed
	// (duplicate). A single op — no race between a GET and a SET.
	return s.rdb.SetNX(ctx, dedupeKeyPrefix+messageID, 1, ttl).Result()
}

// asyncConfig is the async-consumer configuration. The consumer is enabled iff
// the agent is BOTH a registry member (A2A_PORT / registry env present) and has
// a Valkey backend injected — the same MEMORY_BACKEND_ADDR the M5 endpoint uses,
// reused for the seen-set (no new backend, no new env).
type asyncConfig struct {
	// DedupeAddr is the Valkey host:port for the seen-set — reused from
	// MEMORY_BACKEND_ADDR. Empty ⇒ dedupe is best-effort-disabled: the consumer
	// still runs but every message is treated as first-seen (fail-open by
	// construction; there is no store to consult).
	DedupeAddr string
	// SelfName is this agent's name (AGENT_NAME) — recorded on the consume span.
	SelfName string
}

// asyncConsumer holds the async-consume dependencies. Every field is read-only
// after construction; the SeenSet, offloader, and http.Client are
// concurrency-safe, so the whole struct is safe to share across the ksvc's
// request goroutines.
type asyncConsumer struct {
	cfg    asyncConfig
	seen   SeenSet // nil ⇒ dedupe disabled (fail-open: always first-seen).
	tracer trace.Tracer
	// offload rehydrates a $ref payload (blob offload, objectstore.go) BEFORE the
	// agent is invoked, so the agent sees the original payload. nil ⇒ offload
	// disabled (OBJECT_STORE_ADDR absent): a payload is passed through as-is (a
	// producer without a store never emits a $ref, so there is nothing to
	// rehydrate).
	offload *offloader
	// invoke delivers the decoded envelope's payload to the user container and
	// returns its response status. Injectable so unit tests drive the consumer
	// without a real upstream. In production it POSTs to the launcher's own proxy
	// path (which spans + forwards to the agent).
	invoke func(ctx context.Context, env envelope) (int, error)
}

// isCloudEventRequest reports whether an inbound HTTP request carries a
// CloudEvent (either content mode). Binary mode stamps the ce-id / ce-type /
// ce-source headers; structured mode uses Content-Type
// application/cloudevents+json. Recognising either lets the consumer branch a
// Trigger-delivered event away from an ordinary /invoke without parsing the body.
func isCloudEventRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	// Binary content mode: the required ce-id attribute is a header.
	if r.Header.Get("Ce-Id") != "" {
		return true
	}
	// Structured content mode: the whole event is the JSON body, marked by the
	// application/cloudevents+json media type (possibly with a ;charset= suffix).
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.HasPrefix(ct, cloudevents.ApplicationCloudEventsJSON)
}

// consume is the async-consume handler. It decodes the CloudEvent → envelope,
// dedupes on messageId (fail-closed), invokes the agent on a miss, and acks. It
// is wired into the proxy for CloudEvent-shaped POSTs (buildHandler), so an
// eventing agent's Trigger delivery lands here while an ordinary /invoke is
// unaffected.
//
// Ack semantics (Knative Eventing at-least-once): a 2xx acks the event (the
// broker drops it); a non-2xx is a NACK (the broker retries, then DLQs after the
// retry budget). A DUPLICATE is acked (2xx, no re-invoke) — it has already been
// processed. A decode failure is a 400 (a malformed event the broker should not
// retry endlessly — it will DLQ). An agent FAILURE is a 502 NACK (retry/DLQ). A
// DEDUPE-STORE ERROR is a 503 NACK (fail-closed: broker retries; on a transient
// blip the retry's dedupe check succeeds → exactly-once; persistent outage →
// bounded retries exhaust → DLQ, no infinite block).
func (c *asyncConsumer) consume(w http.ResponseWriter, r *http.Request) {
	ctx, span := c.tracer.Start(r.Context(), "a2a.async.consume",
		trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()
	start := time.Now()
	defer func() {
		span.SetAttributes(attribute.Int64("latency_ms", time.Since(start).Milliseconds()))
	}()
	span.SetAttributes(attribute.String("a2a.async.agent", c.cfg.SelfName))

	// Cap the inbound CloudEvent body before decoding — every other launcher
	// inbound path (readA2ABody, readCappedBody) enforces this. Blob offload
	// keeps event bodies small (a $ref, not the payload), so 1MiB is ample.
	r.Body = http.MaxBytesReader(w, r.Body, maxAsyncBody)
	// Decode the CloudEvent from the HTTP request (handles both content modes).
	evt, err := cloudevents.NewEventFromHTTPRequest(r)
	if err != nil {
		span.SetStatus(codes.Error, "decode CloudEvent: "+err.Error())
		span.SetAttributes(attribute.String("a2a.async.error", "bad_cloudevent"))
		writeJSONError(w, http.StatusBadRequest, "invalid CloudEvent: "+err.Error())
		return
	}

	env, err := cloudEventToEnvelope(*evt)
	if err != nil {
		span.SetStatus(codes.Error, "decode envelope: "+err.Error())
		span.SetAttributes(attribute.String("a2a.async.error", "bad_envelope"))
		writeJSONError(w, http.StatusBadRequest, "invalid async envelope: "+err.Error())
		return
	}
	span.SetAttributes(
		attribute.String("a2a.message.id", env.MessageID),
		attribute.String("a2a.sender", env.SenderAgentID),
		attribute.String("a2a.receiver", env.ReceiverAgentID),
		attribute.String("a2a.conversation.id", env.ConversationID),
	)

	// Dedupe on messageId (fail-closed, M11). Three outcomes from markSeen:
	//   (true, nil)  — first-seen: proceed to invoke.
	//   (false, nil) — duplicate within the TTL: ack (204) without re-invoking.
	//   (_, err)     — dedupe-store error/timeout: NACK (503) so the broker
	//                  retries; a transient blip means the retry succeeds →
	//                  exactly-once; persistent outage → DLQ (M7 machinery).
	firstSeen, seenErr := c.markSeen(ctx, span, env.MessageID)
	if seenErr != nil {
		// Fail-closed: do NOT process — NACK so the broker retries.
		span.SetStatus(codes.Error, "dedupe store error: "+seenErr.Error())
		span.SetAttributes(attribute.Bool("a2a.dedup_fail_closed", true))
		writeJSONError(w, http.StatusServiceUnavailable, "dedupe store unavailable: "+seenErr.Error())
		return
	}
	if !firstSeen {
		span.SetAttributes(attribute.Bool("a2a.dedup_hit", true))
		span.AddEvent("a2a.async.dedup_hit")
		// 204: acked, not re-processed.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	span.SetAttributes(attribute.Bool("a2a.dedup_hit", false))

	// Blob rehydrate (m7.6b): if the payload is a $ref (offloaded on publish
	// because it was >256KiB), GET the original bytes from the object store BEFORE
	// invoking the agent so the agent sees the real payload — never a $ref. A
	// dangling / failed GET is a NACK (502): the message DLQs rather than
	// delivering a broken payload (specs/eventing-scaling.md §"Edge cases:
	// Oversize payload"). Runs AFTER dedupe (a duplicate is acked without a
	// needless GET) and BEFORE invoke (the X-A2A-Envelope the invoker stamps
	// carries the rehydrated payload).
	if c.offload != nil {
		rehydrated, wasRef, rErr := c.offload.rehydrate(ctx, env.Payload)
		if rErr != nil {
			span.SetStatus(codes.Error, "rehydrate payload: "+rErr.Error())
			span.SetAttributes(attribute.String("a2a.async.error", "rehydrate_failed"))
			// NACK (502): a $ref we cannot resolve must DLQ, not reach the agent.
			writeJSONError(w, http.StatusBadGateway, "rehydrate payload failed: "+rErr.Error())
			return
		}
		if wasRef {
			span.SetAttributes(
				attribute.Bool("a2a.blob.rehydrated", true),
				attribute.Int("a2a.blob.size", len(rehydrated)),
			)
			env.Payload = rehydrated
		}
	}

	// First sighting: invoke the agent with the envelope payload.
	status, err := c.invoke(ctx, env)
	if err != nil {
		// The agent could not be reached / errored: NACK (502) so the broker
		// retries, then DLQs after the retry budget. NOT an ack — the message was
		// not processed.
		span.SetStatus(codes.Error, "agent invoke: "+err.Error())
		span.SetAttributes(attribute.String("a2a.async.error", "agent_unreachable"))
		writeJSONError(w, http.StatusBadGateway, "agent invoke failed: "+err.Error())
		return
	}
	span.SetAttributes(attribute.Int("a2a.agent.status", status))
	if status >= http.StatusInternalServerError {
		// The agent processed but failed (5xx): NACK so the broker retries/DLQs.
		// A poison message that fails every time lands in the DLQ after the retry
		// budget — the 🧪. We surface the agent's status so the broker's
		// retry/DLQ logic sees a failure.
		span.SetStatus(codes.Error, fmt.Sprintf("agent status %d", status))
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	// Processed successfully: ack.
	w.WriteHeader(http.StatusNoContent)
}

// markSeen consults the seen-set for messageID and returns (firstSeen, err).
// A nil store (dedupe disabled) yields (true, nil) — treat every message as
// first-seen when there is no store to consult. An error from the store (Valkey
// unreachable, per-op timeout) is returned as-is so the caller can FAIL CLOSED
// (NACK — do not process), preventing a dedupe-store blip from causing
// double-processing (M11, resolves M7 deferral). The error and the fail-closed
// decision are recorded on the span for observability.
func (c *asyncConsumer) markSeen(ctx context.Context, span trace.Span, messageID string) (bool, error) {
	if c.seen == nil {
		span.SetAttributes(attribute.Bool("a2a.dedup_enabled", false))
		return true, nil
	}
	opCtx, cancel := context.WithTimeout(ctx, dedupeOpTimeout)
	defer cancel()

	firstSeen, err := c.seen.MarkSeen(opCtx, messageID, dedupeTTL)
	if err != nil {
		// Fail CLOSED: surface the error so consume NACKs. Record it on the span
		// so the operator can observe the fail-closed decision and error detail.
		span.AddEvent("a2a.async.dedup_fail_closed", trace.WithAttributes(
			attribute.String("error", err.Error()),
		))
		return false, err
	}
	return firstSeen, nil
}

// newProxyInvoker builds the production invoke func: it POSTs the envelope's
// payload to the launcher's OWN proxy /invoke path (127.0.0.1:proxyPort), so the
// agent.invoke boundary span + the user container see the payload exactly as a
// sync call would. The full envelope travels in the X-A2A-Envelope header (as on
// the sync path) so the callee launcher's inbound access control can read it, and
// so a chained async→sync hop inherits the conversation context.
//
// It returns the agent's HTTP status. A transport error (agent down) is returned
// as an error so consume NACKs (retry/DLQ); a 5xx is returned as a status so
// consume can DLQ a poison message.
func newProxyInvoker(proxyPort int, client *http.Client) func(context.Context, envelope) (int, error) {
	base := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	return func(ctx context.Context, env envelope) (int, error) {
		payload := env.Payload
		if len(payload) == 0 {
			payload = []byte("null")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/invoke", bytes.NewReader(payload))
		if err != nil {
			return 0, fmt.Errorf("build invoke request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if env.ConversationID != "" {
			req.Header.Set("X-Conversation-Id", env.ConversationID)
		}
		// Carry the full envelope so the callee's inbound guard (a2a.go) can
		// enforce registry isolation / allowedCallers on the async path too.
		if envJSON, mErr := json.Marshal(env); mErr == nil {
			req.Header.Set(a2aEnvelopeHeader, string(envJSON))
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, fmt.Errorf("invoke agent: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		// Drain (bounded) so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAsyncBody))
		return resp.StatusCode, nil
	}
}

// ── publish (producer side) ──────────────────────────────────────────────────

// publishEnvelope encodes env as a CloudEvent (cloudevent.go) and POSTs it to
// brokerURL — the registry broker's addressable URL. It is the minimal producer
// path: enough for the e2e (and a producer example, m7.7) to emit an async A2A
// event that the broker routes to the target agent's Trigger.
//
// Blob offload (m7.6b): when off is non-nil and env's serialized payload exceeds
// offloadThreshold (256KiB), the payload is PUT to the object store under a
// content-addressed key and REPLACED with a {"$ref":...,"$size":n} object BEFORE
// the CloudEvent is encoded — so the event body carried to the broker stays
// small (a tiny $ref, well within maxAsyncBody). A store PUT failure is a typed
// error and the event is NOT emitted (best-effort, like memory/tools —
// specs/eventing-scaling.md §"Edge cases: Oversize payload"). A nil off (no
// OBJECT_STORE_ADDR) or a sub-threshold payload passes through inline unchanged.
//
// capToken (M141.4, ADR 0121) is the run capability relayed when brokerURL is the platform's async
// publish edge instead of a Knative Broker: that edge authenticates on it and derives the sender's
// namespace + registry from the control plane, so no broker credential ever reaches an agent pod. Empty
// for a direct-to-Broker publish, which is unchanged.
//
// It uses the CloudEvents HTTP binding (binary content mode) so the broker sees a
// well-formed event with the ce-id/ce-type/ce-source headers a Trigger filters
// on. A non-2xx broker response is an error (the event was not accepted); a
// transport failure likewise — the caller treats publish as best-effort and
// surfaces the typed error, never a bare hang.
func publishEnvelope(
	ctx context.Context, client *http.Client, brokerURL, capToken string, env envelope, off *offloader,
) error {
	if off != nil {
		offloaded, wasOffloaded, err := off.maybeOffload(ctx, env.Payload)
		if err != nil {
			// MinIO down / refused on publish: best-effort typed error, no
			// half-offloaded event emitted.
			return fmt.Errorf("offload oversize payload: %w", err)
		}
		if wasOffloaded {
			env.Payload = offloaded
		}
	}

	evt, err := envelopeToCloudEvent(env)
	if err != nil {
		return fmt.Errorf("encode CloudEvent: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, brokerURL, nil)
	if err != nil {
		return fmt.Errorf("build publish request: %w", err)
	}
	// WriteRequest stamps the CloudEvent binding headers + body onto the request
	// (binary content mode by default).
	if err := cehttp.WriteRequest(ctx, cloudevents.ToMessage(&evt), req); err != nil {
		return fmt.Errorf("write CloudEvent to request: %w", err)
	}
	// M141.4: when the destination is the platform's async PUBLISH EDGE rather than a Knative Broker,
	// relay the run capability — that edge authenticates on it and derives the sender's namespace +
	// registry from the control plane, so an agent pod needs no broker credentials of its own
	// (ADR 0121). Publishing straight at a Broker URL is unchanged: empty token, no header.
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("publish to broker %q: %w", brokerURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAsyncBody))
	// The broker acks a received event with 2xx (typically 202 Accepted).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("broker rejected event: status %d", resp.StatusCode)
	}
	return nil
}
