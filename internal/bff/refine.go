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
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/ctxmesh/agentry/internal/expand"
)

// This file implements POST /api/agents/refine — the pure in-place editing
// endpoint (m71.1). It takes an existing simplified agent.yaml (CurrentSpec) plus
// a natural-language instruction and rewrites the whole document via the caller's
// connected provider model. It is CALLER-SCOPED (ADR 0011) and PURE — it NEVER
// writes to the cluster; the caller reviews the result before any separate create/
// apply call. It is a sibling of POST /api/agents/generate (ADR 0014) and reuses
// the same model-resolution and chat machinery.
//
// Security: inline credential material is rejected in BOTH the INPUT (CurrentSpec,
// untrusted client input) and the OUTPUT (the model's rewrite) before any processing
// or response — the check runs twice, so a poisoned spec cannot be laundered through
// the model.
//
// Reliability: if the model's first output fails expand-validation the endpoint makes
// ONE internal retry with the expand error appended ("fix it and re-emit the full
// agent.yaml"). If the retry also fails a 422 regenerate response lets the UI surface
// the affordance (same discipline as generate — a bad output is a non-event, never a
// 500, never an apply).

// maxRefineRequestBytes bounds the refine request body (a spec + an instruction +
// a small transcript). 512 KiB is generous for a capped 8-turn transcript and
// stops a hostile large body.
const maxRefineRequestBytes = 512 << 10 // 512 KiB

// refineCostTag marks a refine request's provider metadata (operation=refine) so refine spend is
// DISTINGUISHABLE from generate (create-from-prompt) + agent runs in the provider's cost analytics.
const refineCostTag = "agentry/refine"

// maxTranscriptTurns is the server-enforced cap on the transcript the caller
// passes. Even if the UI sends a longer history we only forward the last N turns
// to the model — keeps the prompt bounded and prevents context-stuffing.
const maxTranscriptTurns = 8

// refineSystemPrompt constrains the model to rewrite the WHOLE agent.yaml applying
// the user's instruction — the same schema the generation prompt uses, but framed
// for editing rather than creation (m71.1, ADR 0014 sibling).
const refineSystemPrompt = `You are editing an existing agent.yaml for the agentry platform.
Rewrite the WHOLE document applying the user's instruction.

Output ONLY the full updated agent.yaml — no prose, no explanation, no markdown code fences,
no diff, no patch. Emit the COMPLETE document, not just the changed fields.

Keep unchanged fields exactly as they are. Apply ONLY what the instruction asks for.

The agent.yaml schema (ONLY these fields are valid; omit any you do not need):
  name: <dns-1123 name, required>
  runtime: managed        # ALWAYS use "managed" — do not change this
  systemPrompt: <the agent's system prompt>
  tools: [<tool catalog name>, ...]   # optional
  model:
    route: <model route alias>        # optional
  resources: { cpu: <e.g. 250m>, memory: <e.g. 256Mi> }   # optional
  scaling: { min: <int>, max: <int> }                       # optional
  budget: { perConversationUSD: <number>, perAgentUSD: <number> }  # optional

Rules:
  - ALWAYS keep "runtime: managed". Do NOT change it to anything else.
  - Do NOT invent fields outside this schema; unknown fields are rejected.
  - Route ANY credential to a secretRef — NEVER inline a key, token, secret, or password.
  - Re-emit the COMPLETE agent.yaml, not a fragment.`

