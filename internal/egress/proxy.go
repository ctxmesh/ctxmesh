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

// Package egress is the INJECTING EGRESS SIDECAR (ADR 0030 §1): a per-pod, localhost proxy
// the agent's MCP tool calls are pointed at. For each call it VERIFIES the run capability
// (the invoking user's unforgeable identity), RESOLVES that user's OBO credential from the
// credential plane, INJECTS it as Authorization: Bearer, and forwards to the REAL MCP
// server. The agent (user) container holds NEITHER the token NOR the real URL — a
// prompt-injected agent can call through the sidecar but cannot read the credential.
package egress

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/agent-engine/internal/credresolve"
	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// ProxyConfig configures a Proxy.
type ProxyConfig struct {
	// Verifier checks the run capability against the platform public key + audience.
	Verifier *runcap.Verifier
	// Resolver resolves the invoking user's OBO credential (credresolve.K8sBackend in prod).
	Resolver credresolve.CredentialResolver
	// Namespace is the grant SOURCE namespace — the agent's own namespace (POD_NAMESPACE),
	// the key under which its users' grants were consented.
	Namespace string
	// ExpectedAgent, when non-empty, is the agent identity (ns/name) this sidecar serves; a
	// capability whose `act` (actor) is a DIFFERENT agent is rejected, so a capability minted
	// for agent A can never be redeemed at agent B's sidecar (ADR 0029 §5 scoping).
	ExpectedAgent string
	// Routes maps a server name (the first path segment) to its real upstream + auth type.
	Routes RouteTable
	// Transport is the RoundTripper for the upstream forward (nil ⇒ http.DefaultTransport).
	Transport http.RoundTripper
	// Log is the structured logger.
	Log logr.Logger
}

// Proxy is the sidecar HTTP handler.
type Proxy struct {
	cfg     ProxyConfig
	reverse *httputil.ReverseProxy
}

type (
	targetCtxKey struct{}
	credCtxKey   struct{}
)

// NewProxy builds a Proxy. The single ReverseProxy reads the per-request upstream + injected
// credential from the request context (stashed by ServeHTTP after verify+resolve).
func NewProxy(cfg ProxyConfig) *Proxy {
	p := &Proxy{cfg: cfg}
	p.reverse = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target, _ := pr.In.Context().Value(targetCtxKey{}).(*url.URL)
			if target != nil {
				pr.SetURL(target)
				pr.Out.Host = target.Host
			}
			// The capability is proof of identity for the sidecar ONLY — never leak it to
			// the upstream MCP server. Strip any inbound Authorization and inject ours.
			pr.Out.Header.Del(runcap.HeaderName)
			pr.Out.Header.Del("Authorization")
			if cred, _ := pr.In.Context().Value(credCtxKey{}).(string); cred != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+cred)
			}
		},
		// Stream responses immediately — MCP streamable-http replies as SSE.
		FlushInterval: -1,
		Transport:     cfg.Transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			cfg.Log.Error(err, "egress: upstream forward failed")
			writeError(w, http.StatusBadGateway, "upstream_unreachable", "could not reach the MCP server")
		},
	}
	return p
}

// ServeHTTP resolves the route, verifies the capability, resolves the credential, and
// forwards. It fails CLOSED: no/invalid capability, an unknown route, or an agent-scope
// mismatch is rejected before any upstream call.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, remainder, ok := p.cfg.Routes.routeForPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "no_route", "no egress route for this server")
		return
	}

	// Verify the run capability — the ONLY source of the invoking user's identity. The
	// sidecar never trusts a name the agent supplies.
	token := r.Header.Get(runcap.HeaderName)
	if token == "" {
		// No capability ⇒ an unattended / direct call. Personal OBO needs the invoker's
		// identity; org/public (no-capability) resolution lands in m25.9.
		writeError(w, http.StatusUnauthorized, "no_capability", "this tool call carries no run capability")
		return
	}
	runCap, err := p.cfg.Verifier.Verify(token)
	if err != nil {
		p.cfg.Log.Info("egress: capability rejected", "reason", err.Error(), "server", route.Name)
		writeError(w, http.StatusUnauthorized, "invalid_capability", "the run capability was rejected")
		return
	}
	if p.cfg.ExpectedAgent != "" && runCap.Agent != p.cfg.ExpectedAgent {
		// A capability minted for another agent must not be redeemable here.
		writeError(w, http.StatusForbidden, "agent_mismatch", "the run capability was minted for a different agent")
		return
	}

	// Resolve THIS user's OBO credential for THIS server.
	cred, err := p.cfg.Resolver.Resolve(r.Context(), p.cfg.Namespace, route.Name, runCap.User)
	switch {
	case err == nil:
		// Have the invoking user's fresh token — inject it below.
	case errors.Is(err, credresolve.ErrNoCredential):
		// An open server — forward with no Authorization (cred stays empty).
	case errors.Is(err, credresolve.ErrConsentRequired):
		// The user must connect their own account. Honest structured error (the full
		// consent contract — connect URL + run status — lands in m25.9).
		writeError(w, http.StatusForbidden, "consent_required", "connect your account to use this tool")
		return
	default:
		p.cfg.Log.Error(err, "egress: credential resolution failed", "server", route.Name)
		writeError(w, http.StatusBadGateway, "resolve_failed", "could not resolve the credential")
		return
	}

	// Forward to the real upstream: rewrite the path to strip the /<server> prefix, then let
	// the ReverseProxy inject the credential + strip the capability (see Rewrite).
	forwardURL := *route.Target()
	r.URL.Path = remainder
	ctx := context.WithValue(r.Context(), targetCtxKey{}, &forwardURL)
	ctx = context.WithValue(ctx, credCtxKey{}, cred.Value)
	p.reverse.ServeHTTP(w, r.WithContext(ctx))
}

// errorBody is the sidecar's structured error surface — a machine-readable code + a short
// message. It NEVER carries token material.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeError writes a JSON structured error with the given status.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: code, Message: message})
}
