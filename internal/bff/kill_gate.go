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
	"net/http"
)

// Layer (c) of the scoped kill switch (M146, ADR 0126 §3): a killed scope REFUSES new runs.
//
// This is the layer that makes the stop authoritative rather than advisory. Layer (a) — the state-layer
// marker that aborts an in-flight model call — is deliberately fail-OPEN (ADR 0063 §D3), so it cannot
// carry the guarantee on its own: during exactly the incident a kill exists for, a degraded state layer
// would quietly stop enforcing it.
//
// So this gate reads the CONTROL PLANE and fails CLOSED. An unreachable Valkey changes nothing here; an
// unreadable kill set refuses rather than admits.

// killRefusalStatus is 423 Locked, not 409 or 503. The scope is deliberately held by an operator — a
// state the caller cannot resolve by retrying differently, and which is not a conflict with another
// request nor a transient unavailability. 423 says "this is locked, and someone has to unlock it".
const killRefusalStatus = http.StatusLocked

// refuseIfKilled writes a refusal and returns true when the (namespace, agent) is under an active
// emergency stop, so a handler can `if s.refuseIfKilled(...) { return }`.
//
// FAIL-CLOSED both ways: an unreadable kill set refuses the run, because admitting work we cannot prove
// is permitted is the failure mode the whole feature exists to prevent. The message distinguishes the
// two cases so an operator debugging a refusal is not left guessing whether they are looking at a kill
// or at a broken control plane.
//
// A platform with no kill store configured admits everything, exactly as it did pre-M146 — the feature
// is inert when unprovisioned rather than wedging every create.
func (s *Server) refuseIfKilled(w http.ResponseWriter, ctx context.Context, namespace, agent string) bool {
	if s.killScopes == nil {
		return false
	}
	filter, err := s.claimFilter(ctx)
	if err != nil {
		s.log.Error(err, "kill gate: could not read the active kill set — refusing the run (fail-closed)",
			"namespace", namespace, "agent", agent)
		writeError(w, killRefusalStatus,
			"cannot confirm whether this agent is under an emergency stop — refusing the run (the platform fails closed here)")
		return true
	}
	if !filter.Excludes(namespace, agent) {
		return false
	}
	s.log.Info("kill gate: refusing a run under an active emergency stop",
		"namespace", namespace, "agent", agent)
	writeError(w, killRefusalStatus,
		"this agent is under an emergency stop — new runs are refused until it is lifted")
	return true
}
