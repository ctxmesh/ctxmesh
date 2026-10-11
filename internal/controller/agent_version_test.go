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

// Unit tests for the AGENT_VERSION every agent pod carries (no build tag — runs in make test / tier0).
package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

func podTemplateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, agentsv1beta1.AddToScheme(s))
	require.NoError(t, servingv1.AddToScheme(s))
	return s
}

func agentVersionEnv(t *testing.T, pod podTemplate) string {
	t.Helper()
	require.NotEmpty(t, pod.containers)
	var values []string
	for _, e := range pod.containers[0].Env {
		if e.Name == envAgentVersion {
			assert.Nil(t, e.ValueFrom, "AGENT_VERSION must be static (Knative refuses valueFrom here)")
			values = append(values, e.Value)
		}
	}
	require.Len(t, values, 1, "the user container carries AGENT_VERSION exactly once")
	return values[0]
}

// A canary serves two revisions at once, and the detector can only tell their scores apart if each
// revision's pod names the version it was built from. Both arms are built by buildPodTemplate from
// their own spec; this proves each carries its own version, and that it is the name the revision
// and status.latestVersion use.
func TestEachCanaryArmCarriesItsOwnVersion(t *testing.T) {
	t.Parallel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	r := &AgentDeploymentReconciler{
		Client: fake.NewClientBuilder().WithScheme(podTemplateScheme(t)).WithObjects(ns).Build(),
		Scheme: podTemplateScheme(t),
	}
	arm := func(port int32) (version, revision string) {
		deploy := &agentsv1beta1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "canary", Namespace: "team-a"},
			Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest", Port: port},
		}
		pod, err := r.buildPodTemplate(context.Background(), deploy)
		require.NoError(t, err)
		hash, err := specHash(deploy.Spec)
		require.NoError(t, err)
		require.NotEmpty(t, pod.digest, "every revision name carries the -h suffix")
		return agentVersionEnv(t, pod), agentVersionName(deploy.Name, hash) + "-h" + pod.digest
	}

	oldVersion, oldRev := arm(8080)
	candidateVersion, candidateRev := arm(8090)

	assert.NotEqual(t, oldVersion, candidateVersion, "two specs are two versions, and each arm must say which it is")
	assert.True(t, strings.HasPrefix(oldRev, oldVersion+"-h"), "the old arm %q serves %q", oldRev, oldVersion)
	assert.True(t, strings.HasPrefix(candidateRev, candidateVersion+"-h"),
		"the candidate %q serves %q", candidateRev, candidateVersion)
}

// No spec can carry its own hash, so an operator's AGENT_VERSION opts the agent out of per-version
// scoring. It still wins, like every platform env var, and is never duplicated.
func TestAnOperatorsAgentVersionWins(t *testing.T) {
	t.Parallel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	r := &AgentDeploymentReconciler{
		Client: fake.NewClientBuilder().WithScheme(podTemplateScheme(t)).WithObjects(ns).Build(),
		Scheme: podTemplateScheme(t),
	}
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "pinned", Namespace: "team-a"},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Env:   []corev1.EnvVar{{Name: envAgentVersion, Value: "1.2.3"}},
		},
	}
	pod, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", agentVersionEnv(t, pod))
}

// Adding AGENT_VERSION changes every agent's pod template, and a template change under an unchanged
// revision name is dropped by the controller and refused by Knative. The contract bump must therefore
// move every revision name, including an agent whose other digest inputs are all empty.
func TestTheAgentVersionContractRollsEveryAgentOnce(t *testing.T) {
	t.Parallel()

	for _, base := range []string{"", combinedBindingDigest("", "", "", "", "", "", universalIdentitySADigest(false), "", "", "")} {
		for _, exporting := range []bool{false, true} {
			now := traceExportFold(base, exporting)
			assert.Len(t, now, 8, "the -h suffix is never empty")
			assert.NotEqual(t, podContractFold(base, "v4", exporting), now,
				"an agent revisioned before AGENT_VERSION (digest %q, export %v) must roll", base, exporting)
		}
	}
	// An unconfined agent skips the hardening fold, so the contract fold is all that moves it.
	assert.NotEqual(t, podContractFold(hardeningFold("", true), "v4", false), traceExportFold(hardeningFold("", true), false))
}
