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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// agentDeploymentGVK is the GroupVersionKind string used for ownerReference Kind
// comparison. ownerReference.Kind is "AgentDeployment" (the Go type name as
// registered), not the plural form.
const agentDeploymentOwnerKind = "AgentDeployment"

// handleDeleteAgent serves DELETE /api/agents/{ns}/{name} — removes the named
// AgentDeployment via the CALLER-SCOPED client (ADR 0011). A viewer's DELETE
// returns the API server's real 403; the BFF never pre-empts the decision.
//
// Owned children (those with an ownerReference to this AgentDeployment) are
// garbage-collected by Kubernetes automatically and are NOT touched here. Independent
// references (MCPToolBinding, AgentScalingPolicy, MemoryBinding whose agentRef
// names this agent but who carry NO ownerReference) are intentionally LEFT IN PLACE —
// orphan pruning is deferred out of scope (ADR 0017). The caller must use
// GET /api/agents/{ns}/{name}/references to preview the impact before deleting.
//
// Responses:
//   - 204 No Content on success.
//   - 404 when the agent does not exist (honest; no silencing).
//   - 403 when the caller's RBAC denies the delete (surfaced from the API server).
//   - 401 when no bearer token is present (before any K8s call).
func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
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

	// Delete the AgentDeployment via the caller-scoped client. The API server
	// enforces the caller's RBAC on the DELETE (a viewer's delete surfaces as 403
	// from the K8s API — never pre-empted by the BFF). Owned children are
	// garbage-collected by Kubernetes; independent references are left as orphans
	// (ADR 0017, orphan pruning deferred).
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if err := caller.Delete(r.Context(), ad); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "agent not found")
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to delete the agent")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, "unauthorized: token rejected by the API server")
		default:
			s.log.Error(err, "delete AgentDeployment failed", "namespace", ns, "name", name)
			writeError(w, http.StatusInternalServerError, "failed to delete agent")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleAgentReferences serves GET /api/agents/{ns}/{name}/references — the
// delete-impact preview (ADR 0017). Using the CALLER-SCOPED client it lists the
// three reference kinds (MCPToolBinding, AgentScalingPolicy, MemoryBinding) in
// the namespace and selects those whose spec.agentRef equals the agent name.
//
// For each referencing object it classifies:
//   - ownedByAgent: true — the object carries an ownerReference whose Kind is
//     AgentDeployment and Name matches the agent being deleted. Kubernetes will
//     garbage-collect it automatically when the AgentDeployment is removed.
//   - ownedByAgent: false — the object references the agent via agentRef but
//     has no ownerReference to it. Deleting the agent will NOT cascade to this
//     object — it becomes an independent orphan (orphan pruning is deferred,
//     ADR 0017).
//
// A missing agent → 404 (checked first). Caller-scoped list errors surface
// honestly: a 403 on the list is the API server's real denial (the BFF never
// swallows it). All slices in the response are non-nil on the wire.
func (s *Server) handleAgentReferences(w http.ResponseWriter, r *http.Request) {
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

	// Verify the agent exists first — a 404 on the agent is the caller's first
	// signal that nothing references it (and the delete would be a no-op).
	var ad agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ad); err != nil {
		s.writeGetError(w, err, "agent")
		return
	}

	refs, err := collectAgentReferences(r.Context(), caller, ns, name)
	if err != nil {
		// A Forbidden is the API server's real denial — surface honestly. Any
		// other list error is a server fault (500); neither is swallowed as [].
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list agent references failed", "namespace", ns, "agent", name)
		writeError(w, http.StatusInternalServerError, "failed to list agent references")
		return
	}

	writeJSON(w, http.StatusOK, refs)
}

// AgentReferenceEntry is one object that references an AgentDeployment by
// spec.agentRef. Kind is the CRD kind name; Name is the object's own metadata
// name; OwnedByAgent is true when the object carries an ownerReference to the
// AgentDeployment (it will be garbage-collected on delete) and false when it is
// an independent reference (potential orphan on delete).
type AgentReferenceEntry struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	OwnedByAgent bool   `json:"ownedByAgent"`
}

