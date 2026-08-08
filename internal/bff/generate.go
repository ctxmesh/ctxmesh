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
	"io"
	"net/http"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/expand"
)

// This file implements POST /api/agents/generate — the create-from-prompt
// generation endpoint (ADR 0014). It turns a natural-language description into a
// REVIEWED, expand-validated agent config. It is CALLER-SCOPED (ADR 0011): the
// generation model's key is resolved with the CALLER'S own client via the SAME
// route → SecretBinding → Secret path the connect flow's model-list re-probe uses
// (providers.go), so a viewer who cannot read the Secret is denied by the API
// server (403 surfaces). The key is used ONLY for the one chat call — it is never
// returned in a DTO and never logged.
//
// The endpoint NEVER auto-applies: a valid generation returns the config for a
// review step; an invalid one returns the raw output + the expand error (HTTP
// 422) so the UI shows a regenerate affordance — not a 500, not an apply.

// maxGenerateRequestBytes bounds the generate body (a short description + a couple
// of small strings). 64 KiB is generous and stops a hostile large body.
const maxGenerateRequestBytes = 64 << 10 // 64 KiB

// generationSystemPrompt constrains the model to emit ONLY the simplified
// agent.yaml the internal/expand core accepts — the exact schema the CLI + form
// consume (ADR 0014: one mapping, no divergent generator). It is deliberately
// explicit about the allowed fields and the managed-runtime default so the output
// expand-validates; a field expand does not understand fails validation →
// regenerate.
const generationSystemPrompt = `You are a configuration generator for the agent-engine platform.
Turn the user's description of an agent into a single simplified agent.yaml document.

Output ONLY the YAML — no prose, no explanation, no markdown code fences.

The agent.yaml schema (emit ONLY these fields; omit any you do not need):
  name: <dns-1123 name, required>
  runtime: managed        # ALWAYS use "managed" (no Docker build required)
  systemPrompt: <the agent's system prompt, required for a useful agent>
  tools: [<tool catalog name>, ...]   # optional; only tools the user asked for
  model:
    route: <model route alias>        # optional
  resources: { cpu: <e.g. 250m>, memory: <e.g. 256Mi> }   # optional
  scaling: { min: <int>, max: <int> }                       # optional
  budget: { perConversationUSD: <number>, perAgentUSD: <number> }  # optional

Rules:
  - ALWAYS set "runtime: managed" and DO NOT set "image".
  - Do NOT invent fields outside this schema; unknown fields are rejected.
  - Prefer a concise, well-scoped systemPrompt derived from the description.`

// handleGenerate serves POST /api/agents/generate (ADR 0014). It resolves the
// generation model (the caller's connected provider by default; a platform-pinned
// model when the operator configured them and the request picked one), reads the
// provider key CALLER-SCOPED, calls the provider chat/completions server-side with
// a schema-constraining system prompt, and validates the emitted YAML through the
// SAME internal/expand core. A valid generation → the config + CRD preview for
// review (never applied). An invalid generation → the raw output + the expand
// error (422) so the UI regenerates — not a 500, not an apply.
func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxGenerateRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req GenerateAgentRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Description) == "" {
		writeError(w, http.StatusBadRequest, "description is required")
		return
	}

	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// Resolve the generation model + the caller-scoped key. This reuses the connect
	// flow's route → SecretBinding → Secret path (providers.go), so RBAC is the API
	// server's and the key stays server-side.
	gen, gerr := s.resolveGeneration(r.Context(), caller, ns, &req)
	if gerr != nil {
		writeError(w, gerr.status, gerr.msg)
		return
	}

	// Server-side chat/completions. The key rides on the request headers only; it
	// is never returned or logged. A rejected key → 422 (an upstream key rejection,
	// NOT the caller's session — FUNC-9/ADR 0027), an unreachable provider → 502
	// (honest, never a 500).
	output, err := chatComplete(r.Context(), s.providerHTTP,
		gen.provider, gen.apiKey, gen.baseURL, gen.model, generationSystemPrompt, req.Description)
	if err != nil {
		if pe, isPE := isProviderError(err); isPE {
			writeError(w, pe.status, pe.msg)
			return
		}
		writeError(w, http.StatusBadGateway, "generation call failed")
		return
	}

	// Strip any stray markdown fences the model may wrap the YAML in, then validate
	// through the SAME expand core the CLI + form use — one mapping, no divergence.
	agentYAML := stripCodeFence(output)
	manifests, err := s.adapters.Expand.Expand(r.Context(), []byte(agentYAML))
	if err != nil {
		// An invalid generation is NOT a 500 and is NOT applied. Hand back the raw
		// output + the validation reason so the UI shows a REGENERATE affordance.
		var xe *expand.Error
		reason := "the model produced output that could not be validated"
		if errors.As(err, &xe) {
			reason = xe.Error()
		} else {
			// A non-expand error here is a server fault (the pure transform should
			// only ever return *expand.Error). Log it, but STILL let the user
			// regenerate rather than 500 — a miss must stay recoverable.
			s.log.Error(err, "generation expand-validate failed (unclassified)")
		}
		writeJSON(w, http.StatusUnprocessableEntity, GenerateInvalidResponse{
			Error:      "the generated config was not valid — regenerate to try again",
			Reason:     reason,
			AgentYAML:  agentYAML,
			Model:      gen.model,
			Provider:   gen.provider,
			Regenerate: true,
		})
		return
	}

	writeJSON(w, http.StatusOK, GenerateAgentResponse{
		AgentYAML: agentYAML,
		Expanded:  string(manifests),
		Model:     gen.model,
		Provider:  gen.provider,
		Warnings:  []string{},
	})
}

