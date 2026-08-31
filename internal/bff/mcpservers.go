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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// The BYO-MCP register + tool-catalog handlers (ADR 0016). All are CALLER-SCOPED
// (ADR 0011): the Secret / SecretBinding / ToolRegistry / NetworkPolicy are
// created and read with the CALLER'S own client, so the K8s API server enforces
// the caller's RBAC — a viewer with no create on those kinds is denied by the API
// server and the 403 surfaces (never a BFF-SA fallback). The optional bearer key
// is validated once by the probe, then written ONLY into the Secret — it is never
// returned in a DTO and never logged (the m14.4 discipline, repeated).
//
// Structurally this mirrors the connect-a-provider flow (providers.go): probe an
// external thing with a key → store the key as a Secret → create CRDs
// caller-scoped. The MCP-specific parts are the discovery handshake (mcp_client.go
// captures inputSchema), the ToolRegistry (one user-added entry per tool, storing
// inputSchema), the trust policy (self-serve vs pending-approval), and per-server
// egress.

// Labels/annotations stamped on every object the MCP register flow creates so
// GET /api/mcpservers can list exactly the register-managed ToolRegistries and
// the server is discoverable. Kept as constants so create + list agree.
const (
	// managedByMCP marks an object as created by the BYO-MCP register flow. It is
	// the value of labelManagedBy (shared with the connect flow's constant).
	managedByMCP = "ctxmesh-mcp"

	// labelMCPScope / labelMCPOwner stamp the server's SCOPE + OWNER as LABELS at
	// register (ADR 0029 §1/§3). scope ∈ {public, personal, org} is a visibility +
	// whose-credential axis, orthogonal to auth; owner is the registrant's userGrantHash
	// (personal only). They are metadata-only — a resolve/revoke NEVER keys on them (the
	// credential path keys on the invoker's hash + Secret RBAC), so tampering can hide/
	// show a listing but can never redirect a credential.
	labelMCPScope = "mcp.ctxmesh.ai/scope"
	labelMCPOwner = "mcp.ctxmesh.ai/owner"

	// labelMCPOriginNamespace / labelMCPOriginName stamp the PROVENANCE of a
	// discover-then-materialize copy (m73.6, ADR 0067 §3): when "Connect" imports a
	// catalog-discovered server's DEFINITION into the caller's own namespace, the
	// local copy records where it was materialized from. They are metadata-only —
	// the copy is a FROZEN one-time snapshot (no watch/sync back to the origin) and
	// no credential resolution ever keys on them. NON-secret: they name a namespace +
	// object name, never any token. Absent on a natively-registered server.
	labelMCPOriginNamespace = "mcp.ctxmesh.ai/origin-namespace"
	labelMCPOriginName      = "mcp.ctxmesh.ai/origin-name"

	// The scope values. personal is the default for a user-added server (visible only to
	// its owner); public is a no-auth / deliberately-open server (visible to all); org is
	// an admin-shared server (visible to all). A server with NO scope label is
	// grandfathered as org (behavior-preserving for pre-m25 servers).
	scopePublic   = "public"
	scopePersonal = "personal"
	scopeOrg      = "org"

	// labelMCPVisibility / labelMCPCredentialSource are the ADR 0067 §1/§2 two-axis
	// split of the legacy single-axis labelMCPScope. They are written alongside the
	// legacy label for one release (rollback aid) and are the authoritative read path
	// once stamped.
	//
	// visibility ∈ {private, team, org, public} controls who can see the server in
	// listings:
	//   private  — owner only (owner-gate via labelMCPOwner)
	//   team     — all members of the namespace (no owner gate)
	//   org      — all org members
	//   public   — unauthenticated / all callers
	//
	// credential-source ∈ {byo-oauth, shared, none} names how the server is
	// authenticated at egress:
	//   byo-oauth — per-user OAuth grant (owner supplies the credential)
	//   shared    — an org-level shared credential (promote path)
	//   none      — no auth required (open/public MCP server)
	labelMCPVisibility       = "mcp.ctxmesh.ai/visibility"
	labelMCPCredentialSource = "mcp.ctxmesh.ai/credential-source"

	visibilityPrivate = "private"
	visibilityTeam    = "team"
	visibilityOrg     = "org"
	visibilityPublic  = "public"

	credSourceByoOAuth = "byo-oauth"
	credSourceShared   = "shared"
	credSourceNone     = "none"

	// annMCPURL persists the registered server's URL on the ToolRegistry so the
	// list projection can surface it (non-secret).
	annMCPURL = "agents.ctxmesh.ai/mcp-url"
	// annMCPStatus persists the trust status ("approved"/"pending") so the list
	// projection can surface it without re-deriving from the entries.
	annMCPStatus = "agents.ctxmesh.ai/mcp-status"
	// annMCPSecret persists the Secret NAME (a reference, never the key) when the
	// server was registered with a key.
	annMCPSecret = "agents.ctxmesh.ai/mcp-secret"
	// annMCPOAuth* persist the discovered OAuth CLIENT config on the ToolRegistry so a
	// per-user grant can be BEGUN from just {server, ns} — the authorization endpoint is
	// otherwise discovered at register (ADR 0028) and thrown away (ADR 0031). All are
	// NON-SECRET: the public authorization/token endpoints, the public (PKCE) client id,
	// the requested scope, and the registered redirect URI. NO token material ever.
	annMCPOAuthAuthEndpoint  = "agents.ctxmesh.ai/mcp-oauth-authorization-endpoint"
	annMCPOAuthTokenEndpoint = "agents.ctxmesh.ai/mcp-oauth-token-endpoint"
	annMCPOAuthClientID      = "agents.ctxmesh.ai/mcp-oauth-client-id"
	annMCPOAuthScope         = "agents.ctxmesh.ai/mcp-oauth-scope"
	annMCPOAuthRedirectURI   = "agents.ctxmesh.ai/mcp-oauth-redirect-uri"
)

