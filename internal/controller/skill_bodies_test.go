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
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/skill"
	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/prompt"
)

const skillTestNS = "team-a"

// skillVersions is a fake version lookup: ref → version.
type skillVersions map[string]skill.SkillVersion

func (v skillVersions) Version(_ context.Context, _, name, digest string) (skill.SkillVersion, bool, error) {
	sv, ok := v[name+"@"+digest]
	return sv, ok, nil
}

// fakeGit serves files by (repo, ref, path), counting fetches, and fails when err is set.
type fakeGit struct {
	files map[string]string
	err   error
	calls int
}

func (g *fakeGit) Resolve(_ context.Context, src agentsv1beta1.GitPromptSource) (prompt.Resolved, error) {
	g.calls++
	if g.err != nil {
		return prompt.Resolved{}, g.err
	}
	content, ok := g.files[src.Repo+"|"+src.Ref+"|"+src.Path]
	if !ok {
		return prompt.Resolved{}, prompt.ErrNotFound
	}
	return prompt.Resolved{Content: content}, nil
}

// countingObjects wraps an object store and counts reads, so a test can prove a key is never read.
type countingObjects struct {
	store *objectstore.MemObjectStore
	gets  []string
}

func (c *countingObjects) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.gets = append(c.gets, key)
	return c.store.Get(ctx, key)
}

// skillFixture builds versions and sources for skills whose bodies are given by name.
type skillFixture struct {
	versions skillVersions
	git      *fakeGit
	objects  *countingObjects
}

func newSkillFixture() *skillFixture {
	return &skillFixture{
		versions: skillVersions{},
		git:      &fakeGit{files: map[string]string{}},
		objects:  &countingObjects{store: objectstore.NewMemObjectStore()},
	}
}

// gitSkill registers a git-sourced version of name whose source holds served; the version's digest is
// that of pinned, which differs from served only when a test wants a mismatch.
func (f *skillFixture) gitSkill(name, pinned, served string) string {
	digest := skill.Digest([]byte(pinned))
	v := skill.SkillVersion{
		Namespace: skillTestNS, Skill: name, Digest: digest, Source: skill.SourceGit,
		Repo: "https://github.com/acme/skills", Ref: strings.Repeat("a", 40), Path: name + "/SKILL.md",
	}
	f.versions[name+"@"+digest] = v
	f.git.files[v.Repo+"|"+v.Ref+"|"+v.Path] = served
	return name + "@" + digest
}

// uploadSkill registers an uploaded version of name, stored under key, whose object holds served.
func (f *skillFixture) uploadSkill(t *testing.T, name, key, pinned, served string) string {
	t.Helper()
	digest := skill.Digest([]byte(pinned))
	f.versions[name+"@"+digest] = skill.SkillVersion{
		Namespace: skillTestNS, Skill: name, Digest: digest, Source: skill.SourceUpload, ObjectKey: key,
	}
	require.NoError(t, f.objects.store.Put(context.Background(), key, strings.NewReader(served), int64(len(served)), ""))
	return name + "@" + digest
}

func (f *skillFixture) resolver(capBytes int, now time.Time) skillBodyResolver {
	return skillBodyResolver{
		versions: f.versions, git: f.git, objects: f.objects,
		capBytes: capBytes, now: func() time.Time { return now },
	}
}

func noSizes(string) (int, bool) { return 0, false }

func (f *skillFixture) resolve(capBytes int, resolved ...string) skillBodyOutcome {
	return f.resolver(capBytes, time.Now()).resolve(context.Background(), skillTestNS, resolved, nil, nil, noSizes)
}

func problemReasons(out skillBodyOutcome) map[string]string {
	got := map[string]string{}
	for _, p := range out.problems {
		got[p.name] = p.reason
	}
	return got
}

// Both sources resolve to the bytes their digest names, and only those bytes are mounted.
func TestASkillBodyResolvesFromEachSource(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	git := f.gitSkill("refund-policy", "Refunds: check the order date first.", "Refunds: check the order date first.")
	up := f.uploadSkill(t, "escalation", "skills/team-a/escalation/SKILL.md", "Escalate after two failed fixes.",
		"Escalate after two failed fixes.")

	out := f.resolve(maxSkillBodiesBytes, git, up)

	assert.Empty(t, out.problems)
	assert.Equal(t, []string{git, up}, out.mounted)
	assert.Equal(t, map[string][]byte{
		"refund-policy": []byte("Refunds: check the order date first."),
		"escalation":    []byte("Escalate after two failed fixes."),
	}, out.bodies)
	assert.Equal(t, 1, f.git.calls)
	assert.Equal(t, []string{"skills/team-a/escalation/SKILL.md"}, f.objects.gets)
}

