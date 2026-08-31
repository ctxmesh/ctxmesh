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
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/auditlog"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

const (
	// resourceKindMCPGrant is the audit ResourceKind for a per-user MCP grant create/revoke (ADR 0056 §2).
	resourceKindMCPGrant = "MCPGrant"
	// detailKeyUserHash is the audit detail key carrying the (non-secret) per-user grant hash.
	detailKeyUserHash = "userHash"
)

// parseGrantConsentRequest decodes + validates the POST /api/mcp/oauth/grant body.
// It returns a typed *createError (status + client-safe message) on a bad request.
// The OAuth config is validated by the handler (cfg.validate); here we only ensure
// the server + an oauth-typed auth block are present. No secret material is present.
func parseGrantConsentRequest(raw []byte) (MCPGrantConsentRequest, *createError) {
	var req MCPGrantConsentRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, &createError{status: http.StatusBadRequest, msg: msgInvalidJSONBody}
	}
	if strings.TrimSpace(req.Server) == "" {
		return req, &createError{status: http.StatusBadRequest, msg: "server is required (the registered MCP server to consent to)"}
	}
	// Auth is OPTIONAL (ADR 0031): the server's OAuth client config is recovered from the
	// registration, so the caller (e.g. the Playground) begins consent from just the
	// server name. When an auth block IS supplied (a legacy server, or an override) and
	// names a type, that type must be oauth.
	if req.Auth != nil && strings.TrimSpace(req.Auth.Type) != "" && !strings.EqualFold(strings.TrimSpace(req.Auth.Type), oauthAuthType) {
		return req, &createError{status: http.StatusBadRequest, msg: `auth.type must be "oauth" for per-user consent`}
	}
	return req, nil
}

// The per-user on-behalf-of (OBO) GRANT handlers (m17.3, ADR 0016 §5): a user
// consents to an OAuth MCP server → THEIR OAuth grant is stored as a (user, server)
// Secret; a user revokes their OWN grant → the next call for that (user, server)
// returns "consent required". All are CALLER-SCOPED (ADR 0011): the consenting /
// revoking user's identity comes from a SelfSubjectReview through their own token,
// and the grant Secret writes run as that user. A user can only touch their OWN
// grant. Every create/revoke is audited (never the token).
//
// The consent flow REUSES the m17.2 Auth-Code + PKCE machinery (startOAuthFlow +
// the shared callback) — the only difference is the pending flow is marked with the
// consenting user's hash (grantUserHash), so the callback stores a grant Secret
// instead of registering a server.

// callerUsername resolves the invoking caller's username via a SelfSubjectReview
// through their OWN client (the same path handleWhoAmI uses, ADR 0011). This is the
// stable identity a (user, server) grant is keyed by — the BFF never trusts a
// client-supplied user field; the API server is the single source of who the caller
// is. A rejected/empty identity is an error (a grant must not be keyed to an
// anonymous "").
func callerUsername(ctx context.Context, caller client.Client) (string, error) {
	// A SelfSubjectReview asks the API server "who am I?" for the token scoping this
	// client — a create-only virtual resource the server fills on the create.
	review := &authnv1.SelfSubjectReview{}
	if err := caller.Create(ctx, review); err != nil {
		return "", err
	}
	username := strings.TrimSpace(review.Status.UserInfo.Username)
	if username == "" {
		return "", errors.New("the API server returned no username for the caller")
	}
	return username, nil
}