// AgentReferencesResponse is returned by GET /api/agents/{ns}/{name}/references.
// References is the flat list of objects that reference the agent via agentRef.
// GCCount is how many will be garbage-collected by Kubernetes on delete (owned).
// OrphanCount is how many will be left in place as independent orphans. Both
// counts and References are always present (0/[] when empty, never null).
type AgentReferencesResponse struct {
	// References is the flat list of referencing objects, ordered by kind+name.
	References []AgentReferenceEntry `json:"references"`
	// GCCount is the number of references that are owned (will be GC'd on delete).
	GCCount int `json:"gcCount"`
	// OrphanCount is the number of independent references (will NOT be cascaded).
	OrphanCount int `json:"orphanCount"`
}

// collectAgentReferences lists the three reference kinds caller-scoped and
// returns the delete-impact DTO. A list failure on any kind is returned
// immediately (the caller's handler classifies it). All slices are non-nil.
func collectAgentReferences(ctx context.Context, cl client.Client, ns, name string) (AgentReferencesResponse, error) {
	var out AgentReferencesResponse
	out.References = []AgentReferenceEntry{} // non-nil [] on the wire

	inNS := []client.ListOption{client.InNamespace(ns)}

	// MCPToolBinding — references the agent via spec.agentRef.
	toolList, err := listMCPToolBindings(ctx, cl, inNS...)
	if err != nil {
		return out, err
	}
	for i := range toolList.Items {
		obj := &toolList.Items[i]
		if obj.Spec.AgentRef != name {
			continue
		}
		owned := hasOwnerRef(obj.OwnerReferences, agentDeploymentOwnerKind, name)
		out.References = append(out.References, AgentReferenceEntry{
			Kind:         "MCPToolBinding",
			Name:         obj.Name,
			OwnedByAgent: owned,
		})
	}

	// AgentScalingPolicy — references the agent via spec.agentRef.
	policyList, err := listAgentScalingPolicies(ctx, cl, inNS...)
	if err != nil {
		return out, err
	}
	for i := range policyList.Items {
		obj := &policyList.Items[i]
		if obj.Spec.AgentRef != name {
			continue
		}
		owned := hasOwnerRef(obj.OwnerReferences, agentDeploymentOwnerKind, name)
		out.References = append(out.References, AgentReferenceEntry{
			Kind:         "AgentScalingPolicy",
			Name:         obj.Name,
			OwnedByAgent: owned,
		})
	}

	// MemoryBinding — references the agent via spec.agentRef.
	memList, err := listMemoryBindings(ctx, cl, inNS...)
	if err != nil {
		return out, err
	}
	for i := range memList.Items {
		obj := &memList.Items[i]
		if obj.Spec.AgentRef != name {
			continue
		}
		owned := hasOwnerRef(obj.OwnerReferences, agentDeploymentOwnerKind, name)
		out.References = append(out.References, AgentReferenceEntry{
			Kind:         "MemoryBinding",
			Name:         obj.Name,
			OwnedByAgent: owned,
		})
	}

	// Tally the summary counts.
	for _, ref := range out.References {
		if ref.OwnedByAgent {
			out.GCCount++
		} else {
			out.OrphanCount++
		}
	}

	return out, nil
}

// hasOwnerRef reports whether the ownerReferences list contains an entry whose
// Kind equals kind and Name equals name. It does NOT check the UID or API group
// because the BFF works with same-namespace references (same-namespace AgentDeployments)
// and the Kind+Name pair is sufficient for the GC vs orphan classification.
func hasOwnerRef(refs []metav1.OwnerReference, kind, name string) bool {
	for _, r := range refs {
		if r.Kind == kind && r.Name == name {
			return true
		}
	}
	return false
}

// listAgentScalingPolicies lists AgentScalingPolicies via the reader
// (delete-impact references).
func listAgentScalingPolicies(ctx context.Context, r AgentReader, opts ...client.ListOption) (*agentsv1alpha1.AgentScalingPolicyList, error) {
	var out agentsv1alpha1.AgentScalingPolicyList
	if err := r.List(ctx, &out, opts...); err != nil {
		return nil, err
	}
	return &out, nil
}
