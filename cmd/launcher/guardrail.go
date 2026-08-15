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
// builds the deterministic, fail-closed engine from that env and scans every scan
// point on the model REQUEST and RESPONSE bodies:
//
//   - input      (request, user-role messages): the untrusted user prompt.
//   - toolOutput (request, tool-role messages): tool results re-enter as tool-role
//     messages on the next model call — scanning them here catches injected
//     instructions BEFORE the model consumes them. This is a TRIPWIRE for known
//     patterns (posture), NOT injection resistance (ADR 0059 §3).
//   - output     (response, choices[].message.content): the model completion.
//
// Each rule carries an action:
//
//   - block:     the matched call is refused with a typed 403 guardrail_blocked.
//                On an input/tool block the upstream is NEVER reached; on an OUTPUT
//                block the model already generated, so spend is still booked (ADR
//                0059 §7) and the completion is REPLACED with the refusal.
//   - auditOnly: recorded (span event) and the content passes unchanged.
//   - redact:    the matched substring is replaced with telemetry.RedactString's
//                [REDACTED:<name>] marker and the body is RE-SERIALIZED — the scrubbed
//                request is forwarded (input/tool) or the scrubbed response relayed
//                (output). Only the matched message/completion STRING changes; every
//                other field (tool_calls, non-string content, role, ...) is preserved.
//
// Precedence within a scan point: block > redact > auditOnly. A block short-circuits
// (a blocked call is never also redacted); redactions are applied to the surviving body.
//
// Fail-closed is the invariant: an active policy whose request body cannot be fully
// scanned (oversize, unparseable JSON, malformed body) is BLOCKED, never truncated
// and forwarded — a guarded call that can't be inspected must not pass. (The response
// body is already buffered and priced by M8; an unparseable RESPONSE is relayed as-is
// — a completion is the model's own output being sanitised on the way OUT, not
// untrusted input inspected before it runs, and failing every non-JSON stream closed
// would be wrong.)
//
// PII-safe audit (ADR 0059 §6): the guardrail.decision span event carries the
// detector name, action, scan point, a sha256 content HASH, and the match offsets —
// NEVER the matched substring (else the guardrail audit becomes the largest PII
// repository the platform holds). This holds for redact decisions too.
//
// No GUARDRAIL_POLICY ⇒ newGuardrailEngine returns nil and BOTH paths are byte-for-byte
// unchanged: serve() does not buffer the request, forward() streams r.Body exactly as
// pre-M66, and the response is relayed verbatim.

