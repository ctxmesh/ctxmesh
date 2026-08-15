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

// The fenced LLM-judge (M66 m66.8, ADR 0059 §8 Fork-5). An OPTIONAL semantic-classification
// layer that augments the deterministic guardrail engine with a model call. It is fenced so it
// is loop-safe, cheap, and can NEVER become a liability:
//
//   - Cascaded (runs last): the judge runs only when semanticJudge.enabled, applies to the scan
//     point, AND the deterministic detectors did NOT already block that content. A deterministic
//     block short-circuits — the judge is never called on already-blocked content, so it is the
//     residual step over content the fast path let through.
//
//   - Loop-safe BY CONSTRUCTION: the judge issues its chat-completion to gp.upstream DIRECTLY
//     (the real LiteLLM, the same target forward() uses, with the same gateway auth). It MUST NOT
//     go to localhost:2996 / re-enter the guardrail proxy — otherwise the judge's own upstream
//     call would be re-scanned and could recursively spawn another judge call. Because it targets
//     the real gateway, not this proxy, that recursion is impossible.
//
//   - Fails OPEN on its own error/timeout (deliberate — ADR 0059 Fork-5): the judge is best-effort
//     augmentation and is NEVER the basis of the fail-closed guarantee. A judge error / timeout /
//     unparseable verdict → ALLOW + log (do NOT block). A flaky judge must not take down all guarded
//     traffic; the deterministic pipeline is the guarantee. Only a clean FLAGGED verdict enforces.
//
//   - Cached: an in-process cache keyed by hash(scanned content + policyVersion) so identical
//     content costs at most one judge call. policyVersion is a stable hash of the judge policy so a
//     policy change invalidates the cache. The cache is size-bounded so it cannot grow unbounded.
//
//   - Exempt from per-user limits + per-conversation cap: the judge's own upstream call is SYSTEM
//     traffic — it is issued OUT-OF-BAND (a fresh http.Request built here, not the caller's r), so it
//     never flows through the per-user accumulator (userHashFromRequest) or the per-conversation
//     budget (those read the caller's X-Conversation-Id / run-capability headers, which this request
//     deliberately does not carry).
//
//   - Default off: enabled=false (or no semanticJudge) ⇒ zero judge calls, zero overhead — the
//     scan path is byte-for-byte the deterministic pipeline (newSemanticJudge returns nil).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
)

const (
	// judgeDetectorName is the STABLE detector label the judge stamps on its guardrail.decision
	// event and 403 refusal body. It marks a decision as coming from the semantic judge rather than
	// a deterministic detector.
	judgeDetectorName = "semantic-judge"

	// judgeTimeout bounds each judge upstream round-trip. It is deliberately short: the judge is
	// best-effort augmentation, and a slow judge must fail OPEN quickly rather than stall the guarded
	// call. Shorter than gatewayRequestTimeout (the primary forward() budget).
	judgeTimeout = 10 * time.Second

	// judgeVerdictFlagged / judgeVerdictSafe are the two verdicts the judge instruction constrains the
	// model to. Comparison is case-insensitive + trimmed; anything else is UNPARSEABLE ⇒ fail open.
	judgeVerdictFlagged = "FLAGGED"
	judgeVerdictSafe    = "SAFE"

	// judgeInstruction is appended to the operator's policy in the system message. It constrains the
	// model to a single-token verdict so parsing is deterministic and the response is cheap.
	judgeInstruction = "\n\nYou are a content classifier. Apply the policy above to the user " +
		"message. Reply with EXACTLY one word and nothing else: FLAGGED if the message violates the " +
		"policy, or SAFE if it does not."

	// judgeCacheMaxEntries bounds the in-process verdict cache so it cannot grow unbounded under a
	// stream of unique content. When full, the cache is cleared wholesale (a coarse but simple bound
	// that keeps hot content cached within a policy generation; the judge fails open on a miss anyway).
	judgeCacheMaxEntries = 4096
)

