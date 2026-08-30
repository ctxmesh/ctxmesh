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

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/runcap"
)

// fakeUserStore is an in-memory userQuotaStore for the enforcer unit tests (mirrors fakeTenantStore).
type fakeUserStore struct {
	rpmCount   int64
	rpmErr     error
	spend      float64
	spendErr   error
	added      float64
	addErr     error
	inflight   int
	acquireErr error
	released   int
}

func (f *fakeUserStore) IncrRPM(_ context.Context, _ string, _ int64) (int64, error) {
	if f.rpmErr != nil {
		return 0, f.rpmErr
	}
	f.rpmCount++
	return f.rpmCount, nil
}

func (f *fakeUserStore) Spend(_ context.Context, _ string) (float64, error) {
	return f.spend, f.spendErr
}

func (f *fakeUserStore) AddSpend(_ context.Context, _ string, delta float64) error {
	f.added += delta
	return f.addErr
}

func (f *fakeUserStore) AcquireSlot(_ context.Context, _ string, maxSlots int) (bool, error) {
	if f.acquireErr != nil {
		return false, f.acquireErr
	}
	if f.inflight >= maxSlots {
		return false, nil
	}
	f.inflight++
	return true, nil
}

func (f *fakeUserStore) ReleaseSlot(_ context.Context, _ string) error {
	if f.inflight > 0 {
		f.inflight--
	}
	f.released++
	return nil
}

const testUserHash = "u-alicehash"

// The per-user RPM cap allows up to the limit, then 429s.
func TestUserQuota_RateLimit(t *testing.T) {
	store := &fakeUserStore{}
	q := &userQuota{rpm: 2, store: store, logf: noopLog}
	ctx := context.Background()

	d1, _ := q.preCall(ctx, testUserHash, 0)
	assert.Nil(t, d1, "1st call under the cap")
	d2, _ := q.preCall(ctx, testUserHash, 0)
	assert.Nil(t, d2, "2nd call at the cap")
	deny, _ := q.preCall(ctx, testUserHash, 0)
	require.NotNil(t, deny, "3rd call over the cap must be denied")
	assert.Equal(t, 429, deny.status)
	assert.Equal(t, "user_rate_limited", deny.code)
}

// Rate fails OPEN: a Valkey error on the RPM path ALLOWS (a blip must not 429 a legitimate user).
func TestUserQuota_RateFailsOpen(t *testing.T) {
	store := &fakeUserStore{rpmErr: errors.New("valkey down")}
	q := &userQuota{rpm: 1, store: store, logf: noopLog}
	d, _ := q.preCall(context.Background(), testUserHash, 0)
	assert.Nil(t, d, "an rpm store error must fail OPEN (ADR 0059 §8)")
}

// The per-user budget allows while spent+estimate ≤ cap, then 402s with spent/cap.
func TestUserQuota_Budget(t *testing.T) {
	store := &fakeUserStore{spend: 4.0}
	q := &userQuota{budgetUSD: 5.0, hasBudget: true, store: store, logf: noopLog}
	ctx := context.Background()

	d, _ := q.preCall(ctx, testUserHash, 0.5)
	assert.Nil(t, d, "4.0 + 0.5 ≤ 5.0 allowed")

	store.spend = 4.8
	deny, _ := q.preCall(ctx, testUserHash, 0.5)
	require.NotNil(t, deny, "4.8 + 0.5 > 5.0 must be denied")
	assert.Equal(t, 402, deny.status)
	assert.Equal(t, "user_budget_exceeded", deny.code)
	assert.InDelta(t, 4.8, deny.spent, 0.001)
	assert.InDelta(t, 5.0, deny.capUSD, 0.001)
}

