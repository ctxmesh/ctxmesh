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

package egress

// Tool-call governance (M82, ADR 0074 §1): the resolved spec.runtime.toolPolicy the controller
// delivers to this sidecar (a mounted, watched ConfigMap file). This file lands the DELIVERY +
// PARSE + HOLD plumbing only — the policy is stored behind an RWMutex and atomically swapped on a
// live reload, but it is NOT yet enforced. ServeHTTP does not consult it; behavior stays PERMISSIVE
// (a denied/require-approval tool still works). Enforcement (deny 403 / batch-reject / method
// allow-list / fail-closed-on-unparseable / the approval voucher / the fan-out ceiling) is a later
// M82 task (ADR 0074 §2-§5). An absent/empty policy ⇒ no policy (permissive, as before M82).

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// ToolPolicy is the parsed spec.runtime.toolPolicy the sidecar holds. It mirrors the CRD's
// ToolPolicySpec (api/v1alpha1) — the SAME shape the controller marshals into the mounted file and
// into AGENT_RUNTIME for the SDK — so the wire enforcement (a later M82 task) and the SDK's managed
// loop judge the same policy. Only the deny/require-approval-relevant fields are needed at the
// sidecar; the SDK-only steering fields (forcedChoice, parallelLimit) are parsed + retained for
// completeness but unused here.
type ToolPolicy struct {
	// Default is the rule applied to any tool without an explicit override
	// (allow | deny | require-approval). Empty ⇒ allow (the CRD default).
	Default string `json:"default,omitempty"`
	// Overrides is the per-tool-name policy list, applied in order (first match wins).
	Overrides []ToolPolicyOverride `json:"overrides,omitempty"`
	// ForcedChoice + ParallelLimit are SDK-loop steering knobs (not wire-enforceable at the
	// sidecar); retained on parse for completeness.
	ForcedChoice  string `json:"forcedChoice,omitempty"`
	ParallelLimit int32  `json:"parallelLimit,omitempty"`
}

// ToolPolicyOverride is one named tool-level rule (mirrors the CRD's ToolPolicyOverride).
type ToolPolicyOverride struct {
	Name      string `json:"name"`
	Rule      string `json:"rule"`
	Retryable bool   `json:"retryable,omitempty"`
}

// The normalized tool-policy rules (matches the CRD enum, lower-cased). RuleFor / normalizeRule
// always return one of these (or a verbatim unrecognized value, treated as non-allow → fail-closed).
const (
	RuleAllow           = "allow"
	RuleDeny            = "deny"
	RuleRequireApproval = "require-approval"
)

// RuleFor returns the effective rule for a tool name: the first matching override's rule, else the
// policy default (empty default ⇒ "allow"). This is the read the enforcement path consults on the
// hot path — the toolName argument is the WIRE `params.name` (the tool the model named), NOT the
// route's path segment (M82.2, ADR 0074 §5): a remote tool routes under its ServerName segment, so
// for a multi-tool OBO server (one route, many tools) each tools/call's params.name is matched here
// independently. Policy override names ARE the wire params.name (no aliasing today — see the CRD's
// ToolPolicyOverride, keyed on the bound tool's name).
func (p *ToolPolicy) RuleFor(toolName string) string {
	if p == nil {
		return RuleAllow
	}
	for i := range p.Overrides {
		if p.Overrides[i].Name == toolName {
			return normalizeRule(p.Overrides[i].Rule)
		}
	}
	return normalizeRule(p.Default)
}

// Restricts reports whether this policy carries ANY deny or require-approval rule — i.e. it is NOT a
// pure-allow policy. This is the gate for the §5 fail-closed body-parsing regime: on a restrictive
// route the sidecar rejects batch bodies, requires a method allow-list for tool-less requests, and
// fails closed on any tools/call whose params.name can't be extracted. A pure-allow policy (nil,
// default allow with no restrictive override) returns false → the route stays byte-for-byte
// permissive (no new rejection, no security need). A non-allow default, or any deny/require-approval
// override, makes the whole route restrictive (an unidentifiable call could be smuggling a denied
// tool, so it must fail closed even for tools the default would allow).
func (p *ToolPolicy) Restricts() bool {
	if p == nil {
		return false
	}
	if normalizeRule(p.Default) != RuleAllow {
		return true
	}
	for i := range p.Overrides {
		if normalizeRule(p.Overrides[i].Rule) != RuleAllow {
			return true
		}
	}
	return false
}

// normalizeRule maps an empty rule to the CRD default ("allow") and lower-cases for a stable
// comparison; an unrecognized value is returned verbatim (the CRD enum already bounds valid input,
// so this is defensive only).
func normalizeRule(rule string) string {
	r := strings.ToLower(strings.TrimSpace(rule))
	if r == "" {
		return "allow"
	}
	return r
}

// ParseToolPolicy parses the mounted tool-policy JSON into a ToolPolicy. Empty/whitespace ⇒ (nil,
// nil): no policy (permissive), the byte-compatible pre-M82 state. Malformed JSON ⇒ an error, which
// the reload path handles fail-closed / keep-last-good.
func ParseToolPolicy(raw string) (*ToolPolicy, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var p ToolPolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("egress: parse tool policy: %w", err)
	}
	return &p, nil
}

// PolicyHolder guards the current tool policy behind an RWMutex so the fsnotify watcher can
// atomically swap it while request goroutines read it (mirrors the launcher's guardrailHolder). The
// zero value is a valid empty holder (nil policy ⇒ permissive). It also remembers the raw JSON the
// current policy was built from so the watcher can skip a rebuild on a byte-identical (spurious)
// event.
type PolicyHolder struct {
	mu      sync.RWMutex
	current *ToolPolicy
	rawJSON string
}

// Store swaps in a new policy under the write lock (the reload path). A nil policy means "no active
// policy" (permissive). raw is the JSON it was built from (for the unchanged-content skip).
func (h *PolicyHolder) Store(p *ToolPolicy, raw string) {
	h.mu.Lock()
	h.current = p
	h.rawJSON = raw
	h.mu.Unlock()
}

// Load returns the current policy under the read lock. nil ⇒ no active policy (permissive). Safe on
// a nil receiver (an unwired holder) so a caller can always read.
func (h *PolicyHolder) Load() *ToolPolicy {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	p := h.current
	h.mu.RUnlock()
	return p
}

// RawEquals reports whether the held policy was built from byte-identical raw JSON (the
// skip-reparse fast path on a spurious fsnotify event).
func (h *PolicyHolder) RawEquals(raw string) bool {
	h.mu.RLock()
	eq := h.rawJSON == raw
	h.mu.RUnlock()
	return eq
}