// semanticJudgeConfig mirrors the api/v1beta1.SemanticJudge JSON injected in GUARDRAIL_POLICY. Only
// the fields the in-path judge consumes are modeled; unknown fields are ignored (same launcher-local
// mirror pattern as guardrailPolicyConfig). The controller applies the CRD defaults (action=block,
// appliesTo=output) before serializing, so a blank here means "spec omitted it" and we default again
// (defence-in-depth), never silently drop.
type semanticJudgeConfig struct {
	Enabled    bool   `json:"enabled,omitempty"`
	ModelRoute string `json:"modelRoute,omitempty"`
	Policy     string `json:"policy,omitempty"`
	Action     string `json:"action,omitempty"`
	AppliesTo  string `json:"appliesTo,omitempty"`
}

// parseSemanticJudge extracts the semanticJudge section from the raw GUARDRAIL_POLICY JSON (m66.8).
// Empty policy or no semanticJudge ⇒ (nil, nil): no judge. A policy that does not parse is a hard
// error — same fail-closed LOAD posture as newGuardrailEngine / parseUserRateLimit (the load path is
// always fail-closed; the judge's RUN path is what fails open). Kept separate from newGuardrailEngine
// because the judge is enforced by the gateway proxy (it needs the upstream + client), not the
// content-scanning engine.
func parseSemanticJudge(policyJSON string) (*semanticJudgeConfig, error) {
	if policyJSON == "" {
		return nil, nil
	}
	var cfg struct {
		SemanticJudge *semanticJudgeConfig `json:"semanticJudge,omitempty"`
	}
	if err := json.Unmarshal([]byte(policyJSON), &cfg); err != nil {
		return nil, fmt.Errorf("guardrail: parsing GUARDRAIL_POLICY: %w", err)
	}
	return cfg.SemanticJudge, nil
}

// judgeVerdict is a cached classification outcome. flagged is true iff the judge returned FLAGGED.
type judgeVerdict struct {
	flagged bool
}

// semanticJudge is the compiled, process-wide LLM-judge. Built once from GUARDRAIL_POLICY when
// semanticJudge.enabled; nil ⇒ no judge (zero overhead). It holds the classification policy, the
// scan point it applies to, the enforcement action, and a bounded verdict cache keyed by
// hash(content + policyVersion).
type semanticJudge struct {
	modelRoute string
	policy     string          // the operator's classification prompt (natural language)
	action     guardrailAction // block | auditOnly (redact is meaningless for a whole-message verdict)
	scanPoint  guardrailScanPoint
	// policyVersion is a stable hash of the judge policy (modelRoute + policy + instruction). It keys
	// the cache so any policy change invalidates prior verdicts without a manual flush.
	policyVersion string

	mu    sync.Mutex
	cache map[string]judgeVerdict

	// timeout bounds each upstream round-trip (overridable in tests; defaults to judgeTimeout).
	timeout time.Duration
}

// newSemanticJudge compiles the judge from the raw GUARDRAIL_POLICY JSON. It returns (nil, nil) when
// there is no semanticJudge OR it is disabled (default off ⇒ zero judge calls). A policy that does
// not parse is a hard error (fail-closed load). A judge with enabled=true but no modelRoute is a
// misconfiguration we treat as OFF with a caller log — the judge fails open, so it must never block
// simply because it has nowhere to send its call.
func newSemanticJudge(policyJSON string, logf func(string, ...any)) (*semanticJudge, error) {
	cfg, err := parseSemanticJudge(policyJSON)
	if err != nil {
		return nil, err
	}
	if cfg == nil || !cfg.Enabled {
		return nil, nil // default off: no judge, no overhead.
	}
	if strings.TrimSpace(cfg.ModelRoute) == "" {
		// enabled but unroutable: cannot issue a judge call. Leave the judge OFF (fail open) rather
		// than block every guarded call — the deterministic pipeline still applies.
		if logf != nil {
			logf("launcher: gateway: semanticJudge.enabled but modelRoute is empty — LLM-judge DISABLED (fail-open)")
		}
		return nil, nil
	}

	action := normalizeJudgeAction(cfg.Action)                 // CRD default: block
	scanPoint := normalizeScanPoint(cfg.AppliesTo, scanOutput) // CRD default: output
	system := cfg.Policy + judgeInstruction
	ver := sha256.Sum256([]byte(cfg.ModelRoute + "\x00" + system))

	return &semanticJudge{
		modelRoute:    cfg.ModelRoute,
		policy:        system,
		action:        action,
		scanPoint:     scanPoint,
		policyVersion: fmt.Sprintf("%x", ver[:]),
		cache:         make(map[string]judgeVerdict),
		timeout:       judgeTimeout,
	}, nil
}

