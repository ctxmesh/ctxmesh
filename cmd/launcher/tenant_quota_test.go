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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTenantStore is an in-memory tenantQuotaStore for the enforcer unit tests.
type fakeTenantStore struct {
	rpmCount int64
	rpmErr   error
	spend    float64
	spendErr error
	added    float64
	addErr   error
}

func (f *fakeTenantStore) IncrRPM(_ context.Context, _ string, _ int64) (int64, error) {
	if f.rpmErr != nil {
		return 0, f.rpmErr
	}
	f.rpmCount++
	return f.rpmCount, nil
}

func (f *fakeTenantStore) Spend(_ context.Context, _ string) (float64, error) {
	return f.spend, f.spendErr
}

func (f *fakeTenantStore) AddSpend(_ context.Context, _ string, delta float64) error {
	f.added += delta
	return f.addErr
}

func noopLog(string, ...any) {}

// The RPM cap allows up to the limit, then 429s.
func TestTenantQuota_RateLimit(t *testing.T) {
	store := &fakeTenantStore{}
	q := &tenantQuota{id: "acme", rpm: 2, store: store, logf: noopLog}
	ctx := context.Background()

	assert.Nil(t, q.preCall(ctx, 0), "1st call under the cap")
	assert.Nil(t, q.preCall(ctx, 0), "2nd call at the cap")
	deny := q.preCall(ctx, 0)
	require.NotNil(t, deny, "3rd call over the cap must be denied")
	assert.Equal(t, 429, deny.status)
	assert.Equal(t, "tenant_rate_limited", deny.code)
}

// The aggregate budget allows while spent+estimate ≤ cap, then 402s with spent/cap.
func TestTenantQuota_Budget(t *testing.T) {
	store := &fakeTenantStore{spend: 90}
	q := &tenantQuota{id: "acme", budgetUSD: 100, hasBudget: true, store: store, logf: noopLog}
	ctx := context.Background()

	assert.Nil(t, q.preCall(ctx, 5), "90 + 5 ≤ 100 allowed")

	store.spend = 98
	deny := q.preCall(ctx, 5)
	require.NotNil(t, deny, "98 + 5 > 100 must be denied")
	assert.Equal(t, 402, deny.status)
	assert.Equal(t, "tenant_budget_exceeded", deny.code)
	assert.InDelta(t, 98.0, deny.spent, 0.001)
	assert.InDelta(t, 100.0, deny.capUSD, 0.001)
}

// Money fails CLOSED: a Valkey read error on the budget path denies (402), never lets spend run past.
func TestTenantQuota_BudgetFailsClosed(t *testing.T) {
	store := &fakeTenantStore{spendErr: errors.New("valkey down")}
	q := &tenantQuota{id: "acme", budgetUSD: 100, hasBudget: true, store: store, logf: noopLog}
	deny := q.preCall(context.Background(), 1)
	require.NotNil(t, deny, "a budget read error must fail closed")
	assert.Equal(t, 402, deny.status)
}

// Rate fails OPEN: a Valkey error on the RPM path allows (a blip must not 429 a whole tenant).
func TestTenantQuota_RateFailsOpen(t *testing.T) {
	store := &fakeTenantStore{rpmErr: errors.New("valkey down")}
	q := &tenantQuota{id: "acme", rpm: 1, store: store, logf: noopLog}
	assert.Nil(t, q.preCall(context.Background(), 0), "an rpm error must fail open")
}

// postCall accrues actual spend only when a budget is set; nil-receiver is a no-op.
func TestTenantQuota_PostCallAndNilSafe(t *testing.T) {
	store := &fakeTenantStore{}
	q := &tenantQuota{id: "acme", budgetUSD: 100, hasBudget: true, store: store, logf: noopLog}
	q.postCall(context.Background(), 0.05)
	assert.InDelta(t, 0.05, store.added, 0.0001)

	// No budget → no accrual.
	noBudget := &tenantQuota{id: "x", store: store, logf: noopLog}
	noBudget.postCall(context.Background(), 0.05)
	assert.InDelta(t, 0.05, store.added, 0.0001, "no-budget tenant must not accrue")

	// Nil receiver is safe on both paths.
	var nilQ *tenantQuota
	assert.Nil(t, nilQ.preCall(context.Background(), 1))
	nilQ.postCall(context.Background(), 1)
}

func TestMoneyToFloat(t *testing.T) {
	assert.InDelta(t, 100.0, moneyToFloat("100.00"), 0.0001)
	assert.InDelta(t, 0.0035, moneyToFloat("0.0035"), 0.00001)
	assert.InDelta(t, 0.0, moneyToFloat(""), 0.0001, "unparseable → 0")
}
