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
	"encoding/json"
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

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/prompt"
)

// PromptVersion is retired to Postgres (ADR 0044): the controller resolves the prompt from the
// DENORMALIZED resolved-prompt annotation the BFF stamps (m40.3), NOT a PromptVersion CRD. These envtests
// therefore stamp the annotation on the AgentDeployment rather than creating a PromptVersion object.

// echoGit is the fixed prompt git pointer; ref is the swappable pin.
func echoGit(ref string) agentsv1alpha1.GitPromptSource {
	return agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  ref,
		Path: "agents/echo/system.txt",
	}
}

// stampPrompt sets the resolved-prompt annotation (the denormalized pointer) on the agent.
func stampPrompt(t *testing.T, deploy *agentsv1alpha1.AgentDeployment, promptName string, git agentsv1alpha1.GitPromptSource) {
	t.Helper()
	raw, err := json.Marshal(prompt.ResolvedPointer{Name: promptName, Repo: git.Repo, Ref: git.Ref, Path: git.Path})
	require.NoError(t, err)
	if deploy.Annotations == nil {
		deploy.Annotations = map[string]string{}
	}
	deploy.Annotations[prompt.ResolvedPromptAnnotation] = string(raw)
}

// promptCMByLabel returns the agent's current content-addressed prompt ConfigMap (found by
// the promptAgentLabel — the name now carries a content digest, FUNC-3). It asserts exactly
// one exists (in envtest with no live Revisions, superseded CMs are pruned).
func promptCMByLabel(t *testing.T, agentName, namespace string) corev1.ConfigMap {
	t.Helper()
	var cms corev1.ConfigMapList
	require.NoError(t, k8sClient.List(testCtx, &cms,
		client.InNamespace(namespace), client.MatchingLabels{promptAgentLabel: agentName}))
	require.Len(t, cms.Items, 1, "exactly one current prompt ConfigMap for %s", agentName)
	return cms.Items[0]
}

// promptCMCount returns how many content-addressed prompt ConfigMaps exist for the agent.
func promptCMCount(t *testing.T, agentName, namespace string) int {
	t.Helper()
	var cms corev1.ConfigMapList
	require.NoError(t, k8sClient.List(testCtx, &cms,
		client.InNamespace(namespace), client.MatchingLabels{promptAgentLabel: agentName}))
	return len(cms.Items)
}

// TestReconcile_PromptMaterialisedAndRevisionRolls: an agent with a stamped prompt pointer gets the resolved
// prompt materialised into a <agent>-prompt ConfigMap, mounted read-only with PROMPT_FILE + PROMPT_VERSION
// static env — and its revision name carries a "-h<digest>" suffix, whereas the SAME agent WITHOUT a prompt
// gets the bare "-{hash}" name. This proves the prompt rolls a revision.
func TestReconcile_PromptMaterialisedAndRevisionRolls(t *testing.T) {
	const (
		name      = "prompt-agent"
		namespace = "default"
		image     = "ghcr.io/ctxmesh/example-agent:pinned"
	)

	git := echoGit("v1")
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:     image,
			PromptRef: "echo-prompt-v1",
		},
	}
	stampPrompt(t, deploy, "echo-prompt-v1", git)
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
	cm := promptCMByLabel(t, name, namespace)
	content := cm.Data[promptConfigMapKey]
	require.NotEmpty(t, content, "prompt ConfigMap must carry the resolved content")

	wantVersion := prompt.Version(git, content)

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
			assert.Equal(t, cm.Name, v.ConfigMap.Name, "volume references the content-addressed prompt CM")
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

	// ── Baseline: an identical agent WITHOUT a prompt has the bare name ─────────
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
	assert.Equal(t, "bare-agent-"+bareHash+bareIdentitySuffix, bareKsvc.Spec.Template.Name,
		"a promptless agent has the spec-hash revision name (+ the C7b identity-SA suffix)")
	for _, e := range bareKsvc.Spec.Template.Spec.Containers[0].Env {
		assert.NotEqual(t, envPromptFile, e.Name, "promptless agent must not get PROMPT_FILE")
		assert.NotEqual(t, envPromptVersion, e.Name, "promptless agent must not get PROMPT_VERSION")
	}
}

