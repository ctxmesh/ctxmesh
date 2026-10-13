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
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/skill"
	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/prompt"
)

// Skill bodies (ADR 0137) are what the launcher's POST /skills/load returns. The controller fetches
// each attached skill's pinned version, checks the bytes hash to the version's digest, and mounts the
// verified bodies read-only at SKILL_DIR, one file per skill name. Only attached skills are fetched,
// and a body enters the model's context only when the agent calls load().
const (
	// skillConfigMapSuffix names an agent's skill-body ConfigMaps: <agent>-skills-<hash8>, where the
	// hash is over the mounted refs. A different mounted set is a different ConfigMap, so a revision
	// never sees the bodies under its mount change.
	skillConfigMapSuffix = "-skills-"

	// skillAgentLabel groups an agent's skill-body ConfigMaps so superseded ones can be pruned.
	skillAgentLabel = "agents.ctxmesh.ai/skill-agent"

	// maxSkillBodiesBytes caps the bodies one agent mounts. They share one ConfigMap, which the API
	// server refuses above 1 MiB of keys and values; 900 KiB leaves room for the keys and for the
	// object's metadata. A body is never truncated: one that does not fit is left out whole, and a
	// fetch stops reading one byte past this, since a longer body can never be mounted.
	maxSkillBodiesBytes = 900 << 10

	// conditionSkillBodies reports whether every attached skill's body is mounted. False names each
	// skill whose body is not, and why; load() answers 501 for those.
	conditionSkillBodies = "SkillBodies"

	reasonSkillBodiesMounted     = "Mounted"
	reasonSkillDigestMismatch    = "DigestMismatch"
	reasonSkillSourceRefused     = "SourceRefused"
	reasonSkillTooLarge          = "TooLarge"
	reasonSkillFetchFailed       = "FetchFailed"
	reasonSkillSourceUnavailable = "SourceUnavailable"

	// skillFetchBudget bounds the fetching in one reconcile. The reconcile waits for it and holds one
	// of the few AgentDeployment workers (agentDeploymentWorkers) meanwhile; a skill not reached in
	// time is tried on the next reconcile, without counting as a failure.
	skillFetchBudget = 10 * time.Second

	// skillDialTimeout bounds connecting to a source, its TLS handshake and its response headers, so
	// an unreachable host costs seconds, once per backoff window (skillFetchState's host breaker).
	skillDialTimeout = 3 * time.Second

	// skillRetryBase and skillRetryMax bound the backoff between attempts at a failed body, and at a
	// host that failed. A failure only a new version can clear is looked at again after skillRetryMax,
	// on whatever reconcile comes next, and schedules none of its own.
	skillRetryBase = 10 * time.Second
	skillRetryMax  = 10 * time.Minute

	// maxSkillDetailBytes and maxSkillMessageBytes keep the SkillBodies message far below the 32768
	// the API server allows a condition message, whatever a version's pointers hold.
	maxSkillDetailBytes  = 512
	maxSkillMessageBytes = 8192

	// githubHost is the one git host skill bodies are fetched from (prompt.HTTPResolver).
	githubHost = "github.com"
	// objectStoreHost names the object store in the host breaker.
	objectStoreHost = "object-store"
)

// skillReasonSeverity orders the reasons for the condition's Reason when several skills fail: the
// first is the one most worth acting on.
var skillReasonSeverity = []string{
	reasonSkillDigestMismatch, reasonSkillSourceRefused, reasonSkillTooLarge,
	reasonSkillFetchFailed, reasonSkillSourceUnavailable,
}

// NewSkillGitFetcher returns the git fetcher for AgentDeploymentReconciler.SkillGit: public
// github.com repositories only. It sends no token, on purpose: anyone who can add a skill version
// names the file, so a token here would let them read, through load(), any file the token can read.
func NewSkillGitFetcher() prompt.Resolver {
	return prompt.NewHTTPResolverWithOptions(prompt.HTTPResolverOptions{
		MaxBytes:    maxSkillBodiesBytes,
		DialTimeout: skillDialTimeout,
		Timeout:     skillFetchBudget,
	})
}

// skillObjectReader reads an uploaded skill body. objectstore.ObjectStore satisfies it.
type skillObjectReader interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// skillVersionLookup finds the version a pinned ref names. skill.Resolver satisfies it.
type skillVersionLookup interface {
	Version(ctx context.Context, namespace, name, digest string) (skill.SkillVersion, bool, error)
}

