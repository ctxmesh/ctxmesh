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

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/runcap"
)

// ceilingPolicy is a pure-allow policy carrying ONLY a fan-out ceiling — no deny / require-approval
// rule. This is the key case the ceiling adds: Restricts()==false but NeedsInspection()==true, so the
// body must be classified to count tool calls even though every tool is allowed.
func ceilingPolicy(max int32) *ToolPolicy {
	return &ToolPolicy{Default: RuleAllow, MaxToolCallsPerRun: max}
}

// mintRun mints a valid runcap for a specific RunID (the harness's default mint hardcodes "run-1";
// the ceiling keys off the VERIFIED RunID, so per-run isolation needs distinct minted runs).
func (h *policyHarness) mintRun(t *testing.T, runID string) string {
	t.Helper()
	tok, err := h.signer.Mint(runcap.MintRequest{User: "u-alice", Agent: testAgent, RunID: runID, TTL: 5 * time.Minute})
	require.NoError(t, err)
	return tok
}

// sendAsRun drives one request carrying a runcap for the given RunID (so different runs can be
// exercised against the same sidecar to prove per-run isolation).
func (h *policyHarness) sendAsRun(t *testing.T, runID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+testServer, strings.NewReader(body))
	req.Header.Set(runcap.HeaderName, h.mintRun(t, runID))
	rec := httptest.NewRecorder()
	h.proxy.ServeHTTP(rec, req)
	return rec
}

// (1) A single run flooding an ALLOWED tool is forwarded up to the ceiling, then every subsequent call
// gets a terminal 403 tool_call_ceiling_exceeded (fail-closed past the limit).
func TestCeilingFloodBlockedPastLimit(t *testing.T) {
	const limit = 3
	h := newPolicyHarness(t, ceilingPolicy(limit))

	for i := 1; i <= limit; i++ {
		rec := h.sendAsRun(t, "run-A", toolCallBody("safe"))
		require.Equalf(t, http.StatusOK, rec.Code, "call %d/%d should be under the ceiling", i, limit)
	}
	assert.Equal(t, limit, h.up.hits, "exactly the ceiling's worth of calls reached the upstream")

	// The (limit+1)th and every further call is denied with a TERMINAL 403 — non-retryable.
	for range 3 {
		rec := h.sendAsRun(t, "run-A", toolCallBody("safe"))
		require.Equal(t, http.StatusForbidden, rec.Code, "past the ceiling every call is denied")
		assert.Contains(t, rec.Body.String(), "tool_call_ceiling_exceeded")
	}
	assert.Equal(t, limit, h.up.hits, "no further calls reached the upstream past the ceiling")
}

// (J9) The fan-out chokepoint records a per-(agent, tool, outcome) metric: forwarded calls under the ceiling
// count outcome=forwarded; a call past the ceiling counts outcome=ceiling_denied — the per-tool observability
// M82's per-run TOTAL cap lacked. A unique tool name keeps the counter deltas isolated from other tests.
func TestCeilingRecordsPerToolMetric(t *testing.T) {
	const limit = 2
	const tool = "j9_metric_tool"
	h := newPolicyHarness(t, ceilingPolicy(limit))
	fwd := func() float64 {
		return testutil.ToFloat64(toolCallsTotal.WithLabelValues(testAgent, tool, "forwarded"))
	}
	denied := func() float64 {
		return testutil.ToFloat64(toolCallsTotal.WithLabelValues(testAgent, tool, "ceiling_denied"))
	}
	fwd0, denied0 := fwd(), denied()

	for i := 1; i <= limit; i++ {
		rec := h.sendAsRun(t, "run-J9", toolCallBody(tool))
		require.Equalf(t, http.StatusOK, rec.Code, "call %d/%d is under the ceiling", i, limit)
	}
	rec := h.sendAsRun(t, "run-J9", toolCallBody(tool))
	require.Equal(t, http.StatusForbidden, rec.Code, "the past-ceiling call is denied")

	assert.Equal(t, float64(limit), fwd()-fwd0, "each under-ceiling forward increments outcome=forwarded")
	assert.Equal(t, float64(1), denied()-denied0, "the past-ceiling call increments outcome=ceiling_denied")
}

