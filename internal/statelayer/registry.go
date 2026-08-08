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

package statelayer

import (
	"context"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// registryIDLabelKey is the label the AgentDeployment controller stamps on a per-agent
// identity ServiceAccount carrying the agent's registry id (ADR 0052 §C6 RESOLUTION). It
// MUST match the controller's registryIDLabel (internal/controller) — the server-trusted
// source for the shared-memory boundary, replacing the runcap `bnd` claim.
const registryIDLabelKey = "agents.ctxmesh.ai/registry-id"

// registryCacheTTL bounds how long a resolved (ns,agent)→registry mapping is cached. A
// membership change is reflected within the TTL; a stale value is a same-tenant blip (the
// (ns,agent) is TokenReview-verified), never a cross-tenant leak. Kept short because a
// wrong shared-scratchpad key is user-visible.
const registryCacheTTL = 30 * time.Second

// saRegistryResolver resolves an agent's shared-memory registry by reading the
// registryIDLabel LABEL on its identity ServiceAccount via the API, cached. It implements
// RegistryResolver.
type saRegistryResolver struct {
	client kubernetes.Interface
	ttl    time.Duration
	now    func() time.Time

	mu    sync.RWMutex
	cache map[string]registryCacheEntry
}

type registryCacheEntry struct {
	registry string
	expiry   time.Time
}

// NewSARegistryResolver builds a RegistryResolver over a Kubernetes client. It reads the
// registryIDLabel on the agent's identity SA (get serviceaccounts) and caches the result.
func NewSARegistryResolver(client kubernetes.Interface) RegistryResolver {
	return &saRegistryResolver{
		client: client,
		ttl:    registryCacheTTL,
		now:    time.Now,
		cache:  make(map[string]registryCacheEntry),
	}
}

func (r *saRegistryResolver) Registry(ctx context.Context, namespace, serviceAccount string) (string, error) {
	key := namespace + "/" + serviceAccount
	if reg, ok := r.cached(key); ok {
		return reg, nil
	}
	sa, err := r.client.CoreV1().ServiceAccounts(namespace).Get(ctx, serviceAccount, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// No identity SA (GC race, or the pod isn't a per-agent identity) → no
			// registry → the caller falls back to the PRIVATE scope. Not an infra error;
			// cache briefly so a burst doesn't hammer the API.
			r.store(key, "")
			return "", nil
		}
		// A genuine API failure (RBAC/apiserver down) — surface it so the caller fails
		// CLOSED on shared scope rather than keying under a guessed registry.
		return "", fmt.Errorf("get serviceaccount %s: %w", key, err)
	}
	reg := sa.Labels[registryIDLabelKey]
	r.store(key, reg)
	return reg, nil
}

func (r *saRegistryResolver) cached(key string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.cache[key]
	if !ok || !r.now().Before(e.expiry) {
		return "", false
	}
	return e.registry, true
}

func (r *saRegistryResolver) store(key, registry string) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	// Sweep expired entries so the cache stays bounded to live agents.
	for k, e := range r.cache {
		if !now.Before(e.expiry) {
			delete(r.cache, k)
		}
	}
	r.cache[key] = registryCacheEntry{registry: registry, expiry: now.Add(r.ttl)}
}
