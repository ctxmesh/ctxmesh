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

// Package egress is the INJECTING EGRESS SIDECAR (ADR 0030 §1): a per-pod, localhost proxy
// the agent's MCP tool calls are pointed at. For each call it VERIFIES the run capability
// (the invoking user's unforgeable identity), RESOLVES that user's OBO credential from the
// credential plane, INJECTS it as Authorization: Bearer, and forwards to the REAL MCP
// server. The agent (user) container holds NEITHER the token NOR the real URL — a
// prompt-injected agent can call through the sidecar but cannot read the credential.
package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/agent-engine/internal/credresolve"
	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// maxRecordBody bounds how much of a tool request/response the record seam buffers into a fixture,
// so a pathological payload can never balloon a sidecar's memory. A tool call's JSON-RPC message +
// result are small; beyond the cap the capture is skipped for that direction (never a failed
// forward — capture is best-effort). Generous vs a normal tool payload.
const maxRecordBody = 8 << 20 // 8 MiB

// toolCapture is the per-request record-mode state stashed in the request context (captureCtxKey)
// by ServeHTTP and read by the ReverseProxy's ModifyResponse hook. It carries the VERIFIED run id
// the fixture is keyed on (never the relayed header value), the pre-injection request body, and the
// parsed JSON-RPC matchers — so ModifyResponse can capture the verbatim upstream response and hand
// the whole interaction to the recorder. nil / absent ⇒ this call is not being recorded.
type toolCapture struct {
	runID    string
	callID   string
	toolName string
	reqBody  []byte
}

// ProxyConfig configures a Proxy.
type ProxyConfig struct {
	// Verifier checks the run capability against the platform public key + audience.
	Verifier *runcap.Verifier
	// Resolver resolves the invoking user's OBO credential (credresolve.K8sBackend in prod).
	Resolver credresolve.CredentialResolver
	// Namespace is the grant SOURCE namespace — the agent's own namespace (POD_NAMESPACE),
	// the key under which its users' grants were consented.
	Namespace string
	// ExpectedAgent, when non-empty, is the agent identity (ns/name) this sidecar serves; a
	// capability whose `act` (actor) is a DIFFERENT agent is rejected, so a capability minted
	// for agent A can never be redeemed at agent B's sidecar (ADR 0029 §5 scoping). Used as the
	// scoping gate ONLY when ExpectedBoundary is empty (a standalone agent / pre-ADR-0033).
	ExpectedAgent string
	// ExpectedBoundary, when non-empty, is the trust boundary (ADR 0033) this sidecar serves —
	// the agent's registry. It SUPERSEDES the ExpectedAgent gate: a capability is redeemable
	// here iff its `bnd` matches, so teammates in the same registry can act on-behalf-of the
	// user (relayed across A2A, m30.3) while a different registry's capability is rejected. A
	// standalone agent's boundary is unique to it, so this reduces to the per-agent check.
	ExpectedBoundary string
	// Routes maps a server name (the first path segment) to its real upstream + auth type. It is the
	// STATIC route table (from the EGRESS_ROUTES env) — the source when RoutesHolder is nil (legacy /
	// tests, byte-for-byte unchanged).
	Routes RouteTable
	// RoutesHolder, when non-nil, supersedes Routes with a hot-reloadable table the sidecar fsnotify-
	// watches from the mounted EGRESS_ROUTES_FILE (J7): a remote-tool-URL edit updates the ConfigMap
	// the holder reloads, so it takes effect on the running sidecar WITHOUT a revision roll. nil ⇒ the
	// static Routes above.
	RoutesHolder *RouteHolder
	// Transport is the RoundTripper for the upstream forward (nil ⇒ http.DefaultTransport).
	Transport http.RoundTripper
	// Log is the structured logger.
	Log logr.Logger
	// Recorder, when non-nil, is the M78 record-mode TOOL capture (ADR 0071 §1/C1): every tool
	// call of a RECORDED run (the SDK relays X-Ctxmesh-Record) is captured pre-injection (request
	// body) + verbatim (upstream response) into the run's replay fixture. nil ⇒ record mode off,
	// the capture path is a no-op (zero overhead, the forward is byte-for-byte unchanged).
	Recorder *ToolRecorder
	// Policy holds the resolved spec.runtime.toolPolicy the controller delivers (M82, ADR 0074 §1),
	// read + fsnotify-watched from the mounted TOOL_POLICY_FILE. ServeHTTP CONSULTS it on the hot path
	// (Policy.Load()) and ENFORCES it: deny → 403, require-approval → the voucher protocol, plus the
	// fan-out ceiling. A malformed initial policy is a hard startup error (C16, ADR 0087). nil ⇒ no
	// holder wired (permissive). A nil-VALUED holder (no policy set) is also permissive.
	Policy *PolicyHolder
}

