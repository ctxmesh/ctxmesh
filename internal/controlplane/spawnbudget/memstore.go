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

package spawnbudget

import (
	"context"
	"fmt"
	"sync"
)

type memStore struct {
	mu   sync.RWMutex
	data map[string]Budget
}

// NewMemStore returns an in-memory Store (reconcile unit tests + a dev twin).
func NewMemStore() Store { return &memStore{data: make(map[string]Budget)} }

func key(ns, agent string) string { return ns + "/" + agent }

func (s *memStore) Set(_ context.Context, b Budget) error {
	if b.Namespace == "" || b.Agent == "" {
		return fmt.Errorf("spawnbudget: namespace and agent are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key(b.Namespace, b.Agent)] = b
	return nil
}

func (s *memStore) Delete(_ context.Context, namespace, agent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key(namespace, agent))
	return nil
}

func (s *memStore) Get(_ context.Context, namespace, agent string) (Budget, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.data[key(namespace, agent)]
	if !ok {
		return Budget{}, false, nil
	}
	return b, true, nil
}
