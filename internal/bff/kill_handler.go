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
	"encoding/json"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/agentry/internal/controlplane/auditlog"
	"github.com/ctxmesh/agentry/internal/controlplane/killscope"
)

// The kill-switch control surface (M146.5, ADR 0126 §5).
//
// A fleet stop is the most destructive control the platform has, so it is gated by its OWN verb rather
// than riding an existing persona. `kill` is a custom verb on the EXISTING agentdeployments resource:
// Kubernetes RBAC takes arbitrary verbs, so this needs no new CRD and no new BFF privilege — the check
// is the same caller-scoped SelfSubjectAccessReview every other control-plane write uses (ADR 0011: the
// BFF's own service account still holds `rules: []`).
//
// The shipped ClusterRole `agentry-killswitch` carries this verb and is bound to NOBODY by default. An
// operator binds it deliberately. That is the point: a control that can halt the fleet should require an
// explicit, auditable grant, not arrive implied by "operator".

// verbKill is the custom RBAC verb gating every kill-switch mutation.
const verbKill = "kill"

// killRequest is the wire body for a kill or an un-kill.
type killRequest struct {
	// Level is agent | namespace | tenant | fleet.
	Level string `json:"level"`
	// Namespace is required for agent/namespace, forbidden otherwise.
	Namespace string `json:"namespace,omitempty"`
	// Agent is required for the agent level.
	Agent string `json:"agent,omitempty"`
	// Tenant is required for the tenant level.
	Tenant string `json:"tenant,omitempty"`
	// Reason is required on a kill. An unexplained fleet stop discovered at 3am is nearly as bad as no
	// stop at all, so this is enforced rather than encouraged.
	Reason string `json:"reason,omitempty"`
}

// killResponse reports the outcome so an un-kill of a scope that was not killed reads as an honest
// no-op instead of a success that did nothing.
type killResponse struct {
	Scope   string `json:"scope"`
	Applied bool   `json:"applied"`
}

