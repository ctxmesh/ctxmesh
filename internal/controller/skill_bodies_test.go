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
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	state    *skillFetchState
}

func newSkillFixture() *skillFixture {
	return &skillFixture{
		versions: skillVersions{},
		git:      &fakeGit{files: map[string]string{}},
		objects:  &countingObjects{store: objectstore.NewMemObjectStore()},
		state:    &skillFetchState{},
	}
}

const skillTestSHA = "cccccccccccccccccccccccccccccccccccccccc"

// gitSkill registers a git-sourced version of name whose source holds served (nothing when served is
// ""); the version's digest is that of pinned, which differs from served only when a test wants it to.
func (f *skillFixture) gitSkill(name, pinned, served string) string {
	return f.gitSkillAt("https://github.com/acme/skills", skillTestSHA, name, pinned, served)
}

func (f *skillFixture) gitSkillAt(repo, ref, name, pinned, served string) string {
	digest := skill.Digest([]byte(pinned))
	v := skill.SkillVersion{
		Namespace: skillTestNS, Skill: name, Digest: digest, Source: skill.SourceGit,
		Repo: repo, Ref: ref, Path: name + "/SKILL.md",
	}
	f.versions[name+"@"+digest] = v
	if served != "" {
		f.git.files[v.Repo+"|"+v.Ref+"|"+v.Path] = served
	}
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
		versions: f.versions, git: f.git, objects: f.objects, state: f.state,
		capBytes: capBytes, now: func() time.Time { return now },
	}
}

func (f *skillFixture) resolveAt(now time.Time, resolved ...string) skillBodyOutcome {
	return f.resolver(maxSkillBodiesBytes, now).resolve(context.Background(), skillTestNS, resolved, nil)
}

