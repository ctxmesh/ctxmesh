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
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ctxmesh/agentry/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/agentry/internal/credresolve"
	"github.com/ctxmesh/agentry/internal/enduseroidc"
)

// errEndUserBearerRejected signals that a bearer WAS presented at an end-user-enabled agent origin and
// FAILED verification (forged / expired / wrong-issuer). It is deliberately distinct from a nil-error
// ok=false result (no bearer, or the namespace has no enabled end-user IdP → a genuine console request
// that falls through to the caller-scoped K8s path): a rejected end-user bearer is a definite auth
// failure the end-user path OWNS, so the caller returns 401 (re-authenticate) instead of falling through
// to the console path — whose "agent is required" 400 an end-user chat body (carrying no agent) would
// otherwise hit, masking the real cause and leaving the SPA no signal to re-login (ADR 0107 §3).
var errEndUserBearerRejected = errors.New("bff: end-user bearer rejected")

// endUserTokenVerifier verifies an end-user OIDC ID token against a tenant issuer (M137/EU1b). The
// concrete impl is *enduseroidc.Verifier; the interface lets tests inject a fake.
type endUserTokenVerifier interface {
	Verify(ctx context.Context, issuer, clientID, rawIDToken string) (enduseroidc.Identity, error)
}

// endUserPrincipalPrefix tags a persisted principal (the CallerUsername column) as a verified end-user
// identity rather than a K8s username (M137/EU1b, ADR 0106 §4-5). The worker re-mint, handoff, and the
// run-ownership gate all treat it as an opaque owner string; only the mint path parses it.
const endUserPrincipalPrefix = "oidc:"

// endUserPrincipal builds the persisted principal for a verified end-user: "oidc:<iss>#<sub>".
func endUserPrincipal(issuer, subject string) string {
	return endUserPrincipalPrefix + issuer + "#" + subject
}

// parseEndUserPrincipal splits an "oidc:<iss>#<sub>" principal back into (issuer, subject). An OIDC
// issuer is a base URL and never contains '#', so the FIRST '#' after the prefix is the separator (a
// subject MAY contain '#'). ok=false for a plain K8s username (no prefix / no separator).
func parseEndUserPrincipal(principal string) (issuer, subject string, ok bool) {
	rest, hasPrefix := strings.CutPrefix(principal, endUserPrincipalPrefix)
	if !hasPrefix {
		return "", "", false
	}
	// The FIRST '#' separates the issuer (a URL, no '#') from the subject (which may contain '#').
	iss, sub, found := strings.Cut(rest, "#")
	if !found {
		return "", "", false
	}
	return iss, sub, true
}

// principalGrantHash derives the run-capability subject for a principal: EndUserHash for an end-user
// (oidc:) principal, userGrantHash for a K8s username (M137/EU1b, ADR 0106 §5). isEndUser tells the
// caller to enforce the mandatory-HMAC-key rule for end-user identities.
func principalGrantHash(principal string) (hash string, isEndUser bool) {
	if issuer, subject, ok := parseEndUserPrincipal(principal); ok {
		var key []byte
		if p := grantHMACKey.Load(); p != nil {
			key = *p
		}
		return credresolve.EndUserHash(key, issuer, subject), true
	}
	return userGrantHash(principal), false
}

// hmacKeyConfigured reports whether a non-empty grant HMAC key is set — MANDATORY before minting an
// end-user capability (an unsalted end-user hash is enumerable, ADR 0106 §5).
func hmacKeyConfigured() bool {
	p := grantHMACKey.Load()
	return p != nil && len(*p) > 0
}

