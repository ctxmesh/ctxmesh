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

package bff

import (
	"context"
	"strings"
	"sync"
	"time"
)

// isTruthyParam reports whether a query-string value means "on": "1", "true", "yes", or "on"
// (case-insensitive). Anything else — including "" and "0" — is off. Keeps opt-in flags like
// ?enrich= permissive about the caller's spelling without treating a bare presence as true.
func isTruthyParam(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", labelValueTrue, "yes", "on":
		return true
	default:
		return false
	}
}

// Per-trace runs enrichment (ADR 0081) — real tokens + a coarse status for the Runs list.
//
// The Langfuse traces-LIST endpoint (GET /api/public/traces) carries neither a per-trace
// usage/token total nor a status field (verified against the raw API — only totalCost is
// present; observations come back as ID strings). So the plain runs list can only show cost +
// latency honestly; tokens read "—" and there is no outcome column. To surface REAL tokens + a
// status the BFF fetches each visible trace's /detail (TraceDetail → Rollup.Tokens + the coarse
// traceStatus), which DOES sum the observations. That is an N+1 over the visible page, so it is
// OPT-IN (the handler gates it on ?enrich=) and bounded: only the visible rows, at most
// traceEnrichConcurrency in flight, results cached for traceEnrichTTL, and FAIL-SOFT — a per-trace
// fetch error leaves that row's tokens/status untouched (an honest "—"/blank), never failing the
// list. Display-only: enrichment fills the fields the console renders; it does not change which
// rows the list returns (the status FILTER stays documented-not-applied, handleRuns).
const (
	// traceEnrichTTL bounds how long a per-trace (tokens,status) result is reused. Runs are
	// terminal once traced, so their totals do not change — a minute keeps a paging/refresh
	// session cheap without holding stale data meaningfully long.
	traceEnrichTTL = 60 * time.Second
	// traceEnrichConcurrency caps in-flight /detail fetches so enriching a full page cannot
	// stampede Langfuse (bounded load, ADR 0081).
	traceEnrichConcurrency = 5
	// traceEnrichCacheCap bounds the cache's memory — a lazily-swept, capped map (never unbounded).
	traceEnrichCacheCap = 2048
)

// enrichEntry is one cached per-trace enrichment: the real token total + the coarse status,
// with an expiry for the TTL sweep.
type enrichEntry struct {
	tokens  int64
	status  string
	expires time.Time
}

// traceEnrichCache is a small, bounded, TTL cache of per-trace enrichment keyed by traceID.
// Mutex-guarded (the enrichment fans out across goroutines). Always constructed in NewServer.
type traceEnrichCache struct {
	mu  sync.Mutex
	m   map[string]enrichEntry
	ttl time.Duration
	cap int
	now func() time.Time // injectable for tests
}

func newTraceEnrichCache() *traceEnrichCache {
	return &traceEnrichCache{
		m:   make(map[string]enrichEntry),
		ttl: traceEnrichTTL,
		cap: traceEnrichCacheCap,
		now: time.Now,
	}
}

// get returns the cached enrichment for id when present and unexpired.
func (c *traceEnrichCache) get(id string) (enrichEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok || c.now().After(e.expires) {
		return enrichEntry{}, false
	}
	return e, true
}

// put stores an enrichment under id with a fresh expiry. Bounded: at cap it first sweeps expired
// entries, and if still full it drops the write (a future cache miss is cheaper than unbounded
// growth) — the cache is an optimization, never a correctness dependency.
func (c *traceEnrichCache) put(id string, e enrichEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.expires = c.now().Add(c.ttl)
	if len(c.m) >= c.cap {
		for k, v := range c.m {
			if c.now().After(v.expires) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.cap {
			return
		}
	}
	c.m[id] = e
}

// enrichRunsWithDetail fills each run's real token total + coarse status by fetching the trace's
// /detail — see the package comment above for why (the list carries neither) and the bounds
// (opt-in, visible-page-only, bounded concurrency, cached, fail-soft). Returns the same slice,
// enriched in place. A nil Langfuse adapter or an empty page is a no-op.
func (s *Server) enrichRunsWithDetail(ctx context.Context, runs []RunSummary) []RunSummary {
	if len(runs) == 0 || s.adapters.Langfuse == nil || s.enrichCache == nil {
		return runs
	}
	sem := make(chan struct{}, traceEnrichConcurrency)
	var wg sync.WaitGroup
	for i := range runs {
		id := runs[i].TraceID
		if id == "" {
			continue
		}
		if e, ok := s.enrichCache.get(id); ok {
			applyEnrich(&runs[i], e)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, traceID string) {
			defer wg.Done()
			defer func() { <-sem }()
			// Fail-soft: a per-trace fetch error (or a trace with no detail) leaves this row's
			// tokens/status untouched — an honest "—"/blank, never a failed list (ADR 0081).
			detail, err := s.adapters.Langfuse.TraceDetail(ctx, traceID)
			if err != nil {
				return
			}
			e := enrichEntry{tokens: detail.Rollup.Tokens, status: traceStatus(detail)}
			s.enrichCache.put(traceID, e)
			applyEnrich(&runs[idx], e) // distinct index per goroutine → no shared-slot race
		}(i, id)
	}
	wg.Wait()
	return runs
}

// applyEnrich overlays a cached enrichment onto one run. Tokens are overwritten ONLY when the
// detail carried a real total (>0): the list total is usually 0/not-captured, but on the rare
// occasion it is present we never clobber it with a 0. Status is set verbatim (an enriched run
// always has a determinable ok/error/"" outcome).
func applyEnrich(r *RunSummary, e enrichEntry) {
	if e.tokens > 0 {
		r.Tokens = e.tokens
	}
	r.Status = e.status
}
