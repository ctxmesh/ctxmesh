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

package toolregistry

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// memStore is the in-memory twin of the Postgres store — the conformance suite runs both. Not for prod.
type memStore struct {
	mu   sync.Mutex
	rows map[string]ToolRegistry // key: namespace + "\x00" + name
	now  func() time.Time
}

// NewMemStore returns an in-memory Store.
func NewMemStore() Store {
	return &memStore{rows: map[string]ToolRegistry{}, now: time.Now}
}

func memKey(ns, name string) string { return ns + "\x00" + name }

func (m *memStore) Get(_ context.Context, ns, name string) (*ToolRegistry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tr, ok := m.rows[memKey(ns, name)]
	if !ok {
		return nil, controlplane.ErrNotFound
	}
	out := cloneRegistry(tr)
	return &out, nil
}

func (m *memStore) Upsert(_ context.Context, tr ToolRegistry) (*ToolRegistry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	k := memKey(tr.Namespace, tr.Name)
	if prev, ok := m.rows[k]; ok {
		tr.Version = prev.Version + 1
		tr.CreatedAt = prev.CreatedAt
	} else {
		tr.Version = 1
		tr.CreatedAt = now
	}
	tr.UpdatedAt = now
	stored := cloneRegistry(tr)
	m.rows[k] = stored
	out := cloneRegistry(stored)
	return &out, nil
}

func (m *memStore) Delete(_ context.Context, ns, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, memKey(ns, name))
	return nil
}

func (m *memStore) List(_ context.Context, opts controlplane.ListOptions) (controlplane.Page[ToolRegistry], error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	matched := make([]ToolRegistry, 0, len(m.rows))
	for _, tr := range m.rows {
		if opts.Namespace != "" && tr.Namespace != opts.Namespace {
			continue
		}
		if s := strings.TrimSpace(opts.Search); s != "" && !strings.Contains(strings.ToLower(tr.Name), strings.ToLower(s)) {
			continue
		}
		if !labelsMatch(tr.Labels, opts.Labels) {
			continue
		}
		matched = append(matched, tr)
	}
	sortRegistries(matched, opts.SortBy, opts.SortDesc)

	total := int64(len(matched))
	offset, limit := opts.Offset(), opts.Limit()
	offset = min(offset, len(matched))
	end := min(offset+limit, len(matched))
	page := make([]ToolRegistry, 0, end-offset)
	for i := offset; i < end; i++ {
		page = append(page, cloneRegistry(matched[i]))
	}
	return controlplane.Page[ToolRegistry]{
		Items:    page,
		Total:    total,
		NextPage: controlplane.NextToken(offset, limit, total),
	}, nil
}

func sortRegistries(rows []ToolRegistry, sortBy string, desc bool) {
	slices.SortStableFunc(rows, func(a, b ToolRegistry) int {
		c := compareRegistries(a, b, sortBy)
		if desc {
			return -c
		}
		return c
	})
}

func compareRegistries(a, b ToolRegistry, sortBy string) int {
	switch sortBy {
	case sortByCreatedAt:
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
	case sortByUpdatedAt:
		if c := a.UpdatedAt.Compare(b.UpdatedAt); c != 0 {
			return c
		}
	}
	return strings.Compare(a.Namespace+"\x00"+a.Name, b.Namespace+"\x00"+b.Name)
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// cloneRegistry deep-copies so callers can't mutate stored state; normalizes empty maps/slices to nil for
// parity with the Postgres store (which round-trips {}/[] to nil on scan).
func cloneRegistry(in ToolRegistry) ToolRegistry {
	out := in
	out.Tools = cloneTools(in.Tools)
	out.Annotations = cloneStrMap(in.Annotations)
	out.Labels = cloneStrMap(in.Labels)
	return out
}

func cloneTools(in []ToolEntry) []ToolEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]ToolEntry, len(in))
	for i, e := range in {
		out[i] = e
		if len(e.InputSchema) > 0 {
			out[i].InputSchema = slices.Clone(e.InputSchema)
		} else {
			out[i].InputSchema = nil
		}
	}
	return out
}

func cloneStrMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}
