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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// envValues returns every value the user container carries for name, in order.
func envValues(t *testing.T, pod podTemplate, name string) []string {
	t.Helper()
	require.NotEmpty(t, pod.containers)
	var values []string
	for _, e := range pod.containers[0].Env {
		if e.Name == name {
			values = append(values, e.Value)
		}
	}
	return values
}

// identityReconciler is a reconciler whose fake client holds deploy (with a status subresource, so the
// condition can be written) and objs.
func identityReconciler(t *testing.T, deploy *agentsv1beta1.AgentDeployment, objs ...client.Object) *AgentDeploymentReconciler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(podTemplateScheme(t)).
		WithObjects(append(objs, deploy)...).
		WithStatusSubresource(&agentsv1beta1.AgentDeployment{}).Build()
	return &AgentDeploymentReconciler{Client: c, Scheme: podTemplateScheme(t)}
}

// An author cannot name their agent's namespace, name, version or tenant: the pod carries the
// platform's values, once each, and the attempt is reported. Every other variable is kept.
func TestAnIdentityVariableInSpecEnvIsThePlatforms(t *testing.T) {
	t.Parallel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "forger", Namespace: "team-a"},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Env: []corev1.EnvVar{
				{Name: envTenantID, Value: "acme"},
				{Name: envPodNamespace, Value: "team-b"},
				{Name: "FOO", Value: "bar"},
				{Name: envAgentName, Value: "their-agent"},
				{Name: envAgentVersion, Value: "their-agent-12345678"},
				{Name: envPodNamespace, Value: "team-c"},
			},
		},
	}
	r := identityReconciler(t, deploy, ns)
	pod, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)

	hash, err := specHash(deploy.Spec)
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a"}, envValues(t, pod, envPodNamespace))
	assert.Equal(t, []string{"forger"}, envValues(t, pod, envAgentName))
	assert.Equal(t, []string{agentVersionName("forger", hash)}, envValues(t, pod, envAgentVersion))
	assert.Empty(t, envValues(t, pod, envTenantID), "no Tenant owns team-a, so there is no tenant to name")
	assert.Equal(t, []string{"bar"}, envValues(t, pod, "FOO"), "only identity variables are the platform's")

	var stored agentsv1beta1.AgentDeployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(deploy), &stored))
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, conditionIdentityEnvIgnored)
	require.NotNil(t, cond, "the attempt is visible on the agent")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, reasonIdentityEnvSet, cond.Reason)
	assert.Contains(t, cond.Message, "POD_NAMESPACE, AGENT_NAME, AGENT_VERSION, TENANT_ID")

	// Removing them clears the report.
	stored.Spec.Env = []corev1.EnvVar{{Name: "FOO", Value: "bar"}}
	_, err = r.buildPodTemplate(context.Background(), &stored)
	require.NoError(t, err)
	var cleared agentsv1beta1.AgentDeployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(deploy), &cleared))
	assert.Nil(t, apimeta.FindStatusCondition(cleared.Status.Conditions, conditionIdentityEnvIgnored))
}

// Where a Tenant owns the namespace, TENANT_ID is that tenant's, whatever spec.env says.
func TestTenantIDIsTheNamespacesTenant(t *testing.T) {
	t.Parallel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{tenantLabel: "alpha"}}}
	tenant := &agentsv1beta1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}}
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "billed", Namespace: "team-a"},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Env:   []corev1.EnvVar{{Name: envTenantID, Value: "beta"}},
		},
	}
	r := identityReconciler(t, deploy, ns, tenant)
	pod, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha"}, envValues(t, pod, envTenantID))
}

// Dropping identity variables changes the pod and nothing a digest reads, so it must move the revision
// name or never reach a running pod; the name the pod had before is the same pod renamed.
func TestDroppingIdentityEnvRenamesTheRevision(t *testing.T) {
	t.Parallel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	forged := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "forger", Namespace: "team-a"},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Env:   []corev1.EnvVar{{Name: envPodNamespace, Value: "team-b"}},
		},
	}
	plain := forged.DeepCopy()
	plain.Spec.Env = nil

	r := identityReconciler(t, forged, ns)
	hash, err := specHash(forged.Spec)
	require.NoError(t, err)
	cand, err := r.candidateRevisionName(context.Background(), forged, hash)
	require.NoError(t, err)

	// The digest an agent gets without identity env is what the controller computed for this one before.
	plainPod, err := r.buildPodTemplate(context.Background(), plain)
	require.NoError(t, err)
	before := revisionName(forged.Name, hash, plainPod.digest)

	assert.NotEqual(t, before, cand.name, "the corrected pod must roll")
	assert.True(t, cand.renamedFrom(before), "the pod it replaces is the same pod renamed: no second canary arm")
	assert.True(t, cand.names(before), "and an approval of it carries")

	again, err := r.candidateRevisionName(context.Background(), forged, hash)
	require.NoError(t, err)
	assert.Equal(t, cand.name, again.name, "the name is stable, or the agent would restart on every reconcile")
}

func TestWithoutIdentityEnv(t *testing.T) {
	t.Parallel()

	kept, ignored := withoutIdentityEnv([]corev1.EnvVar{
		{Name: "A", Value: "1"}, {Name: envAgentVersion}, {Name: "agent_name"}, {Name: envAgentName}, {Name: envAgentVersion},
	})
	assert.Equal(t, []corev1.EnvVar{{Name: "A", Value: "1"}, {Name: "agent_name"}}, kept,
		"environment names are case-sensitive: agent_name is not AGENT_NAME")
	assert.Equal(t, []string{envAgentName, envAgentVersion}, ignored)

	kept, ignored = withoutIdentityEnv(nil)
	assert.Empty(t, kept)
	assert.Empty(t, ignored)
}