// skillBodyProblem is one attached skill whose body is not mounted. detail is written for the
// condition message: it names the user's own pointers, never an error string, which can carry the
// store's internal addresses, and never the digest of bytes that failed verification, which would
// let a caller learn the hash of an object it cannot read.
type skillBodyProblem struct {
	name   string
	reason string
	detail string
}

// skillFetchFailure is a body that could not be mounted, kept so it is not fetched again before
// retryAt. requeue says whether a reconcile is scheduled for retryAt.
type skillFetchFailure struct {
	failures int
	retryAt  time.Time
	requeue  bool
	problem  skillBodyProblem
}

// skillHostBreaker spaces out attempts at a host that failed: none before openUntil, then one at a
// time until one succeeds.
type skillHostBreaker struct {
	failures  int
	openUntil time.Time
	probing   bool
}

// skillFetchState is the controller-wide state of skill-body fetches, shared by every agent: what one
// agent learned about a version or a host, another does not pay for again.
type skillFetchState struct {
	mu sync.Mutex
	// failures holds, per "<namespace>/<name>@<digest>", the failure to wait out.
	failures map[string]skillFetchFailure
	// hosts holds, per source host, its breaker. A healthy host has none.
	hosts map[string]*skillHostBreaker
	// sizes holds, per "<namespace>/<name>@<digest>", the size of a verified body. A digest names
	// fixed bytes, so an entry never goes stale.
	sizes map[string]int
	// pendingPrune holds the "<namespace>/<name>" of agents with no skills whose superseded
	// ConfigMaps could not all be pruned yet, so their next reconciles look again.
	pendingPrune map[string]bool
}

// lock takes the mutex and makes the maps; the caller unlocks.
func (s *skillFetchState) lock() {
	s.mu.Lock()
	if s.failures == nil {
		s.failures = map[string]skillFetchFailure{}
		s.hosts = map[string]*skillHostBreaker{}
		s.sizes = map[string]int{}
		s.pendingPrune = map[string]bool{}
	}
}

func (s *skillFetchState) failure(key string) (skillFetchFailure, bool) {
	s.lock()
	defer s.mu.Unlock()
	f, ok := s.failures[key]
	return f, ok
}

// recordFailure keeps f, and drops failures nobody has looked at for a full backoff window.
func (s *skillFetchState) recordFailure(key string, f skillFetchFailure, now time.Time) {
	s.lock()
	defer s.mu.Unlock()
	for k, old := range s.failures {
		if now.Sub(old.retryAt) > skillRetryMax {
			delete(s.failures, k)
		}
	}
	s.failures[key] = f
}

func (s *skillFetchState) clearFailure(key string) {
	s.lock()
	defer s.mu.Unlock()
	delete(s.failures, key)
}

func (s *skillFetchState) size(key string) (int, bool) {
	s.lock()
	defer s.mu.Unlock()
	n, ok := s.sizes[key]
	return n, ok
}

func (s *skillFetchState) recordSize(key string, n int) {
	s.lock()
	defer s.mu.Unlock()
	s.sizes[key] = n
}

func (s *skillFetchState) prunePending(agentKey string) bool {
	s.lock()
	defer s.mu.Unlock()
	return s.pendingPrune[agentKey]
}

func (s *skillFetchState) setPrunePending(agentKey string, pending bool) {
	s.lock()
	defer s.mu.Unlock()
	if pending {
		s.pendingPrune[agentKey] = true
	} else {
		delete(s.pendingPrune, agentKey)
	}
}

// acquireHost reports whether a fetch from host may go ahead now. While the host's window is open it
// may not, and the caller is told when to try again; once the window has passed, one caller at a time
// may, until one of them succeeds. Every true answer must be followed by releaseHost.
func (s *skillFetchState) acquireHost(host string, now time.Time) (bool, time.Time) {
	s.lock()
	defer s.mu.Unlock()
	b := s.hosts[host]
	switch {
	case b == nil:
		return true, time.Time{}
	case now.Before(b.openUntil):
		return false, b.openUntil
	case b.probing:
		return false, now.Add(skillRetryBase)
	}
	b.probing = true
	return true, time.Time{}
}

// releaseHost records how a fetch from host went: hostFailed only for a failure of the host itself
// (unreachable, timing out, answering 5xx), not for a missing, oversized or mismatched file.
func (s *skillFetchState) releaseHost(host string, hostFailed bool, now time.Time) {
	s.lock()
	defer s.mu.Unlock()
	if !hostFailed {
		delete(s.hosts, host)
		return
	}
	b := s.hosts[host]
	if b == nil {
		b = &skillHostBreaker{}
		s.hosts[host] = b
	}
	b.probing = false
	if now.Before(b.openUntil) {
		return // another worker's failure already opened this window
	}
	b.failures++
	b.openUntil = now.Add(skillRetryDelay(b.failures))
}

