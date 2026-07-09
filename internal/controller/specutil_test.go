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

// TestCombinedBindingDigest_PresenceCombinations: neither / tools-only /
// memory-only / both must produce four DISTINCT outcomes (empty for neither,
// three distinct non-empty digests otherwise). The "b=<x>;m=<y>" framing with
// hex-only components makes cross-presence collisions impossible.
func TestCombinedBindingDigest_PresenceCombinations(t *testing.T) {
	toolD := digestTools()
	memD := memoryBindingDigest(true, memoryDefaultAddr)
	require.NotEmpty(t, toolD)
	require.NotEmpty(t, memD)

	neither := combinedBindingDigest("", "")
	toolsOnly := combinedBindingDigest(toolD, "")
	memOnly := combinedBindingDigest("", memD)
	both := combinedBindingDigest(toolD, memD)

	assert.Equal(t, "", neither, "no bindings of any type → empty digest (bare revision name)")
	assert.NotEmpty(t, toolsOnly)
	assert.NotEmpty(t, memOnly)
	assert.NotEmpty(t, both)
	assert.NotEqual(t, toolsOnly, memOnly, "tools-only vs memory-only must differ")
	assert.NotEqual(t, toolsOnly, both, "tools-only vs both must differ")
	assert.NotEqual(t, memOnly, both, "memory-only vs both must differ")
	assert.Len(t, both, 8, "combined digest is 8 hex chars — the bounded suffix budget")
}

// TestCombinedBindingDigest_EitherComponentFlips: changing EITHER component
// (tool set or memory addr) must flip the combined digest — otherwise the
// CreateOrUpdate revision-name guard would silently skip the re-apply.
func TestCombinedBindingDigest_EitherComponentFlips(t *testing.T) {
	toolD1 := digestTools()
	toolD2 := toolmanifest.StructuralDigest([]toolmanifest.SidecarTool{
		{BindingName: "bind-a", ToolName: "echo", Image: "dev.local/echo:2", Port: 3001},
	}, true)
	require.NotEqual(t, toolD1, toolD2, "precondition: image change flips the tool digest")

	memD1 := memoryBindingDigest(true, memoryDefaultAddr)
	memD2 := memoryBindingDigest(true, "other-valkey.ns.svc:6380")
	require.NotEqual(t, memD1, memD2, "precondition: addr change flips the memory digest")

	base := combinedBindingDigest(toolD1, memD1)
	assert.NotEqual(t, base, combinedBindingDigest(toolD2, memD1),
		"tool component change must flip the combined digest")
	assert.NotEqual(t, base, combinedBindingDigest(toolD1, memD2),
		"memory component change must flip the combined digest")
}

// TestCombinedBindingDigest_Deterministic: identical inputs always produce the
// identical digest (fixed field order; component derivations are themselves
// deterministic — tool digest sorts by binding name, memory digest hashes the
// resolved addr).
func TestCombinedBindingDigest_Deterministic(t *testing.T) {
	toolD := digestTools()
	memD := memoryBindingDigest(true, memoryDefaultAddr)

	assert.Equal(t, combinedBindingDigest(toolD, memD), combinedBindingDigest(toolD, memD))
	assert.Equal(t, combinedBindingDigest(toolD, ""), combinedBindingDigest(toolD, ""))
	assert.Equal(t, combinedBindingDigest("", memD), combinedBindingDigest("", memD))
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
