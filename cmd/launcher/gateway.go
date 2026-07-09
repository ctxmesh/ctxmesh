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

// The launcher's OUTBOUND gateway proxy (M8, specs/cost-governance.md). A third
// launcher listener — beside the :2998 memory endpoint and the :2997 A2A
// endpoint — started ONLY when the controller injected a budget (a BUDGET_* env
// var is present). When wired, the controller repoints the agent's
// MODEL_GATEWAY_URL at this listener and sets GATEWAY_UPSTREAM_URL to the real
// LiteLLM gateway. The agent's LLM calls therefore pass THROUGH the launcher,
// which is where cost enforcement must live: it is our Go seam on the outbound
// LLM path (LiteLLM is a third-party Python process we do not modify; it prices
// each call for the M3 cost span but has no budget logic).
//
// Enforcement per call (specs/cost-governance.md "Gateway enforcement contract"):
//   - read identity + budget from the request headers the launcher itself stamps
//     (X-Conversation-Id, X-Agent-Name, X-Budget-*),
//   - PRE-CALL hard check: refuse with a typed budget_exceeded (HTTP 402) BEFORE
//     forwarding when spent + estimate would breach a hard cap,
//   - forward to LiteLLM, price the response (reusing LiteLLM's own cost when it
//     reports one), book the ACTUAL cost,
//   - emit a one-shot soft alert when a threshold is first crossed,
//   - record budget.* attributes on a gateway.call span.
//
// No budget env ⇒ this listener never starts and MODEL_GATEWAY_URL still points
// straight at LiteLLM: the M2 happy path is byte-for-byte unchanged.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ctxmesh/agent-engine/internal/gateway/budget"
)

const (
	// defaultGatewayProxyPort is the localhost port the outbound gateway proxy
	// binds when GATEWAY_PROXY_PORT is unset.
	defaultGatewayProxyPort = 2996

	// gatewayRequestTimeout bounds each upstream LiteLLM round-trip.
	gatewayRequestTimeout = 60 * time.Second

	// maxGatewayRespBody caps how much of the LiteLLM response we read to price
	// the call (parse the usage block). Completion bodies are small; 4MiB is a
	// generous ceiling that also bounds proxy memory.
	maxGatewayRespBody = 4 << 20

	// Identity + budget headers the launcher stamps on the outbound request and
	// re-reads here. Kept as constants so the stamp site and the read site cannot
	// drift.
	hdrConversationID = "X-Conversation-Id"
	hdrAgentName      = "X-Agent-Name"
	hdrBudgetConvUSD  = "X-Budget-Conversation-USD"
	hdrBudgetAgentUSD = "X-Budget-Agent-USD"
	hdrBudgetSoftPct  = "X-Budget-Soft-Pct"

	// errBudgetExceededCost is the typed error code for a hard-cap circuit-break
	// on the COST dimension (§14). It is deliberately the same string the A2A hop
	// guard uses (agent-mesh.md errBudgetExceeded) — both are "budget_exceeded",
	// the response's "dimension" field distinguishes cost (conversation|agent)
	// from the hop guard.
	errBudgetExceededCost = "budget_exceeded"

	// defaultSoftPct mirrors the CRD default (BudgetSpec.softThresholdPct=80) so a
	// missing/blank X-Budget-Soft-Pct still enforces a sane soft threshold.
	defaultSoftPct = 80
)

// gatewayConfig is the outbound-gateway-proxy configuration parsed from env.
type gatewayConfig struct {
	// UpstreamURL is the real LiteLLM gateway base URL (GATEWAY_UPSTREAM_URL).
	// Empty ⇒ the proxy is DISABLED (no listener started): the agent talks to
	// LiteLLM directly, exactly as pre-M8.
	UpstreamURL string
	// Port is the localhost port this proxy listens on (GATEWAY_PROXY_PORT,
	// default 2996). The controller points MODEL_GATEWAY_URL here.
	Port int
	// AgentName keys per-agent spend (from AGENT_NAME).
	AgentName string
	// ConvCapUSD / AgentCapUSD are the raw budget-cap strings (BUDGET_PER_*_USD).
	// Empty ⇒ that dimension is unenforced.
	ConvCapUSD  string
	AgentCapUSD string
	// SoftPct is the soft-alert percentage (BUDGET_SOFT_PCT), default 80.
	SoftPct int
}

