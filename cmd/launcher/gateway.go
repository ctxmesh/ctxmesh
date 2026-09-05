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
// launcher listener — beside the :2998 memory endpoint and the :2997 AMP
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
	"bytes"
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

	"github.com/ctxmesh/ctxmesh/internal/gateway/budget"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
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
	// on the COST dimension (§14). It is deliberately the same string the AMP hop
	// guard uses (agent-mesh.md errBudgetExceeded) — both are "budget_exceeded",
	// the response's "dimension" field distinguishes cost (conversation|agent)
	// from the hop guard.
	errBudgetExceededCost = "budget_exceeded"

	// defaultSoftPct mirrors the CRD default (BudgetSpec.softThresholdPct=80) so a
	// missing/blank X-Budget-Soft-Pct still enforces a sane soft threshold.
	defaultSoftPct = 80

	// dimensionUser is the budgetErrorBody "dimension" for a per-END-USER (OBO) quota
	// denial (m66.7), distinguishing it from the "tenant"/cost denials a client parses
	// the same way.
	dimensionUser = "user"

	// errRunCancelled is the typed error code for the m70.8 real-kill refusal: the run was
	// cancelled, so the launcher refuses the model call (409) instead of forwarding it.
	errRunCancelled = "run_cancelled"
	// dimensionRun is the budgetErrorBody "dimension" for the run-cancelled refusal, so a
	// client parses it like the other typed gateway refusals (tenant / user / cost).
	dimensionRun = "run"
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
	// AgentNamespace (POD_NAMESPACE) forms the unambiguous "<namespace>/<name>" agent identity the
	// M78 record fixture stamps as provenance (Fixture.Agent). Not load-bearing for budget/quota.
	AgentNamespace string
	// ConvCapUSD / AgentCapUSD are the raw budget-cap strings (BUDGET_PER_*_USD).
	// Empty ⇒ that dimension is unenforced.
	ConvCapUSD  string
	AgentCapUSD string
	// SoftPct is the soft-alert percentage (BUDGET_SOFT_PCT), default 80.
	SoftPct int

	// Tenant model quota (M47, ADR 0046). TenantID gates the tenant enforcement;
	// TenantBudgetUSD / TenantRPM are the aggregate caps; QuotaAddr is the shared
	// state-layer Valkey the caps accumulate in (TENANT_QUOTA_ADDR — every agent +
	// replica in the tenant coordinates on ONE bucket). Empty TenantID ⇒ untenanted.
	TenantID          string
	TenantBudgetUSD   string
	TenantRPM         int
	TenantMaxInFlight int
	QuotaAddr         string

	// StatelayerProxyURL (M53, ADR 0050 §8 phase 2): when set, the tenant quota ops
	// route through the state-layer proxy (which holds the Valkey credential + derives
	// the tenant from the launcher's pod token) instead of the direct QuotaAddr. Takes
	// precedence over QuotaAddr — dual-mode during the migration.
	StatelayerProxyURL string
	// PodTokenPath is the mounted projected SA-token the proxy authenticates (audience
	// statelayer-proxy). Defaults to defaultPodTokenPath when the proxy URL is set.
	PodTokenPath string

	// GuardrailPolicy (M66, ADR 0059 §8): the resolved GuardrailPolicy spec serialized to
	// JSON (GUARDRAIL_POLICY env). PRE-K3 delivery path — still honored for a policy passed
	// directly by env (unit tests, any un-migrated path). Its presence FORCES the proxy on so
	// the in-path guardrail engine (m66.3) can inspect model input/output. Empty ⇒ no guardrail
	// by env (the m66.2 controller fails a dangling/invalid ref closed, so this is only ever
	// set for a VALIDATED policy).
	GuardrailPolicy string
	// GuardrailPolicyFile (K3, ADR 0059 Fork-2): the path to the mounted, read-only ConfigMap
	// file carrying the resolved GuardrailPolicy JSON (GUARDRAIL_POLICY_FILE env). This is the
	// K3 delivery path: the launcher reads it at startup AND watches it (fsnotify), so a
	// GuardrailPolicy edit propagates WITHOUT a revision roll. Its presence FORCES the proxy on
	// (GatewayProxyEnabled) exactly as the env did — the interposition trigger now keys on the
	// mounted file, not the digest. Empty ⇒ no file wired (the pre-K3 env path applies).
	GuardrailPolicyFile string
	// BFFInternalURL is the BFF's cluster-internal URL (BFF_INTERNAL_URL env), used by the
	// durable guardrail block audit (m66.9): the launcher POSTs a PII-safe compliance record
	// to POST /api/internal/guardrail-event best-effort, async. Empty ⇒ the durable POST is
	// skipped; the span event emitted by emitGuardrailDecision remains the only record.
	// Same env the delegate client uses (delegate.go, BFFURL field) — one source of truth.
	BFFInternalURL string

	// RecordCapable (M78, ADR 0071 §1): true when the controller injected RECORD_CAPABLE=true
	// (spec.record) — the record-mode interposition reason. Its presence FORCES the proxy on
	// (GatewayProxyEnabled) so the gateway is interposed and CAN capture; the actual capture is
	// gated PER-RUN by the X-Ctxmesh-Record header the BFF stamps on a recorded run's invoke,
	// which the SDK relays on each model call. A record-capable agent whose object store is
	// unconfigured is a startup error (fail-closed, C2) — see newGatewayProxy. Empty ⇒ the
	// agent is not record-capable and the model path is byte-for-byte unchanged (no capture).
	RecordCapable bool
}

// defaultPodTokenPath is where the controller mounts the launcher's projected
// serviceAccountToken volume (must match the controller's mount path, M53).
const defaultPodTokenPath = "/var/run/secrets/statelayer-proxy/token"

