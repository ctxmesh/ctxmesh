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
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

const testCapabilityKey = "dGVzdC1jYXBhYmlsaXR5LXB1YmxpYy1rZXktMzJieXQ="

// The two values AMP_REQUIRE_ATTESTATION takes in a member's pod.
var (
	postureRequire = strconv.FormatBool(true)
	postureAccept  = strconv.FormatBool(false)
)

// meshFixture is a namespace, a registry selecting team=mesh, and an agent in it (a member when labelled).
func meshFixture(name string, member bool, env ...corev1.EnvVar) (*agentsv1beta1.AgentDeployment, []client.Object) {
	labels := map[string]string{}
	if member {
		labels["team"] = "mesh"
	}
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Labels: labels},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
			Role:  "worker",
			Env:   env,
		},
	}
	return deploy, []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&agentsv1beta1.AgentRegistry{
			ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "team-a"},
			Spec: agentsv1beta1.AgentRegistrySpec{
				RegistryId:     "mesh-id",
				MemberSelector: metav1.LabelSelector{MatchLabels: map[string]string{"team": "mesh"}},
			},
		},
	}
}

func meshReconciler(
	t *testing.T, deploy *agentsv1beta1.AgentDeployment, objs []client.Object, acceptUnattested bool, key string,
) *AgentDeploymentReconciler {
	t.Helper()
	r := identityReconciler(t, deploy, objs...)
	r.AMPAcceptUnattested = acceptUnattested
	r.OBOEgress.CapabilityPublicKeyB64 = key
	return r
}

func podVolume(pod podTemplate, name string) []corev1.Volume {
	var out []corev1.Volume
	for _, v := range pod.volumes {
		if v.Name == name {
			out = append(out, v)
		}
	}
	return out
}

func userMounts(pod podTemplate, name string) []corev1.VolumeMount {
	var out []corev1.VolumeMount
	for _, m := range pod.containers[0].VolumeMounts {
		if m.Name == name {
			out = append(out, m)
		}
	}
	return out
}

// A registry member's registry and role are the platform's: spec.env cannot claim another registry or
// role, and the attempt is reported. A non-member cannot make itself one by naming a registry.
func TestARegistryAndRoleInSpecEnvAreThePlatforms(t *testing.T) {
	t.Parallel()

	forged := []corev1.EnvVar{
		{Name: envAgentRegistryID, Value: "other-registry"},
		{Name: envAgentRole, Value: "orchestrator"},
	}
	member, objs := meshFixture("member", true, forged...)
	r := meshReconciler(t, member, objs, false, testCapabilityKey)
	pod, err := r.buildPodTemplate(context.Background(), member)
	require.NoError(t, err)
	assert.Equal(t, []string{"mesh-id"}, envValues(t, pod, envAgentRegistryID))
	assert.Equal(t, []string{"worker"}, envValues(t, pod, envAgentRole), "spec.role, not spec.env")

	var stored agentsv1beta1.AgentDeployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(member), &stored))
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, conditionIdentityEnvIgnored)
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "AGENT_REGISTRY_ID, AGENT_ROLE")

	loner, objs := meshFixture("loner", false, forged...)
	r = meshReconciler(t, loner, objs, false, testCapabilityKey)
	pod, err = r.buildPodTemplate(context.Background(), loner)
	require.NoError(t, err)
	assert.Empty(t, envValues(t, pod, envAgentRegistryID), "naming a registry does not make an agent a member")
	assert.Empty(t, envValues(t, pod, envAgentRole))
}

// Every member gets what attested AMP needs: its BFF-audience pod token, the capability key, and the
// posture. A non-member gets none of it, and a guarded member gets the token once.
func TestARegistryMemberCarriesTheAttestationWiring(t *testing.T) {
	t.Parallel()

	member, objs := meshFixture("member", true)
	r := meshReconciler(t, member, objs, false, testCapabilityKey)
	pod, err := r.buildPodTemplate(context.Background(), member)
	require.NoError(t, err)

	vols := podVolume(pod, bffPodTokenVolume)
	require.Len(t, vols, 1, "the member's pod projects a BFF-audience token")
	require.NotNil(t, vols[0].Projected)
	sa := vols[0].Projected.Sources[0].ServiceAccountToken
	require.NotNil(t, sa)
	assert.Equal(t, runcap.BFFPodAudience, sa.Audience)
	mounts := userMounts(pod, bffPodTokenVolume)
	require.Len(t, mounts, 1)
	assert.Equal(t, bffPodTokenMountPath, mounts[0].MountPath)
	assert.True(t, mounts[0].ReadOnly)
	assert.Equal(t, []string{bffPodTokenMountPath + "/token"}, envValues(t, pod, envBFFPodTokenPath))
	assert.Equal(t, []string{testCapabilityKey}, envValues(t, pod, envMCPCapPublicKey))
	assert.Equal(t, []string{postureRequire}, envValues(t, pod, envAMPRequireAttestation), "require is the default")

	accepting := meshReconciler(t, member.DeepCopy(), objs, true, testCapabilityKey)
	pod, err = accepting.buildPodTemplate(context.Background(), member)
	require.NoError(t, err)
	assert.Equal(t, []string{postureAccept}, envValues(t, pod, envAMPRequireAttestation))

	loner, objs := meshFixture("loner", false)
	r = meshReconciler(t, loner, objs, false, testCapabilityKey)
	pod, err = r.buildPodTemplate(context.Background(), loner)
	require.NoError(t, err)
	assert.Empty(t, podVolume(pod, bffPodTokenVolume), "an agent in no registry is unchanged")
	assert.Empty(t, envValues(t, pod, envBFFPodTokenPath))
	assert.Empty(t, envValues(t, pod, envMCPCapPublicKey))
	assert.Empty(t, envValues(t, pod, envAMPRequireAttestation))
}

