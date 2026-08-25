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

package budget

import "context"

// Dimension names the budget axis a decision refers to. It is the "dimension"
// field of the budget_exceeded response and part of the trace attributes.
type Dimension string

const (
	// DimensionConversation is the per-conversation-id cost cap.
	DimensionConversation Dimension = "conversation"
	// DimensionAgent is the per-agent (all conversations) cost cap.
	DimensionAgent Dimension = "agent"
)

// State is the budget state a call is in, reported on the trace span
// (specs/cost-governance.md "Trace": budget.state = ok|soft|exceeded).
type State string

const (
	// StateOK: spend is below the soft threshold on every set dimension.
	StateOK State = "ok"
	// StateSoft: spend has crossed the soft threshold on some dimension but is
	// still under the hard cap — the call proceeds, an alert fires once.
	StateSoft State = "soft"
	// StateExceeded: the pre-call hard check tripped — the call is refused.
	StateExceeded State = "exceeded"
)

// Caps is the per-request budget read from the launcher-stamped headers. An
// unset (nil) cap means that dimension is unenforced; SoftPct applies to
// whichever caps are set. Identity carries the keys the spend accrues under.
type Caps struct {
	// ConversationID keys the per-conversation spend. Empty ⇒ no per-conversation
	// enforcement (the agent did not stamp X-Conversation-Id).
	ConversationID string
	// AgentName keys the per-agent spend. Empty ⇒ no per-agent enforcement.
	AgentName string

	// ConvCap / AgentCap are the hard USD caps. nil ⇒ that dimension is unenforced.
	ConvCap  *Money
	AgentCap *Money

	// SoftPct is the soft-alert threshold percentage (1..99). The gateway alerts
	// once when a set dimension's spend first crosses SoftPct% of its cap.
	SoftPct int
}

// Enforced reports whether ANY dimension is actually enforceable for this
// request: a cap is set AND its identity key is present. When false the proxy
// forwards with zero budget overhead (the no-budget passthrough path).
func (c Caps) Enforced() bool {
	return (c.ConvCap != nil && c.ConversationID != "") ||
		(c.AgentCap != nil && c.AgentName != "")
}

// PreCallDecision is the verdict of the pre-call hard check.
type PreCallDecision struct {
	// Allowed is false when a hard cap would be breached — the caller must NOT
	// forward to the provider and must return budget_exceeded.
	Allowed bool
	// Dimension / Spent / Cap describe the tripped dimension (valid only when
	// Allowed is false). Spent is the ALREADY-accumulated spend (not including the
	// estimate) so the response reports real dollars spent so far.
	Dimension Dimension
	Spent     Money
	Cap       Money
}

// Enforcer wraps an Accountant with the decision logic. It is safe for
// concurrent use (the Accountant is). One Enforcer is shared across all requests
// in a launcher process.
type Enforcer struct {
	acct    *Accountant
	backend SpendBackend
	logf    func(string, ...any)
}

// NewEnforcer builds an in-memory Enforcer (dev / no shared seam).
func NewEnforcer() *Enforcer {
	return &Enforcer{acct: NewAccountant(), logf: func(string, ...any) {}}
}

// NewEnforcerWithBackend builds an Enforcer that ENFORCES against a durable, cross-replica spend store
// (F2, ADR 0099) — so a per-agent/per-conversation cap is real under scale-out + survives restarts,
// instead of the per-replica in-memory total that re-armed on every roll.
func NewEnforcerWithBackend(b SpendBackend, logf func(string, ...any)) *Enforcer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Enforcer{acct: NewAccountant(), backend: b, logf: logf}
}

// SpendBackend is the durable cross-replica spend store the Enforcer reads/writes when configured (the
// statelayer-proxy in prod). The per-AGENT identity is proxy-derived from the pod token (AgentSpent needs
// no name); the conversation id is launcher-supplied. A read error makes PreCall FAIL CLOSED. The per-agent
// key WRITE is the Q8 agent-spend accountant's job (booking it here too would double-count) — this books
// only the conversation dimension.
type SpendBackend interface {
	AgentSpent(ctx context.Context) (Money, error)
	ConvSpent(ctx context.Context, convID string) (Money, error)
	AddConvSpend(ctx context.Context, convID string, delta Money) error
}

// Accountant exposes the underlying accountant (for tests / introspection).
func (e *Enforcer) Accountant() *Accountant { return e.acct }

// PreCall runs the HARD check BEFORE the provider call. For each SET dimension
// it tests whether alreadySpent + estimate would exceed the hard cap; if so it
// refuses (Allowed=false) naming the tripped dimension and reporting the real
// already-spent total and the cap. Both caps set ⇒ the conversation dimension is
// tested first so the STRICTER-tripping ceiling is reported deterministically;
// the spec only requires that the response name which dimension tripped.
//
// estimate is a conservative cost for the pending call (a huge single call must
// not slip through), supplied by the caller's estimator. It is used ONLY for the
// hard comparison here — the ACTUAL cost is added by PostCall after the call.
func (e *Enforcer) PreCall(ctx context.Context, c Caps, estimate Money) PreCallDecision {
	conv, agent, ok := e.readSpent(ctx, c)
	if !ok {
		// F2: could not read the durable cross-replica budget — FAIL CLOSED (refuse) rather than allow
		// on a stale/zero local total. (The backend adapter logs the underlying error.)
		return PreCallDecision{Allowed: false, Dimension: DimensionConversation}
	}
	if c.ConvCap != nil && c.ConversationID != "" {
		if conv.Add(estimate).GreaterThan(*c.ConvCap) {
			return PreCallDecision{Allowed: false, Dimension: DimensionConversation, Spent: conv, Cap: *c.ConvCap}
		}
	}
	if c.AgentCap != nil && c.AgentName != "" {
		if agent.Add(estimate).GreaterThan(*c.AgentCap) {
			return PreCallDecision{Allowed: false, Dimension: DimensionAgent, Spent: agent, Cap: *c.AgentCap}
		}
	}
	return PreCallDecision{Allowed: true}
}

