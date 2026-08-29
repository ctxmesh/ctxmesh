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

// Package enduseragent is the control-plane store for the end-user AGENT exposure mirror (M137/EU1b,
// ADR 0107). The AgentDeployment reconciler mirrors an agent's endpoint + spec here ONLY when
// spec.endUserAccess=true, so the BFF's end-user path resolves an end-user run WITHOUT a K8s read
// (ADR 0011 — no new RBAC). ROW-EXISTENCE IS THE EXPOSURE GATE (fail-closed): Get returns ok=false for
// an unexposed agent (the BFF 404s), and an empty Endpoint signals "not Ready yet" (the BFF 409s).
package enduseragent

import "context"

// ExposedAgent is an agent exposed to end-users, mirrored from its AgentDeployment (ADR 0107).
type ExposedAgent struct {
	Namespace     string
	Agent         string
	Endpoint      string // the invoke URL (status.URL); "" ⇒ the agent is not Ready yet
	RecordCapable bool
	OutputSchema  string
}

// Store persists + reads the exposure mirror. Writes come from the AgentDeployment reconcile (Set on
// converge for an exposed agent; Delete on unset / agent-delete); reads (Get) serve the BFF end-user path.
type Store interface {
	// Set upserts the exposed agent's row.
	Set(ctx context.Context, a ExposedAgent) error
	// Delete removes the (namespace, agent) row (a no-op when absent).
	Delete(ctx context.Context, namespace, agent string) error
	// Get resolves (namespace, agent) → its exposed row. Returns (zero, false, nil) when the agent is
	// NOT exposed to end-users (fail-closed — the BFF treats it as a uniform 404, no existence oracle).
	Get(ctx context.Context, namespace, agent string) (a ExposedAgent, ok bool, err error)
}
