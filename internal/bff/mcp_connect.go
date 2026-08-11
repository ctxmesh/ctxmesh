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

// Discover-then-materialize (m73.6, ADR 0067 §3). "Connect" on a catalog-discovered
// MCP server imports its DEFINITION into the CALLER's own namespace so they can use
// it with their OWN credential. Cross-namespace USE is impossible by construction
// (an MCPToolBinding's registryRef is same-namespace, and egress + credential
// resolution are namespace-keyed), so USE = materialize a local copy, after which
// the existing per-namespace machinery runs unchanged.
//
// THE CREDENTIAL-SAFETY CRUX: the publisher's token NEVER crosses the namespace
// boundary. The copy is created with NO credential material — apiKey:"" and
// oauthSecretData:nil — so createMCPObjects writes no Secret for it. Only the
// NON-secret definition (url, tools, authType, OAuth CLIENT config) is copied. The
// consumer OBO-connects their own credential later, exactly as a fresh OAuth
// register does.
//
// Authz model (mirrors the m73.4 catalog predicate — ADR 0067 §6 amending ADR 0011):
//   - The origin row is read from the store on the BFF's own cpDB connection (the
//     amended-ADR-0011 model — the caller has no RBAC in the origin namespace).
//   - A SECURITY GATE re-verifies the origin is DISCOVERABLE by this caller before
//     materializing: we do NOT trust that the client only calls Connect for catalog
//     entries. A caller must not materialize an arbitrary private server by guessing
//     ns+name. Undiscoverable → 404 (never 403 — do not confirm the server exists).
//   - The materialize itself uses the CALLER's own client (caller-scoped, SSAR-gated
//     by createMCPObjects), so the K8s API server / store SSAR enforces the caller's
//     RBAC in their OWN namespace. No BFF-SA grant is needed.

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// MCPConnectRequest is the POST /api/mcp/connect body. It names the origin server to
// import and, optionally, a local name for the copy (default = originName). The copy
// always lands in the CALLER's own namespace — there is no target-namespace field:
// materialize is inherently into your own namespace (that is the whole point).
type MCPConnectRequest struct {
	// OriginNamespace is the namespace the discovered server is registered in. Required.
	OriginNamespace string `json:"originNamespace"`
	// OriginName is the discovered server's (ToolRegistry) name in its origin namespace. Required.
	OriginName string `json:"originName"`
	// Name is the OPTIONAL local name for the materialized copy in the caller's namespace.
	// Empty → the origin name is reused (sanitized to an RFC-1123 object name).
	Name string `json:"name"`
	// Namespace is the CALLER's own namespace — where the local copy is materialized.
	// Empty → the default namespace. This is the caller's namespace, NOT a way to
	// write into someone else's: createMCPObjects runs caller-scoped, so a write into
	// a namespace the caller has no RBAC in is the API server's / store's real 403.
	Namespace string `json:"namespace"`
}

// MCPConnectResponse reports the materialize outcome. Status is "connected" for a
// fresh materialize or "already-connected" for an idempotent re-connect. The server
// summary carries only NON-secret material (the list DTO shape).
type MCPConnectResponse struct {
	Status string           `json:"status"`
	Server MCPServerSummary `json:"server"`
}

