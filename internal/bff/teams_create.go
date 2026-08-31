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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	sigsyaml "sigs.k8s.io/yaml"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// This file implements POST /api/teams — the team-create endpoint (ADR 0065 D4).
// It decodes a caller-supplied AgentTeam YAML, validates structure, and creates
// the team caller-scoped (ADR 0011). It never auto-applies from generate; the
// caller must call this endpoint explicitly after reviewing the generated YAML.

// CreateTeamRequest is the POST /api/teams body.
type CreateTeamRequest struct {
	// TeamYAML is the AgentTeam manifest to create (required). Must be a valid
	// AgentTeam with spec.supervisor.agentRef and at least one roster entry.
	TeamYAML string `json:"teamYAML"`
	// Namespace scopes the create; empty → the default namespace.
	Namespace string `json:"namespace,omitempty"`
}

// handleCreateTeam serves POST /api/teams (ADR 0065 D4).
// It is caller-scoped (ADR 0011): RBAC is enforced by the caller's own token.
// Structural validation (supervisor, roster, no inline secrets) runs before the
// K8s write; a viewer → 403; a name collision → 409.
func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxGenerateRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req CreateTeamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}
	if strings.TrimSpace(req.TeamYAML) == "" {
		writeError(w, http.StatusBadRequest, "teamYAML is required")
		return
	}

	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// Defense-in-depth: reject inline secrets (consistent with the agent-create path).
	if path := findInlineSecretInYAML(req.TeamYAML); path != "" {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("inline secret rejected at %s — store credentials in Kubernetes Secrets, not in the team manifest", path))
		return
	}

	// Decode the YAML into a typed AgentTeam (the same path teams_generate.go uses).
	var team agentsv1beta1.AgentTeam
	if decErr := sigsyaml.Unmarshal([]byte(req.TeamYAML), &team); decErr != nil {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("teamYAML could not be decoded as an AgentTeam: %v", decErr))
		return
	}

	// Structural validation: supervisor is required.
	if strings.TrimSpace(team.Spec.Supervisor.AgentRef) == "" {
		writeError(w, http.StatusBadRequest, "teamYAML must include spec.supervisor.agentRef")
		return
	}

	// Structural validation: at least one roster entry.
	if len(team.Spec.Roster) == 0 {
		writeError(w, http.StatusBadRequest, "teamYAML must include at least one spec.roster entry")
		return
	}

	// Structural validation: registryRef is required so the team can resolve members.
	if strings.TrimSpace(team.Spec.RegistryRef) == "" {
		writeError(w, http.StatusBadRequest, "teamYAML must include spec.registryRef")
		return
	}

	// Stamp the namespace from the request.
	team.Namespace = ns

	// Create caller-scoped — the K8s API server enforces the caller's own RBAC.
	if cErr := caller.Create(r.Context(), &team); cErr != nil {
		switch {
		case apierrors.IsAlreadyExists(cErr):
			writeError(w, http.StatusConflict,
				fmt.Sprintf("AgentTeam %q already exists in namespace %q", team.Name, ns))
		case apierrors.IsForbidden(cErr):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to create AgentTeam")
		case apierrors.IsUnauthorized(cErr):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		case apierrors.IsInvalid(cErr):
			writeError(w, http.StatusBadRequest, fmt.Sprintf("AgentTeam rejected by API server: %v", cErr))
		default:
			s.log.Error(cErr, "create AgentTeam failed", "name", team.Name, "namespace", ns)
			writeError(w, http.StatusBadGateway, "failed to create AgentTeam")
		}
		return
	}

	writeJSON(w, http.StatusCreated, newAgentTeamSummary(&team))
}

// findInlineSecretInYAML is a thin wrapper that parses the YAML into a generic
// tree and calls findInlineSecret (from agents_source_spec.go) to detect
// credential-shaped values. Returns the dotted field path of the first violation
// found, or "" when clean. Defense-in-depth: teams should never carry credentials,
// but we enforce this consistently with the agent-create path.
func findInlineSecretInYAML(yamlStr string) string {
	var tree any
	if err := sigsyaml.Unmarshal([]byte(yamlStr), &tree); err != nil {
		// Unparseable YAML — the caller will get a decode error later; no secret path.
		return ""
	}
	return findInlineSecret(tree, "")
}