func (f *skillFixture) resolve(capBytes int, resolved ...string) skillBodyOutcome {
	return f.resolver(capBytes, time.Now()).resolve(context.Background(), skillTestNS, resolved, nil)
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
	assert.True(t, out.retryAt.IsZero(), "nothing to retry")
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

// Whoever adds a skill version names the file the controller fetches. Telling "no file there" from
// "a different file there" would tell them which paths exist on a source the controller can reach and
// they may not, so the two read the same.
func TestAMissingFileAndAMismatchReadTheSame(t *testing.T) {
	t.Parallel()

	missing := newSkillFixture()
	ref := missing.gitSkill("refund-policy", "the reviewed body", "")
	different := newSkillFixture()
	require.Equal(t, ref, different.gitSkill("refund-policy", "the reviewed body", "another body"))

	a := missing.resolve(maxSkillBodiesBytes, ref)
	b := different.resolve(maxSkillBodiesBytes, ref)

	require.Len(t, a.problems, 1)
	assert.Equal(t, a.problems, b.problems, "a missing file and a mismatched one are reported identically")
	assert.Equal(t, skillBodiesCondition(1, 1, a.problems), skillBodiesCondition(1, 1, b.problems))
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
	assert.True(t, out.retryAt.IsZero(), "a body too large stays too large: nothing to retry")

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
	n, known := f.state.size(skillTestNS + "/" + big)
	assert.True(t, known, "the verified size is kept, so it is not fetched again")
	assert.Equal(t, maxSkillBodiesBytes+1, n)
}

// A source holding more than any agent may mount is reported as too large, not as a digest mismatch,
// although the read stopped before the bytes could be hashed.
func TestAnOversizedSourceIsTooLargeNotAMismatch(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	up := f.uploadSkill(t, "huge-upload", "skills/team-a/huge", "x", strings.Repeat("x", maxSkillBodiesBytes+1))
	git := f.gitSkill("huge-git", "y", "y")
	f.git.err = fmt.Errorf("%w: over the limit", prompt.ErrTooLarge)

	out := f.resolve(maxSkillBodiesBytes, up, git)

	assert.Equal(t, map[string]string{
		"huge-upload": reasonSkillTooLarge,
		"huge-git":    reasonSkillTooLarge,
	}, problemReasons(out))
	assert.True(t, out.retryAt.IsZero())
}

// A known size that cannot fit is decided without fetching the body again.
func TestAKnownTooLargeBodyIsNotFetchedAgain(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	big := f.gitSkill("big", "0123456789", "0123456789")
	f.state.recordSize(skillTestNS+"/"+big, 10)

	out := f.resolve(5, big)

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

// A git pointer stored before ValidateVersion bounded and cleaned it is refused, never fetched, and a
// repository off github.com is not one the controller fetches from.
func TestAnUnfetchableGitPointerIsRefused(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	dirty := f.gitSkillAt("https://github.com/acme/skills", skillTestSHA+"/../../other", "dirty", "a", "a")
	elsewhere := f.gitSkillAt("https://gitlab.com/acme/skills", skillTestSHA, "elsewhere", "b", "b")

	out := f.resolve(maxSkillBodiesBytes, dirty, elsewhere)

	assert.Equal(t, map[string]string{
		"dirty":     reasonSkillSourceRefused,
		"elsewhere": reasonSkillSourceUnavailable,
	}, problemReasons(out))
	assert.Zero(t, f.git.calls)
	assert.True(t, out.retryAt.IsZero(), "only a new version changes either, so neither schedules a retry")
}

// Without a source this install can read, a body is reported unavailable, not attempted.
func TestAnUnreadableSourceIsReported(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	git := f.gitSkill("refund-policy", "a", "a")
	up := f.uploadSkill(t, "escalation", "skills/team-a/escalation", "b", "b")
	r := skillBodyResolver{versions: f.versions, state: f.state, capBytes: maxSkillBodiesBytes, now: time.Now}

	out := r.resolve(context.Background(), skillTestNS, []string{git, up}, nil)

	assert.Empty(t, out.bodies)
	assert.Equal(t, map[string]string{
		"refund-policy": reasonSkillSourceUnavailable,
		"escalation":    reasonSkillSourceUnavailable,
	}, problemReasons(out))
	assert.True(t, out.retryAt.IsZero(), "only a configuration change clears this, so it schedules no retry")
}

// A mismatch at a pinned git ref is permanent: it is reported and waits for a new version. An object
// key can be written again, so a mismatch there is retried.
func TestOnlyAMutableSourceRetriesAMismatch(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	git := f.gitSkill("pinned", "reviewed", "different")
	t0 := time.Now()
	out := f.resolveAt(t0, git)
	assert.Equal(t, map[string]string{"pinned": reasonSkillDigestMismatch}, problemReasons(out))
	assert.True(t, out.retryAt.IsZero(), "a mismatch at an immutable ref schedules no retry")

	f.resolveAt(t0.Add(skillRetryMax/2), git)
	assert.Equal(t, 1, f.git.calls, "and is not fetched again on the reconciles that come anyway")

	up := f.uploadSkill(t, "uploaded", "skills/team-a/uploaded", "reviewed", "different")
	out = f.resolveAt(t0, up)
	assert.Equal(t, t0.Add(skillRetryBase), out.retryAt, "an object can be written again, so it is retried")
}

// A failed fetch is retried on a doubling backoff, and not before: reconciles run on every Knative
// status change, and an unreachable git host must not be asked on each one. The failure belongs to
// the version, so another agent attaching it waits too.
func TestAFailedFetchBacksOff(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.gitSkill("refund-policy", "body", "body")
	f.git.err = errors.New("unexpected status 503")
	t0 := time.Now()

	first := f.resolveAt(t0, ref)
	assert.Equal(t, map[string]string{"refund-policy": reasonSkillFetchFailed}, problemReasons(first))
	assert.Equal(t, t0.Add(skillRetryBase), first.retryAt)
	assert.Equal(t, 1, f.git.calls)

	early := f.resolveAt(t0.Add(skillRetryBase/2), ref)
	assert.Equal(t, 1, f.git.calls, "not fetched again before the retry is due, by this agent or another")
	assert.Equal(t, first.problems, early.problems, "the failure is carried, not forgotten")
	assert.Equal(t, t0.Add(skillRetryBase), early.retryAt)

	due := f.resolveAt(t0.Add(skillRetryBase), ref)
	assert.Equal(t, 2, f.git.calls)
	assert.Equal(t, t0.Add(skillRetryBase).Add(2*skillRetryBase), due.retryAt, "the delay doubles")

	f.git.err = nil
	healed := f.resolveAt(t0.Add(time.Hour), ref)
	assert.Equal(t, []string{ref}, healed.mounted)
	assert.True(t, healed.retryAt.IsZero())
	_, failed := f.state.failure(skillTestNS + "/" + ref)
	assert.False(t, failed)

	assert.Equal(t, skillRetryMax, skillRetryDelay(40), "the delay is capped")
}

// A host that fails is tried once per backoff window for the whole controller: every other skill on
// it fails fast meanwhile, without a request, so failing agents cannot hold every worker.
func TestAnUnreachableHostIsTriedOncePerWindow(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	a := f.gitSkill("a", "a", "a")
	b := f.gitSkill("b", "b", "b")
	f.git.err = errors.New("dial tcp: i/o timeout")
	t0 := time.Now()

	out := f.resolveAt(t0, a, b)
	assert.Equal(t, 1, f.git.calls, "the second skill on the failed host is not attempted")
	assert.Equal(t, map[string]string{"a": reasonSkillFetchFailed, "b": reasonSkillFetchFailed}, problemReasons(out))
	_, recorded := f.state.failure(skillTestNS + "/" + b)
	assert.False(t, recorded, "a skill skipped for its host is not counted as failed itself")

	f.resolveAt(t0.Add(skillRetryBase/2), b)
	assert.Equal(t, 1, f.git.calls, "still within the host's window")

	f.git.err = nil
	healed := f.resolveAt(t0.Add(skillRetryBase), b)
	assert.Equal(t, []string{b}, healed.mounted, "after the window one attempt goes through, and heals the host")

	ok, _ := f.state.acquireHost("probe", t0)
	require.True(t, ok)
	f.state.releaseHost("probe", true, t0)
	ok, _ = f.state.acquireHost("probe", t0.Add(skillRetryBase))
	require.True(t, ok, "the first caller after the window probes")
	ok, _ = f.state.acquireHost("probe", t0.Add(skillRetryBase))
	assert.False(t, ok, "one probe at a time")
}

// A missing or oversized file is an answer from a healthy host, so it does not hold up other skills.
func TestAMissingFileDoesNotTripTheHostBreaker(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	missing := f.gitSkill("missing", "a", "")
	present := f.gitSkill("present", "b", "b")

	out := f.resolve(maxSkillBodiesBytes, missing, present)

	assert.Equal(t, []string{present}, out.mounted)
	assert.Equal(t, 2, f.git.calls)
}

// A skill the fetch budget did not reach is tried on the next reconcile, and is not recorded as a
// failure: it never failed.
func TestASkillNotReachedInTimeIsNotAFailure(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.gitSkill("late", "a", "a")
	spent, cancel := context.WithCancel(context.Background())
	cancel()
	t0 := time.Now()

	out := f.resolver(maxSkillBodiesBytes, t0).resolve(spent, skillTestNS, []string{ref}, nil)

	assert.Equal(t, map[string]string{"late": reasonSkillFetchFailed}, problemReasons(out))
	assert.Zero(t, f.git.calls)
	_, recorded := f.state.failure(skillTestNS + "/" + ref)
	assert.False(t, recorded)
	assert.Equal(t, t0.Add(skillRetryBase), out.retryAt, "and the reconcile comes back for it")
}

// The condition is readable by anyone who can read the agent, so it names the user's own pointers and
// never an error string, which can carry the store's internal address.
func TestTheConditionKeepsFetchErrorsOut(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.gitSkill("refund-policy", "b", "b")
	f.git.err = errors.New("dial tcp 10.0.0.7:443: connect: connection refused")

	out := f.resolve(maxSkillBodiesBytes, ref)
	require.Len(t, out.errs, 1)

	cond := skillBodiesCondition(1, 1, out.problems)
	assert.Equal(t, reasonSkillFetchFailed, cond.Reason)
	assert.Contains(t, cond.Message, "refund-policy/SKILL.md")
	assert.NotContains(t, cond.Message, "10.0.0.7")
}

// A version's pointers are echoed into the message of every agent attaching it, so the message is cut
// to stay far below the API server's cap whatever they hold, and cut on a rune boundary.
func TestTheConditionStaysUnderTheCap(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", 20000)
	problems := make([]skillBodyProblem, 0, 16)
	for i := range 16 {
		problems = append(problems, skillBodyProblem{
			name: fmt.Sprintf("skill-%d", i), reason: reasonSkillFetchFailed, detail: "it could not be fetched from path " + long,
		})
	}
	cond := skillBodiesCondition(1, 16, problems)

	assert.LessOrEqual(t, len(cond.Message), maxSkillMessageBytes)
	assert.Less(t, maxSkillMessageBytes, 32768, "the CRD's own cap")
	assert.True(t, utf8.ValidString(cond.Message), "cut on a rune boundary")
	assert.Equal(t, "ab…", clip("abcdefgh", 5))
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

	got := f.resolver(maxSkillBodiesBytes, time.Now()).resolve(context.Background(), skillTestNS, []string{ref}, have)
	assert.Equal(t, []string{ref}, got.mounted)
	assert.Zero(t, f.git.calls, "a verified body is not fetched")

	edited := corev1.ConfigMap{Data: map[string]string{"refund-policy": "an edited body"}}
	assert.Empty(t, verifiedSkillBodies([]corev1.ConfigMap{edited}, []string{ref}), "an edited value is not reused")
}

// skillReconciler is a reconciler over a fake client holding deploy, with the fixture's sources. lists
// counts the ConfigMap Lists it sends.
func skillReconciler(t *testing.T, f *skillFixture, deploy *agentsv1beta1.AgentDeployment, objs ...client.Object) (*AgentDeploymentReconciler, client.Client, *int) {
	t.Helper()
	lists := new(int)
	kube := fake.NewClientBuilder().WithScheme(podTemplateScheme(t)).
		WithObjects(append(objs, deploy)...).
		WithStatusSubresource(&agentsv1beta1.AgentDeployment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.ConfigMapList); ok {
					*lists++
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	return &AgentDeploymentReconciler{
		Client: kube, Scheme: podTemplateScheme(t),
		Skills: fixtureSkillResolver{f.versions}, SkillGit: f.git, SkillObjects: f.objects,
	}, kube, lists
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

// ownedBy makes deploy the controller owner of obj, as the reconciler would have.
func ownedBy(deploy *agentsv1beta1.AgentDeployment) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion: agentsv1beta1.GroupVersion.String(), Kind: "AgentDeployment",
		Name: deploy.Name, UID: deploy.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}}
}

// The pod mounts an immutable, owned ConfigMap holding exactly the verified bodies, keyed by skill
// name — the file name the launcher reads under SKILL_DIR — through an optional volume, and the mount
// moves the revision name.
func TestTheMountedConfigMapHoldsTheVerifiedBodies(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.gitSkill("refund-policy", "Refunds: check the order date first.", "Refunds: check the order date first.")
	deploy := skilledAgent(ref)
	r, kube, _ := skillReconciler(t, f, deploy)

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
	assert.True(t, ptr.Deref(vol.ConfigMap.Optional, false),
		"a pod still starts without the ConfigMap; the launcher then answers 501")

	cond := apimeta.FindStatusCondition(deploy.Status.Conditions, conditionSkillBodies)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)

	// The same agent with no body mounted (git unreachable on a cold start) has another revision name.
	f2 := newSkillFixture()
	ref2 := f2.gitSkill("refund-policy", "Refunds: check the order date first.", "Refunds: check the order date first.")
	f2.git.err = errors.New("unreachable")
	deploy2 := skilledAgent(ref2)
	r2, _, _ := skillReconciler(t, f2, deploy2)
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
	planted := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: skillConfigMapName("support", skillBodiesHash([]string{ref})), Namespace: skillTestNS,
			Labels:          map[string]string{skillAgentLabel: "support"},
			OwnerReferences: ownedBy(deploy), // owned and labelled as the reconciler would: only the extra key gives it away
		},
		Data:      map[string]string{"refund-policy": "the body", "payroll": "a skill this agent never attached"},
		Immutable: ptr.To(true),
	}
	r, kube, _ := skillReconciler(t, f, deploy, planted)

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
	r, kube, _ := skillReconciler(t, f, deploy)

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