// handleMCPConnect serves POST /api/mcp/connect (m73.6, ADR 0067 §3). See the file
// header for the model. It:
//  1. reads + validates the request;
//  2. reads the origin's NON-secret definition from the store and re-checks the
//     caller may DISCOVER it (else 404) — the security gate;
//  3. materializes a FROZEN local copy into the caller's namespace with NO credential
//     (apiKey:"" + oauthSecretData:nil — the crux) plus provenance labels;
//  4. idempotent re-connect: an existing local copy of that name → 200 "already-connected".
func (s *Server) handleMCPConnect(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.toolRegistryStore == nil {
		writeError(w, http.StatusNotImplemented, msgToolRegistryStoreRequired)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req MCPConnectRequest
	if jErr := json.Unmarshal(raw, &req); jErr != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}

	originNS := strings.TrimSpace(req.OriginNamespace)
	originName := strings.TrimSpace(req.OriginName)
	if originNS == "" || originName == "" {
		writeError(w, http.StatusBadRequest, "originNamespace and originName are required")
		return
	}

	callerNS := strings.TrimSpace(req.Namespace)
	if callerNS == "" {
		callerNS = defaultCreateNamespace
	}
	localName := mcpServerName(req.Name)
	if strings.TrimSpace(req.Name) == "" {
		localName = mcpServerName(originName)
	}

	res, mErr := s.materializePublishedMCP(r, caller, originNS, originName, localName, callerNS)
	if mErr != nil {
		writeError(w, mErr.status, mErr.msg)
		return
	}
	writeJSON(w, http.StatusOK, MCPConnectResponse{Status: res.status, Server: res.summary})
}

// mcpMaterializeResult is the outcome of materializePublishedMCP: the materialized
// (or idempotently already-present) local copy's status + non-secret summary.
type mcpMaterializeResult struct {
	// status is "connected" for a fresh materialize or "already-connected" for an
	// idempotent re-connect (an existing local copy of the same name).
	status string
	// summary is the list DTO of the local copy — NO secret material.
	summary MCPServerSummary
}