// Money fails CLOSED: a Valkey read error on the budget path DENIES (402), never lets spend run past.
func TestUserQuota_BudgetFailsClosed(t *testing.T) {
	store := &fakeUserStore{spendErr: errors.New("valkey down")}
	q := &userQuota{budgetUSD: 5.0, hasBudget: true, store: store, logf: noopLog}
	deny, _ := q.preCall(context.Background(), testUserHash, 1)
	require.NotNil(t, deny, "a budget read error must fail CLOSED (ADR 0059 §8)")
	assert.Equal(t, 402, deny.status)
	assert.Equal(t, "user_budget_exceeded", deny.code)
}

// The concurrency cap allows up to maxInFlight in-flight; the next is 429'd; releasing frees capacity; and a
// store error on acquire fails OPEN.
func TestUserQuota_Concurrency(t *testing.T) {
	store := &fakeUserStore{}
	q := &userQuota{maxConcurrent: 2, store: store, logf: noopLog}
	ctx := context.Background()

	d1, rel1 := q.preCall(ctx, testUserHash, 0)
	require.Nil(t, d1, "1st in-flight allowed")
	d2, rel2 := q.preCall(ctx, testUserHash, 0)
	require.Nil(t, d2, "2nd in-flight allowed")

	deny, _ := q.preCall(ctx, testUserHash, 0)
	require.NotNil(t, deny, "3rd concurrent call must be denied")
	assert.Equal(t, 429, deny.status)
	assert.Equal(t, "user_concurrency_exceeded", deny.code)

	rel1() // free a slot
	assert.Equal(t, 1, store.released, "releasing the deferred func must decrement in-flight")
	d3, rel3 := q.preCall(ctx, testUserHash, 0)
	require.Nil(t, d3, "a call is allowed again after a slot frees")
	rel2()
	rel3()

	// A Valkey error on acquire fails OPEN.
	store.acquireErr = errors.New("valkey down")
	dOpen, _ := q.preCall(ctx, testUserHash, 0)
	assert.Nil(t, dOpen, "a concurrency store error must fail OPEN")
}

// postCall accrues actual spend only when a budget is set; blank userHash / nil-receiver are no-ops.
func TestUserQuota_PostCallAndNilSafe(t *testing.T) {
	store := &fakeUserStore{}
	q := &userQuota{budgetUSD: 5.0, hasBudget: true, store: store, logf: noopLog}
	q.postCall(context.Background(), testUserHash, 0.05)
	assert.InDelta(t, 0.05, store.added, 0.0001)

	// Blank userHash → no accrual (a missing/forged capability booked no user).
	q.postCall(context.Background(), "", 0.05)
	assert.InDelta(t, 0.05, store.added, 0.0001, "blank userHash must not accrue")

	// No budget → no accrual.
	noBudget := &userQuota{store: store, logf: noopLog}
	noBudget.postCall(context.Background(), testUserHash, 0.05)
	assert.InDelta(t, 0.05, store.added, 0.0001, "no-budget quota must not accrue")

	// Nil receiver is safe on both paths.
	var nilQ *userQuota
	d, rel := nilQ.preCall(context.Background(), testUserHash, 1)
	assert.Nil(t, d)
	rel() // noop release must not panic
	nilQ.postCall(context.Background(), testUserHash, 1)

	// Blank userHash on preCall is a no-op (skip enforcement).
	d2, rel2 := q.preCall(context.Background(), "", 1)
	assert.Nil(t, d2, "blank userHash must skip per-user enforcement")
	rel2()
}