// GatewayProxyEnabled reports whether the outbound gateway proxy should start.
// True iff an upstream gateway URL was injected AND at least one budget cap is
// set — the ONLY reason to interpose the proxy is to enforce a cap. With neither
// cap set the controller does not inject GATEWAY_UPSTREAM_URL, so the agent's
// MODEL_GATEWAY_URL keeps pointing straight at LiteLLM.
func (c Config) GatewayProxyEnabled() bool {
	g := c.Gateway
	return g.UpstreamURL != "" && (g.ConvCapUSD != "" || g.AgentCapUSD != "")
}

// loadGatewayConfig parses the outbound-gateway-proxy configuration from env.
//
// Environment variables (all injected by the controller when spec.budget is set):
//
//	GATEWAY_UPSTREAM_URL (gate): the real LiteLLM base URL. Empty ⇒ the proxy is
//	  not started; every other budget env is then irrelevant.
//	GATEWAY_PROXY_PORT (optional): listener port (default 2996).
//	BUDGET_PER_CONVERSATION_USD / BUDGET_PER_AGENT_USD: hard caps (either may be
//	  empty ⇒ that dimension unenforced).
//	BUDGET_SOFT_PCT: soft-alert percentage (default 80 when blank/invalid).
//	AGENT_NAME: keys per-agent spend (shared launcher config).
//
// Like the memory/A2A loaders it does not hard-fail on a blank cap or name when
// the gate is set — a misconfiguration degrades to "that dimension unenforced"
// rather than crashing the launcher on a best-effort path.
func loadGatewayConfig(lookup func(string) string, agentName string) (gatewayConfig, error) {
	upstream := lookup("GATEWAY_UPSTREAM_URL")
	if upstream == "" {
		return gatewayConfig{}, nil
	}

	port, err := parsePort(lookup("GATEWAY_PROXY_PORT"), defaultGatewayProxyPort)
	if err != nil {
		return gatewayConfig{}, fmt.Errorf("GATEWAY_PROXY_PORT: %w", err)
	}

	softPct := defaultSoftPct
	if raw := strings.TrimSpace(lookup("BUDGET_SOFT_PCT")); raw != "" {
		if v, convErr := strconv.Atoi(raw); convErr == nil && v >= 1 && v <= 99 {
			softPct = v
		}
	}

	return gatewayConfig{
		UpstreamURL: strings.TrimRight(upstream, "/"),
		Port:        port,
		AgentName:   agentName,
		ConvCapUSD:  strings.TrimSpace(lookup("BUDGET_PER_CONVERSATION_USD")),
		AgentCapUSD: strings.TrimSpace(lookup("BUDGET_PER_AGENT_USD")),
		SoftPct:     softPct,
	}, nil
}

// budgetErrorBody is the typed budget_exceeded response body (§14). spent/cap are
// exact-decimal strings.
type budgetErrorBody struct {
	Error     string `json:"error"`
	Dimension string `json:"dimension"`
	Spent     string `json:"spent"`
	Cap       string `json:"cap"`
}

// gatewayProxy is the outbound gateway proxy handler. It holds the shared,
// process-wide enforcer/estimator (spend accumulates across every request) and
// forwards to the upstream LiteLLM gateway.
type gatewayProxy struct {
	cfg       gatewayConfig
	upstream  *url.URL
	enforcer  *budget.Enforcer
	estimator *budget.Estimator
	client    *http.Client
	tracer    trace.Tracer
	// caps is the per-process budget derived once from the config (the caps are
	// static — the same for every request this launcher serves). Parsed at
	// construction so a malformed cap fails fast at startup, not per request.
	convCap  *budget.Money
	agentCap *budget.Money
	logf     func(string, ...any)
}