// generationTarget bundles the resolved provider + model + the caller-scoped key
// for the one chat call. apiKey is never surfaced beyond chatComplete.
type generationTarget struct {
	provider string
	model    string
	baseURL  string
	apiKey   string
}

// resolveGeneration determines which provider + model to generate through and
// reads the provider key CALLER-SCOPED, returning a *providerError-shaped
// *createError (status + safe message) on failure — never leaking the key.
//
// Model selection (ADR 0014/0015):
//   - Platform-pinned models: when the operator pinned a generation-model list
//     (a values seam) AND the request named a Model, it must be one of the pinned
//     models (else 400). The pinned models are the UI dropdown source.
//   - Connected-provider default: otherwise the caller's connected provider (the
//     named req.Provider, or the single connected one) supplies the key + the
//     model (req.Model if given, else the route's primary model).
//
// The key comes from the connect-managed ModelRoute → SecretBinding → Secret, read
// with the caller's client (the SAME path as providers.go's model re-probe).
func (s *Server) resolveGeneration(ctx context.Context, caller client.Client, ns string, req *GenerateAgentRequest) (generationTarget, *createError) {
	reqModel := strings.TrimSpace(req.Model)

	// If the operator pinned platform generation models and the request picked one,
	// enforce membership (a pinned model outside the list is a 400). The connected
	// provider still supplies the KEY — the pin only governs which model is used.
	pinned := s.platformGenerationModels
	if len(pinned) > 0 && reqModel != "" && !slices.Contains(pinned, reqModel) {
		return generationTarget{}, &createError{
			status: http.StatusBadRequest,
			msg:    "the requested model is not an allowed platform generation model",
		}
	}

	// Resolve the connected provider route (the KEY source). req.Provider names it;
	// empty → the caller's single connected provider (400 if none / ambiguous).
	route, rerr := s.resolveConnectedRoute(ctx, caller, ns, strings.TrimSpace(req.Provider))
	if rerr != nil {
		return generationTarget{}, rerr
	}
	provider, secretBindingRef, baseURL := routeProbeInputs(route)
	if secretBindingRef == "" {
		return generationTarget{}, &createError{
			status: http.StatusConflict,
			msg:    "the connected provider has no secret binding to read the key from",
		}
	}

	apiKey, kerr := readBindingKey(ctx, caller, ns, secretBindingRef)
	if kerr != nil {
		return generationTarget{}, kerr
	}

	// Pick the model: an explicit request model wins (already pin-checked above);
	// else the route's primary model.
	model := reqModel
	if model == "" {
		model = routePrimaryModel(route)
	}
	if model == "" {
		return generationTarget{}, &createError{
			status: http.StatusBadRequest,
			msg:    "no generation model available — pick a model or reconnect the provider",
		}
	}

	return generationTarget{provider: provider, model: model, baseURL: baseURL, apiKey: apiKey}, nil
}

