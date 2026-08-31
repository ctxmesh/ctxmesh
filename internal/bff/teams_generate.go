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
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

// This file implements POST /api/teams/generate — the team composition endpoint
// (ADR 0065 D4). It composes an AgentTeamSpec referencing EXISTING published
// members of a named AgentRegistry, validated referentially so a hallucinated
// agentRef never reaches the cluster. It is CALLER-SCOPED (ADR 0011) and
// NEVER auto-applies: the generated spec + eligible member list are returned
// for review; the UI creates via the existing team-create path / kubectl.

// teamGenCostTag is the provider cost marker for team-generate calls (analogous to
// generationCostTag for agent-generate): distinguishable spend in provider analytics.
const teamGenCostTag = "agentry/team-generate"

// teamGenInvalidMsg is the client-safe headline for a 422 team-generate response.
const teamGenInvalidMsg = "the generated team spec was not valid — regenerate to try again"

// teamGenerationSystemPrompt constrains the model to emit ONLY a valid AgentTeam
// spec from the provided eligible agents. It is explicit about allowed fields and
// the referential constraint so the output validates; unknown fields or non-listed
// agentRefs fail decode/validation → regenerate.
const teamGenerationSystemPrompt = `You are a team composition tool for the agentry platform.
Compose an AgentTeam from the EXISTING agents listed below.

Output ONLY the AgentTeam YAML — no prose, no explanation, no markdown code fences.

The AgentTeam YAML schema (emit ONLY these fields; omit any you do not need):
  apiVersion: agents.ctxmesh.ai/v1beta1
  kind: AgentTeam
  metadata:
    name: <dns-1123 name, required>
  spec:
    registryRef: <set to the exact registry name provided, required>
    supervisor:
      agentRef: <name of one agent from the eligible list who should orchestrate, required>
    roster:                              # at least one entry, required
      - name: <roster-local identifier, dns-1123>
        agentRef: <MUST be one of the listed agent names>
        description: <short description of what this sub-agent does>
    spawnBudget:                         # optional
      maxFanOut: <int>
      maxSpawnDepth: <int>
      maxTotalSpawns: <int>

CRITICAL RULES:
  - registryRef MUST be set to the exact registry name provided.
  - supervisor.agentRef and every roster[].agentRef MUST be one of the agent names from the ELIGIBLE AGENTS list.
  - Do NOT invent agent names; use ONLY agents from the provided list.
  - Choose the most appropriate agent as supervisor (one with an orchestrator role if evident from its description).
  - Include at least one roster entry.`

// teamMemberInfo carries the name and optional description of one eligible member
// offered to the generation model.
type teamMemberInfo struct {
	name        string
	description string // role or short description, if available; "" when absent
}

