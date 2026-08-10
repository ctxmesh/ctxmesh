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
	"math/big"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/alertstore"
	"github.com/ctxmesh/agent-engine/internal/controlplane/auditlog"
	"github.com/ctxmesh/agent-engine/internal/controlplane/costrollup"
	"github.com/ctxmesh/agent-engine/internal/gateway/budget"
)

// alertPolicyRequeue is how often each AlertPolicy is re-evaluated. The window-based conditions
// (errorRate/p95Latency/budgetSoft) pull from off-cluster stores with no watch, so a periodic requeue
// drives re-evaluation. The event-driven regressionDetected condition also re-fires on the AgentDeployment
// watch (see SetupWithManager); 1m keeps the durable value fresh without hammering the stores.
const alertPolicyRequeue = time.Minute

// Alert condition type identifiers (must match the AlertCondition.type enum in
// api/v1beta1/alertpolicy_types.go).
const (
	condTypeRegressionDetected = "regressionDetected"
	condTypeBudgetSoft         = "budgetSoft"
	condTypeErrorRate          = "errorRate"
	condTypeP95Latency         = "p95Latency"
	condTypeForecastExceeded   = "forecastExceeded"
	condTypeRunFailureRate     = "runFailureRate"
)

// AlertPolicyReconciler reconciles an AlertPolicy object (M70, ADR 0063 D2). It evaluates each policy
// condition, fires ONCE per false→true transition (dedup keyed on AlertCondition.name in the AlertPolicy
// .status), and PERSISTS the fired alert durably to the cpDB alerts ledger + the audit_log. NOTIFICATION
// dispatch (webhook POST + the console read feed) is a SEPARATE later task (m70.5) — m70.4 is DETECTION +
// PERSISTENCE only; it never changes traffic, promotes, or rolls back anything.
//
// Store wiring (injected in cmd/main.go from the manager's existing cpDB; ALL nil-safe — a nil dep makes
// that persistence path a logged no-op, never a panic, and the reconciler still evaluates + updates
// .status):
//   - Alerts  (cpDB): appends a fired-alert row on a false→true transition, resolves it on true→false.
//   - Rollups (cpDB): the durable cost-rollup ledger, read for the budgetSoft condition's tenant MTD spend.
//   - Audit   (cpDB): an "alert.fired" audit entry per fired alert.
type AlertPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Alerts is the durable fired-alert ledger (from cpDB). nil ⇒ fired alerts are not persisted
	// (evaluation + .status still work; a dev deployment without cpDB).
	Alerts alertstore.Store

	// Rollups is the durable cost-rollup ledger (from cpDB), read for the budgetSoft condition's tenant
	// month-to-date spend. nil ⇒ budgetSoft abstains (no data source).
	Rollups costrollup.Store

	// Audit is the control-plane audit store (from cpDB). nil ⇒ fired alerts are not audited.
	Audit auditlog.Store
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=alertpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=alertpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=alertpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments/status,verbs=get
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=tenants,verbs=get;list;watch

