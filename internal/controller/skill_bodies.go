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
	"slices"
	"strings"
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
	// object's metadata. A body is never truncated: one that does not fit is left out whole.
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

	// skillFetchBudget bounds the fetching in one pod-template build. The reconcile waits for it and
	// holds one of the few AgentDeployment workers (agentDeploymentWorkers) meanwhile, so an
	// unreachable git host or object store costs at most this much per retry; what is left
	// unfetched is retried on its backoff.
	skillFetchBudget = 10 * time.Second

	// skillRetryBase and skillRetryMax bound the backoff between attempts at a body that failed.
	skillRetryBase = 10 * time.Second
	skillRetryMax  = 10 * time.Minute
)

// skillReasonSeverity orders the reasons for the condition's Reason when several skills fail: the
// first is the one most worth acting on.
var skillReasonSeverity = []string{
	reasonSkillDigestMismatch, reasonSkillSourceRefused, reasonSkillTooLarge,
	reasonSkillFetchFailed, reasonSkillSourceUnavailable,
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
// retryAt. requeue says whether the reconcile is scheduled for retryAt; a failure only a
// configuration change can clear waits for the next reconcile instead.
type skillFetchFailure struct {
	failures int
	retryAt  time.Time
	requeue  bool
	problem  skillBodyProblem
}

// skillFailureState is the value of AgentDeploymentReconciler.skillFailures: ref → failure. A new
// map is stored on every build, never one mutated in place.
type skillFailureState map[string]skillFetchFailure

// skillBodyError is why one body was not fetched. cause is for the controller log only.
type skillBodyError struct {
	skillBodyProblem
	requeue bool
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
	// failures is the retry state to keep for this agent.
	failures skillFailureState
	// sizes holds the size of each body verified by a fetch in this resolution, by ref.
	sizes map[string]int
	// errs are the fetch errors of this resolution, for the log.
	errs []*skillBodyError
}

// skillBodyResolver resolves an agent's attached skill bodies. Its sources are interfaces so the
// unit tests drive every path without a network.
type skillBodyResolver struct {
	versions skillVersionLookup
	git      prompt.Resolver
	objects  skillObjectReader
	capBytes int
	now      func() time.Time
}

// resolve returns the bodies to mount for the pinned refs, in spec order. A body already verified in
// one of the agent's ConfigMaps (have, by ref) is reused; any other is fetched and verified, unless
// an earlier failure (prior) is not yet due for retry, or a known size (sizeOf) cannot fit. Bodies
// are packed in spec order: one that would take the total over capBytes is left out whole, and a
// later, smaller one may still fit.
func (s skillBodyResolver) resolve(
	ctx context.Context,
	namespace string,
	resolved []string,
	have map[string][]byte,
	prior skillFailureState,
	sizeOf func(ref string) (int, bool),
) skillBodyOutcome {
	out := skillBodyOutcome{
		bodies:   map[string][]byte{},
		failures: skillFailureState{},
		sizes:    map[string]int{},
	}
	remaining := s.capBytes
	now := s.now()
	for _, ref := range resolved {
		name, digest, ok := strings.Cut(ref, "@")
		if !ok {
			continue // ResolveAll only returns pinned refs
		}
		body, verified := have[ref]
		if !verified {
			if n, known := sizeOf(ref); known && n > remaining {
				out.problems = append(out.problems, skillTooLarge(name, n, s.capBytes))
				continue
			}
			if f, failed := prior[ref]; failed && now.Before(f.retryAt) {
				out.failures[ref] = f
				out.problems = append(out.problems, f.problem)
				continue
			}
			var ferr *skillBodyError
			body, ferr = s.fetch(ctx, namespace, name, digest)
			if ferr != nil {
				attempts := prior[ref].failures + 1
				out.failures[ref] = skillFetchFailure{
					failures: attempts,
					retryAt:  now.Add(skillRetryDelay(attempts)),
					requeue:  ferr.requeue,
					problem:  ferr.skillBodyProblem,
				}
				out.problems = append(out.problems, ferr.skillBodyProblem)
				out.errs = append(out.errs, ferr)
				continue
			}
			out.sizes[ref] = len(body)
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

// fetch returns the body of one pinned version, verified against its digest. Unverified bytes are
// never returned.
func (s skillBodyResolver) fetch(ctx context.Context, namespace, name, digest string) ([]byte, *skillBodyError) {
	fail := func(reason, detail string, requeue bool, cause error) *skillBodyError {
		return &skillBodyError{
			skillBodyProblem: skillBodyProblem{name: name, reason: reason, detail: detail},
			requeue:          requeue, cause: cause,
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

	var body []byte
	switch v.Source {
	case skill.SourceGit:
		if s.git == nil {
			return nil, fail(reasonSkillSourceUnavailable,
				"it is sourced from git and this controller has no git fetcher", false, nil)
		}
		res, gerr := s.git.Resolve(ctx, agentsv1beta1.GitPromptSource{Repo: v.Repo, Ref: v.Ref, Path: v.Path})
		if gerr != nil {
			detail := fmt.Sprintf("it could not be fetched from git (repo %s, ref %s, path %s)", v.Repo, v.Ref, v.Path)
			if errors.Is(gerr, prompt.ErrNotFound) {
				detail += ": no file is at that path and ref, or the repository is not on github.com, " +
					"the one git host bodies are fetched from"
			}
			return nil, fail(reasonSkillFetchFailed, detail, true, gerr)
		}
		body = []byte(res.Content)
	case skill.SourceUpload:
		if !skill.OwnsObjectKey(namespace, v.ObjectKey) {
			return nil, fail(reasonSkillSourceRefused,
				fmt.Sprintf("its object key %q is outside %s, the only prefix this namespace's skills are read from",
					v.ObjectKey, skill.ObjectKeyPrefix(namespace)), false, nil)
		}
		if s.objects == nil {
			return nil, fail(reasonSkillSourceUnavailable,
				"it was uploaded and this install has no object store to read it from", false, nil)
		}
		body, err = readSkillObject(ctx, s.objects, v.ObjectKey)
		if err != nil {
			return nil, fail(reasonSkillFetchFailed,
				fmt.Sprintf("it could not be read from the object store (key %s)", v.ObjectKey), true, err)
		}
	default:
		return nil, fail(reasonSkillSourceUnavailable,
			fmt.Sprintf("its version names a source this controller cannot read (%q)", v.Source), false, nil)
	}

	if skill.Digest(body) != digest {
		return nil, fail(reasonSkillDigestMismatch,
			fmt.Sprintf("the bytes at its source do not hash to %s, so they are not mounted", digest), true, nil)
	}
	return body, nil
}

// readSkillObject reads at most one byte more than a version may hold: a longer object cannot hash to
// a valid version's digest, so the rest is never needed.
func readSkillObject(ctx context.Context, objects skillObjectReader, key string) ([]byte, error) {
	rc, err := objects.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(rc, skill.MaxBodyBytes+1))
	if closeErr := rc.Close(); readErr == nil && closeErr != nil {
		readErr = closeErr
	}
	if readErr != nil {
		return nil, readErr
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

// skillVolume is the pod volume for the mounted skill bodies; injectSkills adds the mount.
func skillVolume(configMap string) corev1.Volume {
	return corev1.Volume{
		Name: skillVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMap},
			},
		},
	}
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
	agentKey := deploy.Namespace + "/" + deploy.Name
	var cms corev1.ConfigMapList
	if err := r.List(ctx, &cms,
		client.InNamespace(deploy.Namespace),
		client.MatchingLabels{skillAgentLabel: deploy.Name}); err != nil {
		return skillMount{}, fmt.Errorf("listing skill ConfigMaps: %w", err)
	}
	if len(resolved) == 0 {
		r.skillFailures.Delete(agentKey)
		if err := r.pruneSkillConfigMaps(ctx, deploy, cms.Items, ""); err != nil {
			return skillMount{}, err
		}
		return skillMount{}, r.syncSkillBodiesCondition(ctx, deploy, 0, nil)
	}

	var prior skillFailureState
	if v, ok := r.skillFailures.Load(agentKey); ok {
		prior = v.(skillFailureState)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, skillFetchBudget)
	defer cancel()
	resolver := skillBodyResolver{
		versions: r.Skills,
		git:      r.SkillGit,
		objects:  r.skillObjects(ctx),
		capBytes: maxSkillBodiesBytes,
		now:      time.Now,
	}
	out := resolver.resolve(fetchCtx, deploy.Namespace, resolved, verifiedSkillBodies(cms.Items, resolved), prior,
		func(ref string) (int, bool) {
			v, ok := r.skillSizes.Load(deploy.Namespace + "/" + ref)
			if !ok {
				return 0, false
			}
			return v.(int), true
		})
	for ref, n := range out.sizes {
		r.skillSizes.Store(deploy.Namespace+"/"+ref, n)
	}
	if len(out.failures) == 0 {
		r.skillFailures.Delete(agentKey)
	} else {
		r.skillFailures.Store(agentKey, out.failures)
	}
	log := logf.FromContext(ctx)
	for _, e := range out.errs {
		log.Error(e.cause, "skill body not mounted", "agent", agentKey, "skill", e.name,
			"reason", e.reason, "detail", e.detail)
	}

	var mount skillMount
	if len(out.mounted) > 0 {
		mount.hash = skillBodiesHash(out.mounted)
		mount.configMap = skillConfigMapName(deploy.Name, mount.hash)
		if err := r.ensureSkillConfigMap(ctx, deploy, mount.configMap, out.bodies); err != nil {
			return skillMount{}, err
		}
	}
	if err := r.pruneSkillConfigMaps(ctx, deploy, cms.Items, mount.configMap); err != nil {
		return skillMount{}, err
	}
	return mount, r.syncSkillBodiesCondition(ctx, deploy, len(resolved), out.problems)
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

// pruneSkillConfigMaps deletes the agent's skill ConfigMaps other than current that no live Revision
// mounts, so a held candidate or a rollback target keeps its bodies.
func (r *AgentDeploymentReconciler) pruneSkillConfigMaps(
	ctx context.Context, deploy *agentsv1beta1.AgentDeployment, existing []corev1.ConfigMap, current string,
) error {
	var stale []*corev1.ConfigMap
	for i := range existing {
		if existing[i].Name != current {
			stale = append(stale, &existing[i])
		}
	}
	if len(stale) == 0 {
		return nil
	}
	inUse, err := r.configMapsMountedByRevisions(ctx, deploy, skillVolumeName)
	if err != nil {
		return err
	}
	for _, cm := range stale {
		if inUse[cm.Name] {
			continue
		}
		if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("pruning superseded skill ConfigMap %s: %w", cm.Name, err)
		}
	}
	return nil
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
	store, err := objectstore.NewStoreAt(addr, objectStoreDevAccessKey, objectStoreDevSecretKey)
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
		parts[i] = p.name + ": " + p.detail
		if slices.Index(skillReasonSeverity, p.reason) < slices.Index(skillReasonSeverity, reason) {
			reason = p.reason
		}
	}
	return metav1.Condition{
		Type:               conditionSkillBodies,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		ObservedGeneration: generation,
		Message: fmt.Sprintf("load() answers 501 for %d of the %d attached skills, whose bodies are not mounted. %s.",
			len(problems), attached, strings.Join(parts, "; ")),
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

// skillBodyRetryAfter returns how long until the agent's earliest skill body is due for another
// fetch, and false when none is waiting on one.
func (r *AgentDeploymentReconciler) skillBodyRetryAfter(namespace, name string) (time.Duration, bool) {
	v, ok := r.skillFailures.Load(namespace + "/" + name)
	if !ok {
		return 0, false
	}
	var earliest time.Time
	for _, f := range v.(skillFailureState) {
		if f.requeue && (earliest.IsZero() || f.retryAt.Before(earliest)) {
			earliest = f.retryAt
		}
	}
	if earliest.IsZero() {
		return 0, false
	}
	return max(time.Until(earliest), time.Second), true
}

// requeueForSkillBodies schedules the next reconcile no later than the earliest skill body retry.
func (r *AgentDeploymentReconciler) requeueForSkillBodies(
	result ctrl.Result, namespace, name string,
) ctrl.Result {
	after, ok := r.skillBodyRetryAfter(namespace, name)
	if ok && (result.RequeueAfter == 0 || result.RequeueAfter > after) {
		result.RequeueAfter = after
	}
	return result
}