// networkPolicyMCPSuffix names the per-server egress NetworkPolicy derived from
// the server name (deterministic, so a re-register collides cleanly).
const networkPolicyMCPSuffix = "-mcp-egress"

// networkPolicyKind is the Kind label used in the created/deleted object lists.
const networkPolicyKind = "NetworkPolicy"

// handleRegisterMCPServer serves POST /api/mcpservers (ADR 0016). It:
//  1. reads + validates the request body;
//  2. PROBES the MCP server (initialize + tools/list) → the tools with their
//     inputSchema. A bad URL / non-MCP endpoint → an honest 4xx, never a 500;
//  3. if apiKey given → creates a Secret + SecretBinding (the key server-side
//     only — never in a DTO/log);
//  4. creates a user-added ToolRegistry entry per discovered tool, STORING each
//     tool's inputSchema. Trust: self-serve (default) → approved (bindable);
//     hardened (mcp.requireApproval) → pending-approval;
//  5. opens PER-SERVER egress (a NetworkPolicy allowing the agent registry to
//     reach that server's host/port) — self-serve opens it now.
//
// All creates use the CALLER'S client. A viewer denied a create surfaces a 403.
func (s *Server) handleRegisterMCPServer(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	req, perr := parseRegisterMCPRequest(raw)
	if perr != nil {
		writeError(w, perr.status, perr.msg)
		return
	}

	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = defaultCreateNamespace
	}
	name := mcpServerName(req.Name)

	// OAuth 2.1 tier (m17.2, ADR 0016): when auth.type == "oauth", registration does
	// NOT probe/create now — the server's tools are behind a token the user must
	// consent to. Instead the BFF starts an Auth-Code + PKCE flow SERVER-SIDE and
	// returns ONLY the authorization URL + state handle; the browser redirect →
	// consent → GET /api/mcp/oauth/callback completes the registration (token
	// exchange + Secret + probe). No token/verifier is ever in this branch's output.
	if req.Auth != nil && strings.EqualFold(strings.TrimSpace(req.Auth.Type), oauthAuthType) {
		s.beginMCPOAuthRegistration(w, r, req, name, ns)
		return
	}

	// (2) Probe the server. A bad URL / non-MCP endpoint → an honest 4xx with a
	// teaching message; an unreachable host → 502. The optional key is used only
	// for the probe here; it is neither returned nor logged.
	tools, err := probeMCPServer(r.Context(), s.providerHTTP, req.URL, req.APIKey)
	if err != nil {
		if me, isME := isMCPError(err); isME {
			writeError(w, me.status, me.msg)
			return
		}
		writeError(w, http.StatusBadGateway, "MCP discovery failed")
		return
	}

	// (3)+(4)+(5) Create the objects with the CALLER'S client. The key goes ONLY
	// into the Secret. A partial create (K8s is not transactional) leaves the
	// earlier objects and the error names the one that failed; the deterministic
	// names make a re-register a clean AlreadyExists → 409 (documented idempotency,
	// like the m14.4 provider orphan note).
	status := s.mcpApprovalStatus()
	// ADR 0067 §2 (reach-preserving new default):
	//   no-auth server  → visibility=team, credentialSource=none, scope=public (legacy inverse)
	//   keyed/OAuth server → visibility=private, credentialSource=byo-oauth, scope=personal (legacy inverse)
	// Both axes are validated via validateMCPCells before the create.
	var (
		scope            string
		visibility       string
		credentialSource string
		owner            string
	)
	if strings.TrimSpace(req.APIKey) == "" {
		// No-auth / open server: team-visible, no credential.
		scope = scopePublic
		visibility = visibilityTeam
		credentialSource = credSourceNone
		// Stamp owner when the identity is resolvable (harmless; enables "my servers" UX).
		if username, uErr := callerUsername(r.Context(), caller); uErr == nil {
			owner = userGrantHash(username)
		}
	} else {
		// Keyed server: private to the registrant.
		scope = scopePersonal
		visibility = visibilityPrivate
		credentialSource = credSourceByoOAuth
		if username, uErr := callerUsername(r.Context(), caller); uErr == nil {
			owner = userGrantHash(username)
		} else {
			// If the identity can't be resolved (never in prod — the same caller client
			// would then also fail the object create below), register without an owner label
			// rather than failing the whole flow.
			s.log.Error(uErr, "mcp register: could not resolve owner identity; server registered without an owner label")
		}
	}
	created, cErr := s.createMCPObjects(r.Context(), caller, mcpCreateSpec{
		name:             name,
		namespace:        ns,
		url:              strings.TrimSpace(req.URL),
		apiKey:           req.APIKey,
		tools:            tools,
		status:           status,
		scope:            scope,
		owner:            owner,
		visibility:       visibility,
		credentialSource: credentialSource,
	})
	if cErr != nil {
		writeError(w, cErr.status, cErr.msg)
		return
	}

	secretName := ""
	if strings.TrimSpace(req.APIKey) != "" {
		secretName = name
	}
	writeJSON(w, http.StatusCreated, RegisterMCPServerResponse{
		Server: MCPServerSummary{
			Name:       name,
			Namespace:  ns,
			URL:        strings.TrimSpace(req.URL),
			ToolCount:  len(tools),
			Status:     status,
			SecretName: secretName,
		},
		Tools:   toolCatalogEntriesFromDiscovered(name, ns, tools, status),
		Created: created,
	})
}