// normalizeJudgeAction maps the semanticJudge.action string to a guardrailAction. The CRD enum is
// block|auditOnly only (redact makes no sense for a whole-message verdict); a blank/unknown value
// defaults to block (the CRD default) — never silently dropped.
func normalizeJudgeAction(s string) guardrailAction {
	switch guardrailAction(s) {
	case actionAuditOnly:
		return actionAuditOnly
	default:
		return actionBlock // block is both the CRD default and the safe default for an unknown value.
	}
}

// appliesTo reports whether the judge runs at the given scan point. The judge's appliesTo covers a
// point when it is that point exactly or "all" (mirroring the engine's addRule fan-out).
func (j *semanticJudge) appliesTo(point guardrailScanPoint) bool {
	return j.scanPoint == point || j.scanPoint == scanAll
}

// cacheKey is hash(scanned content + policyVersion): a policy change (which rolls policyVersion)
// invalidates every prior verdict, and identical content within a policy generation hits the cache.
func (j *semanticJudge) cacheKey(content string) string {
	h := sha256.Sum256([]byte(j.policyVersion + "\x00" + content))
	return fmt.Sprintf("%x", h[:])
}

// lookup returns a cached verdict for the content, if any.
func (j *semanticJudge) lookup(key string) (judgeVerdict, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	v, ok := j.cache[key]
	return v, ok
}

// store records a verdict, bounding the cache: when it reaches judgeCacheMaxEntries it is cleared
// wholesale before inserting, so it can never grow unbounded. Coarse, but the judge fails open on a
// miss, so eviction is never a correctness risk — only a re-classification cost.
func (j *semanticJudge) store(key string, v judgeVerdict) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.cache) >= judgeCacheMaxEntries {
		j.cache = make(map[string]judgeVerdict, judgeCacheMaxEntries)
	}
	j.cache[key] = v
}

// classify runs the cascaded judge over one piece of text at a scan point and returns a decision to
// enforce, or ok=false when nothing should be enforced (SAFE verdict, judge error/timeout, cache-fed
// SAFE, or judge inapplicable — all of which ALLOW). It is the residual step: callers invoke it only
// after the deterministic scan did NOT block this content.
//
// Fail-OPEN contract: any judge error, timeout, or unparseable verdict returns ok=false (ALLOW) with
// a caller-side log — the judge NEVER blocks on its own failure. A FLAGGED verdict returns a decision
// carrying j.action (block ⇒ refuse, auditOnly ⇒ record) plus a PII-safe content hash (NEVER the raw
// content).
//
// The gatewayProxy is passed so the judge can issue its upstream call THROUGH the same client/auth
// the primary forward() uses (gp.forwardJudge), targeting gp.upstream directly — loop-safe by
// construction.
func (j *semanticJudge) classify(
	ctx context.Context, gp *gatewayProxy, r *http.Request, text string, point guardrailScanPoint,
) (guardrailDecision, bool) {
	if !j.appliesTo(point) || strings.TrimSpace(text) == "" {
		return guardrailDecision{}, false
	}

	h := sha256.Sum256([]byte(text))
	hash := fmt.Sprintf("%x", h[:])
	key := j.cacheKey(text)

	// Cache: identical content (within this policy generation) ⇒ one judge call.
	if v, ok := j.lookup(key); ok {
		if !v.flagged {
			return guardrailDecision{}, false // cached SAFE ⇒ allow.
		}
		return j.flaggedDecision(point, hash), true
	}

	flagged, err := j.ask(ctx, gp, r, text)
	if err != nil {
		// FAIL OPEN: a judge error / timeout / unparseable verdict must not block. Do NOT cache an
		// error (so a transient failure doesn't stick), log it, and allow.
		gp.logf("launcher: gateway: semantic-judge error at scan_point=%s (fail-open, call allowed): %v", point, err)
		return guardrailDecision{}, false
	}

	j.store(key, judgeVerdict{flagged: flagged})
	if !flagged {
		return guardrailDecision{}, false // SAFE ⇒ allow.
	}
	return j.flaggedDecision(point, hash), true
}