// beginMCPGrantConsent serves POST /api/mcp/oauth/grant — a user initiating OAuth
// consent for an ALREADY-REGISTERED OAuth server so their per-user grant is stored
// (m17.3, ADR 0016 §5). It:
//  1. recovers the server's OAuth client config from the register-time annotations
//     (ADR 0031) — so the caller begins consent from just {server, ns}; this also
//     confirms the server is a registered OAuth server (a non-OAuth / absent /
//     unreadable server → an honest 4xx);
//  2. resolves the caller's identity (SelfSubjectReview, caller-scoped) — the grant
//     is keyed to THIS user, never a client-supplied user field;
//  3. starts the m17.2 Auth-Code + PKCE flow with the recovered OAuth config, marked
//     with the caller's HASHED identity so the shared callback stores a
//     (user, server) grant Secret.
//
// It returns 202 + the authorization URL + state (the m17.2 pending shape) — NO
// token, NO verifier. The request carries only {server, ns} (an optional auth block
// overlays the recovered config for legacy servers); no secret material is in the
// request or the response.
func (s *Server) beginMCPGrantConsent(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	req, perr := parseGrantConsentRequest(raw)
	if perr != nil {
		writeError(w, perr.status, perr.msg)
		return
	}

	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = defaultCreateNamespace
	}
	server := mcpServerName(req.Server)

	// Recover the server's OAuth client config from the registration (ADR 0031) — the
	// caller (e.g. the Playground) need not, and cannot, supply it. This also confirms the
	// server is a registered OAuth server (caller-scoped): a non-OAuth / absent /
	// unreadable server → an honest 4xx. A caller MAY still pass an explicit auth block (a
	// legacy server registered before config persistence, or an override its own origin's
	// redirect); non-empty request fields overlay the recovered base.
	cfg, serverURL, cErr := s.recoverRegisteredOAuth(r.Context(), caller, ns, server)
	if cErr != nil {
		writeError(w, cErr.status, cErr.msg)
		return
	}
	cfg = overlayOAuthConfig(cfg, req.Auth)

	// The redirect_uri is SERVER-controlled (ADR 0040): one canonical, pre-registered callback for
	// every consent regardless of the initiating origin. Consent opened from an agent's own hostname
	// would otherwise send that hostname's unregistered callback and be refused by the provider. The
	// SPA-supplied origin is only a hint; the canonical origin wins (falls back to the request origin
	// when no console URL is configured — single-origin behaviour unchanged).
	cfg.RedirectURI = s.canonicalOrigin(r) + mcpOAuthCallbackPath

	// Legacy backfill (m26.1b): a server registered before config persistence has no
	// endpoints/clientId to recover. Re-run discovery/DCR from the stored URL — register's
	// own path — to complete the config, then persist the annotations so it is a ONE-TIME
	// recovery (subsequent connects read them). The SPA supplies the redirectURI (only the
	// browser knows its origin); discovery needs it. A discovery failure is a
	// connect-validation outcome (ADR 0027) → 422, not a 500.
	if oauthConfigIncomplete(cfg) {
		discovered, dErr := discoverMCPOAuthConfig(r.Context(), nil, serverURL, "", cfg.RedirectURI)
		if dErr != nil {
			writeError(w, http.StatusUnprocessableEntity,
				"could not recover this server's OAuth config automatically: "+dErr.Error())
			return
		}
		cfg = fillEmptyOAuthConfig(cfg, discovered)
		s.backfillMCPOAuthConfig(r.Context(), caller, ns, server, cfg) // best-effort
	}

	if vErr := cfg.validate(); vErr != nil {
		writeError(w, vErr.status, vErr.msg)
		return
	}

	// Resolve the caller's identity (caller-scoped) — the grant is keyed to THIS user.
	username, uErr := callerUsername(r.Context(), caller)
	if uErr != nil {
		if status, msg, isRBAC := classifyReadError(uErr); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(uErr, "resolve caller identity for MCP grant consent failed")
		writeError(w, http.StatusInternalServerError, "failed to resolve caller identity")
		return
	}

	// Start the PKCE flow marked with the caller's HASHED identity + their token
	// (the callback's grant-Secret write runs as this user). The redirect back
	// carries no Authorization header, so the flow carries the identity that gates
	// the write (ADR 0011).
	// Scope the grant to the connecting agent's trust boundary (ADR 0033) when the consent is
	// initiated from a specific agent's run — so it empowers that agent's registry, not every
	// agent the user owns. No agent (e.g. a servers-page consent) ⇒ "" (a legacy unscoped grant).
	boundary := ""
	if a := strings.TrimSpace(req.Agent); a != "" {
		boundary = agentBoundary(r.Context(), caller, ns, a)
	}

	authURL, state, sErr := s.startOAuthFlow(cfg, pendingOAuthFlow{
		callerToken:   bearerToken(r),
		serverName:    server,
		namespace:     ns,
		serverURL:     serverURL,
		grantUserHash: userGrantHash(username),
		boundary:      boundary,
		// Where to relay the "connected" signal after the callback (ADR 0040) — the agent hostname
		// that opened the popup, or "" (same-origin) when off-domain / no console configured.
		openerOrigin: s.allowedOpenerOrigin(r),
	})
	if sErr != nil {
		s.log.Error(sErr, "start MCP grant consent flow failed", "server", server)
		writeError(w, http.StatusInternalServerError, "failed to start the consent flow")
		return
	}

	writeJSON(w, http.StatusAccepted, OAuthPendingResponse{
		Status:           "authorization_required",
		AuthorizationURL: authURL,
		State:            state,
		Server: MCPServerSummary{
			Name:      server,
			Namespace: ns,
			URL:       serverURL,
			AuthType:  oauthAuthType,
		},
	})
}