// GatewayProxyEnabled reports whether the outbound gateway proxy should start.
// True iff an upstream gateway URL was injected AND there is a reason to interpose
// the proxy: at least one budget cap, a tenant quota, OR a guardrail policy (M66,
// ADR 0059 §8 — a guarded agent must route its LLM calls THROUGH the proxy so the
// in-path guardrail engine can inspect them). The guardrail reason fires on EITHER the
// pre-K3 GUARDRAIL_POLICY env OR the K3 mounted GUARDRAIL_POLICY_FILE (the interposition
// trigger now keys on the mounted file's presence, not the structural digest). With none
// of these the controller does not inject GATEWAY_UPSTREAM_URL, so the agent's
// MODEL_GATEWAY_URL keeps pointing straight at LiteLLM.
func (c Config) GatewayProxyEnabled() bool {
	g := c.Gateway
	return g.UpstreamURL != "" &&
		(g.ConvCapUSD != "" || g.AgentCapUSD != "" || g.TenantID != "" ||
			g.GuardrailPolicy != "" || g.GuardrailPolicyFile != "" || g.RecordCapable)
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
// Like the memory/AMP loaders it does not hard-fail on a blank cap or name when
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
		UpstreamURL:         strings.TrimRight(upstream, "/"),
		Port:                port,
		AgentName:           agentName,
		AgentNamespace:      strings.TrimSpace(lookup("POD_NAMESPACE")),
		ConvCapUSD:          strings.TrimSpace(lookup("BUDGET_PER_CONVERSATION_USD")),
		AgentCapUSD:         strings.TrimSpace(lookup("BUDGET_PER_AGENT_USD")),
		SoftPct:             softPct,
		TenantID:            strings.TrimSpace(lookup("TENANT_ID")),
		TenantBudgetUSD:     strings.TrimSpace(lookup("TENANT_BUDGET_USD")),
		TenantRPM:           positiveIntEnv(lookup, "TENANT_RPM"),
		TenantMaxInFlight:   positiveIntEnv(lookup, "TENANT_MAX_CONCURRENT"),
		QuotaAddr:           strings.TrimSpace(lookup("TENANT_QUOTA_ADDR")),
		StatelayerProxyURL:  strings.TrimSpace(lookup("STATELAYER_PROXY_URL")),
		PodTokenPath:        strings.TrimSpace(lookup("STATELAYER_TOKEN_PATH")),
		GuardrailPolicy:     strings.TrimSpace(lookup("GUARDRAIL_POLICY")),
		GuardrailPolicyFile: strings.TrimSpace(lookup("GUARDRAIL_POLICY_FILE")),
		BFFInternalURL:      strings.TrimRight(strings.TrimSpace(lookup("BFF_INTERNAL_URL")), "/"),
		RecordCapable:       strings.EqualFold(strings.TrimSpace(lookup("RECORD_CAPABLE")), "true"),
	}, nil
}