// resolveConnectedRoute returns the connect-managed ModelRoute to generate through.
// When name is set it is fetched by name (and must be connect-managed); when empty
// the caller's connect-managed routes are listed and the SINGLE one is used — zero
// connected providers or more than one (with no name) is an honest 400 asking the
// caller to connect / pick. All reads are caller-scoped (RBAC surfaces as 403).
func (s *Server) resolveConnectedRoute(ctx context.Context, caller client.Client, ns, name string) (*agentsv1alpha1.ModelRoute, *createError) {
	if name != "" {
		var route agentsv1alpha1.ModelRoute
		if err := caller.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, &route); err != nil {
			return nil, getErrorToCreateError(err, "provider")
		}
		if route.Labels[labelManagedBy] != managedByConnect {
			return nil, &createError{status: http.StatusBadRequest, msg: "the named provider is not a connected provider"}
		}
		return &route, nil
	}

	var routes agentsv1alpha1.ModelRouteList
	if err := caller.List(ctx, &routes,
		client.InNamespace(ns), client.MatchingLabels{labelManagedBy: managedByConnect}); err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			return nil, &createError{status: status, msg: msg}
		}
		return nil, &createError{status: http.StatusBadGateway, msg: "failed to list connected providers"}
	}
	switch len(routes.Items) {
	case 0:
		return nil, &createError{
			status: http.StatusBadRequest,
			msg:    "no connected provider — connect a provider before generating",
		}
	case 1:
		return &routes.Items[0], nil
	default:
		return nil, &createError{
			status: http.StatusBadRequest,
			msg:    "more than one connected provider — specify which provider to generate with",
		}
	}
}

// readBindingKey resolves a SecretBinding → Secret and returns the api-key, read
// with the caller's client (backend kubernetes, the connect-flow shape). A viewer
// denied the Secret read is rejected by the API server → the 403 surfaces. The
// returned key is used ONLY for the chat call; it is never logged or returned.
func readBindingKey(ctx context.Context, caller client.Client, ns, bindingName string) (string, *createError) {
	var binding agentsv1alpha1.SecretBinding
	if err := caller.Get(ctx, client.ObjectKey{Name: bindingName, Namespace: ns}, &binding); err != nil {
		return "", getErrorToCreateError(err, "secret binding")
	}
	var secret corev1.Secret
	if err := caller.Get(ctx, client.ObjectKey{Name: binding.Spec.SecretRef.Name, Namespace: ns}, &secret); err != nil {
		return "", getErrorToCreateError(err, "secret")
	}
	keyBytes, ok := secret.Data[binding.Spec.SecretRef.Key]
	if !ok || len(keyBytes) == 0 {
		return "", &createError{status: http.StatusConflict, msg: "the provider secret is missing its api-key"}
	}
	return string(keyBytes), nil
}

// getErrorToCreateError maps a caller-scoped Get failure to a typed *createError
// with an honest HTTP status (the resolve path returns *createError, not a raw
// http write): Forbidden → 403 (a viewer denied by the API server), Unauthorized
// → 401, NotFound → 404 (named), anything else → 502. It mirrors writeGetError
// (providers.go) but returns the typed error the generate resolve path threads.
func getErrorToCreateError(err error, what string) *createError {
	switch {
	case apierrors.IsForbidden(err):
		return &createError{status: http.StatusForbidden, msg: "forbidden: not allowed to read the " + what}
	case apierrors.IsUnauthorized(err):
		return &createError{status: http.StatusUnauthorized, msg: msgTokenRejected}
	case apierrors.IsNotFound(err):
		return &createError{status: http.StatusNotFound, msg: what + " not found"}
	default:
		return &createError{status: http.StatusBadGateway, msg: "failed to read the " + what}
	}
}

// routePrimaryModel returns the connect-managed route's primary model (the first
// provider entry's model). Empty when the route carries no model.
func routePrimaryModel(mr *agentsv1alpha1.ModelRoute) string {
	for _, p := range mr.Spec.Providers {
		if p.Model != "" {
			return p.Model
		}
	}
	return ""
}

// stripCodeFence removes a leading/trailing markdown code fence the model may wrap
// the YAML in (```yaml ... ``` or ``` ... ```), returning the inner content. Input
// with no fence is returned trimmed. This is a lenient tidy — a fence the model
// leaves in would otherwise make valid YAML fail expand-validation.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	// Drop the opening fence line (```… up to the first newline).
	if nl := strings.IndexByte(t, '\n'); nl >= 0 {
		t = t[nl+1:]
	} else {
		return "" // only a fence line, no content
	}
	// Drop a trailing closing fence.
	if i := strings.LastIndex(t, "```"); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}
