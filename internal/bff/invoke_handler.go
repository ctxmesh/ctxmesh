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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// maxInvokeRequestBytes bounds the Playground input body. A run's input is a
// small JSON prompt; 1 MiB caps what a single request can force us to buffer.
const maxInvokeRequestBytes = 1 << 20 // 1 MiB

// InvokeRequest is the POST /api/invoke body: which deployed agent to run and the
// raw input to POST to its /invoke. Namespace empty → the default namespace (the
// same convention as the create path).
type InvokeRequest struct {
	// Agent is the AgentDeployment name to run (required).
	Agent string `json:"agent"`
	// Namespace scopes the agent lookup; empty → the default namespace.
	Namespace string `json:"namespace"`
	// Input is the raw request body forwarded verbatim to the agent's /invoke.
	Input json.RawMessage `json:"input"`
}

// InvokeResponse is returned by POST /api/invoke: the run's traceId (the hand-off
// the SPA feeds to /api/traces/{id} for the trace-tree + embedded deep-view) and
// the agent's raw response as a string. traceId is always present on a run that
// reached the agent — even a failed (non-2xx) run was traced.
type InvokeResponse struct {
	TraceID  string `json:"traceId"`
	Response string `json:"response"`
}

// handleInvoke serves POST /api/invoke — the Playground run. It is CALLER-SCOPED
// (ADR 0011): the AgentDeployment is looked up through the caller's own client,
// so the run acts as the caller and the K8s API server enforces the caller's RBAC
// — a viewer with no read/invoke rights surfaces a 403, never an SA fallback. The
// address (status.url) resolved here is handed to the pure-HTTP InvokeAdapter,
// which opens the trace and POSTs /invoke; the adapter never touches Kubernetes.
//
// Errors surface honestly: missing token → 401 (before any K8s call), agent not
// found → 404, RBAC denial on the lookup → 403, agent not ready (no url yet) →
// 409, upstream /invoke failure → 502. A failed run is never reported as 200.
func (s *Server) handleInvoke(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxInvokeRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req InvokeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Agent) == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return
	}

	endpoint, ok := s.resolveAgentEndpoint(w, r, caller, req.Agent, req.Namespace)
	if !ok {
		return
	}

	// The input is forwarded verbatim; an empty input becomes an empty JSON body
	// the agent can accept or reject on its own.
	input := []byte(req.Input)
	resp, traceID, err := s.adapters.Invoke.Invoke(r.Context(), endpoint, input)
	if err != nil {
		var ie *invokeError
		if errors.As(err, &ie) {
			// The agent answered non-2xx: surface its status as a 502 (an upstream
			// failure) with the real reason, and still hand back the traceId so the
			// user can inspect the failed run's trace.
			writeJSON(w, http.StatusBadGateway, InvokeErrorResponse{
				Error:   ie.Error(),
				TraceID: traceID,
			})
			return
		}
		s.log.Error(err, "playground invoke failed", "agent", req.Agent)
		writeError(w, http.StatusBadGateway, "failed to invoke agent")
		return
	}

	writeJSON(w, http.StatusOK, InvokeResponse{
		TraceID:  traceID,
		Response: string(resp),
	})
}

// InvokeErrorResponse is returned when the agent answered non-2xx: the honest
// error plus the traceId of the (failed but traced) run, so the Playground can
// still open its deep-view.
type InvokeErrorResponse struct {
	Error   string `json:"error"`
	TraceID string `json:"traceId"`
}

// resolveAgentEndpoint reads the named AgentDeployment through the CALLER-SCOPED
// client and returns its status.url (the base for /invoke). It writes the right
// error response and returns ok=false on any failure: a K8s Forbidden → 403 (the
// caller may not read the agent), not-found → 404, and an agent with no url yet
// (not Ready / no assigned endpoint) → 409. The caller's RBAC — not the BFF — is
// what gates this read (ADR 0011).
func (s *Server) resolveAgentEndpoint(w http.ResponseWriter, r *http.Request, caller client.Client, name, namespace string) (string, bool) {
	ns := namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	var deploy agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, &deploy); err != nil {
		switch {
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to read the requested agent")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, "unauthorized: token rejected by the API server")
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "agent not found")
		default:
			s.log.Error(err, "resolve agent for invoke failed", "agent", name, "namespace", ns)
			writeError(w, http.StatusInternalServerError, "failed to resolve agent")
		}
		return "", false
	}

	url := strings.TrimSpace(deploy.Status.URL)
	if url == "" {
		// The agent exists but has no assigned endpoint yet (not Ready). A run
		// cannot be dispatched — surface it as a conflict, not a fake success.
		writeError(w, http.StatusConflict, "agent is not ready (no endpoint assigned yet)")
		return "", false
	}
	return url, true
}
