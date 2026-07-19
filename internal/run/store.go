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

package run

import (
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned when a run id is unknown to the store.
var ErrNotFound = errors.New("run: not found")

// Store persists runs for the execution contract (ADR 0034). Phase 1 (M31) is a HOT store —
// in-process, NOT durable across a pod restart; M32 replaces the backing with a durable store
// behind this same seam (durable run state + a worker execution path). The interface is written
// so a durable implementation slots in without touching callers.
type Store interface {
	// Create stores a new run. It errors if the id already exists (ids are unique).
	Create(r *Run) error
	// Get returns a COPY of the run (callers must not mutate the store's object directly).
	Get(id string) (*Run, error)
	// Update applies fn to the stored run atomically and returns a copy of the result. fn runs
	// under the store lock; a non-nil error from fn (e.g. an illegal Transition) aborts the
	// update and leaves the run unchanged.
	Update(id string, fn func(*Run) error) (*Run, error)
	// List returns copies of all runs (unordered) — for a caller-scoped runs browser.
	List() []*Run
}

// memStore is the hot in-memory Store. It is safe for concurrent use.
type memStore struct {
	mu   sync.RWMutex
	now  func() time.Time
	runs map[string]*Run
}

// NewMemStore returns a hot in-memory run store.
func NewMemStore() Store {
	return &memStore{now: time.Now, runs: map[string]*Run{}}
}

func (m *memStore) Create(r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.ID]; ok {
		return errors.New("run: id already exists")
	}
	m.runs[r.ID] = cloneRun(r)
	return nil
}

func (m *memStore) Get(id string) (*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneRun(r), nil
}

func (m *memStore) Update(id string, fn func(*Run) error) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return nil, ErrNotFound
	}
	// Apply fn to a working copy so a mid-mutation error can't leave a partial write.
	working := cloneRun(r)
	if err := fn(working); err != nil {
		return nil, err
	}
	m.runs[id] = working
	return cloneRun(working), nil
}

func (m *memStore) List() []*Run {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		out = append(out, cloneRun(r))
	}
	return out
}

// cloneRun returns a deep-enough copy so a returned run can be read/mutated by a caller without
// racing the store's copy (the slices + the Action pointer are copied, not aliased).
func cloneRun(r *Run) *Run {
	c := *r
	if r.Messages != nil {
		c.Messages = append([]Message(nil), r.Messages...)
	}
	if r.Input != nil {
		c.Input = append([]byte(nil), r.Input...)
	}
	if r.RequiresAction != nil {
		a := *r.RequiresAction
		if r.RequiresAction.Servers != nil {
			a.Servers = append([]string(nil), r.RequiresAction.Servers...)
		}
		c.RequiresAction = &a
	}
	return &c
}
