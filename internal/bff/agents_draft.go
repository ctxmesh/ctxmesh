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
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// stageLabel is the well-known label key that marks an AgentDeployment's
// lifecycle stage (ADR 0065 D1 — draft early, iterate live, publish when done).
// The label rides the AgentDeployment from birth; clearing it is the "publish"
// action. Only the value stageDraft is defined today; future values (e.g.
// "deprecated") extend the space without changing the key.
const stageLabel = "agents.ctxmesh.ai/stage"

// stageDraft is the label VALUE that marks an AgentDeployment as a draft. A
// draft is a real deployed AgentDeployment the user can invoke for testing
// before publishing to team/registry consumption. Draft agents are excluded from
// the default list (GET /api/agents) and from team-generate; they are included
// only when the caller explicitly opts in via ?includeDrafts=true.
const (
	stageDraft = "draft"
	// statusPublished is the action-outcome string returned by POST /api/agents/{ns}/{name}/publish
	// and POST /api/mcp/servers/{ns}/{name}/publish on success. Named once so agents_draft.go and
	// mcp_publish.go agree on the wire value.
	statusPublished = "published"
)

// AgentPublishResponse is returned by POST /api/agents/{ns}/{name}/publish on
// success: the identity of the published agent and the action status. The status
// is "published" whether the agent was a draft or already published (idempotent).
type AgentPublishResponse struct {
	// Namespace / Name identify the agent acted on.
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Status is the action outcome — always "published" on success (the stage
	// label is absent from the object, so it is live for team/registry consumption).
	Status string `json:"status"`
}

// AgentPatcher is the narrow caller-scoped seam the publish action needs: read the
// live AgentDeployment and patch it to remove the draft label. Satisfied by the
// same controller-runtime client.Client the edit/delete paths use, so the real BFF
// passes the caller-scoped client (ADR 0011) and the K8s API server enforces RBAC
// on both the Get and the Patch.
type AgentPatcher interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
	Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error
}

// handlePublishAgent serves POST /api/agents/{ns}/{name}/publish — the publish
// action for a draft agent (ADR 0065 D1). It removes the
// agents.ctxmesh.ai/stage=draft label from the AgentDeployment so it becomes
// visible to the default agents list and team/registry consumption.
//
// It is CALLER-SCOPED (ADR 0011): the Get + Patch run through the caller's own
// client, so the K8s API server enforces the caller's RBAC. A viewer whose RBAC
// forbids the Patch surfaces as 403; a missing agent is 404. Idempotent: publishing
// an already-published agent returns 200 with status "published" — no-op, no error.
func (s *Server) handlePublishAgent(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	ns := strings.TrimSpace(r.PathValue("ns"))
	name := strings.TrimSpace(r.PathValue("name"))
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}

	if err := publishAgent(r.Context(), caller, ns, name); err != nil {
		var ce *createError
		if isCreateError(err, &ce) {
			if ce.status >= 500 {
				s.log.Error(err, "publish agent failed")
			}
			writeError(w, ce.status, ce.msg)
			return
		}
		s.log.Error(err, "publish agent failed (unclassified)")
		writeError(w, http.StatusInternalServerError, "failed to publish agent")
		return
	}

	writeJSON(w, http.StatusOK, AgentPublishResponse{
		Namespace: ns,
		Name:      name,
		Status:    statusPublished,
	})
}

// publishAgent removes the agents.ctxmesh.ai/stage label from the named
// AgentDeployment so it transitions from draft to published. It is idempotent:
// an already-published (no stage label) agent is a no-op — the label is simply
// absent and we return nil without a Patch call. A not-found agent is a 404; an
// RBAC denial is a 403; any other API failure is a 502.
//
// The mutation is a MergeFrom patch (the same pattern the rollback handler uses for
// annotation-only mutations): the live object is deep-copied as the base, the stage
// label is deleted from the mutated copy, and the diff is applied — so only the
// label key changes and all other labels are preserved intact.
func publishAgent(ctx context.Context, patcher AgentPatcher, ns, name string) error {
	var live agentsv1alpha1.AgentDeployment
	if err := patcher.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return &createError{status: 404, msg: "agent not found"}
		}
		if apierrors.IsForbidden(err) {
			return &createError{status: 403, msg: "forbidden: not allowed to get agent"}
		}
		return &createError{status: 502, msg: "failed to get agent"}
	}

	// Idempotent: the stage label is already absent → already published, no-op.
	if live.Labels[stageLabel] != stageDraft {
		return nil
	}

	// Build a MergeFrom patch: deep-copy the live object as the base, then delete
	// the stage label from the mutated copy. The client computes the JSON Merge
	// Patch diff (only the labels field changes) and applies it via the caller's
	// scoped client. Only the stage label is removed; all other labels stay intact.
	base := live.DeepCopy()
	delete(live.Labels, stageLabel)

	if pErr := patcher.Patch(ctx, &live, client.MergeFrom(base)); pErr != nil {
		if apierrors.IsNotFound(pErr) {
			return &createError{status: 404, msg: "agent not found"}
		}
		if apierrors.IsForbidden(pErr) {
			return &createError{status: 403, msg: "forbidden: not allowed to update agent"}
		}
		if apierrors.IsConflict(pErr) {
			return &createError{status: 409, msg: "conflict: agent was modified concurrently; retry"}
		}
		return &createError{status: 502, msg: "failed to publish agent"}
	}
	return nil
}

// createAgentOptions carries per-create options for createAgentFromYAML that
// are outside the core expand/create pipeline (e.g. the draft stage flag). The
// zero value is a normal (immediately published) create — byte-for-byte unchanged
// for existing callers. Fields added here must never change the default behaviour
// of a call that doesn't set them.
type createAgentOptions struct {
	// draft, when true, stamps the agents.ctxmesh.ai/stage=draft label on the
	// primary AgentDeployment before it is created, so the agent is born as a draft.
	draft bool
}

// stampDraftLabel sets the agents.ctxmesh.ai/stage=draft label on the primary
// AgentDeployment among the decoded objects, so a create-as-draft request marks
// the object at birth. It touches ONLY the AgentDeployment (not generated
// bindings/versions). It is a no-op when objs contains no AgentDeployment.
func stampDraftLabel(objs []decodedObject) {
	for _, o := range objs {
		if o.kind != agentDeploymentKind {
			continue
		}
		lbls := o.obj.GetLabels()
		if lbls == nil {
			lbls = map[string]string{}
		}
		lbls[stageLabel] = stageDraft
		o.obj.SetLabels(lbls)
	}
}

// isDraftAgent reports whether an AgentDeployment carries the stage=draft label.
func isDraftAgent(ad *agentsv1alpha1.AgentDeployment) bool {
	return ad.Labels[stageLabel] == stageDraft
}

// isCreateError is a helper that type-asserts err to *createError and stores it
// in ce when it succeeds, returning true. Used to keep the error-handling chain
// concise without re-assigning via errors.As (which requires a pointer to pointer).
func isCreateError(err error, ce **createError) bool {
	if err == nil {
		return false
	}
	c, ok := err.(*createError) //nolint:errorlint // *createError is never wrapped; it is always the concrete type
	if ok {
		*ce = c
	}
	return ok
}