// The digest is the version's identity, so bytes that do not hash to it are not that version: they are
// never mounted, from either source, and the condition never reveals what they did hash to — that
// would let a caller learn the hash of an object it cannot read, then pin it.
func TestAMismatchedBodyIsNeverMounted(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	git := f.gitSkill("refund-policy", "what was reviewed", "what is there now")
	up := f.uploadSkill(t, "escalation", "skills/team-a/escalation/SKILL.md", "what was reviewed", "what is there now")

	out := f.resolve(maxSkillBodiesBytes, git, up)

	assert.Empty(t, out.bodies, "no unverified byte is mounted")
	assert.Empty(t, out.mounted)
	assert.Equal(t, map[string]string{
		"refund-policy": reasonSkillDigestMismatch,
		"escalation":    reasonSkillDigestMismatch,
	}, problemReasons(out))

	cond := skillBodiesCondition(1, 2, out.problems)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonSkillDigestMismatch, cond.Reason)
	assert.Contains(t, cond.Message, "refund-policy")
	assert.Contains(t, cond.Message, skill.Digest([]byte("what was reviewed")), "names the pinned digest")
	assert.NotContains(t, cond.Message, skill.Digest([]byte("what is there now")), "never the digest of what is there")
}

// The bodies share one ConfigMap, so their total is capped. A body that does not fit is left out whole —
// never truncated — and a later, smaller one still fits. Nothing is mounted past the cap.
func TestSkillBodiesAreCapped(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	big := strings.Repeat("b", 70)
	first := f.gitSkill("first", strings.Repeat("a", 60), strings.Repeat("a", 60))
	second := f.gitSkill("second", big, big)
	third := f.gitSkill("third", strings.Repeat("c", 30), strings.Repeat("c", 30))

	out := f.resolve(100, first, second, third)

	assert.Equal(t, []string{first, third}, out.mounted)
	assert.Equal(t, map[string]string{"second": reasonSkillTooLarge}, problemReasons(out))
	assert.NotContains(t, out.bodies, "second", "a body that does not fit is left out, not truncated")
	total := 0
	for _, b := range out.bodies {
		total += len(b)
	}
	assert.LessOrEqual(t, total, 100)

	cond := skillBodiesCondition(1, 3, out.problems)
	assert.Equal(t, reasonSkillTooLarge, cond.Reason)
	assert.Contains(t, cond.Message, "second: its body is 70 bytes")
}

// One body larger than the cap on its own is left out, and the others mount.
func TestTheRealCapLeavesOutABodyTooLargeAlone(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	huge := strings.Repeat("x", maxSkillBodiesBytes+1)
	small := f.gitSkill("small", "fits", "fits")
	big := f.gitSkill("huge", huge, huge)

	out := f.resolve(maxSkillBodiesBytes, big, small)

	assert.Equal(t, []string{small}, out.mounted)
	assert.Equal(t, map[string]string{"huge": reasonSkillTooLarge}, problemReasons(out))
	assert.Equal(t, maxSkillBodiesBytes+1, out.sizes[big], "the verified size is kept, so it is not fetched again")
}

// A known size that cannot fit is decided without fetching the body again.
func TestAKnownTooLargeBodyIsNotFetchedAgain(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	big := f.gitSkill("big", "0123456789", "0123456789")
	sizes := func(ref string) (int, bool) { return 10, ref == big }

	out := f.resolver(5, time.Now()).resolve(context.Background(), skillTestNS, []string{big}, nil, nil, sizes)

	assert.Equal(t, map[string]string{"big": reasonSkillTooLarge}, problemReasons(out))
	assert.Zero(t, f.git.calls)
}

// The controller reads uploads with the platform's credential, so a version whose key is outside its
// namespace's prefix — written before the BFF validated keys, or straight into the database — is
// refused without reading the object.
func TestAForeignObjectKeyIsNeverRead(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.uploadSkill(t, "borrowed", "knowledge/team-b/kb/contract.pdf", "secret", "secret")

	out := f.resolve(maxSkillBodiesBytes, ref)

	assert.Empty(t, out.bodies)
	assert.Equal(t, map[string]string{"borrowed": reasonSkillSourceRefused}, problemReasons(out))
	assert.Empty(t, f.objects.gets, "the foreign key is never read")
}