// runCounterTTL bounds how long a per-run fan-out counter entry lives after its last tool call
// (M82.5). It is set generously: a run whose runcap has expired can never increment again (ServeHTTP
// rejects the expired capability upstream, before enforcement), so the entry only needs to age out to
// reclaim memory — an hour comfortably outlives any live run's tool-call cadence.
const runCounterTTL = time.Hour

// runCounterSoftCap is the entry count past which increment() sweeps stale entries inline. Below it,
// increment is a pure O(1) map bump (the common path); the sweep only runs when the map has actually
// grown, keeping the hot path cheap.
const runCounterSoftCap = 4096

// runCallEntry is one run's fan-out tally: how many tool calls it has forwarded and when it was last
// seen (for TTL eviction).
type runCallEntry struct {
	count    int
	lastSeen time.Time
}

// runCallCounter is the in-pod, per-run tool-call fan-out tally that backs the anti-DoS ceiling
// (M82.5, ADR 0074). It is a mutex-guarded map keyed on the VERIFIED runcap RunID.
//
// SECURITY: runIDs come from cryptographically-VERIFIED runcaps — ServeHTTP verifies the capability's
// signature before enforcement, so the map only ever grows with genuinely-minted runs. An attacker
// cannot forge distinct runIDs to balloon the map, and TTL eviction reclaims finished runs. This is
// the IN-POD floor only; cross-pod / fleet coordination (Valkey) is explicitly deferred.
type runCallCounter struct {
	mu  sync.Mutex
	m   map[string]*runCallEntry
	now func() time.Time // injectable clock for deterministic tests; defaults to time.Now
}

// newRunCallCounter builds an empty counter with the real clock.
func newRunCallCounter() *runCallCounter {
	return &runCallCounter{m: make(map[string]*runCallEntry), now: time.Now}
}

// increment bumps the run's tally under the lock and returns the NEW count. It opportunistically
// sweeps stale entries (lastSeen older than runCounterTTL) — but only when the map has grown past the
// soft cap, so the common path stays O(1). The sweep is safe to run on the same lock: a swept run
// that later reappears simply starts a fresh entry (its runcap would have to still be valid, which
// means it was not actually finished — a benign re-count, never an under-count of a live run).
func (c *runCallCounter) increment(runID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.m) > runCounterSoftCap {
		for id, e := range c.m {
			if now.Sub(e.lastSeen) > runCounterTTL {
				delete(c.m, id)
			}
		}
	}
	e := c.m[runID]
	if e == nil {
		e = &runCallEntry{}
		c.m[runID] = e
	}
	e.count++
	e.lastSeen = now
	return e.count
}

// Proxy is the sidecar HTTP handler.
type Proxy struct {
	cfg         ProxyConfig
	reverse     *httputil.ReverseProxy
	callCounter *runCallCounter
}

type (
	targetCtxKey  struct{}
	credCtxKey    struct{}
	captureCtxKey struct{}
)

// NewProxy builds a Proxy. The single ReverseProxy reads the per-request upstream + injected
// credential from the request context (stashed by ServeHTTP after verify+resolve).
func NewProxy(cfg ProxyConfig) *Proxy {
	p := &Proxy{cfg: cfg, callCounter: newRunCallCounter()}
	p.reverse = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target, _ := pr.In.Context().Value(targetCtxKey{}).(*url.URL)
			if target != nil {
				pr.SetURL(target)
				pr.Out.Host = target.Host
			}
			// The capability + approval voucher are proof for the sidecar ONLY — never leak them to
			// the upstream MCP server. Strip them (and any inbound Authorization) and inject ours.
			pr.Out.Header.Del(runcap.HeaderName)
			pr.Out.Header.Del(runcap.ApprovalHeaderName)
			pr.Out.Header.Del("Authorization")
			if cred, _ := pr.In.Context().Value(credCtxKey{}).(string); cred != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+cred)
			}
		},
		// Stream responses immediately — MCP streamable-http replies as SSE.
		FlushInterval: -1,
		Transport:     cfg.Transport,
		// Record mode (M78, ADR 0071 §1/C1): capture the VERBATIM upstream response bytes for a
		// recorded tool call. The response carries NO credential (the OBO bearer is a REQUEST header,
		// injected by Rewrite; the upstream never echoes it back), so C4 holds. We buffer the body,
		// hand a copy to the recorder, and restore an identical reader so the agent sees byte-for-
		// byte the same response incl. SSE/streamable-http framing. nil-recorder / non-recorded call
		// ⇒ this hook is a no-op (nil capture in the context). Best-effort: a read error skips
		// capture, never fails the forward.
		ModifyResponse: p.captureResponse,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			cfg.Log.Error(err, "egress: upstream forward failed")
			writeError(w, http.StatusBadGateway, "upstream_unreachable", "could not reach the MCP server")
		},
	}
	return p
}

