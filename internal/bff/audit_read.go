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

	"github.com/ctxmesh/agent-engine/internal/controlplane/auditlog"
	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
)

// AuditEventSummary is one audit row on the wire (GET /api/audit). It NEVER carries secret
// material — the store never held any (tokens live only in grant Secrets); `detail` is the
// non-secret context (server name, boundary, userHash, request id, emitting pod).
type AuditEventSummary struct {
	ID           int64          `json:"id"`
	OccurredAt   string         `json:"occurredAt"` // RFC3339
	Source       string         `json:"source"`     // "controller" | "bff"
	Actor        string         `json:"actor"`
	ActorKind    string         `json:"actorKind"` // "user" | "controller" | "system"
	Action       string         `json:"action"`
	ResourceKind string         `json:"resourceKind,omitempty"`
	ResourceName string         `json:"resourceName,omitempty"`
	Namespace    string         `json:"namespace,omitempty"`
	Outcome      string         `json:"outcome"` // "success" | "denied" | "error"
	TraceID      string         `json:"traceId,omitempty"`
	Detail       map[string]any `json:"detail,omitempty"`
}

// AuditListResponse is the paged GET /api/audit body (list-contract cursor, ADR 0019).
type AuditListResponse struct {
	Items      []AuditEventSummary `json:"items"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

// handleListAudit serves GET /api/audit?namespace=&actor=&action=&kind=&limit=&cursor= — the
// compliance audit trail (ADR 0056 §4, executing ADR 0019's read contract). It mirrors
// handleListToolRegistries: nil store ⇒ **501** (audit not enabled); a caller-scoped SSAR on the
// `auditlogs` resource (persona gate — a denial is **403**, never `[]`); then the store's keyset
// page. An unscoped list (`namespace` empty) requires cluster-wide audit-read; a namespaced
// audit-reader passes `?namespace=` and the SSAR is namespace-scoped.
func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.auditStore == nil {
		writeError(w, http.StatusNotImplemented,
			"the audit surface requires the control-plane store (CONTROLPLANE_DSN); it is not enabled")
		return
	}

	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))

	// Persona gate (never per-row): one SSAR on `auditlogs`. Scoped to ?namespace= when given (a
	// namespaced audit-reader sees only their namespace); empty ⇒ a cluster-wide audit-read check.
	if err := s.authorizeStore(r.Context(), caller, authz.VerbList, resourceAuditLogs, namespace, ""); err != nil {
		s.writeAuthzError(w, err, "read the audit log")
		return
	}

	page, err := s.auditStore.List(r.Context(), auditlog.Query{
		Namespace:    namespace,
		Actor:        strings.TrimSpace(r.URL.Query().Get("actor")),
		Action:       strings.TrimSpace(r.URL.Query().Get("action")),
		ResourceKind: strings.TrimSpace(r.URL.Query().Get("kind")),
		PageSize:     parseListLimit(r.URL.Query().Get("limit")),
		Cursor:       r.URL.Query().Get("cursor"),
	})
	if err != nil {
		s.log.Error(err, "list audit events from store failed")
		writeError(w, http.StatusInternalServerError, "failed to read the audit log")
		return
	}

	items := make([]AuditEventSummary, 0, len(page.Items))
	for i := range page.Items {
		e := &page.Items[i]
		items = append(items, AuditEventSummary{
			ID:           e.ID,
			OccurredAt:   e.OccurredAt.UTC().Format(time.RFC3339),
			Source:       e.Source,
			Actor:        e.Actor,
			ActorKind:    e.ActorKind,
			Action:       e.Action,
			ResourceKind: e.ResourceKind,
			ResourceName: e.ResourceName,
			Namespace:    e.Namespace,
			Outcome:      e.Outcome,
			TraceID:      e.TraceID,
			Detail:       e.Detail,
		})
	}
	writeJSON(w, http.StatusOK, AuditListResponse{Items: items, NextCursor: page.NextCursor})
}
