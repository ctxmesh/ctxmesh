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

package agentversion

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
)

type key struct{ ns, uid, name string }

// memStore is the in-memory twin used by unit tests and envtests. It passes the same conformance
// suite as the Postgres store.
type memStore struct {
	mu       sync.RWMutex
	rows     map[key]Version
	seq      int64
	imported *ImportResult
	now      func() time.Time
}

// NewMemStore returns an in-memory Store.
func NewMemStore() Store {
	return &memStore{rows: map[key]Version{}, now: time.Now}
}

func (s *memStore) Record(_ context.Context, v Version) (bool, error) {
	if err := validate(v); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordLocked(v), nil
}

func (s *memStore) recordLocked(v Version) bool {
	k := key{v.Namespace, v.AgentUID, v.Name}
	if _, ok := s.rows[k]; ok {
		return false
	}
	s.seq++
	v.Seq = s.seq
	v.Source = sourceOrController(v.Source)
	if v.CreatedAt.IsZero() {
		v.CreatedAt = s.now().UTC()
	}
	v.Snapshot = *v.Snapshot.DeepCopy()
	s.rows[k] = v
	return true
}

func (s *memStore) Get(_ context.Context, namespace, agentUID, name string) (Version, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.rows[key{namespace, agentUID, name}]
	if !ok {
		return Version{}, false, nil
	}
	v.Snapshot = *v.Snapshot.DeepCopy()
	return v, true, nil
}

func (s *memStore) Owners(_ context.Context, namespace, name string) ([]Owner, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Owner{}
	for k, v := range s.rows {
		if k.ns == namespace && k.name == name {
			out = append(out, Owner{Agent: v.Agent, AgentUID: v.AgentUID})
		}
	}
	slices.SortFunc(out, func(a, b Owner) int {
		if a.Agent != b.Agent {
			return cmp.Compare(a.Agent, b.Agent)
		}
		return cmp.Compare(a.AgentUID, b.AgentUID)
	})
	return out, nil
}

func (s *memStore) List(_ context.Context, namespace, agentUID string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listLocked(namespace, agentUID), nil
}

// listLocked returns the incarnation's entries, newest first.
func (s *memStore) listLocked(namespace, agentUID string) []Entry {
	out := []Entry{}
	for k, v := range s.rows {
		if k.ns == namespace && k.uid == agentUID {
			out = append(out, Entry{Name: v.Name, Seq: v.Seq, Source: v.Source, CreatedAt: v.CreatedAt})
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return cmp.Compare(b.Seq, a.Seq) })
	return out
}

func (s *memStore) Prior(_ context.Context, namespace, agentUID, name string) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := s.listLocked(namespace, agentUID)
	for i, e := range entries {
		if e.Name == name {
			if i+1 < len(entries) {
				return entries[i+1].Name, true, nil
			}
			return "", false, nil
		}
	}
	return "", false, nil
}

func (s *memStore) Prune(_ context.Context, namespace, agent, agentUID string, keep int, pinned []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := map[string]bool{}
	for i, e := range s.listLocked(namespace, agentUID) {
		if i < keep {
			kept[e.Name] = true
		}
	}
	for _, p := range pinned {
		kept[p] = true
	}
	removed := 0
	for k, v := range s.rows {
		if k.ns != namespace || v.Agent != agent {
			continue
		}
		if k.uid != agentUID || !kept[k.name] {
			delete(s.rows, k)
			removed++
		}
	}
	return removed, nil
}

func (s *memStore) DeleteAgent(_ context.Context, namespace, agent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.rows {
		if k.ns == namespace && v.Agent == agent {
			delete(s.rows, k)
		}
	}
	return nil
}

func (s *memStore) ImportOnce(ctx context.Context, load func(context.Context) ([]Version, int, error)) (ImportResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.imported != nil {
		return ImportResult{}, nil
	}
	versions, skipped, err := load(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	for _, v := range versions {
		if err := validate(v); err != nil {
			return ImportResult{}, err
		}
	}
	// Validated first so a bad row leaves nothing written, as the Postgres transaction does.
	imported := 0
	for _, v := range versions {
		v.Source = SourceImport
		if s.recordLocked(v) {
			imported++
		}
	}
	s.imported = &ImportResult{Ran: true, Imported: imported, Skipped: skipped}
	return *s.imported, nil
}