// completeGrantConsent stores the exchanged tokens as the consenting user's
// (user, server) grant Secret and returns 201 (m17.3). It is the grant branch of
// the shared OAuth callback: the tokens land ONLY in the Secret's data
// (oauthSecretData) — never a DTO/log/annotation/label. The write runs caller-
// scoped (the token captured at consent-begin); a viewer's denied write surfaces a
// 403. The response DTO carries the (user, server) identity + the server, never a
// token.
func (s *Server) completeGrantConsent(ctx context.Context, w http.ResponseWriter, r *http.Request, caller client.Client, flow pendingOAuthFlow, toks oauthTokens) {
	// SPI write path (ADR 0032): when a token-service grant store is configured, DELEGATE
	// the persist to it so the grant lands in the config-selected backend (Postgres /
	// remote) with DB/vault creds kept in the token-service, not this BFF. Otherwise fall
	// through to the direct k8s-Secret write (the kubernetes default / dev).
	if s.grantStore != nil {
		g := credresolve.Grant{
			Tokens:    toks,
			Config:    credresolve.OAuthConfig{TokenEndpoint: flow.oauth.TokenEndpoint, ClientID: flow.oauth.ClientID},
			ServerURL: flow.serverURL,
		}
		// The grant is stored under the connecting agent's trust boundary (ADR 0033), captured
		// at consent-begin — so the write key matches the boundary a run of that agent resolves
		// within (m30.5). "" ⇒ a legacy unscoped grant (connect-for-all).
		if err := s.grantStore.StoreGrant(ctx, flow.namespace, flow.boundary, flow.serverName, flow.grantUserHash, g); err != nil {
			s.log.Error(err, "delegate MCP per-user grant store failed", "server", flow.serverName)
			oauthCallbackError(w, r, "failed to store the per-user grant")
			return
		}
		s.grantAudit().record(grantAuditEntry{
			action:    grantActionCreate,
			server:    flow.serverName,
			userHash:  flow.grantUserHash,
			namespace: flow.namespace,
		})
		s.appendAudit(ctx, auditlog.Entry{
			Actor: s.auditActor(ctx, caller), Action: auditActionGrantCreate,
			ResourceKind: resourceKindMCPGrant, ResourceName: flow.serverName, Namespace: flow.namespace,
			Detail: map[string]any{detailKeyUserHash: flow.grantUserHash, "boundary": flow.boundary},
		})
		oauthCallbackConnected(w, r, flow.serverName, flow.openerOrigin)
		return
	}

	// In locked mode the grant lands in the credential namespace with the source ns
	// folded into the coordinates + the source-namespace label; in legacy mode it stays
	// in flow.namespace under the original name (sourceNs label "").
	grantNS, grantName := s.grantCoordinates(flow.namespace, flow.boundary, flow.serverName, flow.grantUserHash)
	labels := grantSecretLabels(flow.serverName, flow.grantUserHash, s.grantSourceNSLabel(flow.namespace), flow.boundary)
	ann := map[string]string{annMCPGrantServerURL: flow.serverURL}
	if flow.boundary != "" {
		// Record the raw boundary (non-secret) so the migration ListGrants preserves the scope.
		ann[credresolve.AnnGrantBoundary] = flow.boundary
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      grantName,
			Namespace: grantNS,
			Labels:    labels,
			// The server URL is non-secret and useful to the refresh path; the tokens
			// are NEVER here — only in Data (oauthSecretData).
			Annotations: ann,
		},
		Type: corev1.SecretTypeOpaque,
		Data: oauthSecretData(flow.oauth, toks),
	}

	// Upsert: a re-consent for the SAME (user, server) replaces the stored grant
	// (rotated tokens) rather than failing on AlreadyExists — re-consent is a valid,
	// idempotent user action. The write runs as the privileged credential client in
	// locked mode (tenants have no RBAC there), else caller-scoped (ADR 0011).
	if err := s.upsertGrantSecret(ctx, s.grantClient(caller), secret); err != nil {
		if _, msg, isRBAC := classifyReadError(err); isRBAC {
			oauthCallbackError(w, r, msg)
			return
		}
		s.log.Error(err, "store MCP per-user grant failed", "server", flow.serverName)
		oauthCallbackError(w, r, "failed to store the per-user grant")
		return
	}

	s.grantAudit().record(grantAuditEntry{
		action:    grantActionCreate,
		server:    flow.serverName,
		userHash:  flow.grantUserHash,
		namespace: flow.namespace,
	})
	s.appendAudit(ctx, auditlog.Entry{
		Actor: s.auditActor(ctx, caller), Action: auditActionGrantCreate,
		ResourceKind: resourceKindMCPGrant, ResourceName: flow.serverName, Namespace: flow.namespace,
		Detail: map[string]any{detailKeyUserHash: flow.grantUserHash, "boundary": flow.boundary},
	})

	// This callback is browser-facing (the OAuth redirect target): send the user back
	// to the tool catalog with a success toast, not JSON. No token is ever in the URL.
	oauthCallbackConnected(w, r, flow.serverName, flow.openerOrigin)
}

