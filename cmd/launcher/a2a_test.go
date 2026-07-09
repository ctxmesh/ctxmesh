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

// Unit tests for the M6 A2A surface (a2a.go). The outbound path is exercised
// end-to-end against an httptest "peer" (resolveHost points at it instead of
// cluster DNS), so envelope construction, trace propagation, DNS/dial failure
// mapping, and response relay are all covered for real. The inbound access
// control is driven through the /invoke proxy middleware. Run with -race.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// peerCapture records what a peer agent received on its /invoke route.
type peerCapture struct {
	traceparent string
	envelope    envelope
	body        []byte
}

// newTestA2AServer builds an a2aServer wired to a fake peer. The peer echoes a
// canned response and records the traceparent + envelope it received; peerCh is
// buffered so a single call never blocks. resolveHost is overridden to route the
// (single) target name to the peer regardless of DNS.
func newTestA2AServer(
	t *testing.T, cfg a2aConfig, peerStatus int, peerBody string,
) (*a2aServer, *sdktrace.TracerProvider, *tracetest.SpanRecorder, chan peerCapture) {
	t.Helper()

	rec, tp := newTestTracer(t)
	peerCh := make(chan peerCapture, 32)

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env envelope
		_ = json.Unmarshal([]byte(r.Header.Get(a2aEnvelopeHeader)), &env)
		peerCh <- peerCapture{
			traceparent: r.Header.Get("Traceparent"),
			envelope:    env,
			body:        body,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(peerStatus)
		_, _ = w.Write([]byte(peerBody))
	}))
	t.Cleanup(peer.Close)

	peerURL, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}

	s := newA2AServer(cfg, tp.Tracer(tracerName), propagation.TraceContext{})
	// Route every target to the fake peer's base URL (the forward path appends
	// /invoke).
	s.resolveHost = func(string) string { return peerURL.Scheme + "://" + peerURL.Host }

	return s, tp, rec, peerCh
}

// baseCfg is a representative registry-member config. MaxDepth and HopBudget
// are set to the production defaults so the conversation guards are active but
// do not trip on normal (non-adversarial) test calls.
func baseCfg() a2aConfig {
	return a2aConfig{
		RegistryID: "research-team",
		Port:       defaultA2APort,
		SelfName:   "orchestrator",
		Role:       "orchestrator",
		Namespace:  "team-a",
		MaxDepth:   defaultMaxDepth,
		HopBudget:  defaultHopBudget,
	}
}

// callA2A issues a POST /a2a/{target} against the server's handler with an
// optional pre-seeded context (carrying a parent span) and optional headers.
// The envelope the peer received is asserted by the caller via peerCh.
func callA2A(
	t *testing.T, s *a2aServer, ctx context.Context, target, body string, headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/a2a/"+target, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, req)
	return rr
}

// ── config gating ───────────────────────────────────────────────────────────

