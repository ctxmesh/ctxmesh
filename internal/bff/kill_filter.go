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
	"sync"
	"time"

	"github.com/ctxmesh/agentry/internal/controlplane/killscope"
	"github.com/ctxmesh/agentry/internal/run"
)

// Layer (b) of the scoped kill switch (M146, ADR 0126 §3): a killed scope's queued runs must NOT be
// CLAIMED.
//
// This is the layer that is easy to forget and expensive to miss. Since ADR 0125 every run is queued and
// drained by the worker pool, so a stop that only interrupts in-flight work leaves a backlog that
// executes the instant the kill is lifted — a "stop" that is really a "pause with a surprise ending",
// structurally the same trap as M143's G18 (a timeout that is satisfied is a timeout that is disarmed).
//
// It reads the CONTROL PLANE, never the state layer, and fails CLOSED — see claimFilter.

// killFilterTTL bounds how stale the claim filter may be. The claim loop runs continuously, so re-reading
// the (normally empty) kill set on every tick would be pure overhead; a second is far below the human
// timescale of an incident and well above the loop's.
const killFilterTTL = time.Second

// killFilterCache memoises the expanded filter for killFilterTTL.
type killFilterCache struct {
	mu       sync.Mutex
	filter   run.ClaimFilter
	err      error
	expires  time.Time
	nowFn    func() time.Time
	resolved bool
}

func (c *killFilterCache) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

// claimFilter returns the exclusion the worker must apply when claiming.
//
// FAIL-CLOSED, with a deliberate limit on what that means: if the kill set cannot be read (or a killed
// tenant's membership cannot be resolved) we do not know what is stopped, so the worker declines to
// START new work. It never CANCELS running work — "when in doubt, don't start" is the safe direction,
// while "when in doubt, kill" would turn a control-plane blip into an outage.
//
// With no kill store configured the filter is empty: an install that never uses the feature behaves
// exactly as it did pre-M146, rather than being unable to claim.
func (s *Server) claimFilter(ctx context.Context) (run.ClaimFilter, error) {
	if s.killScopes == nil {
		return run.ClaimFilter{}, nil
	}
	c := &s.killFilter
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved && c.now().Before(c.expires) {
		return c.filter, c.err
	}

	kills, err := s.killScopes.Active(ctx)
	if err != nil {
		c.filter, c.err = run.ClaimFilter{}, err
	} else {
		var members killscope.TenantMembers
		if s.namespaceTenantStore != nil {
			members = s.namespaceTenantStore.MembersOf
		}
		expanded, xErr := killscope.Expand(ctx, kills, members)
		c.filter, c.err = toClaimFilter(expanded), xErr
	}
	c.resolved = true
	c.expires = c.now().Add(killFilterTTL)
	return c.filter, c.err
}

// toClaimFilter converts the control plane's expansion into the run store's shape. The two types are
// deliberately separate: `run` must not import the control plane (a store knows namespaces and agents,
// not tenants), and the conversion is the one place that boundary is crossed.
func toClaimFilter(f killscope.Filter) run.ClaimFilter {
	out := run.ClaimFilter{HaltAll: f.HaltAll, Namespaces: f.Namespaces}
	for _, a := range f.Agents {
		out.Agents = append(out.Agents, run.AgentRef{Namespace: a.Namespace, Agent: a.Agent})
	}
	return out
}
