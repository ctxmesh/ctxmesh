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
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/ctxmesh/agentry/internal/controlplane"
	"github.com/ctxmesh/agentry/internal/controlplane/toolregistry"
)

// NewRegistryPollSource builds the leader-elected Postgres poll source (ADR 0044
// §1) that emits ToolRegistry-change events onto ch — wired into the
// MCPToolBinding controller's RegistryChanges channel when the ToolRegistry CRD
// is retired (RETIRE_TR). Returned as a manager.Runnable for mgr.Add; the
// concrete type still satisfies LeaderElectionRunnable, so it stays pinned to the
// leader.
func NewRegistryPollSource(store toolregistry.Store, ch chan<- event.GenericEvent) manager.Runnable {
	return &registryPollSource{store: store, ch: ch}
}

// registryPollInterval is how often the Postgres poll source re-lists the
// ToolRegistry catalog to detect changes (ADR 0044 §1). Tuned for a small,
// low-churn catalog: fast enough that a binding re-validates within seconds of a
// catalog edit, cheap enough that a full list every tick is negligible.
const registryPollInterval = 15 * time.Second

// registrySnapshot is the minimal per-registry state the poll diff needs: the
// key (namespace/name) to build the event object, and the store's OCC Version as
// the change signal (bumped on every Upsert).
type registrySnapshot struct {
	namespace string
	name      string
	version   int64
}

// registryPollSource turns Postgres ToolRegistry changes into controller-runtime
// events (ADR 0044 §1), replacing the CRD watch once the ToolRegistry CRD is
// retired (M45). It re-lists the catalog every interval and, diffing against the
// previous snapshot by the store's OCC Version, emits a GenericEvent for every
// registry that was added, changed, or deleted onto ch — where the MCPToolBinding
// controller's source.Channel feeds them through the SAME mapRegistryToBindings
// map function the CRD watch used, fanning each out to the referencing bindings.
//
// A full re-list + diff (rather than an `updated_at > high-water` query) is a
// deliberate robustness choice: it detects DELETES — a deleted row bumps no
// timestamp — which the binding path needs to flip a stale binding to
// RegistryNotFound. The catalog is small (tens–low-hundreds of registries), so a
// full list every tick is cheap. It is pinned to the leader (one poller, never a
// herd) and drains cleanly on context cancellation.
type registryPollSource struct {
	store    toolregistry.Store
	ch       chan<- event.GenericEvent
	interval time.Duration
}

// NeedLeaderElection pins the poller to the leader — one replica emits the
// change stream, never a herd.
func (s *registryPollSource) NeedLeaderElection() bool { return true }

// Start runs the poll loop until the context is cancelled. prev starts empty, so
// the first tick treats every current registry as new and enqueues its bindings
// once — a one-time full re-validation that makes the poller self-sufficient
// (independent of the For() initial sync) and heals anything missed while
// leadership was vacant; the workqueue dedupes it against the initial sync.
func (s *registryPollSource) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("toolregistry-poll")
	interval := s.interval
	if interval <= 0 {
		interval = registryPollInterval
	}
	prev := map[string]registrySnapshot{}
	t := time.NewTicker(interval)
	defer t.Stop()
	log.Info("ToolRegistry Postgres poll source started (CRD watch replaced)", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			cur, err := s.snapshot(ctx)
			if err != nil {
				log.Error(err, "polling ToolRegistry catalog; will retry next tick")
				continue
			}
			if n := s.emitChanges(ctx, prev, cur); n > 0 {
				log.V(1).Info("enqueued referencing bindings for changed ToolRegistries", "registries", n)
			}
			prev = cur
		}
	}
}

// snapshot lists the whole catalog into a keyed map (namespace/name → snapshot).
// It pages to MaxPageSize so a catalog larger than one page is captured whole;
// the store's List returns Version per row, the diff's change signal.
//
// Above one page a concurrent write between pages can tear the offset (a
// duplicated or skipped row). That is harmless — the next tick re-lists and
// self-corrects — and moot at the ADR's tens–low-hundreds catalog size, where the
// whole catalog is a single List with no tear.
func (s *registryPollSource) snapshot(ctx context.Context) (map[string]registrySnapshot, error) {
	out := map[string]registrySnapshot{}
	token := ""
	for {
		page, err := s.store.List(ctx, controlplane.ListOptions{PageSize: controlplane.MaxPageSize, PageToken: token})
		if err != nil {
			return nil, fmt.Errorf("listing ToolRegistries for poll: %w", err)
		}
		for i := range page.Items {
			r := &page.Items[i]
			out[r.Namespace+"/"+r.Name] = registrySnapshot{namespace: r.Namespace, name: r.Name, version: r.Version}
		}
		if page.NextPage == "" {
			break
		}
		token = page.NextPage
	}
	return out, nil
}

// emitChanges sends a GenericEvent for every registry that was added, changed (a
// different OCC Version), or deleted between prev and cur, and returns how many
// it emitted. Each send races context cancellation so a shutdown mid-emit drains
// promptly instead of blocking on an undrained channel.
func (s *registryPollSource) emitChanges(ctx context.Context, prev, cur map[string]registrySnapshot) int {
	emitted := 0
	send := func(rs registrySnapshot) bool {
		// A lightweight client.Object carrying only namespace/name — ToolRegistry is
		// no longer a CRD/runtime.Object, and mapRegistryToBindings reads only the
		// key off it (GetNamespace/GetName).
		evt := event.GenericEvent{Object: &metav1.PartialObjectMetadata{
			ObjectMeta: metav1.ObjectMeta{Namespace: rs.namespace, Name: rs.name},
		}}
		select {
		case s.ch <- evt:
			emitted++
			return true
		case <-ctx.Done():
			return false
		}
	}
	// Added or changed: present now with a new-or-different Version.
	for k, c := range cur {
		if p, ok := prev[k]; ok && p.version == c.version {
			continue
		}
		if !send(c) {
			return emitted
		}
	}
	// Deleted: present before, gone now — the referencing bindings must flip to
	// RegistryNotFound, which no timestamp-based query would ever surface.
	for k, p := range prev {
		if _, ok := cur[k]; ok {
			continue
		}
		if !send(p) {
			return emitted
		}
	}
	return emitted
}
