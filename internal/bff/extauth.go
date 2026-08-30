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
	"net/http"
	"strings"

	"github.com/ctxmesh/agentry/internal/runcap"
)

// registerExtAuthRoutes wires the Envoy ext-auth endpoint (ADR 0039) — only when the caller-client
// seam AND the capability signer are present (a token-authenticated hit to an agent URL gets OBO).
func (s *Server) registerExtAuthRoutes(authed *http.ServeMux) {
	if s.callerClients == nil || s.capabilitySigner == nil {
		return
	}
	// Under /api/ so it reaches the authenticated mux (api.Handle("/api/", requireAuth(authed)))
	// — requireAuth 401s a missing/invalid token before the handler, which is exactly the deny
	// signal Envoy needs.
	//
	// Envoy's HTTP ext_authz PREPENDS its path_prefix ("/api/extauth") to the ORIGINAL request path,
	// so the auth request arrives as "/api/extauth" + "<original path>" (e.g. /api/extauth/invoke for
	// a client POST /invoke). Match the exact path (direct calls) AND the whole subtree (the Envoy
	// case); the handler is path-agnostic — it derives the agent from the forwarded host, not the URL.
	// No method constraint: Envoy replays the caller's method, whatever it is.
	authed.HandleFunc("/api/extauth", s.handleExtAuth)
	authed.HandleFunc("/api/extauth/", s.handleExtAuth)
}

// handleExtAuth is the Envoy Gateway HTTP ext-auth endpoint (ADR 0039). Envoy calls it per request
// to an AGENT hostname; it AUTHENTICATES the caller (resolves the caller's identity via a
// SelfSubjectReview — a missing OR invalid token 401s and Envoy denies), derives the target agent
// from the forwarded host, mints the run capability for that user + the agent's trust boundary
// (identical to POST /api/invoke), and returns it in a response header Envoy injects UPSTREAM. A 200
// (with or without the header) allows; a 401 denies.
//
// This gives a token-authenticated hit to an AGENT URL the same OBO the console front door has — the
// agent still only RELAYS the capability (never forges it), so the ADR 0033 model holds end to end.
func (s *Server) handleExtAuth(w http.ResponseWriter, r *http.Request) {
	// Derive the target agent from the ORIGINAL request host Envoy forwards (X-Forwarded-Host, or the
	// Host it preserved). Identity is host-derived either way.
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	agent, ns := parseAgentFromHost(host)

	// M137/EU1b (ADR 0106): if the target agent's tenant has an end-user IdP and the bearer verifies
	// against it, authenticate as an END-USER — mint the runcap with the end-user principal + the
	// standalone boundary, WITHOUT ever building a K8s client (structural K8s-path separation: an
	// end-user bearer never reaches a TokenReview). A console/forged bearer fails verification and falls
	// through to the K8s path below (where a forged token 401s). The end-user's ID token is stripped
	// upstream of the agent pod by the SecurityPolicy (it never sees a K8s or an OIDC bearer).
	if agent != "" {
		if principal, _, isEndUser, _ := s.resolveEndUserPrincipal(r.Context(), r, ns); isEndUser {
			// Two-key exposure gate (ADR 0107): the agent must opt into end-user access
			// (spec.endUserAccess → a mirror row). A verified end-user hitting a NON-exposed agent is
			// authenticated-but-not-authorized → 403 (Envoy denies) — never fall through to the K8s path
			// (it IS a verified end-user, not a console token).
			if !s.endUserAgentExposed(r.Context(), ns, agent) {
				writeError(w, http.StatusForbidden, "this agent is not available to end users")
				return
			}
			if token, minted := s.mintRunCapability(principal, ns, agent, endUserAgentBoundary(ns, agent), ""); minted {
				w.Header().Set(runcap.HeaderName, token)
			}
			w.WriteHeader(http.StatusOK) // allow (end-user authenticated + agent exposed)
			return
		}
	}

	// Console/K8s path (ADR 0039, unchanged). callerClient rejects a MISSING token (401); an invalid
	// token is validated below.
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	// AUTHENTICATE before allowing (audit SEC-2). callerClient builds a LAZY, unvalidated client for
	// any non-empty token — it does not touch the API server. Resolve the caller's identity now (a
	// SelfSubjectReview, the real token validation) and DENY (401) when it fails, so a bogus bearer
	// can no longer invoke an agent by falling through to allow. Only the CAPABILITY below is
	// best-effort; the authentication here is NOT.
	username, err := callerUsername(r.Context(), caller)
	if err != nil || strings.TrimSpace(username) == "" {
		writeError(w, http.StatusUnauthorized, "invalid or unresolvable caller token")
		return
	}
	// Mint + inject the capability. If the host isn't a resolvable agent, or the (authenticated)
	// identity has no grant for this boundary, ALLOW WITHOUT a capability — the run proceeds unattended
	// (org/public creds only), never another user's grant (ADR 0033).
	if agent != "" {
		boundary := agentBoundary(r.Context(), caller, ns, agent)
		if token, minted := s.mintRunCapability(username, ns, agent, boundary, ""); minted {
			w.Header().Set(runcap.HeaderName, token)
		}
	}
	w.WriteHeader(http.StatusOK) // allow (authenticated)
}

// parseAgentFromHost parses an agent hostname "<agent>.<ns>.<baseDomain>" into (agent, namespace):
// the first two DNS labels are the agent + its namespace (Knative's namespace-scoped domain); the
// rest is the base domain and is irrelevant to identity. Returns ("", "") when the host has fewer
// than two labels (not an agent host we can resolve).
func parseAgentFromHost(host string) (agent, ns string) {
	host = strings.TrimSpace(host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i] // strip :port
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 || labels[0] == "" || labels[1] == "" {
		return "", ""
	}
	return labels[0], labels[1]
}
