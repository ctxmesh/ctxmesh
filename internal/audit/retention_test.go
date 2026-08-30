package audit

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/controlplane/auditlog"
)

// The retention pruner (m63.6, ADR 0056 §5): a leader-elected Runnable that deletes audit_log rows older
// than now()-retention. Tests use the memstore twin (the pgstore passes the same PruneBefore conformance).

func TestRetentionPruner_DropsOldKeepsRecent(t *testing.T) {
	ctx := context.Background()
	store := auditlog.NewMemStore()
	now := time.Now().UTC()
	require.NoError(t, store.Append(ctx, auditlog.Entry{Actor: "old", OccurredAt: now.Add(-100 * 24 * time.Hour)}))
	require.NoError(t, store.Append(ctx, auditlog.Entry{Actor: "recent", OccurredAt: now.Add(-1 * 24 * time.Hour)}))

	before := testutil.ToFloat64(auditPrunedTotal)
	p := NewRetentionPruner(store, 90*24*time.Hour, logr.Discard())
	p.prune(ctx)

	page, err := store.List(ctx, auditlog.Query{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1, "the 100-day-old row is pruned; the 1-day-old row is kept")
	assert.Equal(t, "recent", page.Items[0].Actor)
	assert.InDelta(t, 1.0, testutil.ToFloat64(auditPrunedTotal)-before, 0.001,
		"exactly one pruned row is counted in the Prometheus counter")
}

func TestRetentionPruner_IsLeaderElected(t *testing.T) {
	p := NewRetentionPruner(auditlog.NewMemStore(), time.Hour, logr.Discard())
	assert.True(t, p.NeedLeaderElection(), "the pruner deletes — exactly one leader runs it, never a herd")
}

func TestRetentionPruner_DefaultsNonPositiveRetention(t *testing.T) {
	assert.Equal(t, DefaultRetention, NewRetentionPruner(auditlog.NewMemStore(), 0, logr.Discard()).retention)
	assert.Equal(t, DefaultRetention, NewRetentionPruner(auditlog.NewMemStore(), -5*time.Hour, logr.Discard()).retention)
}

func TestRetentionPruner_StartPrunesImmediatelyThenStopsOnCancel(t *testing.T) {
	store := auditlog.NewMemStore()
	now := time.Now().UTC()
	require.NoError(t, store.Append(context.Background(),
		auditlog.Entry{Actor: "old", OccurredAt: now.Add(-100 * 24 * time.Hour)}))

	p := NewRetentionPruner(store, 90*24*time.Hour, logr.Discard())
	p.interval = time.Hour // long — we assert the STARTUP prune, not a tick

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- p.Start(ctx) }()

	// Start prunes once immediately (heals a freshly-elected leader) — the old row vanishes without
	// waiting a full interval.
	require.Eventually(t, func() bool {
		page, _ := store.List(context.Background(), auditlog.Query{})
		return len(page.Items) == 0
	}, 2*time.Second, 10*time.Millisecond, "the startup prune enforces the window without waiting an interval")

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err, "Start returns cleanly on context cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return on context cancel")
	}
}
