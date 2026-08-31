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

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustMoney(t *testing.T, s string) Money {
	t.Helper()
	m, err := ParseMoney(s)
	require.NoError(t, err)
	return m
}

func ptr(m Money) *Money { return &m }

// ── Money: exact decimal, no float drift ────────────────────────────────────

func TestMoney_ParseValid(t *testing.T) {
	for _, s := range []string{"0", "0.50", "10", "10.00", "1.234567", "999999.999999"} {
		m, err := ParseMoney(s)
		require.NoError(t, err, "should parse %q", s)
		_ = m
	}
}

func TestMoney_ParseInvalid(t *testing.T) {
	for _, s := range []string{"", ".", "-1", "1.2.3", "1e5", "abc", "1/2", " 1", "$1"} {
		_, err := ParseMoney(s)
		assert.Error(t, err, "should reject %q", s)
	}
}

// TestMoney_NoFloatDrift is the load-bearing property: summing a value that has
// no exact float64 representation many times must stay EXACT. 0.1 accumulated 10
// times is exactly 1.0 here (float64 would give 0.9999999999999999).
func TestMoney_NoFloatDrift(t *testing.T) {
	sum := Zero()
	ten := mustMoney(t, "0.1")
	for range 10 {
		sum = sum.Add(ten)
	}
	assert.Equal(t, "1.000000", sum.String(), "10 × 0.1 must be exactly 1.000000, not a drifted float")

	// A finer case: 0.000001 (one micro-dollar) summed a million times = $1.00.
	micro := mustMoney(t, "0.000001")
	sum = Zero()
	for range 1_000_000 {
		sum = sum.Add(micro)
	}
	assert.Equal(t, "1.000000", sum.String())
}

func TestMoney_CmpAndThresholds(t *testing.T) {
	half := mustMoney(t, "0.50")
	cap := mustMoney(t, "1.00")
	assert.True(t, cap.GreaterThan(half))
	assert.False(t, half.GreaterThan(cap))
	assert.True(t, cap.AtLeast(cap))
	assert.True(t, half.Add(half).AtLeast(cap), "0.50+0.50 >= 1.00")

	// Soft threshold: 80% of $1.00 is exactly $0.80.
	assert.Equal(t, "0.800000", cap.MulPercent(80).String())
}

// ── Accountant: spend accumulates per conversation and per agent ────────────

func TestAccountant_AccumulatesPerDimension(t *testing.T) {
	a := NewAccountant()
	c := mustMoney(t, "0.10")

	a.Add("conv-1", "agent-x", c)
	a.Add("conv-1", "agent-x", c)
	a.Add("conv-2", "agent-x", c) // different conversation, same agent

	assert.Equal(t, "0.200000", a.ConvSpent("conv-1").String(), "conv-1 got two calls")
	assert.Equal(t, "0.100000", a.ConvSpent("conv-2").String(), "conv-2 got one call")
	assert.Equal(t, "0.300000", a.AgentSpent("agent-x").String(), "agent saw all three")
	assert.True(t, a.ConvSpent("unknown").IsZero(), "unseen key reads as $0")
}

func TestAccountant_EmptyKeysSkipped(t *testing.T) {
	a := NewAccountant()
	// No conversation id ⇒ only the agent dimension accrues.
	a.Add("", "agent-y", mustMoney(t, "0.05"))
	assert.True(t, a.ConvSpent("").IsZero())
	assert.Equal(t, "0.050000", a.AgentSpent("agent-y").String())
}

func TestAccountant_TTLEviction(t *testing.T) {
	a := NewAccountant()
	now := time.Unix(0, 0)
	a.now = func() time.Time { return now }

	a.Add("conv-ttl", "agent-ttl", mustMoney(t, "0.10"))
	assert.Equal(t, "0.100000", a.ConvSpent("conv-ttl").String())

	// Advance beyond the TTL: the entry ages out and reads as $0.
	now = now.Add(spendTTL + time.Minute)
	assert.True(t, a.ConvSpent("conv-ttl").IsZero(), "spend older than TTL is evicted")
	assert.True(t, a.AgentSpent("agent-ttl").IsZero())
}

