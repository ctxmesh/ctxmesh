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

// ── POST /api/workflows/{name}/runs (m67.4, ADR 0060) ─────────────────────────────────────────────────────
//
// Creates a workflow instance run. The handler:
//   1. Resolves the named Workflow CR through the CALLER-SCOPED client (ADR 0011 — caller's RBAC governs).
//   2. Validates the request body's `input` against the workflow's inputSchema (when declared).
//   3. Snapshots the resolved WorkflowSpec to JSON and pins it on the new run (SpecSnapshot pattern).
//   4. Creates the run (IsWorkflowInstance() == true → the run-worker routes it to executeWorkflow).
//   5. Returns 202 {id, status:"queued"}.
//
// ── WorkflowNodeResolverFromClient (m67.4, ADR 0060) ──────────────────────────────────────────────────────
//
// The production WorkflowNodeResolver uses the BFF's own in-cluster client (a privileged SA, NOT a
// caller-scoped client) to read an AgentDeployment's status.URL off-request. The workflow executor
// runs inside the run-worker goroutine, which has no request context and thus no caller bearer token;
// the resolver fills the seam the executor needs (name→url) without reopening the confused-deputy
// gap (it reads ONLY AgentDeployment status, never writes, and only in the workflow's registered
// namespace — the registry trust boundary was enforced at validation time by m67.1).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/run"
)

// WorkflowNodeResolverFromClient returns a WorkflowNodeResolver backed by the given cluster client
// (the BFF's own SA — the same mechanism credentialClient uses for locked MCP grants). It reads the
// AgentDeployment's status.URL off-request; the caller MUST supply a client with read access to
// AgentDeployments in all workflow namespaces (the production BFF's own SA has this via cluster RBAC).
// The scheme must have agentsv1alpha1 registered.
func WorkflowNodeResolverFromClient(cl client.Client, _ *k8sruntime.Scheme) WorkflowNodeResolver {
	return func(ctx context.Context, namespace, agentRef string) (string, error) {
		var deploy agentsv1alpha1.AgentDeployment
		if err := cl.Get(ctx, client.ObjectKey{Name: agentRef, Namespace: namespace}, &deploy); err != nil {
			if apierrors.IsNotFound(err) {
				return "", fmt.Errorf("workflow node agent %q not found in namespace %q", agentRef, namespace)
			}
			return "", fmt.Errorf("reading workflow node agent %q: %w", agentRef, err)
		}
		url := strings.TrimSpace(deploy.Status.URL)
		if url == "" {
			return "", fmt.Errorf("workflow node agent %q has no endpoint yet (not Ready)", agentRef)
		}
		return url, nil
	}
}

// registerWorkflowRunRoutes mounts the workflow invocation endpoint on the authed mux. It follows
// the same guard pattern as registerRunRoutes: both the Invoke adapter AND callerClients must be
// present (the workflow executor re-invokes node agents via the run-worker path, and the creation
// step resolves the Workflow CRD through the caller-scoped client, ADR 0011).
func (s *Server) registerWorkflowRunRoutes(authed *http.ServeMux) {
	if s.adapters.Invoke != nil && s.callerClients != nil {
		authed.HandleFunc("POST /api/workflows/{name}/runs", s.handleCreateWorkflowRun)
		return
	}
	authed.Handle("POST /api/workflows/{name}/runs", notImplemented("workflow runs"))
}

