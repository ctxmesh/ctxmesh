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

// The in-path guardrail engine (M66, ADR 0059 §8). When the controller resolves a
// spec.guardrailPolicyRef it injects the validated GuardrailPolicySpec as the
// GUARDRAIL_POLICY env and FORCES the outbound :2996 proxy on (m66.2). This file
// builds the deterministic, fail-closed engine from that env and scans the INPUT
// path (the model REQUEST body) before it reaches LiteLLM:
//
//   - block:     a matched input rule refuses the call with a typed 403
//                guardrail_blocked; the upstream is NEVER reached.
//   - auditOnly: a matched input rule is recorded (span event) and the call proceeds.
//   - redact:    output/tool scanning + the redact action are m66.5; the engine is
//                structured so those slot in (rules already carry action + appliesTo).
//
// Fail-closed is the invariant: an active policy whose request body cannot be fully
// scanned (oversize, unparseable JSON, malformed body) is BLOCKED, never truncated
// and forwarded — a guarded call that can't be inspected must not pass.
//
// PII-safe audit (ADR 0059 §6): the guardrail.decision span event carries the
// detector name, action, scan point, a sha256 content HASH, and the match offsets —
// NEVER the matched substring (else the guardrail audit becomes the largest PII
// repository the platform holds).
//
// No GUARDRAIL_POLICY ⇒ newGuardrailEngine returns nil and the request path is
// byte-for-byte unchanged: serve() does not buffer, forward() streams r.Body exactly
// as pre-M66.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ctxmesh/agent-engine/internal/telemetry"
)

const (
	// maxGatewayReqBody caps how much of the model REQUEST body the guardrail engine
	// buffers to scan it, mirroring maxGatewayRespBody (4 MiB). A body that EXCEEDS
	// this cap is fail-closed BLOCKED (never truncate-and-forward): a guarded call
	// whose content can't be fully scanned must not pass. Only engaged under an active
	// policy — with no GUARDRAIL_POLICY the request body is never buffered.
	maxGatewayReqBody = 4 << 20

	// guardrailBlockedType is the STABLE error "type" the proxy returns on a block
	// decision. The gateway-runtime SDK treats guardrail_blocked as non-retryable
	// (m66.6), so this string is a contract surface — do not change it.
	guardrailBlockedType = "guardrail_blocked"

	// guardrailBlockedStatus is the HTTP status a blocked call receives. 403 Forbidden
	// (not a budget-style 402, not a 4xx the SDK would retry): the request is refused
	// on policy grounds, distinct from budget_exceeded (402) and tenant denials.
	guardrailBlockedStatus = 403
)

// guardrailAction is the enforcement action carried on a compiled rule. Mirrors the
// CRD enum (block | redact | auditOnly). redact is defined for m66.5; this task's
// input path enforces block and auditOnly only.
type guardrailAction string

const (
	actionBlock     guardrailAction = "block"
	actionRedact    guardrailAction = "redact"
	actionAuditOnly guardrailAction = "auditOnly"
)

// guardrailScanPoint is the message direction a rule applies to (CRD appliesTo enum).
type guardrailScanPoint string

const (
	scanInput      guardrailScanPoint = "input"
	scanOutput     guardrailScanPoint = "output"
	scanToolOutput guardrailScanPoint = "toolOutput"
	scanAll        guardrailScanPoint = "all"
)

// ── the injected policy shape (mirror of GuardrailPolicySpec) ─────────────────
//
// The controller serializes api/v1beta1.GuardrailPolicySpec verbatim into
// GUARDRAIL_POLICY (guardrail_resolve.go: json.Marshal(policy.Spec)). We define a
// launcher-local mirror rather than importing the API package so the launcher stays
// dependency-light (the budget/runtime config parsing follows the same pattern).
// Only the fields the in-path engine consumes are modeled; unknown fields (the
// LLM-judge, user rate limits) are ignored by json.Unmarshal and handled in later
// tasks.

// guardrailPolicyConfig mirrors GuardrailPolicySpec's JSON.
type guardrailPolicyConfig struct {
	PIIDetectors    *piiGuardrailConfig `json:"piiDetectors,omitempty"`
	PatternDenylist []patternRuleConfig `json:"patternDenylist,omitempty"`
	// failMode is "closed" (default) or "open". Retained so a future task can honor
	// it at engine-run failures; the load path itself is always fail-closed.
	FailMode string `json:"failMode,omitempty"`
}

