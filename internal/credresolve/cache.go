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

package credresolve

import (
	"sync"
	"time"
)

// maxCacheTTL caps how long a resolved token is served from the local cache regardless
// of its own (possibly long/absent) expiry. It bounds two things: memory staleness of a
// non-expiring token, and the window during which a revoked grant is still served by a
// pod's local cache before it re-reads the Secret. A few minutes keeps the fast path hot
// (the latency win of the per-pod sidecar, ADR 0030) while keeping the cache "short-TTL".
const maxCacheTTL = 5 * time.Minute

// tokenCache is a small, mutex-guarded, per-backend cache of resolved access tokens
// keyed by grant coordinates (readNamespace/name — which already encode the source
// namespace, server, and user hash, so entries never collide across users). It lets the
// per-pod sidecar answer a repeat tool call without re-reading the grant Secret or
// re-refreshing, which is the latency half of the two-tier design.
//
// It caches only access tokens (never refresh tokens) and only the expiry needed to
// decide validity — it is not a store of record; the grant Secret is.
type tokenCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

// cacheEntry is a cached access token and the instant it stops being served locally
// (the earlier of its refresh-skewed expiry and maxCacheTTL).
type cacheEntry struct {
	access     string
	servedThru time.Time
}

// newTokenCache builds an empty cache.
func newTokenCache() *tokenCache {
	return &tokenCache{entries: make(map[string]cacheEntry)}
}

// get returns the cached access token for key when it is still within its local serve
// window at now, else ("", false). It opportunistically drops an expired entry so the
// map does not grow unbounded without a background janitor.
func (c *tokenCache) get(key string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if !now.Before(e.servedThru) {
		delete(c.entries, key)
		return "", false
	}
	return e.access, true
}

// put caches access under key, served until the earlier of (tokenExpiry - RefreshSkew)
// and (now + maxCacheTTL). A zero tokenExpiry (non-expiring token) is served for
// maxCacheTTL. A token already inside its skew window is not cached (servedThru <= now →
// the next get misses and re-resolves), so the cache never serves a soon-to-expire token.
func (c *tokenCache) put(key, access string, tokenExpiry, now time.Time) {
	hardCap := now.Add(maxCacheTTL)
	servedThru := hardCap
	if !tokenExpiry.IsZero() {
		skewed := tokenExpiry.Add(-RefreshSkew)
		if skewed.Before(servedThru) {
			servedThru = skewed
		}
	}
	if !servedThru.After(now) {
		// Nothing worth caching (the token is already within its refresh window).
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{access: access, servedThru: servedThru}
}

// evict drops any cached entry for key — called on revoke so a revoked grant is not
// served from this backend's cache afterward.
func (c *tokenCache) evict(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}
