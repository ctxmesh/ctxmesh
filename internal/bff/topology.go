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
func buildTopology(ctx context.Context, r AgentReader) (TopologyResponse, error) {
	registries, err := listAgentRegistries(ctx, r)
	if err != nil {
		return TopologyResponse{}, fmt.Errorf("list AgentRegistries: %w", err)
	}
	deployments, err := listAgentDeployments(ctx, r)
	if err != nil {
		return TopologyResponse{}, fmt.Errorf("list AgentDeployments: %w", err)
	}
	bindings, err := listMCPToolBindings(ctx, r)
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