// positiveIntEnv parses a positive integer env var, returning 0 when unset/blank/invalid/≤0.
func positiveIntEnv(lookup func(string) string, name string) int {
	if raw := strings.TrimSpace(lookup(name)); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

// budgetErrorBody is the typed budget_exceeded response body (§14). spent/cap are
// exact-decimal strings.
type budgetErrorBody struct {
	Error     string `json:"error"`
	Dimension string `json:"dimension"`
	Spent     string `json:"spent"`
	Cap       string `json:"cap"`
}

// controlStore reads a run's CONTROL verb from the pod-authed state-layer proxy (m70.8, the real-kill
// cancel channel). The httpTenantStore (the proxy client) satisfies it. An interface so the gateway
// unit-tests the cancel enforcement against a fake without a live proxy.
type controlStore interface {
	// Control returns the run's control verb ("cancel" or ""), or an error on a transport/proxy failure
	// (the gateway then fails OPEN — no verb ⇒ no cancel).
	Control(ctx context.Context, runID string) (string, error)
}

// controlVerbCancel is the ONLY control verb the gateway acts on in v1 (real-kill). An unknown verb
// (a future nudge / take-over) is ignored — the marker is a verb channel, so a newer BFF verb never
// breaks an older launcher.
const controlVerbCancel = "cancel"

// controlPollInterval is how often the mid-call abort goroutine re-checks the control marker while a
// model call is in flight (m70.8). ~1s is responsive enough to abort a long streaming call promptly
// without hammering the proxy; the goroutine exits the moment the request finishes (ctx.Done()).
const controlPollInterval = time.Second

// gatewayProxy is the outbound gateway proxy handler. It holds the shared,
// process-wide enforcer/estimator (spend accumulates across every request) and
// forwards to the upstream LiteLLM gateway.
type gatewayProxy struct {
	cfg       gatewayConfig
	upstream  *url.URL
	enforcer  *budget.Enforcer
	estimator *budget.Estimator
	client    *http.Client
	// streamClient forwards the K2 (ADR 0086) streaming path. It has NO overall Timeout — a
	// client-level deadline would abort a long SSE response mid-stream — so termination is driven
	// by the request context (the m70.8 mid-call abort cancels fwdCtx). Same redirect refusal.
	streamClient *http.Client
	tracer       trace.Tracer
	// caps is the per-process budget derived once from the config (the caps are
	// static — the same for every request this launcher serves). Parsed at
	// construction so a malformed cap fails fast at startup, not per request.
	convCap  *budget.Money
	agentCap *budget.Money
	// tenant enforces the M47 tenant model quota (rate + aggregate budget) against
	// the shared Valkey. nil ⇒ untenanted or no tenant model caps.
	tenant *tenantQuota
	// agentSpend books this agent's post-call model spend onto a durable per-agent monthly Valkey key
	// (agent:{ns}/{name}:spend:{YYYY-MM}), ADDITIVE to the tenant key at the SAME accounting point (m84.6,
	// ADR 0063 D1 follow-up). It is a durable cost-rollup SOURCE (the cost-rollup worker snapshots it),
	// NOT an enforcement cap. nil ⇒ off (no direct Valkey addr, or an unnamed agent) — a nil-safe no-op.
	agentSpend *agentSpendAccountant
	// policy holds the three POLICY-DERIVED, RUNTIME-RELOADABLE pieces — the guardrail
	// engine, the fenced LLM-judge, and the per-user quota — behind an RWMutex so a
	// GuardrailPolicy edit can atomically swap the whole bundle WITHOUT a revision roll
	// (K3, ADR 0059 Fork-2). The request path reads them via the accessor methods
	// (guardrailEngine()/semanticJudge()/userQuota()), which take the read lock; the
	// fsnotify watcher (guardrail_reload.go) swaps the bundle under the write lock. Every
	// piece is derived from the SAME policy JSON, so they always move together.
	policy guardrailHolder
	// capVerifier verifies the inbound run capability to resolve the invoking user's hashed
	// id (m66.7). Built from the same MCP_CAPABILITY_PUBLIC_KEY / MCP_CAPABILITY_AUDIENCE the
	// OBO egress path uses. nil ⇒ no key provisioned: per-user enforcement fails OPEN (skipped)
	// even when a userRateLimit is set — a missing verifier is treated like a missing capability.
	// It depends ONLY on process env (the OBO key), never on the policy, so it is NOT reloaded —
	// it is built once at construction and read directly.
	capVerifier *runcap.Verifier
	// control reads a run's CONTROL verb from the pod-authed state-layer proxy (m70.8, the real-kill
	// cancel channel). Set ONLY when the launcher runs the state-layer-proxy path (STATELAYER_PROXY_URL):
	// the httpTenantStore is the proxy client that already holds the pod token. nil in the direct-Valkey
	// legacy mode or when unconfigured ⇒ the cancel-check is a NO-OP (the model-call path is unchanged) —
	// the real kill is an ACCELERATOR layered onto the durable status-flip cancel, never a hard dependency.
	control controlStore
	// bffInternalURL is BFF_INTERNAL_URL (process-level, from gatewayConfig). Used by the durable
	// guardrail block audit (m66.9, ADR 0059 §9): the launcher POSTs a PII-safe compliance record
	// to the BFF's ingest endpoint best-effort, async. Empty ⇒ the durable POST is skipped; the
	// span event (emitted by emitGuardrailDecision) remains the only record.
	bffInternalURL string
	// recorder is the M78 record-mode capture (ADR 0071 §1). Non-nil ONLY for a RECORD-CAPABLE agent
	// (RECORD_CAPABLE=true); it accumulates per-run model interactions and Puts a per-run fixture to
	// the durable object store. nil ⇒ record mode is off and the capture path is a no-op (zero
	// overhead, the model path byte-for-byte unchanged). Capture is further gated PER-RUN by the
	// X-Ctxmesh-Record header on each model call (a non-recorded run through a record-capable agent
	// captures nothing).
	recorder *modelRecorder
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
	// Runtime-reloadable GuardrailPolicy (K3, ADR 0059 Fork-2): when the controller mounted the
	// policy as a ConfigMap file, watch it so an edit propagates to THIS running agent without a
	// revision roll. The watcher runs for the process lifetime (it exits when the launcher exits);
	// a watcher-init failure is logged and the initial policy stays active (fixed, not reloadable).
	if cfg.Gateway.GuardrailPolicyFile != "" {
		go gp.watchGuardrailPolicy(cfg.Gateway.GuardrailPolicyFile, nil)
	}
	return &http.Server{
		Addr:    loopbackAddr(cfg.Gateway.Port),
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
		cfg:            cfg,
		upstream:       u,
		enforcer:       budget.NewEnforcer(),
		estimator:      budget.NewEstimator(),
		client:         &http.Client{Timeout: gatewayRequestTimeout, CheckRedirect: refuseRedirect},
		streamClient:   newStreamClient(), // no overall timeout (K2); a header + idle deadline (K9)
		tracer:         tracer,
		bffInternalURL: cfg.BFFInternalURL,
		logf:           logf,
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

	// Tenant model quota (M47/M53): enforce against the SHARED accumulator so every replica coordinates.
	// The STATE-LAYER PROXY (M53, ADR 0050 §8 phase 2) takes precedence over the direct Valkey: it holds
	// the credential + derives the tenant from this launcher's pod token, so the agent holds no Valkey
	// credential. Falls back to the direct QuotaAddr during the migration; neither set ⇒ log loudly and
	// leave quota unenforced (a visible misconfig — the controller injects one when caps exist).
	if cfg.TenantID != "" {
		tq := &tenantQuota{id: cfg.TenantID, rpm: cfg.TenantRPM, maxConcurrent: cfg.TenantMaxInFlight, logf: logf}
		if cfg.TenantBudgetUSD != "" {
			tq.budgetUSD = moneyToFloat(cfg.TenantBudgetUSD)
			tq.hasBudget = tq.budgetUSD > 0
		}
		switch {
		case cfg.StatelayerProxyURL != "":
			tq.store = newHTTPTenantStore(cfg.StatelayerProxyURL, resolvePodTokenPath(cfg.PodTokenPath))
			gp.tenant = tq
		case cfg.QuotaAddr != "":
			tq.store = newRedisTenantStore(cfg.QuotaAddr)
			gp.tenant = tq
		default:
			logf("launcher: gateway: tenant %s has model caps but no STATELAYER_PROXY_URL or "+
				"TENANT_QUOTA_ADDR — quota NOT enforced", cfg.TenantID)
		}
	}

	// Per-agent durable cost rollup (m84.6): book post-call spend onto a per-agent monthly Valkey key
	// alongside the tenant key, so the cost-rollup worker can snapshot a durable per-agent spend series.
	// nil (off) when the agent is unnamed or no direct Valkey addr is configured — exactly like the
	// per-user quota; never blocks a model call.
	gp.agentSpend = newAgentSpendAccountant(cfg, logf)

	// Real-kill control channel (m70.8): the launcher polls the pod-authed /control endpoint to abort a
	// cancelled run's in-flight model call. It is available ONLY on the state-layer-proxy path (which gives
	// the launcher a pod-token reach to the proxy) — and it is INDEPENDENT of the tenant quota, so a
	// proxied-but-untenanted agent (guardrail-only, or no caps) is still real-killable. With no proxy URL
	// (direct-Valkey legacy mode or unconfigured) gp.control stays nil ⇒ the cancel-check is a no-op and the
	// model-call path is byte-for-byte unchanged (the durable status-flip cancel still applies).
	if cfg.StatelayerProxyURL != "" {
		// F2 (ADR 0099): ENFORCE the per-agent/per-conversation budget against the durable statelayer-proxy
		// (real across replicas; survives restarts) instead of the per-replica in-memory total. Fail-closed.
		tenantStore := newHTTPTenantStore(cfg.StatelayerProxyURL, resolvePodTokenPath(cfg.PodTokenPath))
		gp.enforcer = budget.NewEnforcerWithBackend(tenantStore, logf)
		gp.control = newHTTPTenantStore(cfg.StatelayerProxyURL, resolvePodTokenPath(cfg.PodTokenPath))
	}

	// The run-capability verifier resolves the invoking user's hashed id (per-user quota,
	// m66.7) AND the trusted run id (real-kill cancel, m70.8). It depends ONLY on the OBO
	// capability key (process env), never on the guardrail policy, so it is built ONCE here and
	// is NOT part of the reloadable bundle — a policy edit must not disturb the real-kill path.
	gp.capVerifier = buildCapVerifier(logf)

	// Guardrail policy (M66 / K3): the INITIAL policy comes from the mounted file when the
	// controller wired one (GUARDRAIL_POLICY_FILE), else from the GUARDRAIL_POLICY env
	// (byte-compatible pre-K3 fallback — used by unit tests and any un-migrated path). Build the
	// three policy-derived pieces (engine + judge + per-user quota) into the swappable bundle.
	// FAIL-CLOSED at startup: a malformed/uncompilable policy is a HARD construction error, exactly
	// as pre-K3 — the controller already validated it (m66.2), so this is defence-in-depth and the
	// non-reload path keeps its fail-closed load. The RELOAD path (guardrail_reload.go) is
	// keep-last-good on a bad edit; only startup fails hard.
	initialPolicy := cfg.GuardrailPolicy
	if cfg.GuardrailPolicyFile != "" {
		fileJSON, ferr := readGuardrailPolicyFile(cfg.GuardrailPolicyFile)
		if ferr != nil {
			return nil, fmt.Errorf("gateway: reading GUARDRAIL_POLICY_FILE %q: %w", cfg.GuardrailPolicyFile, ferr)
		}
		initialPolicy = fileJSON
	}
	bundle, err := buildGuardrailBundle(initialPolicy, cfg, logf)
	if err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
	gp.policy.store(bundle)

	// Record mode (M78, ADR 0071 §1): build the per-run model recorder for a RECORD-CAPABLE agent.
	// FAIL-CLOSED (C2): record was requested (RECORD_CAPABLE=true) but the durable object store is
	// not configured (OBJECT_STORE_ADDR unset ⇒ no sink for the fixture) is a HARD startup error —
	// never a silent capture-nothing. A non-record-capable agent gets no recorder (nil), zero overhead.
	if cfg.RecordCapable {
		rec, rerr := newModelRecorder(cfg.AgentName, cfg.AgentNamespace, logf)
		if rerr != nil {
			return nil, fmt.Errorf("gateway: record mode requested but not usable: %w", rerr)
		}
		gp.recorder = rec
	}

	return gp, nil
}

// buildUserQuota constructs the per-user (OBO) quota enforcer from the guardrail policy's
// userRateLimit (M66, ADR 0059 §8). It returns (nil, nil) when there is no userRateLimit —
// per-user enforcement is simply off. A malformed policy is a hard error (the load path is
// fail-closed, matching newGuardrailEngine). The per-user quota shares the tenant quota's
// Valkey (TENANT_QUOTA_ADDR); with no direct Valkey addr (or the state-layer-proxy-only path) the
// per-user store cannot be built, so enforcement stays OFF (fail-open) with a loud log — a missing
// accumulator must never block model calls.
//
// It takes the policy JSON as a parameter (not cfg.GuardrailPolicy) so the K3 RELOAD path can
// rebuild the per-user quota from the freshly-mounted policy. The run-capability verifier is built
// separately (buildCapVerifier) because it depends only on process env, not the policy.
func buildUserQuota(policyJSON string, cfg gatewayConfig, logf func(string, ...any)) (*userQuota, error) {
	limit, err := parseUserRateLimit(policyJSON)
	if err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
	if limit == nil {
		return nil, nil
	}

	uq := &userQuota{rpm: limit.RequestsPerMinute, maxConcurrent: limit.MaxInFlight, logf: logf}
	if limit.SpendUSD != "" {
		uq.budgetUSD = moneyToFloat(limit.SpendUSD)
		uq.hasBudget = uq.budgetUSD > 0
	}

	// The per-user accumulator lives in the SAME shared Valkey the tenant quota uses (direct mode) or is
	// proxied through the state-layer proxy (proxy mode). Store selection mirrors newAgentSpendAccountant:
	// the proxy takes precedence — the state-layer proxy now serves the per-user endpoints (M107 C20),
	// passing the userHash in the request body (the pod token identifies the agent; the proxy stores the
	// user hash the launcher resolved from the verified run capability). Without either address leave
	// enforcement OFF (a visible misconfig), never silently block.
	switch {
	case cfg.StatelayerProxyURL != "":
		uq.store = newHTTPUserStore(cfg.StatelayerProxyURL, resolvePodTokenPath(cfg.PodTokenPath))
	case cfg.QuotaAddr != "":
		uq.store = newRedisUserStore(cfg.QuotaAddr)
	default:
		logf("launcher: gateway: userRateLimit configured but no STATELAYER_PROXY_URL / " +
			"TENANT_QUOTA_ADDR — per-user limits NOT enforced")
		return nil, nil
	}
	return uq, nil
}

// buildCapVerifier builds the run-capability verifier from the OBO capability public key +
// audience the controller provisions for the egress path (m66.7 per-user quota, m70.8 real-kill).
// It depends ONLY on process env, NEVER on the guardrail policy, so it is built ONCE at
// construction and is not part of the reloadable bundle. Without a key we cannot TRUST any user/run
// id: the per-user quota and the cancel-check both fail OPEN (skipped) rather than fail-closed,
// which would break every guarded call whose capability didn't propagate (ADR 0059 §8).
func buildCapVerifier(logf func(string, ...any)) *runcap.Verifier {
	pubB64 := strings.TrimSpace(os.Getenv("MCP_CAPABILITY_PUBLIC_KEY"))
	if pubB64 == "" {
		return nil
	}
	pub, derr := runcap.DecodePublicKey(pubB64)
	if derr != nil {
		logf("launcher: gateway: bad MCP_CAPABILITY_PUBLIC_KEY (%v) — per-user limits / cancel fail OPEN (skipped)", derr)
		return nil
	}
	return runcap.NewVerifier(pub, strings.TrimSpace(os.Getenv("MCP_CAPABILITY_AUDIENCE")), nil)
}

// handler returns the HTTP handler for the proxy listener.
func (gp *gatewayProxy) handler() http.Handler {
	return http.HandlerFunc(gp.serve)
}

// capsFor builds the per-request Caps.
//
//   - ConversationID is read from the X-Conversation-Id header the AGENT stamps
//     on its outbound LLM call — the SAME documented platform convention the
//     memory (:2998) and AMP (:2997) paths already use (conversationIDFromRequest).
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

	// Snapshot the reloadable guardrail bundle ONCE per request (K3): the engine, judge, and
	// per-user quota are all read from this single consistent bundle, so a concurrent policy
	// reload can never split a request across an old+new policy (an atomic swap replaces the
	// whole bundle; this request finishes on whichever bundle it snapshotted).
	pol := gp.policy.load()

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
	// Estimate is conservative: the last observed cost on this route. It feeds BOTH the M8 per-agent
	// budget and the M47 tenant aggregate budget.
	est := gp.estimator.Estimate(route)
	// M8 per-conversation/agent budget — only when a dimension is enforceable (cap set + identity key).
	if caps.Enforced() {
		if dec := gp.enforcer.PreCall(ctx, caps, est); !dec.Allowed {
			gp.writeBudgetExceeded(w, span, dec)
			return
		}
	}
	// M47 tenant model quota (rate + aggregate budget + concurrency), independent of the M8 caps (nil-safe).
	// releaseSlot frees the concurrency slot when this call finishes (noop when no slot was taken).
	deny, releaseSlot := gp.tenant.preCall(ctx, moneyToFloat(est.String()))
	if deny != nil {
		gp.writeTenantDeny(w, span, deny)
		return
	}
	defer releaseSlot()

	// M66 per-END-USER (OBO) model quota (rate + monthly budget + concurrency), independent of the tenant
	// caps. The invoking user's hashed id is resolved from the VERIFIED run capability (userHashFromRequest);
	// a missing/forged/unverifiable capability yields "" ⇒ per-user enforcement is SKIPPED (fail-open, ADR
	// 0059 §8) — the call is still bounded by gateway auth + the guardrail content pipeline + the tenant
	// quota. releaseUser frees the per-user concurrency slot on finish (noop when none was taken / userHash "").
	userHash := gp.userHashFromRequest(r, pol.user)
	if userHash != "" {
		span.SetAttributes(attribute.String("user.quota.subject", userHash))
	}
	userDeny, releaseUser := pol.user.preCall(ctx, userHash, moneyToFloat(est.String()))
	if userDeny != nil {
		gp.writeUserDeny(w, span, userDeny)
		return
	}
	defer releaseUser()

	// ── Guardrail request scan (M66, ADR 0059 §8) ──────────────────────────
	// Only when a policy is active. Buffers the request body (fail-closed on oversize
	// or unparseable), scans the user-role input AND tool-role tool-output against
	// block/redact/auditOnly rules, refuses a block hit with a typed guardrail_blocked
	// BEFORE forwarding, and forwards the SCRUBBED body on a redact hit. With no policy
	// (guardrail nil) this is skipped entirely and the body streams unchanged.
	// A recorded run is buffered-only (the fixture captures a single reassembled body), so it may
	// never take the streaming path — streaming is allowed only for an eligible policy AND a
	// non-recorded run. streaming is set true by the request guardrail when this stream:true call
	// is permitted; serve() then routes it to the SSE hold-release path (K2, ADR 0086).
	recording := gp.recorder != nil && recordRunIDFromRequest(r.Header) != ""
	streaming := false
	if pol.engine != nil {
		refused, isStream := gp.applyRequestGuardrail(w, span, r, pol, pol.streamEligible && !recording)
		if refused {
			return
		}
		streaming = isStream
	}

	// ── Real-kill cancel channel (m70.8) ───────────────────────────────────
	// Resolve the TRUSTED run id from the verified capability (fail-open to "" — no trusted run id ⇒ no
	// cancel enforcement, the model-call path is unchanged). PRE-CALL check: if the run's control marker
	// says "cancel", REFUSE the call BEFORE forwarding so the agent's model call errors and its loop
	// unwinds — the real kill at call-boundary granularity. gp.control is nil off the state-layer-proxy
	// path, so this whole block is a no-op there.
	runID := gp.extractRunID(r)
	if runID != "" {
		span.SetAttributes(attribute.String("run.id", runID))
		if gp.controlIsCancel(ctx, runID) {
			gp.writeRunCancelled(w, span, runID)
			return
		}
	}

	// Mid-call abort (m70.8, the richer kill): while a (possibly long/streaming) model call is in flight,
	// poll the control marker and abort the forward the moment it flips to "cancel". Wrap the forward ctx
	// in a cancel func and spawn a poller that exits when the request finishes (fwdCtx.Done()) — so the
	// goroutine can never outlive the call. A no-op when there is no trusted run id / no control store.
	fwdCtx := ctx
	if runID != "" {
		var cancelFwd context.CancelFunc
		fwdCtx, cancelFwd = context.WithCancel(ctx)
		defer cancelFwd()
		go gp.pollControlAbort(fwdCtx, runID, cancelFwd)
	}

	// ── Record mode (M78, ADR 0071 §1): buffer the request BODY before forward ──
	// When this call belongs to a RECORDED run (the record-capable agent got a X-Ctxmesh-Record:
	// <runId> relayed on the model call), buffer the request body here so the fixture can capture the
	// agent-visible request (JSON — NO credential; the gateway Authorization header is never
	// captured, C4). forward() drains r.Body, so we buffer + restore it (the same pattern the
	// guardrail path uses) so the forward still streams the exact bytes. recordRunID == "" for a
	// non-recorded run through this same agent ⇒ zero overhead, no buffering.
	recordRunID := ""
	var recordReqBody []byte
	if gp.recorder != nil {
		if runID := recordRunIDFromRequest(r.Header); runID != "" {
			recordRunID = runID
			buffered, _, berr := readLimited(r.Body, maxGatewayReqBody)
			if r.Body != nil {
				_ = r.Body.Close()
			}
			if berr == nil {
				recordReqBody = recordRequestBody(buffered)
				r.Body = io.NopCloser(bytes.NewReader(buffered))
			} else {
				// Could not buffer the request — capture the response only (request body omitted).
				// Never fail the model call on a record-path read error.
				gp.logf("launcher: gateway: record: could not buffer request body for run %s: %v", recordRunID, berr)
				r.Body = io.NopCloser(bytes.NewReader(nil))
			}
		}
	}

	// ── F1 (M126): stream the NON-guardrailed interposed class VERBATIM ─────
	// A budget/tenant/record-off agent's stream:true call previously fell through to the buffered
	// forward below (a 60s abort, a 4MiB truncation, and $0 spend on truncation). Route it through the
	// verbatim relay instead. A guardrailed call already set `streaming` (serveStreaming, below); a
	// recorded run stays buffered — the recorded stream:true 60s/4MiB wall is carded m52.G19.
	if !streaming && pol.engine == nil && recordRunID == "" && requestIsStream(r) {
		gp.serveStreamingVerbatim(fwdCtx, w, span, r, pol, caps, route, userHash)
		return
	}

	// ── Streaming guarded path (K2, ADR 0086) ──────────────────────────────
	// A permitted stream:true call goes to the SSE hold-release path: it forwards with streaming,
	// scans the completion token-by-token, and releases clean tokens as they settle (blocking
	// BEFORE an offending span is released). It books spend from the final usage chunk, so the
	// tenant/user/agent accounting stays at parity with the buffered path. recording is false here
	// by construction (allowStream required !recording), so record capture below is skipped.
	if streaming {
		gp.serveStreaming(fwdCtx, w, span, r, pol, caps, route, userHash)
		return
	}

	// ── Forward to LiteLLM ─────────────────────────────────────────────────
	resp, body, err := gp.forward(fwdCtx, r)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "gateway upstream error")
		http.Error(w, fmt.Sprintf("gateway upstream error: %v", err), http.StatusBadGateway)
		return
	}

	// ── Guardrail output scan (M66, ADR 0059 §8) ────────────────────────────
	// Scan the completion (choices[].message.content) against the output rules. A block
	// SUBSTITUTES a guardrail_blocked body for the completion (the client never sees it);
	// a redact relays the completion with [REDACTED:<name>]. relayBody is what the client
	// receives; spend is still booked below on a block, since the model already generated
	// (ADR 0059 §7). Only runs under an active policy with output rules — otherwise
	// relayBody == body (the response is relayed verbatim).
	relayBody := body
	outputBlocked := false
	if pol.engine != nil {
		relayBody, outputBlocked = gp.applyOutputGuardrail(ctx, span, r, body, pol)
	}

	// Relay the (possibly substituted/redacted) response. An output block overrides the
	// upstream status with 403 guardrail_blocked so the client sees a typed refusal, not a
	// 200 with a withheld body; otherwise the upstream status/headers are relayed as-is.
	if outputBlocked {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(guardrailBlockedStatus) // 403
	} else {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
	}
	if _, werr := w.Write(relayBody); werr != nil {
		gp.logf("launcher: gateway: write response: %v", werr)
	}

	// ── Record mode (M78, ADR 0071 §1): capture the model interaction ──────
	// Capture the exact bytes the AGENT received — the RELAYED response (post any guardrail
	// redact/block), its relayed status + Content-Type — so a replay re-serves what the run really
	// saw. The bytes are captured VERBATIM incl. SSE framing (forward() buffered them raw; we never
	// parse-and-reassemble). Only for a recorded run (recordRunID != ""); the request body was
	// buffered above (credential-free by construction, C4). Best-effort: a store failure is logged,
	// never surfaced to the agent.
	if recordRunID != "" {
		relayStatus := resp.StatusCode
		relayContentType := resp.Header.Get("Content-Type")
		if outputBlocked {
			relayStatus = guardrailBlockedStatus
			relayContentType = "application/json"
		}
		gp.recorder.capture(ctx, recordRunID, recordReqBody, relayBody, relayContentType, relayStatus)
	}

	// ── POST-CALL accounting ───────────────────────────────────────────────
	// Only book cost for a successful completion (a 4xx/5xx from LiteLLM cost the
	// tenant nothing and must not accrue spend). An OUTPUT BLOCK still books spend: the
	// model already generated, so accounting runs on the ORIGINAL upstream body/usage
	// (ADR 0059 §7) even though the completion was withheld from the client. Price prefers
	// LiteLLM's own cost header; falls back to the deterministic token table.
	if resp.StatusCode != http.StatusOK {
		return
	}
	// A non-guardrailed stream:true call is buffered here (serve routes to serveStreaming only under an
	// active policy), so `body` may be the raw SSE stream — usageBodyForPricing extracts the usage chunk
	// so streaming agents book spend (m122.1); a plain completion body passes through unchanged.
	actual := budget.PriceCall(resp.Header.Get(budget.LiteLLMCostHeader), usageBodyForPricing(body))
	gp.bookSpend(ctx, span, caps, route, userHash, pol, actual)
}