func TestAccountant_SoftFiredOneShot(t *testing.T) {
	a := NewAccountant()
	assert.True(t, a.MarkConvSoftFired("c"), "first mark fires")
	assert.False(t, a.MarkConvSoftFired("c"), "second mark does not")
	assert.True(t, a.MarkAgentSoftFired("ag"), "agent dimension latches independently")
	assert.False(t, a.MarkAgentSoftFired("ag"))
	assert.False(t, a.MarkConvSoftFired(""), "empty key never fires")
}

// ── Enforcer: pre-call hard check, post-call state, one-shot soft alert ──────

// convCaps builds the standard conversation-only Caps used across these tests:
// conversation "c1", a $1.00 hard cap, an 80% soft threshold ($0.80).
func convCaps() Caps {
	m, _ := ParseMoney("1.00")
	return Caps{ConversationID: "c1", ConvCap: &m, SoftPct: 80}
}

func TestEnforcer_UnderBudgetAllows(t *testing.T) {
	e := NewEnforcer()
	caps := convCaps()
	dec := e.PreCall(context.Background(), caps, mustMoney(t, "0.10"))
	assert.True(t, dec.Allowed, "well under cap → allowed")
}

func TestEnforcer_HardBreachRefusesBeforeCall(t *testing.T) {
	e := NewEnforcer()
	caps := convCaps()

	// Book $0.95 of actual spend.
	_, _, _, _ = e.PostCall(context.Background(), caps, mustMoney(t, "0.95"))

	// Next call estimated at $0.10 ⇒ 0.95 + 0.10 = 1.05 > 1.00 ⇒ refuse.
	dec := e.PreCall(context.Background(), caps, mustMoney(t, "0.10"))
	require.False(t, dec.Allowed, "spent+estimate over cap → refuse")
	assert.Equal(t, DimensionConversation, dec.Dimension)
	assert.Equal(t, "0.950000", dec.Spent.String(), "reports real already-spent, not incl. estimate")
	assert.Equal(t, "1.000000", dec.Cap.String())
}

func TestEnforcer_SoftBreachOneShotAlert(t *testing.T) {
	e := NewEnforcer()
	caps := convCaps() // soft = $0.80

	// First call $0.50 → still ok, no alert.
	_, _, st, alert := e.PostCall(context.Background(), caps, mustMoney(t, "0.50"))
	assert.Equal(t, StateOK, st)
	assert.Nil(t, alert)

	// Second call $0.40 → total $0.90 ≥ soft $0.80 → soft state, alert fires ONCE.
	_, _, st, alert = e.PostCall(context.Background(), caps, mustMoney(t, "0.40"))
	assert.Equal(t, StateSoft, st)
	require.NotNil(t, alert, "crossing soft fires an alert")
	assert.Equal(t, DimensionConversation, alert.Dimension)
	assert.Equal(t, "0.900000", alert.Spent.String())
	assert.Equal(t, "0.800000", alert.SoftUSD.String())

	// Third call keeps it in soft but must NOT re-alert (one-shot).
	_, _, st, alert = e.PostCall(context.Background(), caps, mustMoney(t, "0.05"))
	assert.Equal(t, StateSoft, st)
	assert.Nil(t, alert, "soft alert is one-shot")
}

func TestEnforcer_PostCallExceededState(t *testing.T) {
	e := NewEnforcer()
	caps := convCaps()
	// A call that lands exactly on the cap → exceeded state (the NEXT PreCall
	// refuses), and no soft alert (exceeded supersedes soft).
	_, _, st, alert := e.PostCall(context.Background(), caps, mustMoney(t, "1.00"))
	assert.Equal(t, StateExceeded, st)
	assert.Nil(t, alert)

	dec := e.PreCall(context.Background(), caps, mustMoney(t, "0.000001"))
	assert.False(t, dec.Allowed, "already at cap → next call refused")
}

