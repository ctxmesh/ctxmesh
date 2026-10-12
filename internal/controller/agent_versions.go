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
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentversion"
)

// Version history lives in the control-plane store (ADR 0042). Recording a version fails open: the
// way out of a bad deploy is to apply the old spec, and that must not depend on Postgres. A version
// that could not be recorded is visible (the VersionRecorded condition, a metric) and retried.

const (
	// conditionVersionRecorded says whether the spec's version is in the control-plane store: True
	// once recorded, False while it could not be.
	conditionVersionRecorded = "VersionRecorded"
	reasonVersionRecorded    = "Recorded"
	reasonVersionNotRecorded = "VersionNotRecorded"

	// reasonVersionStoreUnavailable is the RolledBack=Unknown reason while a rollback cannot read
	// version history. A requested rollback stays requested and is retried.
	reasonVersionStoreUnavailable = "VersionStoreUnavailable"

	// versionStoreRetry is how soon a reconcile that could not reach the version store runs again.
	versionStoreRetry = 30 * time.Second

	// versionHistoryKeep is how many of an agent's newest versions pruning keeps, besides the pinned
	// ones (pruneVersions).
	versionHistoryKeep = 20

	// versionRecordedTTL is how long recordVersion trusts its memory of a recorded version.
	versionRecordedTTL = 10 * time.Minute
)

var versionRecordFailures = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "ctxmesh_agent_version_record_failures_total",
	Help: "Times the controller could not record an agent's version in the control-plane store. " +
		"The deploy continued; the version is retried and the agent carries VersionRecorded=False until it is recorded.",
})

func init() {
	metrics.Registry.MustRegister(versionRecordFailures)
}

// retrySoon shortens a successful reconcile's requeue to versionStoreRetry. An error already
// requeues with backoff.
func retrySoon(result ctrl.Result, err error) ctrl.Result {
	if err == nil && (result.RequeueAfter == 0 || result.RequeueAfter > versionStoreRetry) {
		result.RequeueAfter = versionStoreRetry
	}
	return result
}

// recordedVersion is the last version this process recorded for an agent, and when.
type recordedVersion struct {
	uid, name string
	at        time.Time
}

// msgStoreUnavailable says in product terms why something that needs the control-plane store did not
// happen. Condition messages are readable by anyone who can read the object; the error itself, which
// names the database host, user and database, goes to the controller log only.
const msgStoreUnavailable = "the control-plane store is unavailable"

// recordVersion records the version the spec is served as and reports whether it is recorded. A
// store error never stops the reconcile: it is logged, counted and put on status, and the caller
// requeues. A new version prunes the agent's history.
//
// The last version recorded per agent is remembered for versionRecordedTTL, so a reconcile of an
// unchanged spec costs no store round trip. After that the record is written again (a no-op when
// the row exists), so a row lost outside this process (a restore, a failover, a manual cleanup) is
// recorded again and VersionRecorded does not stay True on a memory.
func (r *AgentDeploymentReconciler) recordVersion(
	ctx context.Context,
	deploy *agentsv1beta1.AgentDeployment,
	versionName string,
) bool {
	if r.Versions == nil {
		return true
	}
	key := deploy.Namespace + "/" + deploy.Name
	now := r.now()
	if v, ok := r.recorded.Load(key); ok {
		if last, _ := v.(recordedVersion); last.uid == string(deploy.UID) && last.name == versionName &&
			now.Sub(last.at) < versionRecordedTTL {
			setVersionRecorded(deploy, "")
			return true
		}
	}
	inserted, err := r.Versions.Record(ctx, agentversion.Version{
		Namespace: deploy.Namespace,
		Agent:     deploy.Name,
		AgentUID:  string(deploy.UID),
		Name:      versionName,
		Snapshot:  versionedSpec(deploy.Spec),
		Source:    agentversion.SourceController,
	})
	if err != nil {
		versionRecordFailures.Inc()
		logf.FromContext(ctx).Error(err, "Could not record the agent's version; deploying anyway and retrying",
			"version", versionName, "retryIn", versionStoreRetry.String())
		setVersionRecorded(deploy, fmt.Sprintf(
			"version %q could not be recorded: %s. The deploy went ahead; a rollback to this version is "+
				"not possible until it is recorded. Retrying every %s.",
			versionName, msgStoreUnavailable, versionStoreRetry))
		return false
	}
	r.recorded.Store(key, recordedVersion{uid: string(deploy.UID), name: versionName, at: now})
	setVersionRecorded(deploy, "")
	if inserted {
		r.pruneVersions(ctx, deploy, versionName)
	}
	return true
}