// captureResponse is the ReverseProxy ModifyResponse hook that records a recorded tool call's
// VERBATIM upstream response (M78, ADR 0071 §1/C1). It reads the per-request toolCapture from the
// context (nil ⇒ not a recorded call, no-op), buffers the response body (capped, best-effort),
// restores an identical body reader so the agent receives byte-for-byte the same response, and
// hands the whole interaction (pre-injection request + verbatim response) to the recorder keyed on
// the VERIFIED run id. It never returns an error — a record-path failure must not fail the forward.
func (p *Proxy) captureResponse(resp *http.Response) error {
	cap, _ := resp.Request.Context().Value(captureCtxKey{}).(*toolCapture)
	if cap == nil || p.cfg.Recorder == nil {
		return nil
	}
	// O9: capture the tool response Content-Type so replay serves the recorded framing instead of
	// sniffing SSE-vs-JSON from the bytes.
	contentType := resp.Header.Get("Content-Type")
	body := resp.Body
	if body == nil {
		p.cfg.Recorder.capture(resp.Request.Context(), cap.runID, cap.callID, cap.toolName, cap.reqBody, nil, contentType)
		return nil
	}
	buffered, err := io.ReadAll(io.LimitReader(body, maxRecordBody+1))
	_ = body.Close()
	if err != nil || int64(len(buffered)) > maxRecordBody {
		// Could not fully buffer the response (read error or over the cap) — restore what we read so
		// the forward still streams, and skip capture for this call rather than store a truncated
		// response (a partial fixture would mis-replay). The request/response are never mutated.
		resp.Body = io.NopCloser(bytes.NewReader(buffered))
		p.cfg.Log.Info("egress: record: response body not fully buffered — skipping tool capture",
			"run", cap.runID, "tool", cap.toolName)
		return nil
	}
	// Restore an identical reader so the agent sees the response verbatim.
	resp.Body = io.NopCloser(bytes.NewReader(buffered))
	p.cfg.Recorder.capture(resp.Request.Context(), cap.runID, cap.callID, cap.toolName, cap.reqBody, buffered, contentType)
	return nil
}

// currentRoutes returns the live route table: the hot-reloadable RoutesHolder when one is wired AND
// currently non-empty (J7 — a URL edit takes effect without a roll), else the static Routes (legacy /
// tests). A holder that is wired but momentarily empty (e.g. an operator cleared the file) falls back
// to the static table rather than serving nothing — a routes table is always required.
func (p *Proxy) currentRoutes() RouteTable {
	if t := p.cfg.RoutesHolder.Load(); len(t) > 0 {
		return t
	}
	return p.cfg.Routes
}

