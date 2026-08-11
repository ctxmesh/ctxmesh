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

// alertpolicy_approval.go — the HITL approval-waiting notification (M75, m75.3, ADR 0069 §3).
//
// When a run pauses on requires_action/plan_approval it has no "notify the approver" hook. This rides
// the ENTIRE M70 notification plane (the alertstore ledger + the webhook/console channels) — it builds
// no new channel. The one genuine divergence from the aggregate AlertPolicy conditions is the DEDUP KEY:
// the aggregate fire-once dedup is keyed on the condition NAME (one firing state per condition), but
// approval-waiting is a PER-RUN event — many runs wait at once — so it dedups on
// (policy, condition, runID). Shoehorning it into the aggregate loop would collapse every waiting run
// into one alert and silently drop the second run's notification (the verified wrinkle, ADR 0069 §3).
//
// The dedup + resolution ledger is the SAME alerts table: a per-run row carries the runID in the Alert.Agent
// field (the aggregate conditions leave Agent=""), so a List(namespace) newest-first scan finds the open
// row for (policy, condition, runID). We FIRE once per newly-waiting run and RESOLVE the open row when the
// run leaves the waiting state (approved/denied/expired) — mirroring the aggregate false→true / true→false.
//
// The payload carries a POINTER — a deep-link to the AUTHENTICATED console approval view — never the public
// share link and never an approve-magic-link. Approval stays caller-scoped (POST /api/runs/{id}/resume).

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/alertstore"
	"github.com/ctxmesh/agent-engine/internal/controlplane/auditlog"
)

// urlQueryEscape escapes a query-parameter value for the console deep-link.
func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// evaluateApprovalWaiting is the per-run pass for an approvalWaiting condition. It lists the runs in the
// policy's namespace currently paused on plan_approval, restricts them to the policy's selected agents,
// FIRES a per-run notification for each NEW waiting run (dedup keyed (policy, condition, runID)), and
// RESOLVES the open row for any previously-waiting run that has since left the waiting state.
//
// Nil-safe + fail-open per the reconciler idiom: a missing run store or a read error is a logged skip,
// never a wedge — a transient miss simply re-fires (dedup) or re-resolves next tick.
func (r *AlertPolicyReconciler) evaluateApprovalWaiting(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	agents []agentsv1alpha1.AgentDeployment,
	now metav1.Time,
) {
	log := logf.FromContext(ctx)

	if r.Runs == nil {
		log.V(1).Info("approvalWaiting abstains: run store not wired (no control-plane DB)",
			"alertpolicy", ap.Name, "condition", cond.Name)
		return
	}

	// The set of agent names this policy selects — approval-waiting only notifies for runs whose agent
	// the policy watches (a namespace can hold many agents; a policy scopes to a subset).
	selected := make(map[string]struct{}, len(agents))
	for i := range agents {
		selected[agents[i].Name] = struct{}{}
	}

	waiting, err := r.Runs.ListWaitingApproval(ctx, ap.Namespace)
	if err != nil {
		log.Error(err, "listing plan_approval-waiting runs failed (skipping approvalWaiting this tick)",
			"alertpolicy", ap.Name, "condition", cond.Name, "namespace", ap.Namespace)
		return
	}

	// Snapshot this namespace's alert ledger ONCE per pass — both the per-run dedup (is there already an
	// open row for this runID?) and the resolve pass read it. A read error skips the whole pass (dedup
	// would fail open and could double-fire), so we bail rather than risk a duplicate notification.
	var openRuns map[string]struct{} // runIDs (Alert.Agent) with an OPEN approvalWaiting row for this (policy, condition)
	if r.Alerts != nil {
		rows, listErr := r.Alerts.List(ctx, ap.Namespace, alertstore.MaxListLimit)
		if listErr != nil {
			log.Error(listErr, "listing alerts for approvalWaiting failed (skipping this tick)",
				"alertpolicy", ap.Name, "condition", cond.Name)
			return
		}
		openRuns = make(map[string]struct{})
		for i := range rows {
			if isOpenApprovalRowFor(rows[i], ap.Name, cond.Name) {
				openRuns[rows[i].Agent] = struct{}{} // Alert.Agent carries the runID
			}
		}
	}

	// currentlyWaiting is the set of run ids (in the selected agents) still paused right now — the
	// resolve pass below closes any open per-run alert NOT in this set.
	currentlyWaiting := make(map[string]struct{}, len(waiting))

	for i := range waiting {
		w := waiting[i]
		if _, ok := selected[w.Agent]; !ok {
			continue // not an agent this policy watches
		}
		currentlyWaiting[w.ID] = struct{}{}

		if _, alreadyOpen := openRuns[w.ID]; alreadyOpen {
			continue // dedup: already notified for this (policy, condition, runID) — do not re-fire
		}
		r.recordApprovalWaiting(ctx, ap, cond, w, now)
	}

	// Resolve any open approval-waiting alert for this (policy, condition) whose run has left the waiting
	// state (approved/denied/expired/deleted → no longer listed as waiting).
	for runID := range openRuns {
		if _, stillWaiting := currentlyWaiting[runID]; stillWaiting {
			continue // run still paused — leave the alert open
		}
		r.resolveApprovalRun(ctx, ap, cond, runID)
	}
}