// (2) A DIFFERENT run (distinct verified runcap RunID) is unaffected by the first run hitting its
// ceiling — per-run isolation.
func TestCeilingPerRunIsolation(t *testing.T) {
	const limit = 2
	h := newPolicyHarness(t, ceilingPolicy(limit))

	// run-A exhausts its ceiling.
	require.Equal(t, http.StatusOK, h.sendAsRun(t, "run-A", toolCallBody("safe")).Code)
	require.Equal(t, http.StatusOK, h.sendAsRun(t, "run-A", toolCallBody("safe")).Code)
	require.Equal(t, http.StatusForbidden, h.sendAsRun(t, "run-A", toolCallBody("safe")).Code, "run-A is now capped")

	// run-B is a fresh run: its own ceiling is untouched.
	require.Equal(t, http.StatusOK, h.sendAsRun(t, "run-B", toolCallBody("safe")).Code, "run-B has its own tally")
	require.Equal(t, http.StatusOK, h.sendAsRun(t, "run-B", toolCallBody("safe")).Code)
	require.Equal(t, http.StatusForbidden, h.sendAsRun(t, "run-B", toolCallBody("safe")).Code, "run-B caps independently")

	assert.Equal(t, 4, h.up.hits, "each run forwarded exactly its own ceiling (2+2), the caps are per-run")
}

// (3) A DENIED tool call does NOT consume the ceiling — denied calls aren't dispatches. ceiling=2,
// one deny + three allows ⇒ the deny doesn't count, so the three allows still get 2 through then 403.
func TestCeilingDeniedCallDoesNotCount(t *testing.T) {
	const limit = 2
	// Restrictive AND ceilinged: "danger" denied, everything else allowed, ceiling=2.
	policy := &ToolPolicy{
		Default:            RuleAllow,
		MaxToolCallsPerRun: limit,
		Overrides:          []ToolPolicyOverride{{Name: "danger", Rule: RuleDeny}},
	}
	h := newPolicyHarness(t, policy)

	// A denied call: 403 tool_denied, and it must NOT have incremented the ceiling.
	rec := h.sendAsRun(t, "run-A", toolCallBody("danger"))
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "tool_denied")
	assert.Equal(t, 0, h.up.hits)

	// Now three allowed calls: the deny did not consume the ceiling, so 2 forward then the 3rd is 403.
	require.Equal(t, http.StatusOK, h.sendAsRun(t, "run-A", toolCallBody("safe")).Code, "1st allow forwards")
	require.Equal(t, http.StatusOK, h.sendAsRun(t, "run-A", toolCallBody("safe")).Code, "2nd allow forwards")
	rec = h.sendAsRun(t, "run-A", toolCallBody("safe"))
	require.Equal(t, http.StatusForbidden, rec.Code, "3rd allow exceeds the ceiling")
	assert.Contains(t, rec.Body.String(), "tool_call_ceiling_exceeded")
	assert.Equal(t, 2, h.up.hits, "only the two allowed calls under the ceiling reached the upstream")
}

// (3b) An UNAPPROVED require-approval call does NOT consume the ceiling either (it isn't a dispatch);
// a voucher-APPROVED call DOES count. ceiling=1: an approval_required attempt then an approved call
// still forwards (the failed attempt didn't burn the single slot), and a second approved call is 403.
func TestCeilingUnapprovedRequireApprovalDoesNotCount(t *testing.T) {
	const tool = "audit"
	policy := &ToolPolicy{
		Default:            RuleAllow,
		MaxToolCallsPerRun: 1,
		Overrides:          []ToolPolicyOverride{{Name: tool, Rule: RuleRequireApproval}},
	}
	h := newPolicyHarness(t, policy)

	// An unapproved require-approval call: typed 403 approval_required, must NOT count.
	rec := h.send(t, toolCallBody(tool)) // default mint uses run-1
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "approval_required")
	assert.Equal(t, 0, h.up.hits)

	// A voucher-approved call now forwards (the failed attempt didn't burn the single ceiling slot).
	voucher := h.mintVoucher(t, "run-1", tool, 5*time.Minute)
	rec = h.sendWithVoucher(t, toolCallBody(tool), voucher)
	require.Equal(t, http.StatusOK, rec.Code, "an approved dispatch forwards")
	assert.Equal(t, 1, h.up.hits)

	// A SECOND approved call exceeds the ceiling of 1 — an approved dispatch DOES count. (A distinct,
	// still-valid TTL here also proves the ceiling verdict is independent of the voucher's lifetime.)
	voucher2 := h.mintVoucher(t, "run-1", tool, 10*time.Minute)
	rec = h.sendWithVoucher(t, toolCallBody(tool), voucher2)
	require.Equal(t, http.StatusForbidden, rec.Code, "the 2nd approved dispatch exceeds ceiling=1")
	assert.Contains(t, rec.Body.String(), "tool_call_ceiling_exceeded")
	assert.Equal(t, 1, h.up.hits)
}