// ServeHTTP resolves the route, verifies the capability, resolves the credential, and
// forwards. It fails CLOSED: no/invalid capability, an unknown route, or an agent-scope
// mismatch is rejected before any upstream call.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, remainder, ok := p.currentRoutes().routeForPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "no_route", "no egress route for this server")
		return
	}

	// Verify the run capability — the ONLY source of the invoking user's identity. The
	// sidecar never trusts a name the agent supplies.
	token := r.Header.Get(runcap.HeaderName)
	if token == "" {
		// No capability ⇒ an unattended / direct call. Personal OBO needs the invoker's
		// identity; org/public (no-capability) resolution lands in m25.9.
		writeError(w, http.StatusUnauthorized, "no_capability", "this tool call carries no run capability")
		return
	}
	runCap, err := p.cfg.Verifier.Verify(token)
	if err != nil {
		p.cfg.Log.Info("egress: capability rejected", "reason", err.Error(), "server", route.Name)
		writeError(w, http.StatusUnauthorized, "invalid_capability", "the run capability was rejected")
		return
	}
	// Scoping gate (ADR 0033, m30.3): prefer the BOUNDARY check — a capability is redeemable here
	// iff its boundary matches this sidecar's registry, so a teammate's relayed capability is
	// accepted (team-OBO) but a different registry's is not. Fall back to the exact-agent check
	// only when no boundary is configured (a standalone agent, or a pre-ADR-0033 sidecar).
	switch {
	case p.cfg.ExpectedBoundary != "":
		if runCap.Boundary != p.cfg.ExpectedBoundary {
			writeError(w, http.StatusForbidden, "boundary_mismatch", "the run capability is scoped to a different trust boundary")
			return
		}
	case p.cfg.ExpectedAgent != "" && runCap.Agent != p.cfg.ExpectedAgent:
		// A capability minted for another agent must not be redeemable here.
		writeError(w, http.StatusForbidden, "agent_mismatch", "the run capability was minted for a different agent")
		return
	}

	// ── Tool-policy enforcement (M82.2, ADR 0074 §2/§5) — FAIL CLOSED ──
	// The sidecar is the authoritative chokepoint: it uniquely holds the verified runcap AND the
	// wire tool name, so this is where deny/require-approval become unbypassable for OBO tools (a
	// custom loop calling around the sidecar has no credential). We enforce BEFORE resolving the OBO
	// credential — a denied call must never trigger a credential lookup — and before the record seam.
	// On a pure-allow route this is a cheap policy read + early return: the body is NOT touched and
	// the forward stays byte-for-byte identical to pre-M82. On a restrictive route (any deny /
	// require-approval rule) it buffers + classifies the body and closes the §5 bypasses.
	if !p.enforceToolPolicy(w, r, route.Name, runCap.RunID) {
		return
	}

	// Resolve THIS user's OBO credential for THIS server, within the run's trust boundary
	// (ADR 0033): the capability carries the boundary (the invoking agent's registry, or the
	// agent when standalone), so an agent resolves only grants scoped to its own boundary.
	cred, err := p.cfg.Resolver.Resolve(r.Context(), p.cfg.Namespace, runCap.Boundary, route.Name, runCap.User)
	switch {
	case err == nil:
		// Have the invoking user's fresh token — inject it below.
	case errors.Is(err, credresolve.ErrNoCredential):
		// An open server — forward with no Authorization (cred stays empty).
	case errors.Is(err, credresolve.ErrConsentRequired):
		// The user must connect their own account (ADR 0029 §2). The structured error names
		// the SERVER so the runtime can surface a "Connect your account for <server>" CTA
		// (m25.9 consent contract) — the SDK maps it to a distinct consent_required outcome.
		writeConsentRequired(w, route.Name)
		return
	default:
		p.cfg.Log.Error(err, "egress: credential resolution failed", "server", route.Name)
		writeError(w, http.StatusBadGateway, "resolve_failed", "could not resolve the credential")
		return
	}

	// ── Record mode (M78, ADR 0071 §1/C1): buffer the request body PRE-INJECTION ──
	// This is the C4-safe capture point: we buffer the agent-visible request BODY here, BEFORE
	// reverse.ServeHTTP injects the OBO bearer (Rewrite sets the Authorization header on the
	// OUTBOUND request; the body is untouched). So the captured request carries no credential by
	// construction. Gated per-run by the X-Ctxmesh-Record header the SDK relays on a recorded
	// call; keyed on the VERIFIED runCap.RunID (never the relayed value) so a forged header cannot
	// mis-key another run's fixture. recorder nil / header absent ⇒ no buffering, zero overhead.
	var capture *toolCapture
	if p.cfg.Recorder != nil && r.Header.Get(RecordHeaderName) != "" && runCap.RunID != "" {
		capture = p.bufferRequestForRecord(r, runCap.RunID)
	}

	// Forward to the real upstream: rewrite the path to strip the /<server> prefix, then let
	// the ReverseProxy inject the credential + strip the capability (see Rewrite).
	forwardURL := *route.Target()
	r.URL.Path = remainder
	ctx := context.WithValue(r.Context(), targetCtxKey{}, &forwardURL)
	ctx = context.WithValue(ctx, credCtxKey{}, cred.Value)
	if capture != nil {
		ctx = context.WithValue(ctx, captureCtxKey{}, capture)
	}
	p.reverse.ServeHTTP(w, r.WithContext(ctx))
}