import (
	"bytes"
	"context"
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

	// guardrailStreamingUnsupportedType is the STABLE error "type" the proxy returns
	// when a guarded agent sends a stream:true request (m66.6, ADR 0059 §4). Streaming
	// is incompatible with output-blocking: you cannot un-send tokens already streamed
	// to the client. Guarded agents are buffered-only; the SDK must never retry this.
	guardrailStreamingUnsupportedType = "guardrail_streaming_unsupported"
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

// Chat message roles the guardrail scans map to scan points: a user-role message is the untrusted
// input, a tool-role message is a re-entering tool result (toolOutput). system/assistant are the
// agent's own config and are never scanned. Shared by scanRequest and the LLM-judge cascade.
const (
	roleUser = "user"
	roleTool = "tool"
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
	// UserRateLimit configures per-end-user (OBO) rate/abuse limits (m66.7). Read by the
	// gateway proxy (gateway.go) to build the per-user quota enforcer; nil ⇒ no per-user
	// limit. The engine's content rules ignore it.
	UserRateLimit *userRateLimitConfig `json:"userRateLimit,omitempty"`
	// failMode is "closed" (default) or "open". Retained so a future task can honor
	// it at engine-run failures; the load path itself is always fail-closed.
	FailMode string `json:"failMode,omitempty"`
}

// userRateLimitConfig mirrors UserRateLimit's JSON (api/v1beta1). Zero/blank fields ⇒
// that dimension is unenforced (matching the CRD "0 means unlimited" contract).
type userRateLimitConfig struct {
	RequestsPerMinute int    `json:"requestsPerMinute,omitempty"`
	SpendUSD          string `json:"spendUSD,omitempty"`
	MaxInFlight       int    `json:"maxInFlight,omitempty"`
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

// parseUserRateLimit extracts the userRateLimit section from the raw GUARDRAIL_POLICY
// JSON (m66.7). Empty policy or no userRateLimit ⇒ (nil, nil): no per-user limit. A
// policy that does not parse is a hard error — same fail-closed load posture as
// newGuardrailEngine (the controller already validated the policy, so this is
// defence-in-depth). Kept separate from newGuardrailEngine because the per-user quota
// is enforced by the gateway proxy (gateway.go), not the content-scanning engine.
func parseUserRateLimit(policyJSON string) (*userRateLimitConfig, error) {
	if policyJSON == "" {
		return nil, nil
	}
	var cfg guardrailPolicyConfig
	if err := json.Unmarshal([]byte(policyJSON), &cfg); err != nil {
		return nil, fmt.Errorf("guardrail: parsing GUARDRAIL_POLICY: %w", err)
	}
	return cfg.UserRateLimit, nil
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
	// scanPoint is where the scan ran (input | toolOutput | output).
	scanPoint guardrailScanPoint
	// contentHash is sha256(scanned text) — the audit key that never leaks content.
	contentHash string
	// startOffset / endOffset are the [start,end) byte span of the match within the
	// scanned text. Offsets locate the hit for audit without carrying the substring.
	startOffset int
	endOffset   int
}

// chatBody is a fidelity-preserving view of an OpenAI-style chat/completions REQUEST
// body. The top-level object is kept as an ordered-insensitive map of RawMessage so
// EVERY field (model, temperature, tools, ...) round-trips untouched; only "messages"
// is decoded structurally. Redaction mutates a single message's "content" string and
// re-marshals — non-string content, tool_calls, name, tool_call_id and any other
// message field are preserved verbatim because each message is itself held as a map of
// RawMessage (see chatMsg).
type chatBody struct {
	top      map[string]json.RawMessage
	messages []chatMsg
}

// chatMsg is one message. raw holds every field of the message object; role and
// stringContent are the DECODED views the scanner needs. hasStringContent is false when
// content is absent or a non-string (e.g. the multimodal array form) — such a message
// contributes no scannable text and is never mutated.
type chatMsg struct {
	raw              map[string]json.RawMessage
	role             string
	content          string
	hasStringContent bool
}

// parseChatBody decodes body into a chatBody, or returns ok=false when the body is not
// a JSON object with a messages array we can read (⇒ the request path fails closed).
func parseChatBody(body []byte) (*chatBody, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false
	}
	rawMsgs, present := top["messages"]
	if !present {
		// No messages key: nothing to scan, but the body is well-formed JSON. Treat it as
		// an empty message set (a clean scan) rather than a fail-closed block.
		return &chatBody{top: top}, true
	}
	var msgObjs []map[string]json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgObjs); err != nil {
		return nil, false
	}
	cb := &chatBody{top: top, messages: make([]chatMsg, 0, len(msgObjs))}
	for _, mo := range msgObjs {
		m := chatMsg{raw: mo}
		if rr, ok := mo["role"]; ok {
			_ = json.Unmarshal(rr, &m.role) // best-effort; a non-string role stays ""
		}
		if cr, ok := mo["content"]; ok {
			var s string
			if err := json.Unmarshal(cr, &s); err == nil {
				m.content = s
				m.hasStringContent = true
			}
		}
		cb.messages = append(cb.messages, m)
	}
	return cb, true
}

// setMessageContent rewrites the string content of message i and re-marshals ONLY that
// field back into its raw map, so every other field (tool_calls, name, ...) survives.
func (cb *chatBody) setMessageContent(i int, content string) error {
	enc, err := json.Marshal(content)
	if err != nil {
		return err
	}
	cb.messages[i].raw["content"] = enc
	cb.messages[i].content = content
	return nil
}

// marshal re-serialises the (possibly mutated) body. It rebuilds the messages array from
// each message's raw map and writes it back under "messages", preserving all other
// top-level fields. Only fields we deliberately rewrote differ from the input.
func (cb *chatBody) marshal() ([]byte, error) {
	if cb.messages != nil {
		rawMsgs := make([]map[string]json.RawMessage, len(cb.messages))
		for i := range cb.messages {
			rawMsgs[i] = cb.messages[i].raw
		}
		enc, err := json.Marshal(rawMsgs)
		if err != nil {
			return nil, err
		}
		cb.top["messages"] = enc
	}
	return json.Marshal(cb.top)
}