// bookSpend runs the POST-CALL accounting for a priced completion — the SHARED path for both the
// buffered (serve) and streaming (serveStreaming) responses, so per-tenant / per-user / per-agent
// spend and the M8 budget enforcement stay byte-for-byte at parity across the two. An OUTPUT BLOCK
// still books spend (the model already generated; ADR 0059 §7); the caller passes actual=$0 for an
// unpriceable call. All the postCall sinks are nil-safe. Never call this on a non-200 upstream.
func (gp *gatewayProxy) bookSpend(
	ctx context.Context, span trace.Span, caps budget.Caps, route, userHash string,
	pol *guardrailBundle, actual budget.Money,
) {
	gp.estimator.Observe(route, actual)
	// M47: accrue the tenant's aggregate spend to the shared Valkey (nil-safe; only when a tenant budget
	// is set). Runs even when the M8 per-agent budget is not enforced for this agent.
	gp.tenant.postCall(ctx, moneyToFloat(actual.String()))
	// m84.6: ADDITIVELY book the SAME delta onto this agent's durable per-agent monthly Valkey key
	// (a SEPARATE key — the tenant key above is untouched) so the cost-rollup worker can snapshot a
	// durable per-agent spend series. nil-safe; independent of any budget cap.
	gp.agentSpend.postCall(ctx, moneyToFloat(actual.String()))
	// M66: accrue the invoking user's monthly spend (nil-safe; only when a per-user budget is set AND the
	// capability resolved a userHash — a missing/forged capability booked no user, so nothing accrues).
	// Uses the SAME per-request bundle snapshot as the pre-call so the pre/post pair is symmetric even
	// across a concurrent reload.
	pol.user.postCall(ctx, userHash, moneyToFloat(actual.String()))

	if !caps.Enforced() {
		return
	}
	convSpent, agentSpent, state, alert := gp.enforcer.PostCall(ctx, caps, actual)
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

// writeTenantDeny emits the typed tenant-quota rejection (402 tenant_budget_exceeded / 429
// tenant_rate_limited, dimension "tenant") and marks the span. The provider is NOT called (M47
// circuit-break). Reuses budgetErrorBody so a caller parses tenant + per-agent denials the same way.
func (gp *gatewayProxy) writeTenantDeny(w http.ResponseWriter, span trace.Span, deny *tenantDeny) {
	span.SetAttributes(
		attribute.String("tenant.quota.code", deny.code),
		attribute.Int("tenant.quota.status", deny.status),
	)
	span.SetStatus(codes.Error, deny.code)

	body := budgetErrorBody{Error: deny.code, Dimension: "tenant"}
	if deny.status == http.StatusPaymentRequired {
		body.Spent = strconv.FormatFloat(deny.spent, 'f', -1, 64)
		body.Cap = strconv.FormatFloat(deny.capUSD, 'f', -1, 64)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(deny.status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		gp.logf("launcher: gateway: encode tenant deny: %v", err)
	}
	gp.logf("launcher: gateway: %s (tenant=%s status=%d, call refused)", deny.code, gp.tenant.id, deny.status)
}

// userHashFromRequest resolves the invoking user's ALREADY-HASHED id from the inbound run capability
// (the X-Ctxmesh-Run-Capability header the SDK relays on the model call, m66.7). It returns "" — meaning
// "no trusted user, skip per-user enforcement" (FAIL-OPEN, ADR 0059 §8) — whenever there is nothing to
// enforce or nothing to trust:
//   - no per-user quota configured, or no verifier provisioned;
//   - the header is absent (an unattended/offline run, or the capability did not propagate);
//   - the capability does not VERIFY (bad signature / expired / wrong audience — a FORGED token is treated
//     exactly like an absent one, so it can NEVER inject a spoofed userHash);
//   - the capability verifies but carries no user id.
//
// Rate-limiting is availability plumbing: a missing/unverifiable identity must NOT fail the model call
// (that would break every guarded call whose capability didn't propagate). The call stays bounded by the
// gateway auth + the guardrail content pipeline + the tenant quota.
func (gp *gatewayProxy) userHashFromRequest(r *http.Request, user *userQuota) string {
	if user == nil || gp.capVerifier == nil {
		return ""
	}
	token := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if token == "" {
		return ""
	}
	capability, err := gp.capVerifier.Verify(token)
	if err != nil {
		// A forged/expired/wrong-audience capability is NOT trusted — treat it as absent (fail-open per-user)
		// and never as a user. It cannot grant a spoofed userHash.
		gp.logf("launcher: gateway: run capability failed verification (per-user limits skipped, fail-open): %v", err)
		return ""
	}
	return capability.User
}

// extractRunID resolves the TRUSTED run id from the inbound run capability (the X-Ctxmesh-Run-Capability
// header the SDK relays on the model call), parallel to userHashFromRequest (m70.8, the real-kill cancel
// channel). It returns "" — meaning "no trusted run id, skip cancel enforcement" (FAIL-OPEN) — whenever
// there is nothing to trust:
//   - no control store (not the state-layer-proxy path), or no capability verifier provisioned;
//   - the capability header is absent (an unattended run, or it did not propagate);
//   - the capability does not VERIFY (bad signature / expired / wrong audience — a FORGED token is treated
//     exactly like an absent one, so it can NEVER inject a spoofed run id to cancel someone else's run);
//   - the capability verifies but carries no run id.
//
// The run id is taken from the VERIFIED capability (not a raw header), so an agent cannot forge a run id to
// probe/cancel another run's marker: it can only ever read its OWN run's control verb.
func (gp *gatewayProxy) extractRunID(r *http.Request) string {
	if gp.control == nil || gp.capVerifier == nil {
		return ""
	}
	token := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if token == "" {
		return ""
	}
	capability, err := gp.capVerifier.Verify(token)
	if err != nil {
		// A forged/expired capability is NOT trusted — treat it as absent (no cancel enforcement). It
		// cannot grant a spoofed run id.
		gp.logf("launcher: gateway: run capability failed verification (cancel-check skipped, fail-open): %v", err)
		return ""
	}
	return capability.RunID
}

// controlIsCancel reports whether the run's CONTROL marker says "cancel" (m70.8). It FAILS OPEN: an empty
// run id (no trusted capability), a nil control store, or ANY proxy/transport error → false (do NOT cancel)
// — a control-plane blip must never spuriously kill a live model call. Only the exact verb "cancel" trips
// it; an unknown/future verb is ignored.
func (gp *gatewayProxy) controlIsCancel(ctx context.Context, runID string) bool {
	if gp.control == nil || runID == "" {
		return false
	}
	verb, err := gp.control.Control(ctx, runID)
	if err != nil {
		gp.logf("launcher: gateway: control check failed (fail-open, call proceeds): %v", err)
		return false
	}
	return verb == controlVerbCancel
}

// pollControlAbort polls the run's control marker every controlPollInterval while a model call is in
// flight and calls cancelFwd on the FIRST "cancel" — aborting a long/streaming in-flight call at the
// transport (m70.8, the richer real-kill). It MUST exit when the request finishes: it selects on
// ctx.Done() (the DERIVED forward ctx, cancelled by serve's defer), so the goroutine can never leak past
// the call. Fails OPEN on any Control error (controlIsCancel logs at the call site) — never aborts on a
// blip. The Control read is bounded by its own client timeout, so a hung proxy cannot wedge the poller.
func (gp *gatewayProxy) pollControlAbort(ctx context.Context, runID string, cancelFwd context.CancelFunc) {
	ticker := time.NewTicker(controlPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return // the request finished (or was already aborted) — stop polling
		case <-ticker.C:
			if gp.controlIsCancel(ctx, runID) {
				gp.logf("launcher: gateway: run %s cancelled mid-call — aborting the in-flight model call", runID)
				cancelFwd()
				return
			}
		}
	}
}

// writeRunCancelled emits the pre-call real-kill refusal (m70.8): the run was cancelled, so the model
// call is REFUSED before forwarding. It returns a typed 409 run_cancelled so the agent's model call
// errors and its loop unwinds — reusing budgetErrorBody's shape so a client parses it like the other
// typed gateway refusals (dimension "run"). The provider is NOT called.
func (gp *gatewayProxy) writeRunCancelled(w http.ResponseWriter, span trace.Span, runID string) {
	span.SetAttributes(
		attribute.String("run.control", controlVerbCancel),
		attribute.String("run.id", runID),
	)
	span.SetStatus(codes.Error, errRunCancelled)

	body := budgetErrorBody{Error: errRunCancelled, Dimension: dimensionRun}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict) // 409 — the run is cancelled; the call must not proceed
	if err := json.NewEncoder(w).Encode(body); err != nil {
		gp.logf("launcher: gateway: encode run_cancelled: %v", err)
	}
	gp.logf("launcher: gateway: run_cancelled (run=%s, call refused — real kill)", runID)
}

