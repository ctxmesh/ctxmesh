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

// Unit tests for specHash (no build tag — runs in make test / tier0).
package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
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
