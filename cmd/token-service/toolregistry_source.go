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

package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/controlplane"
	"github.com/ctxmesh/agentry/internal/controlplane/toolregistry"
)

// defaultToolRegistryCacheTTL bounds how stale the runtime's auth-type / org-scope
// view can be after retirement. Short enough that a scope/OAuth change propagates
// to the OBO path within seconds; long enough that a per-tool-call read almost
// never touches Postgres.
const defaultToolRegistryCacheTTL = 30 * time.Second

// toolRegistrySource reads a server's ToolRegistry for the runtime auth-type /
// org-scope resolution. After the ToolRegistry CRD is retired (RETIRE_TR, ADR 0044)
// the runtime reads Postgres instead of the K8s API — on the OBO hot path, per
// tool-call. To bound the blast radius of a Postgres blip (Fable's design gate),
// it wraps the store in a short-TTL cache that ALSO SERVES THE LAST-KNOWN VALUE on
// a store error: a brief outage is invisible for any recently-resolved server, and
// only a never-before-seen server during a sustained outage degrades — and even
// then fail-SAFE (a lookup error is treated by the callers as "not OAuth", never a
// credential leak).
type toolRegistrySource struct {
	store toolregistry.Store
	ttl   time.Duration
	log   logr.Logger

	mu   sync.Mutex
	rows map[string]trCacheEntry
}

type trCacheEntry struct {
	tr        *agentsv1alpha1.ToolRegistry // nil ⇒ negative (server does not exist)
	fetchedAt time.Time
}

func newToolRegistrySource(store toolregistry.Store, ttl time.Duration, log logr.Logger) *toolRegistrySource {
	if ttl <= 0 {
		ttl = defaultToolRegistryCacheTTL
	}
	return &toolRegistrySource{store: store, ttl: ttl, log: log, rows: map[string]trCacheEntry{}}
}

// getTR returns the server's ToolRegistry, or (nil, nil) when it does not exist —
// matching the CRD getTR contract, so the callers treat a miss conservatively
// (not-OAuth / not-org). Only the fields the runtime consults are projected: the
// auth-type / OAuth-config annotations and the scope label (ToolRegistry has no
// controller-owned status the runtime reads).
func (s *toolRegistrySource) getTR(ctx context.Context, ns, server string) (*agentsv1alpha1.ToolRegistry, error) {
	key := ns + "/" + server
	if e, ok := s.fresh(key); ok {
		return e.tr, nil
	}
	r, err := s.store.Get(ctx, ns, server)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			s.put(key, nil)
			return nil, nil
		}
		// Store error → serve the last-known value if we have one (the blast-radius
		// mitigation); otherwise propagate (the callers degrade to "not OAuth",
		// fail-safe). Never cache an error.
		if e, ok := s.stale(key); ok {
			s.log.Info("toolregistry store read failed; serving cached value",
				"namespace", ns, "server", server, "err", err.Error())
			return e.tr, nil
		}
		return nil, err
	}
	tr := &agentsv1alpha1.ToolRegistry{ObjectMeta: metav1.ObjectMeta{
		Namespace: r.Namespace, Name: r.Name, Annotations: r.Annotations, Labels: r.Labels,
	}}
	s.put(key, tr)
	return tr, nil
}

// fresh returns a cached entry only if it is within the TTL.
func (s *toolRegistrySource) fresh(key string) (trCacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[key]
	if ok && time.Since(e.fetchedAt) < s.ttl {
		return e, true
	}
	return trCacheEntry{}, false
}

// stale returns any cached entry regardless of age (the serve-on-error fallback).
func (s *toolRegistrySource) stale(key string) (trCacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[key]
	return e, ok
}

func (s *toolRegistrySource) put(key string, tr *agentsv1alpha1.ToolRegistry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[key] = trCacheEntry{tr: tr, fetchedAt: time.Now()}
}
