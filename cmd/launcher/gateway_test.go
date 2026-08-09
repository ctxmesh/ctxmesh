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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockGateway is a stand-in LiteLLM gateway. It counts calls (to prove the
// pre-call circuit-break never reaches the provider), records the last request's
// headers (to prove the internal budget headers are stripped), and returns a
// completion whose usage.total_tokens the caller controls so the deterministic
// price is predictable ($0.000001/token → tokens µ-dollars).
type mockGateway struct {
	server     *httptest.Server
	calls      atomic.Int64
	lastHeader http.Header
	tokens     int
	// costHeader, when non-empty, is set as x-litellm-response-cost so the test
	// can exercise the "reuse LiteLLM's own cost" path.
	costHeader string
}

func newMockGateway(t *testing.T, tokens int) *mockGateway {
	t.Helper()
	m := &mockGateway{tokens: tokens}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		m.lastHeader = r.Header.Clone()
		if m.costHeader != "" {
			w.Header().Set("x-litellm-response-cost", m.costHeader)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"MOCK_OK"}}],"usage":{"total_tokens":%d}}`, m.tokens)
	}))
	t.Cleanup(m.server.Close)
	return m
}

func newTestGatewayProxy(t *testing.T, cfg gatewayConfig) *gatewayProxy {
	t.Helper()
	_, tp := newTestTracer(t)
	gp, err := newGatewayProxy(cfg, tp.Tracer("test"), func(string, ...any) {})
	require.NoError(t, err)
	return gp
}

// doInvoke sends a chat/completions request through the proxy with the given
// conversation id header (empty ⇒ omit it) and returns the recorder.
func doInvoke(gp *gatewayProxy, convID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/chat/completions",
		strings.NewReader(`{"model":"r","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dummy")
	if convID != "" {
		req.Header.Set(hdrConversationID, convID)
	}
	rr := httptest.NewRecorder()
	gp.handler().ServeHTTP(rr, req)
	return rr
}

// ── config gating ───────────────────────────────────────────────────────────

func TestGatewayProxyEnabled_Gate(t *testing.T) {
	// No upstream ⇒ disabled regardless of caps.
	cfg, err := loadGatewayConfig(mapLookup(map[string]string{
		"BUDGET_PER_CONVERSATION_USD": "0.50",
	}), "ag")
	require.NoError(t, err)
	assert.False(t, Config{Gateway: cfg}.GatewayProxyEnabled(), "no upstream URL ⇒ proxy off")

	// Upstream but no cap ⇒ disabled (nothing to enforce).
	cfg, err = loadGatewayConfig(mapLookup(map[string]string{
		"GATEWAY_UPSTREAM_URL": "http://lite:4000",
	}), "ag")
	require.NoError(t, err)
	assert.False(t, Config{Gateway: cfg}.GatewayProxyEnabled(), "no cap ⇒ proxy off")

	// Upstream + a cap ⇒ enabled.
	cfg, err = loadGatewayConfig(mapLookup(map[string]string{
		"GATEWAY_UPSTREAM_URL":        "http://lite:4000",
		"BUDGET_PER_CONVERSATION_USD": "0.50",
		"BUDGET_SOFT_PCT":             "75",
	}), "ag")
	require.NoError(t, err)
	assert.True(t, Config{Gateway: cfg}.GatewayProxyEnabled())
	assert.Equal(t, 75, cfg.SoftPct)
	assert.Equal(t, "ag", cfg.AgentName)
}

// TestGatewayProxyEnabled_GuardrailForcesProxyOn proves the M66 (ADR 0059 §8) contract:
// a GUARDRAIL_POLICY env forces the outbound proxy ON even for an agent with NO budget cap
// and NO tenant quota — a guarded agent must route its LLM calls THROUGH the proxy so the
// in-path guardrail engine can inspect them.
func TestGatewayProxyEnabled_GuardrailForcesProxyOn(t *testing.T) {
	// Upstream + a guardrail policy, but no budget/tenant ⇒ ENABLED (the guardrail forces it on).
	cfg, err := loadGatewayConfig(mapLookup(map[string]string{
		"GATEWAY_UPSTREAM_URL": "http://lite:4000",
		"GUARDRAIL_POLICY":     `{"failMode":"closed","patternDenylist":[{"name":"jb","pattern":"ignore.*instructions"}]}`,
	}), "ag")
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.GuardrailPolicy, "GUARDRAIL_POLICY must be loaded into the config")
	assert.True(t, Config{Gateway: cfg}.GatewayProxyEnabled(),
		"a guardrail policy must force the proxy on even without a budget/tenant")

	// Sanity: upstream present but NO guardrail and NO cap ⇒ still off (byte-compatible pre-M66).
	cfg, err = loadGatewayConfig(mapLookup(map[string]string{
		"GATEWAY_UPSTREAM_URL": "http://lite:4000",
	}), "ag")
	require.NoError(t, err)
	assert.Empty(t, cfg.GuardrailPolicy)
	assert.False(t, Config{Gateway: cfg}.GatewayProxyEnabled(),
		"no guardrail, no cap, no tenant ⇒ proxy stays off")
}

