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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// The delete-MCP-server surface (m26.3, ADR 0031 lifecycle gap). Register builds a
// bundle — Secret + SecretBinding + ToolRegistry + NetworkPolicy (ADR 0011) — but
// nothing tore it down; the console was register-only. These handlers add a scoped
// deregister with a delete-impact preview:
//
//   - GET    /api/mcpservers/{ns}/{name}/references → the dependent MCPToolBindings
//     (those whose RegistryRef IS the server) that would go RegistryNotFound on delete.
//   - DELETE /api/mcpservers/{ns}/{name}            → tear down the whole bundle.
//
// Authorization is CALLER-SCOPED RBAC by construction (the deletes run as the caller,
// so a viewer's delete is the API server's real 403) PLUS an app-layer PERSONAL-owner
// guard (ADR 0029): a personal server may be deleted only by its owner. org/public
// servers rely on caller-scoped RBAC alone.

// MCPServerReference is one object that depends on an MCP server (an MCPToolBinding
// whose RegistryRef is the server). Deleting the server leaves it RegistryNotFound
// until it is re-pointed or removed.
type MCPServerReference struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	AgentRef string `json:"agentRef"`
}

// MCPServerReferencesResponse is the delete-impact preview for an MCP server.
type MCPServerReferencesResponse struct {
	// References is the flat list of dependent bindings (non-nil [] when empty).
	References []MCPServerReference `json:"references"`
	// BindingCount is len(References) — the number of bindings that would break.
	BindingCount int `json:"bindingCount"`
}

// DeleteMCPServerResponse reports the outcome of a deregister: the bundle kinds that
// were deleted, and the bindings left dangling (RegistryNotFound) by the delete.
type DeleteMCPServerResponse struct {
	Deleted          []string `json:"deleted"`
	OrphanedBindings []string `json:"orphanedBindings"`
}

// mcpServerPath extracts + validates {ns}/{name} from the request path.
func mcpServerPath(w http.ResponseWriter, r *http.Request) (ns, name string, ok bool) {
	ns = strings.TrimSpace(r.PathValue("ns"))
	name = strings.TrimSpace(r.PathValue("name"))
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return "", "", false
	}
	return ns, name, true
}

// mcpServerReferences lists the MCPToolBindings that depend on the server (RegistryRef
// == server) — the delete-impact set. Caller-scoped; a list error is returned to the
// handler to classify.
func mcpServerReferences(ctx context.Context, cl client.Client, ns, server string) ([]MCPServerReference, error) {
	list, err := listMCPToolBindings(ctx, cl, client.InNamespace(ns))
	if err != nil {
		return nil, err
	}
	out := []MCPServerReference{}
	for i := range list.Items {
		b := &list.Items[i]
		if b.Spec.RegistryRef == server {
			out = append(out, MCPServerReference{Kind: "MCPToolBinding", Name: b.Name, AgentRef: b.Spec.AgentRef})
		}
	}
	return out, nil
}

func mcpRefNames(refs []MCPServerReference) []string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	return names
}

// handleMCPServerReferences serves GET /api/mcpservers/{ns}/{name}/references — the
// delete-impact preview. Confirms the server is register-managed (caller-scoped),
// then returns the dependent bindings.
func (s *Server) handleMCPServerReferences(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	ns, name, ok := mcpServerPath(w, r)
	if !ok {
		return
	}

	var tr agentsv1alpha1.ToolRegistry
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &tr); err != nil {
		s.writeGetError(w, err, "MCP server")
		return
	}
	if tr.Labels[labelManagedBy] != managedByMCP {
		writeError(w, http.StatusNotFound, "no such registered MCP server")
		return
	}

	refs, err := mcpServerReferences(r.Context(), caller, ns, name)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list MCP server references failed", "namespace", ns, "name", name)
		writeError(w, http.StatusInternalServerError, "failed to list references")
		return
	}
	writeJSON(w, http.StatusOK, MCPServerReferencesResponse{References: refs, BindingCount: len(refs)})
}

// handleDeleteMCPServer serves DELETE /api/mcpservers/{ns}/{name} — the scoped
// deregister. It reads the server (confirming it is register-managed), enforces the
// personal-owner guard, then tears down the bundle caller-scoped: the ToolRegistry
// (the server itself, required) then best-effort the NetworkPolicy / SecretBinding /
// Secret (a key-less/public server has no Secret; a pending server has no egress NP).
func (s *Server) handleDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	ns, name, ok := mcpServerPath(w, r)
	if !ok {
		return
	}

	var tr agentsv1alpha1.ToolRegistry
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &tr); err != nil {
		s.writeGetError(w, err, "MCP server")
		return
	}
	if tr.Labels[labelManagedBy] != managedByMCP {
		writeError(w, http.StatusNotFound, "no such registered MCP server")
		return
	}

	// Personal-owner guard (ADR 0029): a personal server may be deleted ONLY by its
	// owner. org/public rely on caller-scoped RBAC (a viewer's delete → the API 403).
	if tr.Labels[labelMCPScope] == scopePersonal {
		username, uErr := callerUsername(r.Context(), caller)
		if uErr != nil {
			if status, msg, isRBAC := classifyReadError(uErr); isRBAC {
				writeError(w, status, msg)
				return
			}
			s.log.Error(uErr, "resolve caller identity for MCP server delete failed")
			writeError(w, http.StatusInternalServerError, "failed to resolve caller identity")
			return
		}
		if tr.Labels[labelMCPOwner] != userGrantHash(username) {
			writeError(w, http.StatusForbidden, "forbidden: only the owner can delete a personal MCP server")
			return
		}
	}

	// Delete-impact, captured BEFORE the delete. A non-RBAC list failure is not fatal
	// to the delete — the bundle still goes; report an empty impact.
	refs, refErr := mcpServerReferences(r.Context(), caller, ns, name)
	if refErr != nil {
		if status, msg, isRBAC := classifyReadError(refErr); isRBAC {
			writeError(w, status, msg)
			return
		}
		refs = []MCPServerReference{}
	}

	// The ToolRegistry is the server itself — required, with real error handling.
	deleted := []string{}
	if err := caller.Delete(r.Context(), &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, "no such registered MCP server")
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to delete the MCP server")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "delete MCP server ToolRegistry failed", "namespace", ns, "name", name)
			writeError(w, http.StatusInternalServerError, "failed to delete the MCP server")
		}
		return
	}
	deleted = append(deleted, toolRegistryKind)

	// Best-effort teardown of the rest of the bundle. NotFound is expected (optional
	// objects); any other error is logged as a leftover, not fatal — the server (the
	// registry) is already gone, and the caller who could delete the registry can
	// delete these too, so in practice they all succeed together.
	for _, o := range []struct {
		kind string
		obj  client.Object
	}{
		{networkPolicyKind, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + networkPolicyMCPSuffix}}},
		{secretBindingKind, &agentsv1alpha1.SecretBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}},
		{secretKind, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}},
	} {
		if err := caller.Delete(r.Context(), o.obj); err != nil {
			if !apierrors.IsNotFound(err) {
				s.log.Error(err, "delete MCP server bundle object left a leftover", "kind", o.kind, "namespace", ns, "name", o.obj.GetName())
			}
			continue
		}
		deleted = append(deleted, o.kind)
	}

	writeJSON(w, http.StatusOK, DeleteMCPServerResponse{Deleted: deleted, OrphanedBindings: mcpRefNames(refs)})
}
