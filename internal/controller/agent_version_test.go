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

// A candidate's superseded-contract names differ from its name by the contract alone: every fold made
// after the contract (the launcher here) is made to each. A fold missing from one of them would make
// every approval of that contract's revisions fail to carry.
func TestACandidateIsNamedUnderEachSupersededContract(t *testing.T) {
	t.Parallel()

	const launcher = "ghcr.io/ctxmesh/launcher@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	features := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: knativeServingNamespace, Name: knativeFeaturesConfigMap}}
	features.Data = map[string]string{}
	for _, f := range launcherRequiredKnativeFlags {
		features.Data[f] = "enabled"
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	r := &AgentDeploymentReconciler{
		Client:        fake.NewClientBuilder().WithScheme(podTemplateScheme(t)).WithObjects(ns, features).Build(),
		Scheme:        podTemplateScheme(t),
		LauncherImage: launcher,
	}
	deploy := func(port int32) *agentsv1beta1.AgentDeployment {
		return &agentsv1beta1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "renamed", Namespace: "team-a"},
			Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest", Port: port},
		}
	}
	d := deploy(8080)
	hash, err := specHash(d.Spec)
	require.NoError(t, err)
	cand, err := r.candidateRevisionName(context.Background(), d, hash)
	require.NoError(t, err)

	pre := hardeningFold(combinedBindingDigest("", "", "", "", "", "", universalIdentitySADigest(false), "", "", ""), false)
	named := func(digest string) string {
		return agentVersionName(d.Name, hash) + "-h" + foldDigest(digest, "|launcher:"+launcher)
	}
	require.Equal(t, named(traceExportFold(pre, false)), cand.name,
		"the test's digest inputs must be the controller's, or the names below are not what older controllers wrote")
	v4, release := named(podContractFold(pre, "v4", false)), named(pre)
	assert.ElementsMatch(t, []string{v4, release}, cand.superseded)

	for _, rev := range []string{cand.name, v4, release} {
		assert.True(t, cand.names(rev), "%q is the candidate's pod", rev)
	}
	assert.False(t, cand.renamedFrom(cand.name))
	assert.True(t, cand.renamedFrom(v4))
	assert.False(t, cand.names(""))

	other, err := specHash(deploy(8090).Spec)
	require.NoError(t, err)
	otherCand, err := r.candidateRevisionName(context.Background(), deploy(8090), other)
	require.NoError(t, err)
	for _, rev := range append([]string{otherCand.name}, otherCand.superseded...) {
		assert.False(t, cand.names(rev), "another spec's revision %q is not this pod under any contract", rev)
	}
}