// (4) The ceiling is configurable via the policy field (1 vs 5 behave accordingly) AND hot-reloads:
// storing a higher-ceiling policy admits subsequent calls that the old ceiling denied.
func TestCeilingConfigurableAndHotReloads(t *testing.T) {
	// Ceiling of 1.
	h1 := newPolicyHarness(t, ceilingPolicy(1))
	require.Equal(t, http.StatusOK, h1.sendAsRun(t, "run-A", toolCallBody("safe")).Code)
	require.Equal(t, http.StatusForbidden, h1.sendAsRun(t, "run-A", toolCallBody("safe")).Code, "ceiling=1 caps after one call")
	assert.Equal(t, 1, h1.up.hits)

	// Ceiling of 5 on a fresh run.
	h5 := newPolicyHarness(t, ceilingPolicy(5))
	for i := range 5 {
		require.Equalf(t, http.StatusOK, h5.sendAsRun(t, "run-B", toolCallBody("safe")).Code, "call %d under ceiling=5", i+1)
	}
	require.Equal(t, http.StatusForbidden, h5.sendAsRun(t, "run-B", toolCallBody("safe")).Code, "6th call exceeds ceiling=5")
	assert.Equal(t, 5, h5.up.hits)

	// Hot reload: run-C hits the low ceiling, then a NEW policy with a higher ceiling is Stored and the
	// same run is admitted again (the counter is not reset, but the raised limit now exceeds its tally).
	h := newPolicyHarness(t, ceilingPolicy(2))
	require.Equal(t, http.StatusOK, h.sendAsRun(t, "run-C", toolCallBody("safe")).Code)
	require.Equal(t, http.StatusOK, h.sendAsRun(t, "run-C", toolCallBody("safe")).Code)
	require.Equal(t, http.StatusForbidden, h.sendAsRun(t, "run-C", toolCallBody("safe")).Code, "run-C capped at 2")
	assert.Equal(t, 2, h.up.hits)

	h.holder.Store(ceilingPolicy(10), "") // raise the ceiling live
	require.Equal(t, http.StatusOK, h.sendAsRun(t, "run-C", toolCallBody("safe")).Code,
		"after a hot reload to a higher ceiling the same run is admitted again")
	assert.Equal(t, 3, h.up.hits, "the reloaded, higher ceiling took effect on the hot path")
}

// (5) MaxToolCallsPerRun==0 on an otherwise pure-allow policy ⇒ NeedsInspection()==false ⇒ the body is
// NOT read and the forward is byte-for-byte unchanged (no ceiling, no counting). A batch and an
// unparseable body — which a restrictive/ceilinged route would reject — are forwarded verbatim.
func TestCeilingZeroIsPermissiveNoBodyRead(t *testing.T) {
	// Pure-allow, ceiling absent (0).
	assert.False(t, ceilingPolicy(0).NeedsInspection(), "ceiling=0 on a pure-allow policy needs no inspection")

	h := newPolicyHarness(t, ceilingPolicy(0))

	// A batch is forwarded (NOT rejected) — proof the body regime is not engaged.
	rec := h.sendAsRun(t, "run-A", fmt.Sprintf(`[%s,%s]`, toolCallBody("safe"), toolCallBody("safe")))
	require.Equal(t, http.StatusOK, rec.Code, "ceiling=0 pure-allow does not reject batches")

	// An unparseable body is forwarded (NOT failed closed).
	rec = h.sendAsRun(t, "run-A", `{not json`)
	require.Equal(t, http.StatusOK, rec.Code, "ceiling=0 pure-allow does not fail closed")

	// Many plain tool calls, all forwarded — no ceiling ever fires.
	for range 10 {
		require.Equal(t, http.StatusOK, h.sendAsRun(t, "run-A", toolCallBody("safe")).Code)
	}
	assert.Equal(t, 12, h.up.hits, "every call forwarded byte-for-byte — no ceiling on a 0-ceiling pure-allow policy")
}

