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

package alertstore

import (
	"context"
	"sort"
	"sync"
	"time"
)

// memStore is an in-memory Store for unit tests. It mimics the Postgres semantics:
// newest-first ordering, namespace filter, limit capping, and best-effort Resolve.
type memStore struct {
	mu     sync.Mutex
	rows   []Alert
	nextID int64
}

// NewMemStore returns an in-memory Store for unit testing. It is NOT safe for use in
// production (no persistence, no schema). Tests use it instead of a real Postgres.
func NewMemStore() Store {
	return &memStore{nextID: 1}
}

func (m *memStore) Append(_ context.Context, a Alert) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID
	m.nextID++
	if a.FiredAt.IsZero() {
		a.FiredAt = time.Now().UTC()
	}
	a.ID = id
	m.rows = append(m.rows, a)
	return id, nil
}

func (m *memStore) List(_ context.Context, namespace string, limit int) ([]Alert, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	limit = clampLimit(limit)

	// Collect matching rows.
	var matching []Alert
	for _, r := range m.rows {
		if r.Namespace == namespace {
			matching = append(matching, r)
		}
	}

	// Sort newest-first (by FiredAt DESC, ID DESC — mirrors the SQL ORDER BY).
	sort.Slice(matching, func(i, j int) bool {
		ti, tj := matching[i].FiredAt, matching[j].FiredAt
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return matching[i].ID > matching[j].ID
	})

	if len(matching) > limit {
		matching = matching[:limit]
	}
	return matching, nil
}

func (m *memStore) Resolve(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].ID == id && m.rows[i].ResolvedAt == nil {
			t := time.Now().UTC()
			m.rows[i].ResolvedAt = &t
			break
		}
	}
	return nil
}