// handleRefine serves POST /api/agents/refine (m71.1). It is PURE: it resolves the
// generation model caller-scoped, rewrites the provided spec via the model, and
// expand-validates the result — returning the new spec + a changed-fields diff for
// review. It NEVER writes to the cluster.
func (s *Server) handleRefine(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRefineRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req RefineAgentRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.CurrentSpec) == "" {
		writeError(w, http.StatusBadRequest, "currentSpec is required")
		return
	}
	if strings.TrimSpace(req.Instruction) == "" {
		writeError(w, http.StatusBadRequest, "instruction is required")
		return
	}

	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// Reject inline secrets in the untrusted INPUT before any model call.
	// A poisoned CurrentSpec must never reach the model or be processed further.
	var inputTree any
	if parseErr := sigsyaml.Unmarshal([]byte(req.CurrentSpec), &inputTree); parseErr != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid currentSpec YAML: %v", parseErr))
		return
	}
	if path := findInlineSecret(inputTree, ""); path != "" {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("inline secrets are not allowed in currentSpec (found credential field %q); reference a SecretBinding by name instead", path))
		return
	}

	// Resolve the provider + model + caller-scoped key (same path as generate).
	gen, gerr := s.resolveGeneration(r.Context(), caller, ns, &GenerateAgentRequest{
		Provider:  req.Provider,
		Model:     req.Model,
		Namespace: req.Namespace,
	})
	if gerr != nil {
		writeError(w, gerr.status, gerr.msg)
		return
	}

	// Cap the transcript server-side regardless of what the client sent.
	transcript := cappedTranscript(req.Transcript, maxTranscriptTurns)

	// Build the user message: CurrentSpec + (optional) capped transcript + Instruction.
	userMsg := buildRefineUserMessage(req.CurrentSpec, transcript, req.Instruction)

	// First attempt.
	agentYAML, manifests, refineErr := s.refineAttempt(r.Context(), gen, userMsg)
	if refineErr != nil {
		// One internal retry: append the expand error to the prompt and try again.
		retryMsg := userMsg + "\n\nYour previous output failed validation with: " + refineErr.Error() +
			"\nFix the issue and re-emit the complete agent.yaml."
		agentYAML, manifests, refineErr = s.refineAttempt(r.Context(), gen, retryMsg)
		if refineErr != nil {
			// Both attempts failed. Before including the raw output in the 422, check if
			// the model emitted an inline secret — if so, omit the raw YAML from the
			// error body so a malicious or confused model cannot smuggle a credential
			// through the regenerate affordance.
			safeYAML := agentYAML
			if rawSecretPath := outputInlineSecretPath(agentYAML); rawSecretPath != "" {
				safeYAML = "" // redact: do not return a secret-bearing raw output
			}
			writeJSON(w, http.StatusUnprocessableEntity, GenerateInvalidResponse{
				Error:      "the refined config was not valid — try a different instruction or regenerate",
				Reason:     refineErr.Error(),
				AgentYAML:  safeYAML,
				Model:      gen.model,
				Provider:   gen.provider,
				Regenerate: true,
			})
			return
		}
	}

	// Reject inline secrets in the OUTPUT before returning anything to the caller.
	// This covers the success path — the model's rewritten spec must never carry
	// inline credentials even when it otherwise expand-validates.
	if secretPath := outputInlineSecretPath(agentYAML); secretPath != "" {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("the model emitted an inline secret (field %q); cannot return the output — use secretRef instead", secretPath))
		return
	}

	// Compute the changed-fields diff (top-level key comparison, never a line-diff).
	diff := changedFields(req.CurrentSpec, agentYAML)

	writeJSON(w, http.StatusOK, RefineAgentResponse{
		AgentYAML: agentYAML,
		Expanded:  string(manifests),
		Diff:      diff,
		Model:     gen.model,
		Provider:  gen.provider,
		Warnings:  []string{},
	})
}

// refineAttempt issues one chatComplete → stripCodeFence → Expand cycle. On
// success it returns (yaml, manifests, nil). On chat failure it returns a wrapped
// error (the caller decides retry vs. 5xx). On expand failure it returns (raw yaml,
// nil, err) so the caller can attach the raw output to the 422 or retry with the
// error.
func (s *Server) refineAttempt(ctx context.Context, gen generationTarget, userMsg string) (string, []byte, error) {
	// Route via the gateway (no caller key) when gen.viaGateway, else the direct provider call — one
	// seam with generate/team-generate (M133). refineCostTag keeps refine spend separately attributable.
	output, err := s.generationChat(ctx, gen, refineSystemPrompt, userMsg, refineCostTag)
	if err != nil {
		return "", nil, err
	}

	agentYAML := stripCodeFence(output)
	manifests, xerr := s.adapters.Expand.Expand(ctx, []byte(agentYAML))
	if xerr != nil {
		var xe *expand.Error
		reason := "the model produced output that could not be validated"
		if errors.As(xerr, &xe) {
			reason = xe.Error()
		} else {
			s.log.Error(xerr, "refine expand-validate failed (unclassified)")
		}
		return agentYAML, nil, errors.New(reason)
	}
	return agentYAML, manifests, nil
}