// piiGuardrailConfig mirrors PIIGuardrail's JSON.
type piiGuardrailConfig struct {
	BuiltIns  *bool                  `json:"builtIns,omitempty"`
	Custom    []customDetectorConfig `json:"custom,omitempty"`
	Action    string                 `json:"action,omitempty"`
	AppliesTo string                 `json:"appliesTo,omitempty"`
}

// customDetectorConfig mirrors CustomDetectorRule's JSON.
type customDetectorConfig struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

// patternRuleConfig mirrors PatternRule's JSON.
type patternRuleConfig struct {
	Name      string `json:"name"`
	Pattern   string `json:"pattern"`
	Action    string `json:"action,omitempty"`
	AppliesTo string `json:"appliesTo,omitempty"`
}

// ── the compiled engine ───────────────────────────────────────────────────────

// guardrailRule is one compiled detector paired with its action + scan point + name.
// The engine indexes rules by scan point so the input path runs only input-applicable
// rules; output/tool rules (m66.5) share the same structure.
type guardrailRule struct {
	detector  telemetry.Detector
	action    guardrailAction
	scanPoint guardrailScanPoint
	name      string
}

// guardrailEngine is the process-wide, immutable guardrail. It holds the compiled
// rules indexed by scan point. Built once at proxy construction from GUARDRAIL_POLICY;
// nil ⇒ no policy ⇒ the request path is byte-for-byte unchanged.
type guardrailEngine struct {
	// input holds every rule whose appliesTo covers the input direction (input | all).
	// output/toolOutput indices are populated the same way for m66.5.
	input      []guardrailRule
	output     []guardrailRule
	toolOutput []guardrailRule
}

// newGuardrailEngine builds the engine from the raw GUARDRAIL_POLICY JSON. An empty
// string ⇒ (nil, nil): no policy, the caller keeps the pre-M66 streaming path.
//
// Fail-closed load: the policy JSON must parse and EVERY pattern must compile. The
// m66.2 controller already validated patterns before injecting (a bad pattern holds
// the agent NotReady), so an error here is defence-in-depth — but it is still a hard
// error, never a silent fallback to a weaker detector set (ADR 0059 §8).
func newGuardrailEngine(policyJSON string) (*guardrailEngine, error) {
	if policyJSON == "" {
		return nil, nil
	}

	var cfg guardrailPolicyConfig
	if err := json.Unmarshal([]byte(policyJSON), &cfg); err != nil {
		return nil, fmt.Errorf("guardrail: parsing GUARDRAIL_POLICY: %w", err)
	}

	eng := &guardrailEngine{}

	// PII detectors: built-ins (when enabled) + custom, via the FAIL-CLOSED strict
	// builder — a single bad custom pattern returns (nil, error), never a degraded set.
	if cfg.PIIDetectors != nil {
		pii := cfg.PIIDetectors
		builtIns := pii.BuiltIns == nil || *pii.BuiltIns // CRD default is true
		custom := make([]telemetry.CustomDetectorSpec, 0, len(pii.Custom))
		for _, c := range pii.Custom {
			custom = append(custom, telemetry.CustomDetectorSpec{Name: c.Name, Pattern: c.Pattern})
		}

		var detectors []telemetry.Detector
		if builtIns {
			// Built-ins + custom (strict). DetectorsWithCustomStrict always includes the
			// built-in defaults; custom patterns are validated fail-closed.
			ds, err := telemetry.DetectorsWithCustomStrict(custom)
			if err != nil {
				return nil, fmt.Errorf("guardrail: %w", err)
			}
			detectors = ds
		} else {
			// Built-ins disabled: use ONLY the (strictly validated) custom detectors. Build
			// them via the strict path to validate, then drop the built-in defaults it
			// prepends so a builtIns=false policy does not silently re-enable them.
			ds, err := telemetry.DetectorsWithCustomStrict(custom)
			if err != nil {
				return nil, fmt.Errorf("guardrail: %w", err)
			}
			detectors = ds[len(telemetry.DefaultDetectors()):]
		}

		action := normalizeAction(pii.Action, actionRedact) // CRD default: redact
		scanPoint := normalizeScanPoint(pii.AppliesTo, scanAll)
		for _, d := range detectors {
			eng.addRule(guardrailRule{detector: d, action: action, scanPoint: scanPoint, name: d.Name})
		}
	}

	// patternDenylist: each named RE2 pattern is its own single-detector rule, carrying
	// its own action + appliesTo. Compiled fail-closed via the strict builder.
	for _, p := range cfg.PatternDenylist {
		ds, err := telemetry.DetectorsWithCustomStrict([]telemetry.CustomDetectorSpec{{Name: p.Name, Pattern: p.Pattern}})
		if err != nil {
			return nil, fmt.Errorf("guardrail: patternDenylist %q: %w", p.Name, err)
		}
		// DetectorsWithCustomStrict prepends the built-in defaults; the denylist rule is the
		// LAST detector (the one we just appended). Take only it.
		rule := ds[len(ds)-1]
		action := normalizeAction(p.Action, actionBlock) // CRD default: block
		scanPoint := normalizeScanPoint(p.AppliesTo, scanAll)
		eng.addRule(guardrailRule{detector: rule, action: action, scanPoint: scanPoint, name: p.Name})
	}

	return eng, nil
}

