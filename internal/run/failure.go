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

package run

// FailureCode is the CLOSED platform vocabulary of terminal-failure classes (M138, ADR 0109 §2). It is
// derived from the failure CODE PATH — never parsed from the free-text Error message (routing on a
// model/tool-influenced string would hand the model the workflow topology). A workflow `catch` clause
// matches on these codes; they are the ONE-WAY-DOOR vocabulary (workflows hardcode them forever), so the
// set is fixed here. Bare-lowercase codes are PLATFORM-RESERVED; user-defined typed errors (a future
// feature) will be PREFIXED so they can never collide. Deferred (not v1): hierarchical codes, cause
// chains, per-type retry policy.
type FailureCode string

const (
	FailureTimeout         FailureCode = "timeout"          // a deadline / lease-expiry / poll timeout
	FailureCancelled       FailureCode = "cancelled"        // an operator cancel, an approval/plan denial, a cascade
	FailureBudgetExceeded  FailureCode = "budget_exceeded"  // a spend/quota/spawn-budget denial
	FailureGuardrailDenied FailureCode = "guardrail_denied" // a guardrail policy blocked the run
	FailureToolError       FailureCode = "tool_error"       // a tool/MCP call failed terminally
	FailureAgentError      FailureCode = "agent_error"      // the agent's own execution failed (the default for a failed run)
	FailurePlatform        FailureCode = "platform_error"   // an infrastructure / unclassified failure (the total-function fallback)
)

// allFailureCodes is the closed set — used by CRD validation (the catch clause's error list) and tests.
var allFailureCodes = []FailureCode{
	FailureTimeout, FailureCancelled, FailureBudgetExceeded, FailureGuardrailDenied,
	FailureToolError, FailureAgentError, FailurePlatform,
}

// AllFailureCodes returns the closed platform failure-code vocabulary (a copy).
func AllFailureCodes() []FailureCode {
	out := make([]FailureCode, len(allFailureCodes))
	copy(out, allFailureCodes)
	return out
}

// IsPlatformFailureCode reports whether s is one of the reserved platform codes (or the catch-all "*").
// Used by CRD validation to reject an unknown code in a `catch` clause (a typo would silently never match).
func IsPlatformFailureCode(s string) bool {
	if s == CatchAll {
		return true
	}
	for _, c := range allFailureCodes {
		if string(c) == s {
			return true
		}
	}
	return false
}

// CatchAll is the wildcard error matcher in a workflow `catch` clause (AWS Step Functions `States.ALL`).
const CatchAll = "*"

// Failure is the structured, type-safe failure projection of a terminal run (M138, ADR 0109 §1): the
// classified code, the display message, and the node/agent that failed. It is what a workflow `catch`
// handler binds as the `error` CEL variable — routing matches on Code only; Message is display/data.
type Failure struct {
	Code    FailureCode `json:"type"`
	Message string      `json:"message,omitempty"`
	Node    string      `json:"node,omitempty"`
}

// CELMap projects the failure into the map a workflow catch handler binds as the `error` CEL variable
// (M138, ADR 0109 §4): {type, message, node}. The message is truncated to maxMessageBytes (0 ⇒ no cap)
// so a pathological tool error can't bloat CEL evaluation or a downstream prompt. Routing matches only
// `type`; message/node are display data.
func (f Failure) CELMap(maxMessageBytes int) map[string]any {
	msg := f.Message
	if maxMessageBytes > 0 && len(msg) > maxMessageBytes {
		msg = msg[:maxMessageBytes]
	}
	return map[string]any{"type": string(f.Code), "message": msg, "node": f.Node}
}

// ClassifyFailure maps a terminal run to its failure code FROM THE PATH (ADR 0109 §1), total over all
// inputs. A code STAMPED at the failure site (the specific denial paths — budget/guardrail/tool/poison)
// wins; otherwise it is derived from the terminal STATUS (itself a structured path signal, never the
// message): expired ⇒ timeout, cancelled ⇒ cancelled, failed ⇒ agent_error (a failed run = the agent's
// execution failed). Any other terminal-nonsuccess ⇒ platform_error. A non-terminal/succeeded run has no
// failure (returns "").
func ClassifyFailure(status Status, stamped FailureCode) FailureCode {
	if stamped != "" {
		return stamped
	}
	switch status {
	case StatusExpired:
		return FailureTimeout
	case StatusCancelled:
		return FailureCancelled
	case StatusFailed:
		return FailureAgentError
	case StatusSucceeded, StatusQueued, StatusRunning, StatusRequiresAction, StatusWaiting:
		return "" // not a terminal failure
	default:
		return FailurePlatform
	}
}