// Reconcile evaluates one AlertPolicy: it resolves the selected AgentDeployments, evaluates each
// condition to a firing boolean + a human value, and updates the per-condition status with fire-once
// semantics (append a durable alert on false→true, resolve it on true→false). It always requeues after
// alertPolicyRequeue so the window-based conditions track advancing data.
func (r *AlertPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ap agentsv1beta1.AlertPolicy
	if err := r.Get(ctx, req.NamespacedName, &ap); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // deleted before we saw it — nothing to do
		}
		return ctrl.Result{}, fmt.Errorf("fetching AlertPolicy: %w", err)
	}
	if !ap.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // being deleted — nothing to evaluate
	}

	// Resolve the AgentDeployments this policy watches (matchLabels ∪ names in the policy's namespace).
	agents, err := r.selectedAgents(ctx, &ap)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving selected AgentDeployments: %w", err)
	}

	changed := false
	now := metav1.Now()
	for i := range ap.Spec.Conditions {
		cond := ap.Spec.Conditions[i]
		firing, value := r.evaluateCondition(ctx, &ap, cond, agents)
		if r.applyConditionResult(ctx, &ap, cond, firing, value, now) {
			changed = true
		}
	}

	if changed {
		if err := r.Status().Update(ctx, &ap); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating AlertPolicy status: %w", err)
		}
	}

	// Stamp observedGeneration when it has advanced (a separate change — status may already be current
	// from the condition pass above, so guard the write).
	if ap.Status.ObservedGeneration != ap.Generation {
		ap.Status.ObservedGeneration = ap.Generation
		if err := r.Status().Update(ctx, &ap); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating AlertPolicy observedGeneration: %w", err)
		}
		log.V(1).Info("AlertPolicy observedGeneration stamped",
			"alertpolicy", ap.Name, "generation", ap.Generation)
	}

	return ctrl.Result{RequeueAfter: alertPolicyRequeue}, nil
}

// selectedAgents returns the AgentDeployments in the policy's namespace matched by spec.selector. An
// empty selector (no matchLabels, no names) matches ALL AgentDeployments in the namespace (the CRD
// contract). matchLabels and names take a UNION.
func (r *AlertPolicyReconciler) selectedAgents(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
) ([]agentsv1alpha1.AgentDeployment, error) {
	sel := ap.Spec.Selector
	empty := len(sel.MatchLabels) == 0 && len(sel.Names) == 0

	var list agentsv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(ap.Namespace)); err != nil {
		return nil, fmt.Errorf("listing AgentDeployments: %w", err)
	}

	names := make(map[string]struct{}, len(sel.Names))
	for _, n := range sel.Names {
		names[n] = struct{}{}
	}

	out := make([]agentsv1alpha1.AgentDeployment, 0, len(list.Items))
	for i := range list.Items {
		d := list.Items[i]
		if empty || matchesLabels(d.Labels, sel.MatchLabels) {
			out = append(out, d)
			continue
		}
		if _, ok := names[d.Name]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}