// flaggedDecision builds the PII-safe guardrail.decision for a FLAGGED verdict. blocked is true only
// when the judge action is block; an auditOnly judge records the hit but does not refuse. The raw
// content NEVER appears — only the detector label, action, scan point, and content hash.
func (j *semanticJudge) flaggedDecision(point guardrailScanPoint, hash string) guardrailDecision {
	return guardrailDecision{
		blocked:     j.action == actionBlock,
		detector:    judgeDetectorName,
		action:      j.action,
		scanPoint:   point,
		contentHash: hash,
	}
}

// ask issues ONE judge chat-completion to the upstream LiteLLM (via gp.forwardJudge, targeting
// gp.upstream directly with the caller's gateway auth) and parses the verdict. It returns
// (flagged, nil) on a parseable SAFE/FLAGGED reply, or (_, error) on any transport failure, non-200
// upstream status, unreadable body, or unparseable/absent verdict — every one of which the caller
// treats as fail-open.
func (j *semanticJudge) ask(ctx context.Context, gp *gatewayProxy, r *http.Request, text string) (bool, error) {
	reqBody, err := json.Marshal(judgeChatRequest{
		Model: j.modelRoute,
		Messages: []judgeChatMessage{
			{Role: "system", Content: j.policy},
			{Role: roleUser, Content: text},
		},
	})
	if err != nil {
		return false, fmt.Errorf("marshal judge request: %w", err)
	}

	// Bound the judge round-trip with its own short timeout, independent of the caller's context, so a
	// slow judge fails open quickly and never inherits a longer primary-call budget.
	jctx, cancel := context.WithTimeout(ctx, j.timeout)
	defer cancel()

	respBody, status, err := gp.forwardJudge(jctx, r, reqBody)
	if err != nil {
		return false, fmt.Errorf("judge upstream: %w", err)
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("judge upstream status %d", status)
	}

	verdict, ok := parseJudgeVerdict(respBody)
	if !ok {
		return false, fmt.Errorf("judge verdict unparseable")
	}
	return verdict, nil
}

// judgeChatRequest / judgeChatMessage are the minimal OpenAI-style chat/completions request the judge
// sends upstream. LiteLLM normalizes this to whatever provider modelRoute resolves to; we send only
// the fields the classification needs.
type judgeChatRequest struct {
	Model    string             `json:"model"`
	Messages []judgeChatMessage `json:"messages"`
}

type judgeChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// parseJudgeVerdict reads a chat/completions response body and returns (flagged, ok). ok is false
// when the body is not parseable as a completion with a string content, or the content (trimmed,
// upper-cased) is neither FLAGGED nor SAFE — an unparseable/ambiguous verdict, which the caller fails
// open on. "FLAGGED" (case-insensitive, trimmed) ⇒ flagged=true.
func parseJudgeVerdict(body []byte) (flagged bool, ok bool) {
	cr, parsed := parseChatResponse(body)
	if !parsed || len(cr.choices) == 0 {
		return false, false
	}
	ch := cr.choices[0]
	if !ch.hasStringContent {
		return false, false
	}
	switch strings.ToUpper(strings.TrimSpace(ch.content)) {
	case judgeVerdictFlagged:
		return true, true
	case judgeVerdictSafe:
		return false, true
	default:
		return false, false // ambiguous ⇒ unparseable ⇒ fail open.
	}
}