// Without a source this install can read, a body is reported unavailable, not attempted.
func TestAnUnreadableSourceIsReported(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	git := f.gitSkill("refund-policy", "a", "a")
	up := f.uploadSkill(t, "escalation", "skills/team-a/escalation", "b", "b")
	r := skillBodyResolver{versions: f.versions, capBytes: maxSkillBodiesBytes, now: time.Now}

	out := r.resolve(context.Background(), skillTestNS, []string{git, up}, nil, nil, noSizes)

	assert.Empty(t, out.bodies)
	assert.Equal(t, map[string]string{
		"refund-policy": reasonSkillSourceUnavailable,
		"escalation":    reasonSkillSourceUnavailable,
	}, problemReasons(out))
	for _, fl := range out.failures {
		assert.False(t, fl.requeue, "only a configuration change clears this, so it schedules no retry")
	}
}

// A failed fetch is retried on a doubling backoff, and not before: reconciles run on every Knative
// status change, and an unreachable git host must not be asked on each one.
func TestAFailedFetchBacksOff(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.gitSkill("refund-policy", "body", "body")
	f.git.err = errors.New("dial tcp raw.githubusercontent.com: i/o timeout")
	t0 := time.Now()

	first := f.resolver(maxSkillBodiesBytes, t0).resolve(context.Background(), skillTestNS, []string{ref}, nil, nil, noSizes)
	require.Contains(t, first.failures, ref)
	assert.Equal(t, reasonSkillFetchFailed, first.failures[ref].problem.reason)
	assert.True(t, first.failures[ref].requeue)
	assert.Equal(t, t0.Add(skillRetryBase), first.failures[ref].retryAt)
	assert.Equal(t, 1, f.git.calls)

	early := f.resolver(maxSkillBodiesBytes, t0.Add(skillRetryBase/2)).
		resolve(context.Background(), skillTestNS, []string{ref}, nil, first.failures, noSizes)
	assert.Equal(t, 1, f.git.calls, "not fetched again before the retry is due")
	assert.Equal(t, first.failures[ref], early.failures[ref], "the failure is carried, not forgotten")
	assert.Equal(t, map[string]string{"refund-policy": reasonSkillFetchFailed}, problemReasons(early))

	due := f.resolver(maxSkillBodiesBytes, t0.Add(skillRetryBase)).
		resolve(context.Background(), skillTestNS, []string{ref}, nil, first.failures, noSizes)
	assert.Equal(t, 2, f.git.calls)
	assert.Equal(t, 2, due.failures[ref].failures)
	assert.Equal(t, t0.Add(skillRetryBase).Add(2*skillRetryBase), due.failures[ref].retryAt, "the delay doubles")

	f.git.err = nil
	healed := f.resolver(maxSkillBodiesBytes, t0.Add(time.Hour)).
		resolve(context.Background(), skillTestNS, []string{ref}, nil, due.failures, noSizes)
	assert.Equal(t, []string{ref}, healed.mounted)
	assert.Empty(t, healed.failures)

	assert.Equal(t, skillRetryMax, skillRetryDelay(40), "the delay is capped")
}

// The condition is readable by anyone who can read the agent, so it names the user's own pointers and
// never an error string, which can carry the store's internal address.
func TestTheConditionKeepsFetchErrorsOut(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.uploadSkill(t, "escalation", "skills/team-a/escalation", "b", "b")
	f.objects.store = objectstore.NewMemObjectStore() // the object is gone

	out := f.resolve(maxSkillBodiesBytes, ref)
	require.Len(t, out.errs, 1)
	require.Error(t, out.errs[0].cause)

	cond := skillBodiesCondition(1, 1, out.problems)
	assert.Equal(t, reasonSkillFetchFailed, cond.Reason)
	assert.Contains(t, cond.Message, "skills/team-a/escalation")
	assert.NotContains(t, cond.Message, out.errs[0].cause.Error())
}