// addRule indexes a rule under every scan point its appliesTo covers. "all" fans out
// to input, output, and toolOutput so a single rule is enforced on every direction.
func (e *guardrailEngine) addRule(r guardrailRule) {
	switch r.scanPoint {
	case scanInput:
		e.input = append(e.input, r)
	case scanOutput:
		e.output = append(e.output, r)
	case scanToolOutput:
		e.toolOutput = append(e.toolOutput, r)
	case scanAll:
		e.input = append(e.input, r)
		e.output = append(e.output, r)
		e.toolOutput = append(e.toolOutput, r)
	}
}

// normalizeAction maps the CRD action string to a guardrailAction, defaulting when
// blank (the controller injects the CRD default, but a blank is treated as the
// dflt — never silently dropped).
func normalizeAction(s string, dflt guardrailAction) guardrailAction {
	switch guardrailAction(s) {
	case actionBlock:
		return actionBlock
	case actionRedact:
		return actionRedact
	case actionAuditOnly:
		return actionAuditOnly
	default:
		return dflt
	}
}

// normalizeScanPoint maps the CRD appliesTo string to a guardrailScanPoint, defaulting
// when blank.
func normalizeScanPoint(s string, dflt guardrailScanPoint) guardrailScanPoint {
	switch guardrailScanPoint(s) {
	case scanInput:
		return scanInput
	case scanOutput:
		return scanOutput
	case scanToolOutput:
		return scanToolOutput
	case scanAll:
		return scanAll
	default:
		return dflt
	}
}

// ── the input scan decision ───────────────────────────────────────────────────

// guardrailDecision is the outcome of scanning a scan point. It is emitted as a
// PII-safe guardrail.decision span event (detector, action, scan point, content hash,
// offsets — NEVER the raw match).
type guardrailDecision struct {
	// blocked is true iff a block-action rule matched: the call must be refused.
	blocked bool
	// detector is the name of the rule that produced this decision (the first hit).
	detector string
	// action is the action of the matched rule.
	action guardrailAction
	// scanPoint is where the scan ran (input, for m66.4).
	scanPoint guardrailScanPoint
	// contentHash is sha256(scanned text) — the audit key that never leaks content.
	contentHash string
	// startOffset / endOffset are the [start,end) byte span of the match within the
	// scanned text. Offsets locate the hit for audit without carrying the substring.
	startOffset int
	endOffset   int
}

// chatCompletionRequest is the minimal shape of an OpenAI-style chat/completions body
// the input scan reads: the messages array with role + content. Extra fields are
// ignored. content is decoded as json.RawMessage first so a non-string content (e.g.
// the multimodal array form) does not fail the whole parse — such a message simply
// contributes no scannable user text (handled in scanInput).
type chatCompletionRequest struct {
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// scanInputResult reports the outcome of scanning the buffered request body.
type scanInputResult struct {
	// block is true when the call must be refused: a block rule hit, OR the body could
	// not be scanned (unparseable JSON) under an active policy — fail-closed.
	block bool
	// decisions is every recorded decision (block + auditOnly hits) for span emission.
	decisions []guardrailDecision
	// failClosedReason, when non-empty, explains a fail-closed block that was NOT a rule
	// hit (e.g. malformed JSON). detector on the block decision is then a synthetic name.
	failClosedReason string
}

// scanInput scans the user-role message content of a buffered chat/completions body
// against the input-applicable rules. It returns a scanInputResult:
//
//   - malformed JSON under an active policy ⇒ block=true, fail-closed (can't parse ⇒
//     can't scan ⇒ deny). This is NOT a silent pass.
//   - a block rule hit ⇒ block=true, plus the decision.
//   - auditOnly hits ⇒ block=false, decisions recorded, the caller forwards.
//   - clean ⇒ block=false, no decisions.
//
// The scanned text is the concatenation of every user-role message's content
// (newline-joined), so a match's offset is stable relative to that joined text and
// the hash is over exactly what was scanned.
func (e *guardrailEngine) scanInput(body []byte) scanInputResult {
	var req chatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// Fail-closed: an active policy cannot scan a body it cannot parse.
		h := sha256.Sum256(body)
		return scanInputResult{
			block:            true,
			failClosedReason: "unparseable request body",
			decisions: []guardrailDecision{{
				blocked:     true,
				detector:    "malformed-request",
				action:      actionBlock,
				scanPoint:   scanInput,
				contentHash: fmt.Sprintf("%x", h[:]),
			}},
		}
	}

	scanned := userContent(req.Messages)
	return e.scanText(scanned, scanInput, e.input)
}

