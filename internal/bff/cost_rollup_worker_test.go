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

package bff

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/costrollup"
)

// fakeRollupStore is an in-memory costrollup.Store for testing the worker without a real DB.
type fakeRollupStore struct {
	mu   sync.Mutex
	rows []costrollup.Rollup
}

func (f *fakeRollupStore) Upsert(_ context.Context, row costrollup.Rollup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent upsert: replace an existing row with the same key, or append a new one.
	for i, existing := range f.rows {
		if existing.ScopeType == row.ScopeType &&
			existing.ScopeID == row.ScopeID &&
			existing.Day.Equal(row.Day) {
			f.rows[i] = row
			return nil
		}
	}
	f.rows = append(f.rows, row)
	return nil
}

func (f *fakeRollupStore) Range(_ context.Context, scopeType, scopeID string, from, to time.Time) ([]costrollup.Rollup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []costrollup.Rollup
	fromDay := from.UTC().Truncate(24 * time.Hour)
	toDay := to.UTC().Truncate(24 * time.Hour)
	for _, r := range f.rows {
		if r.ScopeType == scopeType && r.ScopeID == scopeID {
			d := r.Day.UTC().Truncate(24 * time.Hour)
			if !d.Before(fromDay) && !d.After(toDay) {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func (f *fakeRollupStore) findByScope(scopeType, scopeID string) (costrollup.Rollup, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.ScopeType == scopeType && r.ScopeID == scopeID {
			return r, true
		}
	}
	return costrollup.Rollup{}, false
}

func (f *fakeRollupStore) allRows() []costrollup.Rollup {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]costrollup.Rollup, len(f.rows))
	copy(out, f.rows)
	return out
}

// newCostRollupServer builds a Server wired with the given rollupStore so rollupOnce can be driven
// deterministically in tests. The worker is tenant-scoped (Valkey only — ADR 0063 D1); no Langfuse.
func newCostRollupServer(t *testing.T, store costrollup.Store) *Server {
	t.Helper()
	return NewServer(Options{
		Auth:        AllowAll{},
		RollupStore: store,
		Version:     "test",
		Log:         logr.Discard(),
	})
}

// TestCostRollupWorker_TenantRows verifies that one tick upserts the expected tenant rows by reading
// the spend keys a miniredis seeds for the current month (matching the launcher's format), and ignores
// non-spend keys.
func TestCostRollupWorker_TenantRows(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	now := time.Date(2026, 8, 10, 15, 0, 0, 0, time.UTC)
	month := now.Format("2006-01")

	// Seed two tenant spend keys in the launcher's on-the-wire format:
	// "tenant:{id}:spend:{YYYY-MM}" → a float string (IncrByFloat).
	require.NoError(t, mr.Set(fmt.Sprintf("tenant:acme:spend:%s", month), "12.50"))
	require.NoError(t, mr.Set(fmt.Sprintf("tenant:beta:spend:%s", month), "0.75"))
	// A key that does NOT match the spend pattern — must be ignored.
	require.NoError(t, mr.Set("tenant:acme:rpm:12345", "88"))
	// A spend key for a DIFFERENT month — must be ignored (out of the current-month window).
	require.NoError(t, mr.Set("tenant:acme:spend:2025-01", "999.00"))

	store := &fakeRollupStore{}
	s := newCostRollupServer(t, store)

	cfg := CostRollupConfig{ValKeyAddr: mr.Addr()}
	s.rollupOnce(context.Background(), cfg, now)

	today := now.UTC().Truncate(24 * time.Hour)

	acme, ok := store.findByScope("tenant", "acme")
	require.True(t, ok, "tenant 'acme' row must be upserted")
	assert.Equal(t, "tenant", acme.ScopeType)
	assert.Equal(t, "acme", acme.ScopeID)
	assert.InDelta(t, 12.50, acme.SpendUSD, 1e-9, "acme spend decoded from the launcher's float key")
	assert.Equal(t, int64(0), acme.Tokens, "Valkey keys carry no token count; tokens default to 0")
	assert.Equal(t, today, acme.Day.UTC().Truncate(24*time.Hour), "Day must be today (UTC)")

	beta, ok := store.findByScope("tenant", "beta")
	require.True(t, ok, "tenant 'beta' row must be upserted")
	assert.InDelta(t, 0.75, beta.SpendUSD, 1e-9, "beta spend decoded correctly")

	// Exactly 2 tenant rows (acme + beta); the rpm key and the wrong-month key must be ignored.
	rows := store.allRows()
	assert.Len(t, rows, 2, "exactly 2 tenant rows; rpm + wrong-month keys must be ignored")
}

// TestCostRollupWorker_EmptyValKeyNoOp verifies that an empty ValKeyAddr is an honest no-op.
func TestCostRollupWorker_EmptyValKeyNoOp(t *testing.T) {
	t.Parallel()

	store := &fakeRollupStore{}
	s := newCostRollupServer(t, store)
	s.rollupOnce(context.Background(), CostRollupConfig{}, time.Now().UTC())
	assert.Empty(t, store.allRows(), "no Valkey addr → nothing snapshotted")
}

// TestCostRollupWorker_NilStoreNoPanic verifies that a nil rollupStore produces no panic and
// returns nil (an honest no-op), matching the online-scorer's nil-store behaviour.
func TestCostRollupWorker_NilStoreNoPanic(t *testing.T) {
	t.Parallel()

	s := newCostRollupServer(t, nil) // nil store
	require.NotPanics(t, func() {
		s.rollupOnce(context.Background(), CostRollupConfig{ValKeyAddr: "127.0.0.1:0"}, time.Now().UTC())
	})
}

// TestCostRollupWorker_UpsertIdempotent verifies that ticking twice for the same day with an updated
// spend produces exactly one row per tenant (the second tick overwrites, does not duplicate).
func TestCostRollupWorker_UpsertIdempotent(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	now := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	month := now.Format("2006-01")
	key := fmt.Sprintf("tenant:acme:spend:%s", month)

	require.NoError(t, mr.Set(key, "1.00"))
	store := &fakeRollupStore{}
	s := newCostRollupServer(t, store)
	cfg := CostRollupConfig{ValKeyAddr: mr.Addr()}

	s.rollupOnce(context.Background(), cfg, now)
	// The monthly cumulative grows; the second tick (same day) must overwrite the first.
	require.NoError(t, mr.Set(key, "2.50"))
	s.rollupOnce(context.Background(), cfg, now)

	row, ok := store.findByScope("tenant", "acme")
	require.True(t, ok)
	assert.InDelta(t, 2.50, row.SpendUSD, 1e-9, "second tick must overwrite the first (idempotent upsert)")
	assert.Len(t, store.allRows(), 1, "idempotent upsert must produce exactly 1 row")
}

// TestExtractTenantID verifies the spend-key parsing helper covers the format variants.
func TestExtractTenantID(t *testing.T) {
	t.Parallel()

	suffix := ":spend:2026-08"
	cases := []struct {
		key      string
		wantID   string
		wantDesc string
	}{
		{"tenant:acme:spend:2026-08", "acme", "plain tenant id"},
		{"tenant:beta-corp:spend:2026-08", "beta-corp", "hyphenated tenant id"},
		{"tenant::spend:2026-08", "", "empty id returns empty"},
		// A key whose total length equals len(prefix)+len(suffix) → no room for a tenant id.
		{"tenant::2026-08", "", "key too short (no room for tenant id segment)"},
	}
	for _, tc := range cases {
		got := extractTenantID(tc.key, suffix)
		assert.Equal(t, tc.wantID, got, tc.wantDesc)
	}
}

// TestExtractScopeID_AgentSlash verifies the per-agent scope-id parse is ROBUST to the '/' in the
// "{ns}/{name}" identity — the id is the exact segment between the "agent:" prefix and the ":spend:{month}"
// suffix, not a naive ':'-split (which the '/' does not break, but a fixed-length slice is what guarantees
// it). ':' cannot appear in a k8s ns/name, so ":spend:" is an unambiguous delimiter.
func TestExtractScopeID_AgentSlash(t *testing.T) {
	t.Parallel()

	suffix := ":spend:2026-08"
	cases := []struct {
		key      string
		wantID   string
		wantDesc string
	}{
		{"agent:default/foo:spend:2026-08", "default/foo", "ns/name identity with a slash"},
		{"agent:kube-system/my-agent:spend:2026-08", "kube-system/my-agent", "hyphenated ns and name"},
		{"agent:solo:spend:2026-08", "solo", "bare name (no namespace)"},
		{"agent::spend:2026-08", "", "empty id returns empty"},
		{"agent::2026-08", "", "key too short (no room for id segment)"},
	}
	for _, tc := range cases {
		got := extractScopeID(tc.key, agentSpendKeyPrefix, suffix)
		assert.Equal(t, tc.wantID, got, tc.wantDesc)
	}
}

// TestCostRollupWorker_AgentRows verifies that one tick upserts the expected per-agent rows by reading the
// agent spend keys a miniredis seeds for the current month (matching the launcher's format, including the
// '/' in the scope id), ignores non-spend + wrong-month keys, and coexists with the tenant snapshot.
func TestCostRollupWorker_AgentRows(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	now := time.Date(2026, 8, 10, 15, 0, 0, 0, time.UTC)
	month := now.Format("2006-01")

	// Seed per-agent spend keys in the launcher's on-the-wire format:
	// "agent:{ns}/{name}:spend:{YYYY-MM}" → a float string (IncrByFloat).
	require.NoError(t, mr.Set(fmt.Sprintf("agent:default/foo:spend:%s", month), "3.50"))
	require.NoError(t, mr.Set(fmt.Sprintf("agent:prod/billing-agent:spend:%s", month), "0.25"))
	// A tenant key coexists — the agent SCAN must not pick it up, and the tenant SCAN must not pick agents up.
	require.NoError(t, mr.Set(fmt.Sprintf("tenant:acme:spend:%s", month), "9.00"))
	// A wrong-month agent key — must be ignored (out of the current-month window).
	require.NoError(t, mr.Set("agent:default/foo:spend:2025-01", "999.00"))

	store := &fakeRollupStore{}
	s := newCostRollupServer(t, store)

	cfg := CostRollupConfig{ValKeyAddr: mr.Addr()}
	s.rollupOnce(context.Background(), cfg, now)

	today := now.UTC().Truncate(24 * time.Hour)

	foo, ok := store.findByScope("agent", "default/foo")
	require.True(t, ok, "agent 'default/foo' row must be upserted")
	assert.Equal(t, "agent", foo.ScopeType)
	assert.Equal(t, "default/foo", foo.ScopeID, "scope id preserves the '/' in {ns}/{name}")
	assert.InDelta(t, 3.50, foo.SpendUSD, 1e-9, "foo spend decoded from the launcher's float key")
	assert.Equal(t, int64(0), foo.Tokens, "Valkey keys carry no token count; tokens default to 0")
	assert.Equal(t, today, foo.Day.UTC().Truncate(24*time.Hour), "Day must be today (UTC)")

	billing, ok := store.findByScope("agent", "prod/billing-agent")
	require.True(t, ok, "agent 'prod/billing-agent' row must be upserted")
	assert.InDelta(t, 0.25, billing.SpendUSD, 1e-9, "billing-agent spend decoded correctly")

	// The tenant key is still snapshotted as a tenant row (both scopes in one tick).
	acme, ok := store.findByScope("tenant", "acme")
	require.True(t, ok, "tenant 'acme' row must still be upserted alongside the agents")
	assert.InDelta(t, 9.00, acme.SpendUSD, 1e-9)

	// Exactly 3 rows: 2 agents + 1 tenant; the wrong-month agent key must be ignored.
	rows := store.allRows()
	assert.Len(t, rows, 3, "2 agent rows + 1 tenant row; the wrong-month key must be ignored")
}

// TestCostRollupWorker_AgentRows_RealPostgres is the tier1 round-trip: seed a per-agent Valkey spend key,
// run one tick against the REAL Postgres cost-rollup store (through the full controlplane.OpenDB migration
// chain), and read the durable {scope_type:"agent", scope_id:"ns/name", day, spend} row back via
// Range(...,"agent","ns/name",...). Gated on CONTROLPLANE_TEST_DSN — skipped in CI without a DB.
func TestCostRollupWorker_AgentRows_RealPostgres(t *testing.T) {
	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTROLPLANE_TEST_DSN unset — skipping real-Postgres per-agent rollup round-trip")
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE cost_rollups`)
	require.NoError(t, err)
	store := costrollup.NewPostgresStore(db)

	mr := miniredis.RunT(t)
	now := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	month := now.Format("2006-01")
	// A per-agent key with a '/' in the scope id, in the launcher's on-the-wire format.
	require.NoError(t, mr.Set(fmt.Sprintf("agent:default/foo:spend:%s", month), "4.20"))

	s := newCostRollupServer(t, store)
	s.rollupOnce(context.Background(), CostRollupConfig{ValKeyAddr: mr.Addr()}, now)

	today := now.UTC().Truncate(24 * time.Hour)
	rows, err := store.Range(context.Background(), "agent", "default/foo", today, today)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the worker must have written exactly one durable per-agent row")
	assert.Equal(t, "agent", rows[0].ScopeType)
	assert.Equal(t, "default/foo", rows[0].ScopeID, "scope id preserves the '/' through the durable store")
	assert.InDelta(t, 4.20, rows[0].SpendUSD, 1e-9)
	assert.Equal(t, today, rows[0].Day.UTC().Truncate(24*time.Hour))
}

// TestCostRollupWorker_AgentUpsertIdempotent verifies ticking twice for the same day with an updated
// per-agent spend produces exactly one row per agent (the second tick overwrites, does not duplicate).
func TestCostRollupWorker_AgentUpsertIdempotent(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	now := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	month := now.Format("2006-01")
	key := fmt.Sprintf("agent:default/foo:spend:%s", month)

	require.NoError(t, mr.Set(key, "1.00"))
	store := &fakeRollupStore{}
	s := newCostRollupServer(t, store)
	cfg := CostRollupConfig{ValKeyAddr: mr.Addr()}

	s.rollupOnce(context.Background(), cfg, now)
	require.NoError(t, mr.Set(key, "2.50")) // MTD grows; the second tick (same day) must overwrite the first.
	s.rollupOnce(context.Background(), cfg, now)

	row, ok := store.findByScope("agent", "default/foo")
	require.True(t, ok)
	assert.InDelta(t, 2.50, row.SpendUSD, 1e-9, "second tick must overwrite the first (idempotent upsert)")
	assert.Len(t, store.allRows(), 1, "idempotent upsert must produce exactly 1 agent row")
}