// A ConfigMap carrying the agent's label but not controlled by it is someone else's, and is never
// pruned.
func TestAConfigMapTheAgentDoesNotControlIsNotPruned(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.gitSkill("refund-policy", "body", "body")
	deploy := skilledAgent(ref)
	theirs := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "support-skills-theirs", Namespace: skillTestNS,
			Labels: map[string]string{skillAgentLabel: "support"},
		},
		Data: map[string]string{"notes": "not the controller's"},
	}
	r, kube, _ := skillReconciler(t, f, deploy, theirs)

	_, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)

	var got corev1.ConfigMap
	require.NoError(t, kube.Get(context.Background(), client.ObjectKeyFromObject(theirs), &got),
		"a ConfigMap the agent does not control survives the prune")
}

// A bare Job's pod template never changes after it is created, so the ConfigMap it mounts stays while
// the Job may still start a pod (a retry), whatever the agent's current bodies are; once the Job has
// finished it may go.
func TestAJobsConfigMapIsKeptUntilItFinishes(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	first := f.gitSkill("first", "one", "one")
	second := f.gitSkill("second", "two", "two")
	deploy := skilledAgent(first)
	r, kube, _ := skillReconciler(t, f, deploy)

	pod, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: skillTestNS, OwnerReferences: ownedBy(deploy)},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: pod.volumes}}},
	}
	require.NoError(t, kube.Create(context.Background(), job))

	deploy.Spec.SkillRefs = []string{second}
	_, err = r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	assert.Len(t, supportConfigMaps(t, kube), 2, "the running Job's ConfigMap is kept")

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	require.NoError(t, kube.Status().Update(context.Background(), job))
	_, err = r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	assert.Len(t, supportConfigMaps(t, kube), 1, "a finished Job starts no pod, so its ConfigMap goes")
}