// now is the reconciler's clock (tests set clock).
func (r *AgentDeploymentReconciler) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// setVersionRecorded sets VersionRecorded on deploy in memory: True when failure is empty, else False
// with it. It is not written here: the reconcile's own status write carries it, so recording a version
// adds no status write of its own (an extra write would make a later write from a cached read conflict).
func setVersionRecorded(deploy *agentsv1beta1.AgentDeployment, failure string) {
	cond := metav1.Condition{
		Type:               conditionVersionRecorded,
		Status:             metav1.ConditionTrue,
		Reason:             reasonVersionRecorded,
		Message:            "The serving spec's version is recorded; it can be rolled back to.",
		ObservedGeneration: deploy.Generation,
	}
	if failure != "" {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, reasonVersionNotRecorded, failure
	}
	apimeta.SetStatusCondition(&deploy.Status.Conditions, cond)
}

// carryVersionRecorded copies the VersionRecorded condition set on the reconciled object onto a
// freshly read one that is about to be written in its place.
func carryVersionRecorded(from, to *agentsv1beta1.AgentDeployment) {
	if c := apimeta.FindStatusCondition(from.Status.Conditions, conditionVersionRecorded); c != nil {
		apimeta.SetStatusCondition(&to.Status.Conditions, *c)
	}
}

// pruneVersions keeps the agent's newest versionHistoryKeep versions and every version that may still
// be needed: the current one, the latest one and its prior (the automatic rollback target and the
// detector's baseline), every version a revision of the agent's Knative Service is built from (a
// held, blocked or canaried candidate moves status.latestVersion while an older version keeps
// serving), and every version the gate, the rollout or the rollback history names. It also removes
// the versions of an earlier agent of the same name. Best effort: a failure is logged, nothing is
// pruned, and the next new version prunes again.
func (r *AgentDeploymentReconciler) pruneVersions(ctx context.Context, deploy *agentsv1beta1.AgentDeployment, current string) {
	log := logf.FromContext(ctx)
	uid := string(deploy.UID)
	pinned := []string{current}
	if latest := deploy.Status.LatestVersion; latest != "" {
		pinned = append(pinned, latest)
		prior, ok, err := r.Versions.Prior(ctx, deploy.Namespace, uid, latest)
		if err != nil {
			log.Error(err, "Could not prune version history", "version", current)
			return
		}
		if ok {
			pinned = append(pinned, prior)
		}
	}
	served, err := r.servedRevisions(ctx, deploy)
	if err != nil {
		log.Error(err, "Could not prune version history: the serving revisions are unknown", "version", current)
		return
	}
	for _, rev := range served {
		if v, ok := versionOfRevision(deploy.Name, rev); ok {
			pinned = append(pinned, v)
		}
	}
	if rb := deploy.Status.Rollback; rb != nil {
		pinned = append(pinned, rb.RolledBackTo)
		for _, ev := range rb.History {
			pinned = append(pinned, ev.ToVersion, ev.FromVersion)
		}
	}
	removed, err := r.Versions.Prune(ctx, deploy.Namespace, deploy.Name, uid, versionHistoryKeep, pinned)
	if err != nil {
		log.Error(err, "Could not prune version history", "version", current)
		return
	}
	if removed > 0 {
		log.Info("Pruned version history", "removed", removed, "kept", versionHistoryKeep)
	}
}