// resolveApprovalRun resolves the open approval-waiting alert(s) for this (policy, condition, runID). It
// re-lists (the snapshot may be stale by the time we resolve) and stamps resolved_at on the matching open
// row. nil-safe + best-effort: a missing store or a resolve error is a logged no-op, never a wedge.
func (r *AlertPolicyReconciler) resolveApprovalRun(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	runID string,
) {
	if r.Alerts == nil {
		return
	}
	log := logf.FromContext(ctx)
	rows, err := r.Alerts.List(ctx, ap.Namespace, alertstore.MaxListLimit)
	if err != nil {
		log.Error(err, "listing alerts to resolve departed approval failed (continuing)",
			"alertpolicy", ap.Name, "condition", cond.Name, "run", runID)
		return
	}
	for i := range rows {
		if !isOpenApprovalRow(rows[i], ap.Name, cond.Name, runID) {
			continue
		}
		if err := r.Alerts.Resolve(ctx, rows[i].ID); err != nil {
			log.Error(err, "resolving departed approval-waiting alert failed (continuing)",
				"alertpolicy", ap.Name, "condition", cond.Name, "run", runID, "id", rows[i].ID)
		}
	}
}

// recordApprovalWaiting persists a per-run approval-waiting alert (+ an audit entry) and dispatches the
// policy's channels — the per-run analogue of recordFired. The runID rides Alert.Agent (the per-run dedup
// key); the console approval deep-link rides the webhook payload's Link/RunID (a POINTER, never a
// capability). All persistence is nil-safe; a channel failure never blocks the durable record.
func (r *AlertPolicyReconciler) recordApprovalWaiting(
	ctx context.Context,
	ap *agentsv1beta1.AlertPolicy,
	cond agentsv1beta1.AlertCondition,
	w WaitingApprovalRun,
	now metav1.Time,
) {
	log := logf.FromContext(ctx)

	link := r.consoleRunLink(ap.Namespace, w.Agent, w.ID)
	summary := strings.TrimSpace(w.Message)
	msg := fmt.Sprintf("Run %s (%s/%s) is waiting for approval", w.ID, ap.Namespace, w.Agent)
	if summary != "" {
		msg = fmt.Sprintf("%s: %s", msg, summary)
	}

	if r.Alerts != nil {
		if _, err := r.Alerts.Append(ctx, alertstore.Alert{
			Namespace:  ap.Namespace,
			PolicyName: ap.Name,
			Condition:  cond.Name,
			Agent:      w.ID, // the runID — the per-run dedup + resolve key
			CondType:   condTypeApprovalWaiting,
			Value:      w.Agent, // the agent the paused run targets (human-readable)
			Message:    msg,
			FiredAt:    now.Time,
		}); err != nil {
			log.Error(err, "persisting approval-waiting alert failed (continuing)",
				"alertpolicy", ap.Name, "condition", cond.Name, "run", w.ID)
		}
	} else {
		log.V(1).Info("alert store not wired — approval-waiting alert not persisted",
			"alertpolicy", ap.Name, "condition", cond.Name, "run", w.ID)
	}

	if r.Audit != nil {
		if err := r.Audit.Append(ctx, auditlog.Entry{
			OccurredAt:   now.Time,
			Source:       auditSourceController,
			Actor:        auditActorAlertPolicy,
			ActorKind:    auditSourceController,
			Action:       "alert.approvalWaiting",
			ResourceKind: "AlertPolicy",
			ResourceName: ap.Name,
			Namespace:    ap.Namespace,
			Outcome:      "success",
			Detail: map[string]any{
				"condition":         cond.Name,
				"type":              condTypeApprovalWaiting,
				"run":               w.ID,
				auditDetailKeyAgent: w.Agent,
			},
		}); err != nil {
			log.Error(err, "auditing approval-waiting alert failed (continuing)",
				"alertpolicy", ap.Name, "condition", cond.Name, "run", w.ID)
		}
	}

	// Dispatch AFTER durable persistence — a webhook failure must not prevent the alert being recorded.
	// notifyChannels never returns an error (logs + continues). The payload carries the run-detail link
	// (a POINTER); it is NOT a share link and NOT an approve-magic-link.
	r.notifyApprovalChannels(ctx, ap, cond, w, msg, link, now.Time)
}

// consoleRunLink builds the deep-link to the AUTHENTICATED console approval view for a waiting run. When
// ConsoleURL is set it is an absolute URL; otherwise a relative path (still a pointer). It targets the
// console's live approval surface (the playground), pre-scoped to the run's agent + namespace, so an
// authenticated operator lands on the approve/deny action. It is NEVER the public share link and NEVER an
// approve-magic-link — approval is resolved caller-scoped via POST /api/runs/{id}/resume, not this URL.
func (r *AlertPolicyReconciler) consoleRunLink(namespace, agent, runID string) string {
	path := fmt.Sprintf("/playground?agent=%s&ns=%s&run=%s",
		urlQueryEscape(agent), urlQueryEscape(namespace), urlQueryEscape(runID))
	base := strings.TrimRight(strings.TrimSpace(r.ConsoleURL), "/")
	if base == "" {
		return path
	}
	return base + path
}

// isOpenApprovalRowFor reports whether an alert row is an OPEN (unresolved) approval-waiting row for this
// (policy, condition) — for ANY run. The runID is carried in Alert.Agent.
func isOpenApprovalRowFor(a alertstore.Alert, policy, condition string) bool {
	return a.PolicyName == policy &&
		a.Condition == condition &&
		a.CondType == condTypeApprovalWaiting &&
		a.ResolvedAt == nil
}

// isOpenApprovalRow reports whether an alert row is the OPEN (unresolved) approval-waiting row for this
// (policy, condition, runID). The runID is carried in Agent (the per-run key).
func isOpenApprovalRow(a alertstore.Alert, policy, condition, runID string) bool {
	return isOpenApprovalRowFor(a, policy, condition) && a.Agent == runID
}