// scanResult reports the outcome of scanning (and possibly redacting) a body.
type scanResult struct {
	// block is true when the call must be refused: a block rule hit, OR (request path
	// only) the body could not be scanned under an active policy — fail-closed.
	block bool
	// redacted is true when at least one redact rule mutated the body; the caller must
	// re-serialise and forward/relay the SCRUBBED body.
	redacted bool
	// decisions is every recorded decision (block + redact + auditOnly hits) for span
	// emission. Never carries the raw match.
	decisions []guardrailDecision
	// failClosedReason, when non-empty, explains a fail-closed block that was NOT a rule
	// hit (e.g. malformed JSON). detector on the block decision is then a synthetic name.
	failClosedReason string
}

// failClosed builds a fail-closed block result whose hash is over the raw body.
func failClosed(reason, detector string, point guardrailScanPoint, body []byte) scanResult {
	h := sha256.Sum256(body)
	return scanResult{
		block:            true,
		failClosedReason: reason,
		decisions: []guardrailDecision{{
			blocked:     true,
			detector:    detector,
			action:      actionBlock,
			scanPoint:   point,
			contentHash: fmt.Sprintf("%x", h[:]),
		}},
	}
}

// scanRequest scans the REQUEST body's input (user-role) and toolOutput (tool-role)
// scan points and applies the redact action in place. It returns a scanResult plus the
// (possibly re-serialised) body to forward:
//
//   - malformed body under an active policy ⇒ block, fail-closed (can't parse ⇒ can't
//     scan ⇒ deny). NOT a silent pass.
//   - a block hit (input or tool) ⇒ block; the caller refuses BEFORE forwarding.
//   - redact hits ⇒ the matched content is scrubbed in place; block=false, redacted=true,
//     and the returned body is the re-serialised SCRUBBED request.
//   - auditOnly / clean ⇒ block=false, redacted=false; the original body forwards.
//
// Precedence: a block on any scanned message wins over redact wins over auditOnly. A
// block short-circuits (the body is not redacted — the call is refused outright).
func (e *guardrailEngine) scanRequest(body []byte) (scanResult, []byte) {
	cb, ok := parseChatBody(body)
	if !ok {
		return failClosed("unparseable request body", "malformed-request", scanInput, body), body
	}

	var res scanResult
	for i := range cb.messages {
		m := &cb.messages[i]
		var rules []guardrailRule
		var point guardrailScanPoint
		switch m.role {
		case roleUser:
			rules, point = e.input, scanInput
		case roleTool:
			rules, point = e.toolOutput, scanToolOutput
		default:
			continue // system/assistant content is the agent's own config, not scanned.
		}
		if len(rules) == 0 || !m.hasStringContent {
			continue
		}
		mres, newContent, changed := e.scanAndRedact(m.content, point, rules)
		res.decisions = append(res.decisions, mres.decisions...)
		if mres.block {
			// Block wins outright: refuse the whole call, do not redact anything.
			res.block = true
			return res, body
		}
		if changed {
			if err := cb.setMessageContent(i, newContent); err != nil {
				// Re-marshalling a redacted message must not silently forward the RAW content;
				// fail closed instead (defence-in-depth — marshalling a string cannot realistically fail).
				return failClosed("redaction re-serialisation failed", "redact-serialize", point, body), body
			}
			res.redacted = true
		}
	}

	if !res.redacted {
		return res, body // nothing mutated: forward the original bytes.
	}
	out, err := cb.marshal()
	if err != nil {
		return failClosed("request re-serialisation failed", "redact-serialize", scanInput, body), body
	}
	return res, out
}

// scanText runs the given rules over text at a scan point WITHOUT mutating it, reducing
// the hits to a scanResult. On the FIRST block-action hit it stops (the call is already
// refused). auditOnly + redact hits are all recorded; a redact hit is reported to the
// caller via scanAndRedact, which layers the mutation on top. A clean scan is empty.
//
// Precedence is realised by scanning in a single pass: block short-circuits, so a later
// block cannot be masked by an earlier redact/auditOnly, and a block seen first prevents
// any redaction. Redact vs auditOnly do not conflict (both can apply to disjoint hits).
func (e *guardrailEngine) scanText(text string, point guardrailScanPoint, rules []guardrailRule) scanResult {
	if len(rules) == 0 {
		return scanResult{}
	}
	h := sha256.Sum256([]byte(text))
	hash := fmt.Sprintf("%x", h[:])

	var res scanResult
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
			// A block hit refuses the call. Record it and stop — the decision is made and
			// block wins over any redact/auditOnly that would otherwise follow.
			dec.blocked = true
			res.block = true
			res.decisions = append(res.decisions, dec)
			return res
		case actionAuditOnly, actionRedact:
			// Record and continue. redact's mutation is layered on by scanAndRedact; here we
			// only record the (PII-safe) decision so the hit is never silently dropped.
			res.decisions = append(res.decisions, dec)
		}
	}
	return res
}

