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

// fakeTenantStore is an in-memory tenantQuotaStore for the enforcer unit tests.
type fakeTenantStore struct {
	rpmCount   int64
	rpmErr     error
	spend      float64
	spendErr   error
	added      float64
	addErr     error
	inflight   int
	acquireErr error
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

func (f *fakeTenantStore) AcquireSlot(_ context.Context, _ string, maxSlots int) (bool, error) {
	if f.acquireErr != nil {
		return false, f.acquireErr
	}
	if f.inflight >= maxSlots {
		return false, nil
	}
	f.inflight++
	return true, nil
}

func (f *fakeTenantStore) ReleaseSlot(_ context.Context, _ string) error {
	if f.inflight > 0 {
		f.inflight--
	}
	return nil
}

func noopLog(string, ...any) {}

// The RPM cap allows up to the limit, then 429s.
func TestTenantQuota_RateLimit(t *testing.T) {
	store := &fakeTenantStore{}
	q := &tenantQuota{id: "acme", rpm: 2, store: store, logf: noopLog}
	ctx := context.Background()

	d1, _ := q.preCall(ctx, 0)
	assert.Nil(t, d1, "1st call under the cap")
	d2, _ := q.preCall(ctx, 0)
	assert.Nil(t, d2, "2nd call at the cap")
	deny, _ := q.preCall(ctx, 0)
	require.NotNil(t, deny, "3rd call over the cap must be denied")
	assert.Equal(t, 429, deny.status)
	assert.Equal(t, "tenant_rate_limited", deny.code)
}

// The aggregate budget allows while spent+estimate ≤ cap, then 402s with spent/cap.
func TestTenantQuota_Budget(t *testing.T) {
	store := &fakeTenantStore{spend: 90}
	q := &tenantQuota{id: "acme", budgetUSD: 100, hasBudget: true, store: store, logf: noopLog}
	ctx := context.Background()

	d, _ := q.preCall(ctx, 5)
	assert.Nil(t, d, "90 + 5 ≤ 100 allowed")

	store.spend = 98
	deny, _ := q.preCall(ctx, 5)
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
	deny, _ := q.preCall(context.Background(), 1)
	require.NotNil(t, deny, "a budget read error must fail closed")
	assert.Equal(t, 402, deny.status)
}

// Rate fails OPEN: a Valkey error on the RPM path allows (a blip must not 429 a whole tenant).
func TestTenantQuota_RateFailsOpen(t *testing.T) {
	store := &fakeTenantStore{rpmErr: errors.New("valkey down")}
	q := &tenantQuota{id: "acme", rpm: 1, store: store, logf: noopLog}
	d, _ := q.preCall(context.Background(), 0)
	assert.Nil(t, d, "an rpm error must fail open")
}

// The concurrency cap allows up to maxConcurrent in-flight calls; the next is 429'd, and releasing a slot
// frees capacity again. Concurrency fails OPEN on a Valkey error.
func TestTenantQuota_Concurrency(t *testing.T) {
	store := &fakeTenantStore{}
	q := &tenantQuota{id: "acme", maxConcurrent: 2, store: store, logf: noopLog}
	ctx := context.Background()

	d1, rel1 := q.preCall(ctx, 0)
	require.Nil(t, d1, "1st in-flight allowed")
	d2, rel2 := q.preCall(ctx, 0)
	require.Nil(t, d2, "2nd in-flight allowed")

	deny, _ := q.preCall(ctx, 0)
	require.NotNil(t, deny, "3rd concurrent call must be denied")
	assert.Equal(t, 429, deny.status)
	assert.Equal(t, "tenant_concurrency_exceeded", deny.code)

	rel1() // free a slot
	d3, rel3 := q.preCall(ctx, 0)
	require.Nil(t, d3, "a call is allowed again after a slot frees")
	rel2()
	rel3()

	// A Valkey error on acquire fails open.
	store.acquireErr = errors.New("valkey down")
	dOpen, _ := q.preCall(ctx, 0)
	assert.Nil(t, dOpen, "a concurrency store error must fail open")
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
	d, rel := nilQ.preCall(context.Background(), 1)
	assert.Nil(t, d)
	rel() // noop release must not panic
	nilQ.postCall(context.Background(), 1)
}

// TestTenantQuota_CrossPodCoordination proves the load-bearing M47 property against a REAL (miniredis)
// Valkey: two independent launcher instances ("pods") of the SAME tenant, each with its own redis-backed
// store, collectively share ONE bucket — so their combined rate / spend / concurrency hits the tenant cap
// (PRD §18: replicas cannot collectively overrun). The unit tests above use a fake; this exercises the
// actual INCR / GET / INCRBYFLOAT / DECR commands cross-instance.
func TestTenantQuota_CrossPodCoordination(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()
	// Two "pods" of the same tenant, each with its OWN redis client to the SAME shared Valkey.
	pod := func(id string, rpm int, budget float64, maxConc int) *tenantQuota {
		return &tenantQuota{
			id: id, rpm: rpm, budgetUSD: budget, hasBudget: budget > 0, maxConcurrent: maxConc,
			store: newRedisTenantStore(mr.Addr()), logf: noopLog,
		}
	}

	t.Run("rate is shared across pods", func(t *testing.T) {
		a, b := pod("rate-t", 3, 0, 0), pod("rate-t", 3, 0, 0)
		d1, _ := a.preCall(ctx, 0)
		d2, _ := a.preCall(ctx, 0)
		d3, _ := b.preCall(ctx, 0) // 3rd across pods — still at the cap
		require.Nil(t, d1)
		require.Nil(t, d2)
		require.Nil(t, d3)
		d4, _ := b.preCall(ctx, 0) // 4th across pods — over the shared cap of 3
		require.NotNil(t, d4, "the 4th call across two pods must hit the shared rpm cap")
		assert.Equal(t, 429, d4.status)
	})

	t.Run("spend is shared across pods", func(t *testing.T) {
		a, b := pod("spend-t", 0, 1.0, 0), pod("spend-t", 0, 1.0, 0)
		a.postCall(ctx, 0.60) // pod A books $0.60
		// pod B now sees the shared $0.60; another $0.60 estimate would breach the $1.00 cap.
		deny, _ := b.preCall(ctx, 0.60)
		require.NotNil(t, deny, "pod B must see pod A's spend and deny over the shared budget")
		assert.Equal(t, 402, deny.status)
		assert.InDelta(t, 0.60, deny.spent, 0.001)
	})

	t.Run("concurrency is shared across pods", func(t *testing.T) {
		a, b := pod("conc-t", 0, 0, 2), pod("conc-t", 0, 0, 2)
		d1, _ := a.preCall(ctx, 0) // slot 1 (pod A)
		d2, _ := b.preCall(ctx, 0) // slot 2 (pod B)
		require.Nil(t, d1)
		require.Nil(t, d2)
		d3, _ := a.preCall(ctx, 0) // slot 3 across pods — over the shared cap of 2
		require.NotNil(t, d3, "the 3rd concurrent call across two pods must hit the shared cap")
		assert.Equal(t, 429, d3.status)
		assert.Equal(t, "tenant_concurrency_exceeded", d3.code)
	})
}

// The tenant budget is a RECURRING monthly ceiling (M48, ADR 0047), not a lifetime cap: spend lives
// under a per-month window key, and an operator can reset mid-cycle by DELeting it.
func TestTenantQuota_RecurringSpendWindow(t *testing.T) {
	mr := miniredis.RunT(t)
	store := newRedisTenantStore(mr.Addr())
	ctx := context.Background()

	require.NoError(t, store.AddSpend(ctx, "acme", 3.50))
	require.NoError(t, store.AddSpend(ctx, "acme", 1.50))
	spent, err := store.Spend(ctx, "acme")
	require.NoError(t, err)
	assert.InDelta(t, 5.0, spent, 0.001)

	// Spend lives under the current UTC-month window key — a future month is independent (the reset).
	key := "tenant:acme:spend:" + time.Now().UTC().Format("2006-01")
	assert.True(t, mr.Exists(key), "spend must live under the monthly window key")
	assert.False(t, mr.Exists("tenant:acme:spend:2099-01"), "a future period starts at 0")

	// An operator reset (DEL the window) zeroes the budget mid-cycle.
	mr.Del(key)
	spent, err = store.Spend(ctx, "acme")
	require.NoError(t, err)
	assert.InDelta(t, 0.0, spent, 0.001, "deleting the window key resets the budget")
}

func TestMoneyToFloat(t *testing.T) {
	assert.InDelta(t, 100.0, moneyToFloat("100.00"), 0.0001)
	assert.InDelta(t, 0.0035, moneyToFloat("0.0035"), 0.00001)
	assert.InDelta(t, 0.0, moneyToFloat(""), 0.0001, "unparseable → 0")
}