// mcpApprovalStatus returns the trust status a freshly-registered server's tools
// get: ApprovalPending on a HARDENED cluster (the values-gated mcpRequireApproval
// kill-switch pattern — the M17 approval queue is out of scope; here we only mark
// the state), else ApprovalApproved (self-serve default — immediately bindable).
func (s *Server) mcpApprovalStatus() string {
	if s.mcpRequireApproval {
		return agentsv1alpha1.ApprovalPending
	}
	return agentsv1alpha1.ApprovalApproved
}

// parseRegisterMCPRequest decodes + validates the register body. It returns a
// typed *createError (status + client-safe message) on a bad request. The error
// NEVER contains the key.
func parseRegisterMCPRequest(raw []byte) (RegisterMCPServerRequest, *createError) {
	var req RegisterMCPServerRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, &createError{status: http.StatusBadRequest, msg: msgInvalidJSONBody}
	}
	if strings.TrimSpace(req.Name) == "" {
		return req, &createError{status: http.StatusBadRequest, msg: "name is required"}
	}
	if strings.TrimSpace(req.URL) == "" {
		return req, &createError{status: http.StatusBadRequest, msg: "url is required (remote MCP server endpoint)"}
	}
	return req, nil
}

// mcpServerName derives a deterministic RFC-1123 object name from the server's
// display name, reusing the provider-flow sanitizer's intent (lowercase,
// [a-z0-9-], capped). Deterministic so re-registering the same server collides on
// create → a clean 409.
func mcpServerName(display string) string {
	base := strings.ToLower(strings.TrimSpace(display))
	base = rfc1123Invalid.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "mcp-server"
	}
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	return base
}

// mcpCreateSpec bundles the inputs for the MCP object-create so the creation
// logic is one testable unit.
type mcpCreateSpec struct {
	name      string
	namespace string
	url       string
	apiKey    string
	tools     []discoveredTool
	status    string
	// authType is the credential tier stamped as a non-secret annotation on the
	// created objects ("" / oauthAuthType). It only NAMES the scheme; no credential.
	authType string
	// oauthSecretData, when non-nil, is the OAuth-grant Secret payload (access +
	// refresh token, expiry, token endpoint, client id) written into the Secret
	// INSTEAD of a bearer key. It is the ONLY place the tokens land — never a DTO,
	// log, annotation, or label. Mutually exclusive with a non-empty apiKey.
	oauthSecretData map[string][]byte
	// scope / owner stamp the server's visibility + ownership (ADR 0029 §1). scope ∈
	// {public, personal, org}; owner is the registrant's userGrantHash (personal only).
	// Empty scope stamps nothing (the object is then grandfathered as org on read).
	scope string
	owner string
	// visibility / credentialSource are the ADR 0067 §1/§2 two-axis split. They are
	// written alongside the legacy scope label for one release (rollback aid). When
	// non-empty they are authoritative; absent means the row was created pre-m73 and
	// mcpVisibility() forward-maps from the legacy scope.
	visibility       string
	credentialSource string
	// oauthConfig, when authType == oauthAuthType, is the discovered OAuth CLIENT config
	// (endpoints + public clientId + scope + redirect). It is persisted as NON-SECRET
	// annotations so a per-user grant can later be begun from {server, ns} (ADR 0031).
	// No token material — those live only in oauthSecretData.
	oauthConfig mcpOAuthConfig
	// originNamespace / originName, when non-empty, stamp the discover-then-materialize
	// PROVENANCE labels (m73.6, ADR 0067 §3) on the created copy: which origin
	// namespace + server this local definition was imported from. Empty on a native
	// register (no origin). NON-secret — a namespace + name, never a credential.
	originNamespace string
	originName      string
}

