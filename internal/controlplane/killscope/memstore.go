package killscope

import (
	"context"
	"slices"
	"strings"
	"sync"
)

// memStore is the in-memory twin, used by the zero-dependency (no-Postgres) install and by tests. It is
// a real implementation, not a stub: since ADR 0125 the worker pool runs on the mem store too, so the
// kill switch must behave identically there. The durability of the kill then matches the durability of
// the rest of that install — which is the honest guarantee, not a weaker one hidden behind an interface.
type memStore struct {
	mu    sync.RWMutex
	kills map[string]Kill
}

// NewMemStore returns an in-memory killscope Store.
func NewMemStore() Store { return &memStore{kills: map[string]Kill{}} }

func (m *memStore) Kill(_ context.Context, k Kill) error {
	if err := k.Scope.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kills[k.Scope.Key()] = k // idempotent per scope; a re-kill refreshes reason/principal
	return nil
}

func (m *memStore) Unkill(_ context.Context, s Scope) (bool, error) {
	if err := s.Validate(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := s.Key()
	if _, ok := m.kills[key]; !ok {
		return false, nil
	}
	delete(m.kills, key)
	return true, nil
}

func (m *memStore) Active(_ context.Context) ([]Kill, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Kill, 0, len(m.kills))
	for _, k := range m.kills {
		out = append(out, k)
	}
	// Deterministic order so a caller's expansion (and any test) is stable.
	slices.SortFunc(out, func(a, b Kill) int { return strings.Compare(a.Scope.Key(), b.Scope.Key()) })
	return out, nil
}