// skillBodyError is why one body was not fetched. cause is for the controller log only.
type skillBodyError struct {
	skillBodyProblem
	// record says the failure is kept for the ref, so it is not fetched again before its retry. A
	// skill that was never tried, or whose host's breaker is open, is not a failure of the ref.
	record  bool
	requeue bool
	// retryAt is when to look again for a failure that is not recorded.
	retryAt time.Time
	cause   error
}

// skillBodyOutcome is one resolution of an agent's attached skill bodies.
type skillBodyOutcome struct {
	// bodies maps a skill name to its verified body; only mounted skills are in it.
	bodies map[string][]byte
	// mounted lists the refs whose bodies are in bodies, in spec order.
	mounted []string
	// problems lists the attached skills whose bodies are not mounted, in spec order.
	problems []skillBodyProblem
	// retryAt is the earliest time a reconcile should look again; zero when none needs to.
	retryAt time.Time
	// errs are the fetch errors of this resolution that have a cause, for the log.
	errs []*skillBodyError
}

func (o *skillBodyOutcome) retryNoLaterThan(t time.Time) {
	if !t.IsZero() && (o.retryAt.IsZero() || t.Before(o.retryAt)) {
		o.retryAt = t
	}
}

// skillBodyResolver resolves an agent's attached skill bodies. Its sources are interfaces so the
// unit tests drive every path without a network.
type skillBodyResolver struct {
	versions skillVersionLookup
	git      prompt.Resolver
	objects  skillObjectReader
	state    *skillFetchState
	capBytes int
	now      func() time.Time
}

// resolve returns the bodies to mount for the pinned refs, in spec order. A body already verified in
// one of the agent's ConfigMaps (have, by ref) is reused; any other is fetched and verified, unless a
// failure recorded for the ref is not yet due for retry, or its known size cannot fit. Bodies are
// packed in spec order: one that would take the total over capBytes is left out whole, and a later,
// smaller one may still fit.
func (s skillBodyResolver) resolve(
	ctx context.Context, namespace string, resolved []string, have map[string][]byte,
) skillBodyOutcome {
	out := skillBodyOutcome{bodies: map[string][]byte{}}
	remaining := s.capBytes
	for _, ref := range resolved {
		name, digest, ok := strings.Cut(ref, "@")
		if !ok {
			continue // ResolveAll only returns pinned refs
		}
		body, verified := have[ref]
		if !verified {
			var mounted bool
			body, mounted = s.obtain(ctx, &out, namespace, name, digest, remaining)
			if !mounted {
				continue
			}
		}
		if len(body) > remaining {
			out.problems = append(out.problems, skillTooLarge(name, len(body), s.capBytes))
			continue
		}
		remaining -= len(body)
		out.bodies[name] = body
		out.mounted = append(out.mounted, ref)
	}
	return out
}

// obtain fetches one body unless what is known already rules it out, recording the outcome on out
// and in the shared state. It reports whether body is a verified body to pack.
func (s skillBodyResolver) obtain(
	ctx context.Context, out *skillBodyOutcome, namespace, name, digest string, remaining int,
) ([]byte, bool) {
	key := namespace + "/" + name + "@" + digest
	now := s.now()
	if n, known := s.state.size(key); known && n > remaining {
		out.problems = append(out.problems, skillTooLarge(name, n, s.capBytes))
		return nil, false
	}
	if f, failed := s.state.failure(key); failed && now.Before(f.retryAt) {
		out.problems = append(out.problems, f.problem)
		if f.requeue {
			out.retryNoLaterThan(f.retryAt)
		}
		return nil, false
	}

	var body []byte
	var ferr *skillBodyError
	if ctx.Err() != nil {
		ferr = &skillBodyError{
			skillBodyProblem: skillBodyProblem{
				name: name, reason: reasonSkillFetchFailed,
				detail: "it was not fetched yet: the time a reconcile gives fetching ran out first",
			},
			requeue: true, retryAt: now.Add(skillRetryBase),
		}
	} else {
		body, ferr = s.fetch(ctx, namespace, name, digest)
	}
	if ferr == nil {
		s.state.clearFailure(key)
		s.state.recordSize(key, len(body))
		return body, true
	}

	out.problems = append(out.problems, ferr.skillBodyProblem)
	if ferr.cause != nil {
		out.errs = append(out.errs, ferr)
	}
	retryAt := ferr.retryAt
	if ferr.record {
		prior, _ := s.state.failure(key)
		attempts := prior.failures + 1
		delay := skillRetryDelay(attempts)
		if !ferr.requeue {
			delay = skillRetryMax
		}
		retryAt = now.Add(delay)
		s.state.recordFailure(key, skillFetchFailure{
			failures: attempts, retryAt: retryAt, requeue: ferr.requeue, problem: ferr.skillBodyProblem,
		}, now)
	}
	if ferr.requeue {
		out.retryNoLaterThan(retryAt)
	}
	return nil, false
}

