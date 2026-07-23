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

package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// The control-plane sync reconcilers (ADR 0042 Amendment 4) are the authoritative
// write path for the Postgres read-switch: each watches a moved entity's CRD and
// drives the control-plane store to match (upsert on add/update, delete on
// NotFound). The informer's initial list-sync backfills every existing object;
// a leader-elected startup pass (storeOrphanPruner) deletes store rows whose CRD
// is gone (a delete missed while the reconciler was down). The best-effort BFF
// mirror stays as a fast path but is no longer load-bearing.

// syncHealthInterval requeues a successfully-synced object as a periodic
// self-heal — cheap belt-and-suspenders coverage for any write the event stream
// missed (the Upsert is idempotent last-write-wins).
const syncHealthInterval = 30 * time.Minute

const (
	// entityToolRegistry is the only syncing entity until ToolRegistry is retired (M45);
	// PromptVersion is already retired (ADR 0044) so no longer syncs. The metric keeps the
	// {entity} label for forward-compat + dashboard stability.
	entityToolRegistry = "tool_registry"

	syncResultOK    = "ok"
	syncResultError = "error"
)

// controlplaneSyncTotal counts sync-reconcile outcomes so the read-switch has
// operational visibility (is the projection converging?) before reads flip.
var controlplaneSyncTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "controlplane_sync_total",
		Help: "Control-plane store sync reconcile outcomes by entity and result (ADR 0042 Amendment 4).",
	},
	[]string{"entity", "result"},
)

func init() { metrics.Registry.MustRegister(controlplaneSyncTotal) }

//nolint:unparam // entity is always entityToolRegistry until a second entity syncs again (M45); the label is part of the metric contract.
func recordSync(entity, result string) { controlplaneSyncTotal.WithLabelValues(entity, result).Inc() }

// storeOrphanPruner is a leader-elected, one-shot startup Runnable that deletes
// control-plane store rows with no corresponding CRD (an orphan left when a CRD
// was deleted while the sync reconciler was down — the informer never replays
// that delete, and a re-list only upserts). It runs after the manager's cache is
// synced (leader-election Runnables start after cache sync), so the CRD read is
// consistent. prune collects orphans first, then deletes — offset pagination
// would skip rows if mutated mid-scan.
type storeOrphanPruner struct {
	prune func(ctx context.Context) error
}

func (p *storeOrphanPruner) Start(ctx context.Context) error { return p.prune(ctx) }

// NeedLeaderElection pins the prune to the leader — one replica reconciles the
// whole set, never a herd.
func (p *storeOrphanPruner) NeedLeaderElection() bool { return true }