// createMCPObjects creates, with the caller's client and in dependency order:
//   - a Secret + SecretBinding holding the bearer key (ONLY when a key was given —
//     the key lands in the Secret and nowhere else);
//   - a ToolRegistry with one user-added entry per discovered tool, each storing
//     the tool's inputSchema (the m14.3-review requirement) and the trust status;
//   - a per-server egress NetworkPolicy allowing the agent registry to reach the
//     server's host/port (self-serve opens it now; pending-approval would open it
//     on approval — an M17 step — but M14 self-serve opens it here).
//
// It returns the flat identity of every created object, or a typed *createError
// with the right HTTP status on the first failure (a viewer's denied create → an
// honest 403).
func (s *Server) createMCPObjects(ctx context.Context, caller client.Client, spec mcpCreateSpec) ([]createdObject, *createError) {
	labels := map[string]string{
		labelManagedBy: managedByMCP,
	}
	// Scope + owner (ADR 0029 §1/§3) — metadata-only visibility labels. A resolve/revoke
	// never keys on these, so they can hide/show a listing but never redirect a credential.
	if spec.scope != "" {
		labels[labelMCPScope] = spec.scope
	}
	if spec.owner != "" {
		labels[labelMCPOwner] = spec.owner
	}
	// ADR 0067 §1/§2: stamp the two new axes alongside the legacy label (rollback aid).
	// When visibility is non-empty, both axes are written; their values have already been
	// validated by the caller (validateMCPCells). An empty visibility means the spec was
	// built by a pre-m73 call site — those go through the normal legacy scope label only.
	if spec.visibility != "" {
		labels[labelMCPVisibility] = spec.visibility
	}
	if spec.credentialSource != "" {
		labels[labelMCPCredentialSource] = spec.credentialSource
	}
	// Provenance for a discover-then-materialize copy (m73.6, ADR 0067 §3). Stamped
	// atomically with the rest of the object so the copy carries its origin from
	// creation. Metadata-only — a resolve/revoke never keys on these.
	if spec.originNamespace != "" {
		labels[labelMCPOriginNamespace] = spec.originNamespace
	}
	if spec.originName != "" {
		labels[labelMCPOriginName] = spec.originName
	}
	hasKey := strings.TrimSpace(spec.apiKey) != ""
	hasOAuth := len(spec.oauthSecretData) > 0
	// The two credential tiers are mutually exclusive: an OAuth grant OR a bearer
	// key, never both. hasSecret gates whether a Secret/SecretBinding is created.
	hasSecret := hasKey || hasOAuth

	annotations := map[string]string{
		annMCPURL:    spec.url,
		annMCPStatus: spec.status,
	}
	if spec.authType != "" {
		annotations[annMCPAuthType] = spec.authType
	}
	if hasSecret {
		annotations[annMCPSecret] = spec.name
	}
	// Persist the OAuth client config (NON-SECRET) so a per-user grant can later be begun
	// from just {server, ns} (ADR 0031) — the authorization endpoint is otherwise lost
	// after register. Stamped only for OAuth servers; token material never lands here.
	if spec.authType == oauthAuthType {
		for k, v := range map[string]string{
			annMCPOAuthAuthEndpoint:  spec.oauthConfig.AuthorizationEndpoint,
			annMCPOAuthTokenEndpoint: spec.oauthConfig.TokenEndpoint,
			annMCPOAuthClientID:      spec.oauthConfig.ClientID,
			annMCPOAuthScope:         spec.oauthConfig.Scope,
			annMCPOAuthRedirectURI:   spec.oauthConfig.RedirectURI,
		} {
			if tv := strings.TrimSpace(v); tv != "" {
				annotations[k] = tv
			}
		}
	}

	// Build the ToolRegistry entries, storing each tool's inputSchema verbatim so
	// m14.6b can plumb it to the loop. Names are DNS-1123-sanitized by the CRD's
	// own MaxLength; here we only trim.
	entries := make([]agentsv1alpha1.ToolEntry, 0, len(spec.tools))
	for _, t := range spec.tools {
		entry := agentsv1alpha1.ToolEntry{
			Name:           truncateToolName(t.Name),
			URL:            spec.url,
			Description:    t.Description,
			Source:         agentsv1alpha1.SourceUserAdded,
			ApprovalStatus: spec.status,
		}
		if len(t.InputSchema) > 0 {
			// Store the schema verbatim (RawExtension.Raw) — never re-marshaled.
			entry.InputSchema = &runtime.RawExtension{Raw: append([]byte(nil), t.InputSchema...)}
		}
		entries = append(entries, entry)
	}

	// registry is the server catalog — a plain projection shape (ToolRegistry is
	// retired to Postgres, ADR 0044); it is written to the store, not the K8s API.
	registry := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.name,
			Namespace:   spec.namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{Tools: entries},
	}

	// The bundle is created in dependency order: the credential Secret +
	// SecretBinding (the key store) BEFORE the ToolRegistry catalog, then the egress
	// NetworkPolicy — those three are K8s objects created via the caller; the
	// ToolRegistry goes to the Postgres store between them.
	type kindObj struct {
		kind string
		o    client.Object
	}
	var beforeTR, afterTR []kindObj

	if hasSecret {
		// The Secret holds EITHER the bearer key (key tier) OR the OAuth grant
		// (access/refresh token + expiry + refresh inputs) — never both. This is the
		// ONLY object the credential material lands in; it is never in a DTO, log,
		// annotation, or label. The SecretBinding's referenced key differs by tier so
		// the credential resolver reads the right value.
		secretData := map[string][]byte{secretKeyAPIKey: []byte(spec.apiKey)}
		bindingKey := secretKeyAPIKey
		if hasOAuth {
			secretData = spec.oauthSecretData
			bindingKey = secretKeyOAuthAccessToken
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        spec.name,
				Namespace:   spec.namespace,
				Labels:      labels,
				Annotations: annotations,
			},
			Type: corev1.SecretTypeOpaque,
			Data: secretData,
		}
		binding := &agentsv1alpha1.SecretBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:        spec.name,
				Namespace:   spec.namespace,
				Labels:      labels,
				Annotations: annotations,
			},
			Spec: agentsv1alpha1.SecretBindingSpec{
				Backend:   secretBackendKubernetes,
				SecretRef: agentsv1alpha1.SecretKeyRef{Name: spec.name, Key: bindingKey},
			},
		}
		beforeTR = append(beforeTR, kindObj{secretKind, secret}, kindObj{secretBindingKind, binding})
	}

	// The per-server egress NetworkPolicy — opened for an APPROVED server ONLY
	// (ADR 0016 §4: "egress opens per approved server only"). Self-serve (default →
	// approved) opens egress now. In HARDENED mode a freshly-registered server is
	// pending-approval, so its egress hole is NOT opened here — the M17 approval
	// step opens it when an operator approves (that is the seam; the queue itself is
	// M17). Opening egress for a pending server would be a hole before any approval,
	// so we gate strictly on the status.
	if spec.status == agentsv1alpha1.ApprovalApproved {
		np, npErr := mcpEgressNetworkPolicy(spec.name, spec.namespace, spec.url, labels)
		if npErr != nil {
			return nil, npErr
		}
		afterTR = append(afterTR, kindObj{networkPolicyKind, np})
	}

	created := make([]createdObject, 0, len(beforeTR)+1+len(afterTR))
	createK8s := func(kos []kindObj) *createError {
		for _, ko := range kos {
			if err := caller.Create(ctx, ko.o); err != nil {
				return classifyCreateError(err, ko.kind, ko.o.GetName())
			}
			created = append(created, createdObject{Kind: ko.kind, Name: ko.o.GetName(), Namespace: ko.o.GetNamespace()})
		}
		return nil
	}

	if cErr := createK8s(beforeTR); cErr != nil {
		return created, cErr
	}
	// The ToolRegistry → the store, behind a caller-scoped SSAR VerbCreate + in-app
	// validation (atomic Create → 409). ADR 0044.
	if err := s.authorizeStore(ctx, caller, authz.VerbCreate, resourceToolRegistries, registry.Namespace, ""); err != nil {
		return created, toolRegistryStoreWriteError(err, registry.Name, "register the MCP server")
	}
	rec := crdToolRegistryToStore(registry)
	if vErr := toolregistry.Validate(rec); vErr != nil {
		return created, toolRegistryStoreWriteError(vErr, registry.Name, "register the MCP server")
	}
	if _, err := s.toolRegistryStore.Create(ctx, rec); err != nil {
		return created, toolRegistryStoreWriteError(err, registry.Name, "register the MCP server")
	}
	created = append(created, createdObject{Kind: toolRegistryKind, Name: registry.Name, Namespace: registry.Namespace})

	if cErr := createK8s(afterTR); cErr != nil {
		return created, cErr
	}
	return created, nil
}

