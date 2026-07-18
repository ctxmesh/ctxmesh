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

package credpostgres

import (
	"context"
	"sync"
	"time"
)

// memStore is an in-memory Storage with the same optimistic-concurrency + sweep semantics
// as sqlStore. It exists for tests and the cross-package conformance suite so the Postgres
// Backend can be exercised WITHOUT a live database; production always uses NewStore.
type memStore struct {
	mu   sync.Mutex
	rows map[string]stored
}

// NewMemStore returns an in-memory Storage (for tests / conformance, not production).
func NewMemStore() Storage {
	return &memStore{rows: map[string]stored{}}
}

func memKey(ns, server, userHash string) string { return ns + "|" + server + "|" + userHash }

func (m *memStore) load(_ context.Context, ns, server, userHash string) (stored, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.rows[memKey(ns, server, userHash)]
	return st, ok, nil
}

func (m *memStore) save(_ context.Context, ns, server, userHash string, st stored, expectedVersion int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(ns, server, userHash)
	cur, exists := m.rows[k]
	if expectedVersion == 0 {
		if exists {
			return errConflict
		}
		st.version = 1
		m.rows[k] = st
		return nil
	}
	if !exists || cur.version != expectedVersion {
		return errConflict
	}
	st.version = expectedVersion + 1
	m.rows[k] = st
	return nil
}

func (m *memStore) del(_ context.Context, ns, server, userHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, memKey(ns, server, userHash))
	return nil
}

func (m *memStore) sweepExpired(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for k, st := range m.rows {
		if !st.expiresAt.IsZero() && st.expiresAt.Before(before) {
			delete(m.rows, k)
			n++
		}
	}
	return n, nil
}