// TestUserQuota_CrossPodCoordination proves the per-user cap is SHARED across replicas against a REAL
// (miniredis) Valkey: two independent launcher instances ("pods") each with their own redis-backed store,
// keyed on the SAME userHash, collectively share ONE bucket — so a single user cannot overrun by fanning out
// across replicas. Exercises the actual INCR / GET / INCRBYFLOAT / DECR commands cross-instance.
//
// This is the established real-Valkey pattern for the quota path (mirrors TestTenantQuota_CrossPodCoordination);
// there is no separate DSN-gated real-Valkey integration test for the launcher quota path, so nothing is SKIP'd.
func TestUserQuota_CrossPodCoordination(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()
	pod := func(rpm int, budget float64, maxConc int) *userQuota {
		return &userQuota{
			rpm: rpm, budgetUSD: budget, hasBudget: budget > 0, maxConcurrent: maxConc,
			store: newRedisUserStore(mr.Addr()), logf: noopLog,
		}
	}

	t.Run("rate is shared across pods", func(t *testing.T) {
		a, b := pod(3, 0, 0), pod(3, 0, 0)
		d1, _ := a.preCall(ctx, "rate-u", 0)
		d2, _ := a.preCall(ctx, "rate-u", 0)
		d3, _ := b.preCall(ctx, "rate-u", 0) // 3rd across pods — still at the cap
		require.Nil(t, d1)
		require.Nil(t, d2)
		require.Nil(t, d3)
		d4, _ := b.preCall(ctx, "rate-u", 0) // 4th across pods — over the shared cap of 3
		require.NotNil(t, d4, "the 4th call across two pods must hit the shared rpm cap")
		assert.Equal(t, 429, d4.status)
	})

	t.Run("spend is shared across pods", func(t *testing.T) {
		a, b := pod(0, 1.0, 0), pod(0, 1.0, 0)
		a.postCall(ctx, "spend-u", 0.60) // pod A books $0.60
		deny, _ := b.preCall(ctx, "spend-u", 0.60)
		require.NotNil(t, deny, "pod B must see pod A's spend and deny over the shared budget")
		assert.Equal(t, 402, deny.status)
		assert.InDelta(t, 0.60, deny.spent, 0.001)

		// Spend lives under the per-user, per-UTC-month window key — a RECURRING ceiling (mirroring the
		// tenant window): a new month starts at 0, and an operator resets mid-cycle by DELeting the key.
		key := "user:spend-u:spend:" + time.Now().UTC().Format("2006-01")
		assert.True(t, mr.Exists(key), "per-user spend must live under the monthly window key")
		assert.False(t, mr.Exists("user:spend-u:spend:2099-01"), "a future period starts at 0")
		mr.Del(key)
		reset, err := a.store.Spend(ctx, "spend-u")
		require.NoError(t, err)
		assert.InDelta(t, 0.0, reset, 0.001, "deleting the window key resets the per-user budget")
	})

	t.Run("concurrency is shared across pods, and postCall releases the slot", func(t *testing.T) {
		a, b := pod(0, 0, 2), pod(0, 0, 2)
		_, rel1 := a.preCall(ctx, "conc-u", 0) // slot 1 (pod A)
		d2, _ := b.preCall(ctx, "conc-u", 0)   // slot 2 (pod B)
		require.Nil(t, d2)
		d3, _ := a.preCall(ctx, "conc-u", 0) // slot 3 across pods — over the shared cap of 2
		require.NotNil(t, d3, "the 3rd concurrent call across two pods must hit the shared cap")
		assert.Equal(t, 429, d3.status)
		assert.Equal(t, "user_concurrency_exceeded", d3.code)

		rel1() // releasing frees a slot cross-pod
		d4, rel4 := b.preCall(ctx, "conc-u", 0)
		require.Nil(t, d4, "a slot frees after release")
		rel4()
	})
}

// ── gateway wire tests: capability verification + per-user enforcement end to end ─────────────────────────

const testCapAudience = "credential-plane"

// userLimitPolicy is a GUARDRAIL_POLICY carrying only a userRateLimit (the fields under test).
func userLimitPolicy(rpm int, spendUSD string, maxInFlight int) string {
	url := userRateLimitConfig{RequestsPerMinute: rpm, SpendUSD: spendUSD, MaxInFlight: maxInFlight}
	b, _ := json.Marshal(guardrailPolicyConfig{UserRateLimit: &url})
	return string(b)
}

