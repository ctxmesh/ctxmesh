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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/prompt"
)

// mkPromptVersion creates a PromptVersion with the given git pointer and returns
// it. The referenced repo/path are fixed; the ref is the swappable pin.
func mkPromptVersion(t *testing.T, name, namespace, ref string) *agentsv1alpha1.PromptVersion {
	t.Helper()
	pv := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.PromptVersionSpec{
			Git: agentsv1alpha1.GitPromptSource{
				Repo: "https://github.com/example/prompts.git",
				Ref:  ref,
				Path: "agents/echo/system.txt",
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, pv))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, pv) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(pv), pv))
	return pv
}

// TestReconcile_PromptMaterialisedAndRevisionRolls: an agent with a promptRef
// gets the resolved prompt materialised into a <agent>-prompt ConfigMap, mounted
// read-only into the user container, with PROMPT_FILE + PROMPT_VERSION static env
// — and its revision name carries a "-h<digest>" suffix (the prompt folds into the
// combined binding digest), whereas the SAME agent WITHOUT a promptRef gets the
// bare "-{hash}" name. This proves the prompt rolls a revision.
func TestReconcile_PromptMaterialisedAndRevisionRolls(t *testing.T) {
	const (
		name      = "prompt-agent"
		namespace = "default"
		image     = "ghcr.io/ctxmesh/example-agent:pinned"
	)

	pv := mkPromptVersion(t, "echo-prompt-v1", namespace, "v1")

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:     image,
			PromptRef: pv.Name,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))

	// ── Revision name carries the prompt digest suffix ─────────────────────────
	hash, err := specHash(deploy.Spec)
	require.NoError(t, err)
	revName := ksvc.Spec.Template.Name
	assert.True(t, strings.HasPrefix(revName, name+"-"+hash+"-h"),
		"a prompt-bearing agent's revision name must carry the -h<digest> suffix, got %q", revName)
	assert.LessOrEqual(t, len(revName), 63, "revision name must stay within the DNS-1035 63-char limit")

	// ── Prompt ConfigMap materialised with the resolved content ────────────────
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: promptConfigMapName(name), Namespace: namespace}, &cm),
		"the <agent>-prompt ConfigMap must exist")
	content := cm.Data[promptConfigMapKey]
	require.NotEmpty(t, content, "prompt ConfigMap must carry the resolved content")

	// The resolved content is the fixture resolver's deterministic output for this
	// pointer — assert we can recompute the same version from it.
	wantVersion := prompt.Version(pv.Spec.Git, content)

	// ── User container: prompt volume mount + static env, no valueFrom ─────────
	userContainer := ksvc.Spec.Template.Spec.Containers[0]
	assert.Equal(t, image, userContainer.Image, "user container image is spec.Image")

	var mounted bool
	for _, m := range userContainer.VolumeMounts {
		if m.Name == promptVolumeName {
			mounted = true
			assert.Equal(t, promptMountPath, m.MountPath)
			assert.True(t, m.ReadOnly, "prompt mount must be read-only")
		}
	}
	assert.True(t, mounted, "user container must mount the prompt volume")

	var hasPromptVol bool
	for _, v := range ksvc.Spec.Template.Spec.Volumes {
		if v.Name == promptVolumeName {
			hasPromptVol = true
			require.NotNil(t, v.ConfigMap)
			assert.Equal(t, promptConfigMapName(name), v.ConfigMap.Name)
		}
	}
	assert.True(t, hasPromptVol, "pod must carry the prompt ConfigMap volume")

	env := envByName(userContainer.Env)
	require.Contains(t, env, envPromptFile)
	assert.Equal(t, promptMountPath+"/"+promptConfigMapKey, env[envPromptFile])
	require.Contains(t, env, envPromptVersion)
	assert.Equal(t, wantVersion, env[envPromptVersion],
		"PROMPT_VERSION must be the resolved prompt version (surfaced as prompt.version)")

	// Knative no-valueFrom guard (m5.7): every user-container env must be static.
	for _, e := range userContainer.Env {
		assert.Nil(t, e.ValueFrom, "ksvc env %q must be static (no valueFrom)", e.Name)
	}

	// ── Baseline: an identical agent WITHOUT a promptRef has the bare name ──────
	bare := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "bare-agent", Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: image},
	}
	require.NoError(t, k8sClient.Create(testCtx, bare))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, bare) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(bare), bare))
	reconcileNN(t, newReconciler(), "bare-agent", namespace)

	var bareKsvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: "bare-agent", Namespace: namespace}, &bareKsvc))
	bareHash, err := specHash(bare.Spec)
	require.NoError(t, err)
	assert.Equal(t, "bare-agent-"+bareHash, bareKsvc.Spec.Template.Name,
		"a promptless agent must have the bare revision name (no -h suffix)")
	// And it must NOT mount a prompt volume or inject prompt env.
	for _, e := range bareKsvc.Spec.Template.Spec.Containers[0].Env {
		assert.NotEqual(t, envPromptFile, e.Name, "promptless agent must not get PROMPT_FILE")
		assert.NotEqual(t, envPromptVersion, e.Name, "promptless agent must not get PROMPT_VERSION")
	}
}