// truncateToolName bounds a discovered tool name to the ToolEntry.Name CRD
// MaxLength (63). The real catalog name is what the loop matches on; MCP tool
// names are short, so truncation is defensive.
func truncateToolName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > 63 {
		return name[:63]
	}
	return name
}

// resolveHostIPs looks up a hostname to its IP addresses. It is a package var so
// tests can stub DNS deterministically (an httptest fixture resolves to 127.0.0.1;
// an external host needs a real lookup). Production uses net.LookupIP.
var resolveHostIPs = net.LookupIP

// mcpEgressNetworkPolicy builds a per-server egress NetworkPolicy: it allows
// egress from agent-registry member pods (selected by the presence of the
// registry-id label) to the MCP server's IP(s)/port ONLY — never a blanket open,
// preserving the M6 whitelist + M11 default-deny (ADR 0007/0016).
//
// The `to` peers are ALWAYS bounded ipBlock CIDRs scoped to the server:
//   - an IP-literal host → its /32 (v4) or /128 (v6) host route;
//   - a DNS host → RESOLVED to IP(s) at register time (net.LookupIP), one ipBlock
//     per resolved address. This is the crux: a NetworkPolicy with empty `to`
//     peers means "allow to ANY destination on that port" — a blanket-port hole
//     that would silently undo default-deny. We NEVER emit an empty-peers rule; a
//     host that resolves to nothing → a teaching 4xx (the caller must give an
//     IP/CIDR-addressable server).
//
// Staleness caveat: the ipBlock is pinned at register time. If the server's DNS
// record later changes IPs, the policy goes stale and the caller must re-register
// (acceptable for M14 — an operator-level BYO action, not a hot path). A CNI with
// DNS-aware policies (Cilium FQDN) would remove this caveat; out of scope here.
//
// Kept caller-scoped: this object is created with the caller's client alongside
// the ToolRegistry.
func mcpEgressNetworkPolicy(name, namespace, rawURL string, labels map[string]string) (*networkingv1.NetworkPolicy, *createError) {
	host, port, err := hostPortFromURL(rawURL)
	if err != nil {
		return nil, &createError{status: http.StatusBadRequest, msg: "the MCP server URL has no resolvable host:port for egress"}
	}

	peers, cErr := egressPeersForHost(host)
	if cErr != nil {
		return nil, cErr
	}

	protoTCP := corev1.ProtocolTCP
	portVal := intstr.FromInt32(int32(port)) //nolint:gosec // port is bounded 1..65535 by hostPortFromURL
	egressRule := networkingv1.NetworkPolicyEgressRule{
		// INVARIANT: peers is non-empty and every entry is an ipBlock scoped to the
		// server — never an empty `to` (which would allow the port to anywhere).
		To: peers,
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &protoTCP, Port: &portVal},
		},
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name + networkPolicyMCPSuffix,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: map[string]string{annMCPURL: rawURL},
		},
		Spec: networkingv1.NetworkPolicySpec{
			// Select agent-registry member pods (they carry the registry-id label).
			// Additive to the registry's own default-deny policy: this opens ONE more
			// egress destination (the MCP server's port) without touching anything
			// else — the whitelist grows by exactly this server.
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      registryIDLabelKey,
					Operator: metav1.LabelSelectorOpExists,
				}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{egressRule},
		},
	}
	return np, nil
}