// enforceToolPolicy applies the resolved tool policy at the wire (M82.2, ADR 0074 §2/§5). It returns
// true to CONTINUE the forward and false when it has already written a terminal response (the caller
// must return). The regime is FAIL-CLOSED but only on a RESTRICTIVE route:
//
//   - No policy, or a PURE-ALLOW policy (Restricts()==false) ⇒ permissive, return true WITHOUT
//     touching the body. The forward stays byte-for-byte identical to pre-M82 (no batch reject, no
//     fail-closed) — a permissive route has no security need and must not break existing agents.
//   - A RESTRICTIVE policy (any deny / require-approval rule) ⇒ buffer + classify the body and:
//     (a) REJECT a batch array outright (the smuggling vector) — 400;
//     (b) allow a non-tools/call method ONLY if it's on the handshake/discovery allow-list — else 403;
//     (c) FAIL CLOSED (403) on any tools/call whose params.name can't be extracted, or a body that
//     can't be classified at all (the fail-open bypass, closed);
//     (d) match the WIRE params.name (not the route segment) against the per-tool rule — deny ⇒ 403;
//     require-approval ⇒ the STATELESS approval-voucher protocol (ADR 0074 §3, m82.4): forward only
//     when the request carries a VALID X-Ctxmesh-Approval voucher (signature valid, run == this
//     verified run, tool == this wire tool, unexpired), else a typed 403 approval_required; allow ⇒
//     forward.
//
// runID is the VERIFIED run capability's run id — the voucher must be bound to it (never a header).
// The body is drained + restored so a forwarded call still streams the verbatim bytes (and the
// record seam downstream re-buffers them independently).
func (p *Proxy) enforceToolPolicy(w http.ResponseWriter, r *http.Request, server, runID string) bool {
	policy := p.cfg.Policy.Load()
	if !policy.NeedsInspection() {
		// Pure-allow (or no) policy with NO active ceiling: permissive, body untouched, byte-for-byte
		// unchanged. A ceiling>0 (even on an otherwise pure-allow policy) engages inspection below,
		// because counting tool calls requires classifying the body.
		return true
	}

	// Restrictive route: we must inspect the body, so buffer + restore it (the forward still needs
	// the verbatim bytes). A body read error on a restrictive route fails CLOSED — we cannot prove
	// the call is allowed, so we must not forward it.
	body, err := p.bufferRestoreBody(r)
	if err != nil {
		p.cfg.Log.Info("egress: policy: request body unreadable on a restrictive route — failing closed", "server", server)
		writeError(w, http.StatusForbidden, "tool_denied", "the tool call could not be inspected against the policy")
		return false
	}

	wc, err := classifyWireCall(body)
	if err != nil {
		// Unparseable / empty body on a restrictive route: could be smuggling a denied call — fail closed.
		p.cfg.Log.Info("egress: policy: unclassifiable request on a restrictive route — failing closed", "server", server)
		writeError(w, http.StatusForbidden, "tool_denied", "the tool call could not be identified against the policy")
		return false
	}
	switch {
	case wc.isBatch:
		// (b) A JSON-RPC batch array is the bypass vector — a denied call hidden among allowed
		// ones. MCP streamable-http never needs batches; reject outright before any per-call view.
		p.cfg.Log.Info("egress: policy: batch request rejected on a restrictive route", "server", server)
		writeError(w, http.StatusBadRequest, "batch_not_allowed", "batch tool requests are not allowed under this tool policy")
		return false
	case !wc.isToolCall:
		// (a) A non-tools/call message: allow ONLY the handshake/discovery allow-list without a
		// tool decision; anything else on a restrictive route needs a tool decision it can't give.
		if isAllowlistedMethod(wc.method) {
			return true
		}
		p.cfg.Log.Info("egress: policy: non-allowlisted method rejected on a restrictive route", "server", server, "method", wc.method)
		writeError(w, http.StatusForbidden, "tool_denied", "this method is not permitted under the tool policy")
		return false
	case !wc.hasToolName:
		// (c) A tools/call whose params.name can't be extracted — FAIL CLOSED (do NOT forward).
		p.cfg.Log.Info("egress: policy: tools/call with no identifiable tool name — failing closed", "server", server)
		writeError(w, http.StatusForbidden, "tool_denied", "the tool call did not name a tool")
		return false
	}

	// (d) Per-tool decision on the WIRE params.name (not the route segment — ADR 0074 §5): a
	// multi-tool OBO server routes many tools under one ServerName segment, so each call's
	// params.name is matched independently.
	rule := policy.RuleFor(wc.toolName)
	switch rule {
	case RuleAllow:
		// A forwardable tool call — apply the anti-DoS fan-out ceiling (M82.5) before forwarding.
		return p.admitFanOut(w, policy, server, wc.toolName, runID)
	case RuleRequireApproval:
		// The STATELESS approval-voucher protocol (ADR 0074 §3, m82.4). A require-approval tool is
		// FORWARDED only when the request carries a VALID approval voucher; otherwise the sidecar
		// returns a typed 403 approval_required naming {server, tool, run} so the SDK can pause for a
		// human, mint a voucher on approval, and RETRY carrying it. No control-plane lookup on the hot
		// path — the voucher's signature (the SAME platform key as the runcap) is the whole proof.
		//
		// FAIL-CLOSED: no voucher, or a forged / expired / wrong-run / wrong-tool / runcap-as-voucher
		// token, all yield the typed 403 — never a forward. A delegated sub-run has no human to mint a
		// voucher, so it simply never gets one and is denied automatically.
		voucher := strings.TrimSpace(r.Header.Get(runcap.ApprovalHeaderName))
		if voucher == "" {
			p.cfg.Log.Info("egress: policy: require-approval tool has no voucher — 403 approval_required", "server", server, "tool", wc.toolName)
			writeApprovalRequired(w, server, wc.toolName, runID)
			return false
		}
		if _, vErr := p.cfg.Verifier.VerifyVoucher(voucher, runID, wc.toolName); vErr != nil {
			// A present-but-invalid voucher is a rejection, not a forward — but still surface the
			// typed approval_required so a legitimate SDK (whose voucher expired mid-run, say) can
			// re-request approval rather than treat it as a hard denial.
			p.cfg.Log.Info("egress: policy: require-approval voucher rejected — 403 approval_required",
				"server", server, "tool", wc.toolName, "reason", vErr.Error())
			writeApprovalRequired(w, server, wc.toolName, runID)
			return false
		}
		// Valid voucher for THIS run + THIS tool: the human approved it — forward (into OBO injection),
		// but an approved dispatch still counts against the run's fan-out ceiling (M82.5).
		p.cfg.Log.Info("egress: policy: require-approval voucher accepted — forwarding", "server", server, "tool", wc.toolName)
		return p.admitFanOut(w, policy, server, wc.toolName, runID)
	default:
		// deny (and any non-allow value — fail closed on the unexpected).
		p.cfg.Log.Info("egress: policy: tool denied", "server", server, "tool", wc.toolName, "rule", rule)
		writeError(w, http.StatusForbidden, "tool_denied", "this tool is denied by the tool policy")
		return false
	}
}