// skillSource is where one version's bytes are read from.
type skillSource struct {
	// pointer names the source in the condition message, in the user's own terms.
	pointer string
	// host is the breaker the read goes through.
	host string
	// mutable says the bytes behind the pointer can change, so a mismatch is worth retrying.
	mutable bool
	read    func(context.Context) ([]byte, error)
}

// source returns where v's bytes are read from, or why they cannot be.
func (s skillBodyResolver) source(namespace string, v skill.SkillVersion) (skillSource, *skillBodyProblem) {
	refuse := func(reason, detail string) (skillSource, *skillBodyProblem) {
		return skillSource{}, &skillBodyProblem{name: v.Skill, reason: reason, detail: detail}
	}
	switch v.Source {
	case skill.SourceGit:
		pointer := fmt.Sprintf("repo %s, ref %s, path %s", clip(v.Repo, 160), clip(v.Ref, 80), clip(v.Path, 160))
		if err := skill.ValidGitPin(v.Repo, v.Ref, v.Path); err != nil {
			return refuse(reasonSkillSourceRefused, "its git pointer is not a bounded, clean pin ("+pointer+")")
		}
		if u, err := url.Parse(v.Repo); err != nil || !strings.EqualFold(u.Host, githubHost) {
			return refuse(reasonSkillSourceUnavailable,
				"its repository is not on github.com, the one git host skill bodies are fetched from ("+pointer+")")
		}
		if s.git == nil {
			return refuse(reasonSkillSourceUnavailable, "it is sourced from git and this controller has no git fetcher")
		}
		return skillSource{
			pointer: pointer, host: githubHost, mutable: !skill.IsImmutableRef(v.Ref),
			read: func(ctx context.Context) ([]byte, error) {
				res, err := s.git.Resolve(ctx, agentsv1beta1.GitPromptSource{Repo: v.Repo, Ref: v.Ref, Path: v.Path})
				return []byte(res.Content), err
			},
		}, nil
	case skill.SourceUpload:
		pointer := "object key " + clip(v.ObjectKey, 200)
		if !skill.OwnsObjectKey(namespace, v.ObjectKey) {
			return refuse(reasonSkillSourceRefused, fmt.Sprintf(
				"its %s is not a clean key under %s, the only prefix this namespace's skills are read from",
				pointer, skill.ObjectKeyPrefix(namespace)))
		}
		if s.objects == nil {
			return refuse(reasonSkillSourceUnavailable,
				"it was uploaded and this install has no object store to read it from")
		}
		return skillSource{
			pointer: pointer, host: objectStoreHost, mutable: true, // an object key can be written again
			read: func(ctx context.Context) ([]byte, error) { return readSkillObject(ctx, s.objects, v.ObjectKey) },
		}, nil
	}
	return refuse(reasonSkillSourceUnavailable,
		fmt.Sprintf("its version names a source this controller cannot read (%q)", clip(string(v.Source), 40)))
}