// egressPeersForHost turns an MCP server host into BOUNDED ipBlock egress peers —
// the single choke point that enforces "never an empty-peers rule":
//   - an IP literal → its /32 (v4) or /128 (v6) host route;
//   - a DNS name → resolved via net.LookupIP (stubbable) to one ipBlock per IP.
//
// A DNS host that resolves to NOTHING (or fails) → a teaching 4xx, so the caller
// gets an honest "could not resolve <host>" instead of a silent blanket-port hole.
// The returned slice is guaranteed non-empty on success.
func egressPeersForHost(host string) ([]networkingv1.NetworkPolicyPeer, *createError) {
	if ip := net.ParseIP(host); ip != nil {
		return []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: ipToHostCIDR(ip)}}}, nil
	}

	// A DNS host: resolve it to IP(s) at register time and pin each as an ipBlock.
	ips, err := resolveHostIPs(host)
	if err != nil || len(ips) == 0 {
		return nil, &createError{
			status: http.StatusBadRequest,
			msg: fmt.Sprintf(
				"could not resolve MCP server host %q for per-server egress; provide an IP- or CIDR-addressable MCP server",
				host,
			),
		}
	}
	// Dedupe (LookupIP can return the same address across families/records) and
	// emit one ipBlock per distinct host route so egress is bounded to exactly the
	// resolved addresses.
	seen := map[string]struct{}{}
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(ips))
	for _, ip := range ips {
		cidr := ipToHostCIDR(ip)
		if _, dup := seen[cidr]; dup {
			continue
		}
		seen[cidr] = struct{}{}
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	return peers, nil
}

// ipToHostCIDR renders an IP as a single-host CIDR (/32 for IPv4, /128 for IPv6),
// using the canonical string form so an IPv4-in-IPv6 address is expressed as v4.
func ipToHostCIDR(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String() + "/32"
	}
	return ip.String() + "/128"
}

// registryIDLabelKey is the agent-registry membership label the controller stamps
// on member pods (the same key internal/controller uses as registryIDLabel). An
// additive per-server egress policy selects member pods by its PRESENCE so the
// opened destination applies to every agent in the namespace's registries. Kept
// as a local constant so the BFF does not import the controller package.
const registryIDLabelKey = "agents.ctxmesh.ai/registry-id"

// hostPortFromURL parses a URL into its host and port, defaulting the port from
// the scheme (http→80, https→443). It returns an error for a URL with no host or
// an out-of-range port so the egress peer is always well-formed.
func hostPortFromURL(rawURL string) (string, int, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return "", 0, fmt.Errorf("no host in URL")
	}
	host := u.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("no host in URL")
	}
	portStr := u.Port()
	if portStr == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			return host, 443, nil
		case "http", "":
			return host, 80, nil
		default:
			return "", 0, fmt.Errorf("unknown scheme %q", u.Scheme)
		}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
}

