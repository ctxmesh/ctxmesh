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

// The synchronous agent-to-agent (A2A) mesh surface (M6, agent-mesh.md
// §"The A2A call contract", §"Message envelope", §"Design"). Two surfaces live
// here, both reusing the M3 OTel plumbing (tracer + W3C propagator) rather than
// rebuilding it:
//
//  1. OUTBOUND — a localhost listener serving POST /a2a/{targetAgent}. Started
//     ONLY when AGENT_REGISTRY_ID is injected (i.e. the agent is a resolved
//     AgentRegistry member), the same "env-gate a second listener" pattern as
//     the :2998 memory endpoint. The agent POSTs its opaque JSON payload; the
//     launcher stamps the platform envelope (§12.5), resolves the target over
//     DNS, injects W3C traceparent so the callee CONTINUES the same trace, and
//     forwards. It returns the peer's response, or a typed error — a
//     blocked/unknown/cross-registry target FAST-FAILS, never hangs.
//
//  2. INBOUND access control — a middleware the /invoke proxy runs on the
//     CALLEE side. When a request carries an A2A envelope (header
//     X-A2A-Envelope, stamped by the caller's launcher), the callee enforces
//     its allowedCallers allowlist and role rules BEFORE forwarding to the user
//     container, rejecting a disallowed caller with a typed caller_not_allowed
//     (403). A request with no envelope (an ordinary external /invoke) passes
//     through untouched. (agent-mesh.md §"Design": layers 2–3 are L7, enforced
//     by the callee's launcher on inbound /invoke; layer 1 is NetworkPolicy.)
//
// Trace shape (the crux): caller's agent.invoke → a2a.call span (this file) →
// callee's agent.invoke → callee's tools/memory. The a2a.call span is the
// parent of the outbound HTTP request; injecting its context as traceparent
// makes the callee's server span a child, so ONE trace tree spans both agents.
//
// Envelope depth/path (first-hop vs chained-hop): if the incoming /a2a request
// carries NO X-A2A-Envelope, this is a FIRST hop — the agent was invoked
// externally (or is the conversation root) — so depth=1 and path=[self]. If it
// DOES carry one, the agent was itself reached via A2A and is forwarding, so
// this is a CHAINED hop: depth=incoming.depth+1, path=incoming.path++[self],
// and traceId/registryId/conversationId are INHERITED from the incoming
// envelope (immutable downstream, §12.5). This is the single seam m6.6 extends:
// checkGuards(env) runs against the OUTGOING envelope just before forwarding.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	// defaultA2APort is the localhost port the outbound A2A listener binds when
	// A2A_PORT is unset. The launcher-local ports are :2998 memory, :2999 the
	// MCP discovery sidecar (internal/controller/toolinject.go discoveryPort;
	// injected whenever the agent has tool bindings, orthogonal to registry
	// membership) — so A2A takes :2997. A registry member that ALSO has tool
	// bindings (a mesh orchestrator with tools) shares one pod netns with the
	// sidecar; binding :2999 would EADDRINUSE and fail silently on the
	// best-effort listener path, so A2A must not collide with the sidecar.
	defaultA2APort = 2997

	// a2aDialTimeout bounds the TCP dial to a peer. A cross-registry target is
	// refused at the NetworkPolicy layer (connection refused / timeout); this
	// bound guarantees the caller fast-fails with a typed error instead of
	// hanging (agent-mesh.md §"Edge cases": bounded dial timeout).
	a2aDialTimeout = 2 * time.Second

	// a2aRequestTimeout bounds the whole outbound A2A round-trip (dial + headers
	// + body). A callee that is up but wedged must not block the caller's
	// request path indefinitely — the agent treats A2A as best-effort.
	a2aRequestTimeout = 30 * time.Second

	// maxA2ABody caps the caller's payload (and the peer's response we relay) at
	// 1MiB, matching the memory endpoint's bound. Larger payloads are an M7
	// (async + blob offload) concern.
	maxA2ABody = 1 << 20

	// maxTargetLen bounds the {targetAgent} path segment. It becomes a DNS label
	// and a span attribute, so it must be short and DNS-safe.
	maxTargetLen = 63

	// a2aEnvelopeHeader carries the platform envelope (JSON) between launchers:
	// the caller's launcher stamps it and injects it here; the callee's launcher
	// reads it for access control. It is ALSO how a chained hop learns the
	// incoming envelope (the user container echoes it back on its /a2a call).
	a2aEnvelopeHeader = "X-A2A-Envelope"
)

