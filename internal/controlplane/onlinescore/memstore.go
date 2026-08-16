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

package onlinescore

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// memKey builds the map key for (namespace, agentName, agentVersion, windowStart truncated to hour).
func memKey(namespace, agentName, agentVersion string, windowStart time.Time) string {
	return namespace + "\x00" + agentName + "\x00" + agentVersion + "\x00" + windowStart.Truncate(time.Hour).Format(time.RFC3339)
}

// configKey builds the map key for a per-(namespace, agentName) online-scoring config row.
func configKey(namespace, agentName string) string {
	return namespace + "\x00" + agentName
}

// memStore is the in-memory implementation of Store, used in tests and as a development twin.
type memStore struct {
	mu     sync.RWMutex
	data   map[string]Aggregate
	config map[string]OnlineConfig
}

// NewMemStore returns an in-memory Store (thread-safe; data is lost when the process exits).
func NewMemStore() Store {
	return &memStore{data: make(map[string]Aggregate), config: make(map[string]OnlineConfig)}
}

func (s *memStore) UpsertAggregate(_ context.Context, a Aggregate) error {
	if a.Namespace == "" || a.AgentName == "" {
		return fmt.Errorf("onlinescore: %w: namespace and agentName are required", controlplane.ErrInvalid)
	}
	a.WindowStart = a.WindowStart.Truncate(time.Hour)
	a.UpdatedAt = time.Now().UTC()

	key := memKey(a.Namespace, a.AgentName, a.AgentVersion, a.WindowStart)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.data[key]; ok {
		// Preserve the original ID (the row identity survives an update).
		a.ID = existing.ID
	}
	s.data[key] = a
	return nil
}

func (s *memStore) GetAggregate(_ context.Context, namespace, agentName, agentVersion string, windowStart time.Time) (*Aggregate, error) {
	key := memKey(namespace, agentName, agentVersion, windowStart)
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.data[key]
	if !ok {
		return nil, fmt.Errorf("onlinescore: %w", controlplane.ErrNotFound)
	}
	copy := a
	return &copy, nil
}

func (s *memStore) ListAggregates(_ context.Context, namespace, agentName string, limit int) ([]Aggregate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Aggregate, 0)
	for _, a := range s.data {
		if a.Namespace == namespace && a.AgentName == agentName {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b Aggregate) int {
		// Newest first: b.After(a) → a comes after b → negative means a goes later.
		if a.WindowStart.After(b.WindowStart) {
			return -1
		}
		if b.WindowStart.After(a.WindowStart) {
			return 1
		}
		return 0
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memStore) UpsertOnlineConfig(_ context.Context, cfg OnlineConfig) error {
	if cfg.Namespace == "" || cfg.AgentName == "" {
		return fmt.Errorf("onlinescore: %w: namespace and agentName are required", controlplane.ErrInvalid)
	}
	cfg.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config[configKey(cfg.Namespace, cfg.AgentName)] = cfg
	return nil
}

func (s *memStore) GetOnlineConfig(_ context.Context, namespace, agentName string) (OnlineConfig, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg, ok := s.config[configKey(namespace, agentName)]
	if !ok {
		return OnlineConfig{}, false, nil
	}
	return cfg, true, nil
}

func (s *memStore) DeleteOnlineConfig(_ context.Context, namespace, agentName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.config, configKey(namespace, agentName))
	return nil
}