// toolCatalogEntriesFromDiscovered projects freshly-discovered tools onto the
// flat catalog DTO (used in the register response). InputSchema is passed through
// verbatim; no secret material is present.
func toolCatalogEntriesFromDiscovered(registry, namespace string, tools []discoveredTool, status string) []ToolCatalogEntry {
	out := make([]ToolCatalogEntry, 0, len(tools))
	for _, t := range tools {
		var schema json.RawMessage
		if len(t.InputSchema) > 0 {
			schema = append(json.RawMessage(nil), t.InputSchema...)
		}
		out = append(out, ToolCatalogEntry{
			Name:           truncateToolName(t.Name),
			Registry:       registry,
			Namespace:      namespace,
			Description:    t.Description,
			InputSchema:    schema,
			Source:         agentsv1alpha1.SourceUserAdded,
			ApprovalStatus: status,
		})
	}
	return out
}

// handleListMCPServers serves GET /api/mcpservers — the registered servers, read
// through the CALLER-SCOPED client. It lists register-managed ToolRegistries
// (labelled managed-by=ctxmesh-mcp) and projects each onto the flat
// MCPServerSummary — NO secret material, only the Secret NAME as a reference.
// Servers is [] (not null) for the empty case; a Forbidden on the list surfaces
// as 403, never a swallowed empty list.
func (s *Server) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	namespace := r.URL.Query().Get("namespace")

	registries, err := s.mcpListToolRegistries(r.Context(), caller, namespace,
		map[string]string{labelManagedBy: managedByMCP})
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list MCP servers failed")
		writeError(w, http.StatusInternalServerError, "failed to list MCP servers")
		return
	}

	// Owner-filtered visibility (ADR 0029 §3): public + org (+ grandfathered) are shown to
	// all; a personal server only to its owner. A caller-identity lookup failure yields an
	// empty owner, which hides every personal server (fail-closed, never leak another's).
	callerOwner := ""
	if username, uErr := callerUsername(r.Context(), caller); uErr == nil {
		callerOwner = userGrantHash(username)
	}
	summaries := make([]MCPServerSummary, 0, len(registries.Items))
	for i := range registries.Items {
		if !mcpScopeVisibleTo(&registries.Items[i], callerOwner) {
			continue
		}
		summaries = append(summaries, mcpServerSummaryFromRegistry(&registries.Items[i]))
	}
	slices.SortFunc(summaries, func(a, b MCPServerSummary) int { return strings.Compare(a.Name, b.Name) })

	writeJSON(w, http.StatusOK, MCPServerListResponse{Servers: summaries, Items: summaries})
}

// mcpVisibility returns the two-axis ADR 0067 §1/§2 view of a ToolRegistry:
// (visibility, credentialSource). It is a dual-read helper — it prefers the new
// labels when present, and falls back to a lossless forward-mapping from the
// legacy labelMCPScope so pre-m73 rows are never mis-classified:
//
//	personal  → (private,  byo-oauth)
//	org       → (team,     shared)
//	public    → (team,     none)     ← reach-preserving, not visibilityPublic
//	absent    → (team,     shared)   ← grandfathered org (matches the §R7 default)
//
// The mapping is a one-way translation; the returned values are never written
// back to the store here (callers that write must stamp all three labels).
func mcpVisibility(tr *agentsv1alpha1.ToolRegistry) (visibility, credentialSource string) {
	if v := tr.Labels[labelMCPVisibility]; v != "" {
		// New labels present — read them directly.
		cs := tr.Labels[labelMCPCredentialSource]
		if cs == "" {
			cs = credSourceNone
		}
		return v, cs
	}
	// Legacy forward-mapping (dual-read).
	switch tr.Labels[labelMCPScope] {
	case scopePersonal:
		return visibilityPrivate, credSourceByoOAuth
	case scopeOrg:
		return visibilityTeam, credSourceShared
	case scopePublic:
		return visibilityTeam, credSourceNone
	default: // absent / unknown → grandfathered org
		return visibilityTeam, credSourceShared
	}
}

// validateMCPCells returns a *createError (400) when the (visibility,
// credentialSource) combination is semantically invalid. Only write paths call
// this — never read paths. The two forbidden cells are:
//   - (private, shared):  a private server cannot have a shared credential
//     (shared credentials are org-level; a private server is owner-gated).
//   - (public, shared):   a public server cannot have a shared credential
//     (public means no-auth; a shared credential implies an org identity gate).
func validateMCPCells(visibility, credentialSource string) *createError {
	switch {
	case visibility == visibilityPrivate && credentialSource == credSourceShared:
		return &createError{
			status: http.StatusBadRequest,
			msg:    fmt.Sprintf("invalid MCP server configuration: visibility %q with credential-source %q is not allowed — a private server cannot use a shared credential", visibility, credentialSource),
		}
	case visibility == visibilityPublic && credentialSource == credSourceShared:
		return &createError{
			status: http.StatusBadRequest,
			msg:    fmt.Sprintf("invalid MCP server configuration: visibility %q with credential-source %q is not allowed — a public server cannot use a shared credential", visibility, credentialSource),
		}
	}
	return nil
}