// buildGatewayServer constructs the :2996 http.Server when the budget proxy is
// enabled, or returns nil when it is not. Factored out of main() to keep its
// cyclomatic complexity within the project lint limit. A construction error (bad
// upstream URL / cap) is logged and nil is returned — the agent's
// MODEL_GATEWAY_URL then 502s, a visible misconfig, not a silent budget bypass.
func buildGatewayServer(cfg Config, tracer trace.Tracer) *http.Server {
	if !cfg.GatewayProxyEnabled() {
		return nil
	}
	logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	gp, err := newGatewayProxy(cfg.Gateway, tracer, logf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher: gateway proxy disabled: %v\n", err)
		return nil
	}
	return &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Gateway.Port),
		Handler: gp.handler(),
	}
}

// newGatewayProxy builds the proxy from config. A malformed cap string is a
// startup error (the controller only ever injects CRD-validated decimals, so
// this is defence-in-depth). A nil tracer falls back to the global no-op tracer
// via the caller.
func newGatewayProxy(cfg gatewayConfig, tracer trace.Tracer, logf func(string, ...any)) (*gatewayProxy, error) {
	u, err := url.Parse(cfg.UpstreamURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gateway: invalid GATEWAY_UPSTREAM_URL %q", cfg.UpstreamURL)
	}

	gp := &gatewayProxy{
		cfg:       cfg,
		upstream:  u,
		enforcer:  budget.NewEnforcer(),
		estimator: budget.NewEstimator(),
		client:    &http.Client{Timeout: gatewayRequestTimeout},
		tracer:    tracer,
		logf:      logf,
	}

	if cfg.ConvCapUSD != "" {
		m, perr := budget.ParseMoney(cfg.ConvCapUSD)
		if perr != nil {
			return nil, fmt.Errorf("gateway: BUDGET_PER_CONVERSATION_USD %q: %w", cfg.ConvCapUSD, perr)
		}
		gp.convCap = &m
	}
	if cfg.AgentCapUSD != "" {
		m, perr := budget.ParseMoney(cfg.AgentCapUSD)
		if perr != nil {
			return nil, fmt.Errorf("gateway: BUDGET_PER_AGENT_USD %q: %w", cfg.AgentCapUSD, perr)
		}
		gp.agentCap = &m
	}

	return gp, nil
}

// handler returns the HTTP handler for the proxy listener.
func (gp *gatewayProxy) handler() http.Handler {
	return http.HandlerFunc(gp.serve)
}

// capsFor builds the per-request Caps.
//
//   - ConversationID is read from the X-Conversation-Id header the AGENT stamps
//     on its outbound LLM call — the SAME documented platform convention the
//     memory (:2998) and A2A (:2997) paths already use (conversationIDFromRequest).
//     Absent ⇒ per-conversation enforcement is simply skipped for this call
//     (the agent chose not to identify a conversation); the per-agent cap, if
//     set, still applies.
//   - AgentName + the caps + SoftPct come from the launcher's OWN env (the
//     controller injected them). They are NOT read from headers: the launcher is
//     the enforcement point, so it owns identity/caps directly — an agent cannot
//     forge a higher cap or a different agent name. The X-Budget-* / X-Agent-Name
//     header constants remain the documented contract surface and are stripped
//     before forwarding to LiteLLM.
func (gp *gatewayProxy) capsFor(r *http.Request) budget.Caps {
	return budget.Caps{
		ConversationID: r.Header.Get(hdrConversationID),
		AgentName:      gp.cfg.AgentName,
		ConvCap:        gp.convCap,
		AgentCap:       gp.agentCap,
		SoftPct:        gp.cfg.SoftPct,
	}
}

