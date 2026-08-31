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

	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
)

// onlineConfigReader is the narrow slice of the online-score store the resolver reads: the per-(namespace,
// agent) online-scoring config row the CONTROLLER publishes (m84.3). The BFF worker NEVER reads the agent
// CRDs itself — that would re-introduce the ADR-0011 violation the m69.6 revert removed; instead the
// controller (which legitimately holds evalsuites RBAC) resolves EvalSuite.spec.online → cpDB, and the
// worker reads that cpDB row here.
type onlineConfigReader interface {
	GetOnlineConfig(ctx context.Context, namespace, agentName string) (onlinescore.OnlineConfig, bool, error)
}

// dbOnlineConfigResolver resolves the per-agent online-scoring policy from the CONTROL-PLANE DB (m84.3, ADR
// 0062 Fork 2 / ADR 0011). It implements OnlineConfigResolver by reading the per-(namespace, agent) config
// row the controller wrote (evalSuiteRef → EvalSuite.spec.online → cpDB). This is the ADR-0011-safe path:
// the BFF worker holds NO agent-CRD RBAC and reads only cpDB (which it already reaches). A missing row, or a
// row with enabled=false, ⇒ (nil, nil): the worker falls back to its process-wide defaults (judge OFF) — the
// fail-safe.
type dbOnlineConfigResolver struct {
	store onlineConfigReader
}

// NewDBOnlineConfigResolver builds the cpDB-backed resolver over the online-score store. Returns the
// OnlineConfigResolver seam the worker depends on. Wired in cmd/bff/main.go from the SAME cpDB store the
// worker uses to write aggregates — no new dependency, no agent-CRD RBAC.
func NewDBOnlineConfigResolver(store onlineConfigReader) OnlineConfigResolver {
	return &dbOnlineConfigResolver{store: store}
}

// ResolveOnline implements OnlineConfigResolver: it reads the per-(namespace, agent) config row from cpDB.
//
//   - no row (never published) ⇒ (nil, nil): no policy, worker uses process defaults (judge OFF).
//   - a row with enabled=false ⇒ (nil, nil): the controller explicitly cleared the policy (no evalSuiteRef
//     or no `.online` block) — judge OFF, the fail-safe.
//   - an enabled row ⇒ the parsed ResolvedOnlineConfig (sampleRate/maxScoredPerDay/window/minSamples).
//   - a genuine store read error ⇒ (nil, err): the worker logs it and falls back to defaults for this agent
//     (never a fabricated verdict), exactly as the resolver contract requires.
func (r *dbOnlineConfigResolver) ResolveOnline(ctx context.Context, namespace, agentName string) (*ResolvedOnlineConfig, error) {
	if r.store == nil {
		return nil, nil // no store wired — process defaults (judge OFF).
	}
	cfg, found, err := r.store.GetOnlineConfig(ctx, namespace, agentName)
	if err != nil {
		return nil, fmt.Errorf("reading online-score config for %q/%q: %w", namespace, agentName, err)
	}
	if !found || !cfg.Enabled {
		return nil, nil // no policy / explicitly disabled — process defaults (judge OFF), the fail-safe.
	}
	return &ResolvedOnlineConfig{
		SampleRate:      cfg.SampleRate,
		MaxScoredPerDay: cfg.MaxScoredPerDay,
		Window:          cfg.Window,
		MinSamples:      cfg.MinSamples,
	}, nil
}