// EndUserAuthConfigResponse advertises a tenant's end-user OIDC config for the standalone /chat SPA to
// start Auth-Code+PKCE (M137/EU1b, ADR 0106 §9). No secret — a public PKCE client id only.
type EndUserAuthConfigResponse struct {
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"clientId"`
	Scopes   []string `json:"scopes,omitempty"`
}

// handleEndUserAuthConfig serves GET /api/end-user-auth-config (unauthenticated, M137/EU1b): the target
// tenant's end-user OIDC issuer + public PKCE client id, so the agent-origin SPA can log the end-user in
// against the tenant's IdP. The namespace is HOST-derived (the SPA is served on the agent's own origin);
// a `?ns=` query is the fallback for a direct call. A uniform 404 when the ns has no enabled end-user IdP
// (or does not exist) — no tenant-existence oracle (ADR 0106 §9), no secret emitted.
func (s *Server) handleEndUserAuthConfig(w http.ResponseWriter, r *http.Request) {
	if s.endUserThrottled(w, r) {
		return
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	_, ns := parseAgentFromHost(host)
	if ns == "" {
		ns = strings.TrimSpace(r.URL.Query().Get("ns"))
	}
	cfg, err := s.resolveEndUserIdentity(r.Context(), ns)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not resolve end-user auth config")
		return
	}
	if cfg == nil {
		writeError(w, http.StatusNotFound, "not found") // uniform — no oracle for a non-enabled/nonexistent ns
		return
	}
	writeJSON(w, http.StatusOK, EndUserAuthConfigResponse{Issuer: cfg.Issuer, ClientID: cfg.ClientID, Scopes: cfg.Scopes})
}

// endUserAgentExposed reports whether the agent opted into end-user access (spec.endUserAccess → a
// mirror row, ADR 0107 — the SECOND of the two keys, after the tenant's endUserIdentity). Fail-closed: a
// nil store, a lookup error, or no row ⇒ NOT exposed (an internal agent is never end-user-reachable just
// because its tenant enabled end-user login).
func (s *Server) endUserAgentExposed(ctx context.Context, ns, agent string) bool {
	if s.endUserAgentStore == nil {
		return false
	}
	_, ok, err := s.endUserAgentStore.Get(ctx, ns, agent)
	if err != nil {
		s.log.Error(err, "end-user agent exposure lookup failed (fail-closed)", "namespace", ns, "agent", agent)
		return false
	}
	return ok
}

// endUserThrottled applies the per-IP pre-auth limiter to an UNAUTHENTICATED end-user surface. It returns
// true (and writes a non-oracle 429) when the client IP is over budget — bounding a flood of tenant-IdP
// config probes or attacker-supplied token-verification attempts (M137/EU1c, ADR 0107). It reveals
// nothing about tenants or tokens. A nil/disabled limiter always admits.
func (s *Server) endUserThrottled(w http.ResponseWriter, r *http.Request) bool {
	if s.endUserLimiter.allow(clientIP(r)) {
		return false
	}
	writeError(w, http.StatusTooManyRequests, "too many requests")
	return true
}

// resolveEndUserPrincipal attempts to authenticate the request's bearer as an END-USER of the agent in
// ns (M137/EU1b, ADR 0106 §2-3). It resolves the target tenant's end-user IdP config and verifies the
// bearer against it — WITHOUT ever constructing a caller-scoped K8s client (the structural separation:
// an end-user bearer never reaches a K8s TokenReview). Returns ok=false when ns has no enabled end-user
// IdP or no verifier is configured or no bearer is present — the caller then falls through to the K8s
// console path. But when a bearer IS presented against an enabled end-user IdP and fails verification,
// it returns errEndUserBearerRejected so the caller can 401 (the end-user path owns that auth failure).
func (s *Server) resolveEndUserPrincipal(ctx context.Context, r *http.Request, ns string) (principal string, id enduseroidc.Identity, ok bool, err error) {
	cfg, rErr := s.resolveEndUserIdentity(ctx, ns)
	if rErr != nil || cfg == nil {
		return "", enduseroidc.Identity{}, false, rErr
	}
	if s.endUserVerifier == nil {
		return "", enduseroidc.Identity{}, false, nil
	}
	tok := bearerToken(r)
	if tok == "" {
		return "", enduseroidc.Identity{}, false, nil
	}
	identity, vErr := s.endUserVerifier.Verify(ctx, cfg.Issuer, cfg.ClientID, tok)
	if vErr != nil {
		// A bearer was presented against this tenant's ENABLED end-user IdP and failed verification
		// (forged / expired / wrong-issuer). The end-user path OWNS this auth failure — signal the caller
		// to 401 (re-authenticate) rather than fall through to the console path's "agent is required" 400.
		// We do NOT leak the specific reason (no forgery oracle); the caller decides the response.
		return "", enduseroidc.Identity{}, false, errEndUserBearerRejected
	}
	return endUserPrincipal(identity.Issuer, identity.Subject), identity, true, nil
}

// resolveEndUserIdentity resolves an agent namespace to its tenant's ENABLED end-user OIDC config
// (M137/EU1b, ADR 0106 §2 — route by the TARGET tenant, never by an unverified token claim), or
// (nil, nil) when the namespace has no enabled+complete end-user IdP. The nil result is the fail-CLOSED
// default: /chat then stays console-authenticated and never trusts an unconfigured issuer.
//
// It REFUSES an issuer equal to the console OIDC issuer (ADR 0106 §3b) — belt-and-suspenders over the
// structural K8s-path separation: an end-user token must never gain K8s trust, so a colliding issuer is
// dropped with a loud log, never served. (The cluster service-account issuer joins the refusal set in
// m137.3 with the unified caller resolver.)
func (s *Server) resolveEndUserIdentity(ctx context.Context, ns string) (*namespacetenant.EndUserIdentity, error) {
	if s.namespaceTenantStore == nil || strings.TrimSpace(ns) == "" {
		return nil, nil
	}
	cfg, ok, err := s.namespaceTenantStore.EndUserIdentityForNamespace(ctx, ns)
	if err != nil {
		return nil, err
	}
	if !ok || !cfg.Enabled || strings.TrimSpace(cfg.Issuer) == "" || strings.TrimSpace(cfg.ClientID) == "" {
		return nil, nil
	}
	if s.forbiddenEndUserIssuer(cfg.Issuer) {
		s.log.Error(fmt.Errorf("end-user issuer %q collides with the console OIDC issuer", cfg.Issuer),
			"refusing end-user IdP config (an end-user token must never gain K8s trust)", "namespace", ns)
		return nil, nil
	}
	out := cfg
	return &out, nil
}

// forbiddenEndUserIssuer reports whether an end-user issuer collides with a K8s-trusted issuer — the
// console OIDC issuer OR the cluster service-account issuer (ADR 0106 §3b) — a colliding issuer's tokens
// could gain K8s trust, so it is refused. Comparison is trailing-slash- and case-insensitive; an empty
// issuer is forbidden.
func (s *Server) forbiddenEndUserIssuer(issuer string) bool {
	iss := strings.TrimRight(strings.TrimSpace(issuer), "/")
	if iss == "" {
		return true
	}
	for _, banned := range []string{s.oidcIssuer, s.saIssuer} {
		if b := strings.TrimRight(strings.TrimSpace(banned), "/"); b != "" && strings.EqualFold(iss, b) {
			return true
		}
	}
	return false
}