// TestReconcile_PromptOnlyDeploy_RefSwapKeepsImage is the m9.3 core invariant: swapping the resolved prompt
// pointer's ref (a prompt-ONLY change — spec.Image + specHash are identical; only the stamped annotation
// changes) rolls a NEW Knative revision while the container IMAGE DIGEST is UNCHANGED. No image rebuild.
func TestReconcile_PromptOnlyDeploy_RefSwapKeepsImage(t *testing.T) {
	const (
		name      = "swap-agent"
		namespace = "default"
		image     = "ghcr.io/ctxmesh/example-agent@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:     image,
			PromptRef: "swap-prompt",
		},
	}
	stampPrompt(t, deploy, "swap-prompt", echoGit("ref-v1"))
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

	cm1 := promptCMByLabel(t, name, namespace)
	content1 := cm1.Data[promptConfigMapKey]
	promptVer1 := envByName(ksvc1.Spec.Template.Spec.Containers[0].Env)[envPromptVersion]

	// ── Swap ONLY the resolved-prompt pointer's ref (prompt-only change) ───────
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	stampPrompt(t, deploy, "swap-prompt", echoGit("ref-v2"))
	require.NoError(t, k8sClient.Update(testCtx, deploy))

	reconcileNN(t, r, name, namespace)
	var ksvc2 servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc2))
	rev2 := ksvc2.Spec.Template.Name
	image2 := ksvc2.Spec.Template.Spec.Containers[0].Image

	cm2 := promptCMByLabel(t, name, namespace)
	content2 := cm2.Data[promptConfigMapKey]
	promptVer2 := envByName(ksvc2.Spec.Template.Spec.Containers[0].Env)[envPromptVersion]
	// FUNC-3: the prompt swap produced a NEW content-addressed CM (not an in-place mutation
	// of the one the prior revision mounts).
	assert.NotEqual(t, cm1.Name, cm2.Name, "a prompt swap must create a new content-addressed CM")

	// ── Assert: prompt CHANGED, revision ROLLED, image UNCHANGED ───────────────
	assert.NotEqual(t, content1, content2, "the resolved prompt content must change on a ref swap")
	assert.NotEqual(t, promptVer1, promptVer2, "PROMPT_VERSION must change on a ref swap")
	assert.NotEqual(t, rev1, rev2, "the Knative revision must roll on a prompt-only swap (new -h digest)")

	// THE CORE INVARIANT: the container image digest is byte-identical across the swap — no image rebuild.
	assert.Equal(t, image, image1, "revision 1 uses the pinned image digest")
	assert.Equal(t, image, image2, "revision 2 uses the SAME pinned image digest")
	assert.Equal(t, image1, image2,
		"prompt-only deploy: the container image digest is UNCHANGED across a prompt swap")

	// The spec hash prefix (which folds spec.Image, NOT the annotation) is identical across the swap — only
	// the -h<digest> suffix differs.
	hash, err := specHash(deploy.Spec)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(rev1, name+"-"+hash+"-h"))
	assert.True(t, strings.HasPrefix(rev2, name+"-"+hash+"-h"))
}

// TestReconcile_PromptRefMissing: a promptRef with NO stamped pointer (e.g. a raw kubectl apply) is user
// input — the reconcile sets Ready=False (reason PromptPointerMissing, no panic/hard error) and materialises
// NO ksvc/prompt, so a prior revision (if any) keeps serving.
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
	// No stamped annotation — the unresolved-pointer case.
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	var updated agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &updated))
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, cond, "Ready condition must be set")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Ready must be False for an unstamped promptRef")
	assert.Equal(t, "PromptPointerMissing", cond.Reason)

	var ksvc servingv1.Service
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc)
	assert.Error(t, err, "no ksvc must be created when the prompt pointer is unstamped")

	assert.Equal(t, 0, promptCMCount(t, name, namespace),
		"no prompt ConfigMap must be created on an unstamped promptRef")
}

// TestReconcile_PromptUnresolvable: a stamped prompt pointer the resolver cannot resolve (ErrNotFound — a bad
// ref / missing path) is surfaced as Ready=False (reason PromptUnresolvable), keeping the old revision
// serving. Uses an explicit not-found seed on the fixture resolver so the failure path is exercised offline.
func TestReconcile_PromptUnresolvable(t *testing.T) {
	const (
		name      = "unresolvable-agent"
		namespace = "default"
	)

	git := echoGit("deleted-ref")
	r := &AgentDeploymentReconciler{
		Client:         k8sClient,
		Scheme:         k8sClient.Scheme(),
		PromptResolver: prompt.NewFixtureResolver().SeedNotFound(git),
		Registry:       NewPostgresRegistryReader(testRegStore),
	}

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:     "ghcr.io/ctxmesh/example-agent:latest",
			PromptRef: "unresolvable-prompt",
		},
	}
	stampPrompt(t, deploy, "unresolvable-prompt", git)
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