// fetch returns the body of one pinned version, verified against its digest. Unverified bytes are
// never returned.
func (s skillBodyResolver) fetch(ctx context.Context, namespace, name, digest string) ([]byte, *skillBodyError) {
	fail := func(reason, detail string, requeue bool, cause error) *skillBodyError {
		return &skillBodyError{
			skillBodyProblem: skillBodyProblem{name: name, reason: reason, detail: clip(detail, maxSkillDetailBytes)},
			record:           true, requeue: requeue, cause: cause,
		}
	}
	v, found, err := s.versions.Version(ctx, namespace, name, digest)
	if err != nil {
		return nil, fail(reasonSkillFetchFailed, "its version could not be read from the skill store", true, err)
	}
	if !found {
		return nil, fail(reasonSkillFetchFailed,
			fmt.Sprintf("its version %s is no longer in the skill store", digest), true, nil)
	}
	src, refused := s.source(namespace, v)
	if refused != nil {
		return nil, fail(refused.reason, refused.detail, false, nil)
	}

	if ok, retryAt := s.state.acquireHost(src.host, s.now()); !ok {
		e := fail(reasonSkillFetchFailed,
			"it was not fetched: an earlier fetch from its source failed, and the source is being retried", true, nil)
		e.record, e.retryAt = false, retryAt
		return nil, e
	}
	body, err := src.read(ctx)
	notThere := errors.Is(err, prompt.ErrNotFound) || objectstore.IsNotFound(err)
	tooLarge := errors.Is(err, prompt.ErrTooLarge) || errors.Is(err, errSkillObjectTooLarge)
	s.state.releaseHost(src.host, err != nil && !notThere && !tooLarge, s.now())
	switch {
	case tooLarge:
		return nil, fail(reasonSkillTooLarge, fmt.Sprintf(
			"its body at %s is over the %d bytes an agent's skill bodies share", src.pointer, maxSkillBodiesBytes),
			false, nil)
	case notThere || (err == nil && skill.Digest(body) != digest):
		// One message for both: telling them apart would tell whoever named the pointer whether a file
		// exists there, on a source the controller may be able to read and they may not.
		return nil, fail(reasonSkillDigestMismatch, fmt.Sprintf(
			"nothing at %s hashes to %s: the file is missing there, or its bytes differ, so nothing is mounted",
			src.pointer, digest), src.mutable, nil)
	case err != nil:
		return nil, fail(reasonSkillFetchFailed, "it could not be fetched from "+src.pointer+"; retrying", true, err)
	}
	return body, nil
}

// errSkillObjectTooLarge is readSkillObject's answer for an object longer than any mountable body.
var errSkillObjectTooLarge = errors.New("the object is larger than an agent's skill bodies may be")

// readSkillObject reads at most one byte more than maxSkillBodiesBytes: a longer object can never be
// mounted, so the rest is never needed.
func readSkillObject(ctx context.Context, objects skillObjectReader, key string) ([]byte, error) {
	rc, err := objects.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(rc, maxSkillBodiesBytes+1))
	if closeErr := rc.Close(); readErr == nil && closeErr != nil {
		readErr = closeErr
	}
	if readErr != nil {
		return nil, readErr
	}
	if len(body) > maxSkillBodiesBytes {
		return nil, errSkillObjectTooLarge
	}
	return body, nil
}

func skillTooLarge(name string, size, capBytes int) skillBodyProblem {
	return skillBodyProblem{
		name:   name,
		reason: reasonSkillTooLarge,
		detail: fmt.Sprintf("its body is %d bytes and does not fit in the %d bytes an agent's skill bodies share "+
			"(skills are packed in spec.skillRefs order)", size, capBytes),
	}
}

// skillRetryDelay doubles from skillRetryBase with each failed attempt, up to skillRetryMax.
func skillRetryDelay(attempts int) time.Duration {
	d := skillRetryBase
	for i := 1; i < attempts && d < skillRetryMax; i++ {
		d *= 2
	}
	return min(d, skillRetryMax)
}

// clip shortens s to at most n bytes, at a rune boundary, marking the cut.
func clip(s string, n int) string {
	const mark = "…"
	if len(s) <= n {
		return s
	}
	cut := max(n-len(mark), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// skillBodiesHash names a mounted set: 8 hex characters over its refs, sorted, because the
// ConfigMap's content does not depend on their order.
func skillBodiesHash(mounted []string) string {
	refs := slices.Clone(mounted)
	slices.Sort(refs)
	h := sha256.Sum256([]byte(strings.Join(refs, ",")))
	return fmt.Sprintf("%x", h[:])[:8]
}

func skillConfigMapName(agentName, hash string) string {
	return agentName + skillConfigMapSuffix + hash
}

// skillMount is what the pod template needs from an agent's skill bodies: the ConfigMap to mount and
// the hash folded into the revision digest, both "" when no body is mounted.
type skillMount struct {
	configMap string
	hash      string
}

// skillVolume is the pod volume for the mounted skill bodies; injectSkills adds the mount. It is
// optional, so a pod still starts if the ConfigMap is gone: the launcher then answers 501 for the
// bodies, which is what they are.
func skillVolume(configMap string) corev1.Volume {
	return corev1.Volume{
		Name: skillVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMap},
				Optional:             ptr.To(true),
			},
		},
	}
}