// scanText runs the given rules over text at a scan point and reduces the hits to a
// scanInputResult. On the FIRST block-action hit it stops scanning further rules
// (the call is already refused) and returns a block decision; auditOnly hits are all
// recorded and the caller proceeds. A clean scan returns an empty result.
func (e *guardrailEngine) scanText(text string, point guardrailScanPoint, rules []guardrailRule) scanInputResult {
	if len(rules) == 0 {
		return scanInputResult{}
	}
	h := sha256.Sum256([]byte(text))
	hash := fmt.Sprintf("%x", h[:])

	var res scanInputResult
	for i := range rules {
		r := rules[i]
		matches := telemetry.Scan(text, []telemetry.Detector{r.detector})
		if len(matches) == 0 {
			continue
		}
		m := matches[0] // first (leftmost) hit — offsets locate it for audit.
		dec := guardrailDecision{
			detector:    r.name,
			action:      r.action,
			scanPoint:   point,
			contentHash: hash,
			startOffset: m.Start,
			endOffset:   m.End,
		}
		switch r.action {
		case actionBlock:
			// A block hit refuses the call. Record it and stop — the decision is made.
			dec.blocked = true
			res.block = true
			res.decisions = append(res.decisions, dec)
			return res
		case actionAuditOnly:
			// Record and continue; the call proceeds.
			res.decisions = append(res.decisions, dec)
		case actionRedact:
			// m66.5 implements output/tool redaction. On the input path a redact rule is
			// recorded as an auditOnly-equivalent (the content is not mutated here) so the
			// hit is not silently dropped; the call proceeds.
			res.decisions = append(res.decisions, dec)
		}
	}
	return res
}

// userContent concatenates the content of every user-role message, newline-joined.
// This is the input the guardrail scans: the user's own prompt text (system/assistant
// content is the agent's own configuration, not the untrusted input the input path
// guards). Empty when there are no user messages (a clean scan).
func userContent(msgs []chatMessage) string {
	var b []byte
	first := true
	for i := range msgs {
		if msgs[i].Role != "user" {
			continue
		}
		if !first {
			b = append(b, '\n')
		}
		b = append(b, msgs[i].Content...)
		first = false
	}
	return string(b)
}

// guardrailErrorBody is the typed refusal body for a blocked call. The "type" field
// is the STABLE contract the SDK keys non-retryability on (m66.6) — do not rename it.
// scan_point tells the caller where the block originated (input, for m66.4). No raw
// matched content is ever included.
type guardrailErrorBody struct {
	Error guardrailErrorDetail `json:"error"`
}

type guardrailErrorDetail struct {
	Type      string `json:"type"`
	Detector  string `json:"detector"`
	ScanPoint string `json:"scan_point"`
}

// ── proxy integration ─────────────────────────────────────────────────────────