func TestAGuardedMemberMountsTheBFFTokenOnce(t *testing.T) {
	t.Parallel()

	member, objs := meshFixture("guarded", true)
	member.Spec.GuardrailPolicyRef = "strict"
	objs = append(objs, &agentsv1beta1.GuardrailPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "strict", Namespace: "team-a"},
		Spec: agentsv1beta1.GuardrailPolicySpec{
			FailMode:        "closed",
			PatternDenylist: []agentsv1beta1.PatternRule{{Name: "ssn", Pattern: `\d{3}-\d{2}-\d{4}`, Action: "block"}},
		},
	})
	r := meshReconciler(t, member, objs, false, testCapabilityKey)
	pod, err := r.buildPodTemplate(context.Background(), member)
	require.NoError(t, err)
	assert.Len(t, podVolume(pod, bffPodTokenVolume), 1)
	assert.Len(t, userMounts(pod, bffPodTokenVolume), 1)
	assert.Len(t, envValues(t, pod, envBFFPodTokenPath), 1)
}

// The wiring is read by no digest, so it must move a member's revision name or never reach a running
// pod. The pod named without it, and under the other posture, are the same pod renamed: an approval
// carries across the upgrade and the accept-to-require flip. A rotated key reaches running members.
func TestAttestationWiringRenamesAMembersRevision(t *testing.T) {
	t.Parallel()

	member, objs := meshFixture("member", true)
	hash, err := specHash(member.Spec)
	require.NoError(t, err)
	candidate := func(accept bool, key string) candidateRevision {
		t.Helper()
		r := meshReconciler(t, member.DeepCopy(), objs, accept, key)
		cand, err := r.candidateRevisionName(context.Background(), member, hash)
		require.NoError(t, err)
		return cand
	}

	requiring := candidate(false, testCapabilityKey)
	accepting := candidate(true, testCapabilityKey)
	assert.NotEqual(t, requiring.name, accepting.name, "the posture is pod env, so flipping it rolls")
	assert.True(t, requiring.renamedFrom(accepting.name), "an approval carries from accept to require")
	assert.True(t, accepting.renamedFrom(requiring.name), "and back")
	assert.NotEqual(t, requiring.name, candidate(false, "cm90YXRlZC1rZXk=").name, "a rotated key rolls")

	r := meshReconciler(t, member.DeepCopy(), objs, false, testCapabilityKey)
	pod, err := r.buildPodTemplate(context.Background(), member)
	require.NoError(t, err)
	var unwired string
	for _, d := range pod.supersededDigests {
		if foldDigest(d, ampAttestationFold(r.OBOEgress, true)) == pod.digest {
			unwired = d
		}
	}
	require.NotEmpty(t, unwired, "the digest before the wiring was folded in is a superseded name")
	assert.True(t, requiring.renamedFrom(revisionName(member.Name, hash, unwired)),
		"the pod a controller without the wiring named is this pod renamed")

	again := candidate(false, testCapabilityKey)
	assert.Equal(t, requiring.name, again.name, "the name is stable across reconciles")

	loner, objs := meshFixture("loner", false)
	rl := meshReconciler(t, loner, objs, false, testCapabilityKey)
	lonerPod, err := rl.buildPodTemplate(context.Background(), loner)
	require.NoError(t, err)
	rl.AMPAcceptUnattested = true
	flipped, err := rl.buildPodTemplate(context.Background(), loner)
	require.NoError(t, err)
	assert.Equal(t, lonerPod.digest, flipped.digest, "an agent in no registry does not roll for the posture")
}