// activeKill is one live stop, for the console banner.
type activeKill struct {
	Scope     string `json:"scope"`
	Level     string `json:"level"`
	Namespace string `json:"namespace,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Tenant    string `json:"tenant,omitempty"`
	Reason    string `json:"reason"`
	Principal string `json:"principal"`
}

// parseKillScope decodes and validates a request into a scope. It rejects rather than normalises: a
// safety control must mean exactly what the operator wrote, so an agent kill that also names a tenant is
// an error, not a silently-narrowed agent kill.
func parseKillScope(r *http.Request) (killscope.Scope, killRequest, error) {
	var req killRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<10)).Decode(&req); err != nil {
		return killscope.Scope{}, req, err
	}
	level, err := killscope.ParseLevel(req.Level)
	if err != nil {
		return killscope.Scope{}, req, err
	}
	sc := killscope.Scope{Level: level, Namespace: req.Namespace, Agent: req.Agent, Tenant: req.Tenant}
	return sc, req, sc.Validate()
}

// authorizeKill runs the caller-scoped SSAR for the kill verb. A namespace-scoped stop is checked in
// THAT namespace; a tenant or fleet stop is checked cluster-wide (namespace ""), so halting more than
// one namespace requires cluster-wide authority rather than authority over any single one.
func (s *Server) authorizeKill(ctx context.Context, caller client.Client, sc killscope.Scope) error {
	ns := ""
	if sc.Level == killscope.LevelAgent || sc.Level == killscope.LevelNamespace {
		ns = sc.Namespace
	}
	return s.authorizeStore(ctx, caller, verbKill, resourceAgentDeployments, ns, "")
}

// handleKill serves POST /api/kill — record an emergency stop.
func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	if s.killScopes == nil {
		writeError(w, http.StatusNotImplemented, "the kill switch is not configured on this install")
		return
	}
	sc, req, err := parseKillScope(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required — an unexplained stop is nearly as bad as none")
		return
	}
	ctx := r.Context()
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if aErr := s.authorizeKill(ctx, caller, sc); aErr != nil {
		s.auditKill(ctx, caller, sc, req.Reason, "kill", "denied")
		s.writeAuthzError(w, aErr, "stop agents (the kill verb)")
		return
	}

	principal := s.killPrincipal(ctx, caller)
	if kErr := s.killScopes.Kill(ctx, killscope.Kill{Scope: sc, Reason: req.Reason, Principal: principal}); kErr != nil {
		s.log.Error(kErr, "kill switch: could not record the stop", "scope", sc.Key())
		s.auditKill(ctx, caller, sc, req.Reason, "kill", "error")
		writeError(w, http.StatusInternalServerError, "could not record the stop")
		return
	}
	// The control-plane row is authoritative and is now committed. The marker only ACCELERATES the
	// interrupt of in-flight calls (ADR 0126 §2), so a publish failure is logged and never fails the
	// kill — the stop already holds at the layers that enforce it.
	s.publishKillMarker(ctx, sc, controlVerbCancel)
	s.killFilter = killFilterCache{} // take effect on the next claim rather than after the cache TTL
	s.auditKill(ctx, caller, sc, req.Reason, "kill", "success")
	s.log.Info("kill switch: scope STOPPED", "scope", sc.Key(), "principal", principal, "reason", req.Reason)
	writeJSON(w, http.StatusOK, killResponse{Scope: sc.Key(), Applied: true})
}

// handleUnkill serves POST /api/kill/lift — lift a stop. A distinct, separately audited act: an un-kill
// is as consequential as the kill and must never be a silent expiry.
func (s *Server) handleUnkill(w http.ResponseWriter, r *http.Request) {
	if s.killScopes == nil {
		writeError(w, http.StatusNotImplemented, "the kill switch is not configured on this install")
		return
	}
	sc, _, err := parseKillScope(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if aErr := s.authorizeKill(ctx, caller, sc); aErr != nil {
		s.auditKill(ctx, caller, sc, "", "unkill", "denied")
		s.writeAuthzError(w, aErr, "lift a stop (the kill verb)")
		return
	}

	lifted, uErr := s.killScopes.Unkill(ctx, sc)
	if uErr != nil {
		s.auditKill(ctx, caller, sc, "", "unkill", "error")
		writeError(w, http.StatusInternalServerError, "could not lift the stop")
		return
	}
	s.publishKillMarker(ctx, sc, "") // clear the accelerator; best-effort like the kill
	s.killFilter = killFilterCache{}
	s.auditKill(ctx, caller, sc, "", "unkill", "success")
	s.log.Info("kill switch: scope LIFTED", "scope", sc.Key(), "lifted", lifted)
	writeJSON(w, http.StatusOK, killResponse{Scope: sc.Key(), Applied: lifted})
}

// handleListKills serves GET /api/kills — the active stops, for the console banner.
func (s *Server) handleListKills(w http.ResponseWriter, r *http.Request) {
	if s.killScopes == nil {
		writeJSON(w, http.StatusOK, []activeKill{})
		return
	}
	kills, err := s.killScopes.Active(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read the active stops")
		return
	}
	out := make([]activeKill, 0, len(kills))
	for _, k := range kills {
		out = append(out, activeKill{
			Scope: k.Scope.Key(), Level: string(k.Scope.Level), Namespace: k.Scope.Namespace,
			Agent: k.Scope.Agent, Tenant: k.Scope.Tenant, Reason: k.Reason, Principal: k.Principal,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// killPrincipal resolves who is acting, for the audit row and the console banner. It falls back to a
// marker rather than an empty string: an unattributed kill should read as unattributed, not as nobody.
func (s *Server) killPrincipal(ctx context.Context, caller client.Client) string {
	if caller != nil {
		if u, err := callerUsername(ctx, caller); err == nil && u != "" {
			return u
		}
	}
	return "unattributed"
}

// auditKill appends the audit row. Every outcome is recorded — including `denied`, because an attempted
// fleet stop by someone without the verb is exactly the event a security review wants to see.
func (s *Server) auditKill(ctx context.Context, caller client.Client, sc killscope.Scope, reason, action, outcome string) {
	if s.auditStore == nil {
		return
	}
	principal := s.killPrincipal(ctx, caller)
	entry := auditlog.Entry{
		OccurredAt:   time.Now().UTC(),
		Source:       "bff",
		Actor:        principal,
		ActorKind:    actorKindUser,
		Action:       "killswitch." + action,
		ResourceKind: "KillScope",
		ResourceName: sc.Key(),
		Namespace:    sc.Namespace,
		Outcome:      outcome,
		Detail: map[string]any{
			"level": string(sc.Level), scopeTypeAgent: sc.Agent, scopeTypeTenant: sc.Tenant, "reason": reason,
		},
		DedupKey: "killswitch:" + action + ":" + sc.Key() + ":" + time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := s.auditStore.Append(ctx, entry); err != nil {
		s.log.Error(err, "kill switch: could not append the audit row", "scope", sc.Key(), "action", action)
	}
}

// publishKillMarker writes (or clears) the state-layer accelerator for a scope. Best-effort by design:
// the control-plane row is authoritative, so a marker failure delays the interrupt of in-flight calls
// but never weakens the stop (ADR 0126 §2).
func (s *Server) publishKillMarker(ctx context.Context, sc killscope.Scope, verb string) {
	if s.runControl == nil {
		return
	}
	if err := s.runControl.PublishScope(ctx, sc.MarkerKey(), verb); err != nil {
		s.log.Info("kill switch: could not publish the accelerator marker (the stop still holds)",
			"scope", sc.Key(), "err", err.Error())
	}
}
