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

package enduseragent

import (
	"context"
	"fmt"
	"sync"
)

// memStore is the in-memory Store (reconcile unit tests + a dev twin).
type memStore struct {
	mu   sync.RWMutex
	data map[string]ExposedAgent // "ns/agent" → row
}

// NewMemStore returns an in-memory Store (thread-safe; data lost on exit).
func NewMemStore() Store { return &memStore{data: make(map[string]ExposedAgent)} }

func key(ns, agent string) string { return ns + "/" + agent }

func (s *memStore) Set(_ context.Context, a ExposedAgent) error {
	if a.Namespace == "" || a.Agent == "" {
		return fmt.Errorf("enduseragent: namespace and agent are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key(a.Namespace, a.Agent)] = a
	return nil
}

func (s *memStore) Delete(_ context.Context, namespace, agent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key(namespace, agent))
	return nil
}

func (s *memStore) Get(_ context.Context, namespace, agent string) (ExposedAgent, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.data[key(namespace, agent)]
	if !ok {
		return ExposedAgent{}, false, nil
	}
	return a, true, nil
}
