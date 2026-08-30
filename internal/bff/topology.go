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
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
)

// buildTopology reads the three topology CRDs via the client-go seam and folds
// them into the flat graph DTO the dashboard's React Flow view renders:
//
//	AgentRegistry ──(membership)──▶ AgentDeployment ──(binding)──▶ MCPTool
//
// Membership comes from AgentRegistry.status.members (the controller-resolved
// member names). Tool edges come from MCPToolBinding.spec.agentRef. Agents that
// belong to no registry are still surfaced as nodes (unrooted) so the operator
// sees every workload. The output is deterministic (nodes/edges sorted by id)
// so the graph and the tests are stable; both slices are non-nil.
// opts scope the three list calls; handleTopology passes client.InNamespace(ns)
// when a namespace is selected in the header (m24.3 — the scope now filters the
// dashboard/topology, not just the agents list), or nothing for cluster-wide.
func buildTopology(ctx context.Context, r AgentReader, opts ...client.ListOption) (TopologyResponse, error) {
	registries, err := listAgentRegistries(ctx, r, opts...)
	if err != nil {
		return TopologyResponse{}, fmt.Errorf("list AgentRegistries: %w", err)
	}
	deployments, err := listAgentDeployments(ctx, r, opts...)
	if err != nil {
		return TopologyResponse{}, fmt.Errorf("list AgentDeployments: %w", err)
	}
	bindings, err := listMCPToolBindings(ctx, r, opts...)
	if err != nil {
		return TopologyResponse{}, fmt.Errorf("list MCPToolBindings: %w", err)
	}

	nodes := make([]TopologyNode, 0, len(registries.Items)+len(deployments.Items)+len(bindings.Items))
	edges := make([]TopologyEdge, 0)

	// agentNodeID indexes an agent's node id by "<namespace>/<name>" so the
	// membership + binding edges can resolve their target without a second scan.
	agentNodeID := make(map[string]string, len(deployments.Items))

	// --- Agents (the middle tier) ------------------------------------------
	for i := range deployments.Items {
		ad := &deployments.Items[i]
		id := nodeID(nodeKindAgent, ad.Namespace, ad.Name)
		agentNodeID[nsName(ad.Namespace, ad.Name)] = id
		nodes = append(nodes, TopologyNode{
			ID:        id,
			Kind:      nodeKindAgent,
			Name:      ad.Name,
			Namespace: ad.Namespace,
			Health:    healthFromConditions(ad.Status.Conditions),
			Detail:    ad.Spec.Image,
		})
	}

	// --- Registries (roots) + membership edges -----------------------------
	for i := range registries.Items {
		reg := &registries.Items[i]
		regID := nodeID(nodeKindRegistry, reg.Namespace, reg.Name)
		nodes = append(nodes, TopologyNode{
			ID:        regID,
			Kind:      nodeKindRegistry,
			Name:      reg.Name,
			Namespace: reg.Namespace,
			Health:    healthFromConditions(reg.Status.Conditions),
			Detail:    reg.Spec.RegistryId,
		})
		// status.members holds controller-resolved member agent names in the
		// registry's namespace. Only wire an edge to an agent we actually saw.
		for _, member := range reg.Status.Members {
			if agentID, ok := agentNodeID[nsName(reg.Namespace, member)]; ok {
				edges = append(edges, TopologyEdge{
					ID:     edgeID(regID, agentID),
					Source: regID,
					Target: agentID,
				})
			}
		}
	}

	// --- Tools (leaves) + binding edges ------------------------------------
	for i := range bindings.Items {
		b := &bindings.Items[i]
		toolID := nodeID(nodeKindTool, b.Namespace, b.Name)
		nodes = append(nodes, TopologyNode{
			ID:        toolID,
			Kind:      nodeKindTool,
			Name:      b.Spec.ToolName,
			Namespace: b.Namespace,
			Health:    healthFromConditions(b.Status.Conditions),
			Detail:    b.Spec.Mode,
		})
		if agentID, ok := agentNodeID[nsName(b.Namespace, b.Spec.AgentRef)]; ok {
			edges = append(edges, TopologyEdge{
				ID:     edgeID(agentID, toolID),
				Source: agentID,
				Target: toolID,
			})
		}
	}

	slices.SortFunc(nodes, func(a, b TopologyNode) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(edges, func(a, b TopologyEdge) int { return cmp.Compare(a.ID, b.ID) })
	return TopologyResponse{Nodes: nodes, Edges: edges}, nil
}

// topologyGroupSpec bounds a grouped topology request. group selects the axis
// (registry or namespace); q is a lower-cased name substring (empty = no
// search); expand is the set of group ids whose members to emit as nodes; cap is
// the per-group member-node ceiling so no expanded/searched group can emit an
// unbounded node list (the whole point of the endpoint at 200+ agents).
type topologyGroupSpec struct {
	group  string
	q      string
	expand map[string]bool
	cap    int
}

// groupedAgent pairs a member agent's resolved node with the health it
// contributes to its group's rollup, so the rollup is counted once and reused.
type groupedAgent struct {
	node   TopologyNode
	health string
}

// groupAcc accumulates one group's DTO plus its FULL (pre-cap) member set, so
// the rollup counts the whole group while emission caps the visible members.
type groupAcc struct {
	g       TopologyGroup
	members []groupedAgent
}

// addToRollup bumps a health rollup by one for the given topology health state.
func addToRollup(h *HealthRollup, health string) {
	switch health {
	case healthReady:
		h.Ready++
	case healthNotReady:
		h.NotReady++
	case healthPending:
		h.Pending++
	default:
		h.Unknown++
	}
}

// agentNode projects an AgentDeployment onto the topology node + the health it
// contributes to its group's rollup (derived once, reused for both).
func agentNode(ad *agentsv1alpha1.AgentDeployment) groupedAgent {
	health := healthFromConditions(ad.Status.Conditions)
	return groupedAgent{
		node: TopologyNode{
			ID:        nodeID(nodeKindAgent, ad.Namespace, ad.Name),
			Kind:      nodeKindAgent,
			Name:      ad.Name,
			Namespace: ad.Namespace,
			Health:    health,
			Detail:    ad.Spec.Image,
		},
		health: health,
	}
}

// selectMembers decides which of a group's members to emit as nodes and records
// the cut on the group. Search mode (q set) emits name-matching members; else an
// expanded group emits all its members; a collapsed group emits none. The result
// is capped so no group ever emits an unbounded node list — Truncated/ShownCount
// reflect the cut while the group's rollup already counted the FULL set.
func selectMembers(acc *groupAcc, spec topologyGroupSpec) []groupedAgent {
	// Stable member order so the cap keeps a deterministic prefix.
	slices.SortFunc(acc.members, func(a, b groupedAgent) int { return cmp.Compare(a.node.ID, b.node.ID) })

	var toEmit []groupedAgent
	switch {
	case spec.q != "":
		for _, ga := range acc.members {
			if strings.Contains(strings.ToLower(ga.node.Name), spec.q) {
				toEmit = append(toEmit, ga)
			}
		}
	case spec.expand[acc.g.ID]:
		toEmit = acc.members
	}
	if len(toEmit) == 0 {
		return nil // collapsed: rollup stands, no member nodes
	}
	if len(toEmit) > spec.cap {
		acc.g.Truncated = true
		toEmit = toEmit[:spec.cap]
	}
	acc.g.ShownCount = len(toEmit)
	return toEmit
}

// buildGroupedTopology is the bounded-scale companion to buildTopology. Instead
// of emitting every agent, it folds member agents into GROUPS (by registry or by
// namespace) that each carry a health-rollup COUNT. By default every group is
// COLLAPSED — its member agent/tool nodes are NOT in nodes[] — so a 1000-agent
// cluster returns a handful of groups. A group's members are emitted as nodes
// only when it is in spec.expand, or (when spec.q is set) when a member's name
// matches; in both cases the member nodes are capped at spec.cap and the group's
// Truncated/ShownCount reflect the cut. Rollup counts always reflect the FULL
// group, never the truncated view.
//
// It reads the same three CRDs caller-scoped (ADR 0011) as buildTopology.
func buildGroupedTopology(ctx context.Context, r AgentReader, spec topologyGroupSpec, opts ...client.ListOption) (TopologyResponse, error) {
	registries, err := listAgentRegistries(ctx, r, opts...)
	if err != nil {
		return TopologyResponse{}, fmt.Errorf("list AgentRegistries: %w", err)
	}
	deployments, err := listAgentDeployments(ctx, r, opts...)
	if err != nil {
		return TopologyResponse{}, fmt.Errorf("list AgentDeployments: %w", err)
	}
	bindings, err := listMCPToolBindings(ctx, r, opts...)
	if err != nil {
		return TopologyResponse{}, fmt.Errorf("list MCPToolBindings: %w", err)
	}

	groups := map[string]*groupAcc{}
	// order preserves first-seen group ids for a deterministic groups[] slice.
	order := make([]string, 0)

	ensureGroup := func(id, kind, label, namespace string) *groupAcc {
		if acc, ok := groups[id]; ok {
			return acc
		}
		acc := &groupAcc{g: TopologyGroup{ID: id, Kind: kind, Label: label, Namespace: namespace}}
		groups[id] = acc
		order = append(order, id)
		return acc
	}

	// Resolve each agent's registry group id up front (registry axis only). First
	// registry claiming an agent wins so an agent in two registries still counts
	// once per the axis — the rollups stay a partition, never a double-count.
	registryOf := resolveRegistryMembership(registries, spec.group)

	// Fold every agent into exactly one group (the partition), bumping the group's
	// rollup + member count and stashing the node for possible expansion.
	for i := range deployments.Items {
		ad := &deployments.Items[i]
		var acc *groupAcc
		switch spec.group {
		case groupKindNamespace:
			acc = ensureGroup(namespaceGroupID(ad.Namespace), groupKindNamespace, ad.Namespace, ad.Namespace)
		default: // groupKindRegistry
			if gid, ok := registryOf[nsName(ad.Namespace, ad.Name)]; ok {
				reg := registryForID(registries, gid)
				acc = ensureGroup(gid, groupKindRegistry, reg.Name, reg.Namespace)
			} else {
				// Unrooted agents (no registry membership) get a per-namespace
				// synthetic group so they are still counted and expandable.
				acc = ensureGroup(unrootedGroupID(ad.Namespace), groupKindRegistry, unrootedLabel, ad.Namespace)
			}
		}
		ga := agentNode(ad)
		// Stamp the resolved group id on the node so the SPA partitions members by
		// their ACTUAL group, not by namespace (two registries in one namespace would
		// otherwise both render every agent in it).
		ga.node.Group = acc.g.ID
		acc.g.MemberCount++
		addToRollup(&acc.g.Health, ga.health)
		acc.members = append(acc.members, ga)
	}

	// Registry groups that have zero resolved members (registry exists but its
	// members aren't visible/created) still surface so the operator sees the
	// registry — only under the registry axis.
	if spec.group == groupKindRegistry {
		for i := range registries.Items {
			reg := &registries.Items[i]
			ensureGroup(registryGroupID(reg.Namespace, reg.Name), groupKindRegistry, reg.Name, reg.Namespace)
		}
	}

	// toolsByAgent indexes tool bindings by their agent key so an expanded/matched
	// agent can pull in its bound tool nodes + edges (only when the agent itself
	// is emitted — tools never appear for a collapsed group).
	toolsByAgent := map[string][]*agentsv1alpha1.MCPToolBinding{}
	for i := range bindings.Items {
		b := &bindings.Items[i]
		toolsByAgent[nsName(b.Namespace, b.Spec.AgentRef)] = append(toolsByAgent[nsName(b.Namespace, b.Spec.AgentRef)], b)
	}

	nodes := make([]TopologyNode, 0)
	edges := make([]TopologyEdge, 0)
	emitted := map[string]bool{} // node id → already appended (dedupe tools)

	// emitAgent appends an agent's node plus its bound tool nodes/edges once.
	emitAgent := func(ga groupedAgent) {
		if emitted[ga.node.ID] {
			return
		}
		emitted[ga.node.ID] = true
		nodes = append(nodes, ga.node)
		key := nsName(ga.node.Namespace, ga.node.Name)
		for _, b := range toolsByAgent[key] {
			toolID := nodeID(nodeKindTool, b.Namespace, b.Name)
			if !emitted[toolID] {
				emitted[toolID] = true
				nodes = append(nodes, TopologyNode{
					ID:        toolID,
					Kind:      nodeKindTool,
					Name:      b.Spec.ToolName,
					Namespace: b.Namespace,
					Health:    healthFromConditions(b.Status.Conditions),
					Detail:    b.Spec.Mode,
				})
			}
			edges = append(edges, TopologyEdge{
				ID:     edgeID(ga.node.ID, toolID),
				Source: ga.node.ID,
				Target: toolID,
			})
		}
	}

	// Per group, emit the selected (capped) members as nodes. selectMembers also
	// records Truncated/ShownCount; collapsed groups emit nothing and their rollup
	// stands. No path emits an unbounded node list.
	for _, id := range order {
		for _, ga := range selectMembers(groups[id], spec) {
			emitAgent(ga)
		}
	}

	// Assemble the groups slice in first-seen order, then sort by id for a
	// deterministic response (matches the node/edge ordering guarantee).
	outGroups := make([]TopologyGroup, 0, len(order))
	for _, id := range order {
		outGroups = append(outGroups, groups[id].g)
	}
	slices.SortFunc(outGroups, func(a, b TopologyGroup) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(nodes, func(a, b TopologyNode) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(edges, func(a, b TopologyEdge) int { return cmp.Compare(a.ID, b.ID) })
	return TopologyResponse{Nodes: nodes, Edges: edges, Groups: outGroups}, nil
}

// registryGroupID / namespaceGroupID / unrootedGroupID build the stable, unique
// ids the SPA echoes back in ?expand. The registry id mirrors a registry node id
// so an operator can reason about the two together.
func registryGroupID(namespace, name string) string {
	return nodeID(groupKindRegistry, namespace, name)
}

func namespaceGroupID(namespace string) string {
	return groupKindNamespace + "/" + namespace
}

func unrootedGroupID(namespace string) string {
	return nodeID(groupKindRegistry, namespace, "(unrooted)")
}

// unrootedLabel is the label of the synthetic per-namespace group that holds
// agents belonging to no registry, under the registry axis.
const unrootedLabel = "(unrooted)"

// resolveRegistryMembership maps each member agent's "<namespace>/<name>" key to
// the registry group id that owns it, under the registry axis. First registry to
// claim an agent wins so an agent listed by two registries counts once — the
// grouping stays a partition. Returns an empty (non-nil) map off the registry axis.
func resolveRegistryMembership(registries *agentsv1alpha1.AgentRegistryList, group string) map[string]string {
	out := map[string]string{}
	if group != groupKindRegistry {
		return out
	}
	for i := range registries.Items {
		reg := &registries.Items[i]
		for _, m := range reg.Status.Members {
			key := nsName(reg.Namespace, m)
			if _, taken := out[key]; !taken {
				out[key] = registryGroupID(reg.Namespace, reg.Name)
			}
		}
	}
	return out
}

// registryForID resolves a registry group id back to its registry so the group's
// label/namespace use the registry's real name. Falls back to a zero registry
// (the id is always well-formed here, so this only guards a logic slip).
func registryForID(list *agentsv1alpha1.AgentRegistryList, id string) *agentsv1alpha1.AgentRegistry {
	for i := range list.Items {
		if registryGroupID(list.Items[i].Namespace, list.Items[i].Name) == id {
			return &list.Items[i]
		}
	}
	return &agentsv1alpha1.AgentRegistry{}
}

// nodeID builds the stable, unique id for a topology node.
func nodeID(kind, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s", kind, namespace, name)
}

// edgeID builds a stable id for an edge from its endpoints.
func edgeID(source, target string) string {
	return fmt.Sprintf("%s->%s", source, target)
}

// nsName is the "<namespace>/<name>" key used to resolve edges to agent nodes.
func nsName(namespace, name string) string {
	return namespace + "/" + name
}
