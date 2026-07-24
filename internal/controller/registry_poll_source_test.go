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
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

// drainEvents non-blockingly reads every queued GenericEvent and returns each
// object's namespace/name key. The poll source emits a lightweight
// *PartialObjectMetadata carrying only the key (mapRegistryToBindings reads
// GetNamespace/GetName off it — ToolRegistry is no longer a CRD object).
func drainEvents(t *testing.T, ch chan event.GenericEvent) []string {
	t.Helper()
	var got []string
	for {
		select {
		case e := <-ch:
			if _, ok := e.Object.(*metav1.PartialObjectMetadata); !ok {
				t.Fatalf("event object type = %T, want *PartialObjectMetadata", e.Object)
			}
			got = append(got, e.Object.GetNamespace()+"/"+e.Object.GetName())
		default:
			slices.Sort(got)
			return got
		}
	}
}

func snap(ns, name string, v int64) registrySnapshot {
	return registrySnapshot{namespace: ns, name: name, version: v}
}

func assertKeys(t *testing.T, got, want []string) {
	t.Helper()
	slices.Sort(want)
	if len(got) != len(want) {
		t.Fatalf("emitted keys = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("emitted keys = %v, want %v", got, want)
		}
	}
}

func TestRegistryPollSource_emitChanges(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur map[string]registrySnapshot
		want      []string
	}{
		{
			name: "first poll (empty prev) enqueues every registry",
			prev: map[string]registrySnapshot{},
			cur: map[string]registrySnapshot{
				"default/a": snap("default", "a", 1),
				"team/b":    snap("team", "b", 1),
			},
			want: []string{"default/a", "team/b"},
		},
		{
			name: "unchanged version emits nothing",
			prev: map[string]registrySnapshot{"default/a": snap("default", "a", 3)},
			cur:  map[string]registrySnapshot{"default/a": snap("default", "a", 3)},
			want: nil,
		},
		{
			name: "version bump emits the change",
			prev: map[string]registrySnapshot{"default/a": snap("default", "a", 3)},
			cur:  map[string]registrySnapshot{"default/a": snap("default", "a", 4)},
			want: []string{"default/a"},
		},
		{
			name: "deleted registry emits (no timestamp would surface it)",
			prev: map[string]registrySnapshot{
				"default/a": snap("default", "a", 1),
				"default/b": snap("default", "b", 1),
			},
			cur:  map[string]registrySnapshot{"default/a": snap("default", "a", 1)},
			want: []string{"default/b"},
		},
		{
			name: "add + change + delete in one poll",
			prev: map[string]registrySnapshot{
				"ns/keep":   snap("ns", "keep", 2),
				"ns/change": snap("ns", "change", 1),
				"ns/gone":   snap("ns", "gone", 1),
			},
			cur: map[string]registrySnapshot{
				"ns/keep":   snap("ns", "keep", 2),
				"ns/change": snap("ns", "change", 2),
				"ns/new":    snap("ns", "new", 1),
			},
			want: []string{"ns/change", "ns/gone", "ns/new"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan event.GenericEvent, 16)
			s := &registryPollSource{ch: ch}
			n := s.emitChanges(context.Background(), tc.prev, tc.cur)
			if n != len(tc.want) {
				t.Errorf("emitChanges returned %d, want %d", n, len(tc.want))
			}
			assertKeys(t, drainEvents(t, ch), tc.want)
		})
	}
}

// TestRegistryPollSource_detectsStoreChangesAcrossPolls exercises snapshot +
// emitChanges against a real store across create/update/delete — the exact cycle
// Start runs, proving the poll turns store mutations into the right events.
func TestRegistryPollSource_detectsStoreChangesAcrossPolls(t *testing.T) {
	ctx := context.Background()
	store := toolregistry.NewMemStore()
	ch := make(chan event.GenericEvent, 16)
	s := &registryPollSource{store: store, ch: ch}

	// Poll 1: empty store, empty prev → nothing emitted.
	s1, err := s.snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot 1: %v", err)
	}
	s.emitChanges(ctx, map[string]registrySnapshot{}, s1)
	assertKeys(t, drainEvents(t, ch), nil)

	// Create → detected as an add.
	if _, err := store.Upsert(ctx, toolregistry.ToolRegistry{Namespace: "default", Name: "reg"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	s2, err := s.snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot 2: %v", err)
	}
	s.emitChanges(ctx, s1, s2)
	assertKeys(t, drainEvents(t, ch), []string{"default/reg"})

	// Update (Upsert bumps Version) → detected as a change.
	if _, err := store.Upsert(ctx, toolregistry.ToolRegistry{
		Namespace: "default", Name: "reg",
		Tools: []toolregistry.ToolEntry{{Name: "t1", Image: "img:1"}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	s3, err := s.snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot 3: %v", err)
	}
	if s3["default/reg"].version == s2["default/reg"].version {
		t.Fatalf("Upsert did not bump Version (%d) — the change signal is broken", s3["default/reg"].version)
	}
	s.emitChanges(ctx, s2, s3)
	assertKeys(t, drainEvents(t, ch), []string{"default/reg"})

	// Delete → detected as a removal.
	if err := store.Delete(ctx, "default", "reg"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	s4, err := s.snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot 4: %v", err)
	}
	s.emitChanges(ctx, s3, s4)
	assertKeys(t, drainEvents(t, ch), []string{"default/reg"})
}

// TestRegistryPollSource_StartEmitsOnStoreChange exercises the full Start loop
// with an injected short interval: a registry created after start is emitted on a
// subsequent tick, and Start returns nil (not an error) when the context is
// cancelled. This is the only test that drives the ticker/loop end to end.
func TestRegistryPollSource_StartEmitsOnStoreChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := toolregistry.NewMemStore()
	ch := make(chan event.GenericEvent, 16)
	s := &registryPollSource{store: store, ch: ch, interval: 5 * time.Millisecond}

	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	// Create after Start is running; whether the create lands before the first
	// tick (emitted as a first-poll add) or after (emitted as a diff), the event
	// must arrive.
	if _, err := store.Upsert(ctx, toolregistry.ToolRegistry{Namespace: "default", Name: "reg"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	select {
	case e := <-ch:
		if got := e.Object.GetNamespace() + "/" + e.Object.GetName(); got != "default/reg" {
			t.Fatalf("emitted %q, want default/reg", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not emit a change event within 2s")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return within 2s of cancel")
	}
}

// TestRegistryPollSource_emitChangesStopsOnCancel proves a cancelled context
// aborts the emit loop instead of blocking on an undrained channel.
func TestRegistryPollSource_emitChangesStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan event.GenericEvent) // unbuffered: a send would block without a drain
	s := &registryPollSource{ch: ch}
	n := s.emitChanges(ctx, map[string]registrySnapshot{}, map[string]registrySnapshot{
		"default/a": snap("default", "a", 1),
	})
	if n != 0 {
		t.Fatalf("emitted %d events after cancel, want 0 (should abort the send)", n)
	}
}
