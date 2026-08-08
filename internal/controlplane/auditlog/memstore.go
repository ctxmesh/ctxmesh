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

package auditlog

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
)

// memStore is the in-memory twin of pgStore — the offline/test store that passes the same conformance
// suite. It reproduces the store contract: idempotent Append on DedupKey, newest-first keyset paging.
type memStore struct {
	mu     sync.Mutex
	rows   []Entry
	byKey  map[string]struct{} // dedup
	nextID int64
}

// NewMemStore returns an in-memory Store (tests / offline).
func NewMemStore() Store {
	return &memStore{byKey: map[string]struct{}{}}
}

func (s *memStore) Append(_ context.Context, e Entry) error {
	e = normalize(e)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.byKey[e.DedupKey]; dup {
		return nil // ON CONFLICT DO NOTHING
	}
	s.nextID++
	e.ID = s.nextID
	s.byKey[e.DedupKey] = struct{}{}
	s.rows = append(s.rows, e)
	return nil
}

func (s *memStore) List(_ context.Context, q Query) (Page, error) {
	cur, err := decodeCursor(q.Cursor)
	if err != nil {
		return Page{}, err
	}
	size := clampPageSize(q.PageSize)

	s.mu.Lock()
	matched := make([]Entry, 0, len(s.rows))
	for _, e := range s.rows {
		if !matches(e, q) {
			continue
		}
		if q.Cursor != "" && !older(e, cur) {
			continue // keyset: only rows strictly older than the cursor
		}
		matched = append(matched, e)
	}
	s.mu.Unlock()

	// Newest first: occurred_at DESC, id DESC.
	slices.SortFunc(matched, func(a, b Entry) int {
		if !a.OccurredAt.Equal(b.OccurredAt) {
			return b.OccurredAt.Compare(a.OccurredAt) // DESC
		}
		return cmp.Compare(b.ID, a.ID) // id DESC
	})

	page := Page{Items: []Entry{}}
	if len(matched) > size {
		last := matched[size-1]
		page.NextCursor = encodeCursor(cursor{TS: last.OccurredAt, ID: last.ID})
		matched = matched[:size]
	}
	page.Items = append(page.Items, matched...)
	return page, nil
}

func (s *memStore) PruneBefore(_ context.Context, cutoff time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.rows[:0:0]
	var pruned int64
	for _, e := range s.rows {
		if e.OccurredAt.Before(cutoff) {
			delete(s.byKey, e.DedupKey)
			pruned++
			continue
		}
		kept = append(kept, e)
	}
	s.rows = kept
	return pruned, nil
}

// matches applies the AND-ed exact filters.
func matches(e Entry, q Query) bool {
	if q.Namespace != "" && e.Namespace != q.Namespace {
		return false
	}
	if q.Actor != "" && e.Actor != q.Actor {
		return false
	}
	if q.Action != "" && e.Action != q.Action {
		return false
	}
	if q.ResourceKind != "" && e.ResourceKind != q.ResourceKind {
		return false
	}
	return true
}

// older reports whether e sorts strictly after the cursor position under (occurred_at DESC, id DESC) —
// i.e. e belongs on a later (older) page.
func older(e Entry, c cursor) bool {
	if !e.OccurredAt.Equal(c.TS) {
		return e.OccurredAt.Before(c.TS)
	}
	return e.ID < c.ID
}