// admitFanOut applies the per-run anti-DoS fan-out CEILING (M82.5, ADR 0074) at the point a tool call
// is confirmed FORWARDABLE (an allowed tool, or a require-approval tool with a valid voucher). It
// returns true to forward and false when it has written a terminal 403 (the caller must return). The
// ceiling counts real fan-out only: a denied tool or an unapproved require-approval tool never reaches
// here, so it never consumes the ceiling.
//
//   - MaxToolCallsPerRun <= 0 ⇒ unlimited: no counting, forward.
//   - Empty runID under an ACTIVE ceiling ⇒ FAIL CLOSED (403): a verified runcap with no RunID is a
//     control-plane misconfiguration, and an unattributable call cannot be bounded, so it is denied.
//   - Otherwise increment this run's tally; the (N+1)th forwarded call (count > limit) is denied with
//     a TERMINAL 403 (not 429 — a runaway loop must not be invited to retry; the point is to STOP the
//     flood). Calls at or below the limit forward.
func (p *Proxy) admitFanOut(w http.ResponseWriter, policy *ToolPolicy, server, tool, runID string) bool {
	limit := policy.MaxToolCallsPerRun
	if limit <= 0 {
		p.recordToolCall(tool, "forwarded") // J9: count fan-out even with no active ceiling.
		return true                         // no active ceiling — do not count against a limit.
	}
	if runID == "" {
		// A verified runcap with no RunID under an active ceiling: an unattributable call cannot be
		// bounded, so fail closed rather than let it bypass the ceiling.
		p.cfg.Log.Info("egress: policy: tool call under an active fan-out ceiling has no run id — failing closed",
			"server", server, "tool", tool)
		p.recordToolCall(tool, "ceiling_denied") // J9
		writeError(w, http.StatusForbidden, "tool_call_ceiling_exceeded", "this run has exceeded its tool-call ceiling")
		return false
	}
	n := p.callCounter.increment(runID)
	if n > int(limit) {
		// Terminal 403 (non-retryable) — stop the flood, do NOT invite a retry-after.
		p.cfg.Log.Info("egress: policy: run exceeded its tool-call fan-out ceiling — 403",
			"server", server, "tool", tool, "count", n, "limit", limit)
		p.recordToolCall(tool, "ceiling_denied") // J9
		writeError(w, http.StatusForbidden, "tool_call_ceiling_exceeded", "this run has exceeded its tool-call ceiling")
		return false
	}
	p.recordToolCall(tool, "forwarded") // J9
	return true
}

