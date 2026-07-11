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
	"errors"
	"io"
	"net/http"

	"github.com/ctxmesh/agent-engine/internal/expand"
)

// maxAgentYAMLBytes bounds the request body the config-builder submits. An
// agent.yaml is a small, simplified spec; a megabyte is already far more than any
// real form emits and caps the memory a single request can force us to buffer.
const maxAgentYAMLBytes = 1 << 20 // 1 MiB

// handleExpand serves POST /api/expand — the config-builder preview. The request
// body is the simplified agent.yaml the form produced; the response is the
// expanded CRD manifest(s) as plain YAML the SPA renders as a read-only preview.
// The mapping is the SAME internal/expand core the CLI uses (via the adapter), so
// the preview is byte-identical to `agent-engine expand`. A bad agent.yaml is a
// client error (400 with the validation message) — never a 500 and never a
// silent success.
func (s *Server) handleExpand(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAgentYAMLBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "empty agent.yaml body")
		return
	}

	manifests, err := s.adapters.Expand.Expand(r.Context(), body)
	if err != nil {
		// A parse/validation failure is the caller's input problem → 400 with the
		// safe message. Anything else that is not an expand.Error is unexpected.
		var xe *expand.Error
		if errors.As(err, &xe) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.log.Error(err, "expand failed")
		writeError(w, http.StatusInternalServerError, "failed to expand agent.yaml")
		return
	}

	// The preview is YAML the SPA shows verbatim. Serve it as text/yaml so the
	// browser does not try to parse it as JSON.
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(manifests)
}

// CreateAgentRequest is the POST /api/agents body: the simplified agent.yaml the
// form built plus an optional target namespace. The BFF expands and applies it —
// the browser never sends raw CRDs, only the same simplified schema the CLI and
// the preview consume, so apply cannot diverge from what the user previewed.
type CreateAgentRequest struct {
	// AgentYAML is the simplified agent.yaml (the exact body /api/expand previews).
	AgentYAML string `json:"agentYAML"`
	// Namespace targets the create; empty → the default namespace.
	Namespace string `json:"namespace"`
}

// CreateAgentResponse is returned by POST /api/agents on success: the flat
// identity of every CRD object created (EvalSuite/PromptVersion first, then the
// AgentDeployment). Created is non-nil ([] not null).
type CreateAgentResponse struct {
	Created []createdObject `json:"created"`
}

// handleCreateAgent serves POST /api/agents — the config-builder apply path. It
// expands the submitted agent.yaml through the SAME mapping as the preview,
// decodes the result into typed CRD objects, and creates each through the
// CALLER-SCOPED client (ADR 0011): the K8s API server makes the persona decision
// on the CALLER'S identity, so a viewer's create surfaces as 403 — the BFF's own
// SA is never used here. It returns the created objects' identities. Errors
// surface cleanly: missing token → 401 (before any K8s call), bad agent.yaml →
// 400, already-exists → 409, RBAC denial → 403, other API failure → 502. A
// failed apply is never reported as success.
func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	body, err := readLimitedBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req CreateAgentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.AgentYAML == "" {
		writeError(w, http.StatusBadRequest, "agentYAML is required")
		return
	}

	created, err := createAgentFromYAML(r.Context(), caller, s.scheme, []byte(req.AgentYAML), req.Namespace)
	if err != nil {
		var ce *createError
		if errors.As(err, &ce) {
			// Log server-side faults (5xx) with the underlying detail; client faults
			// (4xx) are the caller's input/permission and need no error log.
			if ce.status >= 500 {
				s.log.Error(err, "create agent failed")
			}
			writeError(w, ce.status, ce.msg)
			return
		}
		s.log.Error(err, "create agent failed (unclassified)")
		writeError(w, http.StatusInternalServerError, "failed to create agent")
		return
	}

	if created == nil {
		created = []createdObject{}
	}
	writeJSON(w, http.StatusCreated, CreateAgentResponse{Created: created})
}