// materializePublishedMCP is the shared discover-then-materialize core (m73.6, ADR 0067
// §3) reused by BOTH POST /api/mcp/connect (handleMCPConnect) and the fork ref-closure's
// compose-connect (m74.4, ADR 0068 §5 — the flywheel compounding). It reads the origin's
// NON-secret definition, re-checks discoverability (the security gate — never trust that
// the caller only connects catalog entries), and materializes a FROZEN local copy into
// the caller's namespace with NO credential (the crux — apiKey "" + oauthSecretData nil).
//
// It returns a typed *createError already carrying the right HTTP status: an
// undiscoverable / absent / erroring origin → 404 (never 403 — do not confirm the server
// exists); an existing local copy of the same name → status "already-connected" (a benign
// idempotent re-connect, NOT an error). The materialize itself runs caller-scoped, so the
// caller's own-namespace RBAC is enforced by createMCPObjects (no BFF-SA grant).
func (s *Server) materializePublishedMCP(r *http.Request, caller client.Client, originNS, originName, localName, callerNS string) (*mcpMaterializeResult, *createError) {
	ctx := r.Context()

	// 1. Read the origin's NON-secret definition from the store (BFF cpDB connection,
	// amended-ADR-0011 — the caller has no RBAC in originNS). A miss is folded into the
	// same 404 as an undiscoverable server below (do not distinguish "absent" from
	// "hidden" — both must be indistinguishable to the caller).
	originRec, gErr := s.toolRegistryStore.Get(ctx, originNS, originName)
	if gErr != nil {
		// ErrNotFound and any other store error alike → the undiscoverable 404. A
		// transient store error must not confirm existence; fail-closed to 404.
		return nil, &createError{status: http.StatusNotFound, msg: "no such discoverable MCP server"}
	}
	origin := storeToolRegistryToCRD(originRec)

	// 2. SECURITY GATE — re-verify the origin is DISCOVERABLE by this caller. We do NOT
	// trust the client to only Connect catalog entries. This mirrors the m73.4 catalog
	// predicate exactly: allow ONLY when the origin is public, OR (org AND originNS is in
	// the caller's tenant), OR originNS == the caller's own namespace. Otherwise 404 —
	// never 403 (a 403 would confirm the server exists to a caller who may not see it).
	if !s.mcpConnectDiscoverable(r, origin, originNS, callerNS) {
		return nil, &createError{status: http.StatusNotFound, msg: "no such discoverable MCP server"}
	}

	// 3. Read the origin's NON-secret definition: url, tools, authType, and the OAuth
	// CLIENT config (endpoints + public clientId + scope + redirect). We NEVER read the
	// origin's Secret / token / apiKey — none of that is touched here.
	url := origin.Annotations[annMCPURL]
	authType := origin.Annotations[annMCPAuthType]
	tools := discoveredToolsFromRegistry(origin)
	oauthConfig := mcpOAuthConfigFromAnnotations(origin.Annotations)

	// credentialSource derives from the auth scheme: an OAuth server → byo-oauth (the
	// consumer supplies their own grant later); anything else (none / no-auth) → none.
	// A materialized copy is ALWAYS private (the consumer's own copy — they may publish
	// it later) and NEVER shared: a shared credential is org-level, and the copy carries
	// no credential at all.
	credentialSource := credSourceNone
	if authType == oauthAuthType {
		credentialSource = credSourceByoOAuth
	}

	// The caller's owner hash (fail-closed: an identity lookup failure leaves owner ""
	// so the copy is not attributed to a wrong owner). The copy is private; the owner
	// gate keys on this hash.
	callerOwner := ""
	if username, uErr := callerUsername(ctx, caller); uErr == nil {
		callerOwner = userGrantHash(username)
	}

	spec := mcpCreateSpec{
		name:      localName,
		namespace: callerNS,
		url:       url,
		// THE CRUX: no credential material crosses the boundary. apiKey stays "" and
		// oauthSecretData stays nil, so createMCPObjects writes NO Secret for the copy.
		apiKey:          "",
		oauthSecretData: nil,
		tools:           tools,
		// status approved → self-serve (immediately bindable + egress opened for the
		// consumer's own namespace), matching a fresh self-serve register.
		status:           agentsv1alpha1.ApprovalApproved,
		authType:         authType,
		visibility:       visibilityPrivate,
		credentialSource: credentialSource,
		owner:            callerOwner,
		oauthConfig:      oauthConfig,
		originNamespace:  originNS,
		originName:       originName,
	}

	created, cErr := s.createMCPObjects(ctx, caller, spec)
	if cErr != nil {
		// 4. Idempotent re-connect: a local copy of that name already exists → the store
		// Create 409s (toolRegistryStoreWriteError maps ErrConflict → StatusConflict). We
		// treat that as a benign already-connected outcome: the existing summary, so a
		// double-click on "Connect" is not a scary error. Any other createError is a real
		// failure and surfaces honestly.
		if cErr.status == http.StatusConflict {
			if existing, xErr := s.toolRegistryStore.Get(ctx, callerNS, localName); xErr == nil {
				return &mcpMaterializeResult{
					status:  "already-connected",
					summary: mcpServerSummaryFromRegistry(storeToolRegistryToCRD(existing)),
				}, nil
			}
		}
		return nil, cErr
	}
	_ = created // the flat created-object identities are not surfaced by connect.

	// 5. Return the materialized server summary (list DTO shape — NO secret material).
	// Re-read the just-created copy from the store so the summary reflects exactly what
	// was persisted (labels/annotations), consistent with the origin projection.
	copyRec, rErr := s.toolRegistryStore.Get(ctx, callerNS, localName)
	if rErr != nil {
		// The create succeeded but the read-back failed — surface a summary built from the
		// spec rather than a 500, so the caller still gets the identity of what was made.
		return &mcpMaterializeResult{status: "connected", summary: mcpServerSummaryFromSpec(spec)}, nil
	}
	return &mcpMaterializeResult{
		status:  "connected",
		summary: mcpServerSummaryFromRegistry(storeToolRegistryToCRD(copyRec)),
	}, nil
}