// A2A typed error codes (agent-mesh.md §"The A2A call contract",
// §"Edge cases"). Each is surfaced to the calling agent as a JSON body
// {"error":<code>,"detail":...} and recorded as a span error — never swallowed,
// never a bare network hang.
const (
	errUnknownTarget    = "unknown_target"     // DNS NXDOMAIN — target not in the registry namespace.
	errCallerNotAllowed = "caller_not_allowed" // access control: caller not on the callee's allowlist / role-denied.
	errBlocked          = "blocked"            // network-layer refusal (cross-registry NetworkPolicy) or dial timeout.
	errUpstreamFailure  = "upstream_failure"   // peer reachable but returned a transport error mid-request.
	errBadRequest       = "bad_request"        // malformed /a2a request (bad target, unreadable body, bad envelope).

	// Conversation guard codes (agent-mesh.md §12.7, added in m6.6).
	errDepthExceeded  = "depth_exceeded"  // depth > maxDepth: call chain is too deep.
	errCycleDetected  = "cycle_detected"  // receiver already appears in the envelope path.
	errBudgetExceeded = "budget_exceeded" // hop budget exhausted for this conversation.
)

// Guard configuration defaults (agent-mesh.md §"AgentRegistry spec", §12.7).
// These match the CRD-level defaults injected by the m6.4 controller; when the
// env vars are absent (non-registry agent, or a registry that omits the fields)
// the launcher falls back to these values so the guards are always active.
const (
	defaultMaxDepth  = 8  // reject A2A beyond this hop depth (CRD: guards.maxDepth).
	defaultHopBudget = 32 // per-conversation hop budget (CRD: guards.hopBudget).
)

// a2aConfig is the subset of configuration the A2A surface needs, parsed from
// env alongside the launcher Config (see loadA2AConfig / A2AEnabled). Every
// field is read-only after construction.
type a2aConfig struct {
	// RegistryID is AGENT_REGISTRY_ID — the stable registry id carried in every
	// envelope. Empty ⇒ the outbound listener is not started at all (the agent
	// is not a registry member) and inbound access control is a no-op.
	RegistryID string
	// Port is A2A_PORT (default 2997).
	Port int
	// SelfName is this agent's name (AGENT_NAME) — the senderAgentId it stamps
	// and the identity it appends to the envelope path.
	SelfName string
	// Role is AGENT_ROLE (orchestrator | worker | reviewer | custom) — stamped
	// into the envelope and checked by the callee's role policy.
	Role string
	// Namespace is POD_NAMESPACE — the Knative-route namespace targets resolve
	// in: http://{target}.{namespace}.svc.cluster.local.
	Namespace string
	// AllowedCallers is the callee-side allowlist (AGENT_ALLOWED_CALLERS,
	// comma-list of sender agent names). Empty ⇒ allow any in-registry caller
	// (registry membership is already enforced by NetworkPolicy). A non-empty
	// list rejects any senderAgentId not on it with caller_not_allowed.
	AllowedCallers []string

	// MaxDepth is the guard limit on envelope depth (A2A_MAX_DEPTH). Injected
	// by the m6.4 controller from the registry guards.maxDepth CRD field.
	// Default: defaultMaxDepth (8) when the env is absent or zero.
	MaxDepth int

	// HopBudget is the per-conversation hop-budget limit (A2A_HOP_BUDGET).
	// Injected by the m6.4 controller from the registry guards.hopBudget CRD
	// field. Default: defaultHopBudget (32) when the env is absent or zero.
	HopBudget int
}

// A2AEnabled reports whether the outbound /a2a listener should be started —
// true iff a registry id was injected (the agent is a registry member).
func (c Config) A2AEnabled() bool {
	return c.A2A.RegistryID != ""
}