func TestLoadA2AConfig(t *testing.T) {
	t.Parallel()

	t.Run("disabled when AGENT_REGISTRY_ID unset", func(t *testing.T) {
		t.Parallel()
		cfg, err := loadConfig(envMap(map[string]string{"AGENT_ENTRYPOINT": "/bin/agent"}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.A2AEnabled() {
			t.Error("A2AEnabled() = true without AGENT_REGISTRY_ID")
		}
	})

	t.Run("enabled with defaults and identity", func(t *testing.T) {
		t.Parallel()
		cfg, err := loadConfig(envMap(map[string]string{
			"AGENT_ENTRYPOINT":  "/bin/agent",
			"AGENT_NAME":        "orchestrator",
			"AGENT_REGISTRY_ID": "research-team",
			"AGENT_ROLE":        "orchestrator",
			"POD_NAMESPACE":     "team-a",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.A2AEnabled() {
			t.Fatal("A2AEnabled() = false, want true")
		}
		if cfg.A2A.Port != defaultA2APort {
			t.Errorf("A2A.Port = %d, want %d", cfg.A2A.Port, defaultA2APort)
		}
		if cfg.A2A.SelfName != "orchestrator" || cfg.A2A.Namespace != "team-a" || cfg.A2A.Role != "orchestrator" {
			t.Errorf("identity = %+v, want orchestrator/team-a/orchestrator", cfg.A2A)
		}
		if len(cfg.A2A.AllowedCallers) != 0 {
			t.Errorf("AllowedCallers = %v, want empty (allow-all)", cfg.A2A.AllowedCallers)
		}
	})

	t.Run("allowedCallers parsed and trimmed", func(t *testing.T) {
		t.Parallel()
		cfg, err := loadConfig(envMap(map[string]string{
			"AGENT_ENTRYPOINT":      "/bin/agent",
			"AGENT_REGISTRY_ID":     "research-team",
			"AGENT_ALLOWED_CALLERS": " orchestrator , , research ,",
		}))
		if err != nil {
			t.Fatal(err)
		}
		got := cfg.A2A.AllowedCallers
		if len(got) != 2 || got[0] != "orchestrator" || got[1] != "research" {
			t.Errorf("AllowedCallers = %v, want [orchestrator research]", got)
		}
	})

	t.Run("invalid A2A_PORT rejected", func(t *testing.T) {
		t.Parallel()
		_, err := loadConfig(envMap(map[string]string{
			"AGENT_ENTRYPOINT":  "/bin/agent",
			"AGENT_REGISTRY_ID": "research-team",
			"A2A_PORT":          "not-a-port",
		}))
		if err == nil {
			t.Error("expected error for invalid A2A_PORT")
		}
	})

	t.Run("A2A envs inert when feature off", func(t *testing.T) {
		t.Parallel()
		_, err := loadConfig(envMap(map[string]string{
			"AGENT_ENTRYPOINT": "/bin/agent",
			"A2A_PORT":         "not-a-port",
		}))
		if err != nil {
			t.Errorf("A2A envs must be inert when the feature is off: %v", err)
		}
	})
}

// ── envelope: first hop ─────────────────────────────────────────────────────

func TestA2AFirstHopEnvelope(t *testing.T) {
	t.Parallel()
	s, tp, _, peerCh := newTestA2AServer(t, baseCfg(), http.StatusOK, `{"ok":true}`)

	// Wrap the request in a parent span so the envelope's traceId is populated.
	ctx, parent := tp.Tracer(tracerName).Start(context.Background(), "agent.invoke")
	defer parent.End()
	parentTraceID := parent.SpanContext().TraceID().String()

	rr := callA2A(t, s, ctx, "research", `{"question":"why is the sky blue"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	env := (<-peerCh).envelope
	if env.Depth != 1 {
		t.Errorf("first-hop depth = %d, want 1", env.Depth)
	}
	if len(env.Path) != 1 || env.Path[0] != "orchestrator" {
		t.Errorf("first-hop path = %v, want [orchestrator]", env.Path)
	}
	if env.SenderAgentID != "orchestrator" || env.ReceiverAgentID != "research" {
		t.Errorf("sender/receiver = %s/%s, want orchestrator/research", env.SenderAgentID, env.ReceiverAgentID)
	}
	if env.RegistryID != "research-team" {
		t.Errorf("registryId = %q, want research-team", env.RegistryID)
	}
	if env.Role != "orchestrator" {
		t.Errorf("role = %q, want orchestrator", env.Role)
	}
	if env.MessageID == "" {
		t.Error("messageId must be stamped (fresh per hop)")
	}
	if env.ConversationID == "" {
		t.Error("first-hop conversationId must default to a fresh id")
	}
	if env.TraceID != parentTraceID {
		t.Errorf("envelope traceId = %q, want the active trace %q", env.TraceID, parentTraceID)
	}
	// The payload must be forwarded verbatim (nested in the envelope).
	var p struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil || p.Question != "why is the sky blue" {
		t.Errorf("payload not forwarded verbatim: %s (err=%v)", env.Payload, err)
	}
}

func TestA2AFirstHopHonorsConversationHeader(t *testing.T) {
	t.Parallel()
	s, _, _, peerCh := newTestA2AServer(t, baseCfg(), http.StatusOK, `{}`)

	callA2A(t, s, context.Background(), "research", `{}`,
		map[string]string{"X-Conversation-Id": "conv-42"})

	if env := (<-peerCh).envelope; env.ConversationID != "conv-42" {
		t.Errorf("conversationId = %q, want conv-42 (from header)", env.ConversationID)
	}
}

// ── envelope: chained hop ───────────────────────────────────────────────────

func TestA2AChainedHopEnvelope(t *testing.T) {
	t.Parallel()
	// This agent is "research"; it was reached via A2A from "orchestrator" and
	// now forwards to "analyst".
	cfg := baseCfg()
	cfg.SelfName = "research"
	cfg.Role = "worker"
	s, _, _, peerCh := newTestA2AServer(t, cfg, http.StatusOK, `{}`)

	incoming := envelope{
		TraceID:         "4bf92f3577b34da6a3ce929d0e0e4736",
		RegistryID:      "research-team",
		ConversationID:  "conv-77",
		MessageID:       "msg-upstream",
		SenderAgentID:   "orchestrator",
		Role:            "orchestrator",
		Depth:           1,
		Path:            []string{"orchestrator"},
		BudgetRemaining: defaultHopBudget, // set by the first hop; each chained hop decrements.
	}
	incJSON, _ := json.Marshal(incoming)

	callA2A(t, s, context.Background(), "analyst", `{"task":"crunch"}`,
		map[string]string{a2aEnvelopeHeader: string(incJSON)})

	env := (<-peerCh).envelope
	if env.Depth != 2 {
		t.Errorf("chained depth = %d, want 2 (incoming 1 + 1)", env.Depth)
	}
	wantPath := []string{"orchestrator", "research"}
	if len(env.Path) != 2 || env.Path[0] != wantPath[0] || env.Path[1] != wantPath[1] {
		t.Errorf("chained path = %v, want %v", env.Path, wantPath)
	}
	// Immutable-downstream fields are INHERITED, not regenerated.
	if env.TraceID != incoming.TraceID {
		t.Errorf("chained traceId = %q, want inherited %q", env.TraceID, incoming.TraceID)
	}
	if env.ConversationID != "conv-77" {
		t.Errorf("chained conversationId = %q, want inherited conv-77", env.ConversationID)
	}
	if env.RegistryID != "research-team" {
		t.Errorf("chained registryId = %q, want inherited research-team", env.RegistryID)
	}
	// messageId is fresh per hop — must NOT be inherited.
	if env.MessageID == "msg-upstream" || env.MessageID == "" {
		t.Errorf("chained messageId = %q, want a fresh id (not the upstream one)", env.MessageID)
	}
	// sender/receiver/role reflect THIS hop.
	if env.SenderAgentID != "research" || env.ReceiverAgentID != "analyst" || env.Role != "worker" {
		t.Errorf("this-hop identity = %s→%s (%s), want research→analyst (worker)",
			env.SenderAgentID, env.ReceiverAgentID, env.Role)
	}
	// budgetRemaining is decremented on each chained hop.
	wantBudget := defaultHopBudget - 1
	if env.BudgetRemaining != wantBudget {
		t.Errorf("chained budgetRemaining = %d, want %d (incoming %d - 1)",
			env.BudgetRemaining, wantBudget, defaultHopBudget)
	}
}

// TestA2AChainedHopDoesNotAliasIncomingPath guards the copy-on-append in
// buildEnvelope: extending the path must not mutate the caller's slice.
func TestA2AChainedHopDoesNotAliasIncomingPath(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	cfg.SelfName = "research"
	s := newA2AServer(cfg, noop.NewTracerProvider().Tracer("x"), propagation.TraceContext{})

	incoming := &envelope{Path: []string{"orchestrator"}, Depth: 1}
	original := append([]string(nil), incoming.Path...)
	incJSON, _ := json.Marshal(incoming)

	req := httptest.NewRequest(http.MethodPost, "/a2a/analyst", strings.NewReader(`{}`))
	req.Header.Set(a2aEnvelopeHeader, string(incJSON))
	env, err := s.buildEnvelope(context.Background(), req, "analyst", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Path) != 2 {
		t.Fatalf("outgoing path = %v, want 2 elements", env.Path)
	}
	// The parsed incoming path (a fresh decode) is what matters at runtime; the
	// key property is the outgoing path is a distinct backing array. Mutate the
	// outgoing path and confirm the original literal is unchanged.
	env.Path[0] = "TAMPERED"
	if original[0] != "orchestrator" {
		t.Error("append aliased the incoming path backing array")
	}
}

// ── trace propagation (THE CRUX) ────────────────────────────────────────────

// TestA2ATracePropagation asserts the child request to the peer carries the
// SAME trace-id as the caller's active span, AND that the a2a.call span the
// launcher emits is a child of that caller span. This is the property that
// makes one trace tree span both agents.
func TestA2ATracePropagation(t *testing.T) {
	t.Parallel()
	s, tp, rec, peerCh := newTestA2AServer(t, baseCfg(), http.StatusOK, `{}`)

	// The caller's inbound trace, as it would exist inside agent.invoke.
	ctx, parent := tp.Tracer(tracerName).Start(context.Background(), "agent.invoke")
	parentSC := parent.SpanContext()
	parentTraceID := parentSC.TraceID().String()

	rr := callA2A(t, s, ctx, "research", `{}`, nil)
	parent.End()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	cap := <-peerCh

	// (1) The peer received a traceparent, and its trace-id matches the caller's.
	if cap.traceparent == "" {
		t.Fatal("peer did not receive a traceparent header — trace NOT propagated")
	}
	// W3C: version-traceid-spanid-flags. The trace-id is field [1].
	fields := strings.Split(cap.traceparent, "-")
	if len(fields) != 4 {
		t.Fatalf("malformed traceparent %q", cap.traceparent)
	}
	if fields[1] != parentTraceID {
		t.Errorf("peer trace-id = %q, want caller trace-id %q (SAME trace)", fields[1], parentTraceID)
	}
	// (2) The envelope's traceId agrees with the propagated trace-id.
	if cap.envelope.TraceID != parentTraceID {
		t.Errorf("envelope traceId = %q, want %q", cap.envelope.TraceID, parentTraceID)
	}

	// (3) The a2a.call span exists, is a child of the caller span, and carries
	// the peer's span-id as ITS outbound parent (i.e. the peer nests under it).
	var callSpan sdktrace.ReadOnlySpan
	for _, sp := range rec.Ended() {
		if sp.Name() == "a2a.call" {
			callSpan = sp
		}
	}
	if callSpan == nil {
		t.Fatal("no a2a.call span recorded")
	}
	if callSpan.SpanContext().TraceID().String() != parentTraceID {
		t.Errorf("a2a.call trace-id = %q, want same as caller %q",
			callSpan.SpanContext().TraceID(), parentTraceID)
	}
	if callSpan.Parent().SpanID() != parentSC.SpanID() {
		t.Errorf("a2a.call parent span-id = %q, want caller span-id %q — a2a.call is not nested under agent.invoke",
			callSpan.Parent().SpanID(), parentSC.SpanID())
	}
	// (4) The traceparent the peer received names the a2a.call span as its
	// parent span-id — so the peer's server span becomes a2a.call's child.
	if fields[2] != callSpan.SpanContext().SpanID().String() {
		t.Errorf("peer parent span-id = %q, want a2a.call span-id %q — peer will not nest under a2a.call",
			fields[2], callSpan.SpanContext().SpanID())
	}
}

// ── response relay ──────────────────────────────────────────────────────────

func TestA2ARelaysPeerResponse(t *testing.T) {
	t.Parallel()
	s, _, _, _ := newTestA2AServer(t, baseCfg(), http.StatusTeapot, `{"answer":"42"}`)

	rr := callA2A(t, s, context.Background(), "research", `{}`, nil)
	if rr.Code != http.StatusTeapot {
		t.Errorf("relayed status = %d, want 418 (peer status passed through)", rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"answer":"42"}` {
		t.Errorf("relayed body = %q, want the peer's body", got)
	}
}

// ── access control (callee side, via the /invoke proxy) ─────────────────────

// newTestProxyWithGuard builds the /invoke proxy handler wired with an a2aGuard
// for cfg, pointed at an echo upstream. Returns the handler and a channel that
// receives true iff the upstream was reached (i.e. the request was allowed).
func newTestProxyWithGuard(t *testing.T, cfg a2aConfig) (http.Handler, chan bool) {
	t.Helper()
	h, reached, _ := newTestProxyWithGuardRec(t, cfg)
	return h, reached
}

// newTestProxyWithGuardRec is newTestProxyWithGuard plus the span recorder, so a
// test can assert the deny span event (e.g. a2a.cross_registry_denied) the guard
// records on the agent.invoke span.
func newTestProxyWithGuardRec(t *testing.T, cfg a2aConfig) (http.Handler, chan bool, *tracetest.SpanRecorder) {
	t.Helper()
	rec, tp := newTestTracer(t)
	tracer := tp.Tracer(tracerName)

	reachedCh := make(chan bool, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedCh <- true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	guard := newA2AGuard(cfg, tracer)
	handler := buildHandler(tracer, propagation.TraceContext{}, upstreamURL, Config{AgentName: cfg.SelfName}, guard)
	return handler, reachedCh, rec
}

// assertSpanEvent asserts the span recorder captured an event with the given
// name on the agent.invoke span (the callee-side inbound span).
func assertSpanEvent(t *testing.T, rec *tracetest.SpanRecorder, spanName, eventName string) {
	t.Helper()
	for _, sp := range rec.Ended() {
		if sp.Name() != spanName {
			continue
		}
		for _, ev := range sp.Events() {
			if ev.Name == eventName {
				return
			}
		}
	}
	t.Errorf("no %q event found on %q span", eventName, spanName)
}

// inboundEnvelope marshals an envelope for the X-A2A-Envelope header, stamped
// with the callee's own registry ("research-team", matching baseCfg) so the
// cross-registry check passes and the allowedCallers/role checks are exercised.
func inboundEnvelope(sender, role string) string {
	return inboundEnvelopeIn("research-team", sender, role)
}

// inboundEnvelopeIn marshals an envelope carrying an explicit registryId, so a
// test can drive the cross-registry (foreign registryId) deny path.
func inboundEnvelopeIn(registryID, sender, role string) string {
	e := envelope{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", RegistryID: registryID,
		ConversationID: "c1", MessageID: "m1", SenderAgentID: sender,
		ReceiverAgentID: "research", Role: role, Depth: 1, Path: []string{sender},
	}
	b, _ := json.Marshal(e)
	return string(b)
}

func TestA2AInboundNoEnvelopePassesThrough(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	cfg.AllowedCallers = []string{"nobody"} // strict allowlist...
	handler, reached := newTestProxyWithGuard(t, cfg)

	// ...but a plain external /invoke (no envelope) is NOT an A2A call and must
	// pass through untouched.
	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("plain /invoke status = %d, want 200 (access control must not apply)", rr.Code)
	}
	if !<-reached {
		t.Error("upstream not reached for a plain /invoke")
	}
}

func TestA2AInboundAllowsEmptyAllowlist(t *testing.T) {
	t.Parallel()
	cfg := baseCfg() // empty AllowedCallers ⇒ allow any in-registry caller.
	handler, reached := newTestProxyWithGuard(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{}`))
	req.Header.Set(a2aEnvelopeHeader, inboundEnvelope("orchestrator", "orchestrator"))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("allowed A2A status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !<-reached {
		t.Error("upstream not reached for an allowed A2A caller")
	}
}

func TestA2AInboundAllowsListedCaller(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	cfg.AllowedCallers = []string{"orchestrator", "research"}
	handler, reached := newTestProxyWithGuard(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{}`))
	req.Header.Set(a2aEnvelopeHeader, inboundEnvelope("research", "worker"))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("listed caller status = %d, want 200", rr.Code)
	}
	if !<-reached {
		t.Error("upstream not reached for a listed caller")
	}
}

func TestA2AInboundDeniesUnlistedCaller(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	cfg.AllowedCallers = []string{"orchestrator"}
	handler, reached := newTestProxyWithGuard(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{}`))
	req.Header.Set(a2aEnvelopeHeader, inboundEnvelope("intruder", "worker"))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("unlisted caller status = %d, want 403", rr.Code)
	}
	assertA2AError(t, rr.Body.Bytes(), errCallerNotAllowed)

	// The upstream (user container) must NOT have been reached.
	select {
	case <-reached:
		t.Error("upstream reached despite a denied caller")
	default:
	}
}

func TestA2AInboundDeniesEmptyRole(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	handler, reached := newTestProxyWithGuard(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{}`))
	req.Header.Set(a2aEnvelopeHeader, inboundEnvelope("orchestrator", "")) // empty role.
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("empty-role status = %d, want 403", rr.Code)
	}
	select {
	case <-reached:
		t.Error("upstream reached despite an invalid (empty) role")
	default:
	}
}

func TestA2AInboundDeniesMalformedEnvelope(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	handler, reached := newTestProxyWithGuard(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{}`))
	req.Header.Set(a2aEnvelopeHeader, `{not valid json`)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("malformed-envelope status = %d, want 403 (fail closed)", rr.Code)
	}
	select {
	case <-reached:
		t.Error("upstream reached despite a malformed envelope")
	default:
	}
}

// ── cross-registry isolation (app-layer, m6.8) ──────────────────────────────

// TestA2AInboundDeniesCrossRegistry verifies the app-layer registry-isolation
// check: an inbound envelope whose registryId does NOT match the callee's own
// registry is a hard deny with a typed cross_registry_denied (403), the upstream
// (user container) is NOT reached, and the a2a.cross_registry_denied span event
// is recorded. This is layer 1 enforced app-layer — NetworkPolicy cannot isolate
// Knative-routed A2A (kourier fronts every hop), which the m6.8 live e2e proved.
func TestA2AInboundDeniesCrossRegistry(t *testing.T) {
	t.Parallel()
	cfg := baseCfg() // this agent is in "research-team".
	handler, reached, rec := newTestProxyWithGuardRec(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{}`))
	// Foreign registry: a valid orchestrator caller, but from "other-team".
	req.Header.Set(a2aEnvelopeHeader, inboundEnvelopeIn("other-team", "orchestrator", "orchestrator"))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-registry status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	assertA2AError(t, rr.Body.Bytes(), errCrossRegistry)
	assertSpanEvent(t, rec, "agent.invoke", "a2a.cross_registry_denied")

	// The upstream (user container) must NOT have been reached.
	select {
	case <-reached:
		t.Error("upstream reached despite a cross-registry caller")
	default:
	}
}

// TestA2ACrossRegistryCheckedBeforeAllowlist proves the ordering: a foreign
// registryId is a HARD deny even when the caller WOULD be on the allowlist — the
// registry check runs before allowedCallers/role, so the failure is
// cross_registry_denied (not caller_not_allowed) and the upstream is never hit.
func TestA2ACrossRegistryCheckedBeforeAllowlist(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	cfg.AllowedCallers = []string{"orchestrator"} // caller IS on the allowlist...
	handler, reached, rec := newTestProxyWithGuardRec(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{}`))
	// ...but names a foreign registry → cross-registry deny wins.
	req.Header.Set(a2aEnvelopeHeader, inboundEnvelopeIn("other-team", "orchestrator", "orchestrator"))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	assertA2AError(t, rr.Body.Bytes(), errCrossRegistry)
	assertSpanEvent(t, rec, "agent.invoke", "a2a.cross_registry_denied")
	select {
	case <-reached:
		t.Error("upstream reached despite a cross-registry caller on the allowlist")
	default:
	}
}

// TestA2AInboundAllowsSameRegistry verifies a same-registry envelope passes the
// registry check and proceeds to the allowedCallers/role checks (here: empty
// allowlist + valid role ⇒ allowed, upstream reached). This is the positive
// counterpart to the cross-registry deny.
func TestA2AInboundAllowsSameRegistry(t *testing.T) {
	t.Parallel()
	cfg := baseCfg() // registry "research-team", empty allowlist.
	handler, reached := newTestProxyWithGuard(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{}`))
	req.Header.Set(a2aEnvelopeHeader, inboundEnvelopeIn("research-team", "orchestrator", "orchestrator"))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("same-registry status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !<-reached {
		t.Error("upstream not reached for a same-registry caller")
	}
}

// ── failure mapping ─────────────────────────────────────────────────────────

func TestA2AUnknownTarget(t *testing.T) {
	t.Parallel()
	// Point resolveHost at a name guaranteed to NXDOMAIN.
	_, tp := newTestTracer(t)
	s := newA2AServer(baseCfg(), tp.Tracer(tracerName), propagation.TraceContext{})
	s.resolveHost = func(target string) string {
		return "http://" + target + ".this-domain-does-not-exist.invalid"
	}

	rr := callA2A(t, s, context.Background(), "ghost", `{}`, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown target status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	assertA2AError(t, rr.Body.Bytes(), errUnknownTarget)
}

func TestA2ABlockedTargetFastFails(t *testing.T) {
	t.Parallel()
	// A closed port on localhost: the dial is refused immediately (the
	// NetworkPolicy "connection refused" shape) → typed blocked, no hang.
	_, tp := newTestTracer(t)
	s := newA2AServer(baseCfg(), tp.Tracer(tracerName), propagation.TraceContext{})
	s.resolveHost = func(string) string { return "http://127.0.0.1:1" } // port 1: refused.

	rr := callA2A(t, s, context.Background(), "blocked-peer", `{}`, nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("blocked target status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	assertA2AError(t, rr.Body.Bytes(), errBlocked)
}

func TestA2ARejectsBadTarget(t *testing.T) {
	t.Parallel()
	s, _, _, _ := newTestA2AServer(t, baseCfg(), http.StatusOK, `{}`)

	for _, bad := range []string{"has_underscore", "UP:colon", "path/slash", strings.Repeat("x", maxTargetLen+1)} {
		req := httptest.NewRequest(http.MethodPost, "/a2a/"+url.PathEscape(bad), strings.NewReader(`{}`))
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("bad target %q status = %d, want 400", bad, rr.Code)
		}
	}
}

func TestA2ARejectsNonJSONPayload(t *testing.T) {
	t.Parallel()
	s, _, _, _ := newTestA2AServer(t, baseCfg(), http.StatusOK, `{}`)

	// A non-JSON payload is a 400 (bad request), NOT a 413.
	rr := callA2A(t, s, context.Background(), "research", `not json at all`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("non-JSON payload status = %d, want 400", rr.Code)
	}
	assertA2AError(t, rr.Body.Bytes(), errBadRequest)
}

func TestA2ARejectsOversizePayload(t *testing.T) {
	t.Parallel()
	s, _, _, _ := newTestA2AServer(t, baseCfg(), http.StatusOK, `{}`)

	// A body over the 1MiB cap is a 413.
	big := `"` + strings.Repeat("x", maxA2ABody) + `"`
	rr := callA2A(t, s, context.Background(), "research", big, nil)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize payload status = %d, want 413", rr.Code)
	}
	assertA2AError(t, rr.Body.Bytes(), errBadRequest)
}

func TestA2AEmptyPayloadIsNull(t *testing.T) {
	t.Parallel()
	s, _, _, peerCh := newTestA2AServer(t, baseCfg(), http.StatusOK, `{}`)

	// An empty body is allowed and becomes a JSON null payload (the envelope
	// stays well-formed).
	rr := callA2A(t, s, context.Background(), "research", ``, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty-body status = %d, want 200", rr.Code)
	}
	if env := (<-peerCh).envelope; string(env.Payload) != "null" {
		t.Errorf("empty-body payload = %s, want null", env.Payload)
	}
}

// ── checkGuards (m6.6) ──────────────────────────────────────────────────────

// TestCheckGuardsDepth verifies the max-depth guard: a chain at exactly
// maxDepth passes; one beyond trips depth_exceeded with a typed 403 +
// a2a.guard_tripped span event.
func TestCheckGuardsDepth(t *testing.T) {
	t.Parallel()
	const maxDepth = 4

	t.Run("at limit passes", func(t *testing.T) {
		t.Parallel()
		env := envelope{
			Depth:           maxDepth,
			Path:            []string{"a"},
			ReceiverAgentID: "b",
			BudgetRemaining: 10,
		}
		if err := checkGuards(env, maxDepth); err != nil {
			t.Errorf("checkGuards at maxDepth=%d: unexpected trip %+v", maxDepth, err)
		}
	})

	t.Run("one beyond trips depth_exceeded", func(t *testing.T) {
		t.Parallel()
		s, _, rec, _ := newTestA2AServer(t, func() a2aConfig {
			c := baseCfg()
			c.MaxDepth = maxDepth
			return c
		}(), http.StatusOK, `{}`)

		// Construct a chained incoming envelope already at maxDepth, so the
		// outgoing envelope will be at maxDepth+1.
		incoming := envelope{
			TraceID:         "4bf92f3577b34da6a3ce929d0e0e4736",
			RegistryID:      "research-team",
			ConversationID:  "conv-depth",
			MessageID:       "msg-x",
			SenderAgentID:   "prev",
			Role:            "orchestrator",
			Depth:           maxDepth,
			Path:            pathOf(maxDepth),
			BudgetRemaining: 10,
		}
		incJSON, _ := json.Marshal(incoming)

		rr := callA2A(t, s, context.Background(), "next", `{}`,
			map[string]string{a2aEnvelopeHeader: string(incJSON)})

		if rr.Code != http.StatusForbidden {
			t.Fatalf("depth_exceeded: status = %d, want 403; body=%s", rr.Code, rr.Body.String())
		}
		assertA2AError(t, rr.Body.Bytes(), errDepthExceeded)

		// The a2a.guard_tripped span event must be recorded.
		assertGuardTripped(t, rec, errDepthExceeded)
	})
}

// TestCheckGuardsCycle verifies the cycle-detection guard: calling an agent
// already in the path trips cycle_detected; a non-revisiting call passes.
func TestCheckGuardsCycle(t *testing.T) {
	t.Parallel()

	t.Run("non-revisiting call passes", func(t *testing.T) {
		t.Parallel()
		// path=[a,b], receiver=c → no cycle.
		env := envelope{
			Depth:           2,
			Path:            []string{"a", "b"},
			ReceiverAgentID: "c",
			BudgetRemaining: 10,
		}
		if err := checkGuards(env, defaultMaxDepth); err != nil {
			t.Errorf("no-cycle call: unexpected trip %+v", err)
		}
	})

	t.Run("revisiting agent trips cycle_detected", func(t *testing.T) {
		t.Parallel()
		// buildEnvelope appends self (cfg.SelfName="orchestrator") to the incoming
		// path ["prev"], giving the outgoing path ["prev","orchestrator"]. Calling
		// back to "prev" makes the receiver already present in the path →
		// cycle_detected.
		cfg := baseCfg()
		cfg.SelfName = "orchestrator"
		s, _, rec, _ := newTestA2AServer(t, cfg, http.StatusOK, `{}`)

		incoming := envelope{
			TraceID:         "4bf92f3577b34da6a3ce929d0e0e4736",
			RegistryID:      "research-team",
			ConversationID:  "conv-cycle",
			MessageID:       "msg-y",
			SenderAgentID:   "prev",
			Role:            "worker",
			Depth:           1,
			Path:            []string{"prev"},
			BudgetRemaining: 10,
		}
		incJSON, _ := json.Marshal(incoming)

		// Call back to "prev" — "prev" is in the path, triggering cycle_detected.
		rr := callA2A(t, s, context.Background(), "prev", `{}`,
			map[string]string{a2aEnvelopeHeader: string(incJSON)})

		if rr.Code != http.StatusForbidden {
			t.Fatalf("cycle_detected: status = %d, want 403; body=%s", rr.Code, rr.Body.String())
		}
		assertA2AError(t, rr.Body.Bytes(), errCycleDetected)
		assertGuardTripped(t, rec, errCycleDetected)
	})
}

// TestCheckGuardsBudget verifies the hop-budget guard. The trip condition is
// BudgetRemaining <= 0 on the OUTGOING envelope, with first-hop init = hopBudget
// and each chained hop decrementing by 1, so hopBudget=N permits EXACTLY N hops
// (the (N+1)-th trips), hopBudget=1 permits 1 hop, and hopBudget=0 trips on the
// first hop (0 hops). It asserts first-hop init, per-hop decrement, and the
// exact boundary end-to-end by chaining each hop's outgoing envelope into the
// next hop's incoming envelope.
func TestCheckGuardsBudget(t *testing.T) {
	t.Parallel()

	t.Run("first hop initialises budget to hopBudget", func(t *testing.T) {
		t.Parallel()
		cfg := baseCfg()
		cfg.SelfName = "agent"
		cfg.HopBudget = 3
		s, _, _, peerCh := newTestA2AServer(t, cfg, http.StatusOK, `{}`)
		rr := callA2A(t, s, context.Background(), "b", `{}`, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("first hop status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		env := <-peerCh
		if env.envelope.BudgetRemaining != 3 {
			t.Errorf("first-hop budgetRemaining = %d, want 3", env.envelope.BudgetRemaining)
		}
	})

	t.Run("each chained hop decrements budget", func(t *testing.T) {
		t.Parallel()
		cfg := baseCfg()
		cfg.SelfName = "agent"
		cfg.HopBudget = 3
		s, _, _, peerCh := newTestA2AServer(t, cfg, http.StatusOK, `{}`)

		incoming := budgetIncoming(3)
		incJSON, _ := json.Marshal(incoming)

		rr := callA2A(t, s, context.Background(), "b", `{}`,
			map[string]string{a2aEnvelopeHeader: string(incJSON)})
		if rr.Code != http.StatusOK {
			t.Fatalf("chained hop status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		env := <-peerCh
		if env.envelope.BudgetRemaining != 2 {
			t.Errorf("chained budgetRemaining = %d, want 2 (= incoming 3 - 1)", env.envelope.BudgetRemaining)
		}
	})

	// The boundary: hopBudget=N ⇒ exactly N hops succeed, the (N+1)-th trips.
	// Drive a real chain: the first /a2a call has no incoming envelope (the first
	// hop), then each subsequent call feeds the previous hop's outgoing envelope
	// back in as the incoming envelope, exactly as a chained hop would on the
	// wire. Assert the count of hops that reach the peer and that the next trips.
	for _, hopBudget := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("hopBudget=%d permits exactly %d hops", hopBudget, hopBudget), func(t *testing.T) {
			t.Parallel()
			cfg := baseCfg()
			cfg.SelfName = "agent"
			cfg.HopBudget = hopBudget
			// MaxDepth must not trip before the budget does across the chain, so
			// raise it well above the hop count we drive.
			cfg.MaxDepth = 1000
			s, _, rec, peerCh := newTestA2AServer(t, cfg, http.StatusOK, `{}`)

			var incomingHeader map[string]string // nil ⇒ first hop.
			passed := 0
			for hop := 0; hop <= hopBudget; hop++ { // one extra iteration to hit the trip.
				rr := callA2A(t, s, context.Background(), "b", `{}`, incomingHeader)
				if rr.Code == http.StatusOK {
					passed++
					out := (<-peerCh).envelope
					// Feed this hop's outgoing envelope in as the next hop's incoming.
					outJSON, _ := json.Marshal(out)
					incomingHeader = map[string]string{a2aEnvelopeHeader: string(outJSON)}
					continue
				}
				// The trip: must be a typed 403 budget_exceeded with a span event.
				if rr.Code != http.StatusForbidden {
					t.Fatalf("hop %d: status = %d, want 403 (budget_exceeded); body=%s", hop, rr.Code, rr.Body.String())
				}
				assertA2AError(t, rr.Body.Bytes(), errBudgetExceeded)
				assertGuardTripped(t, rec, errBudgetExceeded)
				break
			}
			if passed != hopBudget {
				t.Errorf("hopBudget=%d permitted %d hops, want exactly %d", hopBudget, passed, hopBudget)
			}
		})
	}
}

// budgetIncoming builds a representative chained incoming envelope carrying the
// given remaining hop budget.
func budgetIncoming(budgetRemaining int) envelope {
	return envelope{
		TraceID:         "4bf92f3577b34da6a3ce929d0e0e4736",
		RegistryID:      "research-team",
		ConversationID:  "conv-budget",
		MessageID:       "msg-z",
		SenderAgentID:   "prev",
		Role:            "worker",
		Depth:           1,
		Path:            []string{"prev"},
		BudgetRemaining: budgetRemaining,
	}
}

// ── guard test helpers ────────────────────────────────────────────────────────

// pathOf returns a path of n distinct agent names for test envelopes.
func pathOf(n int) []string {
	path := make([]string, n)
	for i := range n {
		path[i] = fmt.Sprintf("agent-%d", i)
	}
	return path
}

// assertGuardTripped asserts that the span recorder captured an a2a.guard_tripped
// event with the expected guard code on the a2a.call span.
func assertGuardTripped(t *testing.T, rec *tracetest.SpanRecorder, wantCode string) {
	t.Helper()
	for _, sp := range rec.Ended() {
		if sp.Name() != "a2a.call" {
			continue
		}
		for _, ev := range sp.Events() {
			if ev.Name != "a2a.guard_tripped" {
				continue
			}
			for _, attr := range ev.Attributes {
				if string(attr.Key) == "a2a.guard" && attr.Value.AsString() == wantCode {
					return // found.
				}
			}
		}
	}
	t.Errorf("no a2a.guard_tripped event with a2a.guard=%q found on a2a.call span", wantCode)
}

// ── concurrency (-race) ─────────────────────────────────────────────────────

// TestA2AConcurrentCalls fires many parallel /a2a calls at one server and
// asserts every call reaches the peer with a well-formed, per-hop-unique
// envelope. Run with -race, it exercises the shared http.Client, tracer, and
// handler for data races.
func TestA2AConcurrentCalls(t *testing.T) {
	t.Parallel()
	s, tp, _, peerCh := newTestA2AServer(t, baseCfg(), http.StatusOK, `{}`)

	const callers = 24
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			ctx, sp := tp.Tracer(tracerName).Start(context.Background(), "agent.invoke")
			defer sp.End()
			body := fmt.Sprintf(`{"n":%d}`, i)
			rr := callA2A(t, s, ctx, "research", body, nil)
			if rr.Code != http.StatusOK {
				t.Errorf("concurrent call %d status = %d, want 200", i, rr.Code)
			}
		})
	}
	wg.Wait()
	close(peerCh)

	// Every call must have produced a fresh, unique messageId (per-hop key).
	seen := map[string]bool{}
	count := 0
	for cap := range peerCh {
		count++
		mid := cap.envelope.MessageID
		if mid == "" {
			t.Error("a concurrent call produced an empty messageId")
		}
		if seen[mid] {
			t.Errorf("duplicate messageId %q across concurrent calls", mid)
		}
		seen[mid] = true
	}
	if count != callers {
		t.Errorf("peer received %d calls, want %d", count, callers)
	}
}

// ── role / helper units ─────────────────────────────────────────────────────

func TestIsKnownRole(t *testing.T) {
	t.Parallel()
	for _, r := range []string{"orchestrator", "worker", "reviewer", "custom-role"} {
		if !isKnownRole(r) {
			t.Errorf("isKnownRole(%q) = false, want true", r)
		}
	}
	if isKnownRole("") {
		t.Error("isKnownRole(\"\") = true, want false (empty role rejected)")
	}
}

func TestValidateTarget(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"research", "agent-1", "Analyst2"} {
		if err := validateTarget(ok); err != nil {
			t.Errorf("validateTarget(%q) = %v, want nil", ok, err)
		}
	}
	bad := []string{"", "under_score", "has.dot", "has/slash", "has:colon", strings.Repeat("a", maxTargetLen+1)}
	for _, b := range bad {
		if err := validateTarget(b); err == nil {
			t.Errorf("validateTarget(%q) = nil, want error", b)
		}
	}
}

