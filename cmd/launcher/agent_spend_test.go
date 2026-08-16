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
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAgentSpendStore is an in-memory agentSpendStore for the booker unit tests.
type fakeAgentSpendStore struct {
	scopeID string
	added   float64
	addErr  error
}

func (f *fakeAgentSpendStore) AddSpend(_ context.Context, scopeID string, delta float64) error {
	f.scopeID = scopeID
	f.added += delta
	return f.addErr
}

// TestAgentSpend_ScopeID verifies the "{ns}/{name}" grammar (and its degradations) matches the Langfuse
// trace-tag identity — including a name that itself contains no ':', so ":spend:" stays an unambiguous
// delimiter downstream.
func TestAgentSpend_ScopeID(t *testing.T) {
	cases := []struct {
		ns, name string
		want     string
		desc     string
	}{
		{"prod", "billing-agent", "prod/billing-agent", "ns/name identity"},
		{"", "solo", "solo", "no namespace → bare name"},
		{"prod", "", "", "no name → not attributable"},
		{"", "", "", "neither → empty"},
	}
	for _, tc := range cases {
		got := agentSpendScopeID(gatewayConfig{AgentNamespace: tc.ns, AgentName: tc.name})
		assert.Equal(t, tc.want, got, tc.desc)
	}
}

// TestAgentSpend_PostCallBooksSameDelta proves the booker INCRBYFLOATs the per-agent key with the actual
// delta, nil-safe, and a non-positive delta is a no-op.
func TestAgentSpend_PostCallBooksSameDelta(t *testing.T) {
	store := &fakeAgentSpendStore{}
	a := &agentSpendAccountant{scopeID: "prod/billing-agent", store: store, logf: noopLog}
	ctx := context.Background()

	a.postCall(ctx, 0.05)
	a.postCall(ctx, 0.03)
	assert.InDelta(t, 0.08, store.added, 1e-9, "each post-call accrues the actual delta")
	assert.Equal(t, "prod/billing-agent", store.scopeID, "booked under the agent's {ns}/{name} scope id")

	// A non-positive delta books nothing (a $0 call).
	a.postCall(ctx, 0)
	a.postCall(ctx, -1)
	assert.InDelta(t, 0.08, store.added, 1e-9, "non-positive deltas are no-ops")

	// A store error is logged, never panics.
	a.store = &fakeAgentSpendStore{addErr: errors.New("valkey down")}
	require.NotPanics(t, func() { a.postCall(ctx, 0.10) })

	// Nil receiver is safe.
	var nilA *agentSpendAccountant
	require.NotPanics(t, func() { nilA.postCall(ctx, 1) })
}

// TestAgentSpend_KeyWithSlash proves the on-the-wire key is agent:{ns}/{name}:spend:{YYYY-MM} against a
// REAL (miniredis) Valkey — the '/' in the scope id lands inside the key exactly, with the same generous
// TTL as the tenant/user spend keys, and INCRBYFLOAT accumulates.
func TestAgentSpend_KeyWithSlash(t *testing.T) {
	mr := miniredis.RunT(t)
	store := newRedisAgentSpendStore(mr.Addr())
	ctx := context.Background()

	require.NoError(t, store.AddSpend(ctx, "prod/billing-agent", 1.25))
	require.NoError(t, store.AddSpend(ctx, "prod/billing-agent", 0.75))

	key := "agent:prod/billing-agent:spend:" + time.Now().UTC().Format("2006-01")
	require.True(t, mr.Exists(key), "spend must live under agent:{ns}/{name}:spend:{YYYY-MM}")
	v, err := mr.Get(key)
	require.NoError(t, err)
	assert.Equal(t, "2", v, "INCRBYFLOAT accumulates the two deltas (1.25 + 0.75 = 2)")

	// The window key carries the same ~2-period TTL as the tenant spend key (self-expiring ledger).
	assert.Equal(t, 62*24*time.Hour, mr.TTL(key), "agent spend key gets the same generous TTL as the tenant key")
}

// TestAgentSpend_TenantKeyUntouched proves the ADDITIVE guarantee: booking per-agent spend touches ONLY
// the agent key — the tenant spend key is written independently by the tenant store and is unaffected.
func TestAgentSpend_TenantKeyUntouched(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()

	tenant := newRedisTenantStore(mr.Addr())
	agent := newRedisAgentSpendStore(mr.Addr())

	// Same $0.40 delta booked to BOTH the tenant aggregate and the agent breakdown (the launcher pattern).
	require.NoError(t, tenant.AddSpend(ctx, "acme", 0.40))
	require.NoError(t, agent.AddSpend(ctx, "prod/billing-agent", 0.40))

	month := time.Now().UTC().Format("2006-01")
	tenantKey := "tenant:acme:spend:" + month
	agentKey := "agent:prod/billing-agent:spend:" + month

	// Tenant key holds ONLY the tenant delta — the agent write did not double-count or corrupt it.
	tv, err := mr.Get(tenantKey)
	require.NoError(t, err)
	assert.Equal(t, "0.4", tv, "tenant spend key holds exactly its own delta — the agent write never touches it")

	av, err := mr.Get(agentKey)
	require.NoError(t, err)
	assert.Equal(t, "0.4", av, "agent spend key holds its own separate delta")

	// The two keys are genuinely distinct (no key aliasing).
	assert.NotEqual(t, tenantKey, agentKey)
}

// TestNewAgentSpendAccountant_Degradations proves the builder mirrors buildUserQuota's "no direct Valkey
// → OFF" degradation and the unnamed-agent → OFF case (a nil-safe no-op, never a blocked call).
func TestNewAgentSpendAccountant_Degradations(t *testing.T) {
	// Named agent + a direct Valkey addr → a live booker.
	live := gatewayConfig{AgentNamespace: "prod", AgentName: "foo", QuotaAddr: "127.0.0.1:6379"}
	a := newAgentSpendAccountant(live, noopLog)
	require.NotNil(t, a)
	assert.Equal(t, "prod/foo", a.scopeID)

	// No direct Valkey addr (state-layer-proxy-only path) → OFF (nil), never a crash.
	proxyOnly := gatewayConfig{AgentNamespace: "prod", AgentName: "foo", StatelayerProxyURL: "http://proxy"}
	off := newAgentSpendAccountant(proxyOnly, noopLog)
	assert.Nil(t, off, "no TENANT_QUOTA_ADDR → per-agent rollup OFF (like the per-user quota)")

	// Unnamed agent → OFF (nothing to key on) even with a Valkey addr.
	unnamed := newAgentSpendAccountant(gatewayConfig{QuotaAddr: "127.0.0.1:6379"}, noopLog)
	assert.Nil(t, unnamed, "an unnamed agent is not per-agent-attributable → OFF")
}