// loadA2AConfig parses the A2A surface configuration from env.
//
// Environment variables:
//
//	AGENT_REGISTRY_ID (gate): the resolved registry id. Empty ⇒ the /a2a
//	  listener is NOT started and inbound access control is inert — every other
//	  A2A env is then irrelevant.
//	A2A_PORT (optional): outbound listener port (default 2997).
//	AGENT_NAME: this agent's name (senderAgentId / path identity). Reused from
//	  the shared launcher config.
//	AGENT_ROLE (optional): this agent's registry role.
//	POD_NAMESPACE: the namespace targets resolve in (Knative route). Injected via
//	  the downward API.
//	AGENT_ALLOWED_CALLERS (optional): comma-separated sender-name allowlist;
//	  empty allows any in-registry caller.
//	A2A_MAX_DEPTH (optional): max hop depth guard (default 8). Injected by the
//	  m6.4 controller from the registry guards.maxDepth CRD field.
//	A2A_HOP_BUDGET (optional): per-conversation hop budget (default 32). Injected
//	  by the m6.4 controller from the registry guards.hopBudget CRD field.
//
// Like loadMemoryConfig, it does NOT hard-fail on a missing name/namespace when
// the feature is on — the controller injects them; an empty segment is a
// visible-but-non-fatal misconfiguration, not a crash, on a best-effort path.
func loadA2AConfig(lookup func(string) string, agentName string) (a2aConfig, error) {
	regID := lookup("AGENT_REGISTRY_ID")
	if regID == "" {
		// Not gated on: the listener is skipped entirely.
		return a2aConfig{}, nil
	}

	port, err := parsePort(lookup("A2A_PORT"), defaultA2APort)
	if err != nil {
		return a2aConfig{}, fmt.Errorf("A2A_PORT: %w", err)
	}

	maxDepth, err := parseGuardInt(lookup("A2A_MAX_DEPTH"), defaultMaxDepth)
	if err != nil {
		return a2aConfig{}, fmt.Errorf("A2A_MAX_DEPTH: %w", err)
	}

	hopBudget, err := parseGuardInt(lookup("A2A_HOP_BUDGET"), defaultHopBudget)
	if err != nil {
		return a2aConfig{}, fmt.Errorf("A2A_HOP_BUDGET: %w", err)
	}

	return a2aConfig{
		RegistryID:     regID,
		Port:           port,
		SelfName:       agentName,
		Role:           lookup("AGENT_ROLE"),
		Namespace:      lookup("POD_NAMESPACE"),
		AllowedCallers: parseCallerList(lookup("AGENT_ALLOWED_CALLERS")),
		MaxDepth:       maxDepth,
		HopBudget:      hopBudget,
	}, nil
}

