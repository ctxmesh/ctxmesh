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

package guardrail

import (
	"fmt"
	"strings"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/telemetry"
)

// The effective streaming mode a guarded agent runs under (M139/K10, ADR 0086). These strings are the
// contract surfaced on GuardrailPolicy.status.streaming.effectiveMode and rendered by the console.
const (
	// EffectiveStreaming — the gateway serves span-suppression streaming (opted in AND stream-safe AND no
	// blocking semanticJudge). Tokens are released once provably clean behind a hold-window.
	EffectiveStreaming = "Streaming"
	// EffectiveBuffered — the M66 default: the whole completion is scanned before any byte is delivered.
	EffectiveBuffered = "Buffered"

	// streamModeEnabled is the StreamingGuardrail.mode value that opts in (any other value ⇒ buffered).
	streamModeEnabled = "Enabled"
)

// StreamingInput is the neutral input to the streaming decision (M139/K10). Both the LAUNCHER (which
// enforces) and the GuardrailPolicyReconciler (which reports) assemble it — the launcher from its live
// engine, the controller from the policy spec — so the SAME decision runs on both sides (no drift).
type StreamingInput struct {
	// OptedIn is spec.streaming.mode == "Enabled".
	OptedIn bool
	// JudgePresent is true when an ACTIVE semanticJudge exists (enabled AND routable) — it needs the whole
	// completion, so it forces buffered (mirrors the launcher's newSemanticJudge non-nil condition).
	JudgePresent bool
	// OutputRules are the policy's OUTPUT detectors (the set actually scanned on the completion).
	OutputRules []OutputRule
}

// StreamingResult is the decision — the effective mode, the hold-window (runes, valid when Streaming), and
// a human-facing reason (esp. WHY a streaming opt-in was downgraded to buffered).
type StreamingResult struct {
	EffectiveMode string
	Window        int
	Reason        string
}

// DecideStreaming is the single, shared streaming-eligibility decision (ADR 0086). Streaming requires ALL
// of: opted in, no active semanticJudge, and a stream-safe output rule set; any miss ⇒ buffered with a
// reason. This is the ONE place the "is it streaming" rule lives, so the launcher's enforcement and the
// controller's reported status can never disagree.
func DecideStreaming(in StreamingInput) StreamingResult {
	if !in.OptedIn {
		return StreamingResult{EffectiveMode: EffectiveBuffered, Reason: "streaming not enabled (spec.streaming.mode is not Enabled) — buffered-only (the default)"}
	}
	if in.JudgePresent {
		return StreamingResult{EffectiveMode: EffectiveBuffered, Reason: "a semanticJudge requires the whole completion — buffered-only"}
	}
	v := AnalyzeOutputStreamability(in.OutputRules)
	if !v.OK {
		return StreamingResult{EffectiveMode: EffectiveBuffered, Reason: "policy is not stream-safe: " + v.Reason}
	}
	return StreamingResult{
		EffectiveMode: EffectiveStreaming,
		Window:        v.Window,
		Reason:        fmt.Sprintf("stream-safe: span-suppression with a %d-rune hold-window", v.Window),
	}
}

// OutputDetectorRules builds a policy's OUTPUT detector rules from its spec (M139/K10) — the PII detectors
// and patternDenylist rules whose appliesTo resolves to output or all — using the SAME fail-closed
// telemetry builder the launcher's engine uses (telemetry.DetectorsWithCustomStrict), so the controller's
// analysis reflects the exact patterns the launcher will scan. Returns an error only if a pattern does not
// compile (which the validation gate already rejects — defence in depth). A nil spec ⇒ no output rules.
func OutputDetectorRules(spec *agentsv1beta1.GuardrailPolicySpec) ([]OutputRule, error) {
	if spec == nil {
		return nil, nil
	}
	var rules []OutputRule

	// PII detectors: built-ins (unless disabled) + custom, applied at output when appliesTo is output/all.
	if spec.PIIDetectors != nil && appliesToOutput(spec.PIIDetectors.AppliesTo) {
		custom := make([]telemetry.CustomDetectorSpec, 0, len(spec.PIIDetectors.Custom))
		for _, c := range spec.PIIDetectors.Custom {
			custom = append(custom, telemetry.CustomDetectorSpec{Name: c.Name, Pattern: c.Pattern})
		}
		ds, err := telemetry.DetectorsWithCustomStrict(custom)
		if err != nil {
			return nil, fmt.Errorf("piiDetectors: %w", err)
		}
		// builtIns=false ⇒ drop the built-in defaults the strict builder prepends (mirrors newGuardrailEngine).
		if spec.PIIDetectors.BuiltIns != nil && !*spec.PIIDetectors.BuiltIns {
			ds = ds[len(telemetry.DefaultDetectors()):]
		}
		for i := range ds {
			rules = append(rules, OutputRule{Name: ds[i].Name, Pattern: ds[i].PatternSource})
		}
	}

	// patternDenylist: each rule is its own detector, applied at output when appliesTo is output/all.
	for i := range spec.PatternDenylist {
		p := spec.PatternDenylist[i]
		if !appliesToOutput(p.AppliesTo) {
			continue
		}
		ds, err := telemetry.DetectorsWithCustomStrict([]telemetry.CustomDetectorSpec{{Name: p.Name, Pattern: p.Pattern}})
		if err != nil {
			return nil, fmt.Errorf("patternDenylist %q: %w", p.Name, err)
		}
		// The strict builder prepends built-in defaults; the denylist rule is the LAST detector.
		last := ds[len(ds)-1]
		rules = append(rules, OutputRule{Name: p.Name, Pattern: last.PatternSource})
	}
	return rules, nil
}

// SemanticJudgeActive mirrors the launcher's newSemanticJudge non-nil condition (an enabled + routable
// judge). An enabled-but-unroutable judge is treated as ABSENT (fail-open) — matching enforcement.
func SemanticJudgeActive(j *agentsv1beta1.SemanticJudge) bool {
	return j != nil && j.Enabled && strings.TrimSpace(j.ModelRoute) != ""
}

// StreamingOptedIn mirrors the launcher's parseStreamingMode + streamModeEnabled check.
func StreamingOptedIn(s *agentsv1beta1.StreamingGuardrail) bool {
	return s != nil && strings.EqualFold(s.Mode, streamModeEnabled)
}

// appliesToOutput reports whether an appliesTo value covers the OUTPUT scan point. Mirrors the launcher's
// normalizeScanPoint(appliesTo, "all"): an unset/unknown value defaults to "all" (which covers output).
func appliesToOutput(appliesTo string) bool {
	switch appliesTo {
	case "input", "toolOutput":
		return false
	default: // "output", "all", "" (default all), or any unknown → default all
		return true
	}
}
