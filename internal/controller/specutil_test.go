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

// Unit tests for specHash and the revision-name digests (no build tag — runs
// in make test / tier0).
package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

func TestSpecHash_Determinism(t *testing.T) {
	spec := agentsv1alpha1.AgentDeploymentSpec{
		Image:          "ghcr.io/ctxmesh/echo-agent:latest",
		ExecutionModel: "serving",
		Port:           8080,
	}

	h1, err := specHash(spec)
	require.NoError(t, err)
	h2, err := specHash(spec)
	require.NoError(t, err)

	assert.Equal(t, h1, h2, "specHash must be deterministic for identical inputs")
	assert.Len(t, h1, 8, "specHash must return exactly 8 hex characters")
}

func TestSpecHash_DifferentSpecs(t *testing.T) {
	spec1 := agentsv1alpha1.AgentDeploymentSpec{
		Image: "ghcr.io/ctxmesh/echo-agent:v1",
		Port:  8080,
	}
	spec2 := agentsv1alpha1.AgentDeploymentSpec{
		Image: "ghcr.io/ctxmesh/echo-agent:v2",
		Port:  8080,
	}

	h1, err := specHash(spec1)
	require.NoError(t, err)
	h2, err := specHash(spec2)
	require.NoError(t, err)

	assert.NotEqual(t, h1, h2, "specHash must differ for different image values")
}

func TestSpecHash_PortChange(t *testing.T) {
	base := agentsv1alpha1.AgentDeploymentSpec{
		Image: "ghcr.io/ctxmesh/echo-agent:latest",
		Port:  8080,
	}
	changed := agentsv1alpha1.AgentDeploymentSpec{
		Image: "ghcr.io/ctxmesh/echo-agent:latest",
		Port:  9090,
	}

	h1, err := specHash(base)
	require.NoError(t, err)
	h2, err := specHash(changed)
	require.NoError(t, err)

	assert.NotEqual(t, h1, h2, "specHash must differ when port changes")
}

func TestSpecHash_EnvOrder_SameSpec(t *testing.T) {
	// Same env vars in same order → same hash
	spec := agentsv1alpha1.AgentDeploymentSpec{
		Image: "ghcr.io/ctxmesh/echo-agent:latest",
		Env: []corev1.EnvVar{
			{Name: "A", Value: "1"},
			{Name: "B", Value: "2"},
		},
	}

	h1, err := specHash(spec)
	require.NoError(t, err)
	h2, err := specHash(spec)
	require.NoError(t, err)

	assert.Equal(t, h1, h2, "specHash must be stable for identical env slices")
}

func TestSpecHash_Format(t *testing.T) {
	spec := agentsv1alpha1.AgentDeploymentSpec{
		Image: "example",
		Port:  8080,
	}
	h, err := specHash(spec)
	require.NoError(t, err)
	assert.Len(t, h, 8)
	// Must be valid lowercase hex
	for _, c := range h {
		assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
			"specHash char %q must be lowercase hex", c)
	}
}

// ── Combined revision-name digest (m5.5 review fix) ─────────────────────────
//
// The revision name is "<name>-<specHash8>" plus, when ANY binding resolves,
// ONE combined "-h<digest8>" suffix (never stacked per-type suffixes — the
// 63-char DNS-1035 budget). These tests pin the combined digest's contract.

// digestTools builds a real tool-component digest via the M4 derivation.
func digestTools() string {
	return toolmanifest.StructuralDigest([]toolmanifest.SidecarTool{
		{BindingName: "bind-a", ToolName: "echo", Image: "dev.local/echo:1", Port: 3001},
	}, true)
}