// A body already verified in the agent's ConfigMap stands in for a fetch, so a running agent never
// loses a mounted body to a later outage. A value that no longer hashes to the digest is fetched afresh.
func TestAVerifiedConfigMapStandsInForAFetch(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.gitSkill("refund-policy", "the body", "the body")
	f.git.err = errors.New("git is down")

	kept := corev1.ConfigMap{Data: map[string]string{"refund-policy": "the body", "detached": "an old skill"}}
	have := verifiedSkillBodies([]corev1.ConfigMap{kept}, []string{ref})
	assert.Equal(t, map[string][]byte{ref: []byte("the body")}, have, "only attached, verified bodies are reused")

	r := f.resolver(maxSkillBodiesBytes, time.Now())
	got := r.resolve(context.Background(), skillTestNS, []string{ref}, have, nil, noSizes)
	assert.Equal(t, []string{ref}, got.mounted)
	assert.Zero(t, f.git.calls, "a verified body is not fetched")

	edited := corev1.ConfigMap{Data: map[string]string{"refund-policy": "an edited body"}}
	assert.Empty(t, verifiedSkillBodies([]corev1.ConfigMap{edited}, []string{ref}), "an edited value is not reused")
}

// skillReconciler is a reconciler over a fake client holding deploy, with the fixture's sources.
func skillReconciler(t *testing.T, f *skillFixture, deploy *agentsv1beta1.AgentDeployment, objs ...client.Object) (*AgentDeploymentReconciler, client.Client) {
	t.Helper()
	kube := fake.NewClientBuilder().WithScheme(podTemplateScheme(t)).
		WithObjects(append(objs, deploy)...).
		WithStatusSubresource(&agentsv1beta1.AgentDeployment{}).Build()
	return &AgentDeploymentReconciler{
		Client: kube, Scheme: podTemplateScheme(t),
		Skills: fixtureSkillResolver{f.versions}, SkillGit: f.git, SkillObjects: f.objects,
	}, kube
}

// fixtureSkillResolver resolves digest refs to themselves and looks versions up in the fixture.
type fixtureSkillResolver struct{ skillVersions }

func (fixtureSkillResolver) Resolve(_ context.Context, _ string, r skill.Ref) (skill.Ref, error) {
	if !r.IsDigest() {
		return skill.Ref{}, fmt.Errorf("the fixture resolves digests only, not %q", r.Version)
	}
	return r, nil
}

func (fixtureSkillResolver) Describe(context.Context, string, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func skilledAgent(refs ...string) *agentsv1beta1.AgentDeployment {
	return &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: skillTestNS, UID: "support-uid"},
		Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest", SkillRefs: refs},
	}
}

// supportConfigMaps lists the skill ConfigMaps of skilledAgent.
func supportConfigMaps(t *testing.T, kube client.Client) []corev1.ConfigMap {
	t.Helper()
	var cms corev1.ConfigMapList
	require.NoError(t, kube.List(context.Background(), &cms,
		client.InNamespace(skillTestNS), client.MatchingLabels{skillAgentLabel: "support"}))
	return cms.Items
}

// The pod mounts an immutable, owned ConfigMap holding exactly the verified bodies, keyed by skill
// name — the file name the launcher reads under SKILL_DIR — and the mount moves the revision name.
func TestTheMountedConfigMapHoldsTheVerifiedBodies(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.gitSkill("refund-policy", "Refunds: check the order date first.", "Refunds: check the order date first.")
	deploy := skilledAgent(ref)
	r, kube := skillReconciler(t, f, deploy)

	pod, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)

	cms := supportConfigMaps(t, kube)
	require.Len(t, cms, 1)
	cm := cms[0]
	assert.Equal(t, map[string]string{"refund-policy": "Refunds: check the order date first."}, cm.Data)
	assert.True(t, ptr.Deref(cm.Immutable, false), "a mounted body cannot be edited under a running agent")
	assert.True(t, metav1.IsControlledBy(&cm, deploy), "the ConfigMap is owned by the agent, so it goes with it")
	assert.Equal(t, skillConfigMapName("support", skillBodiesHash([]string{ref})), cm.Name)

	env := envByNameC(pod.containers[0].Env)
	assert.Equal(t, skillMountPath, env[envSkillDir])
	var vol *corev1.Volume
	for i := range pod.volumes {
		if pod.volumes[i].Name == skillVolumeName {
			vol = &pod.volumes[i]
		}
	}
	require.NotNil(t, vol, "the pod carries the skill-body volume")
	assert.Equal(t, cm.Name, vol.ConfigMap.Name)

	cond := apimeta.FindStatusCondition(deploy.Status.Conditions, conditionSkillBodies)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)

	// The same agent with no body mounted (git unreachable on a cold start) has another revision name.
	f2 := newSkillFixture()
	ref2 := f2.gitSkill("refund-policy", "Refunds: check the order date first.", "Refunds: check the order date first.")
	f2.git.err = errors.New("unreachable")
	deploy2 := skilledAgent(ref2)
	r2, _ := skillReconciler(t, f2, deploy2)
	unmounted, err := r2.buildPodTemplate(context.Background(), deploy2)
	require.NoError(t, err, "a fetch failure never fails the pod template")
	assert.NotEqual(t, unmounted.digest, pod.digest, "mounting the bodies moves the revision name")
	assert.NotContains(t, envByNameC(unmounted.containers[0].Env), envSkillDir)
}

