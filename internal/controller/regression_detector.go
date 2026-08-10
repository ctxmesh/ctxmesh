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
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/onlinescore"
)

// RegressionDetected is the status condition the detector maintains on an AgentDeployment
// (ADR 0062 Fork 4, M69 — DETECTION ONLY; the auto-rollback trigger is DEFERRED per PRD §17.4).
//   - True  → the serving version regressed vs the prior version's baseline (a component breached
//     with enough samples + persistence). Reason names the breaching component; Message carries
//     the deltas. The console pairs this with a ONE-CLICK HUMAN rollback (m69.8/m69.11) — this
//     controller NEVER changes traffic, promotes, or rolls back on its own.
//   - False → evaluated and healthy (a baseline + current windows existed with enough samples and
//     no component breached).
//   - Unknown → not enough data to render a verdict (no baseline version yet, no online-score
//     aggregates, or samples below minSamples). The detector ABSTAINS rather than fabricate.
const conditionRegressionDetected = "RegressionDetected"

// RegressionDetected condition reasons.
const (
	reasonRegressionDetected = "RegressionDetected" // True: a component breached
	reasonNoRegression       = "NoRegression"       // False: evaluated, healthy
	reasonInsufficientData   = "InsufficientData"   // Unknown: sparse / below minSamples
	reasonNoBaseline         = "NoBaseline"         // Unknown: fewer than two versions
	reasonNoServingVersion   = "NoServingVersion"   // Unknown: status.latestVersion unset
	reasonOnlineScoreUnwired = "OnlineScoreUnwired" // Unknown: no store (dev without cpDB)
)

// regressionRequeue is how often the detector re-evaluates a deployment as online-score windows
// advance. There is no watch on the off-cluster online_score_aggregates rows (they are written by
// the online-scoring worker on cpDB), so a periodic requeue drives re-evaluation. 5m is well
// under the hourly aggregate window so a fresh window is picked up promptly without hammering pg.
const regressionRequeue = 5 * time.Minute

// baselineWindowLimit bounds how many recent aggregate windows we pull per version. A handful
// covers the persistence horizon (K) plus a comparable baseline window with head-room.
const baselineWindowLimit = 24

// regressionDetectedGauge exposes the detector's current verdict per deployment as a gauge
// (1 = RegressionDetected True, 0 = False/Unknown). It mirrors the tenant_metrics.go pattern —
// this codebase has NO EventRecorder in its controllers (m68.12 reasoned deviation), so a status
// condition + a metric are the surfacing mechanism, NOT a k8s Event.
var regressionDetectedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "agentengine_regression_detected",
	Help: "1 when the online-score regression detector has flagged the serving AgentVersion as regressed " +
		"vs the prior version's baseline (RegressionDetected=True), else 0. DETECTION ONLY — no auto-rollback " +
		"is wired (ADR 0062 Fork 4; PRD §17.4 defers the auto-trigger). Labels: namespace, agent.",
}, []string{"namespace", "agent"})

func init() {
	metrics.Registry.MustRegister(regressionDetectedGauge)
}

// onlineScoreReader is the narrow slice of the online-score store the detector needs: list the
// recent per-window aggregates for a version so it can compare the serving version against the
// prior version's baseline. A narrow interface lets envtest inject a memstore-backed fake without
// a real Postgres.
type onlineScoreReader interface {
	ListAggregates(ctx context.Context, namespace, agentName string, limit int) ([]onlinescore.Aggregate, error)
}