// matchesLabels reports whether have contains every want key/value pair (subset match, like a
// label selector's matchLabels). An empty want matches nothing here — the empty-selector case is handled
// by the caller (matches all), so this returns false for want=={} to avoid double-counting.
func matchesLabels(have, want map[string]string) bool {
	if len(want) == 0 {
		return false
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// evaluateCondition evaluates a single AlertCondition against the selected agents to a (firing, value)
// pair. It ABSTAINS (not firing) rather than fabricate whenever the data source is missing. value is a
// short human-readable string recorded in status.lastValue + the durable alert row.
func (r *AlertPolicyReconciler) evaluateCondition(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	agents []agentsv1alpha1.AgentDeployment,
) (bool, string) {
	log := logf.FromContext(ctx)

	switch cond.Type {
	case condTypeRegressionDetected:
		return evalRegressionDetected(agents)

	case condTypeBudgetSoft:
		return r.evalBudgetSoft(ctx, ap, cond)

	case condTypeErrorRate, condTypeP95Latency, condTypeForecastExceeded, condTypeRunFailureRate:
		// Recognized-but-abstain: these have no wired data source yet (m52 Theme Q). Log once at V(1)
		// and treat as not firing — NEVER error, so the rest of the policy still evaluates.
		log.V(1).Info("alert condition type not yet evaluated — no data source (m52 Theme Q)",
			"alertpolicy", ap.Name, "condition", cond.Name, "type", cond.Type)
		return false, ""

	default:
		// The CRD enum bounds Type, so this is unreachable in practice; abstain defensively.
		log.V(1).Info("unknown alert condition type — abstaining",
			"alertpolicy", ap.Name, "condition", cond.Name, "type", cond.Type)
		return false, ""
	}
}

// evalRegressionDetected fires when ANY selected agent carries a RegressionDetected=True status
// condition. value lists the breaching agent name(s).
func evalRegressionDetected(agents []agentsv1alpha1.AgentDeployment) (bool, string) {
	breaching := make([]string, 0, len(agents))
	for i := range agents {
		c := apimeta.FindStatusCondition(agents[i].Status.Conditions, conditionRegressionDetected)
		if c != nil && c.Status == metav1.ConditionTrue {
			breaching = append(breaching, agents[i].Name)
		}
	}
	if len(breaching) == 0 {
		return false, ""
	}
	slices.Sort(breaching)
	return true, strings.Join(breaching, ",")
}

// evalBudgetSoft fires when the tenant owning the policy's namespace has consumed at least
// threshold-fraction of its budgetUSD month-to-date. It reuses the gateway enforcer's soft-threshold
// semantics: fire when spent >= budget * threshold (threshold parsed as a 0..1 fraction → percent for
// Money.MulPercent, matching enforcer.go's `spent.AtLeast(cap.MulPercent(softPct))`). It ABSTAINS (not
// firing) when there is no tenant, no budget, no rollups store, or no rollup row — never fabricates.
func (r *AlertPolicyReconciler) evalBudgetSoft(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
) (bool, string) {
	log := logf.FromContext(ctx)

	if r.Rollups == nil {
		log.V(1).Info("budgetSoft abstains: cost-rollup store not wired (no control-plane DB)",
			"alertpolicy", ap.Name, "condition", cond.Name)
		return false, ""
	}

	tc, found, err := resolveTenantForNamespace(ctx, r.Client, ap.Namespace)
	if err != nil {
		log.V(1).Info("budgetSoft abstains: tenant resolution failed",
			"alertpolicy", ap.Name, "condition", cond.Name, "err", err.Error())
		return false, ""
	}
	if !found || tc.budgetUSD == "" {
		log.V(1).Info("budgetSoft abstains: no tenant or no budget for namespace",
			"alertpolicy", ap.Name, "condition", cond.Name, "namespace", ap.Namespace)
		return false, ""
	}

	budgetCap, err := budget.ParseMoney(tc.budgetUSD)
	if err != nil {
		log.V(1).Info("budgetSoft abstains: unparseable tenant budgetUSD",
			"alertpolicy", ap.Name, "condition", cond.Name, "budgetUSD", tc.budgetUSD)
		return false, ""
	}

	// threshold is a 0..1 fraction of budget; MulPercent takes an integer percent, so scale by 100.
	// enforcer.go does the same soft = cap.MulPercent(softPct); spent.AtLeast(soft).
	pct, ok := parseFractionPercent(cond.Threshold)
	if !ok {
		log.V(1).Info("budgetSoft abstains: unparseable threshold (want a 0..1 fraction)",
			"alertpolicy", ap.Name, "condition", cond.Name, "threshold", cond.Threshold)
		return false, ""
	}

	// Read the tenant's month-to-date spend: the newest cost-rollup row in [monthStart, today].
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	rollups, err := r.Rollups.Range(ctx, "tenant", tc.id, monthStart, now)
	if err != nil {
		log.V(1).Info("budgetSoft abstains: cost-rollup range read failed",
			"alertpolicy", ap.Name, "condition", cond.Name, "tenant", tc.id, "err", err.Error())
		return false, ""
	}
	if len(rollups) == 0 {
		log.V(1).Info("budgetSoft abstains: no cost-rollup rows for tenant this month",
			"alertpolicy", ap.Name, "condition", cond.Name, "tenant", tc.id)
		return false, ""
	}
	// Range returns rows day-ASC; the newest (last) row is the current MTD cumulative spend. The rollup
	// carries spend as a float64 (numeric(18,6) → float on scan); big.Rat.SetFloat64 converts it exactly,
	// so the Money comparison below stays lossless in the same way the gateway enforcer's does.
	spendRat := new(big.Rat).SetFloat64(rollups[len(rollups)-1].SpendUSD)
	if spendRat == nil { // NaN/Inf — treat as no signal.
		spendRat = new(big.Rat)
	}
	spent := budget.MoneyFromRat(spendRat)

	// Reuse the enforcer's soft-threshold semantics: fire when spent >= cap * softPct
	// (enforcer.go: soft := cap.MulPercent(softPct); spent.AtLeast(soft)).
	soft := budgetCap.MulPercent(pct)
	value := fmt.Sprintf("%s/%s", spent.String(), budgetCap.String())
	return spent.AtLeast(soft), value
}

// parseFractionPercent parses a 0..1 fraction string (e.g. "0.8") into the integer percent (80) that
// Money.MulPercent consumes. It uses big.Rat so the fraction×100 is exact, then rounds half-up to the
// nearest whole percent (thresholds are authored at percent granularity — "0.8", "0.75"). Returns
// ok=false on an empty or unparseable string. The parse is permissive on the upper bound: a threshold
// >1 yields pct>100, and MulPercent handles that (the soft amount simply exceeds the cap).
func parseFractionPercent(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return 0, false
	}
	// pct = fraction * 100, rounded half-up to the nearest integer.
	r.Mul(r, big.NewRat(100, 1))
	r.Add(r, big.NewRat(1, 2)) // +0.5 for round-half-up before truncation
	num := new(big.Int).Quo(r.Num(), r.Denom())
	return int(num.Int64()), true
}

// applyConditionResult updates the per-condition status entry (keyed by AlertCondition.name) with
// fire-once semantics and returns whether status changed. On a false→true transition it appends a durable
// alert (+ audit); on true→false it resolves the open alert. While firing stays true across ticks it does
// NOT append a new alert (the dedup).
func (r *AlertPolicyReconciler) applyConditionResult(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	firing bool,
	value string,
	now metav1.Time,
) bool {
	idx := -1
	for i := range ap.Status.Conditions {
		if ap.Status.Conditions[i].Name == cond.Name {
			idx = i
			break
		}
	}

	// First time we see this condition: seed an entry. A brand-new condition that is already firing is a
	// false→true transition (prior state was "not firing").
	if idx == -1 {
		ap.Status.Conditions = append(ap.Status.Conditions, agentsv1beta1.AlertConditionStatus{
			Name:   cond.Name,
			Firing: false,
		})
		idx = len(ap.Status.Conditions) - 1
	}

	prev := ap.Status.Conditions[idx]
	changed := false

	switch {
	case firing && !prev.Firing:
		// false→true: fire once.
		ap.Status.Conditions[idx].Firing = true
		ap.Status.Conditions[idx].LastValue = value
		ap.Status.Conditions[idx].LastTransitionTime = now
		r.recordFired(ctx, ap, cond, value, now)
		changed = true

	case !firing && prev.Firing:
		// true→false: resolve the open alert.
		ap.Status.Conditions[idx].Firing = false
		ap.Status.Conditions[idx].LastValue = value
		ap.Status.Conditions[idx].LastTransitionTime = now
		r.resolveOpen(ctx, ap, cond)
		changed = true

	case firing && prev.Firing:
		// Still firing (dedup — no new alert). Refresh lastValue if it moved, without a transition stamp.
		if value != "" && value != prev.LastValue {
			ap.Status.Conditions[idx].LastValue = value
			changed = true
		}
	}

	return changed
}

// recordFired persists a fired alert to the durable ledger (+ an audit entry). Both paths are nil-safe:
// a missing store is a logged no-op, never a panic. The reconciler tracks the appended alert id on the
// per-condition status is NOT modeled (the status has no id field); resolveOpen finds the open row by
// namespace/policy/condition via a best-effort List, so persistence stays decoupled from status shape.
func (r *AlertPolicyReconciler) recordFired(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	value string,
	now metav1.Time,
) {
	log := logf.FromContext(ctx)
	agent := "" // condition-level today; a future per-agent split can populate this.
	msg := fmt.Sprintf("AlertPolicy %s/%s condition %q (%s) fired", ap.Namespace, ap.Name, cond.Name, cond.Type)

	if r.Alerts != nil {
		if _, err := r.Alerts.Append(ctx, alertstore.Alert{
			Namespace:  ap.Namespace,
			PolicyName: ap.Name,
			Condition:  cond.Name,
			Agent:      agent,
			CondType:   cond.Type,
			Value:      value,
			Message:    msg,
			FiredAt:    now.Time,
		}); err != nil {
			log.Error(err, "persisting fired alert failed (continuing)",
				"alertpolicy", ap.Name, "condition", cond.Name)
		}
	} else {
		log.V(1).Info("alert store not wired — fired alert not persisted",
			"alertpolicy", ap.Name, "condition", cond.Name)
	}

	if r.Audit != nil {
		if err := r.Audit.Append(ctx, auditlog.Entry{
			OccurredAt:   now.Time,
			Source:       "controller",
			Actor:        "alertpolicy-controller",
			ActorKind:    "controller",
			Action:       "alert.fired",
			ResourceKind: "AlertPolicy",
			ResourceName: ap.Name,
			Namespace:    ap.Namespace,
			Outcome:      "success",
			Detail: map[string]any{
				"condition": cond.Name,
				"type":      cond.Type,
				"value":     value,
			},
		}); err != nil {
			log.Error(err, "auditing fired alert failed (continuing)",
				"alertpolicy", ap.Name, "condition", cond.Name)
		}
	}
}

// resolveOpen best-effort resolves the open (unresolved) durable alert for this condition on a true→false
// transition. It Lists the namespace's recent alerts and resolves the newest unresolved row matching this
// policy + condition. nil-safe: a missing store is a logged no-op.
func (r *AlertPolicyReconciler) resolveOpen(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
) {
	log := logf.FromContext(ctx)
	if r.Alerts == nil {
		return
	}
	rows, err := r.Alerts.List(ctx, ap.Namespace, alertstore.MaxListLimit)
	if err != nil {
		log.Error(err, "listing alerts to resolve failed (continuing)",
			"alertpolicy", ap.Name, "condition", cond.Name)
		return
	}
	// rows are newest-first; resolve the first unresolved match for this policy + condition.
	for i := range rows {
		if rows[i].PolicyName == ap.Name && rows[i].Condition == cond.Name && rows[i].ResolvedAt == nil {
			if err := r.Alerts.Resolve(ctx, rows[i].ID); err != nil {
				log.Error(err, "resolving alert failed (continuing)",
					"alertpolicy", ap.Name, "condition", cond.Name, "id", rows[i].ID)
			}
			return
		}
	}
}

// SetupWithManager wires the controller to reconcile on AlertPolicy changes AND to re-evaluate every
// AlertPolicy in an AgentDeployment's namespace when that deployment changes (so a RegressionDetected
// transition promptly re-evaluates the policies that select it). The window-based conditions also
// re-evaluate on the periodic RequeueAfter (their data is off-cluster — no watch).
func (r *AlertPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapDeployToPolicies := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			deploy, ok := obj.(*agentsv1alpha1.AgentDeployment)
			if !ok {
				return nil
			}
			var list agentsv1beta1.AlertPolicyList
			if err := r.List(ctx, &list, client.InNamespace(deploy.Namespace)); err != nil {
				return nil
			}
			reqs := make([]reconcile.Request, 0, len(list.Items))
			for i := range list.Items {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: list.Items[i].Namespace,
						Name:      list.Items[i].Name,
					},
				})
			}
			return reqs
		},
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1beta1.AlertPolicy{}).
		Watches(&agentsv1alpha1.AgentDeployment{}, mapDeployToPolicies).
		Named("alertpolicy").
		Complete(r)
}
