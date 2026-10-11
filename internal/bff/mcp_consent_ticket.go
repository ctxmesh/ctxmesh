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
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// Consent tickets (ADR 0040 amendment, m184.42). The binding cookie must land on the console
// origin, where the one registered callback runs, but a chatbox on an agent's own hostname
// cannot set it there. So the chatbox mints a ticket on its own origin, hands it by postMessage
// to the console's /connect page, and that page redeems it on the console origin, where the flow
// begins and the cookie is set in this browser.
//
// The ticket carries the minter's identity, so whoever delivers it to /connect binds the
// minter's flow to the victim's browser. Two checks keep that delivery inside the platform:
// the redeem must come from the console origin (only /connect calls it), and the origin that
// delivered the ticket to /connect must be the hostname of the agent the ticket names, under the
// console's own domain. An attacker can mint a ticket for their own agent, but cannot serve a
// page at that agent's hostname.

// consentTicketTTL is how long a minted ticket may wait for its redeem: long enough for a popup
// to load, short enough that a leaked ticket is useless.
const consentTicketTTL = 60 * time.Second

// connectPagePath is the console page that redeems a consent ticket.
const connectPagePath = "/connect"

// consentTicket is a minted, not yet redeemed consent: everything prepareGrantConsent resolved
// for the minter, so the redeem (which carries no caller credential) begins exactly that flow.
type consentTicket struct {
	CallerToken   string         `json:"callerToken"`
	GrantUserHash string         `json:"grantUserHash"`
	OAuth         mcpOAuthConfig `json:"oauth"`
	Server        string         `json:"server"`
	Namespace     string         `json:"namespace"`
	ServerURL     string         `json:"serverUrl"`
	Boundary      string         `json:"boundary,omitempty"`
	// Agent is the agent whose hostname must deliver the ticket; it runs in Namespace.
	Agent string `json:"agent"`
}

// handleMintConsentTicket serves POST /api/mcp/oauth/ticket (allowlisted at agent origins). It
// runs the same validation as beginning consent, stores a single-use ticket for consentTicketTTL,
// and returns it with the console's /connect URL. No flow starts and no cookie is set here.
func (s *Server) handleMintConsentTicket(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.consoleURL == "" {
		writeError(w, http.StatusNotImplemented,
			"consent tickets need the console origin (bff.consoleURL); begin consent with POST /api/mcp/oauth/grant")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req MCPConsentTicketRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}
	agent := strings.TrimSpace(req.Agent)
	if strings.TrimSpace(req.Server) == "" {
		writeError(w, http.StatusBadRequest, "server is required (the registered MCP server to consent to)")
		return
	}
	if agent == "" {
		writeError(w, http.StatusBadRequest, "agent is required (a consent ticket is redeemed only from that agent's hostname)")
		return
	}

	plan, pErr := s.prepareGrantConsent(r, caller, MCPGrantConsentRequest{
		Server: req.Server, Namespace: req.Namespace, Agent: agent,
	})
	if pErr != nil {
		writeError(w, pErr.status, pErr.msg)
		return
	}

	ticket, err := randToken(32)
	if err != nil {
		s.log.Error(err, "mint MCP consent ticket failed")
		writeError(w, http.StatusInternalServerError, "failed to start the consent flow")
		return
	}
	if err := s.oauthFlows.putTicket(r.Context(), ticket, consentTicket{
		CallerToken:   bearerToken(r),
		GrantUserHash: plan.userHash,
		OAuth:         plan.cfg,
		Server:        plan.server,
		Namespace:     plan.namespace,
		ServerURL:     plan.serverURL,
		Boundary:      plan.boundary,
		Agent:         agent,
	}, s.oauthFlows.now().Add(consentTicketTTL)); err != nil {
		s.log.Error(err, "store MCP consent ticket failed", "server", plan.server)
		writeError(w, http.StatusInternalServerError, "failed to start the consent flow")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, MCPConsentTicketResponse{
		Ticket:     ticket,
		ConnectURL: s.consoleURL + connectPagePath,
		ExpiresIn:  int(consentTicketTTL / time.Second),
	})
}