// parseCallerList splits a comma-separated allowlist, trimming whitespace and
// dropping empties. An empty or all-blank input yields nil (allow-all).
func parseCallerList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// envelope is the platform-owned A2A message envelope (agent-mesh.md §12.5).
// It is stamped by the caller's launcher and immutable downstream EXCEPT depth,
// path, and budgetRemaining, which each hop extends/decrements.
// payload is the caller's opaque JSON.
//
// budgetRemaining is a per-branch, per-conversation hop counter (m6.6). It is
// initialised to hopBudget on the FIRST hop and decremented by 1 on every
// subsequent chained hop. Because this is a synchronous (sync v1) in-envelope
// counter, each branch of a fan-out receives its own copy — a branch that calls
// N agents consumes N from its budget independently of sibling branches. A true
// cross-branch / cross-conversation aggregate budget (and token + wall-clock
// cost tracking) is deferred to CostBudget at M8; do NOT add shared cross-pod
// budget state here.
type envelope struct {
	TraceID         string          `json:"traceId"`
	RegistryID      string          `json:"registryId"`
	ConversationID  string          `json:"conversationId"`
	MessageID       string          `json:"messageId"`
	SenderAgentID   string          `json:"senderAgentId"`
	ReceiverAgentID string          `json:"receiverAgentId"`
	Role            string          `json:"role"`
	Depth           int             `json:"depth"`
	Path            []string        `json:"path"`
	BudgetRemaining int             `json:"budgetRemaining"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

// a2aServer holds the outbound-listener dependencies. Every field is read-only
// after construction and http.Client is safe for concurrent use, so the whole
// struct is safe to share across the listener's goroutines.
type a2aServer struct {
	cfg    a2aConfig
	client *http.Client // reused across all outbound calls — never per-request.
	tracer trace.Tracer
	prop   propagation.TextMapPropagator
	// resolveHost maps a target agent name to its base URL. Injectable so unit
	// tests can point it at an httptest server instead of cluster DNS.
	resolveHost func(target string) string
}

func newA2AServer(cfg a2aConfig, tracer trace.Tracer, prop propagation.TextMapPropagator) *a2aServer {
	s := &a2aServer{
		cfg:    cfg,
		tracer: tracer,
		prop:   prop,
		// One shared client with bounded dial + overall timeout. The Transport
		// pools connections; a per-request context deadline (a2aRequestTimeout)
		// is the primary bound, Timeout is belt-and-braces.
		client: &http.Client{
			Timeout: a2aRequestTimeout,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: a2aDialTimeout}).DialContext,
				// A dead / cross-registry TLS-less peer must not hang the
				// handshake; keep every phase bounded.
				ResponseHeaderTimeout: a2aRequestTimeout,
				MaxIdleConnsPerHost:   16,
			},
		},
	}
	s.resolveHost = s.clusterHost
	return s
}

// clusterHost is the production DNS discovery (agent-mesh.md §12.3, "DNS
// named"): a target resolves to its Knative route in the caller's namespace.
// No capability discovery (phase 2).
func (s *a2aServer) clusterHost(target string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local", target, s.cfg.Namespace)
}

// handler builds the outbound A2A mux. Go 1.22+ pattern routing gives us the
// {targetAgent} path variable and per-method matching for free.
func (s *a2aServer) handler() http.Handler {
	mux := http.NewServeMux()
	// /healthz reports only that the listener is up (no peer is probed) — a
	// cheap, dependency-free liveness signal, matching the memory endpoint.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /a2a/{targetAgent}", s.handleCall)
	return mux
}

// handleCall stamps the envelope, resolves the target, propagates the trace,
// forwards the payload, and relays the peer's response — or a typed error.
func (s *a2aServer) handleCall(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("targetAgent")

	// The a2a.call span is the parent of the outbound request: its context,
	// injected as traceparent, makes the callee's server span a child (§Design,
	// "Tracing"). Extract the caller's inbound context first so a chained hop
	// nests under the same trace, then start the child span under it.
	ctx := s.prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	ctx, span := s.tracer.Start(ctx, "a2a.call", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	start := time.Now()
	defer func() {
		span.SetAttributes(attribute.Int64("latency_ms", time.Since(start).Milliseconds()))
	}()

	span.SetAttributes(
		attribute.String("a2a.registry.id", s.cfg.RegistryID),
		attribute.String("a2a.sender", s.cfg.SelfName),
		attribute.String("a2a.receiver", target),
	)

	if err := validateTarget(target); err != nil {
		s.fail(w, span, http.StatusBadRequest, errBadRequest, err.Error())
		return
	}

	payload, err := readA2ABody(w, r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		s.fail(w, span, status, errBadRequest, err.Error())
		return
	}

	// Build the OUTGOING envelope: first-hop vs chained-hop turns on whether the
	// user container echoed an incoming envelope on this /a2a request.
	env, err := s.buildEnvelope(ctx, r, target, payload)
	if err != nil {
		s.fail(w, span, http.StatusBadRequest, errBadRequest, err.Error())
		return
	}
	span.SetAttributes(
		attribute.Int("a2a.depth", env.Depth),
		attribute.Int("a2a.budget_remaining", env.BudgetRemaining),
		attribute.String("a2a.conversation.id", env.ConversationID),
		attribute.String("a2a.message.id", env.MessageID),
	)

	// m6.6: conversation guards (max depth / cycle / hop budget) run here
	// against the OUTGOING envelope, before we forward. A non-nil guardError
	// maps to a typed 403 + a2a.guard_tripped span event.
	if guardErr := checkGuards(env, s.cfg.MaxDepth); guardErr != nil {
		span.AddEvent("a2a.guard_tripped", trace.WithAttributes(
			attribute.String("a2a.guard", guardErr.code),
		))
		s.fail(w, span, http.StatusForbidden, guardErr.code, guardErr.detail)
		return
	}

	s.forward(ctx, w, span, env)
}

// buildEnvelope constructs the outgoing envelope. See the file header for the
// first-hop vs chained-hop rule; the short version:
//
//   - No incoming X-A2A-Envelope ⇒ FIRST hop: depth=1, path=[self], traceId
//     from the active span, conversationId from the X-Conversation-Id header,
//     budgetRemaining=hopBudget (initialised here; decremented by every chained hop).
//   - An incoming envelope ⇒ CHAINED hop: depth=incoming+1,
//     path=incoming++[self], budgetRemaining=incoming.budgetRemaining-1, and
//     traceId/registryId/conversationId INHERITED (immutable downstream, §12.5).
//
// messageId is ALWAYS fresh (unique per hop — the M7 idempotency key).
func (s *a2aServer) buildEnvelope(
	ctx context.Context, r *http.Request, target string, payload json.RawMessage,
) (envelope, error) {
	self := s.cfg.SelfName

	incoming, err := parseIncomingEnvelope(r.Header.Get(a2aEnvelopeHeader))
	if err != nil {
		return envelope{}, err
	}

	env := envelope{
		RegistryID:      s.cfg.RegistryID,
		MessageID:       uuid.NewString(), // fresh per hop, always.
		SenderAgentID:   self,
		ReceiverAgentID: target,
		Role:            s.cfg.Role,
		Payload:         payload,
	}

	if incoming == nil {
		// FIRST hop: initialise all mutable per-hop fields.
		// budgetRemaining is set to hopBudget here; each chained hop decrements it
		// by 1 (see CHAINED hop path below). checkGuards trips when budgetRemaining
		// goes below zero, so hopBudget=N permits exactly N hops.
		env.TraceID = traceIDFromContext(ctx)
		env.ConversationID = conversationIDFromRequest(r)
		env.Depth = 1
		env.Path = []string{self}
		env.BudgetRemaining = s.cfg.HopBudget
		return env, nil
	}

	// CHAINED hop — inherit the immutable fields, extend depth/path/budget.
	env.RegistryID = incoming.RegistryID
	env.TraceID = incoming.TraceID
	env.ConversationID = incoming.ConversationID
	env.Depth = incoming.Depth + 1
	// Copy-then-append so we never alias the incoming slice's backing array
	// (the incoming envelope is immutable; a shared array would be a data race
	// were it ever retained).
	env.Path = append(append(make([]string, 0, len(incoming.Path)+1), incoming.Path...), self)
	// Decrement the per-branch hop budget. checkGuards will reject if this goes
	// below zero (i.e. < 0 on the OUTGOING envelope means this hop would exceed
	// the budget). The budget is per-branch for sync v1; cross-branch aggregation
	// joins CostBudget at M8.
	env.BudgetRemaining = incoming.BudgetRemaining - 1
	return env, nil
}

// forward resolves the target, injects the envelope + traceparent, sends the
// request, and relays the peer's response — mapping transport failures to typed
// errors so the caller never sees a bare hang.
func (s *a2aServer) forward(ctx context.Context, w http.ResponseWriter, span trace.Span, env envelope) {
	base := s.resolveHost(env.ReceiverAgentID)

	// The wire body is the FULL envelope (payload nested inside) so the callee's
	// launcher can read access-control fields; the envelope also travels as a
	// header for launchers that inspect it without buffering the body.
	envJSON, err := json.Marshal(env)
	if err != nil {
		s.fail(w, span, http.StatusInternalServerError, errUpstreamFailure, "marshal envelope: "+err.Error())
		return
	}

	// Bound the whole round-trip with a context deadline (primary bound; the
	// client Timeout is belt-and-braces).
	reqCtx, cancel := context.WithTimeout(ctx, a2aRequestTimeout)
	defer cancel()

	// The peer is a normal agent: it receives the call on its own /invoke route,
	// where the callee launcher's inbound middleware reads the envelope.
	outReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+"/invoke", bytes.NewReader(envJSON))
	if err != nil {
		s.fail(w, span, http.StatusBadGateway, errUpstreamFailure, "build request: "+err.Error())
		return
	}
	outReq.Header.Set("Content-Type", "application/json")
	outReq.Header.Set(a2aEnvelopeHeader, string(envJSON))
	// Inject W3C traceparent from the a2a.call span's context so the callee
	// CONTINUES this trace (its agent.invoke becomes our child). THIS is what
	// makes one trace tree span both agents.
	s.prop.Inject(reqCtx, propagation.HeaderCarrier(outReq.Header))

	resp, err := s.client.Do(outReq)
	if err != nil {
		s.failDial(w, span, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Relay the peer's response verbatim (status + capped body). A2A is
	// best-effort: even a peer 5xx is the agent's to handle, not a launcher
	// crash — we surface it as-is and mark the span.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxA2ABody))
	if err != nil {
		s.fail(w, span, http.StatusBadGateway, errUpstreamFailure, "read peer response: "+err.Error())
		return
	}
	span.SetAttributes(attribute.Int("a2a.peer.status", resp.StatusCode))
	if resp.StatusCode >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, "peer status "+resp.Status)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// failDial maps an outbound transport error to a typed A2A error. A
// cross-registry target refused at the NetworkPolicy layer, or a dial timeout,
// is "blocked" (fast-fail); an NXDOMAIN target is "unknown_target"; anything
// else mid-flight is "upstream_failure". Each is a span error — surfaced.
func (s *a2aServer) failDial(w http.ResponseWriter, span trace.Span, err error) {
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		s.fail(w, span, http.StatusNotFound, errUnknownTarget, "target not found: "+err.Error())
	case isTimeout(err) || isConnRefused(err):
		// NetworkPolicy refuses cross-registry TCP (connection refused) and a
		// dead/black-holed peer times out on dial — both are "blocked",
		// returned fast, never a long hang.
		s.fail(w, span, http.StatusBadGateway, errBlocked, "target unreachable: "+err.Error())
	default:
		s.fail(w, span, http.StatusBadGateway, errUpstreamFailure, "call failed: "+err.Error())
	}
}

// fail records a typed error on the span and writes the typed JSON body. It is
// the single failure shape of the outbound endpoint — never a bare hang, never
// a swallowed error (agent-mesh.md §"Edge cases": best-effort, typed).
func (s *a2aServer) fail(w http.ResponseWriter, span trace.Span, status int, code, detail string) {
	span.SetStatus(codes.Error, code)
	span.SetAttributes(attribute.String("a2a.error", code))
	writeA2AError(w, status, code, detail)
}

// ── inbound access control (callee side) ────────────────────────────────────

// a2aGuard holds the callee-side access-control policy. It is the inbound
// counterpart to a2aServer, injected into the /invoke proxy middleware.
type a2aGuard struct {
	cfg    a2aConfig
	tracer trace.Tracer
}

func newA2AGuard(cfg a2aConfig, tracer trace.Tracer) *a2aGuard {
	return &a2aGuard{cfg: cfg, tracer: tracer}
}

// enforceInbound is the callee-side access-control check the /invoke proxy runs
// BEFORE forwarding to the user container. It returns true when the request may
// proceed. If the request carries an A2A envelope, it enforces the allowlist +
// role rules; a disallowed caller is rejected here (403 caller_not_allowed,
// traced) and the function returns false, having already written the response.
//
// A request WITHOUT an envelope is an ordinary external /invoke — access
// control does not apply (registry isolation is a mesh-internal concern) — so it
// passes through untouched.
//
// It is wired as a wrapper INSIDE the agent.invoke span (ctx already carries the
// server span) so a denial is a child event of the invoke, not an orphan.
func (g *a2aGuard) enforceInbound(ctx context.Context, w http.ResponseWriter, r *http.Request) bool {
	raw := r.Header.Get(a2aEnvelopeHeader)
	if raw == "" {
		return true // not an A2A call — nothing to enforce.
	}

	env, err := parseIncomingEnvelope(raw)
	if err != nil || env == nil {
		// A malformed envelope on the inbound path is a protocol error: reject
		// rather than silently treat it as a plain call (fail closed).
		g.deny(ctx, w, "", "malformed A2A envelope")
		return false
	}

	if allowed, reason := g.allowCaller(env); !allowed {
		g.deny(ctx, w, env.SenderAgentID, reason)
		return false
	}
	return true
}

// allowCaller applies layers 2–3 of access control (agent-mesh.md §"Design")
// against an inbound envelope: (2) role — the sender's role must be a known
// registry role; (3) per-agent allowedCallers — the sender must be on this
// agent's allowlist when the allowlist is non-empty. It returns (false, reason)
// on denial. Layer 1 (registry isolation) is NetworkPolicy, enforced below us.
func (g *a2aGuard) allowCaller(env *envelope) (bool, string) {
	if !isKnownRole(env.Role) {
		return false, fmt.Sprintf("sender role %q is not a valid registry role", env.Role)
	}
	// Empty allowlist ⇒ allow any in-registry caller (NetworkPolicy already
	// scoped it to the registry).
	if len(g.cfg.AllowedCallers) == 0 {
		return true, ""
	}
	if slices.Contains(g.cfg.AllowedCallers, env.SenderAgentID) {
		return true, ""
	}
	return false, fmt.Sprintf("caller %q not in allowedCallers", env.SenderAgentID)
}

// deny writes the typed 403 caller_not_allowed and records a span event under
// the active agent.invoke span, so the denial is visible in the trace tree.
func (g *a2aGuard) deny(ctx context.Context, w http.ResponseWriter, caller, reason string) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent("a2a.caller_denied", trace.WithAttributes(
		attribute.String("a2a.sender", caller),
		attribute.String("a2a.deny_reason", reason),
	))
	span.SetAttributes(attribute.String("a2a.error", errCallerNotAllowed))
	writeA2AError(w, http.StatusForbidden, errCallerNotAllowed, reason)
}

// ── conversation-guard seam (m6.6) ──────────────────────────────────────────

// guardError is a typed conversation-guard denial (max depth / cycle / hop
// budget). It is defined here so m6.6 can add the checks without reshaping the
// outbound path — handleCall already maps a non-nil guardError to a typed 403.
type guardError struct {
	code   string
	detail string
}

// checkGuards enforces the three conversation guards (agent-mesh.md §12.7)
// against the OUTGOING envelope, just before the hop is forwarded. It is called
// by handleCall with the envelope already stamped (depth incremented, path
// extended, budgetRemaining decremented). Returns nil when all guards pass.
//
// Guards (evaluated in order; first trip wins):
//
//  1. Max depth — env.Depth > maxDepth → depth_exceeded.
//     Rejects a call that would push the hop depth past the registry limit.
//
//  2. Cycle detection — env.ReceiverAgentID ∈ env.Path → cycle_detected.
//     The path accumulates the IDs of agents that have already sent in this
//     chain (senders so far, ending in self). Forwarding to an agent that
//     appears in the path would revisit it without progress.
//
//  3. Hop budget — env.BudgetRemaining < 0 → budget_exceeded.
//     budgetRemaining was initialised to hopBudget on the first hop and
//     decremented by 1 on each chained hop in buildEnvelope. A value of -1
//     means this hop would be the (hopBudget+1)-th hop, exceeding the limit.
//     This is a per-branch counter for sync v1 (each fan-out branch receives
//     its own copy of the envelope). A cross-branch / cross-conversation
//     aggregate budget (and token + wall-clock cost tracking) is deferred to
//     CostBudget at M8 — do NOT add shared cross-pod budget state here.
//
// A tripped guard is a typed best-effort denial: handleCall maps a non-nil
// return to a 403 + a2a.guard_tripped span event; it never panics or crashes.
func checkGuards(env envelope, maxDepth int) *guardError {
	// Guard 1: max depth.
	if env.Depth > maxDepth {
		return &guardError{
			code:   errDepthExceeded,
			detail: fmt.Sprintf("depth %d exceeds maximum %d", env.Depth, maxDepth),
		}
	}

	// Guard 2: cycle detection.
	// env.Path contains the senders that have already participated in this chain
	// (up to and including self, appended by buildEnvelope). The outgoing
	// receiver appears in the path iff we have already visited it.
	if slices.Contains(env.Path, env.ReceiverAgentID) {
		return &guardError{
			code:   errCycleDetected,
			detail: fmt.Sprintf("receiver %q already in path %v", env.ReceiverAgentID, env.Path),
		}
	}

	// Guard 3: hop budget.
	// budgetRemaining < 0 means this hop was one beyond the allowed budget.
	if env.BudgetRemaining < 0 {
		return &guardError{
			code:   errBudgetExceeded,
			detail: fmt.Sprintf("hop budget exhausted (budgetRemaining %d)", env.BudgetRemaining),
		}
	}

	return nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

// The three built-in registry roles (agent-mesh.md: orchestrator | worker |
// reviewer, "the three built-ins always exist").
const (
	roleOrchestrator = "orchestrator"
	roleWorker       = "worker"
	roleReviewer     = "reviewer"
)

// knownRoles is the closed set of built-in registry roles. Custom roles are a
// registry-policy concern; in M6 the launcher accepts any non-empty role as
// long as it is one of the built-ins OR the callee has no stricter policy — we
// validate only that the built-ins are recognised and reject an empty role.
// (Custom-role policy is layered in with the registry role policy, phase 2.)
var knownRoles = map[string]struct{}{
	roleOrchestrator: {},
	roleWorker:       {},
	roleReviewer:     {},
}

// isKnownRole reports whether role is acceptable on an inbound envelope. An
// empty role is always rejected (an envelope must name its sender's role). A
// built-in role is accepted; any other non-empty role is treated as a valid
// custom role (the registry-level role policy — phase 2 — is where a closed
// custom set would be enforced), so we accept it rather than reject legitimate
// custom-role meshes.
func isKnownRole(role string) bool {
	if role == "" {
		return false
	}
	if _, ok := knownRoles[role]; ok {
		return true
	}
	// Non-empty custom role: accepted at the launcher layer (registry role
	// policy is phase 2). Kept as an explicit branch so the intent is clear.
	return true
}

// parseIncomingEnvelope decodes the X-A2A-Envelope header. An empty header
// yields (nil, nil) — the "first hop / no envelope" case. A present-but-invalid
// header is an error (a protocol violation, not a silent first hop).
func parseIncomingEnvelope(raw string) (*envelope, error) {
	if raw == "" {
		return nil, nil
	}
	var env envelope
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("invalid incoming envelope: %w", err)
	}
	return &env, nil
}

// traceIDFromContext returns the active span's 32-hex trace id, or "" if there
// is no valid span context (tracing disabled / no-op provider). Stamped into
// the envelope traceId so the platform can correlate the envelope with its
// trace even outside the W3C header.
func traceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// conversationIDFromRequest resolves the first-hop conversation id from the
// X-Conversation-Id header (documented contract: the agent stamps its
// conversation id there when it opens an A2A call), else a fresh UUID (a first
// hop with no id starts a new conversation). Chained hops inherit the id from
// the incoming envelope and never reach here. Reading a body field is avoided
// deliberately: the payload is opaque to the platform (§12.5) and must not be
// parsed on the hot path.
func conversationIDFromRequest(r *http.Request) string {
	if h := r.Header.Get("X-Conversation-Id"); h != "" {
		return h
	}
	return uuid.NewString()
}

// validateTarget enforces that the target agent name is a short, DNS-safe path
// segment (it becomes a DNS label and a span attribute).
func validateTarget(target string) error {
	if target == "" {
		return errors.New("target agent is required")
	}
	if len(target) > maxTargetLen {
		return fmt.Errorf("target agent name too long (max %d)", maxTargetLen)
	}
	for _, r := range target {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			// DNS-label-safe.
		default:
			return fmt.Errorf("target agent name contains disallowed character %q (DNS label chars only)", r)
		}
	}
	return nil
}

// errBodyTooLarge marks a payload that exceeded the 1MiB cap, so the handler
// can distinguish it (413) from a malformed-JSON payload (400).
var errBodyTooLarge = errors.New("request body exceeds size limit")

// readA2ABody reads the caller's payload under the 1MiB cap. An empty body is
// allowed (a call with no payload) and yields a JSON null so the envelope's
// payload is always valid JSON. An oversize body returns errBodyTooLarge; a
// non-JSON body returns a plain error — the handler maps them to 413 vs 400.
func readA2ABody(w http.ResponseWriter, r *http.Request) (json.RawMessage, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxA2ABody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, fmt.Errorf("%w (%d bytes)", errBodyTooLarge, maxA2ABody)
		}
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return json.RawMessage("null"), nil
	}
	// Validate it is JSON so the envelope stays well-formed; compact it so the
	// nested payload is canonical.
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return nil, fmt.Errorf("payload must be valid JSON: %w", err)
	}
	return json.RawMessage(compact.Bytes()), nil
}

// writeA2AError writes the typed error body {"error":<code>,"detail":<detail>}
// with the given status — the single error shape across the A2A surface.
func writeA2AError(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error  string `json:"error"`
		Detail string `json:"detail,omitempty"`
	}{Error: code, Detail: detail})
}

// parseGuardInt parses a positive-integer guard value (maxDepth, hopBudget)
// from an env-var string. An empty or zero value falls back to the default.
// A negative or non-integer value is an error.
func parseGuardInt(val string, defaultVal int) (int, error) {
	if val == "" {
		return defaultVal, nil
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("invalid guard value %q: must be a positive integer", val)
	}
	if n <= 0 {
		return defaultVal, nil // zero/negative ⇒ fall back to default (controller may omit).
	}
	return n, nil
}

// isTimeout reports whether err is (or wraps) a network timeout.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// isConnRefused reports whether err is (or wraps) a connection-refused / reset,
// the shape NetworkPolicy produces for a blocked cross-registry TCP connection.
func isConnRefused(err error) bool {
	// context.DeadlineExceeded surfaces as a timeout (handled above); a refused
	// connection surfaces as a syscall error whose string contains "refused"
	// or "reset". Matching on the wrapped net.OpError keeps this robust without
	// importing syscall for a platform-specific errno comparison.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		msg := opErr.Err.Error()
		return strings.Contains(msg, "refused") || strings.Contains(msg, "reset") ||
			strings.Contains(msg, "no route to host")
	}
	return false
}