// TestCombinedBindingDigest_PresenceCombinations: the empty (no structural input
// of any type) case yields the empty digest, and every distinct single-component
// presence yields a distinct non-empty digest. The "b=<x>;m=<y>;r=<z>" framing
// with hex-only components makes cross-presence collisions impossible.
func TestCombinedBindingDigest_PresenceCombinations(t *testing.T) {
	toolD := digestTools()
	memD := memoryBindingDigest(true, memoryDefaultAddr)
	regD := registryMembershipDigest(registryMembership{IsMember: true, RegistryID: "team", MaxDepth: 8, HopBudget: 32}, "worker", nil)
	require.NotEmpty(t, toolD)
	require.NotEmpty(t, memD)
	require.NotEmpty(t, regD)

	budD := budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", SoftThresholdPct: 80})
	require.NotEmpty(t, budD)

	neither := combinedBindingDigest("", "", "", "")
	toolsOnly := combinedBindingDigest(toolD, "", "", "")
	memOnly := combinedBindingDigest("", memD, "", "")
	regOnly := combinedBindingDigest("", "", regD, "")
	budgetOnly := combinedBindingDigest("", "", "", budD)
	all := combinedBindingDigest(toolD, memD, regD, budD)

	assert.Equal(t, "", neither, "no structural input of any type → empty digest (bare revision name)")
	assert.NotEmpty(t, toolsOnly)
	assert.NotEmpty(t, memOnly)
	assert.NotEmpty(t, regOnly)
	assert.NotEmpty(t, budgetOnly)
	assert.NotEmpty(t, all)
	// All five non-empty outcomes must be mutually distinct.
	distinct := map[string]bool{toolsOnly: true, memOnly: true, regOnly: true, budgetOnly: true, all: true}
	assert.Len(t, distinct, 5, "tools-only / memory-only / registry-only / budget-only / all must all differ")
	assert.Len(t, all, 8, "combined digest is 8 hex chars — the bounded suffix budget")
}

// TestCombinedBindingDigest_EitherComponentFlips: changing ANY component (tool
// set, memory addr, or registry membership) must flip the combined digest —
// otherwise the CreateOrUpdate revision-name guard would silently skip the
// re-apply.
func TestCombinedBindingDigest_EitherComponentFlips(t *testing.T) {
	toolD1 := digestTools()
	toolD2 := toolmanifest.StructuralDigest([]toolmanifest.SidecarTool{
		{BindingName: "bind-a", ToolName: "echo", Image: "dev.local/echo:2", Port: 3001},
	}, true)
	require.NotEqual(t, toolD1, toolD2, "precondition: image change flips the tool digest")

	memD1 := memoryBindingDigest(true, memoryDefaultAddr)
	memD2 := memoryBindingDigest(true, "other-valkey.ns.svc:6380")
	require.NotEqual(t, memD1, memD2, "precondition: addr change flips the memory digest")

	regD1 := registryMembershipDigest(registryMembership{IsMember: true, RegistryID: "team", MaxDepth: 8, HopBudget: 32}, "worker", nil)
	regD2 := registryMembershipDigest(registryMembership{IsMember: true, RegistryID: "team", MaxDepth: 8, HopBudget: 32}, "orchestrator", nil)
	require.NotEqual(t, regD1, regD2, "precondition: role change flips the registry digest")

	budD1 := budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", SoftThresholdPct: 80})
	budD2 := budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "1.00", SoftThresholdPct: 80})
	require.NotEqual(t, budD1, budD2, "precondition: cap change flips the budget digest")

	base := combinedBindingDigest(toolD1, memD1, regD1, budD1)
	assert.NotEqual(t, base, combinedBindingDigest(toolD2, memD1, regD1, budD1),
		"tool component change must flip the combined digest")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD2, regD1, budD1),
		"memory component change must flip the combined digest")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD1, regD2, budD1),
		"registry component change must flip the combined digest")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD1, regD1, budD2),
		"budget component change must flip the combined digest")
}

// TestCombinedBindingDigest_Deterministic: identical inputs always produce the
// identical digest (fixed field order; component derivations are themselves
// deterministic — tool digest sorts by binding name, memory digest hashes the
// resolved addr, registry digest hashes the resolved membership).
func TestCombinedBindingDigest_Deterministic(t *testing.T) {
	toolD := digestTools()
	memD := memoryBindingDigest(true, memoryDefaultAddr)
	regD := registryMembershipDigest(registryMembership{IsMember: true, RegistryID: "team", MaxDepth: 8, HopBudget: 32}, "worker", nil)

	budD := budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", SoftThresholdPct: 80})

	assert.Equal(t, combinedBindingDigest(toolD, memD, regD, budD), combinedBindingDigest(toolD, memD, regD, budD))
	assert.Equal(t, combinedBindingDigest(toolD, "", "", ""), combinedBindingDigest(toolD, "", "", ""))
	assert.Equal(t, combinedBindingDigest("", memD, "", ""), combinedBindingDigest("", memD, "", ""))
	assert.Equal(t, combinedBindingDigest("", "", regD, ""), combinedBindingDigest("", "", regD, ""))
	assert.Equal(t, combinedBindingDigest("", "", "", budD), combinedBindingDigest("", "", "", budD))
}