func TestEnforcer_AgentDimensionTrips(t *testing.T) {
	e := NewEnforcer()
	agentCap := mustMoney(t, "0.20")
	caps := Caps{AgentName: "ag", AgentCap: &agentCap, SoftPct: 80}

	_, _, _, _ = e.PostCall(context.Background(), caps, mustMoney(t, "0.15"))
	dec := e.PreCall(context.Background(), caps, mustMoney(t, "0.10")) // 0.15+0.10 > 0.20
	require.False(t, dec.Allowed)
	assert.Equal(t, DimensionAgent, dec.Dimension)
}

func TestEnforcer_BothCapsConversationTripsFirst(t *testing.T) {
	e := NewEnforcer()
	convCap := mustMoney(t, "0.50")
	agentCap := mustMoney(t, "0.50")
	caps := Caps{
		ConversationID: "c1", AgentName: "ag",
		ConvCap: &convCap, AgentCap: &agentCap, SoftPct: 80,
	}
	_, _, _, _ = e.PostCall(context.Background(), caps, mustMoney(t, "0.45"))
	dec := e.PreCall(context.Background(), caps, mustMoney(t, "0.10"))
	require.False(t, dec.Allowed)
	// Both dimensions trip; the response deterministically names conversation.
	assert.Equal(t, DimensionConversation, dec.Dimension)
}

func TestEnforcer_NotEnforcedWhenNoCapOrNoID(t *testing.T) {
	// Cap set but no conversation id ⇒ per-conversation dimension not enforceable.
	convCap := mustMoney(t, "1.00")
	assert.False(t, Caps{ConvCap: &convCap, SoftPct: 80}.Enforced(),
		"a conversation cap without a conversation id is not enforceable")
	// Id set but no cap ⇒ not enforceable.
	assert.False(t, Caps{ConversationID: "c1", SoftPct: 80}.Enforced())
	// Both ⇒ enforceable.
	assert.True(t, Caps{ConversationID: "c1", ConvCap: &convCap, SoftPct: 80}.Enforced())
}

// ── Concurrency: no data race under -race ───────────────────────────────────

// TestEnforcer_ConcurrentPostCallNoRace hammers PostCall from many goroutines on
// the same conversation + agent. Run under `go test -race`: the assertion is
// no data race AND the exact final total (money is exact, so N×cost is exact
// regardless of interleaving — the maps are mutex-guarded).
func TestEnforcer_ConcurrentPostCallNoRace(t *testing.T) {
	e := NewEnforcer()
	// High caps so nothing trips — we are testing the accounting path, not refusal.
	big := mustMoney(t, "1000000")
	caps := Caps{
		ConversationID: "c-hot", AgentName: "ag-hot",
		ConvCap: ptr(big), AgentCap: ptr(big), SoftPct: 80,
	}
	const goroutines, perG = 32, 100
	each := mustMoney(t, "0.000001")

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perG {
				// Interleave PreCall + PostCall as the real proxy does.
				_ = e.PreCall(context.Background(), caps, each)
				_, _, _, _ = e.PostCall(context.Background(), caps, each)
			}
		}()
	}
	wg.Wait()

	// goroutines*perG calls × $0.000001 each — exact, no drift.
	wantTotal := Zero()
	for range goroutines * perG {
		wantTotal = wantTotal.Add(each)
	}
	assert.Equal(t, wantTotal.String(), e.Accountant().ConvSpent("c-hot").String(),
		"concurrent spend accounting is exact (mutex-guarded, exact money)")
	assert.Equal(t, wantTotal.String(), e.Accountant().AgentSpent("ag-hot").String())
}

// ── Pricer: reuse LiteLLM cost, else deterministic token table ──────────────

func TestPriceCall_PrefersLiteLLMHeader(t *testing.T) {
	// The header wins even when a usage block is present.
	body := []byte(`{"usage":{"total_tokens":1000}}`)
	got := PriceCall("0.004200", body)
	assert.Equal(t, "0.004200", got.String(), "LiteLLM-reported cost is reused verbatim")
}

