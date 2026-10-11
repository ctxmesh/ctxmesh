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

package oauthpending

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// memStore is the in-memory twin for tests and a BFF built without the control-plane store. It
// seals like the Postgres store so both pass one conformance suite; it is shared only within one
// process, so it cannot serve more than one replica.
type memStore struct {
	mu      sync.Mutex
	records map[string]memRecord
}

type memRecord struct {
	kind      Kind
	sealed    []byte
	expiresAt time.Time
}

// NewMemStore returns an in-memory Store.
func NewMemStore() Store { return &memStore{records: map[string]memRecord{}} }

func (m *memStore) Put(_ context.Context, kind Kind, handle string, payload []byte, expiresAt time.Time) error {
	id, sealed, err := seal(kind, handle, payload)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.records[id]; exists {
		return fmt.Errorf("oauthpending: duplicate %s record", kind)
	}
	m.records[id] = memRecord{kind: kind, sealed: sealed, expiresAt: expiresAt}
	return nil
}

func (m *memStore) Take(_ context.Context, kind Kind, handle string, now time.Time) ([]byte, bool, error) {
	if err := checkArgs(kind, handle); err != nil {
		return nil, false, err
	}
	id := recordID(kind, handle)
	m.mu.Lock()
	rec, ok := m.records[id]
	delete(m.records, id)
	m.mu.Unlock()
	if !ok || rec.kind != kind || !now.Before(rec.expiresAt) {
		return nil, false, nil
	}
	payload, err := open(kind, handle, id, rec.sealed)
	if err != nil {
		return nil, false, err
	}
	return payload, true, nil
}

func (m *memStore) DeleteExpired(_ context.Context, now time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, rec := range m.records {
		if !now.Before(rec.expiresAt) {
			delete(m.records, id)
			n++
		}
	}
	return n, nil
}