// forwardJudge issues the judge's chat-completion to the REAL upstream LiteLLM (gp.upstream) with the
// same gateway auth the primary path carries. It is deliberately NOT gp.forward(): it builds a FRESH
// request from reqBody rather than proxying the caller's r, so:
//
//   - it targets gp.upstream directly (never localhost:2996 / this proxy) — loop-safe by construction;
//   - it carries ONLY the caller's Authorization + Content-Type (the gateway credential), and NONE of
//     the identity headers (X-Conversation-Id, X-Agent-Name, the run capability) — so the judge's own
//     spend is SYSTEM traffic, exempt from the per-user accumulator and the per-conversation cap by
//     construction (those enforcers read headers this request never sends).
//
// It returns the (capped) upstream body and status. Any transport error is returned for the judge to
// fail open on.
func (gp *gatewayProxy) forwardJudge(ctx context.Context, r *http.Request, reqBody []byte) ([]byte, int, error) {
	target := *gp.upstream
	target.Path = singleJoiningPath(gp.upstream.Path, "/chat/completions")

	outReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(reqBody))
	if err != nil {
		return nil, 0, fmt.Errorf("build judge request: %w", err)
	}
	outReq.Header.Set("Content-Type", "application/json")
	// Carry the gateway credential (the same Authorization the primary call presents to LiteLLM) so the
	// judge authenticates identically. We copy ONLY this header — never the budget/identity headers —
	// so the judge call is system traffic, not billed to the invoking user or conversation.
	if auth := r.Header.Get("Authorization"); auth != "" {
		outReq.Header.Set("Authorization", auth)
	}

	resp, err := gp.client.Do(outReq)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGatewayRespBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read judge response: %w", err)
	}
	return body, resp.StatusCode, nil
}

// judgeRequest applies the cascaded judge to the request scan points (input user-role + toolOutput
// tool-role) AFTER the deterministic request scan did not block. It returns a block decision (ok=true)
// when a FLAGGED+block verdict fires, emitting the PII-safe guardrail.decision for EVERY hit
// (auditOnly hits are recorded here; only a block hit is returned to the caller to refuse). SAFE /
// judge-error / auditOnly all return ok=false (the call proceeds).
//
// Cascade: only runs when j != nil, applies to the point, and the deterministic scan already passed
// (the caller invokes this only on the not-blocked path). The judge is the residual step over the
// exact same messages the deterministic engine scanned (user + tool role). The caller owns the
// ResponseWriter, so this returns the decision rather than writing the 403 itself.
func (gp *gatewayProxy) judgeRequest(
	ctx context.Context, span trace.Span, r *http.Request, body []byte, j *semanticJudge,
) (guardrailDecision, bool) {
	if j == nil || (!j.appliesTo(scanInput) && !j.appliesTo(scanToolOutput)) {
		return guardrailDecision{}, false
	}
	cb, ok := parseChatBody(body)
	if !ok {
		// The deterministic path already fail-closed-blocked an unparseable body before we get here; if
		// we somehow can't parse, the judge simply has nothing to classify (fail open).
		return guardrailDecision{}, false
	}
	for i := range cb.messages {
		m := &cb.messages[i]
		if !m.hasStringContent {
			continue
		}
		var point guardrailScanPoint
		switch m.role {
		case roleUser:
			point = scanInput
		case roleTool:
			point = scanToolOutput
		default:
			continue // system/assistant is the agent's own config, not judged (matches the engine).
		}
		if !j.appliesTo(point) {
			continue
		}
		dec, hit := j.classify(ctx, gp, r, m.content, point)
		if !hit {
			continue
		}
		emitGuardrailDecision(span, dec)
		if dec.blocked {
			return dec, true // block hit: the caller writes the 403 and does NOT forward.
		}
	}
	return guardrailDecision{}, false
}

// judgeOutput applies the cascaded judge to the RESPONSE completion (choices[].message.content) AFTER
// the deterministic output scan did not block. It returns a block decision (ok=true) when a
// FLAGGED+block verdict fires — the caller substitutes the guardrail_blocked body for the completion,
// exactly as a deterministic output block does — emitting the PII-safe guardrail.decision for every
// hit. SAFE / judge-error / auditOnly return ok=false (the completion is relayed).
func (gp *gatewayProxy) judgeOutput(
	ctx context.Context, span trace.Span, r *http.Request, body []byte, j *semanticJudge,
) (guardrailDecision, bool) {
	if j == nil || !j.appliesTo(scanOutput) {
		return guardrailDecision{}, false
	}
	cr, ok := parseChatResponse(body)
	if !ok {
		return guardrailDecision{}, false // an unparseable completion is relayed (fail open here too).
	}
	for i := range cr.choices {
		ch := &cr.choices[i]
		if !ch.hasStringContent {
			continue
		}
		dec, hit := j.classify(ctx, gp, r, ch.content, scanOutput)
		if !hit {
			continue
		}
		emitGuardrailDecision(span, dec)
		if dec.blocked {
			return dec, true
		}
	}
	return guardrailDecision{}, false
}
