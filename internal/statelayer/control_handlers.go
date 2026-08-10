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
	"errors"
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
// real-kill poll (m70.8). It is POD-AUTHENTICATED exactly like the quota/dedup endpoints: the agent pod
// presents its projected SA token and the proxy verifies it (authenticatePod) BEFORE any read. This is a
// NON-SECRET, run-scoped read (the verb is "cancel" or empty), so — unlike the quota endpoints — it does
// NOT resolve a tenant: the agent supplies its OWN run id (from its verified capability) and reads only
// that run's marker. The auth gate is what matters (an unauthenticated caller is rejected); the run id is
// opaque and namespaces nothing sensitive.
func (s *Server) handleControlGet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
	defer cancel()

	if s.control == nil || s.podAuth == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "control is not configured on this proxy")
		return
	}
	// Pod-auth FIRST — reject an unauthenticated caller before touching Valkey. Reuse the EXACT pod-token
	// auth the quota/dedup handlers use (authenticatePod → TokenReview); do not weaken it. We only need
	// authentication (is this a real pod?), not the namespace, so the returned ns is discarded.
	if _, err := s.authenticatePod(ctx, bearerToken(r)); err != nil {
		if errors.Is(err, ErrTokenRejected) {
			writeJSONError(w, http.StatusUnauthorized, "invalid pod token")
		} else {
			writeJSONError(w, http.StatusServiceUnavailable, "pod authentication unavailable")
		}
		return
	}

	runID := r.PathValue("runID")
	if runID == "" || len(runID) > maxControlRunID {
		writeJSONError(w, http.StatusBadRequest, "runID is required and must be a bounded id")
		return
	}

	verb, err := s.control.Control(ctx, runID)
	if err != nil {
		// A backend error → 502; the launcher's control client fails OPEN (no verb ⇒ don't cancel), so a
		// Valkey blip never spuriously kills a live run.
		writeJSONError(w, http.StatusBadGateway, "control backend: "+err.Error())
		return
	}
	writeJSON(w, controlResponse{Control: verb})
}