// servedRevisions names every revision the agent's Knative Service applies, routes traffic to, or has
// created, and the revisions the gate and the rollout name. An agent without a Service (a job, or one
// not deployed yet) has only the latter.
func (r *AgentDeploymentReconciler) servedRevisions(ctx context.Context, deploy *agentsv1beta1.AgentDeployment) ([]string, error) {
	var revs []string
	if g := deploy.Status.Gate; g != nil {
		revs = append(revs, g.ScoredRevision)
	}
	if ro := deploy.Status.Rollout; ro != nil {
		revs = append(revs, ro.CandidateRevision)
	}
	var ksvc servingv1.Service
	err := r.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: deploy.Name}, &ksvc)
	if apierrors.IsNotFound(err) {
		return revs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the Knative Service: %w", err)
	}
	revs = append(revs, ksvc.Spec.Template.Name, ksvc.Status.LatestReadyRevisionName, ksvc.Status.LatestCreatedRevisionName)
	for _, t := range ksvc.Spec.Traffic {
		revs = append(revs, t.RevisionName)
	}
	for _, t := range ksvc.Status.Traffic {
		revs = append(revs, t.RevisionName)
	}
	return revs, nil
}

// versionOfRevision returns the version a revision of the named agent is built from. Revisions are
// named "<version>" or "<version>-h<digest>", and a version is "<agent>-<spec-hash-8>".
func versionOfRevision(agent, revision string) (string, bool) {
	prefix := agent + "-"
	if !strings.HasPrefix(revision, prefix) || len(revision) < len(prefix)+specHashLen {
		return "", false
	}
	return revision[:len(prefix)+specHashLen], true
}

// deleteVersions removes a deleted agent's history. Best effort: a failure is logged, and the
// rows of an earlier agent of the same name are also removed by the next agent's first prune.
func (r *AgentDeploymentReconciler) deleteVersions(ctx context.Context, namespace, name string) {
	if r.Versions == nil {
		return
	}
	r.recorded.Delete(namespace + "/" + name)
	if err := r.Versions.DeleteAgent(ctx, namespace, name); err != nil {
		logf.FromContext(ctx).Error(err, "Could not delete a deleted agent's version history",
			"namespace", namespace, "name", name)
	}
}

// priorVersionOf returns the version recorded immediately before version: the automatic rollback
// target and the baseline for judging version's health. ok is false when there is none.
func (r *AgentDeploymentReconciler) priorVersionOf(
	ctx context.Context,
	deploy *agentsv1beta1.AgentDeployment,
	version string,
) (string, bool, error) {
	if r.Versions == nil {
		return "", false, nil
	}
	prior, ok, err := r.Versions.Prior(ctx, deploy.Namespace, string(deploy.UID), version)
	if err != nil {
		return "", false, fmt.Errorf("reading the version before %q: %w", version, err)
	}
	return prior, ok, nil
}

// rollbackTarget resolves a rollback target among this agent's versions. A target that does not
// resolve returns a refusal reason and message instead of a version; err is a store failure.
func (r *AgentDeploymentReconciler) rollbackTarget(
	ctx context.Context,
	deploy *agentsv1beta1.AgentDeployment,
	target string,
) (v agentversion.Version, reason, message string, err error) {
	notFound := fmt.Sprintf("rollback target version %q not found for %q in namespace %q; no action taken",
		target, deploy.Name, deploy.Namespace)
	if r.Versions == nil {
		return v, reasonRollbackTargetNotFound, notFound, nil
	}
	v, ok, err := r.Versions.Get(ctx, deploy.Namespace, string(deploy.UID), target)
	if err != nil {
		return v, "", "", fmt.Errorf("reading rollback target %q: %w", target, err)
	}
	if ok {
		return v, "", "", nil
	}
	owners, err := r.Versions.Owners(ctx, deploy.Namespace, target)
	if err != nil {
		return v, "", "", fmt.Errorf("reading rollback target %q: %w", target, err)
	}
	earlier := false
	for _, o := range owners {
		if o.Agent != deploy.Name {
			return v, reasonRollbackTargetMismatch, fmt.Sprintf(
				"rollback target %q is a version of deployment %q, not %q; no action taken",
				target, o.Agent, deploy.Name), nil
		}
		earlier = true
	}
	if earlier {
		return v, reasonRollbackTargetNotFound, fmt.Sprintf(
			"rollback target %q is a version of an earlier %q that was deleted; this deployment was created "+
				"after it and does not inherit its history; no action taken", target, deploy.Name), nil
	}
	return v, reasonRollbackTargetNotFound, notFound, nil
}