func TestAMPAttestationEnvLeavesSetVariables(t *testing.T) {
	t.Parallel()

	obo := OBOEgressConfig{CapabilityPublicKeyB64: testCapabilityKey, CapabilityAudience: "aud"}
	got := ampAttestationEnv(nil, obo, true)
	assert.Equal(t, []corev1.EnvVar{
		{Name: envBFFPodTokenPath, Value: bffPodTokenMountPath + "/token"},
		{Name: envMCPCapPublicKey, Value: testCapabilityKey},
		{Name: envMCPCapAudience, Value: "aud"},
		{Name: envAMPRequireAttestation, Value: postureRequire},
	}, got)

	set := []corev1.EnvVar{
		{Name: envMCPCapPublicKey, Value: testCapabilityKey},
		{Name: envBFFPodTokenPath, Value: "/p"},
		{Name: envAMPRequireAttestation, Value: postureRequire},
	}
	assert.Equal(t, []corev1.EnvVar{{Name: envMCPCapAudience, Value: "aud"}}, ampAttestationEnv(set, obo, true),
		"a variable the platform already set is never repeated, and the audience does not depend on who set the key")

	assert.Equal(t, []corev1.EnvVar{
		{Name: envBFFPodTokenPath, Value: bffPodTokenMountPath + "/token"},
		{Name: envAMPRequireAttestation, Value: postureAccept},
	}, ampAttestationEnv(nil, OBOEgressConfig{}, false), "no key in the install, no key in the pod")
}

// A member's attestation wiring is the platform's: spec.env cannot switch it to accept unattested calls,
// point it at another key, audience or token, and the attempt is reported. The pod carries the posture
// its revision name folds. An agent in no registry keeps its spec.env values.
func TestAMembersAttestationWiringInSpecEnvIsThePlatforms(t *testing.T) {
	t.Parallel()

	authored := []corev1.EnvVar{
		{Name: envAMPRequireAttestation, Value: postureAccept},
		{Name: envMCPCapPublicKey, Value: "YXV0aG9ycy1vd24ta2V5"},
		{Name: envMCPCapAudience, Value: "authors-audience"},
		{Name: envBFFPodTokenPath, Value: "/tmp/forged-token"},
		{Name: "KEPT", Value: "1"},
	}
	member, objs := meshFixture("member", true, authored...)
	r := meshReconciler(t, member, objs, false, testCapabilityKey)
	r.OBOEgress.CapabilityAudience = "platform-audience"
	pod, err := r.buildPodTemplate(context.Background(), member)
	require.NoError(t, err)
	assert.Equal(t, []string{postureRequire}, envValues(t, pod, envAMPRequireAttestation))
	assert.Equal(t, []string{testCapabilityKey}, envValues(t, pod, envMCPCapPublicKey))
	assert.Equal(t, []string{"platform-audience"}, envValues(t, pod, envMCPCapAudience))
	assert.Equal(t, []string{bffPodTokenMountPath + "/token"}, envValues(t, pod, envBFFPodTokenPath))
	assert.Equal(t, []string{"1"}, envValues(t, pod, "KEPT"))

	var stored agentsv1beta1.AgentDeployment
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(member), &stored))
	cond := apimeta.FindStatusCondition(stored.Status.Conditions, conditionIdentityEnvIgnored)
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message,
		"BFF_POD_TOKEN_PATH, MCP_CAPABILITY_PUBLIC_KEY, MCP_CAPABILITY_AUDIENCE, AMP_REQUIRE_ATTESTATION")

	clean, cleanObjs := meshFixture("member", true, corev1.EnvVar{Name: "KEPT", Value: "1"})
	cleanPod, err := meshReconciler(t, clean, cleanObjs, false, testCapabilityKey).buildPodTemplate(context.Background(), clean)
	require.NoError(t, err)
	assert.NotEqual(t, cleanPod.digest, pod.digest,
		"dropping the authored values changes the pod, so it moves the revision name (identity-env fold)")

	loner, objs := meshFixture("loner", false, authored...)
	r = meshReconciler(t, loner, objs, false, testCapabilityKey)
	pod, err = r.buildPodTemplate(context.Background(), loner)
	require.NoError(t, err)
	assert.Equal(t, []string{postureAccept}, envValues(t, pod, envAMPRequireAttestation),
		"an agent in no registry is not checked by these, and keeps what spec.env says")
	assert.Equal(t, []string{"YXV0aG9ycy1vd24ta2V5"}, envValues(t, pod, envMCPCapPublicKey))
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(loner), &stored))
	assert.Nil(t, apimeta.FindStatusCondition(stored.Status.Conditions, conditionIdentityEnvIgnored))
}
