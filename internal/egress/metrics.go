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

package egress

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// toolCallsTotal counts tool calls that reach the fan-out chokepoint (an allowed tool, or a
// require-approval tool with a valid voucher — the calls the M82.5 ceiling governs), labeled by agent +
// tool + outcome, so an operator sees WHICH tool a run hammers (J9 — the per-(agent,tool) observability
// M82's per-run TOTAL cap lacked). outcome = "forwarded" (admitted) | "ceiling_denied" (blocked by
// maxToolCallsPerRun / an unattributable call under an active ceiling). Denied tools + require-approval
// tools without a voucher never reach the chokepoint, so they are intentionally NOT counted here (that is
// the deny path, not fan-out). Registered to the default registry; the sidecar serves it at /metrics.
//
// Cardinality is bounded: one sidecar process serves ONE agent (or boundary), so the agent label is
// process-constant and the tool label is bounded by the agent's tool set.
var toolCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "ctxmesh_egress_tool_calls_total",
	Help: "Tool calls at the egress fan-out chokepoint by agent, tool, and outcome " +
		"(forwarded | ceiling_denied) — the per-(agent,tool) observability for the M82.5 fan-out ceiling (J9).",
}, []string{"agent", "tool", "outcome"})

// metricAgentLabel is the stable agent identity for the tool-call metric. The sidecar serves ONE agent (or
// one boundary), so the label is process-constant; the fallbacks keep it non-empty for a clean series.
func (p *Proxy) metricAgentLabel() string {
	switch {
	case p.cfg.ExpectedAgent != "":
		return p.cfg.ExpectedAgent
	case p.cfg.ExpectedBoundary != "":
		return p.cfg.ExpectedBoundary
	default:
		return "unknown"
	}
}

// recordToolCall increments the fan-out chokepoint counter for this sidecar's agent + the given tool +
// outcome. Called at each admit/deny decision in admitFanOut (J9).
func (p *Proxy) recordToolCall(tool, outcome string) {
	toolCallsTotal.WithLabelValues(p.metricAgentLabel(), tool, outcome).Inc()
}