// applyInputGuardrail buffers the request body, scans the input path, emits a PII-safe
// guardrail.decision span event per decision, and — on a block — writes the typed
// refusal and returns true (refused; the caller must NOT forward). It returns false
// when the call is cleared to forward, having restored the buffered body onto r.Body
// for forward() to stream.
//
// It is called ONLY when gp.guardrail != nil; with no policy this whole path is
// skipped and the body is never buffered (byte-for-byte-unchanged invariant).
//
// Fail-closed cases (all BLOCK, never a silent pass):
//   - body exceeds maxGatewayReqBody ⇒ the content can't be fully scanned.
//   - body read error ⇒ can't obtain the content to scan.
//   - unparseable request JSON ⇒ can't locate the messages to scan (scanInput).
func (gp *gatewayProxy) applyInputGuardrail(w http.ResponseWriter, span trace.Span, r *http.Request) (refused bool) {
	// Buffer the body with a limit of maxGatewayReqBody + 1 sentinel byte: if the read
	// yields MORE than the cap, the body is oversize and we fail closed rather than
	// truncate-and-forward.
	buffered, oversize, err := readLimited(r.Body, maxGatewayReqBody)
	if r.Body != nil {
		_ = r.Body.Close()
	}
	if err != nil {
		gp.writeGuardrailBlocked(w, span, guardrailDecision{
			blocked: true, detector: "unreadable-request", action: actionBlock, scanPoint: scanInput,
		})
		gp.logf("launcher: gateway: guardrail fail-closed BLOCK (request body read error: %v)", err)
		return true
	}
	if oversize {
		gp.writeGuardrailBlocked(w, span, guardrailDecision{
			blocked: true, detector: "oversize-request", action: actionBlock, scanPoint: scanInput,
		})
		gp.logf("launcher: gateway: guardrail fail-closed BLOCK (request body exceeds %d bytes)", maxGatewayReqBody)
		return true
	}

	// Restore the body for forward() BEFORE any decision so the clean/auditOnly paths
	// stream the exact buffered bytes upstream. Blocked paths return without forwarding,
	// so the restored reader is simply unused (harmless).
	r.Body = io.NopCloser(bytes.NewReader(buffered))
	r.ContentLength = int64(len(buffered))

	res := gp.guardrail.scanInput(buffered)

	// Emit a PII-safe span event for EVERY decision (block + auditOnly). Never carries
	// the raw matched substring — only the detector, action, scan point, content hash,
	// and offsets (ADR 0059 §6).
	for _, dec := range res.decisions {
		emitGuardrailDecision(span, dec)
	}

	if res.block {
		// The first block decision is the refusal cause (scanInput returns it first).
		cause := res.decisions[0]
		gp.writeGuardrailBlocked(w, span, cause)
		reason := res.failClosedReason
		if reason == "" {
			reason = "policy match"
		}
		gp.logf("launcher: gateway: guardrail BLOCK detector=%s scan_point=%s (%s; call refused)",
			cause.detector, cause.scanPoint, reason)
		return true
	}

	return false
}

// readLimited reads up to limit bytes from rc. It returns (bytes, oversize, err):
// oversize is true iff the source held MORE than limit bytes (read via a limit+1
// sentinel), in which case the returned bytes are the first limit+1 and the caller
// must fail closed rather than use them. A nil body reads as empty (not oversize).
func readLimited(rc io.Reader, limit int64) (data []byte, oversize bool, err error) {
	if rc == nil {
		return nil, false, nil
	}
	// Read one extra byte: if we get it, the body was strictly larger than the cap.
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(b)) > limit {
		return b, true, nil
	}
	return b, false, nil
}

// emitGuardrailDecision adds the PII-safe guardrail.decision span event (mirrors the
// budget.alert event). It carries detector, action, scan_point, the content HASH, and
// the match offsets — and NEVER the matched substring, so the guardrail audit trail
// cannot become a PII repository (ADR 0059 §6). For fail-closed decisions with no rule
// match (e.g. malformed body) the offsets are 0/0 and the hash is over the raw body.
func emitGuardrailDecision(span trace.Span, dec guardrailDecision) {
	span.AddEvent("guardrail.decision", trace.WithAttributes(
		attribute.String("guardrail.detector", dec.detector),
		attribute.String("guardrail.action", string(dec.action)),
		attribute.String("guardrail.scan_point", string(dec.scanPoint)),
		attribute.Bool("guardrail.blocked", dec.blocked),
		attribute.String("guardrail.content_hash", dec.contentHash),
		attribute.Int("guardrail.match_start", dec.startOffset),
		attribute.Int("guardrail.match_end", dec.endOffset),
	))
}

// writeGuardrailBlocked writes the typed guardrail_blocked refusal (403 + OpenAI-shaped
// error body) and marks the span. The upstream is NOT called — this is the input-path
// circuit-break. The "type" is the stable guardrail_blocked string the SDK keys
// non-retryability on (m66.6). No raw matched content is ever written.
func (gp *gatewayProxy) writeGuardrailBlocked(w http.ResponseWriter, span trace.Span, dec guardrailDecision) {
	span.SetAttributes(
		attribute.String("guardrail.decision", string(actionBlock)),
		attribute.String("guardrail.detector", dec.detector),
		attribute.String("guardrail.scan_point", string(dec.scanPoint)),
	)
	span.SetStatus(codes.Error, guardrailBlockedType)

	body := guardrailErrorBody{Error: guardrailErrorDetail{
		Type:      guardrailBlockedType,
		Detector:  dec.detector,
		ScanPoint: string(dec.scanPoint),
	}}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(guardrailBlockedStatus) // 403
	if err := json.NewEncoder(w).Encode(body); err != nil {
		gp.logf("launcher: gateway: encode guardrail_blocked: %v", err)
	}
}
