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

package agentcapability

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// memStore is the in-memory Store (reconcile unit tests + a dev twin).
type memStore struct {
	mu   sync.RWMutex
	data map[string]AgentCapability // "ns/agent" → row
}

// NewMemStore returns an in-memory Store (thread-safe; data lost on exit).
func NewMemStore() Store { return &memStore{data: make(map[string]AgentCapability)} }

func key(ns, agent string) string { return ns + "/" + agent }

func (s *memStore) Set(_ context.Context, a AgentCapability) error {
	if a.Namespace == "" || a.Agent == "" {
		return fmt.Errorf("agentcapability: namespace and agent are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a.Tags = slices.Clone(a.Tags) // own the slice — the caller may reuse its backing array
	s.data[key(a.Namespace, a.Agent)] = a
	return nil
}

func (s *memStore) Delete(_ context.Context, namespace, agent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key(namespace, agent))
	return nil
}

func (s *memStore) Get(_ context.Context, namespace, agent string) (AgentCapability, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.data[key(namespace, agent)]
	if !ok {
		return AgentCapability{}, false, nil
	}
	a.Tags = slices.Clone(a.Tags)
	return a, true, nil
}

func (s *memStore) List(_ context.Context, namespace, registryID string) ([]AgentCapability, error) {
	if registryID == "" {
		return nil, nil // discovery is registry-scoped (fail-closed) — mirrors the Postgres store
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AgentCapability, 0, len(s.data))
	for _, a := range s.data {
		// Membership-only rows (no description) advertise nothing — never candidates.
		if a.Namespace == namespace && a.RegistryID == registryID && a.Description != "" {
			a.Tags = slices.Clone(a.Tags)
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b AgentCapability) int { return strings.Compare(a.Agent, b.Agent) })
	return out, nil
}