// writeUserDeny emits the typed per-user quota rejection (402 user_budget_exceeded / 429
// user_rate_limited | user_concurrency_exceeded, dimension "user") and marks the span. The provider is NOT
// called (M66 circuit-break). Reuses budgetErrorBody so a caller parses user, tenant, and per-agent denials
// the same way.
func (gp *gatewayProxy) writeUserDeny(w http.ResponseWriter, span trace.Span, deny *userDeny) {
	span.SetAttributes(
		attribute.String("user.quota.code", deny.code),
		attribute.Int("user.quota.status", deny.status),
	)
	span.SetStatus(codes.Error, deny.code)

	body := budgetErrorBody{Error: deny.code, Dimension: dimensionUser}
	if deny.status == http.StatusPaymentRequired {
		body.Spent = strconv.FormatFloat(deny.spent, 'f', -1, 64)
		body.Cap = strconv.FormatFloat(deny.capUSD, 'f', -1, 64)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(deny.status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		gp.logf("launcher: gateway: encode user deny: %v", err)
	}
	gp.logf("launcher: gateway: %s (status=%d, call refused)", deny.code, deny.status)
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
		attrs = append(
			attrs,
			attribute.String("budget.conversation.spent_usd", convSpent.String()),
			attribute.String("budget.conversation.cap_usd", gp.convCap.String()),
		)
	}
	if gp.agentCap != nil && caps.AgentName != "" {
		attrs = append(
			attrs,
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
	// The run capability is a launcher-internal identity header (m66.7) consumed HERE to
	// enforce per-user limits — it is proof of WHO is invoking, not a LiteLLM credential,
	// and must never leak upstream to the model provider.
	strings.ToLower(runcap.HeaderName): {},
	// The record toggle (M78) is a launcher-internal per-run signal consumed HERE to key the
	// fixture capture — it is not a provider header and must never leak upstream to LiteLLM.
	strings.ToLower(recordHeaderName): {},
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
