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

package costrollup

import (
	"context"
	"slices"
	"sync"
	"time"
)

// memStore is an in-memory Store for unit tests. It is NOT safe for production
// use (no persistence; lost on restart). Mirrors the alertstore.memStore pattern.
type memStore struct {
	mu   sync.Mutex
	rows map[string]Rollup // keyed by "scopeType|scopeID|day"
}

// NewMemStore returns an in-memory Store (thread-safe; data is lost on process exit).
// Use in unit tests and as a dev twin.
func NewMemStore() Store {
	return &memStore{rows: make(map[string]Rollup)}
}

func rowKey(scopeType, scopeID string, day time.Time) string {
	return scopeType + "|" + scopeID + "|" + day.UTC().Truncate(24*time.Hour).Format("2006-01-02")
}

func (s *memStore) Upsert(_ context.Context, row Rollup) error {
	day := row.Day.UTC().Truncate(24 * time.Hour)
	row.Day = day
	row.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[rowKey(row.ScopeType, row.ScopeID, day)] = row
	return nil
}

func (s *memStore) Range(_ context.Context, scopeType, scopeID string, from, to time.Time) ([]Rollup, error) {
	fromDay := from.UTC().Truncate(24 * time.Hour)
	toDay := to.UTC().Truncate(24 * time.Hour)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Rollup, 0)
	for _, r := range s.rows {
		if r.ScopeType != scopeType || r.ScopeID != scopeID {
			continue
		}
		d := r.Day.UTC()
		if (d.Equal(fromDay) || d.After(fromDay)) && (d.Equal(toDay) || d.Before(toDay)) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Rollup) int { return a.Day.Compare(b.Day) })
	return out, nil
}
