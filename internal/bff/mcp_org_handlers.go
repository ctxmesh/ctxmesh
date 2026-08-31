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
	"io"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

// SetOrgCredentialRequest is the POST /api/mcp/org-credential body: an admin promoting a
// registered server to ORG scope + setting its shared credential (ADR 0029 §1/§7). Every
// invoker then uses this one credential, no per-user consent. The credential is secret
// material — it lands ONLY in the org Secret, never a DTO/log/annotation/label.
type SetOrgCredentialRequest struct {
	// Server is the registered MCP server (ToolRegistry) to promote. Required.
	Server string `json:"server"`
	// Namespace scopes the server; empty → the default namespace.
	Namespace string `json:"namespace"`
	// Credential is the shared bearer key all invokers will use. Required.
	Credential string `json:"credential"`
}

// SetOrgCredentialResponse reports the promote outcome — NO credential material.
type SetOrgCredentialResponse struct {
	Status    string `json:"status"`
	Server    string `json:"server"`
	Namespace string `json:"namespace"`
}

// handleSetOrgCredential serves POST /api/mcp/org-credential (ADR 0029 §7): an admin promotes
// a server to org scope + sets its shared credential. The admin gate is RBAC-by-construction:
// the ToolRegistry scope change is written CALLER-SCOPED, so only a principal allowed to
// update the server's ToolRegistry (an operator/admin) can promote it — a viewer gets 403.
// The shared credential is then written server-side to the org Secret; it never surfaces.
func (s *Server) handleSetOrgCredential(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req SetOrgCredentialRequest
	if jErr := json.Unmarshal(raw, &req); jErr != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	server := strings.TrimSpace(req.Server)
	credential := strings.TrimSpace(req.Credential)
	if server == "" || credential == "" {
		writeError(w, http.StatusBadRequest, "server and credential are required")
		return
	}
	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// 1. Promote the server to org scope (CALLER-SCOPED — the admin gate). A viewer who
	// cannot update the ToolRegistry gets a 403 here, before any credential is written.
	tr, gErr := s.mcpGetToolRegistry(r.Context(), caller, ns, server)
	if gErr != nil {
		writeMCPReadError(w, gErr, "MCP server")
		return
	}
	if tr.Labels == nil {
		tr.Labels = map[string]string{}
	}
	tr.Labels[labelMCPScope] = scopeOrg
	delete(tr.Labels, labelMCPOwner) // org has no single owner
	// ADR 0067 §2: stamp the two new axes alongside the legacy label (rollback aid).
	// Set credentialSource=shared (this is the shared-credential path).
	// Visibility: set to team ONLY if the current visibility is private (i.e. max(current, team)).
	// An org/public server already at a wider visibility MUST NOT be downgraded to team
	// by setting a shared credential (m73.5 refinement). The credential axis and the
	// visibility axis are orthogonal — promoting a credential never narrows who can see it.
	tr.Labels[labelMCPCredentialSource] = credSourceShared
	currentVis := tr.Labels[labelMCPVisibility]
	if currentVis == "" || currentVis == visibilityPrivate {
		// Default / private: elevate to team (the minimum shared-credential visibility).
		tr.Labels[labelMCPVisibility] = visibilityTeam
	}
	// If currentVis is already team, org, or public: leave it unchanged (never downgrade).

	// Retired (RETIRE_TR, ADR 0044): the org-promote authz — the SSAR VerbUpdate IS
	// the admin gate (exact RBAC parity with the CRD update this replaces). It runs
	// BEFORE the store write AND before the privileged credential Secret write below,
	// so a denied caller never promotes and no org credential is delivered.
	// TOCTOU note: the SSAR + store Upsert is not one atomic API call like the CRD
	// Update was — a bounded window where the caller's permission could change
	// between the check and the write. Accepted: org-promote RBAC is operator-stable
	// and the window is sub-millisecond; the alternative (a store-side authz) is out
	// of scope. (Documented in ADR 0044.)
	if aErr := s.authorizeStore(r.Context(), caller, authz.VerbUpdate, resourceToolRegistries, ns, server); aErr != nil {
		s.writeAuthzError(w, aErr, "promote the MCP server to org scope")
		return
	}
	rec := crdToolRegistryToStore(tr)
	if vErr := toolregistry.Validate(rec); vErr != nil {
		s.writeValidationError(w, vErr)
		return
	}
	if _, uErr := s.toolRegistryStore.Upsert(r.Context(), rec); uErr != nil {
		s.log.Error(uErr, "org-promote: store update failed", "namespace", ns, "server", server)
		writeError(w, http.StatusInternalServerError, "failed to promote the MCP server to org scope")
		return
	}

	// 2. Write the shared org credential server-side (the credential namespace when locked,
	// else the request namespace). Uses the privileged grant client so the secret lands in
	// the locked trust domain; the promote above is the authorization gate.
	readNS := ns
	if s.lockedCredentials() {
		readNS = s.credentialNamespace
	}
	orgSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credresolve.OrgSecretName(server),
			Namespace: readNS,
			Labels:    credresolve.OrgSecretLabels(server),
		},
		Data: credresolve.OrgSecretData(credential),
	}
	if wErr := s.upsertGrantSecret(r.Context(), s.grantClient(caller), orgSecret); wErr != nil {
		s.log.Error(wErr, "set org credential failed", "server", server, "namespace", ns)
		writeError(w, http.StatusInternalServerError, "failed to store the org credential")
		return
	}

	writeJSON(w, http.StatusOK, SetOrgCredentialResponse{Status: "org-credential-set", Server: server, Namespace: ns})
}

// writeMCPReadError maps a caller-scoped K8s read/write error to an honest HTTP status.
func writeMCPReadError(w http.ResponseWriter, err error, what string) {
	switch {
	case apierrors.IsForbidden(err):
		writeError(w, http.StatusForbidden, "forbidden: not allowed to modify the requested "+what)
	case apierrors.IsUnauthorized(err):
		writeError(w, http.StatusUnauthorized, "unauthorized: token rejected by the API server")
	case apierrors.IsNotFound(err):
		writeError(w, http.StatusNotFound, what+" not found")
	default:
		writeError(w, http.StatusInternalServerError, "failed to resolve the "+what)
	}
}
