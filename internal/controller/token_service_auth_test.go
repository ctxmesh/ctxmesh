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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/credplane"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// The token service takes an agent's identity from its ServiceAccount's name (ADR 0154).
func TestTheTokenServiceKnowsAnAgentsServiceAccount(t *testing.T) {
	t.Parallel()
	assert.Equal(t, credplane.AgentServiceAccountPrefix+"helper", agentIdentitySAName("helper"))
}

// tokenServiceAgent is an agent in team-a with the given long-term memory and knowledge bases.
func tokenServiceAgent(name string, longTerm bool, kbs ...string) (*agentsv1beta1.AgentDeployment, []client.Object) {
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
		Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	if longTerm {
		deploy.Spec.LongTermMemory = &agentsv1beta1.LongTermMemorySpec{Enabled: true}
	}
	objs := make([]client.Object, 0, 1+len(kbs))
	objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}})
	for _, kb := range kbs {
		deploy.Spec.KnowledgeBases = append(deploy.Spec.KnowledgeBases, agentsv1beta1.KnowledgeBaseRef{Name: kb})
		objs = append(objs, &agentsv1beta1.KnowledgeBase{
			ObjectMeta: metav1.ObjectMeta{Name: kb, Namespace: "team-a"},
			Spec:       agentsv1beta1.KnowledgeBaseSpec{EmbeddingRoute: "embed"},
		})
	}
	return deploy, objs
}

func tokenServicePod(t *testing.T, deploy *agentsv1beta1.AgentDeployment, objs []client.Object, tokenServiceURL string) podTemplate {
	t.Helper()
	r := identityReconciler(t, deploy, objs...)
	r.OBOEgress.TokenServiceURL = tokenServiceURL
	pod, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	return pod
}

// An agent with long-term memory or knowledge bases carries the agent-audience token in its own
// container, and only there; an agent with neither carries none.
func TestTheAgentTokenIsMountedWhereTheLauncherCallsTheTokenService(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		longTerm bool
		kbs      []string
		want     bool
	}{
		"long-term memory": {longTerm: true, want: true},
		"knowledge bases":  {kbs: []string{"docs"}, want: true},
		"both":             {longTerm: true, kbs: []string{"docs"}, want: true},
		"neither":          {},
	} {
		t.Run(name, func(t *testing.T) {
			deploy, objs := tokenServiceAgent("agent", tc.longTerm, tc.kbs...)
			pod := tokenServicePod(t, deploy, objs, "http://token-service")

			vols := podVolume(pod, tokenServiceAgentTokenVolume)
			mounts := userMounts(pod, tokenServiceAgentTokenVolume)
			path := envValues(t, pod, envTokenServiceTokenPath)
			if !tc.want {
				assert.Empty(t, vols)
				assert.Empty(t, mounts)
				assert.Empty(t, path)
				return
			}
			require.Len(t, vols, 1)
			require.NotNil(t, vols[0].Projected)
			sa := vols[0].Projected.Sources[0].ServiceAccountToken
			require.NotNil(t, sa)
			assert.Equal(t, runcap.TokenServiceAgentAudience, sa.Audience)
			require.NotNil(t, sa.ExpirationSeconds)
			require.Len(t, mounts, 1)
			assert.True(t, mounts[0].ReadOnly)
			assert.Equal(t, []string{mounts[0].MountPath + "/token"}, path)
			assert.Empty(t, podVolume(pod, tokenServiceSidecarTokenVolume), "the sidecar's audience is never the agent's")
			for _, c := range pod.containers[1:] {
				for _, m := range c.VolumeMounts {
					assert.NotEqual(t, tokenServiceAgentTokenVolume, m.Name, "%s mounts the agent's token", c.Name)
				}
			}
		})
	}
}

// Without a token service the launcher is pointed at none, so there is nothing to authenticate to.
func TestNoAgentTokenWithoutATokenService(t *testing.T) {
	t.Parallel()
	deploy, objs := tokenServiceAgent("agent", true, "docs")
	pod := tokenServicePod(t, deploy, objs, "")
	assert.Empty(t, podVolume(pod, tokenServiceAgentTokenVolume))
	assert.Empty(t, envValues(t, pod, envTokenServiceTokenPath))
}

// The mount moves the revision name, or it never reaches a running pod; the name without it is the
// same pod renamed, so an approval carries.
func TestTheAgentTokenRenamesTheRevision(t *testing.T) {
	t.Parallel()
	deploy, objs := tokenServiceAgent("agent", true)
	with := tokenServicePod(t, deploy.DeepCopy(), objs, "http://token-service")

	require.NotEmpty(t, with.supersededDigests)
	assert.NotContains(t, with.supersededDigests, with.digest)

	withoutFold := with.supersededDigests[len(with.supersededDigests)-1]
	assert.Equal(t, with.digest, foldDigest(withoutFold, tokenServiceTokenFold(false, true)),
		"the revision is the pod without the token, renamed by the fold")
}

func TestTokenServiceTokenFold(t *testing.T) {
	t.Parallel()
	assert.Empty(t, tokenServiceTokenFold(false, false), "a pod with neither token keeps its name")
	seen := map[string]bool{}
	for _, f := range []string{tokenServiceTokenFold(true, false), tokenServiceTokenFold(false, true), tokenServiceTokenFold(true, true)} {
		assert.NotEmpty(t, f)
		assert.False(t, seen[f], "each combination renames differently")
		seen[f] = true
	}
}