// serve enforces the budget then reverse-proxies the request to LiteLLM. It
// wraps the whole thing in a gateway.call span carrying the budget attributes.
func (gp *gatewayProxy) serve(w http.ResponseWriter, r *http.Request) {
	ctx, span := gp.tracer.Start(r.Context(), "gateway.call", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	caps := gp.capsFor(r)
	route := gp.routeFromRequest(r)

	span.SetAttributes(
		attribute.String("agent.name", caps.AgentName),
		attribute.String("gateway.route", route),
	)
	if caps.ConversationID != "" {
		span.SetAttributes(attribute.String("a2a.conversation.id", caps.ConversationID))
	}

	// ── PRE-CALL hard check ────────────────────────────────────────────────
	// Only when a dimension is actually enforceable (cap set + its identity key
	// present). Estimate is conservative: the last observed cost on this route.
	if caps.Enforced() {
		est := gp.estimator.Estimate(route)
		if dec := gp.enforcer.PreCall(caps, est); !dec.Allowed {
			gp.writeBudgetExceeded(w, span, dec)
			return
		}
	}

	// ── Forward to LiteLLM ─────────────────────────────────────────────────
	resp, body, err := gp.forward(ctx, r)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "gateway upstream error")
		http.Error(w, fmt.Sprintf("gateway upstream error: %v", err), http.StatusBadGateway)
		return
	}

	// Relay the upstream response verbatim (status, headers, body). Budget
	// enforcement is transparent to a successful call — the agent sees exactly
	// what LiteLLM returned.
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, werr := w.Write(body); werr != nil {
		gp.logf("launcher: gateway: write response: %v", werr)
	}

	// ── POST-CALL accounting ───────────────────────────────────────────────
	// Only book cost for a successful completion (a 4xx/5xx from LiteLLM cost the
	// tenant nothing and must not accrue spend). Price prefers LiteLLM's own cost
	// header; falls back to the deterministic token table.
	if resp.StatusCode != http.StatusOK || !caps.Enforced() {
		return
	}
	actual := budget.PriceCall(resp.Header.Get(budget.LiteLLMCostHeader), body)
	gp.estimator.Observe(route, actual)

	convSpent, agentSpent, state, alert := gp.enforcer.PostCall(caps, actual)
	gp.annotateSpan(span, caps, convSpent, agentSpent, state, actual)

	if alert != nil {
		// Soft breach: one-shot alert (span event + log line), then continue.
		span.AddEvent("budget.alert", trace.WithAttributes(
			attribute.String("budget.dimension", string(alert.Dimension)),
			attribute.String("budget.spent_usd", alert.Spent.String()),
			attribute.String("budget.cap_usd", alert.Cap.String()),
			attribute.String("budget.soft_usd", alert.SoftUSD.String()),
		))
		gp.logf("launcher: gateway: budget.alert dimension=%s spent=%s cap=%s (soft %d%%=%s)",
			alert.Dimension, alert.Spent, alert.Cap, gp.cfg.SoftPct, alert.SoftUSD)
	}
}

// forward sends the (budget-approved) request to the upstream LiteLLM gateway,
// preserving method, path, query, and body headers, and returns the response
// plus its (capped) body. The upstream response body is fully read so it can be
// both relayed and priced.
func (gp *gatewayProxy) forward(ctx context.Context, r *http.Request) (*http.Response, []byte, error) {
	target := *gp.upstream
	target.Path = singleJoiningPath(gp.upstream.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery

	// r.Body is drained here; the inbound handler owns closing the original.
	var reqBody io.Reader
	if r.Body != nil {
		reqBody = r.Body
	}
	outReq, err := http.NewRequestWithContext(ctx, r.Method, target.String(), reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("build upstream request: %w", err)
	}
	// Copy content-type / authorization / etc., but DROP the budget/identity
	// headers — they are internal to the launcher↔proxy contract and must not
	// leak to LiteLLM.
	copyForwardHeaders(outReq.Header, r.Header)

	resp, err := gp.client.Do(outReq)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGatewayRespBody))
	if err != nil {
		return nil, nil, fmt.Errorf("read upstream response: %w", err)
	}
	return resp, body, nil
}