// handleRevokeMCPGrant serves DELETE /api/mcp/oauth/grant/{server} — a user
// revoking their OWN (user, server) grant (m17.3, ADR 0016 §5). It resolves the
// caller's identity (caller-scoped), deletes ONLY that user's grant Secret, and
// audits it. Caller-scoped: a user can only revoke their own grant — the Secret
// name embeds the CALLER's hash, so another user's grant is a different object the
// caller cannot even name here, and the K8s API server's RBAC still gates the
// delete. After revoke, the next resolve for that (user, server) returns
// consent-required (honest re-consent) — never a silent success over a stale grant.
func (s *Server) handleRevokeMCPGrant(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	server := mcpServerName(r.PathValue("server"))
	ns := strings.TrimSpace(r.URL.Query().Get("namespace"))
	if ns == "" {
		ns = defaultCreateNamespace
	}

	username, uErr := callerUsername(r.Context(), caller)
	if uErr != nil {
		if status, msg, isRBAC := classifyReadError(uErr); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(uErr, "resolve caller identity for MCP grant revoke failed")
		writeError(w, http.StatusInternalServerError, "failed to resolve caller identity")
		return
	}
	userHash := userGrantHash(username)

	// Delete ONLY the caller's own (user, server) grant. The name embeds the caller's
	// hash, so it can never NAME another user's grant; in locked mode the write ran as
	// the privileged credential client, so the delete does too. Revoke targets the legacy
	// unscoped grant (boundary ""); per-boundary revoke (revoke "for agent X") is a later
	// refinement — the revoke DTO carries no agent yet.
	grantNS, grantName := s.grantCoordinates(ns, "", server, userHash)
	gc := s.grantClient(caller)

	// Defence in depth for the privileged, shared-namespace delete: verify the target's
	// labels match THIS caller before removing it, so a truncated-name collision can
	// never delete another user's grant (the resolver's findGrant guards reads the same
	// way). The legacy caller-scoped delete relies on the caller's own RBAC + the
	// hash-embedded name and skips the extra read (behaviour unchanged).
	if s.lockedCredentials() {
		var existing corev1.Secret
		if gErr := gc.Get(r.Context(), client.ObjectKey{Namespace: grantNS, Name: grantName}, &existing); gErr != nil {
			if apierrors.IsNotFound(gErr) {
				writeError(w, http.StatusNotFound, "no per-user grant exists for this server")
				return
			}
			s.log.Error(gErr, "read MCP per-user grant for revoke failed", "server", server)
			writeError(w, http.StatusInternalServerError, "failed to revoke the per-user grant")
			return
		}
		if existing.Labels[labelMCPGrantUser] != userHash ||
			existing.Labels[labelMCPGrantServer] != server ||
			existing.Labels[labelMCPGrantSourceNS] != ns {
			writeError(w, http.StatusNotFound, "no per-user grant exists for this server")
			return
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      grantName,
			Namespace: grantNS,
		},
	}
	if err := gc.Delete(r.Context(), secret); err != nil {
		if apierrors.IsNotFound(err) {
			// No grant to revoke → honest 404, not a false success (there is nothing
			// to re-consent away). The caller learns they had no grant here.
			writeError(w, http.StatusNotFound, "no per-user grant exists for this server")
			return
		}
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "revoke MCP per-user grant failed", "server", server)
		writeError(w, http.StatusInternalServerError, "failed to revoke the per-user grant")
		return
	}

	s.grantAudit().record(grantAuditEntry{
		action:    grantActionRevoke,
		server:    server,
		userHash:  userHash,
		namespace: ns,
	})
	// username is the precise caller (resolved above); use it directly as the audit actor.
	s.appendAudit(r.Context(), auditlog.Entry{
		Actor: username, Action: auditActionGrantRevoke,
		ResourceKind: resourceKindMCPGrant, ResourceName: server, Namespace: ns,
		Detail: map[string]any{detailKeyUserHash: userHash},
	})

	writeJSON(w, http.StatusOK, MCPGrantResponse{
		Status:    "revoked",
		Server:    server,
		Namespace: ns,
		User:      userHash,
	})
}

