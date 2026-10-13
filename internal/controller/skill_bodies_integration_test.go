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
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/skill"
	"github.com/ctxmesh/ctxmesh/internal/objectstore"
)

// skillEnv is a skill store plus the two body sources, wired into a reconciler the way the binary
// wires the real ones.
type skillEnv struct {
	store   skill.Store
	objects *objectstore.MemObjectStore
	git     *fakeGit
}

func newSkillEnv(t *testing.T, namespace, name string) *skillEnv {
	t.Helper()
	e := &skillEnv{store: skill.NewMemStore(), objects: objectstore.NewMemObjectStore(), git: &fakeGit{files: map[string]string{}}}
	require.NoError(t, e.store.UpsertSkill(testCtx, skill.Skill{
		Namespace: namespace, Name: name, Description: "How to handle a refund request.",
	}))
	return e
}

func (e *skillEnv) reconciler() *AgentDeploymentReconciler {
	r := newReconciler()
	r.Skills = skill.NewResolver(e.store)
	r.SkillGit = e.git
	r.SkillObjects = e.objects
	return r
}

// upload adds an uploaded version whose object holds served and whose digest is that of pinned.
func (e *skillEnv) upload(t *testing.T, namespace, name, pinned, served string) string {
	t.Helper()
	digest := skill.Digest([]byte(pinned))
	key := skill.ObjectKeyPrefix(namespace) + name + "/" + digest[len("sha256:"):]
	require.NoError(t, e.objects.Put(testCtx, key, strings.NewReader(served), int64(len(served)), "text/markdown"))
	require.NoError(t, e.store.AddVersion(testCtx, skill.SkillVersion{
		Namespace: namespace, Skill: name, Digest: digest, Source: skill.SourceUpload, ObjectKey: key,
	}))
	return digest
}

// fromGit adds a git-sourced version whose file holds body.
func (e *skillEnv) fromGit(t *testing.T, namespace, name, body string) string {
	t.Helper()
	digest := skill.Digest([]byte(body))
	v := skill.SkillVersion{
		Namespace: namespace, Skill: name, Digest: digest, Source: skill.SourceGit,
		Repo: "https://github.com/acme/skills", Ref: strings.Repeat("c", 40), Path: name + "/SKILL.md",
	}
	e.git.files[v.Repo+"|"+v.Ref+"|"+v.Path] = body
	require.NoError(t, e.store.AddVersion(testCtx, v))
	return digest
}

func skillCMsOf(t *testing.T, agent, namespace string) []corev1.ConfigMap {
	t.Helper()
	var cms corev1.ConfigMapList
	require.NoError(t, k8sClient.List(testCtx, &cms,
		client.InNamespace(namespace), client.MatchingLabels{skillAgentLabel: agent}))
	return cms.Items
}

func ksvcOf(t *testing.T, name, namespace string) servingv1.Service {
	t.Helper()
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	return ksvc
}

func skillVolumeOf(ksvc servingv1.Service) *corev1.Volume {
	for i := range ksvc.Spec.Template.Spec.Volumes {
		if ksvc.Spec.Template.Spec.Volumes[i].Name == skillVolumeName {
			return &ksvc.Spec.Template.Spec.Volumes[i]
		}
	}
	return nil
}

func skillCondition(t *testing.T, name, namespace string) *metav1.Condition {
	t.Helper()
	var got agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &got))
	return apimeta.FindStatusCondition(got.Status.Conditions, conditionSkillBodies)
}

func setSkillRefs(t *testing.T, name, namespace string, refs ...string) {
	t.Helper()
	var got agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &got))
	got.Spec.SkillRefs = refs
	require.NoError(t, k8sClient.Update(testCtx, &got))
}