// readSpent returns the current spend for both enforced dimensions — from the durable SpendBackend
// (F2, cross-replica) when configured, else the in-memory accountant. ok=false ⇒ a backend READ failed
// ⇒ the caller FAILS CLOSED (never fall back to a stale/zero local total for enforcement).
func (e *Enforcer) readSpent(ctx context.Context, c Caps) (conv, agent Money, ok bool) {
	if e.backend == nil {
		return e.acct.ConvSpent(c.ConversationID), e.acct.AgentSpent(c.AgentName), true
	}
	conv, agent = Zero(), Zero()
	if c.ConvCap != nil && c.ConversationID != "" {
		v, err := e.backend.ConvSpent(ctx, c.ConversationID)
		if err != nil {
			return conv, agent, false
		}
		conv = v
	}
	if c.AgentCap != nil && c.AgentName != "" {
		v, err := e.backend.AgentSpent(ctx)
		if err != nil {
			return conv, agent, false
		}
		agent = v
	}
	return conv, agent, true
}

// SoftAlert names one dimension whose one-shot soft alert fired on THIS call, or
// ("", false) if none did. It is called by PostCall after the actual cost is
// booked; it latches at most one alert per call (the conversation dimension wins
// when both cross on the same call — a single alert is enough to signal the run).
type SoftAlert struct {
	Dimension Dimension
	Spent     Money
	Cap       Money
	SoftUSD   Money // the soft-threshold amount (cap * pct/100) that was crossed
}

// PostCall books the call's ACTUAL cost against both set dimensions and returns:
//   - the new per-dimension spend totals (for the trace span),
//   - the resulting budget State (ok|soft|exceeded — "exceeded" here means the
//     booked spend now meets/exceeds a hard cap, e.g. the last permitted call
//     landed exactly on the line; the NEXT PreCall will refuse),
//   - a soft alert to emit once, if this call first crossed a soft threshold.
//
// The state/alert are computed from the freshly-booked totals so they reflect
// reality after the call, and the soft latch guarantees the alert is one-shot.
func (e *Enforcer) PostCall(ctx context.Context, c Caps, actual Money) (convSpent, agentSpent Money, state State, alert *SoftAlert) {
	if e.backend != nil {
		// Book the CONVERSATION spend durably; the per-AGENT key is booked separately by the Q8 agent-spend
		// accountant (booking it here too would double-count). Best-effort — a write error leaves enforcement
		// to the next PreCall read (bounded overshoot, ADR 0099).
		if c.ConvCap != nil && c.ConversationID != "" {
			if err := e.backend.AddConvSpend(ctx, c.ConversationID, actual); err != nil {
				e.logf("launcher: budget: durable conv-spend write failed (enforcement falls to the next read): %v", err)
			}
		}
		// Re-read the durable totals for the state/alert + span annotation. Fail-OPEN here (the read is not
		// the enforcement point — PreCall is): on a read error use the local view.
		if conv, agent, ok := e.readSpent(ctx, c); ok {
			convSpent, agentSpent = conv, agent
		} else {
			convSpent, agentSpent = e.acct.ConvSpent(c.ConversationID), e.acct.AgentSpent(c.AgentName)
		}
	} else {
		convSpent, agentSpent = e.acct.Add(c.ConversationID, c.AgentName, actual)
	}

	state = StateOK

	// Evaluate the conversation dimension.
	if c.ConvCap != nil && c.ConversationID != "" {
		soft := c.ConvCap.MulPercent(c.SoftPct)
		switch {
		case convSpent.AtLeast(*c.ConvCap):
			state = StateExceeded
		case convSpent.AtLeast(soft):
			if state == StateOK {
				state = StateSoft
			}
			if alert == nil && e.acct.MarkConvSoftFired(c.ConversationID) {
				a := SoftAlert{Dimension: DimensionConversation, Spent: convSpent, Cap: *c.ConvCap, SoftUSD: soft}
				alert = &a
			}
		}
	}

	// Evaluate the agent dimension.
	if c.AgentCap != nil && c.AgentName != "" {
		soft := c.AgentCap.MulPercent(c.SoftPct)
		switch {
		case agentSpent.AtLeast(*c.AgentCap):
			state = StateExceeded
		case agentSpent.AtLeast(soft):
			if state == StateOK {
				state = StateSoft
			}
			if alert == nil && e.acct.MarkAgentSoftFired(c.AgentName) {
				a := SoftAlert{Dimension: DimensionAgent, Spent: agentSpent, Cap: *c.AgentCap, SoftUSD: soft}
				alert = &a
			}
		}
	}

	return convSpent, agentSpent, state, alert
}