// registerRefineRoute wires POST /api/agents/refine on the authed mux. It is
// extracted from Handler to keep Handler under the gocyclo threshold (the same
// discipline as registerExtAuthRoutes / registerRunRoutes). Requires BOTH the
// caller-client factory (key is resolved caller-scoped, ADR 0011) and the expand
// adapter (rewrite validated through the one mapping, ADR 0014 sibling); either
// missing → an honest 501.
func (s *Server) registerRefineRoute(mux *http.ServeMux) {
	if s.callerClients != nil && s.adapters.Expand != nil {
		mux.HandleFunc("POST /api/agents/refine", s.handleRefine)
	} else {
		mux.Handle("POST /api/agents/refine", notImplemented("pure spec-editing refine"))
	}
}

// cappedTranscript returns the last n turns from transcript (the server-side cap).
// An empty or short transcript is returned as-is. This prevents the client from
// stuffing unbounded context into the refine prompt.
func cappedTranscript(transcript []RefineTurn, n int) []RefineTurn {
	if len(transcript) <= n {
		return transcript
	}
	return transcript[len(transcript)-n:]
}

// buildRefineUserMessage assembles the user message that the model rewrites:
//  1. The current spec (the input the model must rewrite).
//  2. The (capped) prior transcript as flavor context, labeled so the model
//     understands these are prior exchanges — not authoritative instructions.
//  3. The instruction (the authoritative change request).
func buildRefineUserMessage(currentSpec string, transcript []RefineTurn, instruction string) string {
	var b strings.Builder
	b.WriteString("Current agent.yaml:\n```yaml\n")
	b.WriteString(currentSpec)
	b.WriteString("\n```\n")

	if len(transcript) > 0 {
		b.WriteString("\nPrior conversation context (for flavor only — the instruction below is authoritative):\n")
		for _, turn := range transcript {
			role := strings.TrimSpace(turn.Role)
			if role == "" {
				role = actorKindUser
			}
			b.WriteString(role)
			b.WriteString(": ")
			b.WriteString(strings.TrimSpace(turn.Text))
			b.WriteString("\n")
		}
	}

	b.WriteString("\nInstruction: ")
	b.WriteString(strings.TrimSpace(instruction))
	return b.String()
}

// outputInlineSecretPath parses rawYAML and returns the dotted path of the first
// inline secret it finds (via findInlineSecret). An unparseable document or a clean
// document returns "". This helper is used to check the model's output before
// including it in ANY response — success or error — so inline secrets are never
// returned to the caller regardless of whether the output also passed expand-validation.
func outputInlineSecretPath(rawYAML string) string {
	var tree any
	if err := sigsyaml.Unmarshal([]byte(rawYAML), &tree); err != nil {
		return "" // unparseable — let the expand error take precedence
	}
	return findInlineSecret(tree, "")
}

// changedFields compares two simplified agent.yaml documents and returns a sorted
// list of top-level field names that were added, removed, or modified between
// oldYAML and newYAML. The diff is intentionally shallow (top-level keys only) and
// human-readable — the UI shows a changed-fields summary, not a line-diff. An
// unparseable document returns an empty list (never an error) so a bad parse on the
// diff does not block the response.
func changedFields(oldYAML, newYAML string) []string {
	var oldMap, newMap map[string]any
	_ = sigsyaml.Unmarshal([]byte(oldYAML), &oldMap)
	_ = sigsyaml.Unmarshal([]byte(newYAML), &newMap)

	changed := make(map[string]struct{})

	// Added or modified: present in new but absent from old, or value differs.
	for k, nv := range newMap {
		ov, ok := oldMap[k]
		if !ok {
			changed[k] = struct{}{}
			continue
		}
		// Simple value comparison via re-serialization to JSON to avoid reflect.DeepEqual
		// fragility across yaml→interface{} round-trips (numbers as float64, etc.).
		ovJ, _ := json.Marshal(ov)
		nvJ, _ := json.Marshal(nv)
		if string(ovJ) != string(nvJ) {
			changed[k] = struct{}{}
		}
	}
	// Removed: present in old but absent from new.
	for k := range oldMap {
		if _, ok := newMap[k]; !ok {
			changed[k] = struct{}{}
		}
	}

	result := make([]string, 0, len(changed))
	for k := range changed {
		result = append(result, k)
	}
	slices.Sort(result)
	return result
}