// NeedsInspection unit coverage: it is a superset of Restricts() plus the ceiling.
func TestNeedsInspectionGate(t *testing.T) {
	assert.False(t, (*ToolPolicy)(nil).NeedsInspection(), "nil policy needs no inspection")
	assert.False(t, (&ToolPolicy{}).NeedsInspection(), "empty pure-allow policy needs no inspection")
	assert.False(t, (&ToolPolicy{Default: RuleAllow, MaxToolCallsPerRun: 0}).NeedsInspection(),
		"pure-allow with ceiling=0 needs no inspection")
	assert.True(t, (&ToolPolicy{Default: RuleAllow, MaxToolCallsPerRun: 1}).NeedsInspection(),
		"a ceiling>0 engages inspection even on a pure-allow policy")
	assert.True(t, (&ToolPolicy{Default: RuleDeny}).NeedsInspection(), "a restrictive policy always needs inspection")
	assert.True(t, (&ToolPolicy{Default: RuleDeny, MaxToolCallsPerRun: 5}).NeedsInspection(),
		"restrictive + ceiling needs inspection")
}

// (7) An empty runID under an ACTIVE ceiling fails closed (403) — a verified runcap with no RunID is a
// control-plane misconfiguration; an unattributable call can't be bounded, so it is denied. The runcap
// signer refuses to mint an empty RunID (so this state can only arise from a control-plane bug), so we
// exercise the defensive branch directly on admitFanOut rather than via a forged token.
func TestCeilingEmptyRunIDFailsClosed(t *testing.T) {
	h := newPolicyHarness(t, ceilingPolicy(5))
	rec := httptest.NewRecorder()
	ok := h.proxy.admitFanOut(rec, ceilingPolicy(5), testServer, "safe", "")
	require.False(t, ok, "an unattributable call under an active ceiling must not be forwarded")
	assert.Equal(t, http.StatusForbidden, rec.Code, "empty runID under an active ceiling fails closed")
	assert.Contains(t, rec.Body.String(), "tool_call_ceiling_exceeded")

	// Sanity: with NO active ceiling an empty runID is a no-op pass (no counting, permissive).
	rec2 := httptest.NewRecorder()
	assert.True(t, h.proxy.admitFanOut(rec2, ceilingPolicy(0), testServer, "safe", ""),
		"empty runID with no ceiling forwards (nothing to bound)")
}

// (6) Concurrent increments for the SAME run are race-clean and the count is exact (no lost updates).
// This drives runCallCounter.increment directly — the precise unit under test — so the assertion is
// deterministic (the httptest upstream's plain-int hit counter is itself not concurrency-safe, so we
// don't fan out real requests here). Run under -race.
func TestRunCallCounterConcurrentIncrementsRaceClean(t *testing.T) {
	c := newRunCallCounter()
	const goroutines = 50
	const perG = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perG {
				c.increment("run-shared")
			}
		}()
	}
	wg.Wait()
	// Exactly goroutines*perG increments landed — no lost update under contention.
	assert.Equal(t, goroutines*perG, c.increment("run-shared")-1,
		"every concurrent increment is counted exactly once (race-clean)")
}

// The stale-entry sweep evicts finished runs once the map grows past the soft cap, using the injectable
// clock. A pre-cap map never sweeps (the common O(1) path); past the cap, entries older than the TTL
// are reclaimed while the live run survives.
func TestRunCallCounterSweepsStaleEntries(t *testing.T) {
	now := time.Now()
	c := newRunCallCounter()
	c.now = func() time.Time { return now }

	// Fill past the soft cap with distinct runs, all "seen" now.
	for i := range runCounterSoftCap + 1 {
		c.increment(fmt.Sprintf("old-run-%d", i))
	}
	require.Greater(t, len(c.m), runCounterSoftCap, "map is over the soft cap before the sweep")

	// Advance the clock past the TTL and touch a fresh, live run: the next increment sweeps the stale
	// entries (all older than TTL) and keeps only the live one.
	now = now.Add(runCounterTTL + time.Minute)
	c.increment("live-run")
	assert.Equal(t, 1, len(c.m), "the sweep reclaimed every stale run, leaving only the live one")
	if e := c.m["live-run"]; assert.NotNil(t, e) {
		assert.Equal(t, 1, e.count)
	}
}