// newUserQuotaGateway builds a real gatewayProxy wired for per-user limits: a mock LiteLLM upstream, a
// miniredis-backed quota (via TENANT_QUOTA_ADDR), the userRateLimit policy, and the capability key/audience
// in env (the same MCP_CAPABILITY_* the OBO egress path uses). It returns the proxy plus a signer minting
// VALID capabilities for that key so a test can present a genuine token.
func newUserQuotaGateway(t *testing.T, policy string) (*gatewayProxy, *runcap.Signer) {
	t.Helper()
	mock := newMockGateway(t, 10) // 10 tokens → $0.000010/call

	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	t.Setenv("MCP_CAPABILITY_PUBLIC_KEY", runcap.EncodePublicKey(pub))
	t.Setenv("MCP_CAPABILITY_AUDIENCE", testCapAudience)

	mr := miniredis.RunT(t)
	cfg := gatewayConfig{
		UpstreamURL:     mock.server.URL,
		Port:            0,
		AgentName:       "ag",
		QuotaAddr:       mr.Addr(),
		GuardrailPolicy: policy,
	}
	gp := newTestGatewayProxy(t, cfg)
	require.NotNil(t, gp.userQuota(), "userRateLimit must build a per-user quota")
	require.NotNil(t, gp.capVerifier, "a provisioned capability key must build a verifier")

	signer := runcap.NewSigner(priv, testCapAudience, nil)
	return gp, signer
}

// mintCap mints a valid run capability for the given (hashed) user.
func mintCap(t *testing.T, signer *runcap.Signer, user string) string {
	t.Helper()
	tok, err := signer.Mint(runcap.MintRequest{User: user, Agent: "ag", RunID: "run-1", TTL: 5 * time.Minute})
	require.NoError(t, err)
	return tok
}