func TestGatewayConfig_SoftPctDefault(t *testing.T) {
	cfg, err := loadGatewayConfig(mapLookup(map[string]string{
		"GATEWAY_UPSTREAM_URL":        "http://lite:4000",
		"BUDGET_PER_CONVERSATION_USD": "0.50",
	}), "ag")
	require.NoError(t, err)
	assert.Equal(t, 80, cfg.SoftPct, "blank BUDGET_SOFT_PCT defaults to 80")

	// Out-of-range values fall back to the default.
	cfg, err = loadGatewayConfig(mapLookup(map[string]string{
		"GATEWAY_UPSTREAM_URL":        "http://lite:4000",
		"BUDGET_PER_CONVERSATION_USD": "0.50",
		"BUDGET_SOFT_PCT":             "0",
	}), "ag")
	require.NoError(t, err)
	assert.Equal(t, 80, cfg.SoftPct)
}

// ── the enforcement path ─────────────────────────────────────────────────────

// TestGatewayProxy_UnderBudgetForwards: a call well under the cap is forwarded to
// LiteLLM and its response is relayed verbatim.
func TestGatewayProxy_UnderBudgetForwards(t *testing.T) {
	mock := newMockGateway(t, 10) // 10 tokens → $0.000010 per call
	gp := newTestGatewayProxy(t, gatewayConfig{
		UpstreamURL: mock.server.URL, AgentName: "ag",
		ConvCapUSD: "1.00", SoftPct: 80,
	})

	rr := doInvoke(gp, "conv-1")
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "MOCK_OK", "upstream body relayed verbatim")
	assert.Equal(t, int64(1), mock.calls.Load(), "provider called exactly once")

	// Spend accrued.
	assert.Equal(t, "0.000010", gp.enforcer.Accountant().ConvSpent("conv-1").String())
}

// TestGatewayProxy_HardBreachRefusesAndDoesNotCallProvider is the milestone
// moment: once spend would exceed the hard cap, the proxy returns a typed
// budget_exceeded 402 and the provider is NOT called.
func TestGatewayProxy_HardBreachRefusesAndDoesNotCallProvider(t *testing.T) {
	// Each call costs $0.30 (300k tokens → $0.30). Cap $0.50.
	mock := newMockGateway(t, 300_000)
	gp := newTestGatewayProxy(t, gatewayConfig{
		UpstreamURL: mock.server.URL, AgentName: "ag",
		ConvCapUSD: "0.50", SoftPct: 80,
	})

	// Call 1: spent 0, estimate = floor (tiny) → allowed. Books $0.30.
	rr := doInvoke(gp, "conv-1")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, int64(1), mock.calls.Load())

	// Call 2: spent $0.30, estimate = last observed $0.30 → 0.60 > 0.50 → REFUSE.
	rr = doInvoke(gp, "conv-1")
	require.Equal(t, http.StatusPaymentRequired, rr.Code, "hard breach returns HTTP 402")
	assert.Equal(t, int64(1), mock.calls.Load(), "provider NOT called on the refused request")

	var body budgetErrorBody
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "budget_exceeded", body.Error)
	assert.Equal(t, "conversation", body.Dimension)
	assert.Equal(t, "0.300000", body.Spent, "reports real already-spent")
	assert.Equal(t, "0.500000", body.Cap)

	// The run keeps being refused (the cap still holds even if the agent retries).
	rr = doInvoke(gp, "conv-1")
	assert.Equal(t, http.StatusPaymentRequired, rr.Code)
	assert.Equal(t, int64(1), mock.calls.Load(), "still refused; provider never re-hit")
}

