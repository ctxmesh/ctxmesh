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

package statelayer

import (
	"context"
	"net/http"
)

// controlResponse is the /control/{runID} body: the run's control verb ("" when no marker is set —
// the run is not being cancelled). A verb channel (m70.8): v1 only ever carries "cancel"; future verbs
// (nudge / take-over) reuse the same shape, so an older launcher that only handles "cancel" ignores them.
type controlResponse struct {
	Control string `json:"control"`
}

// maxControlRunID bounds the caller-supplied run id (defence-in-depth; the launcher supplies its OWN
// verified run id, but the path value is still untrusted input on the wire).
const maxControlRunID = 256

// handleControlGet serves GET /control/{runID} — the run's CONTROL verb, for the launcher gateway's
// real-kill poll (m70.8). It is AGENT-AUTHENTICATED exactly like the quota/dedup endpoints
// (authenticateAgentNamespace, m79.2/C7): the agent pod presents its projected SA token and the proxy
// verifies it is a real AGENT identity BEFORE any read — a verified-but-non-agent SA is 403'd (m52.C12,
// closing the auth-only gap where any pod in a tenant namespace could read /control). This is a
// NON-SECRET read (the verb is "cancel" or empty). Since M146 (ADR 0126) it answers for the whole SCOPE
// HIERARCHY covering the run — run → agent → namespace → tenant → fleet — resolved server-side from the
// verified pod identity, so `GET /control/{runID}` keeps its exact wire contract and an already-deployed
// launcher gains scope kills without an image rebuild. It DOES resolve a tenant now (best-effort; see
// below), unlike the pre-M146 run-only read.
func (s *Server) handleControlGet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()

	if s.control == nil || s.podAuth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "control is not configured on this proxy")
		return
	}
	// Agent-auth FIRST — reject an unauthenticated OR non-agent caller before touching Valkey. Mirror the
	// quota/dedup endpoints (authenticateAgentNamespace, m79.2/C7): a verified-but-non-agent SA (e.g. the
	// namespace `default`) is 403'd, not let in — closing the C12 gap where any pod in a tenant namespace
	// could read /control.
	//
	// The namespace is now KEPT, not discarded (M142.3, C15). Authentication alone left the read
	// unscoped: any verified agent could name any run id and learn whether it was being cancelled — a
	// cross-tenant read in a shared state layer. The namespace comes from the pod's own verified identity
	// and is what the key is built from, so a cross-namespace read is structurally impossible rather than
	// merely refused. The proxy cannot check per-agent ownership directly: it holds no run→agent mapping,
	// and its runcap verifier was deliberately retired (ADR 0052 §C6) so a compromised proxy carries no
	// user credential — a property worth more than the finer-grained check would be.
	// M146 (ADR 0126): derive the FULL identity, not just the namespace. The agent name comes from the
	// same un-forgeable agent-<name> SA the namespace does, so widening the read to the agent/ns/tenant/
	// fleet scopes costs no new trust — every scope is still derived from the verified pod, never named
	// by the caller. The wire contract is unchanged (GET /control/{runID}), which is what lets an
	// ALREADY-DEPLOYED agent inherit scope kills with no image rebuild.
	ns, agent, err := s.authenticateAgentIdentity(ctx, bearerToken(r))
	if err != nil {
		writeAgentAuthError(w, err)
		return
	}

	runID := r.PathValue("runID")
	if runID == "" || len(runID) > maxControlRunID {
		writeJSONError(w, http.StatusBadRequest, "runID is required and must be a bounded id")
		return
	}

	// The tenant scope is best-effort: a resolver error or an untenanted namespace SKIPS that scope
	// rather than failing the read. This layer is the fail-OPEN accelerator (ADR 0126 §2) — the
	// fail-CLOSED layers that actually enforce a kill (stop-the-drain, refuse-create) read the control
	// plane, so a tenant kill still holds even when this lookup cannot resolve it.
	tenant, _, tErr := s.resolveTenant(ctx, ns)
	if tErr != nil {
		tenant = ""
	}

	verb, err := s.control.Control(ctx, ControlScope{
		Namespace: ns, Agent: agent, Tenant: tenant, RunID: runID,
	})
	if err != nil {
		// A backend error → 502; the launcher's control client fails OPEN (no verb ⇒ don't cancel), so a
		// Valkey blip never spuriously kills a live run.
		writeJSONError(w, http.StatusBadGateway, "control backend: "+err.Error())
		return
	}
	writeJSON(w, controlResponse{Control: verb})
}