// TestRegistryMembershipDigest_Component pins the registry component's own
// contract: non-member → empty; membership fields (id/depth/budget/role/callers)
// each flip it; deterministic.
func TestRegistryMembershipDigest_Component(t *testing.T) {
	assert.Equal(t, "", registryMembershipDigest(registryMembership{}, "worker", []string{"a"}),
		"non-member → empty component (IsMember is the gate)")

	base := registryMembership{IsMember: true, RegistryID: "team", MaxDepth: 8, HopBudget: 32}
	d := registryMembershipDigest(base, "worker", []string{"a"})
	require.NotEmpty(t, d)
	assert.Equal(t, d, registryMembershipDigest(base, "worker", []string{"a"}), "deterministic")

	idChanged := base
	idChanged.RegistryID = "team-2"
	assert.NotEqual(t, d, registryMembershipDigest(idChanged, "worker", []string{"a"}), "registryId flip")

	depthChanged := base
	depthChanged.MaxDepth = 3
	assert.NotEqual(t, d, registryMembershipDigest(depthChanged, "worker", []string{"a"}), "maxDepth flip")

	budgetChanged := base
	budgetChanged.HopBudget = 5
	assert.NotEqual(t, d, registryMembershipDigest(budgetChanged, "worker", []string{"a"}), "hopBudget flip")

	assert.NotEqual(t, d, registryMembershipDigest(base, "orchestrator", []string{"a"}), "role flip")
	assert.NotEqual(t, d, registryMembershipDigest(base, "worker", []string{"a", "b"}), "allowedCallers flip")
}

// TestMemoryBindingDigest_Component pins the memory component's own contract:
// no binding → empty; addr changes flip it; deterministic.
func TestMemoryBindingDigest_Component(t *testing.T) {
	assert.Equal(t, "", memoryBindingDigest(false, ""), "no binding → empty component")
	assert.Equal(t, "", memoryBindingDigest(false, memoryDefaultAddr),
		"hasBinding is the gate — addr alone must not produce a digest")

	d1 := memoryBindingDigest(true, memoryDefaultAddr)
	d2 := memoryBindingDigest(true, "my-valkey.ns.svc:6380")
	assert.NotEmpty(t, d1)
	assert.NotEqual(t, d1, d2, "different addrs → different components")
	assert.Equal(t, d1, memoryBindingDigest(true, memoryDefaultAddr), "deterministic")
}

// TestBudgetDigest_Component pins the M8 budget component's own contract:
// nil budget → empty; each budget field (conv cap / agent cap / soft pct) flips
// it; deterministic; the soft-pct default (80) is applied so an explicit 80 and
// an omitted value hash the same (they inject the same env).
func TestBudgetDigest_Component(t *testing.T) {
	assert.Equal(t, "", budgetDigest(nil), "nil budget → empty component")

	base := &agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", PerAgentUSD: "10.00", SoftThresholdPct: 80}
	d := budgetDigest(base)
	require.NotEmpty(t, d)
	assert.Len(t, d, 8, "budget digest is 8 hex chars")
	assert.Equal(t, d, budgetDigest(base), "deterministic")

	assert.NotEqual(t, d, budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "1.00", PerAgentUSD: "10.00", SoftThresholdPct: 80}),
		"conversation cap flip")
	assert.NotEqual(t, d, budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", PerAgentUSD: "20.00", SoftThresholdPct: 80}),
		"agent cap flip")
	assert.NotEqual(t, d, budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", PerAgentUSD: "10.00", SoftThresholdPct: 90}),
		"soft pct flip")

	// Soft-pct default: an omitted (0) value hashes identically to an explicit 80.
	assert.Equal(t,
		budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50"}),
		budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", SoftThresholdPct: 80}),
		"omitted soft pct defaults to 80")
}
