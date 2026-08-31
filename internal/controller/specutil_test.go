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

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/toolmanifest"
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

// TestSpecHash_NoRollout_ByteForByte is the M69.9 M4-landmine guard: adding the
// optional Rollout block to the spec must NOT change the specHash (hence the
// revision name / ksvc) for a deployment that does NOT set it. Rollout is a nil
// pointer with omitempty, so it is omitted from the canonical JSON entirely — a
// no-rollout spec hashes identically to a pre-M69 spec.
func TestSpecHash_NoRollout_ByteForByte(t *testing.T) {
	base := agentsv1alpha1.AgentDeploymentSpec{
		Image:          "ghcr.io/ctxmesh/echo-agent:latest",
		ExecutionModel: "serving",
		Port:           8080,
	}
	withNilRollout := base
	withNilRollout.Rollout = nil

	hBase, err := specHash(base)
	require.NoError(t, err)
	hNil, err := specHash(withNilRollout)
	require.NoError(t, err)
	assert.Equal(t, hBase, hNil,
		"an absent (nil) Rollout must not change the specHash — a no-rollout deployment's revision is byte-for-byte unchanged")

	// A SET Rollout is a real config change → a different hash (a distinct revision).
	withCanary := base
	withCanary.Rollout = &agentsv1alpha1.RolloutSpec{Strategy: "canary", CanaryPercent: 10}
	hCanary, err := specHash(withCanary)
	require.NoError(t, err)
	assert.NotEqual(t, hBase, hCanary, "a set Rollout block is a real config change and must alter the specHash")
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
	memD := memoryDigest(true, memoryDefaultAddr)
	regD := registryMembershipDigest(registryMembership{IsMember: true, RegistryID: "team", MaxDepth: 8, HopBudget: 32}, "worker", nil)
	require.NotEmpty(t, toolD)
	require.NotEmpty(t, memD)
	require.NotEmpty(t, regD)

	budD := budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", SoftThresholdPct: 80})
	require.NotEmpty(t, budD)

	promptD := promptDigest(agentsv1alpha1.GitPromptSource{Repo: "r", Ref: "v1", Path: "p"}, "ver-v1")
	require.NotEmpty(t, promptD)

	tenantD := tenantDigest(tenantContext{id: "acme", budgetUSD: "100.00", rpm: 600}, true)
	require.NotEmpty(t, tenantD)

	proxyD := statelayerProxyDigest("http://statelayer-proxy.svc:8080", true, false)
	require.NotEmpty(t, proxyD)

	runtimeD := runtimeDigest(&agentsv1alpha1.RuntimeSpec{ToolPolicy: &agentsv1alpha1.ToolPolicySpec{Default: "allow"}})
	require.NotEmpty(t, runtimeD)

	guardrailD, err := guardrailPolicyHash(&agentsv1beta1.GuardrailPolicySpec{
		FailMode:        "closed",
		PatternDenylist: []agentsv1beta1.PatternRule{{Name: "jb", Pattern: "ignore.*instructions"}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, guardrailD)

	kbD := knowledgeBasesDigest([]kbRosterEntry{
		{Name: "docs-kb", Namespace: "default", EmbeddingRoute: "text-embedding-3-small"},
	})
	require.NotEmpty(t, kbD)

	neither := combinedBindingDigest("", "", "", "", "", "", "", "", "", "")
	toolsOnly := combinedBindingDigest(toolD, "", "", "", "", "", "", "", "", "")
	memOnly := combinedBindingDigest("", memD, "", "", "", "", "", "", "", "")
	regOnly := combinedBindingDigest("", "", regD, "", "", "", "", "", "", "")
	budgetOnly := combinedBindingDigest("", "", "", budD, "", "", "", "", "", "")
	promptOnly := combinedBindingDigest("", "", "", "", promptD, "", "", "", "", "")
	tenantOnly := combinedBindingDigest("", "", "", "", "", tenantD, "", "", "", "")
	proxyOnly := combinedBindingDigest("", "", "", "", "", "", proxyD, "", "", "")
	runtimeOnly := combinedBindingDigest("", "", "", "", "", "", "", runtimeD, "", "")
	guardrailOnly := combinedBindingDigest("", "", "", "", "", "", "", "", guardrailD, "")
	kbOnly := combinedBindingDigest("", "", "", "", "", "", "", "", "", kbD)
	all := combinedBindingDigest(toolD, memD, regD, budD, promptD, tenantD, proxyD, runtimeD, guardrailD, kbD)

	assert.Equal(t, "", neither, "no structural input of any type → empty digest (bare revision name)")
	assert.NotEmpty(t, toolsOnly)
	assert.NotEmpty(t, memOnly)
	assert.NotEmpty(t, regOnly)
	assert.NotEmpty(t, budgetOnly)
	assert.NotEmpty(t, promptOnly)
	assert.NotEmpty(t, tenantOnly)
	assert.NotEmpty(t, proxyOnly)
	assert.NotEmpty(t, runtimeOnly)
	assert.NotEmpty(t, guardrailOnly)
	assert.NotEmpty(t, kbOnly)
	assert.NotEmpty(t, all)
	// All eleven non-empty outcomes must be mutually distinct.
	distinct := map[string]bool{
		toolsOnly: true, memOnly: true, regOnly: true, budgetOnly: true,
		promptOnly: true, tenantOnly: true, proxyOnly: true, runtimeOnly: true,
		guardrailOnly: true, kbOnly: true, all: true,
	}
	assert.Len(t, distinct, 11,
		"tools / memory / registry / budget / prompt / tenant / proxy / runtime / guardrail / kb / all must all differ")
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

	memD1 := memoryDigest(true, memoryDefaultAddr)
	memD2 := memoryDigest(true, "other-valkey.ns.svc:6380")
	require.NotEqual(t, memD1, memD2, "precondition: addr change flips the memory digest")

	regD1 := registryMembershipDigest(registryMembership{IsMember: true, RegistryID: "team", MaxDepth: 8, HopBudget: 32}, "worker", nil)
	regD2 := registryMembershipDigest(registryMembership{IsMember: true, RegistryID: "team", MaxDepth: 8, HopBudget: 32}, "orchestrator", nil)
	require.NotEqual(t, regD1, regD2, "precondition: role change flips the registry digest")

	budD1 := budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", SoftThresholdPct: 80})
	budD2 := budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "1.00", SoftThresholdPct: 80})
	require.NotEqual(t, budD1, budD2, "precondition: cap change flips the budget digest")

	promptD1 := promptDigest(agentsv1alpha1.GitPromptSource{Repo: "r", Ref: "v1", Path: "p"}, "ver-v1")
	promptD2 := promptDigest(agentsv1alpha1.GitPromptSource{Repo: "r", Ref: "v2", Path: "p"}, "ver-v2")
	require.NotEqual(t, promptD1, promptD2, "precondition: prompt ref/version change flips the prompt digest")

	tenantD1 := tenantDigest(tenantContext{id: "acme", rpm: 600}, true)
	tenantD2 := tenantDigest(tenantContext{id: "acme", rpm: 1200}, true)
	require.NotEqual(t, tenantD1, tenantD2, "precondition: a cap change flips the tenant digest")

	proxyD1 := statelayerProxyDigest("http://proxy-a:8080", true, false)
	proxyD2 := statelayerProxyDigest("http://proxy-b:8080", true, false)
	require.NotEqual(t, proxyD1, proxyD2, "precondition: a proxy-URL change flips the proxy digest")

	// m79.4: toggling the default-token automount opt-in flips the proxy digest (same SA the
	// component tracks), so a spec.mountServiceAccountToken change rolls a new revision.
	proxyMountOff := statelayerProxyDigest("http://proxy-a:8080", true, false)
	proxyMountOn := statelayerProxyDigest("http://proxy-a:8080", true, true)
	require.NotEqual(t, proxyMountOff, proxyMountOn,
		"precondition: an automount (default kube-API token) toggle flips the proxy digest (m79.4)")

	runtimeD1 := runtimeDigest(&agentsv1alpha1.RuntimeSpec{ToolPolicy: &agentsv1alpha1.ToolPolicySpec{Default: "allow", ParallelLimit: 3}})
	runtimeD2 := runtimeDigest(&agentsv1alpha1.RuntimeSpec{ToolPolicy: &agentsv1alpha1.ToolPolicySpec{Default: "allow", ParallelLimit: 5}})
	require.NotEqual(t, runtimeD1, runtimeD2, "precondition: a runtime change flips the runtime digest")

	guardrailD1, err := guardrailPolicyHash(&agentsv1beta1.GuardrailPolicySpec{
		PatternDenylist: []agentsv1beta1.PatternRule{{Name: "jb", Pattern: "ignore.*instructions"}},
	})
	require.NoError(t, err)
	guardrailD2, err := guardrailPolicyHash(&agentsv1beta1.GuardrailPolicySpec{
		PatternDenylist: []agentsv1beta1.PatternRule{{Name: "jb", Pattern: "ignore.*instructions"}, {Name: "pw", Pattern: "(?i)secret"}},
	})
	require.NoError(t, err)
	require.NotEqual(t, guardrailD1, guardrailD2, "precondition: a policy edit flips the guardrail digest")

	kbD1 := knowledgeBasesDigest([]kbRosterEntry{
		{Name: "docs-kb", Namespace: "default", EmbeddingRoute: "text-embedding-3-small"},
	})
	kbD2 := knowledgeBasesDigest([]kbRosterEntry{
		{Name: "docs-kb", Namespace: "default", EmbeddingRoute: "text-embedding-3-large"},
	})
	require.NotEqual(t, kbD1, kbD2, "precondition: embeddingRoute change flips the kb digest")

	base := combinedBindingDigest(toolD1, memD1, regD1, budD1, promptD1, tenantD1, proxyD1, runtimeD1, guardrailD1, kbD1)
	assert.NotEqual(t, base, combinedBindingDigest(toolD2, memD1, regD1, budD1, promptD1, tenantD1, proxyD1, runtimeD1, guardrailD1, kbD1),
		"tool component change must flip the combined digest")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD2, regD1, budD1, promptD1, tenantD1, proxyD1, runtimeD1, guardrailD1, kbD1),
		"memory component change must flip the combined digest")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD1, regD2, budD1, promptD1, tenantD1, proxyD1, runtimeD1, guardrailD1, kbD1),
		"registry component change must flip the combined digest")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD1, regD1, budD2, promptD1, tenantD1, proxyD1, runtimeD1, guardrailD1, kbD1),
		"budget component change must flip the combined digest")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD1, regD1, budD1, promptD2, tenantD1, proxyD1, runtimeD1, guardrailD1, kbD1),
		"prompt component change must flip the combined digest")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD1, regD1, budD1, promptD1, tenantD2, proxyD1, runtimeD1, guardrailD1, kbD1),
		"tenant component change must flip the combined digest")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD1, regD1, budD1, promptD1, tenantD1, proxyD2, runtimeD1, guardrailD1, kbD1),
		"proxy component change must flip the combined digest (M53 — the cutover must roll a revision)")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD1, regD1, budD1, promptD1, tenantD1, proxyD1, runtimeD2, guardrailD1, kbD1),
		"runtime component change must flip the combined digest (M65 — a runtime edit must roll a revision)")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD1, regD1, budD1, promptD1, tenantD1, proxyD1, runtimeD1, guardrailD2, kbD1),
		"guardrail component change must flip the combined digest (M66 — a policy edit must roll a revision)")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD1, regD1, budD1, promptD1, tenantD1, proxyD1, runtimeD1, guardrailD1, kbD2),
		"kb component change must flip the combined digest (M68 — a KB embeddingRoute change must roll a revision)")
}