func TestPriceCall_FallsBackToUsageTable(t *testing.T) {
	// No header → deterministic $0.000001/token over total_tokens.
	body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":15,"total_tokens":25}}`)
	got := PriceCall("", body)
	assert.Equal(t, "0.000025", got.String(), "25 tokens × $0.000001 = $0.000025")
}

func TestPriceCall_UsageTotalFromParts(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":15}}`)
	got := PriceCall("", body)
	assert.Equal(t, "0.000025", got.String(), "total falls back to prompt+completion")
}

func TestPriceCall_NoUsageIsZero(t *testing.T) {
	assert.True(t, PriceCall("", []byte(`{"choices":[]}`)).IsZero())
	assert.True(t, PriceCall("bad", []byte("not json")).IsZero(),
		"unparseable cost header + unparseable body → $0")
}

// ── Estimator: last observed cost, floored ──────────────────────────────────

func TestEstimator_FloorThenLastObserved(t *testing.T) {
	e := NewEstimator()
	// Before any observation: the floor.
	first := e.Estimate("route-a")
	assert.Equal(t, "0.000001", first.String())

	e.Observe("route-a", mustMoney(t, "0.05"))
	assert.Equal(t, "0.050000", e.Estimate("route-a").String(), "estimate tracks last observed")

	// A tiny observed cost below the floor is floored up.
	e.Observe("route-a", mustMoney(t, "0.0000005"))
	assert.Equal(t, "0.000001", e.Estimate("route-a").String(), "estimate never drops below the floor")
}

// fakeSpendBackend is a SpendBackend test double: it serves configured spend + optional read errors,
// and records the conversation deltas booked.
type fakeSpendBackend struct {
	conv, agent       Money
	convErr, agentErr error
	added             []Money
}

func (f *fakeSpendBackend) AgentSpent(context.Context) (Money, error) { return f.agent, f.agentErr }
func (f *fakeSpendBackend) ConvSpent(context.Context, string) (Money, error) {
	return f.conv, f.convErr
}

func (f *fakeSpendBackend) AddConvSpend(_ context.Context, _ string, d Money) error {
	f.added = append(f.added, d)
	f.conv = f.conv.Add(d)
	return nil
}

// TestEnforcer_BackendEnforcesAndFailsClosed is the F2 fix (M126/ADR 0099): a proxy-backed Enforcer
// enforces the per-conversation cap against the DURABLE cross-replica total (not per-replica memory),
// FAILS CLOSED on a backend read error, and books the conversation spend durably.
func TestEnforcer_BackendEnforcesAndFailsClosed(t *testing.T) {
	cap := mustMoney(t, "1.00")
	caps := Caps{ConvCap: &cap, ConversationID: "c1", SoftPct: 80}

	// Durable total $0.90; a $0.05 estimate stays under $1.00 → allowed.
	be := &fakeSpendBackend{conv: mustMoney(t, "0.90")}
	e := NewEnforcerWithBackend(be, nil)
	require.True(t, e.PreCall(context.Background(), caps, mustMoney(t, "0.05")).Allowed)

	// $0.90 + $0.15 > $1.00 → refused (the cross-replica total enforces, unlike a fresh per-replica $0).
	dec := e.PreCall(context.Background(), caps, mustMoney(t, "0.15"))
	require.False(t, dec.Allowed)
	require.Equal(t, DimensionConversation, dec.Dimension)

	// A backend READ ERROR must FAIL CLOSED (refuse), never allow on a stale/zero local total.
	beErr := &fakeSpendBackend{convErr: errors.New("proxy unreachable")}
	require.False(t, NewEnforcerWithBackend(beErr, nil).
		PreCall(context.Background(), caps, mustMoney(t, "0.01")).Allowed, "fail closed on a backend read error")

	// PostCall books the CONVERSATION spend durably (the per-agent key is booked by the Q8 accountant).
	e.PostCall(context.Background(), caps, mustMoney(t, "0.10"))
	require.Len(t, be.added, 1, "conv spend booked to the backend exactly once")
}