// scanAndRedact scans text and, when a redact rule matched (and no block short-circuited),
// returns the redacted text. It reports (result, newText, changed): changed is true only
// when a redact rule actually altered the text. The redact decisions already recorded by
// scanText carry the PII-safe hash+offsets of the ORIGINAL text (the audit key), while the
// forwarded/relayed body carries the [REDACTED:<name>] marker — the raw match appears in
// neither.
func (e *guardrailEngine) scanAndRedact(
	text string, point guardrailScanPoint, rules []guardrailRule,
) (res scanResult, newText string, changed bool) {
	res = e.scanText(text, point, rules)
	if res.block {
		return res, text, false // block short-circuits: never redact a refused call.
	}
	// Apply every redact rule's detector to the text. Only the matched substrings change.
	redactDetectors := make([]telemetry.Detector, 0, len(rules))
	for i := range rules {
		if rules[i].action == actionRedact {
			redactDetectors = append(redactDetectors, rules[i].detector)
		}
	}
	if len(redactDetectors) == 0 {
		return res, text, false
	}
	redacted := telemetry.RedactString(text, redactDetectors)
	if redacted == text {
		return res, text, false // a redact rule was configured but nothing matched.
	}
	return res, redacted, true
}

// guardrailErrorBody is the typed refusal body for a blocked call. The "type" field
// is the STABLE contract the SDK keys non-retryability on (m66.6) — do not rename it.
// scan_point tells the caller where the block originated (input | toolOutput | output).
// No raw matched content is ever included.
type guardrailErrorBody struct {
	Error guardrailErrorDetail `json:"error"`
}

type guardrailErrorDetail struct {
	Type      string `json:"type"`
	Detector  string `json:"detector"`
	ScanPoint string `json:"scan_point"`
	Message   string `json:"message,omitempty"`
}