// skillBodiesMemo carries one reconcile's skill-body resolution from its first pod-template build to
// the next: a serving reconcile builds the template twice (the candidate's name, then the Service),
// and the second must neither fetch again nor come out different.
type skillBodiesMemo struct {
	done    bool
	refs    []string
	mount   skillMount
	retryAt time.Time
}

type skillBodiesMemoKey struct{}

// withSkillBodiesMemo returns ctx carrying a fresh memo for one reconcile.
func withSkillBodiesMemo(ctx context.Context) (context.Context, *skillBodiesMemo) {
	memo := &skillBodiesMemo{}
	return context.WithValue(ctx, skillBodiesMemoKey{}, memo), memo
}

// requeueForSkillBodies schedules the next reconcile no later than the memo's retry: a skill body
// that could not be fetched is tried again on its own backoff.
func requeueForSkillBodies(result ctrl.Result, memo *skillBodiesMemo) ctrl.Result {
	if memo == nil || memo.retryAt.IsZero() {
		return result
	}
	after := max(time.Until(memo.retryAt), time.Second)
	if result.RequeueAfter == 0 || result.RequeueAfter > after {
		result.RequeueAfter = after
	}
	return result
}

// reconcileSkillBodies resolves the attached skills' bodies, writes the verified ones into the
// agent's content-addressed ConfigMap, prunes superseded ones, and reports on the SkillBodies
// condition. A body that cannot be fetched or verified is left out and named on the condition, and
// the rest of the agent deploys: it never fails the reconcile. Only a Kubernetes API error does.
func (r *AgentDeploymentReconciler) reconcileSkillBodies(
	ctx context.Context,
	deploy *agentsv1beta1.AgentDeployment,
	resolved []string,
) (skillMount, error) {
	memo, _ := ctx.Value(skillBodiesMemoKey{}).(*skillBodiesMemo)
	if memo != nil && memo.done && slices.Equal(memo.refs, resolved) {
		return memo.mount, nil
	}
	agentKey := deploy.Namespace + "/" + deploy.Name
	if len(resolved) == 0 && !r.skillFetches.prunePending(agentKey) &&
		apimeta.FindStatusCondition(deploy.Status.Conditions, conditionSkillBodies) == nil {
		// Never had skills, or everything they left is gone: nothing to list.
		return skillMount{}, nil
	}

	var cms corev1.ConfigMapList
	if err := r.List(ctx, &cms,
		client.InNamespace(deploy.Namespace),
		client.MatchingLabels{skillAgentLabel: deploy.Name}); err != nil {
		return skillMount{}, fmt.Errorf("listing skill ConfigMaps: %w", err)
	}

	var mount skillMount
	var out skillBodyOutcome
	if len(resolved) > 0 {
		fetchCtx, cancel := context.WithTimeout(ctx, skillFetchBudget)
		defer cancel()
		resolver := skillBodyResolver{
			versions: r.Skills,
			git:      r.SkillGit,
			objects:  r.skillObjects(ctx),
			state:    &r.skillFetches,
			capBytes: maxSkillBodiesBytes,
			now:      time.Now,
		}
		out = resolver.resolve(fetchCtx, deploy.Namespace, resolved, verifiedSkillBodies(cms.Items, resolved))
		log := logf.FromContext(ctx)
		for _, e := range out.errs {
			log.Error(e.cause, "skill body not mounted", "agent", agentKey, "skill", e.name,
				"reason", e.reason, "detail", e.detail)
		}
		if len(out.mounted) > 0 {
			mount.hash = skillBodiesHash(out.mounted)
			mount.configMap = skillConfigMapName(deploy.Name, mount.hash)
			if err := r.ensureSkillConfigMap(ctx, deploy, mount.configMap, out.bodies); err != nil {
				return skillMount{}, err
			}
		}
	}
	kept, err := r.pruneSkillConfigMaps(ctx, deploy, cms.Items, mount.configMap)
	if err != nil {
		return skillMount{}, err
	}
	r.skillFetches.setPrunePending(agentKey, len(resolved) == 0 && kept > 0)
	if err := r.syncSkillBodiesCondition(ctx, deploy, len(resolved), out.problems); err != nil {
		return skillMount{}, err
	}
	if memo != nil {
		*memo = skillBodiesMemo{done: true, refs: slices.Clone(resolved), mount: mount, retryAt: out.retryAt}
	}
	return mount, nil
}