// An agent with an attached skill mounts its verified body where SKILL_DIR points, as the file the
// launcher reads; moving the alias to a new version replaces the ConfigMap and rolls the revision;
// detaching removes the body, the mount and the condition.
func TestReconcile_SkillBodyIsMountedAndFollowsTheVersion(t *testing.T) {
	const (
		name      = "skill-body-agent"
		namespace = "default"
		skillName = "refund-policy"
		v1Body    = "Refunds: check the order date, then the receipt."
		v2Body    = "Refunds: check the order date, the receipt, then the card on file."
	)
	env := newSkillEnv(t, namespace, skillName)
	v1 := env.upload(t, namespace, skillName, v1Body, v1Body)
	require.NoError(t, env.store.SetAlias(testCtx, namespace, skillName, "stable", v1))

	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:     "ghcr.io/ctxmesh/example-agent:pinned",
			SkillRefs: []string{skillName + "@stable"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	reconcileNN(t, env.reconciler(), name, namespace)

	ksvc := ksvcOf(t, name, namespace)
	user := ksvc.Spec.Template.Spec.Containers[0]
	envs := envByName(user.Env)
	assert.Equal(t, skillMountPath, envs[envSkillDir])
	assert.Equal(t, skillName+"@"+v1, envs[envSkillRefs])
	var mounted bool
	for _, m := range user.VolumeMounts {
		if m.Name == skillVolumeName {
			mounted = true
			assert.Equal(t, skillMountPath, m.MountPath)
			assert.True(t, m.ReadOnly)
		}
	}
	assert.True(t, mounted, "the user container mounts the skill bodies")
	vol := skillVolumeOf(ksvc)
	require.NotNil(t, vol)

	cms := skillCMsOf(t, name, namespace)
	require.Len(t, cms, 1)
	first := cms[0]
	assert.Equal(t, vol.ConfigMap.Name, first.Name)
	assert.Equal(t, map[string]string{skillName: v1Body}, first.Data,
		"one file per skill, named as the launcher reads it, holding the verified body")
	assert.True(t, ptr.Deref(first.Immutable, false))

	cond := skillCondition(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, reasonSkillBodiesMounted, cond.Reason)
	firstRevision := ksvc.Spec.Template.Name

	// A new version, from git this time, behind the same alias.
	v2 := env.fromGit(t, namespace, skillName, v2Body)
	require.NoError(t, env.store.SetAlias(testCtx, namespace, skillName, "stable", v2))
	reconcileNN(t, env.reconciler(), name, namespace)

	ksvc = ksvcOf(t, name, namespace)
	assert.NotEqual(t, firstRevision, ksvc.Spec.Template.Name, "a new version rolls the revision")
	cms = skillCMsOf(t, name, namespace)
	require.Len(t, cms, 1, "the superseded ConfigMap is pruned (no live Revision mounts it)")
	assert.NotEqual(t, first.Name, cms[0].Name, "a new version is a new ConfigMap, never an edit of the old")
	assert.Equal(t, map[string]string{skillName: v2Body}, cms[0].Data)
	assert.Equal(t, cms[0].Name, skillVolumeOf(ksvc).ConfigMap.Name)

	// Detach.
	setSkillRefs(t, name, namespace)
	reconcileNN(t, env.reconciler(), name, namespace)

	ksvc = ksvcOf(t, name, namespace)
	assert.Nil(t, skillVolumeOf(ksvc), "no skill attached, no skill volume")
	assert.NotContains(t, envByName(ksvc.Spec.Template.Spec.Containers[0].Env), envSkillDir)
	assert.Empty(t, skillCMsOf(t, name, namespace), "a detached skill's body is not kept")
	assert.Nil(t, skillCondition(t, name, namespace))
}

// Bytes that do not hash to the pinned digest are never mounted: the agent still deploys, SkillBodies
// names the skill, and the reconcile is scheduled to look again.
func TestReconcile_SkillDigestMismatchMountsNothing(t *testing.T) {
	const (
		name      = "skill-mismatch-agent"
		namespace = "default"
		skillName = "escalation"
	)
	env := newSkillEnv(t, namespace, skillName)
	digest := env.upload(t, namespace, skillName, "the reviewed procedure", "a procedure nobody reviewed")

	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:     "ghcr.io/ctxmesh/example-agent:pinned",
			SkillRefs: []string{skillName + "@" + digest},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	result, err := env.reconciler().Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err, "a mismatch never fails the reconcile")
	assert.Positive(t, result.RequeueAfter, "the body is looked at again later")

	ksvc := ksvcOf(t, name, namespace)
	assert.Nil(t, skillVolumeOf(ksvc), "nothing is mounted")
	assert.NotContains(t, envByName(ksvc.Spec.Template.Spec.Containers[0].Env), envSkillDir,
		"load() answers 501: no body is mounted")
	assert.Empty(t, skillCMsOf(t, name, namespace))

	cond := skillCondition(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonSkillDigestMismatch, cond.Reason)
	assert.Contains(t, cond.Message, skillName)
	assert.NotContains(t, cond.Message, skill.Digest([]byte("a procedure nobody reviewed")))
}