// bufferRestoreBody reads the full request body (capped) and restores an identical reader so the
// forward still streams the verbatim bytes. Returns the buffered bytes. An error means the body
// could not be read/was oversize — the caller fails closed (a restrictive route must not forward a
// call it can't inspect). A nil body yields empty bytes (classifyWireCall then fails closed).
func (p *Proxy) bufferRestoreBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxRecordBody+1))
	_ = r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(b))
		return nil, err
	}
	if int64(len(b)) > maxRecordBody {
		r.Body = io.NopCloser(bytes.NewReader(b))
		return nil, errors.New("egress: request body exceeds inspection cap")
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

// bufferRequestForRecord drains + restores the request body so the record seam captures the
// agent-visible request bytes (pre-injection, C4-safe) while the forward still streams the exact
// bytes. It also best-effort parses the JSON-RPC tools/call message for the replay matchers
// (call id + tool name). Returns the capture state to stash in the forward context; nil on a body
// read error (capture is skipped rather than failing the tool call).
func (p *Proxy) bufferRequestForRecord(r *http.Request, runID string) *toolCapture {
	var buffered []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxRecordBody+1))
		_ = r.Body.Close()
		if err != nil || int64(len(b)) > maxRecordBody {
			// Restore what we read so the forward still streams, and skip capture (never fail the
			// call on a record-path read error / oversize body).
			r.Body = io.NopCloser(bytes.NewReader(b))
			p.cfg.Log.Info("egress: record: request body not fully buffered — skipping tool capture", "run", runID)
			return nil
		}
		buffered = b
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	callID, toolName := parseToolCall(buffered)
	return &toolCapture{runID: runID, callID: callID, toolName: toolName, reqBody: buffered}
}

// parseToolCall best-effort extracts the JSON-RPC tool-call id + tool name from an MCP
// tools/call request body for the replay matchers (ADR 0071 §2: primary CallID, fallback
// ToolName+ArgsHash). The body is a JSON-RPC message:
//
//	{"jsonrpc":"2.0","id":<n|str>,"method":"tools/call","params":{"name":"<tool>","arguments":{…}}}
//
// callID is the JSON-RPC id rendered as a string (numbers → their decimal form); toolName is
// params.name. Both degrade to "" on a non-tools/call or unparseable body — replay then falls back
// to name+args-hash / captured-order matching. It NEVER fails the call (the argsBody stored is the
// verbatim request regardless).
func parseToolCall(body []byte) (callID, toolName string) {
	if len(body) == 0 {
		return "", ""
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return "", ""
	}
	if msg.Method != "tools/call" {
		// A handshake / tools-list / notification, not a tool call — no matcher keys.
		return "", ""
	}
	return jsonRPCID(msg.ID), msg.Params.Name
}

// jsonRPCID renders a JSON-RPC id (a JSON number or string) to the stable string form the fixture
// stores as CallID. A string id is unquoted; a numeric id becomes its decimal string; null/absent
// yields "". Best-effort — a shape it cannot render yields "".
func jsonRPCID(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(trimmed, &n) == nil {
		return n.String()
	}
	return ""
}