// writeGuardrailStreamingUnsupported writes the typed guardrail_streaming_unsupported
// rejection (422 + OpenAI-shaped error body) when a guarded agent sends a stream:true
// request. Streaming is incompatible with output-blocking (ADR 0059 §4): tokens already
// streamed to the client cannot be un-sent if an output rule later trips. Guarded agents
// MUST use the buffered path (stream:false); the SDK's stream/stream_completion methods
// are therefore off-limits for guarded agents. The upstream is NOT called.
func (gp *gatewayProxy) writeGuardrailStreamingUnsupported(w http.ResponseWriter, span trace.Span) {
	span.SetAttributes(
		attribute.String("guardrail.decision", string(actionBlock)),
		attribute.String("guardrail.detector", "streaming-unsupported"),
		attribute.String("guardrail.scan_point", "input"),
	)
	span.SetStatus(codes.Error, guardrailStreamingUnsupportedType)

	body, _ := json.Marshal(guardrailErrorBody{Error: guardrailErrorDetail{
		Type:    guardrailStreamingUnsupportedType,
		Message: "guarded agents are buffered-only; remove stream:true from the request",
	}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity) // 422
	if _, err := w.Write(body); err != nil {
		gp.logf("launcher: gateway: write guardrail_streaming_unsupported: %v", err)
	}
	gp.logf("launcher: gateway: guardrail REJECT stream:true (guarded agents are buffered-only, ADR 0059 §4)")
}

// ── proxy integration ─────────────────────────────────────────────────────────

// applyRequestGuardrail buffers the request body, scans BOTH request scan points —
// input (user-role) and toolOutput (tool-role) — emits a PII-safe guardrail.decision
// span event per decision, and:
//
//   - on a block: writes the typed 403 refusal and returns true (refused; the caller
//     must NOT forward; the upstream is never reached).
//   - on a redact hit: scrubs the matched content in place, re-serialises the body, and
//     restores the SCRUBBED bytes onto r.Body so forward() streams the redacted request.
//   - clean / auditOnly: restores the exact buffered bytes onto r.Body.
//
// It is called ONLY when pol.engine != nil; with no policy this whole path is
// skipped and the body is never buffered (byte-for-byte-unchanged invariant). The engine +
// judge come from the per-request bundle snapshot (pol) so a concurrent K3 reload cannot split
// the scan across two policies.
//
// Fail-closed cases (all BLOCK, never a silent pass):
//   - stream:true in the request body when a policy is active ⇒ streaming is incompatible
//     with output-blocking (ADR 0059 §4): guarded agents are buffered-only.
//   - body exceeds maxGatewayReqBody ⇒ the content can't be fully scanned.
//   - body read error ⇒ can't obtain the content to scan.
//   - unparseable request JSON ⇒ can't locate the messages to scan (scanRequest).
//   - a redaction that cannot be re-serialised ⇒ never forward the raw content.
func (gp *gatewayProxy) applyRequestGuardrail(
	w http.ResponseWriter, span trace.Span, r *http.Request, pol *guardrailBundle,
) (refused bool) {
	// Buffer the body with a limit of maxGatewayReqBody + 1 sentinel byte: if the read
	// yields MORE than the cap, the body is oversize and we fail closed rather than
	// truncate-and-forward.
	buffered, oversize, err := readLimited(r.Body, maxGatewayReqBody)
	if r.Body != nil {
		_ = r.Body.Close()
	}
	if err != nil {
		failDec := guardrailDecision{blocked: true, detector: "unreadable-request", action: actionBlock, scanPoint: scanInput}
		gp.writeGuardrailBlocked(w, span, failDec)
		gp.logf("launcher: gateway: guardrail fail-closed BLOCK (request body read error: %v)", err)
		gp.fireGuardrailBlockAudit(r, failDec) // m66.9: durable record even for fail-closed blocks
		return true
	}
	if oversize {
		failDec := guardrailDecision{blocked: true, detector: "oversize-request", action: actionBlock, scanPoint: scanInput}
		gp.writeGuardrailBlocked(w, span, failDec)
		gp.logf("launcher: gateway: guardrail fail-closed BLOCK (request body exceeds %d bytes)", maxGatewayReqBody)
		gp.fireGuardrailBlockAudit(r, failDec) // m66.9: durable record even for fail-closed blocks
		return true
	}

	// Streaming incompatibility check (m66.6, ADR 0059 §4): a guarded agent MUST NOT
	// use stream:true. Output-blocking cannot un-send tokens already streamed to the
	// client, so the guardrail can only work on a fully-buffered response. Reject the
	// call with a typed guardrail_streaming_unsupported BEFORE content-scanning.
	// This uses the already-buffered body (parseChatBody is cheap) — no extra read.
	if requestHasStreamTrue(buffered) {
		// Restore the body so the caller is not surprised (though we return refused=true).
		r.Body = io.NopCloser(bytes.NewReader(buffered))
		r.ContentLength = int64(len(buffered))
		gp.writeGuardrailStreamingUnsupported(w, span)
		return true
	}

	res, forwardBody := pol.engine.scanRequest(buffered)

	// Restore the (possibly scrubbed) body for forward() BEFORE any decision so the
	// clean/auditOnly/redact paths stream the right bytes upstream. On redact this is the
	// SCRUBBED body — the model receives [REDACTED:<name>], never the raw content. Blocked
	// paths return without forwarding, so the restored reader is simply unused (harmless).
	r.Body = io.NopCloser(bytes.NewReader(forwardBody))
	r.ContentLength = int64(len(forwardBody))

	// Emit a PII-safe span event for EVERY decision (block + redact + auditOnly). Never
	// carries the raw matched substring — only the detector, action, scan point, content
	// hash, and offsets (ADR 0059 §6).
	for _, dec := range res.decisions {
		emitGuardrailDecision(span, dec)
	}

	if res.block {
		// The blocked decision is the refusal cause — NOT decisions[0], which may be an
		// earlier redact/auditOnly hit on the same scan point (block > redact > auditOnly).
		cause := blockCause(res.decisions)
		gp.writeGuardrailBlocked(w, span, cause)
		reason := res.failClosedReason
		if reason == "" {
			reason = "policy match"
		}
		gp.logf("launcher: gateway: guardrail BLOCK detector=%s scan_point=%s (%s; call refused)",
			cause.detector, cause.scanPoint, reason)
		// m66.9: fire the durable compliance record AFTER the refusal is written — the block is
		// never delayed by the audit write (fire-and-forget goroutine, independent timeout).
		gp.fireGuardrailBlockAudit(r, cause)
		return true
	}

	if res.redacted {
		gp.logf("launcher: gateway: guardrail REDACT applied to request (scrubbed body forwarded)")
	}

	// ── Cascaded LLM-judge (M66 m66.8, ADR 0059 §8 Fork-5) ──────────────────────
	// The judge is the RESIDUAL step: it runs only now that the deterministic detectors did NOT block
	// this request (a deterministic block short-circuited above and never reached here). It classifies
	// the SAME user/tool content over the body being forwarded, issues its call to the REAL upstream
	// (loop-safe), and fails OPEN on its own error — so a flaky judge never blocks. A FLAGGED+block
	// verdict refuses the call with the typed guardrail_blocked (detector "semantic-judge") BEFORE
	// forwarding; auditOnly/SAFE/judge-error proceed. nil judge ⇒ this is a no-op.
	if pol.judge != nil {
		if dec, blocked := gp.judgeRequest(r.Context(), span, r, forwardBody, pol.judge); blocked {
			gp.writeGuardrailBlocked(w, span, dec)
			gp.logf("launcher: gateway: guardrail BLOCK detector=%s scan_point=%s (semantic-judge FLAGGED; call refused)",
				dec.detector, dec.scanPoint)
			// m66.9: durable compliance record for a semantic-judge block (same contract).
			gp.fireGuardrailBlockAudit(r, dec)
			return true
		}
	}
	return false
}

// applyOutputGuardrail scans the buffered RESPONSE completion (choices[].message.content)
// against the output-applicable rules and returns the body to relay to the client plus
// whether the call was blocked:
//
//   - block: the completion is REPLACED with a typed guardrail_blocked body (scan_point
//     "output") — the client never sees the flagged completion. Spend is STILL booked by
//     the caller (the model already generated; ADR 0059 §7). Returns blocked=true.
//   - redact: the matched completion content is scrubbed in place and the response is
//     re-serialised — the client receives the completion with [REDACTED:<name>].
//   - auditOnly / clean: the original response body is relayed byte-for-byte.
//
// An unparseable / non-JSON response is relayed unchanged (a completion is the model's own
// output on the way OUT — failing every non-JSON stream closed would be wrong; the request
// path, which inspects untrusted input BEFORE it runs, is the one that fails closed).
// Called ONLY when pol.engine != nil; the engine + judge come from the per-request bundle
// snapshot (pol) so a concurrent K3 reload cannot split the scan across two policies.
func (gp *gatewayProxy) applyOutputGuardrail(
	ctx context.Context, span trace.Span, r *http.Request, body []byte, pol *guardrailBundle,
) (out []byte, blocked bool) {
	// The deterministic output scan runs first (block > redact > auditOnly). Skip it only when there
	// are no output rules — but STILL fall through to the judge, which may apply to output even with no
	// deterministic output rules configured.
	relayBody := body
	if len(pol.engine.output) > 0 {
		res, scrubbed := pol.engine.scanOutput(body)
		relayBody = scrubbed
		for _, dec := range res.decisions {
			emitGuardrailDecision(span, dec)
		}
		if res.block {
			cause := blockCause(res.decisions)
			gp.markOutputBlockSpan(span, cause)
			gp.logf("launcher: gateway: guardrail BLOCK detector=%s scan_point=output (policy match; completion withheld)",
				cause.detector)
			// m66.9: durable compliance record — fire AFTER the decision is recorded; the body
			// substitution happens in serve() after this returns. Fire-and-forget, never delays.
			gp.fireGuardrailBlockAudit(r, cause)
			return guardrailBlockedBody(cause), true
		}
		if res.redacted {
			gp.logf("launcher: gateway: guardrail REDACT applied to completion (scrubbed response relayed)")
		}
	}

	// ── Cascaded LLM-judge (M66 m66.8, ADR 0059 §8 Fork-5) ──────────────────────
	// Residual step on the completion the deterministic output scan did NOT block. It classifies the
	// (possibly scrubbed) completion, calls the REAL upstream (loop-safe), and fails OPEN on its own
	// error. A FLAGGED+block verdict SUBSTITUTES the guardrail_blocked body for the completion — the
	// client never sees the flagged output — exactly as a deterministic output block does.
	if pol.judge != nil {
		if dec, blk := gp.judgeOutput(ctx, span, r, relayBody, pol.judge); blk {
			gp.markOutputBlockSpan(span, dec)
			gp.logf("launcher: gateway: guardrail BLOCK detector=%s scan_point=output "+
				"(semantic-judge FLAGGED; completion withheld)", dec.detector)
			// m66.9: durable compliance record for a semantic-judge output block.
			gp.fireGuardrailBlockAudit(r, dec)
			return guardrailBlockedBody(dec), true
		}
	}
	return relayBody, false
}

// markOutputBlockSpan stamps the output-block attributes + error status on the span. Shared by the
// deterministic output block and the judge output block so both surface identically.
func (gp *gatewayProxy) markOutputBlockSpan(span trace.Span, cause guardrailDecision) {
	span.SetAttributes(
		attribute.String("guardrail.decision", string(actionBlock)),
		attribute.String("guardrail.detector", cause.detector),
		attribute.String("guardrail.scan_point", string(cause.scanPoint)),
	)
	span.SetStatus(codes.Error, guardrailBlockedType)
}

// scanOutput scans a buffered chat/completions RESPONSE body's completion content
// (choices[].message.content) against the output rules and applies redaction in place.
// It returns a scanResult plus the (possibly re-serialised) body to relay. A non-JSON or
// unparseable response is relayed unchanged (block=false, redacted=false) — see
// applyOutputGuardrail. Precedence is block > redact > auditOnly, as on the request path.
func (e *guardrailEngine) scanOutput(body []byte) (scanResult, []byte) {
	if len(e.output) == 0 {
		return scanResult{}, body
	}
	cr, ok := parseChatResponse(body)
	if !ok {
		return scanResult{}, body // relay a completion we can't parse unchanged.
	}

	var res scanResult
	for i := range cr.choices {
		ch := &cr.choices[i]
		if !ch.hasStringContent {
			continue
		}
		mres, newContent, changed := e.scanAndRedact(ch.content, scanOutput, e.output)
		res.decisions = append(res.decisions, mres.decisions...)
		if mres.block {
			res.block = true
			return res, body // block wins: the caller substitutes the refusal body.
		}
		if changed {
			if err := cr.setChoiceContent(i, newContent); err != nil {
				// Never relay the raw completion if we cannot re-serialise the redaction: relay the
				// original bytes unredacted would leak PII, so drop to a block substitution instead.
				res.block = true
				res.decisions = append(res.decisions, guardrailDecision{
					blocked: true, detector: "redact-serialize", action: actionBlock, scanPoint: scanOutput,
				})
				return res, body
			}
			res.redacted = true
		}
	}

	if !res.redacted {
		return res, body
	}
	out, err := cr.marshal()
	if err != nil {
		res.block = true
		res.decisions = append(res.decisions, guardrailDecision{
			blocked: true, detector: "redact-serialize", action: actionBlock, scanPoint: scanOutput,
		})
		return res, body
	}
	return res, out
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

// requestHasStreamTrue reports whether the buffered request body is a JSON object with
// "stream": true. It is used by the guardrail request path to detect (and reject) streaming
// calls from guarded agents — output-blocking is incompatible with streaming (ADR 0059 §4).
// We use parseChatBody (already at hand) to extract the top-level "stream" field without a
// second JSON parse. Returns false for any unparseable/non-JSON body (the fail-closed scan
// that follows will block such a body anyway via parseChatBody).
func requestHasStreamTrue(body []byte) bool {
	// parseChatBody gives us the top-level map without a second full parse.
	cb, ok := parseChatBody(body)
	if !ok {
		return false
	}
	raw, present := cb.top["stream"]
	if !present {
		return false
	}
	// json.RawMessage "true" is exactly the 4-byte literal; anything else (false, null,
	// a number) is not a streaming request.
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	return v
}

// blockCause returns the blocked decision from a decision list — the refusal cause. It is
// NOT necessarily decisions[0]: on a scan point with an earlier redact/auditOnly hit and a
// later block hit, block wins (precedence) and its decision is the one surfaced in the 403
// and the span. Falls back to decisions[0] defensively (a block result always has one).
func blockCause(decisions []guardrailDecision) guardrailDecision {
	for _, d := range decisions {
		if d.blocked {
			return d
		}
	}
	return decisions[0]
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

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(guardrailBlockedStatus) // 403
	if _, err := w.Write(guardrailBlockedBody(dec)); err != nil {
		gp.logf("launcher: gateway: write guardrail_blocked: %v", err)
	}
}

// guardrailBlockedBody marshals the typed guardrail_blocked refusal body for a decision.
// Shared by the request-path write (writeGuardrailBlocked) and the OUTPUT-block
// substitution (applyOutputGuardrail), so both emit the identical OpenAI-shaped body with
// the stable "type" the SDK keys non-retryability on (m66.6). No raw matched content is
// ever included. json.Marshal of this fixed struct cannot fail, so the error is dropped.
func guardrailBlockedBody(dec guardrailDecision) []byte {
	b, _ := json.Marshal(guardrailErrorBody{Error: guardrailErrorDetail{
		Type:      guardrailBlockedType,
		Detector:  dec.detector,
		ScanPoint: string(dec.scanPoint),
	}})
	return b
}

// ── the response (completion) body model ───────────────────────────────────────
//
// chatResponse mirrors chatBody for the RESPONSE: the top-level object is preserved as a
// RawMessage map (usage, id, model, ... all round-trip), only "choices" is decoded, and
// each choice keeps every field (finish_reason, index, tool_calls on the message, ...)
// as a RawMessage map so redacting choices[i].message.content mutates ONLY that string.

type chatResponse struct {
	top     map[string]json.RawMessage
	choices []chatChoice
}

// chatChoice is one entry of the response choices array. raw holds the whole choice
// object; msgRaw holds the "message" object's fields; content / hasStringContent are the
// decoded completion text (absent / non-string ⇒ nothing to scan or mutate).
type chatChoice struct {
	raw              map[string]json.RawMessage
	msgRaw           map[string]json.RawMessage
	content          string
	hasStringContent bool
}

// parseChatResponse decodes a chat/completions response body, or ok=false when it is not a
// JSON object with a choices array (⇒ relay unchanged).
func parseChatResponse(body []byte) (*chatResponse, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false
	}
	rawChoices, present := top["choices"]
	if !present {
		return &chatResponse{top: top}, true // no choices: nothing to scan (clean).
	}
	var choiceObjs []map[string]json.RawMessage
	if err := json.Unmarshal(rawChoices, &choiceObjs); err != nil {
		return nil, false
	}
	cr := &chatResponse{top: top, choices: make([]chatChoice, 0, len(choiceObjs))}
	for _, co := range choiceObjs {
		ch := chatChoice{raw: co}
		if mr, ok := co["message"]; ok {
			var msgObj map[string]json.RawMessage
			if err := json.Unmarshal(mr, &msgObj); err == nil {
				ch.msgRaw = msgObj
				if cr2, ok := msgObj["content"]; ok {
					var s string
					if err := json.Unmarshal(cr2, &s); err == nil {
						ch.content = s
						ch.hasStringContent = true
					}
				}
			}
		}
		cr.choices = append(cr.choices, ch)
	}
	return cr, true
}

// setChoiceContent rewrites choice i's message.content string and re-marshals ONLY that
// field back through the message → choice raw maps, so tool_calls / finish_reason / every
// other field survives verbatim.
func (cr *chatResponse) setChoiceContent(i int, content string) error {
	enc, err := json.Marshal(content)
	if err != nil {
		return err
	}
	cr.choices[i].msgRaw["content"] = enc
	msgEnc, err := json.Marshal(cr.choices[i].msgRaw)
	if err != nil {
		return err
	}
	cr.choices[i].raw["message"] = msgEnc
	cr.choices[i].content = content
	return nil
}

// marshal re-serialises the (possibly mutated) response, rebuilding choices from each
// choice's raw map and preserving every other top-level field (usage, ...).
func (cr *chatResponse) marshal() ([]byte, error) {
	if cr.choices != nil {
		rawChoices := make([]map[string]json.RawMessage, len(cr.choices))
		for i := range cr.choices {
			rawChoices[i] = cr.choices[i].raw
		}
		enc, err := json.Marshal(rawChoices)
		if err != nil {
			return nil, err
		}
		cr.top["choices"] = enc
	}
	return json.Marshal(cr.top)
}