// upsertGrantSecret creates the grant Secret, or updates it in place when one
// already exists for the SAME (user, server) — a re-consent rotates the stored
// tokens rather than failing on AlreadyExists. Both paths run caller-scoped.
func (s *Server) upsertGrantSecret(ctx context.Context, caller client.Client, secret *corev1.Secret) error {
	if err := caller.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		var existing corev1.Secret
		if gErr := caller.Get(ctx, client.ObjectKeyFromObject(secret), &existing); gErr != nil {
			return gErr
		}
		existing.Labels = secret.Labels
		existing.Annotations = secret.Annotations
		existing.Data = secret.Data
		return caller.Update(ctx, &existing)
	}
	return nil
}

// confirmOAuthServer confirms the named server is a registered OAuth server and
// recovers its OAuth client config + URL from the register-time annotations (ADR
// 0031), reading the register-managed ToolRegistry CALLER-SCOPED. A non-OAuth
// server, an absent server, or one the caller cannot read → an honest 4xx (never a
// silently-started consent for a server that cannot use one). The recovered config's
// fields may be EMPTY for a legacy server registered before config persistence — the
// caller then overlays an explicitly-supplied config (or fails validation), so a
// one-click Playground connect works for new servers and legacy stays connectable.
func (s *Server) recoverRegisteredOAuth(ctx context.Context, caller client.Client, ns, server string) (mcpOAuthConfig, string, *createError) {
	tr, err := s.mcpGetToolRegistry(ctx, caller, ns, server)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return mcpOAuthConfig{}, "", &createError{status: http.StatusNotFound, msg: "no such registered MCP server"}
		}
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			return mcpOAuthConfig{}, "", &createError{status: status, msg: msg}
		}
		return mcpOAuthConfig{}, "", &createError{status: http.StatusInternalServerError, msg: "failed to read the MCP server"}
	}
	if tr.Annotations[annMCPAuthType] != oauthAuthType {
		return mcpOAuthConfig{}, "", &createError{status: http.StatusBadRequest, msg: "this MCP server is not an OAuth server; per-user consent does not apply"}
	}
	cfg := mcpOAuthConfig{
		AuthorizationEndpoint: tr.Annotations[annMCPOAuthAuthEndpoint],
		TokenEndpoint:         tr.Annotations[annMCPOAuthTokenEndpoint],
		ClientID:              tr.Annotations[annMCPOAuthClientID],
		Scope:                 tr.Annotations[annMCPOAuthScope],
		RedirectURI:           tr.Annotations[annMCPOAuthRedirectURI],
	}
	return cfg, tr.Annotations[annMCPURL], nil
}