// wireCall is the enforcement-time classification of a request body (M82.2, ADR 0074 §5). Unlike
// parseToolCall (record mode, which fails OPEN — "couldn't identify the tool" ⇒ ("","")), this is
// used FAIL-CLOSED on a restrictive route, so it distinguishes the cases parseToolCall collapses:
// a batch array, a non-tools/call method (for the allow-list), and a tools/call with a present vs
// absent params.name. err is non-nil only for a body the enforcement path cannot classify (an
// unparseable non-batch body) — which the caller treats as fail-closed.
type wireCall struct {
	isBatch     bool   // top-level JSON array (a JSON-RPC batch — the bypass vector)
	method      string // the JSON-RPC method (e.g. "tools/call", "initialize"); "" if absent
	isToolCall  bool   // method == "tools/call"
	toolName    string // params.name for a tools/call; "" if absent/empty
	hasToolName bool   // params.name was present AND non-empty
}

// classifyWireCall inspects a request body for fail-closed policy enforcement. It first detects a
// batch by the first non-space byte ('[' ⇒ a top-level JSON array) BEFORE any unmarshal, so a batch
// is rejected structurally rather than sniffed from a decode error. Otherwise it unmarshals the
// single JSON-RPC message and reports the method + (for a tools/call) whether params.name is present.
// An empty body or a body that is neither a batch nor a decodable JSON object is an error (the caller
// fails closed). This NEVER mutates the body — the verbatim bytes are still forwarded on allow.
func classifyWireCall(body []byte) (wireCall, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return wireCall{}, errors.New("egress: empty request body")
	}
	// Batch detection is structural: a top-level JSON array (first non-space byte '[') is a
	// JSON-RPC batch. MCP streamable-http does not need batches; a batch is the smuggling vector
	// (a denied call hidden among allowed ones), so we flag it before decoding anything.
	if trimmed[0] == '[' {
		return wireCall{isBatch: true}, nil
	}
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		return wireCall{}, fmt.Errorf("egress: unparseable request body: %w", err)
	}
	wc := wireCall{method: msg.Method, isToolCall: msg.Method == "tools/call"}
	if wc.isToolCall {
		wc.toolName = msg.Params.Name
		wc.hasToolName = strings.TrimSpace(msg.Params.Name) != ""
	}
	return wc, nil
}

// isAllowlistedMethod reports whether a non-tools/call JSON-RPC method may pass a restrictive route
// WITHOUT a per-tool policy decision (ADR 0074 §5(a)): the MCP handshake + discovery + liveness that
// carry no tool invocation — initialize, tools/list, ping, and any notifications/* method. Anything
// else on a restrictive route needs a tool decision (and, absent one, fails closed).
func isAllowlistedMethod(method string) bool {
	switch method {
	case "initialize", "tools/list", "ping":
		return true
	}
	return strings.HasPrefix(method, "notifications/")
}

// errorBody is the sidecar's structured error surface — a machine-readable code + a short
// message (+ the server, for consent_required; + the tool/run, for approval_required). It NEVER
// carries token material.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Server  string `json:"server,omitempty"`
	// Tool / Run identify the require-approval decision point (ADR 0074 §3) so the SDK can pause for a
	// human on exactly this {tool, run} and, on approval, retry with a voucher bound to it.
	Tool string `json:"tool,omitempty"`
	Run  string `json:"run,omitempty"`
}

// writeError writes a JSON structured error with the given status.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: code, Message: message})
}

// writeConsentRequired writes the structured consent_required error, naming the server the
// user must connect their account to (the m25.9 consent contract).
func writeConsentRequired(w http.ResponseWriter, server string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(errorBody{
		Error:   "consent_required",
		Message: "connect your account to use this tool",
		Server:  server,
	})
}

// writeApprovalRequired writes the structured approval_required error (ADR 0074 §3): a typed 403 that
// names the {server, tool, run} a human must approve before the tool can run. The SDK maps it to a
// requires_action (approval) outcome; on approval the BFF mints a voucher bound to {run, tool} and the
// SDK retries carrying it. It is the require-approval analogue of writeConsentRequired — a structured,
// RECOVERABLE 403, not a hard denial (deny stays "tool_denied"). It carries NO token material.
func writeApprovalRequired(w http.ResponseWriter, server, tool, runID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(errorBody{
		Error:   "approval_required",
		Message: "this tool requires human approval before it can run",
		Server:  server,
		Tool:    tool,
		Run:     runID,
	})
}