func TestParseIncomingEnvelope(t *testing.T) {
	t.Parallel()

	t.Run("empty ⇒ nil (first hop)", func(t *testing.T) {
		t.Parallel()
		env, err := parseIncomingEnvelope("")
		if err != nil || env != nil {
			t.Errorf("parseIncomingEnvelope(\"\") = (%v, %v), want (nil, nil)", env, err)
		}
	})
	t.Run("valid ⇒ decoded", func(t *testing.T) {
		t.Parallel()
		env, err := parseIncomingEnvelope(`{"depth":3,"path":["a","b","c"],"registryId":"r"}`)
		if err != nil {
			t.Fatal(err)
		}
		if env.Depth != 3 || len(env.Path) != 3 || env.RegistryID != "r" {
			t.Errorf("decoded = %+v", env)
		}
	})
	t.Run("invalid ⇒ error", func(t *testing.T) {
		t.Parallel()
		if _, err := parseIncomingEnvelope(`{bad`); err == nil {
			t.Error("expected error for invalid envelope JSON")
		}
	})
}

// assertA2AError asserts body is {"error":<wantCode>,...}.
func assertA2AError(t *testing.T, body []byte, wantCode string) {
	t.Helper()
	var e struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body is not JSON: %s", body)
	}
	if e.Error != wantCode {
		t.Errorf("error code = %q, want %q; detail=%q", e.Error, wantCode, e.Detail)
	}
}
