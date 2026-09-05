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

package namespacetenant

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

// memStore is the in-memory implementation of Store, used in the reconcile unit test (tier0/tier1,
// no Postgres) and as a development twin. It keys by namespace (1 ns ∈ ≤1 tenant, ADR 0046).
type memStore struct {
	mu   sync.RWMutex
	data map[string]string // namespace → tenant
	// hardCap tracks the per-tenant at-storage-hard-cap flag (m80.3). Only tenants at their hard cap
	// have an entry (true); a tenant under cap / with no hard cap has no entry (⇒ not exceeded).
	hardCap map[string]bool // tenant → at-hard-cap
	// endUser tracks the per-tenant end-user OIDC config (M137/EU1b). A tenant with no entry has no
	// end-user IdP (fail-closed for end-user auth).
	endUser map[string]EndUserIdentity // tenant → config
}

// NewMemStore returns an in-memory Store (thread-safe; data is lost when the process exits).
func NewMemStore() Store {
	return &memStore{
		data:    make(map[string]string),
		hardCap: make(map[string]bool),
		endUser: make(map[string]EndUserIdentity),
	}
}

func (s *memStore) SetMembers(_ context.Context, tenant string, namespaces []string) error {
	if tenant == "" {
		return fmt.Errorf("namespacetenant: tenant is required")
	}
	want := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		if ns != "" {
			want[ns] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Upsert (re-attribute a moved namespace too).
	for ns := range want {
		s.data[ns] = tenant
	}
	// Prune rows this tenant owns that left the set.
	for ns, owner := range s.data {
		if owner == tenant && !want[ns] {
			delete(s.data, ns)
		}
	}
	return nil
}

func (s *memStore) DeleteTenant(_ context.Context, tenant string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ns, owner := range s.data {
		if owner == tenant {
			delete(s.data, ns)
		}
	}
	delete(s.hardCap, tenant)
	delete(s.endUser, tenant)
	return nil
}

// AllNamespaces returns every mirrored namespace, across all tenants, sorted ascending.
func (s *memStore) AllNamespaces(_ context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.data))
	for ns := range s.data {
		out = append(out, ns)
	}
	slices.Sort(out)
	return out, nil
}

func (s *memStore) MembersOf(_ context.Context, tenant string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0)
	for ns, owner := range s.data {
		if owner == tenant {
			out = append(out, ns)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *memStore) TenantOf(_ context.Context, namespace string) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenant, ok := s.data[namespace]
	return tenant, ok, nil
}

func (s *memStore) SetStorageHardCapExceeded(_ context.Context, tenant string, exceeded bool) error {
	if tenant == "" {
		return fmt.Errorf("namespacetenant: tenant is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if exceeded {
		s.hardCap[tenant] = true
	} else {
		delete(s.hardCap, tenant) // absent entry ⇒ not exceeded (keeps the map to only at-cap tenants).
	}
	return nil
}

func (s *memStore) StorageHardCapExceededFor(_ context.Context, namespace string) (bool, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenant, ok := s.data[namespace]
	if !ok {
		return false, false, nil // no row for this namespace → fail-open (not blocked).
	}
	return s.hardCap[tenant], true, nil
}

func (s *memStore) SetEndUserIdentity(_ context.Context, tenant string, cfg EndUserIdentity) error {
	if tenant == "" {
		return fmt.Errorf("namespacetenant: tenant is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endUser[tenant] = cfg
	return nil
}

func (s *memStore) EndUserIdentityForNamespace(_ context.Context, namespace string) (EndUserIdentity, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tenant, ok := s.data[namespace]
	if !ok {
		return EndUserIdentity{}, false, nil // namespace maps to no tenant → fail-closed (no end-user IdP).
	}
	cfg, ok := s.endUser[tenant]
	if !ok {
		return EndUserIdentity{}, false, nil // tenant has no config → fail-closed.
	}
	return cfg, true, nil
}