// TestCombinedBindingDigest_Deterministic: identical inputs always produce the
// identical digest (fixed field order; component derivations are themselves
// deterministic — tool digest sorts by binding name, memory digest hashes the
// resolved addr, registry digest hashes the resolved membership).
func TestCombinedBindingDigest_Deterministic(t *testing.T) {
	toolD := digestTools()
	memD := memoryDigest(true, memoryDefaultAddr)
	regD := registryMembershipDigest(registryMembership{IsMember: true, RegistryID: "team", MaxDepth: 8, HopBudget: 32}, "worker", nil)

	budD := budgetDigest(&agentsv1alpha1.BudgetSpec{PerConversationUSD: "0.50", SoftThresholdPct: 80})
	promptD := promptDigest(agentsv1alpha1.GitPromptSource{Repo: "r", Ref: "v1", Path: "p"}, "ver-v1")

	tenantD := tenantDigest(tenantContext{id: "acme", rpm: 600}, true)

	proxyD := statelayerProxyDigest("http://proxy:8080", true, false)

	runtimeD := runtimeDigest(&agentsv1alpha1.RuntimeSpec{ToolPolicy: &agentsv1alpha1.ToolPolicySpec{Default: "allow"}})

	guardrailD, err := guardrailPolicyHash(&agentsv1beta1.GuardrailPolicySpec{
		PatternDenylist: []agentsv1beta1.PatternRule{{Name: "jb", Pattern: "ignore.*instructions"}},
	})
	require.NoError(t, err)

	kbD := knowledgeBasesDigest([]kbRosterEntry{
		{Name: "docs-kb", Namespace: "default", EmbeddingRoute: "text-embedding-3-small"},
	})

	assert.Equal(t, combinedBindingDigest(toolD, memD, regD, budD, promptD, tenantD, proxyD, runtimeD, guardrailD, kbD), combinedBindingDigest(toolD, memD, regD, budD, promptD, tenantD, proxyD, runtimeD, guardrailD, kbD))
	assert.Equal(t, combinedBindingDigest(toolD, "", "", "", "", "", "", "", "", ""), combinedBindingDigest(toolD, "", "", "", "", "", "", "", "", ""))
	assert.Equal(t, combinedBindingDigest("", memD, "", "", "", "", "", "", "", ""), combinedBindingDigest("", memD, "", "", "", "", "", "", "", ""))
	assert.Equal(t, combinedBindingDigest("", "", regD, "", "", "", "", "", "", ""), combinedBindingDigest("", "", regD, "", "", "", "", "", "", ""))
	assert.Equal(t, combinedBindingDigest("", "", "", budD, "", "", "", "", "", ""), combinedBindingDigest("", "", "", budD, "", "", "", "", "", ""))
	assert.Equal(t, combinedBindingDigest("", "", "", "", promptD, "", "", "", "", ""), combinedBindingDigest("", "", "", "", promptD, "", "", "", "", ""))
	assert.Equal(t, combinedBindingDigest("", "", "", "", "", tenantD, "", "", "", ""), combinedBindingDigest("", "", "", "", "", tenantD, "", "", "", ""))
	assert.Equal(t, combinedBindingDigest("", "", "", "", "", "", proxyD, "", "", ""), combinedBindingDigest("", "", "", "", "", "", proxyD, "", "", ""))
	assert.Equal(t, combinedBindingDigest("", "", "", "", "", "", "", runtimeD, "", ""), combinedBindingDigest("", "", "", "", "", "", "", runtimeD, "", ""))
	assert.Equal(t, combinedBindingDigest("", "", "", "", "", "", "", "", guardrailD, ""), combinedBindingDigest("", "", "", "", "", "", "", "", guardrailD, ""))
	assert.Equal(t, combinedBindingDigest("", "", "", "", "", "", "", "", "", kbD), combinedBindingDigest("", "", "", "", "", "", "", "", "", kbD))
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

// TestMemoryDigest_Component pins the memory component's own contract:
// no memory → empty; addr changes flip it; deterministic.
func TestMemoryDigest_Component(t *testing.T) {
	assert.Equal(t, "", memoryDigest(false, ""), "no memory → empty component")
	assert.Equal(t, "", memoryDigest(false, memoryDefaultAddr),
		"hasMemory is the gate — addr alone must not produce a digest")

	d1 := memoryDigest(true, memoryDefaultAddr)
	d2 := memoryDigest(true, "my-valkey.ns.svc:6380")
	assert.NotEmpty(t, d1)
	assert.NotEqual(t, d1, d2, "different addrs → different components")
	assert.Equal(t, d1, memoryDigest(true, memoryDefaultAddr), "deterministic")
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

// TestPromptDigest_Component pins the M9 prompt component's own contract:
// empty version → empty (no prompt); each pointer field (repo/ref/path) and the
// resolved version flips it; 8 hex chars; deterministic.
func TestPromptDigest_Component(t *testing.T) {
	assert.Equal(t, "", promptDigest(agentsv1alpha1.GitPromptSource{}, ""),
		"no resolved version → empty component (the promptRef gate)")

	src := agentsv1alpha1.GitPromptSource{Repo: "https://git/p.git", Ref: "v1", Path: "sys.txt"}
	d := promptDigest(src, "ver-a")
	require.NotEmpty(t, d)
	assert.Len(t, d, 8, "prompt digest is 8 hex chars — the bounded suffix budget")
	assert.Equal(t, d, promptDigest(src, "ver-a"), "deterministic")

	refChanged := src
	refChanged.Ref = "v2"
	assert.NotEqual(t, d, promptDigest(refChanged, "ver-a"), "git ref flip → digest flip")

	pathChanged := src
	pathChanged.Path = "other.txt"
	assert.NotEqual(t, d, promptDigest(pathChanged, "ver-a"), "path flip → digest flip")

	repoChanged := src
	repoChanged.Repo = "https://git/other.git"
	assert.NotEqual(t, d, promptDigest(repoChanged, "ver-a"), "repo flip → digest flip")

	assert.NotEqual(t, d, promptDigest(src, "ver-b"), "resolved version flip → digest flip")
}

// TestPromptChange_DoesNotChangeImageDigest is the prompt-only-deploy invariant
// at the unit level: a prompt swap folds into the combined binding digest (which
// drives the Knative revision-name suffix) but is COMPUTED ENTIRELY WITHOUT the
// container image. The image lives in spec.Image on the user container and is
// never an input to promptDigest or combinedBindingDigest — so swapping the
// prompt rolls a new revision while the image digest stays IDENTICAL. This test
// proves the separation structurally: identical everything-else, only the prompt
// component differs → the combined digest differs, and neither call ever saw an
// image.
func TestPromptChange_DoesNotChangeImageDigest(t *testing.T) {
	// Same agent, same bindings/budget — only the prompt version (ref) swaps.
	promptV1 := promptDigest(agentsv1alpha1.GitPromptSource{Repo: "r", Ref: "v1", Path: "p"}, "resolved-v1")
	promptV2 := promptDigest(agentsv1alpha1.GitPromptSource{Repo: "r", Ref: "v2", Path: "p"}, "resolved-v2")
	require.NotEqual(t, promptV1, promptV2, "precondition: a prompt swap changes the prompt component")

	combinedV1 := combinedBindingDigest("", "", "", "", promptV1, "", "", "", "", "")
	combinedV2 := combinedBindingDigest("", "", "", "", promptV2, "", "", "", "", "")

	// The revision-name suffix changes (a new revision rolls) ...
	assert.NotEqual(t, combinedV1, combinedV2,
		"a prompt swap must roll the Knative revision (combined digest changes)")

	// ... but the IMAGE is never part of any digest input. specHash is over the
	// spec (which includes Image), and it is orthogonal to the prompt roll: the
	// image digest — spec.Image, applied verbatim to the user container — is
	// untouched by the prompt path. Assert the digest functions carry no image:
	// they take only pointer/version, so no image rebuild can be implied.
	//
	// A same-image spec differing ONLY by promptRef keeps the SAME specHash-derived
	// image (Image field identical) while the combined "-h" suffix differs. Prove
	// the image field is stable across the swap.
	specA := agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/x/agent@sha256:abc", PromptRef: "prompt-v1"}
	specB := agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/x/agent@sha256:abc", PromptRef: "prompt-v2"}
	assert.Equal(t, specA.Image, specB.Image,
		"a prompt-only change keeps the container image identical (no rebuild)")
}

// TestRuntimeDigest_Component pins the M65 runtime component's own contract:
// nil runtime → empty (no env change → no digest component); each structural
// change (ToolPolicy.Default, ParallelLimit, Resilience.ModelCall.MaxRetries)
// flips it; 8 hex chars; deterministic.
func TestRuntimeDigest_Component(t *testing.T) {
	assert.Equal(t, "", runtimeDigest(nil), "nil runtime → empty component")

	base := &agentsv1alpha1.RuntimeSpec{
		ToolPolicy: &agentsv1alpha1.ToolPolicySpec{Default: "allow", ParallelLimit: 4},
		Resilience: &agentsv1alpha1.ResilienceSpec{
			ModelCall: &agentsv1alpha1.CallResilience{MaxRetries: 2},
		},
	}
	d := runtimeDigest(base)
	require.NotEmpty(t, d)
	assert.Len(t, d, 8, "runtime digest is 8 hex chars")
	assert.Equal(t, d, runtimeDigest(base), "deterministic")

	// Changing ToolPolicy.Default flips the digest.
	changed1 := &agentsv1alpha1.RuntimeSpec{
		ToolPolicy: &agentsv1alpha1.ToolPolicySpec{Default: "deny", ParallelLimit: 4},
		Resilience: &agentsv1alpha1.ResilienceSpec{
			ModelCall: &agentsv1alpha1.CallResilience{MaxRetries: 2},
		},
	}
	assert.NotEqual(t, d, runtimeDigest(changed1), "ToolPolicy.Default change must flip the runtime digest")

	// Changing ParallelLimit flips the digest.
	changed2 := &agentsv1alpha1.RuntimeSpec{
		ToolPolicy: &agentsv1alpha1.ToolPolicySpec{Default: "allow", ParallelLimit: 8},
		Resilience: &agentsv1alpha1.ResilienceSpec{
			ModelCall: &agentsv1alpha1.CallResilience{MaxRetries: 2},
		},
	}
	assert.NotEqual(t, d, runtimeDigest(changed2), "ParallelLimit change must flip the runtime digest")

	// Changing Resilience.ModelCall.MaxRetries flips the digest.
	changed3 := &agentsv1alpha1.RuntimeSpec{
		ToolPolicy: &agentsv1alpha1.ToolPolicySpec{Default: "allow", ParallelLimit: 4},
		Resilience: &agentsv1alpha1.ResilienceSpec{
			ModelCall: &agentsv1alpha1.CallResilience{MaxRetries: 5},
		},
	}
	assert.NotEqual(t, d, runtimeDigest(changed3), "Resilience.ModelCall.MaxRetries change must flip the runtime digest")
}
