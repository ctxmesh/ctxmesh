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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/credresolve"
)

// agentBoundary returns the trust boundary (ADR 0033) a personal OBO grant is scoped to for a run
// of the named agent: the agent's REGISTRY when it belongs to one — agents in a registry
// collaborate over A2A and share the invoking user's credential — else the AGENT itself
// (standalone). It mirrors the controller's resolveAgentRegistry membership rule (an agent is in AT
// MOST ONE registry; on a multi-match the first by name wins; terminating registries are excluded).
//
// It fails SAFE: any lookup error degrades to the per-agent boundary, never "" — an empty boundary
// would silently widen the grant to the legacy unscoped key (every agent shares), the exact
// over-broad behaviour ADR 0033 removes. The consent WRITE path and the capability MINT path both
// call this so the boundary a grant is stored under matches the boundary a run resolves with.
func agentBoundary(ctx context.Context, c client.Client, ns, agentName string) string {
	standalone := credresolve.AgentBoundary(ns, agentName)

	var agent agentsv1alpha1.AgentDeployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: agentName}, &agent); err != nil {
		return standalone
	}
	var registries agentsv1alpha1.AgentRegistryList
	if err := c.List(ctx, &registries, client.InNamespace(ns)); err != nil {
		return standalone
	}

	agentLabels := labels.Set(agent.Labels)
	var best *agentsv1alpha1.AgentRegistry
	for i := range registries.Items {
		reg := &registries.Items[i]
		if !reg.DeletionTimestamp.IsZero() {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&reg.Spec.MemberSelector)
		if err != nil || sel.Empty() {
			// A malformed or empty selector matches no members (matching the controller).
			continue
		}
		if sel.Matches(agentLabels) && (best == nil || reg.Name < best.Name) {
			best = reg
		}
	}
	if best != nil && best.Spec.RegistryId != "" {
		return credresolve.RegistryBoundary(best.Spec.RegistryId)
	}
	return standalone
}

// endUserAgentBoundary returns the OBO trust boundary for an END-USER run (M137/EU1b, ADR 0106 §6).
// End-users have NO caller-scoped K8s client, so — unlike agentBoundary — it does NOT read the
// AgentDeployment/AgentRegistry (which would force a new BFF-SA read grant). It returns the per-agent
// STANDALONE boundary: an end-user of a standalone /chat agent gets per-agent OBO grant scoping, which
// is both correct (registry-sharing is a console/A2A collaboration concept) and MORE isolated. Because
// the end-user mint AND the end-user consent grant-write both use THIS boundary, a stored grant resolves
// (the invariant agentBoundary documents). Registry-scoped end-user OBO — if ever needed — is the reopen
// trigger that would justify a bounded BFF-SA agent read; carded, not built (no gate need).
func endUserAgentBoundary(ns, agentName string) string {
	return credresolve.AgentBoundary(ns, agentName)
}