// writeBudgetExceeded emits the typed 402 budget_exceeded response and marks the
// span. The provider is NOT called (this is the circuit-break).
func (gp *gatewayProxy) writeBudgetExceeded(w http.ResponseWriter, span trace.Span, dec budget.PreCallDecision) {
	span.SetAttributes(
		attribute.String("budget.state", string(budget.StateExceeded)),
		attribute.String("budget.dimension", string(dec.Dimension)),
		attribute.String("budget.spent_usd", dec.Spent.String()),
		attribute.String("budget.cap_usd", dec.Cap.String()),
	)
	span.SetStatus(codes.Error, errBudgetExceededCost)

	body := budgetErrorBody{
		Error:     errBudgetExceededCost,
		Dimension: string(dec.Dimension),
		Spent:     dec.Spent.String(),
		Cap:       dec.Cap.String(),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired) // 402
	if err := json.NewEncoder(w).Encode(body); err != nil {
		gp.logf("launcher: gateway: encode budget_exceeded: %v", err)
	}
	gp.logf("launcher: gateway: budget_exceeded dimension=%s spent=%s cap=%s (call refused)",
		dec.Dimension, dec.Spent, dec.Cap)
}

// annotateSpan stamps the post-call budget attributes on the span. Per the spec
// the conversation dimension carries the primary spent/cap attrs; the agent
// dimension is added alongside when set so both are visible.
func (gp *gatewayProxy) annotateSpan(
	span trace.Span,
	caps budget.Caps,
	convSpent, agentSpent budget.Money,
	state budget.State,
	actual budget.Money,
) {
	attrs := []attribute.KeyValue{
		attribute.String("budget.state", string(state)),
		attribute.String("gateway.cost_usd", actual.String()),
	}
	if gp.convCap != nil && caps.ConversationID != "" {
		attrs = append(attrs,
			attribute.String("budget.conversation.spent_usd", convSpent.String()),
			attribute.String("budget.conversation.cap_usd", gp.convCap.String()),
		)
	}
	if gp.agentCap != nil && caps.AgentName != "" {
		attrs = append(attrs,
			attribute.String("budget.agent.spent_usd", agentSpent.String()),
			attribute.String("budget.agent.cap_usd", gp.agentCap.String()),
		)
	}
	span.SetAttributes(attrs...)
}

// routeFromRequest returns the estimate bucket key for a request: the agent name
// (from AGENT_NAME env). The last-observed-cost estimate is per-agent, which is
// the natural granularity — a single agent's calls share a cost profile, and the
// mock's cost is uniform, so this keeps the pre-call estimate deterministic
// without ever parsing the opaque request body (§12.5). It never reads the body
// (which forward() streams straight through to LiteLLM).
func (gp *gatewayProxy) routeFromRequest(_ *http.Request) string {
	return gp.cfg.AgentName
}

// ── header plumbing ─────────────────────────────────────────────────────────

// budgetHeaderSet is the set of launcher-internal headers that must NOT be
// forwarded to LiteLLM.
var budgetHeaderSet = map[string]struct{}{
	strings.ToLower(hdrConversationID): {},
	strings.ToLower(hdrAgentName):      {},
	strings.ToLower(hdrBudgetConvUSD):  {},
	strings.ToLower(hdrBudgetAgentUSD): {},
	strings.ToLower(hdrBudgetSoftPct):  {},
}

// copyForwardHeaders copies request headers to the upstream request, dropping the
// launcher-internal budget/identity headers.
func copyForwardHeaders(dst, src http.Header) {
	for k, vv := range src {
		if _, internal := budgetHeaderSet[strings.ToLower(k)]; internal {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// copyHeaders copies all response headers verbatim.
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// singleJoiningPath joins a base path and a request path with exactly one slash,
// mirroring httputil.NewSingleHostReverseProxy's path handling.
func singleJoiningPath(a, b string) string {
	aSlash := strings.HasSuffix(a, "/")
	bSlash := strings.HasPrefix(b, "/")
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash:
		if a == "" {
			return b
		}
		return a + "/" + b
	}
	return a + b
}
