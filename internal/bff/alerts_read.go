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
	"net/http"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
)

// AlertSummary is one fired-alert row on the wire (GET /api/alerts). It NEVER
// carries internal fields — only the data the console feed needs.
type AlertSummary struct {
	ID         int64   `json:"id"`
	Namespace  string  `json:"namespace"`
	Policy     string  `json:"policy"`
	Condition  string  `json:"condition"`
	Agent      string  `json:"agent,omitempty"`
	Type       string  `json:"type"`
	Value      string  `json:"value,omitempty"`
	Message    string  `json:"message,omitempty"`
	FiredAt    string  `json:"firedAt"`    // RFC3339
	ResolvedAt *string `json:"resolvedAt"` // RFC3339 or null
	Firing     bool    `json:"firing"`     // true when resolvedAt is nil
}

// AlertListResponse is the GET /api/alerts body — a flat list, newest-first.
// No keyset cursor: the alertstore.List returns a bounded newest-first slice
// (limit-capped), matching the simpler store contract (no pagination for now).
type AlertListResponse struct {
	Items []AlertSummary `json:"items"`
}

// handleListAlerts serves GET /api/alerts?namespace=&limit= — the fired-alert
// console feed (M70, ADR 0063 D2). Mirrors handleListAudit:
//   - nil store ⇒ 501 (CONTROLPLANE_DSN not configured)
//   - caller-scoped SSAR on the `alertpolicies` resource (persona gate — a
//     denial is 403, never an empty list)
//   - store List (newest-first, limit-bounded) ⇒ AlertSummary DTOs
func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.alertStore == nil {
		writeError(w, http.StatusNotImplemented,
			"the alerts feed requires the control-plane store (CONTROLPLANE_DSN); it is not enabled")
		return
	}

	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))

	// Persona gate (never per-row): one SSAR on `alertpolicies`. Scoped to
	// ?namespace= when given (a namespaced reader sees only their namespace);
	// empty ⇒ a cluster-wide read check.
	if err := s.authorizeStore(r.Context(), caller, authz.VerbList, resourceAlerts, namespace, ""); err != nil {
		s.writeAuthzError(w, err, "read the alerts feed")
		return
	}

	limit := parseListLimit(r.URL.Query().Get("limit"))
	alerts, err := s.alertStore.List(r.Context(), namespace, limit)
	if err != nil {
		s.log.Error(err, "list alerts from store failed")
		writeError(w, http.StatusInternalServerError, "failed to read the alerts feed")
		return
	}

	items := make([]AlertSummary, 0, len(alerts))
	for i := range alerts {
		a := &alerts[i]
		var resolvedAt *string
		if a.ResolvedAt != nil {
			s := a.ResolvedAt.UTC().Format(time.RFC3339)
			resolvedAt = &s
		}
		items = append(items, AlertSummary{
			ID:         a.ID,
			Namespace:  a.Namespace,
			Policy:     a.PolicyName,
			Condition:  a.Condition,
			Agent:      a.Agent,
			Type:       a.CondType,
			Value:      a.Value,
			Message:    a.Message,
			FiredAt:    a.FiredAt.UTC().Format(time.RFC3339),
			ResolvedAt: resolvedAt,
			Firing:     a.ResolvedAt == nil,
		})
	}
	writeJSON(w, http.StatusOK, AlertListResponse{Items: items})
}
