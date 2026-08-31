package killscope

import (
	"context"
	"fmt"
	"slices"
)

// AgentRef names one agent workload.
type AgentRef struct{ Namespace, Agent string }

// Filter is the active kill set expanded into the concrete predicates a run store can apply. The run
// store knows a run's namespace and agent but nothing about tenants, so tenant kills are resolved to
// their member namespaces HERE, in the control plane that owns that mapping (ADR 0067's ns→tenant
// mirror) — the store stays a store.
type Filter struct {
	// HaltAll short-circuits everything: a fleet kill is active.
	HaltAll bool
	// Namespaces are halted wholesale (namespace kills, plus every member of a killed tenant).
	Namespaces []string
	// Agents are halted individually.
	Agents []AgentRef
}

// Empty reports whether the filter excludes nothing — the overwhelmingly common case, checked first so
// a healthy platform pays almost nothing for the feature existing.
func (f Filter) Empty() bool {
	return !f.HaltAll && len(f.Namespaces) == 0 && len(f.Agents) == 0
}

// Excludes reports whether a run in (namespace, agent) is under an active kill.
func (f Filter) Excludes(namespace, agent string) bool {
	if f.HaltAll {
		return true
	}
	if slices.Contains(f.Namespaces, namespace) {
		return true
	}
	for _, a := range f.Agents {
		if a.Namespace == namespace && a.Agent == agent {
			return true
		}
	}
	return false
}

// TenantMembers resolves a tenant to the namespaces it owns (the ns→tenant mirror's MembersOf).
type TenantMembers func(ctx context.Context, tenant string) ([]string, error)

// Expand turns the active kill set into a Filter.
//
// FAIL-CLOSED, with a deliberate limit on what that means: if a tenant's membership cannot be resolved
// we do not know which namespaces the kill covers, so we return an error and the caller declines to
// START new work. It never CANCELS running work — "when in doubt, don't start" is the safe direction;
// "when in doubt, kill" would turn a mirror blip into an outage.
//
// The blast radius of that posture is bounded: with no active kills (the normal state) this never calls
// TenantMembers at all, so a degraded mirror costs nothing until someone actually pulls a tenant stop —
// at which point halting is what they asked for.
func Expand(ctx context.Context, kills []Kill, members TenantMembers) (Filter, error) {
	var f Filter
	for _, k := range kills {
		switch k.Scope.Level {
		case LevelFleet:
			// Nothing else can widen this; stop expanding.
			return Filter{HaltAll: true}, nil
		case LevelNamespace:
			f.Namespaces = append(f.Namespaces, k.Scope.Namespace)
		case LevelAgent:
			f.Agents = append(f.Agents, AgentRef{Namespace: k.Scope.Namespace, Agent: k.Scope.Agent})
		case LevelTenant:
			if members == nil {
				return Filter{}, fmt.Errorf("killscope: tenant %q is killed but no membership resolver is configured", k.Scope.Tenant)
			}
			ns, err := members(ctx, k.Scope.Tenant)
			if err != nil {
				return Filter{}, fmt.Errorf("killscope: resolving killed tenant %q: %w", k.Scope.Tenant, err)
			}
			f.Namespaces = append(f.Namespaces, ns...)
		}
	}
	return f, nil
}