// One reconcile resolves the bodies once: the second pod-template build reuses the first's result. An
// agent that never had skills lists nothing.
func TestBodiesAreResolvedOncePerReconcile(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	ref := f.gitSkill("refund-policy", "body", "body")
	f.git.err = errors.New("unreachable")
	deploy := skilledAgent(ref)
	r, _, lists := skillReconciler(t, f, deploy)

	ctx, memo := withSkillBodiesMemo(context.Background())
	first, err := r.buildPodTemplate(ctx, deploy)
	require.NoError(t, err)
	f.git.err = nil // were the second build to fetch, it would now come out different
	second, err := r.buildPodTemplate(ctx, deploy)
	require.NoError(t, err)
	assert.Equal(t, 1, *lists, "one ConfigMap List per reconcile")
	assert.Equal(t, 1, f.git.calls, "one fetch per reconcile")
	assert.Equal(t, first.digest, second.digest, "the candidate's name is the Service's")
	assert.Positive(t, requeueForSkillBodies(ctrl.Result{}, memo).RequeueAfter, "the failure schedules its retry")
	assert.LessOrEqual(t, requeueForSkillBodies(ctrl.Result{}, memo).RequeueAfter, skillRetryBase)
	assert.Equal(t, time.Minute/2, requeueForSkillBodies(ctrl.Result{RequeueAfter: time.Minute / 2}, &skillBodiesMemo{}).RequeueAfter,
		"no retry leaves the result alone")

	plain := skilledAgent()
	plain.Name, plain.UID = "plain", "plain-uid"
	r2, _, lists2 := skillReconciler(t, newSkillFixture(), plain)
	_, err = r2.buildPodTemplate(context.Background(), plain)
	require.NoError(t, err)
	assert.Zero(t, *lists2, "an agent that never had skills sends no List")
}

