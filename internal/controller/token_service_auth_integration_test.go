//go:build integration

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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
	"github.com/ctxmesh/ctxmesh/internal/toolmanifest"
)

// tokenServiceEgress is the always-on sidecar's config with a token service to delegate to.
func tokenServiceEgress(url string) OBOEgressConfig {
	cfg := m82EgressConfig
	cfg.TokenServiceURL = url
	return cfg
}

// The egress sidecar's token for the token service (ADR 0154) is mounted into the sidecar and into no
// other container, so the agent's code cannot read it; the agent gets no token of its own without
// memory or knowledge; and the mount renames the revision so it reaches a running pod.
func TestTheSidecarTokenIsMountedOnlyIntoTheSidecar(t *testing.T) {
	const ns = "default"
	const url = "http://mcp-open.default.svc.cluster.local/mcp"
	agent := mkAgent(t, "ts-auth-sidecar", ns)
	mkRegistry(t, "ts-auth-reg", ns,
		agentsv1beta1.ToolEntry{Name: "remote_tool", URL: url},
		agentsv1beta1.ToolEntry{Name: "echo_tool", Image: "ghcr.io/ctxmesh/echo-tool:latest"},
	)
	remote := mkBinding(t, "ts-auth-remote", ns, agentsv1beta1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "ts-auth-reg", ToolName: "remote_tool",
		Mode: toolmanifest.ModeRemote, Server: agentsv1beta1.ToolServer{URL: url},
	})
	inPod := mkBinding(t, "ts-auth-echo", ns, agentsv1beta1.MCPToolBindingSpec{
		AgentRef: agent.Name, RegistryRef: "ts-auth-reg", ToolName: "echo_tool",
		Mode: toolmanifest.ModeSidecar, Server: agentsv1beta1.ToolServer{Image: "ghcr.io/ctxmesh/echo-tool:latest"},
	})

	r := newReconciler()
	r.OBOEgress = tokenServiceEgress("http://token-service")
	reconcileNN(t, r, agent.Name, ns)
	br := newBindingReconciler()
	br.OBOEgress = r.OBOEgress
	reconcileBinding(t, br, remote.Name, ns)
	reconcileBinding(t, br, inPod.Name, ns)

	ksvc := getKsvc(t, agent.Name, ns)
	spec := ksvc.Spec.Template.Spec
	vol, ok := volumeByName(spec.Volumes, tokenServiceSidecarTokenVolume)
	require.True(t, ok, "the pod projects the sidecar's token")
	require.NotNil(t, vol.Projected)
	sa := vol.Projected.Sources[0].ServiceAccountToken
	require.NotNil(t, sa)
	assert.Equal(t, runcap.TokenServicePodAudience, sa.Audience)
	require.NotNil(t, sa.ExpirationSeconds)

	sidecar, ok := containerByName(spec.Containers, egressSidecarContainerName)
	require.True(t, ok)
	var mounted bool
	for _, m := range sidecar.VolumeMounts {
		if m.Name == tokenServiceSidecarTokenVolume {
			mounted = true
			assert.True(t, m.ReadOnly)
			assert.Equal(t, tokenServiceSidecarTokenMountPath, m.MountPath)
		}
	}
	assert.True(t, mounted, "the sidecar mounts its token")
	assert.Contains(t, sidecar.Env, corev1.EnvVar{Name: envTokenServiceTokenPath, Value: tokenServiceSidecarTokenMountPath + "/token"})

	require.Greater(t, len(spec.Containers), 3, "the agent, the collector, the in-pod tool and the sidecar")
	for _, c := range spec.Containers {
		if c.Name == egressSidecarContainerName {
			continue
		}
		for _, m := range c.VolumeMounts {
			assert.NotEqual(t, tokenServiceSidecarTokenVolume, m.Name, "%s mounts the sidecar's token", c.Name)
		}
		for _, e := range c.Env {
			assert.NotEqual(t, envTokenServiceTokenPath, e.Name, "%s is pointed at a token-service token", c.Name)
		}
	}
	_, agentToken := volumeByName(spec.Volumes, tokenServiceAgentTokenVolume)
	assert.False(t, agentToken, "an agent without memory or knowledge carries no agent token")
	assert.False(t, spec.ShareProcessNamespace != nil && *spec.ShareProcessNamespace,
		"no container may read another's mounts through a shared process namespace")

	// The same agent, rendered as if the fold did not exist, is the superseded name of this pod.
	pod, err := r.buildPodTemplate(testCtx, agent)
	require.NoError(t, err)
	require.NotEmpty(t, pod.supersededDigests)
	assert.Equal(t, pod.digest, foldDigest(pod.supersededDigests[len(pod.supersededDigests)-1],
		tokenServiceTokenFold(true, false)), "the sidecar token renames the revision")
	hash, err := specHash(agent.Spec)
	require.NoError(t, err)
	assert.Equal(t, revisionName(agent.Name, hash, pod.digest), ksvc.Spec.Template.Name,
		"the served revision carries the folded name")
}
