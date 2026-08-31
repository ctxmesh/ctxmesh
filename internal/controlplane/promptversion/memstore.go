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

package promptversion

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
)

// memStore is the in-memory twin of the Postgres store — used in unit tests + the cross-impl conformance
// suite (both must behave identically), so BFF/handler tests need no live DB. Not for production.
type memStore struct {
	mu   sync.Mutex
	rows map[string]PromptVersion // key: namespace + "\x00" + name
	now  func() time.Time
}

// NewMemStore returns an in-memory Store.
func NewMemStore() Store {
	return &memStore{rows: map[string]PromptVersion{}, now: time.Now}
}

func memKey(ns, name string) string { return ns + "\x00" + name }

func (m *memStore) Get(_ context.Context, ns, name string) (*PromptVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pv, ok := m.rows[memKey(ns, name)]
	if !ok {
		return nil, controlplane.ErrNotFound
	}
	out := pv
	return &out, nil
}

func (m *memStore) Create(_ context.Context, pv PromptVersion) (*PromptVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(pv.Namespace, pv.Name)
	if _, ok := m.rows[k]; ok {
		return nil, controlplane.ErrConflict
	}
	now := m.now()
	pv.Version = 1
	pv.CreatedAt = now
	pv.UpdatedAt = now
	pv.Labels = cloneLabels(pv.Labels)
	m.rows[k] = pv
	out := pv
	return &out, nil
}

func (m *memStore) Upsert(_ context.Context, pv PromptVersion) (*PromptVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	k := memKey(pv.Namespace, pv.Name)
	if prev, ok := m.rows[k]; ok {
		pv.Version = prev.Version + 1
		pv.CreatedAt = prev.CreatedAt
	} else {
		pv.Version = 1
		pv.CreatedAt = now
	}
	pv.UpdatedAt = now
	pv.Labels = cloneLabels(pv.Labels)
	m.rows[k] = pv
	out := pv
	return &out, nil
}

func (m *memStore) Delete(_ context.Context, ns, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, memKey(ns, name))
	return nil
}

func (m *memStore) List(_ context.Context, opts controlplane.ListOptions) (controlplane.Page[PromptVersion], error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	matched := make([]PromptVersion, 0, len(m.rows))
	for _, pv := range m.rows {
		if opts.Namespace != "" && pv.Namespace != opts.Namespace {
			continue
		}
		if s := strings.TrimSpace(opts.Search); s != "" && !strings.Contains(strings.ToLower(pv.Name), strings.ToLower(s)) {
			continue
		}
		if !labelsMatch(pv.Labels, opts.Labels) {
			continue
		}
		matched = append(matched, pv)
	}

	sortPromptVersions(matched, opts.SortBy, opts.SortDesc)

	total := int64(len(matched))
	offset, limit := opts.Offset(), opts.Limit()
	offset = min(offset, len(matched))
	end := min(offset+limit, len(matched))
	page := make([]PromptVersion, end-offset)
	for i := range page {
		page[i] = matched[offset+i]
		page[i].Labels = cloneLabels(page[i].Labels)
	}
	return controlplane.Page[PromptVersion]{
		Items:    page,
		Total:    total,
		NextPage: controlplane.NextToken(offset, limit, total),
	}, nil
}

// sortPromptVersions orders by the requested column (default: namespace then name), stable + total so
// pagination is deterministic — matching the SQL store's ORDER BY (which appends namespace,name as the
// tiebreak).
func sortPromptVersions(rows []PromptVersion, sortBy string, desc bool) {
	slices.SortStableFunc(rows, func(a, b PromptVersion) int {
		c := comparePromptVersions(a, b, sortBy)
		if desc {
			return -c
		}
		return c
	})
}

func comparePromptVersions(a, b PromptVersion, sortBy string) int {
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

func cloneLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}
