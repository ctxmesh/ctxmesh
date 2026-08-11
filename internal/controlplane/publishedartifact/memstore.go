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

package publishedartifact

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// memStore is the in-memory implementation of Store — the handler-unit-test twin and a development store.
// It mirrors the pgStore semantics exactly: version is monotonic per origin group, Tombstone flags every
// version, GetLatest returns the highest non-tombstoned version. Data is lost when the process exits.
type memStore struct {
	mu   sync.Mutex
	data map[originKey][]PublishedArtifact // origin → its versions in insert (ascending version) order
}

// originKey groups an artifact's versions by (kind, namespace, name), mirroring the Postgres PRIMARY KEY's
// leading columns.
type originKey struct {
	kind, ns, name string
}

// NewMemStore returns an in-memory Store (thread-safe; data is lost when the process exits).
func NewMemStore() Store {
	return &memStore{data: make(map[originKey][]PublishedArtifact)}
}

func (s *memStore) Publish(_ context.Context, rec PublishedArtifact) (int, error) {
	if rec.Kind == "" || rec.OriginNamespace == "" || rec.OriginName == "" {
		return 0, fmt.Errorf("publishedartifact: kind, origin_namespace, and origin_name are required")
	}
	if len(rec.SpecJSON) == 0 {
		return 0, fmt.Errorf("publishedartifact: spec_json is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	k := originKey{rec.Kind, rec.OriginNamespace, rec.OriginName}
	// Monotonic per origin: max existing version + 1 (independent across origins).
	next := 0
	for i := range s.data[k] {
		if v := s.data[k][i].Version; v > next {
			next = v
		}
	}
	next++

	stored := PublishedArtifact{
		Kind:            rec.Kind,
		OriginNamespace: rec.OriginNamespace,
		OriginName:      rec.OriginName,
		Version:         next,
		SpecJSON:        append([]byte(nil), rec.SpecJSON...),
		Visibility:      rec.Visibility,
		ContentHash:     rec.ContentHash,
		PublishedAt:     time.Now().UTC(),
		Tombstoned:      false,
	}
	s.data[k] = append(s.data[k], stored)
	return next, nil
}

func (s *memStore) Tombstone(_ context.Context, kind, ns, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	versions := s.data[originKey{kind, ns, name}]
	for i := range versions {
		versions[i].Tombstoned = true
	}
	return nil
}

// ListTemplates implements Store.ListTemplates — the cross-tenant template catalog read (m74.2, ADR 0068 §2/§3).
// It applies the same leak-safe predicate as the pgStore in Go: latest non-tombstoned version per origin,
// private rows never returned. members is the full tenant member-namespace set including callerNS.
func (s *memStore) ListTemplates(_ context.Context, callerNS string, members []string) ([]PublishedArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// latest tracks the winning (highest version) artifact per origin for the visible rows.
	type latestEntry struct {
		rec     PublishedArtifact
		version int
	}
	seen := make(map[originKey]latestEntry)

	for k, versions := range s.data {
		for i := range versions {
			rec := &versions[i]
			if rec.Tombstoned {
				continue
			}
			// Leak-safe visibility predicate — mirrors the pgStore SQL exactly.
			// "private" and any unknown value leave visible=false (zero value) — never returned.
			var visible bool
			switch rec.Visibility {
			case "public":
				visible = true
			case "org":
				visible = slices.Contains(members, k.ns)
			case "team":
				visible = k.ns == callerNS
			}
			if !visible {
				continue
			}
			// Keep only the latest version per origin.
			if e, ok := seen[k]; !ok || rec.Version > e.version {
				out := *rec
				out.SpecJSON = append([]byte(nil), rec.SpecJSON...)
				seen[k] = latestEntry{rec: out, version: rec.Version}
			}
		}
	}

	result := make([]PublishedArtifact, 0, len(seen))
	for _, e := range seen {
		result = append(result, e.rec)
	}
	return result, nil
}

func (s *memStore) GetLatest(_ context.Context, kind, ns, name string) (*PublishedArtifact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *PublishedArtifact
	for i := range s.data[originKey{kind, ns, name}] {
		rec := &s.data[originKey{kind, ns, name}][i]
		if rec.Tombstoned {
			continue
		}
		if latest == nil || rec.Version > latest.Version {
			latest = rec
		}
	}
	if latest == nil {
		return nil, false, nil
	}
	// Return a defensive copy so a caller mutation never touches the stored record.
	out := *latest
	out.SpecJSON = append([]byte(nil), latest.SpecJSON...)
	return &out, true, nil
}