// invokeWithCap drives a chat/completions request through the proxy with an optional capability header.
func invokeWithCap(gp *gatewayProxy, capTok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/chat/completions",
		strings.NewReader(`{"model":"r","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dummy")
	if capTok != "" {
		req.Header.Set(runcap.HeaderName, capTok)
	}
	rr := httptest.NewRecorder()
	gp.handler().ServeHTTP(rr, req)
	return rr
}

// A valid capability + a per-user rpm cap: the (rpm+1)-th call from the SAME user is 429'd.
func TestGatewayUserQuota_ValidCapability_RPMOverLimit429(t *testing.T) {
	gp, signer := newUserQuotaGateway(t, userLimitPolicy(2, "", 0))
	capTok := mintCap(t, signer, "u-alice")

	assert.Equal(t, http.StatusOK, invokeWithCap(gp, capTok).Code, "1st under cap")
	assert.Equal(t, http.StatusOK, invokeWithCap(gp, capTok).Code, "2nd at cap")
	rr := invokeWithCap(gp, capTok)
	assert.Equal(t, http.StatusTooManyRequests, rr.Code, "3rd over the per-user rpm cap → 429")
	assertUserDenyBody(t, rr, "user_rate_limited")
}

// A valid capability + a per-user spend cap: once the shared monthly spend would breach the cap, the next
// call is 402'd; postCall books the actual spend so the ceiling is reached across calls.
func TestGatewayUserQuota_ValidCapability_SpendOverCap402(t *testing.T) {
	// Cap so low a single 10-token ($0.000010) call's spend + estimate breaches it on the next call.
	gp, signer := newUserQuotaGateway(t, userLimitPolicy(0, "0.000005", 0))
	capTok := mintCap(t, signer, "u-bob")

	// 1st call: spent(0) + est(0) ≤ cap → allowed; books $0.000010 (> cap) for the next read.
	assert.Equal(t, http.StatusOK, invokeWithCap(gp, capTok).Code, "1st call allowed (nothing spent yet)")
	// 2nd call: spent($0.000010) already exceeds the $0.000005 cap → 402.
	rr := invokeWithCap(gp, capTok)
	assert.Equal(t, http.StatusPaymentRequired, rr.Code, "spend over cap → 402")
	assertUserDenyBody(t, rr, "user_budget_exceeded")
}

// A valid capability + maxInFlight: two concurrent held slots then a 3rd is 429'd; after a call finishes the
// slot is released and capacity returns (proves postCall/defer release wired into serve()).
func TestGatewayUserQuota_ValidCapability_InFlightOverMax429AndReleases(t *testing.T) {
	gp, signer := newUserQuotaGateway(t, userLimitPolicy(0, "", 1))
	capTok := mintCap(t, signer, "u-carol")

	// Each invoke completes synchronously (the deferred release runs before the recorder returns), so a cap
	// of 1 is never exceeded by sequential calls — assert the slot is freed each time.
	assert.Equal(t, http.StatusOK, invokeWithCap(gp, capTok).Code, "1st completes, slot released")
	assert.Equal(t, http.StatusOK, invokeWithCap(gp, capTok).Code, "2nd reuses the freed slot")

	// Now hold a slot open by pre-acquiring directly, then a request through serve must be denied.
	held, relHeld := gp.userQuota().preCall(context.Background(), "u-carol", 0)
	require.Nil(t, held, "manual acquire of the single slot")
	rr := invokeWithCap(gp, capTok)
	assert.Equal(t, http.StatusTooManyRequests, rr.Code, "the slot is held → 429")
	assertUserDenyBody(t, rr, "user_concurrency_exceeded")
	relHeld()
	assert.Equal(t, http.StatusOK, invokeWithCap(gp, capTok).Code, "after release the call is allowed again")
}

// MISSING capability + a userRateLimit configured → the call PROCEEDS (fail-open, per-user skipped). Even a
// tiny rpm cap of 1 does not throttle when the caller presents no capability — there is no trusted user to key.
func TestGatewayUserQuota_MissingCapability_FailsOpen(t *testing.T) {
	gp, _ := newUserQuotaGateway(t, userLimitPolicy(1, "", 0))
	for range 5 {
		rr := invokeWithCap(gp, "") // no capability header
		assert.Equal(t, http.StatusOK, rr.Code, "a call with no capability must NOT be per-user throttled (fail-open)")
	}
}

// A FORGED capability (signed by a DIFFERENT key) → treated as ABSENT: the call proceeds (fail-open per-user)
// and the forged token grants NO userHash (it cannot spoof a user into the per-user bucket).
func TestGatewayUserQuota_ForgedCapability_NotTrusted(t *testing.T) {
	gp, _ := newUserQuotaGateway(t, userLimitPolicy(1, "", 0))

	// A signer with a DIFFERENT key pair than the gateway's verifier trusts.
	_, evilPriv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	forged := runcap.NewSigner(evilPriv, testCapAudience, nil)
	forgedTok, err := forged.Mint(runcap.MintRequest{User: "u-attacker", Agent: "ag", RunID: "r", TTL: time.Minute})
	require.NoError(t, err)

	// The verifier rejects the bad signature ⇒ userHash "" ⇒ fail-open, NOT trusted as u-attacker.
	assert.Equal(t, "", gp.userHashFromRequest(mustReq(forgedTok), gp.userQuota()),
		"a forged capability must NOT yield a userHash")

	// End to end: repeated calls with the forged token are NOT throttled (fail-open) — proving the forged
	// identity never entered the per-user bucket (had it been trusted as u-attacker, the rpm-1 cap would 429).
	for range 4 {
		rr := invokeWithCap(gp, forgedTok)
		assert.Equal(t, http.StatusOK, rr.Code,
			"a forged capability must be treated as absent → fail-open, never spoofing a user")
	}
}

// A valid capability's user id is NOT leaked upstream: the capability header is stripped before forwarding to
// LiteLLM (it is a launcher-internal identity header, m66.7).
func TestGatewayUserQuota_CapabilityHeaderStrippedUpstream(t *testing.T) {
	mock := newMockGateway(t, 10)
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	t.Setenv("MCP_CAPABILITY_PUBLIC_KEY", runcap.EncodePublicKey(pub))
	t.Setenv("MCP_CAPABILITY_AUDIENCE", testCapAudience)
	mr := miniredis.RunT(t)
	gp := newTestGatewayProxy(t, gatewayConfig{
		UpstreamURL: mock.server.URL, AgentName: "ag", QuotaAddr: mr.Addr(),
		GuardrailPolicy: userLimitPolicy(10, "", 0),
	})
	signer := runcap.NewSigner(priv, testCapAudience, nil)
	capTok := mintCap(t, signer, "u-dora")

	require.Equal(t, http.StatusOK, invokeWithCap(gp, capTok).Code)
	assert.Empty(t, mock.lastHeader.Get(runcap.HeaderName), "the run capability must NOT leak to LiteLLM")
}

// mustReq builds a bare request carrying the capability header (for direct userHashFromRequest tests).
func mustReq(capTok string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader("{}"))
	if capTok != "" {
		req.Header.Set(runcap.HeaderName, capTok)
	}
	return req
}

func assertUserDenyBody(t *testing.T, rr *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var body budgetErrorBody
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "deny body must be JSON")
	assert.Equal(t, wantCode, body.Error, "deny code")
	assert.Equal(t, "user", body.Dimension, "deny dimension must be 'user'")
}

// A userRateLimit configured but NO capability key provisioned → the verifier is nil, so per-user enforcement
// is OFF (fail-open): a valid-looking token cannot be verified, and calls proceed unthrottled.
func TestGatewayUserQuota_NoKey_FailsOpen(t *testing.T) {
	mock := newMockGateway(t, 10)
	// Deliberately DO NOT set MCP_CAPABILITY_PUBLIC_KEY.
	t.Setenv("MCP_CAPABILITY_PUBLIC_KEY", "")
	mr := miniredis.RunT(t)
	gp := newTestGatewayProxy(t, gatewayConfig{
		UpstreamURL: mock.server.URL, AgentName: "ag", QuotaAddr: mr.Addr(),
		GuardrailPolicy: userLimitPolicy(1, "", 0),
	})
	// No verifier ⇒ userHashFromRequest returns "" for any token ⇒ per-user skipped.
	assert.Nil(t, gp.capVerifier, "no key ⇒ no verifier")
	for range 3 {
		assert.Equal(t, http.StatusOK, invokeWithCap(gp, "any").Code, "no key ⇒ per-user fails open")
	}
}

// A userRateLimit configured but NO shared Valkey addr → the per-user quota cannot be built, so enforcement
// stays OFF (fail-open) rather than blocking model calls.
func TestGatewayUserQuota_NoQuotaAddr_Disabled(t *testing.T) {
	mock := newMockGateway(t, 10)
	gp := newTestGatewayProxy(t, gatewayConfig{
		UpstreamURL: mock.server.URL, AgentName: "ag",
		GuardrailPolicy: userLimitPolicy(1, "", 0), // rpm=1 but no QuotaAddr
	})
	assert.Nil(t, gp.userQuota(), "no TENANT_QUOTA_ADDR ⇒ per-user quota not built")
	for range 3 {
		assert.Equal(t, http.StatusOK, invokeWithCap(gp, "").Code, "no store ⇒ per-user disabled, calls proceed")
	}
}

// Sanity: a policy WITHOUT a userRateLimit builds no per-user quota (byte-compatible pre-m66.7).
func TestGatewayUserQuota_NoUserRateLimit_NoQuota(t *testing.T) {
	mock := newMockGateway(t, 10)
	mr := miniredis.RunT(t)
	gp := newTestGatewayProxy(t, gatewayConfig{
		UpstreamURL: mock.server.URL, AgentName: "ag", QuotaAddr: mr.Addr(),
		GuardrailPolicy: `{"failMode":"closed"}`, // no userRateLimit
	})
	assert.Nil(t, gp.userQuota(), "no userRateLimit ⇒ no per-user quota")
	assert.Equal(t, http.StatusOK, invokeWithCap(gp, "").Code)
}