// TestGatewayProxy_SoftBreachAlertsOnceAndContinues: crossing the soft threshold
// emits a one-shot alert but the call proceeds.
func TestGatewayProxy_SoftBreachAlertsOnceAndContinues(t *testing.T) {
	// Each call $0.30, cap $1.00, soft 80% = $0.80.
	mock := newMockGateway(t, 300_000)
	var alerts atomic.Int64
	_, tp := newTestTracer(t)
	gp, err := newGatewayProxy(gatewayConfig{
		UpstreamURL: mock.server.URL, AgentName: "ag",
		ConvCapUSD: "1.00", SoftPct: 80,
	}, tp.Tracer("test"), func(format string, _ ...any) {
		if strings.Contains(format, "budget.alert") {
			alerts.Add(1)
		}
	})
	require.NoError(t, err)

	// Call 1: $0.30 total → ok, no alert.
	require.Equal(t, http.StatusOK, doInvoke(gp, "conv-1").Code)
	assert.Equal(t, int64(0), alerts.Load())

	// Call 2: $0.60 total → still under soft, no alert.
	require.Equal(t, http.StatusOK, doInvoke(gp, "conv-1").Code)
	assert.Equal(t, int64(0), alerts.Load())

	// Call 3: $0.90 total ≥ soft $0.80 → alert fires ONCE, call still succeeds.
	require.Equal(t, http.StatusOK, doInvoke(gp, "conv-1").Code)
	assert.Equal(t, int64(3), mock.calls.Load(), "soft breach does not block the call")
	assert.Equal(t, int64(1), alerts.Load(), "soft alert is one-shot")
}

// TestGatewayProxy_NoConversationIDStillEnforcesAgentCap: without a conversation
// id, the per-conversation dimension is skipped but a per-agent cap still holds.
func TestGatewayProxy_NoConversationIDStillEnforcesAgentCap(t *testing.T) {
	mock := newMockGateway(t, 300_000) // $0.30/call
	gp := newTestGatewayProxy(t, gatewayConfig{
		UpstreamURL: mock.server.URL, AgentName: "ag",
		AgentCapUSD: "0.50", SoftPct: 80,
	})

	require.Equal(t, http.StatusOK, doInvoke(gp, "").Code) // no conv id
	rr := doInvoke(gp, "")
	assert.Equal(t, http.StatusPaymentRequired, rr.Code, "agent cap trips even without a conversation id")
	var body budgetErrorBody
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "agent", body.Dimension)
}

// TestGatewayProxy_StripsBudgetHeaders: the launcher-internal identity/budget
// headers must NOT leak to LiteLLM; ordinary headers (Authorization) must pass.
func TestGatewayProxy_StripsBudgetHeaders(t *testing.T) {
	mock := newMockGateway(t, 10)
	gp := newTestGatewayProxy(t, gatewayConfig{
		UpstreamURL: mock.server.URL, AgentName: "ag",
		ConvCapUSD: "1.00", SoftPct: 80,
	})

	req := httptest.NewRequest(http.MethodPost, "/chat/completions",
		strings.NewReader(`{"model":"r"}`))
	req.Header.Set("Authorization", "Bearer dummy")
	req.Header.Set(hdrConversationID, "conv-1")
	req.Header.Set(hdrBudgetConvUSD, "1.00") // an agent forging this must be dropped
	gp.handler().ServeHTTP(httptest.NewRecorder(), req)

	require.NotNil(t, mock.lastHeader)
	assert.Equal(t, "Bearer dummy", mock.lastHeader.Get("Authorization"), "ordinary headers pass through")
	assert.Empty(t, mock.lastHeader.Get(hdrConversationID), "X-Conversation-Id stripped")
	assert.Empty(t, mock.lastHeader.Get(hdrBudgetConvUSD), "X-Budget-* stripped")
}

// TestGatewayProxy_ReusesLiteLLMCost: when LiteLLM reports a cost, the proxy
// accounts THAT value (not the token-table fallback).
func TestGatewayProxy_ReusesLiteLLMCost(t *testing.T) {
	mock := newMockGateway(t, 10) // token table would say $0.000010
	mock.costHeader = "0.004200"  // but LiteLLM says $0.0042
	gp := newTestGatewayProxy(t, gatewayConfig{
		UpstreamURL: mock.server.URL, AgentName: "ag",
		ConvCapUSD: "1.00", SoftPct: 80,
	})

	require.Equal(t, http.StatusOK, doInvoke(gp, "conv-1").Code)
	assert.Equal(t, "0.004200", gp.enforcer.Accountant().ConvSpent("conv-1").String(),
		"the LiteLLM-reported cost is what accrues")
}

// TestGatewayProxy_UpstreamErrorNotBooked: a non-200 from LiteLLM costs nothing —
// it must not accrue spend.
func TestGatewayProxy_UpstreamErrorNotBooked(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	t.Cleanup(upstream.Close)

	gp := newTestGatewayProxy(t, gatewayConfig{
		UpstreamURL: upstream.URL, AgentName: "ag",
		ConvCapUSD: "1.00", SoftPct: 80,
	})
	rr := doInvoke(gp, "conv-1")
	assert.Equal(t, http.StatusInternalServerError, rr.Code, "upstream error relayed")
	assert.True(t, gp.enforcer.Accountant().ConvSpent("conv-1").IsZero(), "a failed call accrues no spend")
}

// mapLookup adapts a map to the lookup(string)string signature loadGatewayConfig
// expects.
func mapLookup(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}
