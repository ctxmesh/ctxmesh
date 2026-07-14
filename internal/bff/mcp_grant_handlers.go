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

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
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
	if req.Auth == nil || !strings.EqualFold(strings.TrimSpace(req.Auth.Type), oauthAuthType) {
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
//  1. resolves the caller's identity (SelfSubjectReview, caller-scoped) — the grant
//     is keyed to THIS user, never a client-supplied user field;
//  2. confirms the named server is a registered OAuth server (caller-scoped read of
//     its ToolRegistry) — a non-OAuth / absent / unreadable server → an honest 4xx;
//  3. starts the m17.2 Auth-Code + PKCE flow with the supplied OAuth config, marked
//     with the caller's HASHED identity so the shared callback stores a
//     (user, server) grant Secret.
//
// It returns 202 + the authorization URL + state (the m17.2 pending shape) — NO
// token, NO verifier. The request carries the SAME OAuth client config as register
// (endpoints + public client id + redirect); no secret material is in the request
// or the response.
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

	cfg := mcpOAuthConfig{
		AuthorizationEndpoint: strings.TrimSpace(req.Auth.AuthorizationEndpoint),
		TokenEndpoint:         strings.TrimSpace(req.Auth.TokenEndpoint),
		ClientID:              strings.TrimSpace(req.Auth.ClientID),
		Scope:                 strings.TrimSpace(req.Auth.Scope),
		RedirectURI:           strings.TrimSpace(req.Auth.RedirectURI),
	}
	if cErr := cfg.validate(); cErr != nil {
		writeError(w, cErr.status, cErr.msg)
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

	// Confirm the server is a registered OAuth server (caller-scoped). A non-OAuth /
	// absent / unreadable server → an honest 4xx, so a grant is never started for a
	// server that cannot use one. Recover its URL for the callback's grant metadata.
	serverURL, cErr := s.confirmOAuthServer(r.Context(), caller, ns, server)
	if cErr != nil {
		writeError(w, cErr.status, cErr.msg)
		return
	}

	// Start the PKCE flow marked with the caller's HASHED identity + their token
	// (the callback's grant-Secret write runs as this user). The redirect back
	// carries no Authorization header, so the flow carries the identity that gates
	// the write (ADR 0011).
	authURL, state, sErr := s.startOAuthFlow(cfg, pendingOAuthFlow{
		callerToken:   bearerToken(r),
		serverName:    server,
		namespace:     ns,
		serverURL:     serverURL,
		grantUserHash: userGrantHash(username),
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
func (s *Server) completeGrantConsent(ctx context.Context, w http.ResponseWriter, caller client.Client, flow pendingOAuthFlow, toks oauthTokens) {
	// In locked mode the grant lands in the credential namespace with the source ns
	// folded into the coordinates + the source-namespace label; in legacy mode it stays
	// in flow.namespace under the original name (sourceNs label "").
	grantNS, grantName := s.grantCoordinates(flow.namespace, flow.serverName, flow.grantUserHash)
	labels := grantSecretLabels(flow.serverName, flow.grantUserHash, s.grantSourceNSLabel(flow.namespace))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      grantName,
			Namespace: grantNS,
			Labels:    labels,
			// The server URL is non-secret and useful to the refresh path; the tokens
			// are NEVER here — only in Data (oauthSecretData).
			Annotations: map[string]string{annMCPGrantServerURL: flow.serverURL},
		},
		Type: corev1.SecretTypeOpaque,
		Data: oauthSecretData(flow.oauth, toks),
	}

	// Upsert: a re-consent for the SAME (user, server) replaces the stored grant
	// (rotated tokens) rather than failing on AlreadyExists — re-consent is a valid,
	// idempotent user action. The write runs as the privileged credential client in
	// locked mode (tenants have no RBAC there), else caller-scoped (ADR 0011).
	if err := s.upsertGrantSecret(ctx, s.grantClient(caller), secret); err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "store MCP per-user grant failed", "server", flow.serverName)
		writeError(w, http.StatusInternalServerError, "failed to store the per-user grant")
		return
	}

	s.grantAudit().record(grantAuditEntry{
		action:    grantActionCreate,
		server:    flow.serverName,
		userHash:  flow.grantUserHash,
		namespace: flow.namespace,
	})

	// The response carries the (user, server) identity + the server — NEVER a token.
	writeJSON(w, http.StatusCreated, MCPGrantResponse{
		Status:    "granted",
		Server:    flow.serverName,
		Namespace: flow.namespace,
		User:      flow.grantUserHash,
	})
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
	// the privileged credential client, so the delete does too.
	grantNS, grantName := s.grantCoordinates(ns, server, userHash)
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
// returns its URL, reading the register-managed ToolRegistry CALLER-SCOPED. A
// non-OAuth server, an absent server, or one the caller cannot read → an honest
// 4xx (never a silently-started consent for a server that cannot use one). The
// OAuth client config (endpoints + client id) is supplied on the consent request
// (validated in the handler) — the same shape as register — so this only gates
// that per-user consent applies at all.
func (s *Server) confirmOAuthServer(ctx context.Context, caller client.Client, ns, server string) (string, *createError) {
	var tr agentsv1alpha1.ToolRegistry
	if err := caller.Get(ctx, client.ObjectKey{Name: server, Namespace: ns}, &tr); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &createError{status: http.StatusNotFound, msg: "no such registered MCP server"}
		}
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			return "", &createError{status: status, msg: msg}
		}
		return "", &createError{status: http.StatusInternalServerError, msg: "failed to read the MCP server"}
	}
	if tr.Annotations[annMCPAuthType] != oauthAuthType {
		return "", &createError{status: http.StatusBadRequest, msg: "this MCP server is not an OAuth server; per-user consent does not apply"}
	}
	return tr.Annotations[annMCPURL], nil
}

// grantAudit returns the server's grant auditor (constructed on demand over the
// server logger). Kept as a helper so every grant create/use/revoke audits through
// ONE place — the auditor never carries a token.
func (s *Server) grantAudit() *grantAuditor {
	return newGrantAuditor(s.log)
}