// mcpConnectDiscoverable reports whether the origin server is DISCOVERABLE by a caller
// in callerNS, applying the m73.4 catalog visibility predicate (ADR 0067 §6):
//   - own namespace: always (the caller can materialize their own server);
//   - public: always (world-readable);
//   - org: only when originNS is in the caller's tenant member set;
//   - team / private: only in the caller's own namespace (handled by the own-ns clause).
//
// It uses the same namespaceTenantStore membership resolution as handleCatalog, with the
// same fail-closed degradation (own-ns + public only) on a nil store or a store error.
func (s *Server) mcpConnectDiscoverable(r *http.Request, origin *agentsv1alpha1.ToolRegistry, originNS, callerNS string) bool {
	// Own namespace: always discoverable (materializing your own server is trivially allowed).
	if originNS == callerNS {
		return true
	}

	vis, _ := mcpVisibility(origin)
	switch vis {
	case visibilityPublic:
		// World-readable from any namespace.
		return true
	case visibilityOrg:
		// Org-visible: discoverable only when originNS is in the caller's tenant.
		return s.namespaceInCallerTenant(r, callerNS, originNS)
	default:
		// team / private in a FOREIGN namespace: never discoverable (team is
		// within-namespace only; private is owner-only). The own-ns case is already
		// handled above, so reaching here means a foreign namespace → hidden.
		return false
	}
}

// namespaceInCallerTenant reports whether targetNS is a member of the tenant that owns
// callerNS, via the namespaceTenantStore (ADR 0067 §6). Fail-closed: a nil store, an
// unmapped caller namespace, or any store error → false (the org tier is unreachable,
// so only public + own-ns remain — matching handleCatalog's degraded behavior).
func (s *Server) namespaceInCallerTenant(r *http.Request, callerNS, targetNS string) bool {
	if s.namespaceTenantStore == nil {
		return false
	}
	tenant, ok, err := s.namespaceTenantStore.TenantOf(r.Context(), callerNS)
	if err != nil || !ok {
		return false
	}
	members, mErr := s.namespaceTenantStore.MembersOf(r.Context(), tenant)
	if mErr != nil {
		return false
	}
	return slices.Contains(members, targetNS)
}

// discoveredToolsFromRegistry projects an origin ToolRegistry's tool entries onto the
// []discoveredTool shape createMCPObjects consumes, carrying each tool's name,
// description, and inputSchema (verbatim). NON-secret — a tool catalog only.
func discoveredToolsFromRegistry(tr *agentsv1alpha1.ToolRegistry) []discoveredTool {
	out := make([]discoveredTool, 0, len(tr.Spec.Tools))
	for i := range tr.Spec.Tools {
		t := tr.Spec.Tools[i]
		dt := discoveredTool{Name: t.Name, Description: t.Description}
		if t.InputSchema != nil && len(t.InputSchema.Raw) > 0 {
			dt.InputSchema = append(json.RawMessage(nil), t.InputSchema.Raw...)
		}
		out = append(out, dt)
	}
	return out
}

// mcpOAuthConfigFromAnnotations recovers the NON-secret OAuth CLIENT config from an
// origin ToolRegistry's annotations (annMCPOAuth*), so the materialized copy carries
// the endpoints + public clientId + scope + redirect and the consumer can later begin
// THEIR OWN grant from {server, ns}. NO token material lives in annotations — this
// never touches a Secret.
func mcpOAuthConfigFromAnnotations(ann map[string]string) mcpOAuthConfig {
	return mcpOAuthConfig{
		AuthorizationEndpoint: ann[annMCPOAuthAuthEndpoint],
		TokenEndpoint:         ann[annMCPOAuthTokenEndpoint],
		ClientID:              ann[annMCPOAuthClientID],
		Scope:                 ann[annMCPOAuthScope],
		RedirectURI:           ann[annMCPOAuthRedirectURI],
	}
}

// mcpServerSummaryFromSpec builds a summary from the create spec (used only on the rare
// read-back failure after a successful create). It carries only NON-secret material.
func mcpServerSummaryFromSpec(spec mcpCreateSpec) MCPServerSummary {
	return MCPServerSummary{
		Name:             spec.name,
		Namespace:        spec.namespace,
		URL:              spec.url,
		ToolCount:        len(spec.tools),
		Status:           spec.status,
		AuthType:         spec.authType,
		Visibility:       spec.visibility,
		CredentialSource: spec.credentialSource,
	}
}