// handleGenerateTeam serves POST /api/teams/generate (ADR 0065 D4).
// It resolves the eligible published member set from the given AgentRegistry,
// calls the LLM to compose an AgentTeamSpec, referentially validates every
// agentRef against the eligible set (one internal retry on hallucination), and
// returns the spec + eligible members for review. Never applies.
func (s *Server) handleGenerateTeam(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxGenerateRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req GenerateTeamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}
	if strings.TrimSpace(req.Description) == "" {
		writeError(w, http.StatusBadRequest, "description is required")
		return
	}
	if strings.TrimSpace(req.RegistryRef) == "" {
		writeError(w, http.StatusBadRequest, "registryRef is required")
		return
	}

	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = defaultCreateNamespace
	}
	registryRef := strings.TrimSpace(req.RegistryRef)

	// --- Step 1: resolve the eligible member set (caller-scoped) ---------------
	//
	// Read the AgentRegistry to get status.members, then resolve each member's
	// AgentDeployment to exclude drafts. The eligible set is what we offer the model.
	var registry agentsv1alpha1.AgentRegistry
	if gErr := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: registryRef}, &registry); gErr != nil {
		switch {
		case apierrors.IsNotFound(gErr):
			writeError(w, http.StatusNotFound, fmt.Sprintf("registry %q not found in namespace %q", registryRef, ns))
		case apierrors.IsForbidden(gErr):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to read the registry")
		case apierrors.IsUnauthorized(gErr):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(gErr, "get AgentRegistry failed", "registry", registryRef, "namespace", ns)
			writeError(w, http.StatusBadGateway, "failed to read the registry")
		}
		return
	}

	// Build the eligible member set: status.members minus drafts.
	// For each member name, fetch the AgentDeployment and skip if it is a draft.
	rawMembers := registry.Status.Members
	eligible := make([]teamMemberInfo, 0, len(rawMembers))
	eligibleNames := make([]string, 0, len(rawMembers))

	for _, memberName := range rawMembers {
		var ad agentsv1alpha1.AgentDeployment
		if gErr := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: memberName}, &ad); gErr != nil {
			// If we can't read it (not found / forbidden), skip it rather than hard-failing:
			// the registry may have stale members or the caller may lack RBAC for some.
			s.log.Info("team-generate: skipping unreadable member", "member", memberName, "err", gErr)
			continue
		}
		// Exclude drafts — a draft is not team-eligible until published (ADR 0065 D1).
		if isDraftAgent(&ad) {
			continue
		}
		// Provide a short description from the agent's role, if available.
		desc := ""
		if ad.Spec.Role != "" {
			desc = ad.Spec.Role
		}
		eligible = append(eligible, teamMemberInfo{name: memberName, description: desc})
		eligibleNames = append(eligibleNames, memberName)
	}

	// Empty registry → return an informative response without calling the model.
	if len(eligible) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, GenerateTeamInvalidResponse{
			Error:      "no eligible agents in this registry",
			Reason:     "the registry has no published members — create and publish agents before generating a team",
			TeamYAML:   "",
			Regenerate: false,
		})
		return
	}

	// --- Step 2: resolve generation target (caller-scoped provider + model) ----
	gen, gerr := s.resolveGeneration(r.Context(), caller, ns, &GenerateAgentRequest{
		Provider:  strings.TrimSpace(req.Provider),
		Model:     strings.TrimSpace(req.Model),
		Namespace: ns,
	})
	if gerr != nil {
		writeError(w, gerr.status, gerr.msg)
		return
	}

	// --- Step 3: build the user message with eligible agents -------------------
	userMsg := buildTeamGenerationMessage(req.Description, registryRef, eligible)

	// --- Step 4: call the model (one attempt; one retry on hallucination) ------
	// (caller "user" attribution tag carded with per-tenant virtual keys, m52 — pass "" for now.)
	output, chatErr := s.generationChat(r.Context(), gen, teamGenerationSystemPrompt, userMsg, teamGenCostTag)
	if chatErr != nil {
		if pe, isPE := isProviderError(chatErr); isPE {
			writeError(w, pe.status, pe.msg)
			return
		}
		writeError(w, http.StatusBadGateway, "team generation call failed")
		return
	}

	teamYAML := stripCodeFence(output)

	// --- Step 5: decode + referential validation (with one retry) --------------
	validTeamYAML, reason, valid := validateTeamOutput(teamYAML, registryRef, eligibleNames)
	if !valid {
		// One internal retry: feed the error back to the model.
		retryMsg := buildTeamRetryMessage(req.Description, registryRef, eligible, reason)
		retryOutput, retryErr := s.generationChat(r.Context(), gen, teamGenerationSystemPrompt, retryMsg, teamGenCostTag)
		if retryErr != nil {
			// Retry call failed — surface the original validation failure as 422.
			writeJSON(w, http.StatusUnprocessableEntity, GenerateTeamInvalidResponse{
				Error:      teamGenInvalidMsg,
				Reason:     reason,
				TeamYAML:   teamYAML,
				Model:      gen.model,
				Provider:   gen.provider,
				Regenerate: true,
			})
			return
		}
		retryYAML := stripCodeFence(retryOutput)
		validTeamYAML, reason, valid = validateTeamOutput(retryYAML, registryRef, eligibleNames)
		if !valid {
			writeJSON(w, http.StatusUnprocessableEntity, GenerateTeamInvalidResponse{
				Error:      teamGenInvalidMsg,
				Reason:     reason,
				TeamYAML:   retryYAML,
				Model:      gen.model,
				Provider:   gen.provider,
				Regenerate: true,
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, GenerateTeamResponse{
		TeamYAML:        validTeamYAML,
		Model:           gen.model,
		Provider:        gen.provider,
		Warnings:        []string{},
		EligibleMembers: eligibleNames,
	})
}

// validateTeamOutput decodes the model-emitted YAML into an AgentTeam, checks
// structural requirements (supervisor, at least one roster entry, registryRef
// consistency), and validates every agentRef against the eligible set. Returns
// the YAML (as-is), a reason on failure, and a valid flag. It is a package-level
// function (not a method) so the test file can call it directly.
func validateTeamOutput(teamYAML, registryRef string, eligibleNames []string) (string, string, bool) {
	if strings.TrimSpace(teamYAML) == "" {
		return teamYAML, "the model produced no output", false
	}

	// Decode via sigs.k8s.io/yaml (YAML→JSON→struct); the AgentTeam struct is the
	// schema the controller enforces at runtime, so decoding it here catches field
	// errors before any cluster apply.
	var team agentsv1beta1.AgentTeam
	if decErr := sigsyaml.Unmarshal([]byte(teamYAML), &team); decErr != nil {
		return teamYAML, fmt.Sprintf("the model output could not be decoded as an AgentTeam: %v", decErr), false
	}

	// Structural: supervisor is required.
	if strings.TrimSpace(team.Spec.Supervisor.AgentRef) == "" {
		return teamYAML, "the model omitted spec.supervisor.agentRef (a supervisor is required)", false
	}

	// Structural: at least one roster entry.
	if len(team.Spec.Roster) == 0 {
		return teamYAML, "the model omitted spec.roster (at least one roster entry is required)", false
	}

	// registryRef must match (or be absent — a non-empty wrong value is flagged).
	if r := strings.TrimSpace(team.Spec.RegistryRef); r != "" && r != registryRef {
		return teamYAML, fmt.Sprintf("the model set spec.registryRef to %q but the requested registry is %q", r, registryRef), false
	}

	// Build an O(1) lookup set for eligible names.
	eligibleSet := make(map[string]bool, len(eligibleNames))
	for _, name := range eligibleNames {
		eligibleSet[name] = true
	}

	// Referential validation: supervisor.
	sup := strings.TrimSpace(team.Spec.Supervisor.AgentRef)
	if !eligibleSet[sup] {
		return teamYAML, fmt.Sprintf("supervisor agentRef %q is not in the eligible agent set; use only: %s",
			sup, strings.Join(eligibleNames, ", ")), false
	}

	// Referential validation: every roster entry.
	for i, entry := range team.Spec.Roster {
		ref := strings.TrimSpace(entry.AgentRef)
		if !eligibleSet[ref] {
			return teamYAML, fmt.Sprintf("roster[%d].agentRef %q is not in the eligible agent set; use only: %s",
				i, ref, strings.Join(eligibleNames, ", ")), false
		}
	}

	return teamYAML, "", true
}

// buildTeamGenerationMessage builds the user message sent to the model on the
// first attempt: the team description + the eligible agent list + the registry name.
func buildTeamGenerationMessage(description, registryRef string, eligible []teamMemberInfo) string {
	var sb strings.Builder
	sb.WriteString("Registry name: ")
	sb.WriteString(registryRef)
	sb.WriteString("\n\n")
	sb.WriteString("Team description:\n")
	sb.WriteString(description)
	sb.WriteString("\n\nELIGIBLE AGENTS (use ONLY these agentRefs):\n")
	for _, m := range eligible {
		sb.WriteString("  - name: ")
		sb.WriteString(m.name)
		if m.description != "" {
			sb.WriteString("  # ")
			sb.WriteString(m.description)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// buildTeamRetryMessage builds the user message for the one internal retry,
// incorporating the validation error as feedback so the model can correct itself.
func buildTeamRetryMessage(description, registryRef string, eligible []teamMemberInfo, validationError string) string {
	var sb strings.Builder
	sb.WriteString("Your previous output was rejected:\n")
	sb.WriteString(validationError)
	sb.WriteString("\n\nPlease try again.\n\n")
	sb.WriteString(buildTeamGenerationMessage(description, registryRef, eligible))
	sb.WriteString("\nDo NOT invent any agent names not listed above.\n")
	return sb.String()
}