// RegressionDetectorReconciler watches AgentDeployments and maintains the RegressionDetected
// condition from the online-score aggregates (ADR 0062 Fork 4, M69). It is CONTROLLER-side and
// uses ONLY the manager's client (to read AgentDeployment + AgentVersion history) and the
// online-score store (from the manager's existing cpDB). It emits DETECTION ONLY — it never
// promotes, rolls back, or changes traffic (the auto-trigger is deferred; the human console
// actuator is m69.8).
//
// Store wiring (injected in cmd/main.go from the manager's existing cpDB):
//   - OnlineScore (cpDB): ListAggregates. nil ⇒ a dev deployment without CONTROLPLANE_DSN → the
//     detector abstains (RegressionDetected=Unknown/OnlineScoreUnwired), never fabricates.
type RegressionDetectorReconciler struct {
	client.Client

	// OnlineScore is the control-plane online-score aggregate store (from cpDB). nil ⇒ detection is
	// disabled (the condition is set Unknown/OnlineScoreUnwired); a deployment without cpDB.
	OnlineScore onlineScoreReader

	// Detector carries the detection tunables. Nil ⇒ the package defaults
	// (onlinescore.NewRegressionDetector with a zero DetectorConfig).
	Detector *onlinescore.RegressionDetector
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentversions,verbs=get;list;watch

// Reconcile evaluates one AgentDeployment for a regression and maintains its RegressionDetected
// condition. It always requeues after regressionRequeue so the verdict tracks advancing windows.
func (r *RegressionDetectorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var deploy agentsv1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			// Deployment gone — drop the gauge series so a deleted agent does not linger at a
			// stale verdict.
			regressionDetectedGauge.DeleteLabelValues(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching AgentDeployment: %w", err)
	}
	if !deploy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // being deleted — nothing to evaluate
	}

	detector := r.Detector
	if detector == nil {
		detector = onlinescore.NewRegressionDetector(onlinescore.DetectorConfig{})
	}

	verdict, status, reason, msg := r.evaluate(ctx, &deploy, detector)

	// Gauge: 1 only when firing True; Unknown/False → 0.
	if status == metav1.ConditionTrue {
		regressionDetectedGauge.WithLabelValues(deploy.Namespace, deploy.Name).Set(1)
	} else {
		regressionDetectedGauge.WithLabelValues(deploy.Namespace, deploy.Name).Set(0)
	}

	if err := r.setRegressionCondition(ctx, &deploy, status, reason, msg); err != nil {
		return ctrl.Result{}, err
	}
	if verdict.Regressed {
		log.Info("RegressionDetected on serving version",
			"agent", deploy.Name, "namespace", deploy.Namespace, "detail", msg)
	}
	// Always requeue so the verdict tracks advancing online-score windows (there is no watch on the
	// off-cluster aggregate rows).
	return ctrl.Result{RequeueAfter: regressionRequeue}, nil
}

// evaluate resolves the serving + baseline versions, loads their aggregates, and runs the
// detector. It returns the verdict plus the condition (status, reason, message) to write. It
// ABSTAINS (Unknown) rather than fabricate whenever the data is insufficient.
func (r *RegressionDetectorReconciler) evaluate(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	detector *onlinescore.RegressionDetector,
) (onlinescore.RegressionVerdict, metav1.ConditionStatus, string, string) {
	if r.OnlineScore == nil {
		return onlinescore.RegressionVerdict{}, metav1.ConditionUnknown, reasonOnlineScoreUnwired,
			"online-score store is not wired (no control-plane DB); regression detection is disabled"
	}

	serving := deploy.Status.LatestVersion
	if serving == "" {
		return onlinescore.RegressionVerdict{}, metav1.ConditionUnknown, reasonNoServingVersion,
			"no serving AgentVersion yet (status.latestVersion is empty)"
	}

	baseline, ok, err := r.priorVersion(ctx, deploy, serving)
	if err != nil {
		// A read error is infra — surface Unknown but let the reconcile requeue normally.
		return onlinescore.RegressionVerdict{}, metav1.ConditionUnknown, reasonNoBaseline,
			fmt.Sprintf("could not resolve the prior AgentVersion baseline: %v", err)
	}
	if !ok {
		return onlinescore.RegressionVerdict{}, metav1.ConditionUnknown, reasonNoBaseline,
			"no prior AgentVersion to baseline against (only one version exists)"
	}

	// Load aggregates for both versions. ListAggregates returns (ns, agent) rows most-recent-first;
	// filter to each version so the detector compares like-for-like.
	all, err := r.OnlineScore.ListAggregates(ctx, deploy.Namespace, deploy.Name, 0)
	if err != nil {
		return onlinescore.RegressionVerdict{}, metav1.ConditionUnknown, reasonInsufficientData,
			fmt.Sprintf("could not read online-score aggregates: %v", err)
	}
	currentWindows := filterByVersion(all, serving)
	baselineWindows := filterByVersion(all, baseline)
	if len(currentWindows) == 0 || len(baselineWindows) == 0 {
		return onlinescore.RegressionVerdict{}, metav1.ConditionUnknown, reasonInsufficientData,
			fmt.Sprintf("insufficient online-score data (serving %q: %d windows, baseline %q: %d windows)",
				serving, len(currentWindows), baseline, len(baselineWindows))
	}

	// Baseline reference = the prior version's MOST-RECENT comparable window (its steady state).
	verdict := detector.Detect(baselineWindows[0], currentWindows)

	switch {
	case verdict.Regressed:
		return verdict, metav1.ConditionTrue, reasonRegressionDetected,
			fmt.Sprintf("serving version %q regressed vs baseline %q: %s", serving, baseline, verdict.Summary())
	case verdict.Evaluated:
		return verdict, metav1.ConditionFalse, reasonNoRegression,
			fmt.Sprintf("serving version %q is healthy vs baseline %q (no component breached)", serving, baseline)
	default:
		// Windows existed but no component cleared minSamples — abstain.
		return verdict, metav1.ConditionUnknown, reasonInsufficientData,
			fmt.Sprintf("online-score samples below minSamples (%d) for serving %q; no verdict",
				detector.Config().MinSamples, serving)
	}
}