// TestReconcile_PromptOnlyDeploy_RefSwapKeepsImage is the m9.3 core invariant:
// swapping the referenced PromptVersion's git.ref (a prompt-ONLY change — the
// AgentDeployment spec is untouched, so spec.Image and specHash are identical)
// rolls a NEW Knative revision (new prompt content, new -h<digest> suffix) while
// the container IMAGE DIGEST is UNCHANGED. No image rebuild.
func TestReconcile_PromptOnlyDeploy_RefSwapKeepsImage(t *testing.T) {
	const (
		name      = "swap-agent"
		namespace = "default"
		// A digest-pinned image so "image digest unchanged" is literal.
		image = "ghcr.io/ctxmesh/example-agent@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	)

	pv := mkPromptVersion(t, "swap-prompt", namespace, "ref-v1")

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:     image,
			PromptRef: pv.Name,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	r := newReconciler()

	// ── Reconcile with ref-v1 ──────────────────────────────────────────────────
	reconcileNN(t, r, name, namespace)
	var ksvc1 servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc1))
	rev1 := ksvc1.Spec.Template.Name
	image1 := ksvc1.Spec.Template.Spec.Containers[0].Image

	var cm1 corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: promptConfigMapName(name), Namespace: namespace}, &cm1))
	content1 := cm1.Data[promptConfigMapKey]
	promptVer1 := envByName(ksvc1.Spec.Template.Spec.Containers[0].Env)[envPromptVersion]

	// ── Swap ONLY the PromptVersion's git.ref (prompt-only change) ─────────────
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(pv), pv))
	pv.Spec.Git.Ref = "ref-v2"
	require.NoError(t, k8sClient.Update(testCtx, pv))

	// The AgentDeployment spec is UNCHANGED — reconcile the SAME deployment again.
	reconcileNN(t, r, name, namespace)
	var ksvc2 servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc2))
	rev2 := ksvc2.Spec.Template.Name
	image2 := ksvc2.Spec.Template.Spec.Containers[0].Image

	var cm2 corev1.ConfigMap
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: promptConfigMapName(name), Namespace: namespace}, &cm2))
	content2 := cm2.Data[promptConfigMapKey]
	promptVer2 := envByName(ksvc2.Spec.Template.Spec.Containers[0].Env)[envPromptVersion]

	// ── Assert: prompt CHANGED, revision ROLLED, image UNCHANGED ───────────────
	assert.NotEqual(t, content1, content2, "the resolved prompt content must change on a ref swap")
	assert.NotEqual(t, promptVer1, promptVer2, "PROMPT_VERSION must change on a ref swap")
	assert.NotEqual(t, rev1, rev2, "the Knative revision must roll on a prompt-only swap (new -h digest)")

	// THE CORE INVARIANT: the container image digest is byte-identical across the
	// prompt swap — no image rebuild.
	assert.Equal(t, image, image1, "revision 1 uses the pinned image digest")
	assert.Equal(t, image, image2, "revision 2 uses the SAME pinned image digest")
	assert.Equal(t, image1, image2,
		"prompt-only deploy: the container image digest is UNCHANGED across a prompt swap")

	// The spec hash prefix (which folds spec.Image) is identical across the swap —
	// only the -h<digest> suffix differs. Belt-and-braces that the image path did
	// not move.
	hash, err := specHash(deploy.Spec)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(rev1, name+"-"+hash+"-h"))
	assert.True(t, strings.HasPrefix(rev2, name+"-"+hash+"-h"))
}

// TestReconcile_PromptRefMissing: a promptRef naming a non-existent PromptVersion
// is user input — the reconcile sets Ready=False (no panic, no hard error) and
// materialises NO ksvc/prompt, so a prior revision (if any) would keep serving.
func TestReconcile_PromptRefMissing(t *testing.T) {
	const (
		name      = "badref-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:     "ghcr.io/ctxmesh/example-agent:latest",
			PromptRef: "does-not-exist",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	// reconcileNN asserts NO error — a missing PromptVersion is surfaced on status,
	// not returned as a reconcile error.
	reconcileNN(t, newReconciler(), name, namespace)

	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &updated))
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, cond, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Ready must be False for an unresolvable promptRef")
	assert.Equal(t, "PromptVersionNotFound", cond.Reason)

	// No ksvc must have been created (the workload write is skipped on the user
	// error — the OLD revision, had there been one, keeps serving).
	var ksvc servingv1.Service
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc)
	assert.Error(t, err, "no ksvc must be created when the promptRef is unresolvable")

	// No prompt ConfigMap either.
	var cm corev1.ConfigMap
	err = k8sClient.Get(testCtx,
		types.NamespacedName{Name: promptConfigMapName(name), Namespace: namespace}, &cm)
	assert.Error(t, err, "no prompt ConfigMap must be created on an unresolvable promptRef")
}

// TestReconcile_PromptUnresolvable: a PromptVersion whose git pointer the resolver
// cannot resolve (ErrNotFound — a bad ref / missing path) is likewise surfaced as
// Ready=False (reason PromptUnresolvable), keeping the old revision serving. Uses
// an explicit not-found seed on the fixture resolver so the failure path is
// exercised deterministically offline.
func TestReconcile_PromptUnresolvable(t *testing.T) {
	const (
		name      = "unresolvable-agent"
		namespace = "default"
	)

	pv := mkPromptVersion(t, "unresolvable-prompt", namespace, "deleted-ref")

	// A reconciler whose resolver treats this exact pointer as not-found.
	r := &AgentDeploymentReconciler{
		Client:         k8sClient,
		Scheme:         k8sClient.Scheme(),
		PromptResolver: prompt.NewFixtureResolver().SeedNotFound(pv.Spec.Git),
	}

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:     "ghcr.io/ctxmesh/example-agent:latest",
			PromptRef: pv.Name,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, r, name, namespace)

	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &updated))
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "PromptUnresolvable", cond.Reason)

	var ksvc servingv1.Service
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc)
	assert.Error(t, err, "no ksvc must be created when the git pointer is unresolvable")
}
