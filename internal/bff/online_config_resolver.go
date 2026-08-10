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

package bff

import (
	"context"
	"fmt"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// k8sOnlineConfigResolver reads the per-agent online-scoring policy from the cluster (ADR 0062 Fork 2,
// m69.6): AgentDeployment.spec.evalSuiteRef → EvalSuite.spec.online. It is backed by a read-only
// controller-runtime client.Reader (the manager's cached client in production, a fake in tests). This is
// the online-scoring worker's ONLY k8s read — a narrow, off-request control-plane read (governance #8),
// NOT the caller-scoped CRD path (ADR 0011); its RBAC is an explicit get/list on agentdeployments +
// evalsuites added to the BFF SA role for exactly this worker.
type k8sOnlineConfigResolver struct {
	reader client.Reader
}

// NewK8sOnlineConfigResolver builds the real resolver over a read-only client.Reader (the manager's
// cached client or a direct reader in cmd/bff/main.go). Returns the OnlineConfigResolver seam the worker
// depends on.
func NewK8sOnlineConfigResolver(reader client.Reader) OnlineConfigResolver {
	return &k8sOnlineConfigResolver{reader: reader}
}

// ResolveOnline implements OnlineConfigResolver: it looks up the AgentDeployment's evalSuiteRef, reads the
// referenced EvalSuite's online block, and parses it into a ResolvedOnlineConfig.
//
//   - agent not found / no evalSuiteRef ⇒ (nil, nil): no policy, worker uses process defaults.
//   - EvalSuite not found ⇒ (nil, nil): a dangling ref is not a hard failure — the worker degrades to
//     defaults (the eval-gate controller separately surfaces the dangling ref on the AgentDeployment).
//   - EvalSuite has no online block ⇒ (nil, nil): defaults.
//   - a genuine API read error (not a NotFound) ⇒ (nil, err): the worker logs it and falls back to
//     defaults for this agent — never a fabricated verdict.
//
// Parsing is lenient (mirrors the worker's "bad config ⇒ default, never a panic" discipline): a malformed
// sampleRate or window parses to 0 (the worker then applies its floor), never an error.
func (r *k8sOnlineConfigResolver) ResolveOnline(ctx context.Context, namespace, agentName string) (*ResolvedOnlineConfig, error) {
	var deploy agentsv1alpha1.AgentDeployment
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: agentName}, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // agent gone (a trace outlived its AgentDeployment) — no policy, defaults.
		}
		return nil, fmt.Errorf("reading AgentDeployment %q/%q: %w", namespace, agentName, err)
	}
	ref := deploy.Spec.EvalSuiteRef
	if ref == "" {
		return nil, nil // no gate/policy — process defaults.
	}

	var suite agentsv1alpha1.EvalSuite
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref}, &suite); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // dangling evalSuiteRef — degrade to defaults, not a hard error.
		}
		return nil, fmt.Errorf("reading EvalSuite %q/%q: %w", namespace, ref, err)
	}
	online := suite.Spec.Online
	if online == nil {
		return nil, nil // suite exists but no online policy — process defaults.
	}

	return &ResolvedOnlineConfig{
		SampleRate:      parseSampleRate(online.SampleRate),
		MaxScoredPerDay: int(online.MaxScoredPerDay),
		Window:          parseWindow(online.Window),
		MinSamples:      int(online.MinSamples),
	}, nil
}

// parseSampleRate parses the CRD's decimal-string sampleRate to a float. Empty or malformed ⇒ 0 (judge
// OFF) — the worker's withDefaults/clamp then keeps it in [0,1]. Never an error (bad config degrades).
func parseSampleRate(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseWindow parses the CRD's Go-duration-string window. Empty or malformed ⇒ 0, so the worker applies
// its platform-default window (1h). Never an error (bad config degrades to the default).
func parseWindow(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}
