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
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/controlplane/onlinescore"
)

// onlineConfigWriter is the narrow slice of the online-score store the controller needs to publish each
// agent's online-scoring policy to cpDB (m84.3, ADR 0062 Fork 2 / ADR 0011). The CONTROLLER — which
// legitimately holds evalsuites RBAC — resolves AgentDeployment.spec.evalSuiteRef → EvalSuite.spec.online
// and UPSERTS/DELETES the per-(namespace, agent) row here; the BFF online-scoring worker READS it from cpDB
// (no agent-CRD RBAC). This is the ADR-0011-safe replacement for the reverted BFF-SA agent-CRD ClusterRole.
// A narrow interface lets envtest inject a memstore-backed fake without a real Postgres.
type onlineConfigWriter interface {
	UpsertOnlineConfig(ctx context.Context, cfg onlinescore.OnlineConfig) error
	DeleteOnlineConfig(ctx context.Context, namespace, agentName string) error
}

// onlineConfigAction is the resolved action reconcileOnlineScoreConfig takes for an agent's online policy.
type onlineConfigAction int

const (
	// onlineConfigUpsert: the agent references an EvalSuite with an `.online` block → write the row.
	onlineConfigUpsert onlineConfigAction = iota
	// onlineConfigClear: no evalSuiteRef / dangling ref / no `.online` block → delete the row (judge OFF).
	onlineConfigClear
	// onlineConfigSkip: a transient API read error resolving the suite → leave the existing row untouched
	// (never delete a live policy on a blip; the next reconcile re-resolves).
	onlineConfigSkip
)

// reconcileOnlineScoreConfig publishes (or clears) the agent's online-scoring policy to cpDB every reconcile
// (m84.3). When spec.evalSuiteRef names an EvalSuite with an `.online` block, it UPSERTS the per-(ns, agent)
// config row (enabled + sampleRate + maxScoredPerDay + window + minSamples). When there is NO evalSuiteRef,
// the referenced suite is missing, or the suite has NO `.online` block, it DELETES the row — so the judge is
// OFF for that agent (the fail-safe: no explicit policy ⇒ no judge). The write is a cpDB side-effect, not a
// k8s-object mutation, so it never fights the rest of the reconcile and is idempotent across ticks.
//
// The controller ALREADY holds evalsuites get/list/watch RBAC (agentdeployment_controller.go), so resolving
// the EvalSuite here needs NO new RBAC. When the writer is unwired (dev without cpDB, or envtests that don't
// exercise it) this is a no-op — never a panic. A cpDB write error is logged but NOT surfaced as a reconcile
// error: the online-scoring config is an out-of-band signal for the BFF worker, and blocking the deploy
// reconcile on a cpDB blip would be wrong (the worker safely defaults to judge-OFF on a missing/stale row).
func (r *AgentDeploymentReconciler) reconcileOnlineScoreConfig(ctx context.Context, deploy *agentsv1alpha1.AgentDeployment) {
	if r.OnlineConfig == nil {
		return // writer unwired (dev without cpDB / envtest) — nothing to publish.
	}
	log := logf.FromContext(ctx)

	cfg, action := r.resolveOnlineConfig(ctx, deploy)
	switch action {
	case onlineConfigUpsert:
		if err := r.OnlineConfig.UpsertOnlineConfig(ctx, cfg); err != nil {
			log.Error(err, "online-score config: publishing per-agent policy failed (worker keeps the prior/default policy)",
				"namespace", deploy.Namespace, "agent", deploy.Name)
		}
	case onlineConfigClear:
		if err := r.OnlineConfig.DeleteOnlineConfig(ctx, deploy.Namespace, deploy.Name); err != nil {
			log.Error(err, "online-score config: clearing per-agent policy failed (worker safely defaults to judge-OFF)",
				"namespace", deploy.Namespace, "agent", deploy.Name)
		}
	case onlineConfigSkip:
		// Transient suite-read error already logged in resolveOnlineConfig — leave the existing row untouched.
	}
}

// resolveOnlineConfig resolves the agent's online-scoring policy from its EvalSuite.online block, returning
// the config plus the action to take: Upsert when the agent references an EvalSuite carrying an `.online`
// block; Clear when there is no evalSuiteRef, the suite is dangling (NotFound), or the suite has no `.online`
// block (all ⇒ judge OFF, the fail-safe); Skip on a transient API read error (leave the prior row untouched,
// never delete a live policy on a blip). Parsing is lenient (mirrors the worker's "bad config ⇒ default,
// never a panic" discipline): a malformed sampleRate or window parses to 0.
func (r *AgentDeploymentReconciler) resolveOnlineConfig(ctx context.Context, deploy *agentsv1alpha1.AgentDeployment) (onlinescore.OnlineConfig, onlineConfigAction) {
	if deploy.Spec.EvalSuiteRef == "" {
		return onlinescore.OnlineConfig{}, onlineConfigClear // no gate/policy — clear (judge OFF).
	}

	var suite agentsv1alpha1.EvalSuite
	if err := r.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: deploy.Spec.EvalSuiteRef}, &suite); err != nil {
		if apierrors.IsNotFound(err) {
			return onlinescore.OnlineConfig{}, onlineConfigClear // dangling ref — clear (judge OFF), not a hard error.
		}
		logf.FromContext(ctx).Error(err, "online-score config: reading EvalSuite failed; leaving prior policy untouched",
			"namespace", deploy.Namespace, "evalSuiteRef", deploy.Spec.EvalSuiteRef)
		return onlinescore.OnlineConfig{}, onlineConfigSkip // transient blip — do not mutate the row.
	}

	online := suite.Spec.Online
	if online == nil {
		return onlinescore.OnlineConfig{}, onlineConfigClear // suite exists but no online policy — clear (judge OFF).
	}

	return onlinescore.OnlineConfig{
		Namespace:       deploy.Namespace,
		AgentName:       deploy.Name,
		Enabled:         true,
		SampleRate:      parseOnlineSampleRate(online.SampleRate),
		MaxScoredPerDay: int(online.MaxScoredPerDay),
		Window:          parseOnlineWindow(online.Window),
		MinSamples:      int(online.MinSamples),
	}, onlineConfigUpsert
}

// parseOnlineSampleRate parses the CRD's decimal-string sampleRate to a float. Empty or malformed ⇒ 0 (judge
// OFF) — the worker's clamp keeps it in [0,1]. Never an error (bad config degrades to the safe default).
func parseOnlineSampleRate(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseOnlineWindow parses the CRD's Go-duration-string window. Empty or malformed ⇒ 0, so the worker applies
// its platform-default window (1h). Never an error (bad config degrades to the default).
func parseOnlineWindow(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}