// handleRedeemConsentTicket serves POST /api/mcp/oauth/grant/redeem on the console origin,
// outside requireAuth: the ticket is the credential. It takes the ticket (single use, whatever
// follows), checks where it came from, begins the minter's flow with the console callback as
// redirect_uri, and sets the binding cookie on this response, in this browser.
func (s *Server) handleRedeemConsentTicket(w http.ResponseWriter, r *http.Request) {
	if s.consoleURL == "" {
		writeError(w, http.StatusNotImplemented, "consent tickets need the console origin (bff.consoleURL)")
		return
	}
	// Only the console's own /connect page redeems. A page elsewhere that posted here from the
	// victim's browser could otherwise claim any openerOrigin it liked.
	if !sameOrigin(r.Header.Get("Origin"), s.consoleURL) {
		writeError(w, http.StatusForbidden, "a consent ticket is redeemed only by the console's connect page")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req MCPConsentRedeemRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}
	if strings.TrimSpace(req.Ticket) == "" {
		writeError(w, http.StatusBadRequest, "ticket is required")
		return
	}

	t, ok, err := s.oauthFlows.takeTicket(r.Context(), strings.TrimSpace(req.Ticket))
	if err != nil {
		s.log.Error(err, "read MCP consent ticket failed")
		writeError(w, http.StatusInternalServerError, "failed to read the consent ticket")
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "this connect request has expired or was already used (press Connect again)")
		return
	}
	opener, allowed := s.consentOpenerAllowed(req.OpenerOrigin, t.Agent, t.Namespace)
	if !allowed {
		writeError(w, http.StatusForbidden, "this connect request did not come from the chat of the agent it was started for")
		return
	}

	cfg := t.OAuth
	cfg.RedirectURI = s.consoleURL + mcpOAuthCallbackPath
	authURL, state, err := s.startOAuthFlow(r.Context(), cfg, pendingOAuthFlow{
		callerToken:   t.CallerToken,
		serverName:    t.Server,
		namespace:     t.Namespace,
		serverURL:     t.ServerURL,
		grantUserHash: t.GrantUserHash,
		boundary:      t.Boundary,
		openerOrigin:  opener,
	})
	if err != nil {
		s.log.Error(err, "start MCP grant consent flow from a ticket failed", "server", t.Server)
		writeError(w, http.StatusInternalServerError, "failed to start the consent flow")
		return
	}
	setOAuthBindingCookie(w, r, state)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, MCPConsentRedeemResponse{
		Status:           oauthStatusAuthorizationRequired,
		AuthorizationURL: authURL,
	})
}

// sameOrigin reports whether origin (an Origin header) is the origin of base, comparing scheme
// and host:port.
func sameOrigin(origin, base string) bool {
	o, oErr := url.Parse(strings.TrimSpace(origin))
	b, bErr := url.Parse(base)
	if oErr != nil || bErr != nil || o.Host == "" {
		return false
	}
	return strings.EqualFold(o.Scheme, b.Scheme) && strings.EqualFold(o.Host, b.Host)
}

// consentOpenerAllowed reports whether opener (the origin that handed /connect the ticket) is the
// hostname of agent in ns, and returns it normalized. The host must be <agent>.<ns>.<base> with
// the console's scheme, where base is the domain agent hostnames share with the console
// (agentHostBase). The port is not compared.
func (s *Server) consentOpenerAllowed(opener, agent, ns string) (string, bool) {
	o, err := url.Parse(strings.TrimSpace(opener))
	if err != nil || o.Host == "" || o.User != nil || o.Opaque != "" ||
		(o.Path != "" && o.Path != "/") || o.RawQuery != "" || o.Fragment != "" {
		return "", false
	}
	c, err := url.Parse(s.consoleURL)
	if err != nil || !strings.EqualFold(o.Scheme, c.Scheme) {
		return "", false
	}
	labels := strings.SplitN(strings.ToLower(o.Hostname()), ".", 3)
	if len(labels) != 3 || labels[0] != strings.ToLower(agent) || labels[1] != strings.ToLower(ns) {
		return "", false
	}
	if !agentHostBaseAllows(strings.ToLower(c.Hostname()), labels[2]) {
		return "", false
	}
	return strings.ToLower(o.Scheme) + "://" + strings.ToLower(o.Host), true
}

// agentHostBaseAllows reports whether rest, the part of an opener host after <agent>.<ns>., is the
// domain agent hostnames have for this console host.
//
// The shipped edge serves the console at console.<baseDomain> and agents at
// <agent>.<ns>.<baseDomain> (deploy/edge), so the base is the console host minus its first label,
// and it must match exactly: a page under any other domain, or a deeper subdomain of the base, is
// not one the platform serves. A console at the apex of a registrable domain (its parent is a
// public suffix) has agents directly under itself. A loopback console has agent hostnames that
// resolve to loopback by construction: <ip>.sslip.io or <ip>.nip.io for a loopback IPv4, or
// localhost. A console at any other IP names no agent domain.
func agentHostBaseAllows(consoleHost, rest string) bool {
	if consoleHost == hostLocalhost {
		return loopbackAgentBase(rest)
	}
	if ip := net.ParseIP(consoleHost); ip != nil {
		return ip.IsLoopback() && loopbackAgentBase(rest)
	}
	base := consoleHost
	if _, parent, found := strings.Cut(consoleHost, "."); found {
		if suffix, _ := publicsuffix.PublicSuffix(parent); suffix != parent {
			base = parent
		}
	}
	return rest == base
}

// loopbackAgentBase reports whether rest resolves to a loopback address by construction.
func loopbackAgentBase(rest string) bool {
	if rest == hostLocalhost {
		return true
	}
	for _, wildcard := range []string{".sslip.io", ".nip.io"} {
		if ipPart, found := strings.CutSuffix(rest, wildcard); found {
			ip := net.ParseIP(ipPart)
			return ip != nil && ip.To4() != nil && ip.IsLoopback()
		}
	}
	return false
}
