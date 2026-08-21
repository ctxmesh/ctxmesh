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

package sharedrun

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// memStore is the in-memory implementation of Store — the handler-unit-test twin and a development store.
// It mirrors the pgStore semantics exactly: Create stores hash-only, GetByTokenHash returns the raw row
// (revoked/expired included — the caller decides via IsLive), Revoke is idempotent, ListForRun returns
// ALL rows (including revoked) newest-first (V11: honest "what did I expose?" list). Data is lost when
// the process exits (so a share into this store is not durable — the BFF refuses to mint against a
// non-durable run store, ADR 0069 §1).
type memStore struct {
	mu   sync.Mutex
	data map[string]SharedRun // id → record
}

// NewMemStore returns an in-memory Store (thread-safe; data is lost when the process exits).
func NewMemStore() Store {
	return &memStore{data: make(map[string]SharedRun)}
}

func (s *memStore) Create(_ context.Context, rec SharedRun) error {
	if rec.ID == "" || rec.TokenHash == "" || rec.RunID == "" {
		return fmt.Errorf("sharedrun: id, token_hash, and run_id are required")
	}
	if rec.ExpiresAt.IsZero() {
		return fmt.Errorf("sharedrun: expires_at is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.data[rec.ID]; ok {
		return fmt.Errorf("sharedrun: share id %s already exists", rec.ID)
	}
	// Enforce the token_hash UNIQUE constraint the pgStore gets from the schema.
	for _, existing := range s.data {
		if existing.TokenHash == rec.TokenHash {
			return fmt.Errorf("sharedrun: token_hash already exists")
		}
	}
	stored := rec
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now().UTC()
	} else {
		stored.CreatedAt = stored.CreatedAt.UTC()
	}
	stored.ExpiresAt = stored.ExpiresAt.UTC()
	stored.Revoked = false
	s.data[rec.ID] = stored
	return nil
}

func (s *memStore) GetByTokenHash(_ context.Context, tokenHash string) (*SharedRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.data {
		if rec.TokenHash == tokenHash {
			out := rec // defensive copy
			return &out, true, nil
		}
	}
	return nil, false, nil
}

func (s *memStore) Revoke(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.data[id]; ok {
		rec.Revoked = true
		s.data[id] = rec
	}
	return nil // idempotent: absent id is a no-op success
}

func (s *memStore) ListForRun(_ context.Context, runID string) ([]SharedRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SharedRun
	for _, rec := range s.data {
		// V11: include revoked rows so the UI can badge them ("what did I expose?").
		if rec.RunID == runID {
			out = append(out, rec)
		}
	}
	// Newest-first, mirroring the pgStore ORDER BY created_at DESC.
	slices.SortFunc(out, func(a, b SharedRun) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

func (s *memStore) ListByCreator(_ context.Context, createdBy string) ([]SharedRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SharedRun
	for _, rec := range s.data {
		// V13: the caller's shares across ALL runs, revoked/expired included (the UI badges status).
		if rec.CreatedBy == createdBy {
			out = append(out, rec)
		}
	}
	// Newest-first, mirroring the pgStore ORDER BY created_at DESC.
	slices.SortFunc(out, func(a, b SharedRun) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}