// handleCreateWorkflowRun serves POST /api/workflows/{name}/runs (m67.4).
// Caller-scoped (ADR 0011): the Workflow CRD is resolved through the caller's own client, so the
// K8s API server enforces the caller's RBAC. Returns 202 Accepted with {id, status:"queued"}.
func (s *Server) handleCreateWorkflowRun(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	wfName := r.PathValue("name")
	if wfName == "" {
		writeError(w, http.StatusBadRequest, "workflow name is required")
		return
	}

	// Parse the optional namespace + input from the body.
	req, ok := parseWorkflowRunRequest(w, r)
	if !ok {
		return
	}

	// (1) Resolve the Workflow CR through the caller-scoped client (ADR 0011 — caller's RBAC).
	wf, ok := s.resolveWorkflowCR(w, r.Context(), caller, wfName, req.Namespace)
	if !ok {
		return
	}

	// (2) Validate the request input against the workflow's inputSchema (when declared).
	if err := validateWorkflowInput(wf.Spec.InputSchema, req.Input); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "input does not conform to the workflow's inputSchema: "+err.Error())
		return
	}

	// (3) Snapshot the resolved WorkflowSpec to JSON (the executor drives this pinned snapshot, not
	// the live CR — a live CR edit after instance creation must not change the running graph).
	snapshot, err := json.Marshal(wf.Spec)
	if err != nil {
		s.log.Error(err, "workflow run: could not marshal spec snapshot", "workflow", wfName)
		writeError(w, http.StatusInternalServerError, "failed to snapshot the workflow spec")
		return
	}

	// Mint the run ID.
	runID, err := randToken(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to mint a run id")
		return
	}
	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// (4) Build the workflow instance run with the pinned snapshot + workflowRef.
	// We set WorkflowRef (the CRD name) and SpecSnapshot (the pinned graph the executor drives),
	// plus the execution record (CallerUsername + Boundary) exactly as handleCreateRun does.
	rn := run.New(runID, ns, wfName, req.Input, req.ConversationID, time.Now())
	rn.WorkflowRef = wfName            // the Workflow CR name this run instantiates
	rn.SpecSnapshot = string(snapshot) // pinned at instance-create time — drives the executor
	if username, uErr := callerUsername(r.Context(), caller); uErr == nil {
		rn.CallerUsername = username
		rn.Boundary = agentBoundary(r.Context(), caller, ns, wfName)
	}

	if err := s.runStore.Create(rn); err != nil {
		s.log.Error(err, "create workflow run failed", "workflow", wfName)
		writeError(w, http.StatusInternalServerError, "failed to create the workflow run")
		return
	}

	// (5) Worker-dispatch mode: leave `queued` for the pool. Dev/single-pod: execute in-process.
	// NOTE: we pass a detached background context (not r.Context()) so the workflow execution
	// survives the request returning 202 (the request ctx cancels when the handler returns).
	if !s.runWorkerDispatch {
		execCtx := contextWithConversationID(context.Background(), req.ConversationID)
		go s.executeWorkflow(execCtx, runID)
	}

	writeJSON(w, http.StatusAccepted, CreateRunResponse{ID: runID, Status: string(run.StatusQueued)})
}

// resolveWorkflowCR reads the named Workflow CR through the CALLER-SCOPED client and returns a copy
// (the caller's RBAC — not the BFF — gates this read, ADR 0011). It writes the right error
// response and returns ok=false on any failure: not-found → 404, RBAC → 403, other → 500.
func (s *Server) resolveWorkflowCR(w http.ResponseWriter, ctx context.Context, caller client.Client, name, namespace string) (*agentsv1beta1.Workflow, bool) {
	ns := namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}
	var wf agentsv1beta1.Workflow
	if err := caller.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, &wf); err != nil {
		switch {
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to read the requested workflow")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, "unauthorized: token rejected by the API server")
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "workflow not found")
		default:
			s.log.Error(err, "resolve workflow failed", "workflow", name, "namespace", ns)
			writeError(w, http.StatusInternalServerError, "failed to resolve workflow")
		}
		return nil, false
	}
	return &wf, true
}

// WorkflowRunRequest is the POST /api/workflows/{name}/runs body.
type WorkflowRunRequest struct {
	// Input is the workflow's typed input, validated against inputSchema when declared.
	// Optional when the workflow declares no inputSchema.
	Input json.RawMessage `json:"input,omitempty"`
	// Namespace scopes the Workflow CR lookup; empty → the default namespace.
	Namespace string `json:"namespace,omitempty"`
	// ConversationID threads runs together (same as /api/runs). Optional.
	ConversationID string `json:"conversationId,omitempty"`
}

// parseWorkflowRunRequest reads the bounded POST body. Returns (req, true) on success, writes an
// error and returns (_, false) on failure. An empty/absent body is valid (no input required).
func parseWorkflowRunRequest(w http.ResponseWriter, r *http.Request) (WorkflowRunRequest, bool) {
	if r.Body == nil || r.ContentLength == 0 {
		return WorkflowRunRequest{}, true
	}
	var req WorkflowRunRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxInvokeRequestBytes))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %s", err.Error()))
		return WorkflowRunRequest{}, false
	}
	return req, true
}

// validateWorkflowInput validates the request input against the workflow's declared inputSchema.
// Returns nil when: the workflow has no inputSchema, OR the input is nil/empty (treats as null), OR
// the input conforms to the schema. Fail-closed: an uncompilable schema is an error — the operator
// must fix it; we never silently wave requests through a broken governance contract (ADR 0058 §5).
func validateWorkflowInput(inputSchema *k8sruntime.RawExtension, input json.RawMessage) error {
	if inputSchema == nil || len(inputSchema.Raw) == 0 {
		return nil // no schema declared → no validation (dynamic / untyped workflow input)
	}
	sch, err := jsonschema.CompileString("inputSchema.json", string(inputSchema.Raw))
	if err != nil {
		return fmt.Errorf("workflow inputSchema is not a valid JSON Schema: %w", err)
	}
	// An absent/empty input is treated as JSON null for validation purposes, giving honest feedback
	// ("null does not match required schema") when the schema requires an object, rather than
	// silently skipping validation.
	data := input
	if len(data) == 0 {
		data = json.RawMessage(`null`)
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("input is not valid JSON: %w", err)
	}
	return sch.Validate(v)
}