// mcpScopeVisibleTo reports whether a register-managed server is visible to the caller
// identified by callerOwner (their userGrantHash). It uses the derived ADR 0067
// visibility axis (dual-read via mcpVisibility): only a private server is owner-gated;
// team/org/public are visible to all callers. A server with NO labels is grandfathered
// as team (visible to all) — behavior-preserving for pre-m25/pre-m73 servers (R7).
func mcpScopeVisibleTo(tr *agentsv1alpha1.ToolRegistry, callerOwner string) bool {
	vis, _ := mcpVisibility(tr)
	if vis == visibilityPrivate {
		return callerOwner != "" && tr.Labels[labelMCPOwner] == callerOwner
	}
	return true // team, org, public, or absent (grandfathered)
}

// mcpServerSummaryFromRegistry projects a register-managed ToolRegistry onto the
// flat MCPServerSummary. It carries only NON-secret material: the server/registry
// name, the URL, the tool count, the trust status, and the Secret NAME (a
// reference). No key is ever touched here.
func mcpServerSummaryFromRegistry(tr *agentsv1alpha1.ToolRegistry) MCPServerSummary {
	status := tr.Annotations[annMCPStatus]
	if status == "" {
		status = agentsv1alpha1.ApprovalApproved
	}
	// Scope for display: the label value, grandfathering an absent label to "org"
	// (ADR 0029 — a pre-scope server is visible org-wide). Visibility only; a resolve
	// never keys on this.
	scope := tr.Labels[labelMCPScope]
	if scope == "" {
		scope = scopeOrg
	}
	// ADR 0067 §1/§2: populate the two new axes alongside the legacy Scope field so
	// consumers can adopt the new fields incrementally (back-compat: Scope stays).
	vis, cs := mcpVisibility(tr)
	return MCPServerSummary{
		Name:             tr.Name,
		Namespace:        tr.Namespace,
		URL:              tr.Annotations[annMCPURL],
		ToolCount:        len(tr.Spec.Tools),
		Status:           status,
		SecretName:       tr.Annotations[annMCPSecret],
		AuthType:         mcpServerSummaryAuthType(tr),
		Scope:            scope,
		Visibility:       vis,
		CredentialSource: cs,
	}
}

// handleListTools serves GET /api/tools — the MERGED catalog: every ToolRegistry
// the caller can read (operator-curated + user-added), each tool projected onto
// the flat catalog DTO with its inputSchema, source, and approval status. Read
// through the CALLER-SCOPED client. Tools is [] (not null) for the empty case; a
// Forbidden on the list surfaces as 403. No entry carries secret material.
func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	namespace := r.URL.Query().Get("namespace")

	registries, err := s.mcpListToolRegistries(r.Context(), caller, namespace, nil)
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list tools failed")
		writeError(w, http.StatusInternalServerError, "failed to list tools")
		return
	}

	// Owner-filtered visibility (ADR 0029 §3): a personal MCP server's tools show only to
	// its owner; public/org/curated (no scope) show to all. Fail-closed on an identity
	// lookup failure (empty owner hides every personal server).
	callerOwner := ""
	if username, uErr := callerUsername(r.Context(), caller); uErr == nil {
		callerOwner = userGrantHash(username)
	}
	tools := make([]ToolCatalogEntry, 0)
	for ri := range registries.Items {
		tr := &registries.Items[ri]
		if !mcpScopeVisibleTo(tr, callerOwner) {
			continue
		}
		tools = append(tools, toolCatalogEntriesFromRegistry(tr)...)
	}
	// Deterministic order for stable rendering + tests: by registry then tool name.
	slices.SortFunc(tools, func(a, b ToolCatalogEntry) int {
		if c := strings.Compare(a.Registry, b.Registry); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})

	writeJSON(w, http.StatusOK, ToolCatalogResponse{Tools: tools, Items: tools})
}

// toolCatalogEntriesFromRegistry projects a ToolRegistry's entries onto the flat
// catalog DTO, carrying each tool's inputSchema verbatim + its source/approval.
// A legacy entry with no explicit source/status defaults to curated/approved (so
// pre-M14 operator registries render correctly). No secret material is present —
// the registry itself carries none; the key lives in a Secret this read never
// opens.
func toolCatalogEntriesFromRegistry(tr *agentsv1alpha1.ToolRegistry) []ToolCatalogEntry {
	out := make([]ToolCatalogEntry, 0, len(tr.Spec.Tools))
	for i := range tr.Spec.Tools {
		e := &tr.Spec.Tools[i]
		source := e.Source
		if source == "" {
			source = agentsv1alpha1.SourceCurated
		}
		status := e.ApprovalStatus
		if status == "" {
			status = agentsv1alpha1.ApprovalApproved
		}
		var schema json.RawMessage
		if e.InputSchema != nil && len(e.InputSchema.Raw) > 0 {
			schema = append(json.RawMessage(nil), e.InputSchema.Raw...)
		}
		out = append(out, ToolCatalogEntry{
			Name:           e.Name,
			Registry:       tr.Name,
			Namespace:      tr.Namespace,
			Description:    e.Description,
			InputSchema:    schema,
			Source:         source,
			ApprovalStatus: status,
		})
	}
	return out
}