// overlayOAuthConfig overlays any non-empty fields of an explicitly-supplied auth block
// over the recovered base. It lets a legacy server (no persisted config) still be
// connected by supplying the config, and lets a caller override a single field (e.g. its
// own origin's redirectUri). A nil auth block is a no-op.
func overlayOAuthConfig(base mcpOAuthConfig, auth *MCPAuthRequest) mcpOAuthConfig {
	if auth == nil {
		return base
	}
	if v := strings.TrimSpace(auth.AuthorizationEndpoint); v != "" {
		base.AuthorizationEndpoint = v
	}
	if v := strings.TrimSpace(auth.TokenEndpoint); v != "" {
		base.TokenEndpoint = v
	}
	if v := strings.TrimSpace(auth.ClientID); v != "" {
		base.ClientID = v
	}
	if v := strings.TrimSpace(auth.Scope); v != "" {
		base.Scope = v
	}
	if v := strings.TrimSpace(auth.RedirectURI); v != "" {
		base.RedirectURI = v
	}
	return base
}

// oauthConfigIncomplete reports whether the recovered config is missing an endpoint or the
// client id — the case for a server registered before config persistence (m26.1b), which
// then needs a discovery/DCR backfill before consent can begin.
func oauthConfigIncomplete(c mcpOAuthConfig) bool {
	return strings.TrimSpace(c.AuthorizationEndpoint) == "" ||
		strings.TrimSpace(c.TokenEndpoint) == "" ||
		strings.TrimSpace(c.ClientID) == ""
}

// fillEmptyOAuthConfig fills base's EMPTY fields from src, keeping base's non-empty values
// (e.g. a caller-supplied redirectURI). Used to complete a legacy server's recovered config
// with the discovered endpoints + client id.
func fillEmptyOAuthConfig(base, src mcpOAuthConfig) mcpOAuthConfig {
	if strings.TrimSpace(base.AuthorizationEndpoint) == "" {
		base.AuthorizationEndpoint = src.AuthorizationEndpoint
	}
	if strings.TrimSpace(base.TokenEndpoint) == "" {
		base.TokenEndpoint = src.TokenEndpoint
	}
	if strings.TrimSpace(base.ClientID) == "" {
		base.ClientID = src.ClientID
	}
	if strings.TrimSpace(base.Scope) == "" {
		base.Scope = src.Scope
	}
	if strings.TrimSpace(base.RedirectURI) == "" {
		base.RedirectURI = src.RedirectURI
	}
	return base
}

// backfillMCPOAuthConfig persists the recovered OAuth client config as the annMCPOAuth*
// annotations on the server's ToolRegistry (m26.1b), so a legacy server's inline connect
// recovers from annotations next time instead of re-running discovery/DCR. Best-effort: a
// failed read/write is logged, never fatal — the current consent still proceeds. Runs
// caller-scoped (a viewer who cannot update the registry just skips the backfill).
func (s *Server) backfillMCPOAuthConfig(ctx context.Context, caller client.Client, ns, server string, cfg mcpOAuthConfig) {
	tr, err := s.mcpGetToolRegistry(ctx, caller, ns, server)
	if err != nil {
		s.log.Info("oauth-config backfill skipped: could not read ToolRegistry", "server", server)
		return
	}
	if tr.Annotations == nil {
		tr.Annotations = map[string]string{}
	}
	set := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			tr.Annotations[k] = v
		}
	}
	set(annMCPOAuthAuthEndpoint, cfg.AuthorizationEndpoint)
	set(annMCPOAuthTokenEndpoint, cfg.TokenEndpoint)
	set(annMCPOAuthClientID, cfg.ClientID)
	set(annMCPOAuthScope, cfg.Scope)
	set(annMCPOAuthRedirectURI, cfg.RedirectURI)
	// Persist to the store — best-effort throughout (a failed authz/write just skips
	// the backfill; the current consent still proceeds) behind SSAR VerbUpdate (a
	// caller who cannot update just skips, as on the old CRD path). ADR 0044.
	if aErr := s.authorizeStore(ctx, caller, authz.VerbUpdate, resourceToolRegistries, ns, server); aErr != nil {
		s.log.Info("oauth-config backfill skipped: not permitted to update (non-fatal)", "server", server)
		return
	}
	if _, uErr := s.toolRegistryStore.Upsert(ctx, crdToolRegistryToStore(tr)); uErr != nil {
		s.log.Info("oauth-config backfill skipped: could not persist to store (non-fatal)", "server", server)
	}
}

// grantAudit returns the server's grant auditor (constructed on demand over the
// server logger). Kept as a helper so every grant create/use/revoke audits through
// ONE place — the auditor never carries a token.
func (s *Server) grantAudit() *grantAuditor {
	return newGrantAuditor(s.log)
}