// priorVersion returns the name of the AgentVersion that immediately precedes `serving` in this
// deployment's history, ordered by creation time. Returns (name, true, nil) when a prior version
// exists, ("", false, nil) when `serving` is the only/first version, and an error on a read
// failure. Versions are the immutable per-spec-hash snapshots; the one created just before the
// serving version is the natural baseline (ADR 0062 Fork 4).
func (r *RegressionDetectorReconciler) priorVersion(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	serving string,
) (string, bool, error) {
	var list agentsv1alpha1.AgentVersionList
	if err := r.List(ctx, &list, client.InNamespace(deploy.Namespace)); err != nil {
		return "", false, fmt.Errorf("listing AgentVersions: %w", err)
	}
	// Keep only this deployment's versions.
	mine := make([]agentsv1alpha1.AgentVersion, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.DeploymentName == deploy.Name {
			mine = append(mine, list.Items[i])
		}
	}
	if len(mine) < 2 {
		return "", false, nil // no prior version to baseline against
	}
	sortVersionsByCreation(mine)
	// Find the serving version and return the one immediately before it.
	for i := range mine {
		if mine[i].Name == serving {
			if i == 0 {
				return "", false, nil // serving IS the oldest — no baseline
			}
			return mine[i-1].Name, true, nil
		}
	}
	// serving not found among versions (e.g. status ahead of the version list) → the newest prior
	// is the most recent other version.
	return mine[len(mine)-1].Name, true, nil
}

// sortVersionsByCreation orders an AgentVersion slice oldest → newest by CreationTimestamp,
// tie-breaking on name for determinism (second-granularity timestamps collide often). Shared
// by the regression detector's baseline resolution and the rollback actuator's healthy-target
// check so both agree on version ordering.
func sortVersionsByCreation(versions []agentsv1alpha1.AgentVersion) {
	slices.SortFunc(versions, func(a, b agentsv1alpha1.AgentVersion) int {
		ta, tb := a.CreationTimestamp.Time, b.CreationTimestamp.Time
		if ta.Equal(tb) {
			return strings.Compare(a.Name, b.Name)
		}
		if ta.Before(tb) {
			return -1
		}
		return 1
	})
}

// filterByVersion returns the aggregates for exactly `version`, preserving the input order
// (most-recent-first from ListAggregates), capped at baselineWindowLimit (enough to cover the
// persistence horizon plus a comparable baseline window). Shared by the regression detector and
// the rollback actuator's healthy-target check.
func filterByVersion(all []onlinescore.Aggregate, version string) []onlinescore.Aggregate {
	out := make([]onlinescore.Aggregate, 0, baselineWindowLimit)
	for i := range all {
		if all[i].AgentVersion == version {
			out = append(out, all[i])
			if len(out) >= baselineWindowLimit {
				break
			}
		}
	}
	return out
}

// setRegressionCondition writes the RegressionDetected condition, change-guarded (no write when
// nothing changed) to avoid a status write-storm on the periodic requeue.
func (r *RegressionDetectorReconciler) setRegressionCondition(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	changed := apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
		Type:               conditionRegressionDetected,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: deploy.Generation,
	})
	if !changed {
		return nil // no drift — skip the write (change-guarded)
	}
	if err := r.Status().Update(ctx, deploy); err != nil {
		return fmt.Errorf("updating RegressionDetected condition: %w", err)
	}
	return nil
}

// SetupWithManager wires the detector to reconcile on AgentDeployment changes. The verdict also
// re-evaluates on the periodic RequeueAfter (the online-score rows are off-cluster — no watch).
func (r *RegressionDetectorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentDeployment{}).
		Named("regressiondetector").
		Complete(r)
}
