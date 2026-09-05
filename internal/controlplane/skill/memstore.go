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

package skill

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type key struct{ ns, name string }

// memStore is the in-memory twin, used by tier0/tier1 (no Postgres) and as a development
// double. It passes the same conformance suite as the Postgres implementation, so a behaviour
// that only holds in one of them is a test failure rather than a surprise in production.
type memStore struct {
	mu       sync.RWMutex
	skills   map[key]Skill
	versions map[key][]SkillVersion
	aliases  map[key]map[string]string
	now      func() time.Time
}

// NewMemStore returns an in-memory Store (thread-safe; data is lost when the process exits).
func NewMemStore() Store {
	return &memStore{
		skills:   map[key]Skill{},
		versions: map[key][]SkillVersion{},
		aliases:  map[key]map[string]string{},
		now:      time.Now,
	}
}

func (s *memStore) UpsertSkill(_ context.Context, sk Skill) error {
	if err := ValidateSkill(sk); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{sk.Namespace, sk.Name}
	now := s.now().UTC()
	if existing, ok := s.skills[k]; ok {
		sk.CreatedAt = existing.CreatedAt
	} else {
		sk.CreatedAt = now
	}
	sk.UpdatedAt = now
	s.skills[k] = sk
	return nil
}

func (s *memStore) GetSkill(_ context.Context, ns, name string) (Skill, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sk, ok := s.skills[key{ns, name}]
	return sk, ok, nil
}

func (s *memStore) ListSkills(_ context.Context, ns string) ([]Skill, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Skill{}
	for k, sk := range s.skills {
		if k.ns == ns {
			out = append(out, sk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *memStore) DeleteSkill(_ context.Context, ns, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{ns, name}
	delete(s.skills, k)
	delete(s.versions, k)
	delete(s.aliases, k)
	return nil
}

func (s *memStore) AddVersion(_ context.Context, v SkillVersion) error {
	if err := ValidateVersion(v); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{v.Namespace, v.Skill}
	if _, ok := s.skills[k]; !ok {
		return fmt.Errorf("skill %s/%s does not exist", v.Namespace, v.Skill)
	}
	// Re-adding a digest is a NO-OP, not an error. The same bytes are the same version, so an
	// idempotent retry must succeed and two callers uploading identical content must not fork
	// the history into two "versions" of one thing.
	if slices.ContainsFunc(s.versions[k], func(e SkillVersion) bool { return e.Digest == v.Digest }) {
		return nil
	}
	v.CreatedAt = s.now().UTC()
	s.versions[k] = append(s.versions[k], v)
	return nil
}

func (s *memStore) GetVersion(_ context.Context, ns, skill, digest string) (SkillVersion, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.versions[key{ns, skill}] {
		if v.Digest == digest {
			return v, true, nil
		}
	}
	return SkillVersion{}, false, nil
}

func (s *memStore) ListVersions(_ context.Context, ns, skill string) ([]SkillVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.versions[key{ns, skill}]
	out := make([]SkillVersion, len(src))
	copy(out, src)
	// Newest first. Ties break on digest so the order is TOTAL: two versions added inside the
	// same clock tick must not swap places between calls, or "the newest version" becomes
	// non-deterministic and `latest` starts resolving differently on identical data.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].Digest > out[j].Digest
	})
	return out, nil
}

func (s *memStore) ResolveAlias(ctx context.Context, ns, skill, alias string) (string, bool, error) {
	if strings.EqualFold(alias, "latest") {
		vs, err := s.ListVersions(ctx, ns, skill)
		if err != nil || len(vs) == 0 {
			return "", false, err
		}
		return vs[0].Digest, true, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.aliases[key{ns, skill}][alias]
	return d, ok, nil
}

func (s *memStore) SetAlias(ctx context.Context, ns, skill, alias, digest string) error {
	if strings.EqualFold(alias, "latest") {
		return fmt.Errorf("alias %q is derived from the version history and cannot be set", alias)
	}
	if _, ok, err := s.GetVersion(ctx, ns, skill, digest); err != nil || !ok {
		if err != nil {
			return err
		}
		return fmt.Errorf("cannot point %q at unknown digest %s", alias, digest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{ns, skill}
	if s.aliases[k] == nil {
		s.aliases[k] = map[string]string{}
	}
	s.aliases[k][alias] = digest
	return nil
}