// While an eventing agent's Deployment is mid-rollout, an old ReplicaSet may still replace a pod from
// its own template, so no superseded ConfigMap is pruned until the rollout completes.
func TestADeploymentMidRolloutHoldsThePrune(t *testing.T) {
	t.Parallel()

	f := newSkillFixture()
	first := f.gitSkill("first", "one", "one")
	second := f.gitSkill("second", "two", "two")
	deploy := skilledAgent(first)
	r, kube, _ := skillReconciler(t, f, deploy)

	_, err := r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: skillTestNS, OwnerReferences: ownedBy(deploy)},
	}
	require.NoError(t, kube.Create(context.Background(), dep))
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 2, UpdatedReplicas: 1}
	require.NoError(t, kube.Status().Update(context.Background(), dep))

	deploy.Spec.SkillRefs = []string{second}
	_, err = r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	assert.Len(t, supportConfigMaps(t, kube), 2, "mid-rollout, the old pods' ConfigMap is kept")

	dep.Status.UpdatedReplicas = 2
	require.NoError(t, kube.Status().Update(context.Background(), dep))
	_, err = r.buildPodTemplate(context.Background(), deploy)
	require.NoError(t, err)
	assert.Len(t, supportConfigMaps(t, kube), 1, "once the rollout completes, it goes")
}