// verifiedSkillBodies indexes, by ref, the bodies already in the agent's ConfigMaps that hash to an
// attached ref's digest. Hashing them again is what lets an existing ConfigMap stand in for a fetch:
// an edited value fails the check and is fetched afresh.
func verifiedSkillBodies(cms []corev1.ConfigMap, resolved []string) map[string][]byte {
	byName := map[string]string{} // skill name → pinned digest
	for _, ref := range resolved {
		if name, digest, ok := strings.Cut(ref, "@"); ok {
			byName[name] = digest
		}
	}
	have := map[string][]byte{}
	consider := func(key string, body []byte) {
		if digest, ok := byName[key]; ok && skill.Digest(body) == digest {
			have[key+"@"+digest] = body
		}
	}
	for i := range cms {
		for k, v := range cms[i].Data {
			consider(k, []byte(v))
		}
		for k, v := range cms[i].BinaryData {
			consider(k, v)
		}
	}
	return have
}

// desiredSkillConfigMap is the immutable ConfigMap holding the verified bodies, one key per skill
// name, which is the file name the launcher reads. A body that is not UTF-8 goes in binaryData, so
// the file on disk is byte-for-byte the content its digest names.
func (r *AgentDeploymentReconciler) desiredSkillConfigMap(
	deploy *agentsv1beta1.AgentDeployment, name string, bodies map[string][]byte,
) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: deploy.Namespace,
			Labels: map[string]string{skillAgentLabel: deploy.Name},
		},
		// Immutable, so a body cannot be edited under a running agent: the kubelet stops watching an
		// immutable ConfigMap, and the API server refuses a change to its data.
		Immutable: ptr.To(true),
	}
	for k, body := range bodies {
		if utf8.Valid(body) {
			if cm.Data == nil {
				cm.Data = map[string]string{}
			}
			cm.Data[k] = string(body)
			continue
		}
		if cm.BinaryData == nil {
			cm.BinaryData = map[string][]byte{}
		}
		cm.BinaryData[k] = body
	}
	if err := ctrl.SetControllerReference(deploy, cm, r.Scheme); err != nil {
		return nil, fmt.Errorf("owning skill ConfigMap %s: %w", name, err)
	}
	return cm, nil
}

// skillConfigMapHolds reports whether cm holds exactly bodies.
func skillConfigMapHolds(cm *corev1.ConfigMap, bodies map[string][]byte) bool {
	if len(cm.Data)+len(cm.BinaryData) != len(bodies) {
		return false
	}
	for k, body := range bodies {
		if v, ok := cm.Data[k]; ok {
			if v != string(body) {
				return false
			}
			continue
		}
		if v, ok := cm.BinaryData[k]; !ok || !bytes.Equal(v, body) {
			return false
		}
	}
	return true
}

// ensureSkillConfigMap makes the named ConfigMap hold exactly bodies. One that holds anything else
// under that name, edited or created by someone else, is deleted and created again: it is immutable,
// and its content is what the agent will serve.
func (r *AgentDeploymentReconciler) ensureSkillConfigMap(
	ctx context.Context, deploy *agentsv1beta1.AgentDeployment, name string, bodies map[string][]byte,
) error {
	want, err := r.desiredSkillConfigMap(deploy, name, bodies)
	if err != nil {
		return err
	}
	key := client.ObjectKeyFromObject(want)
	var got corev1.ConfigMap
	err = r.Get(ctx, key, &got)
	if apierrors.IsNotFound(err) {
		err = r.Create(ctx, want)
		if err == nil {
			return nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating skill ConfigMap %s: %w", name, err)
		}
		// Created since the cache was last told: read what is there from the API server.
		err = r.uncachedReader().Get(ctx, key, &got)
	}
	if err != nil {
		return fmt.Errorf("reading skill ConfigMap %s: %w", name, err)
	}

	if skillConfigMapHolds(&got, bodies) && ptr.Deref(got.Immutable, false) && metav1.IsControlledBy(&got, deploy) {
		if got.Labels[skillAgentLabel] == deploy.Name {
			return nil
		}
		// Only metadata may change on an immutable ConfigMap; the label is what GC lists by.
		if got.Labels == nil {
			got.Labels = map[string]string{}
		}
		got.Labels[skillAgentLabel] = deploy.Name
		if err := r.Update(ctx, &got); err != nil {
			return fmt.Errorf("labelling skill ConfigMap %s: %w", name, err)
		}
		return nil
	}
	uid := got.UID
	if err := r.Delete(ctx, &got, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("replacing skill ConfigMap %s: %w", name, err)
	}
	if err := r.Create(ctx, want); err != nil {
		return fmt.Errorf("creating skill ConfigMap %s: %w", name, err)
	}
	return nil
}