// The mounted ConfigMap holds the attached bodies and nothing else. Its name is predictable, so one
// created under it beforehand with an extra key — another skill's body — is replaced, not adopted.
func TestOnlyAttachedBodiesAreMounted(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.gitSkill("refund-policy", "the body", "the body")
	deploy := skilledAgent(ref)
	name := skillConfigMapName("support", skillBodiesHash([]string{ref}))
	planted := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: skillTestNS,
			Labels: map[string]string{skillAgentLabel: "support"},
		},
		Data:      map[string]string{"refund-policy": "the body", "payroll": "a skill this agent never attached"},
		Immutable: ptr.To(true),
	}
	// Owned and labelled as the reconciler would have: only the extra key gives it away.
	planted.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: agentsv1beta1.GroupVersion.String(), Kind: "AgentDeployment",
		Name: deploy.Name, UID: deploy.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}}
	r, kube := skillReconciler(t, f, deploy, planted)

	_, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)

	cms := supportConfigMaps(t, kube)
	require.Len(t, cms, 1)
	assert.Equal(t, map[string]string{"refund-policy": "the body"}, cms[0].Data,
		"a body of a skill that is not attached is never mounted")
}

// Detaching a skill leaves its body out of the next ConfigMap, and the superseded one is pruned.
func TestDetachingASkillRemovesItsBody(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	kept := f.gitSkill("refund-policy", "kept", "kept")
	dropped := f.gitSkill("payroll", "dropped", "dropped")
	deploy := skilledAgent(kept, dropped)
	r, kube := skillReconciler(t, f, deploy)

	_, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	require.Len(t, supportConfigMaps(t, kube), 1)

	deploy.Spec.SkillRefs = []string{kept}
	_, err = r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	cms := supportConfigMaps(t, kube)
	require.Len(t, cms, 1, "the superseded ConfigMap is pruned")
	assert.Equal(t, map[string]string{"refund-policy": "kept"}, cms[0].Data)

	deploy.Spec.SkillRefs = nil
	pod, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	assert.Empty(t, supportConfigMaps(t, kube), "no skill attached, no body kept")
	assert.NotContains(t, envByNameC(pod.containers[0].Env), envSkillDir)
	assert.Nil(t, apimeta.FindStatusCondition(deploy.Status.Conditions, conditionSkillBodies))
}

// A fetch failure schedules the reconcile for the retry; nothing else does.
func TestAFetchFailureRequeuesAtItsRetry(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.gitSkill("refund-policy", "body", "body")
	f.git.err = errors.New("unreachable")
	deploy := skilledAgent(ref)
	r, _ := skillReconciler(t, f, deploy)

	_, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)

	got := r.requeueForSkillBodies(ctrl.Result{}, skillTestNS, "support")
	assert.Positive(t, got.RequeueAfter)
	assert.LessOrEqual(t, got.RequeueAfter, skillRetryBase)
	assert.Equal(t, time.Minute/2, r.requeueForSkillBodies(ctrl.Result{RequeueAfter: time.Minute / 2}, "other", "agent").RequeueAfter,
		"another agent's retry is not this one's")

	f.git.err = nil
	r.skillFailures.Store(skillTestNS+"/support", skillFailureState{}) // the retry is due
	_, err = r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	assert.Zero(t, r.requeueForSkillBodies(ctrl.Result{}, skillTestNS, "support").RequeueAfter)
}