// uncachedReader reads from the API server when the binary provides a reader that does.
func (r *AgentDeploymentReconciler) uncachedReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// pruneSkillConfigMaps deletes the agent's own skill ConfigMaps other than current that no workload of
// the agent may still start a pod from (configMapsInUse), and returns how many superseded ones it
// kept. A ConfigMap carrying the label but not controlled by the agent is never touched.
func (r *AgentDeploymentReconciler) pruneSkillConfigMaps(
	ctx context.Context, deploy *agentsv1beta1.AgentDeployment, existing []corev1.ConfigMap, current string,
) (int, error) {
	var stale []*corev1.ConfigMap
	for i := range existing {
		if existing[i].Name != current && metav1.IsControlledBy(&existing[i], deploy) {
			stale = append(stale, &existing[i])
		}
	}
	if len(stale) == 0 {
		return 0, nil
	}
	inUse, hold, err := r.configMapsInUse(ctx, deploy, skillVolumeName)
	if err != nil {
		return 0, err
	}
	if hold {
		return len(stale), nil
	}
	kept := 0
	for _, cm := range stale {
		if inUse[cm.Name] {
			kept++
			continue
		}
		if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("pruning superseded skill ConfigMap %s: %w", cm.Name, err)
		}
	}
	return kept, nil
}

// skillObjects returns the reader for uploaded skill bodies: the one the binary set, else the bundled
// store when this install deploys it, reached with the address and credential agent pods are given.
func (r *AgentDeploymentReconciler) skillObjects(ctx context.Context) skillObjectReader {
	if r.SkillObjects != nil {
		return r.SkillObjects
	}
	if !r.devObjectStore() {
		return nil
	}
	addr := r.platformServices().objectStoreAddr()
	if v, ok := r.bundledObjects.Load(addr); ok {
		return v.(skillObjectReader)
	}
	store, err := objectstore.NewStoreAt(addr, objectStoreDevAccessKey, objectStoreDevSecretKey, skillDialTimeout)
	if err != nil {
		// Only a malformed address fails, and this one is derived from a namespace name. The
		// uploaded skills then report SourceUnavailable.
		logf.FromContext(ctx).Error(err, "the bundled object store cannot be reached; uploaded skill bodies are not mounted",
			"address", addr)
		return nil
	}
	v, _ := r.bundledObjects.LoadOrStore(addr, store)
	return v.(skillObjectReader)
}

// skillBodiesCondition is the SkillBodies condition for an agent with attached skills.
func skillBodiesCondition(generation int64, attached int, problems []skillBodyProblem) metav1.Condition {
	if len(problems) == 0 {
		return metav1.Condition{
			Type:               conditionSkillBodies,
			Status:             metav1.ConditionTrue,
			Reason:             reasonSkillBodiesMounted,
			ObservedGeneration: generation,
			Message: fmt.Sprintf("The bodies of all %d attached skills are mounted, each verified against its "+
				"digest; load() returns them.", attached),
		}
	}
	reason := problems[0].reason
	parts := make([]string, len(problems))
	for i, p := range problems {
		parts[i] = p.name + ": " + clip(p.detail, maxSkillDetailBytes)
		if slices.Index(skillReasonSeverity, p.reason) < slices.Index(skillReasonSeverity, reason) {
			reason = p.reason
		}
	}
	return metav1.Condition{
		Type:               conditionSkillBodies,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		ObservedGeneration: generation,
		Message: clip(fmt.Sprintf("load() answers 501 for %d of the %d attached skills, whose bodies are not mounted. %s.",
			len(problems), attached, strings.Join(parts, "; ")), maxSkillMessageBytes),
	}
}

// syncSkillBodiesCondition sets SkillBodies for an agent with attached skills and removes it from one
// without, writing status only when it changed.
func (r *AgentDeploymentReconciler) syncSkillBodiesCondition(
	ctx context.Context, deploy *agentsv1beta1.AgentDeployment, attached int, problems []skillBodyProblem,
) error {
	var changed bool
	if attached == 0 {
		changed = apimeta.RemoveStatusCondition(&deploy.Status.Conditions, conditionSkillBodies)
	} else {
		changed = apimeta.SetStatusCondition(&deploy.Status.Conditions,
			skillBodiesCondition(deploy.Generation, attached, problems))
	}
	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, deploy); err != nil {
		return fmt.Errorf("updating %s: %w", conditionSkillBodies, err)
	}
	return nil
}
