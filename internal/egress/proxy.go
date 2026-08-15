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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/agent-engine/internal/credresolve"
	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// maxRecordBody bounds how much of a tool request/response the record seam buffers into a fixture,
// so a pathological payload can never balloon a sidecar's memory. A tool call's JSON-RPC message +
// result are small; beyond the cap the capture is skipped for that direction (never a failed
// forward — capture is best-effort). Generous vs a normal tool payload.
const maxRecordBody = 8 << 20 // 8 MiB

// toolCapture is the per-request record-mode state stashed in the request context (captureCtxKey)
// by ServeHTTP and read by the ReverseProxy's ModifyResponse hook. It carries the VERIFIED run id
// the fixture is keyed on (never the relayed header value), the pre-injection request body, and the
// parsed JSON-RPC matchers — so ModifyResponse can capture the verbatim upstream response and hand
// the whole interaction to the recorder. nil / absent ⇒ this call is not being recorded.
type toolCapture struct {
	runID    string
	callID   string
	toolName string
	reqBody  []byte
}

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
	// for agent A can never be redeemed at agent B's sidecar (ADR 0029 §5 scoping). Used as the
	// scoping gate ONLY when ExpectedBoundary is empty (a standalone agent / pre-ADR-0033).
	ExpectedAgent string
	// ExpectedBoundary, when non-empty, is the trust boundary (ADR 0033) this sidecar serves —
	// the agent's registry. It SUPERSEDES the ExpectedAgent gate: a capability is redeemable
	// here iff its `bnd` matches, so teammates in the same registry can act on-behalf-of the
	// user (relayed across A2A, m30.3) while a different registry's capability is rejected. A
	// standalone agent's boundary is unique to it, so this reduces to the per-agent check.
	ExpectedBoundary string
	// Routes maps a server name (the first path segment) to its real upstream + auth type.
	Routes RouteTable
	// Transport is the RoundTripper for the upstream forward (nil ⇒ http.DefaultTransport).
	Transport http.RoundTripper
	// Log is the structured logger.
	Log logr.Logger
	// Recorder, when non-nil, is the M78 record-mode TOOL capture (ADR 0071 §1/C1): every tool
	// call of a RECORDED run (the SDK relays X-Ctxmesh-Record) is captured pre-injection (request
	// body) + verbatim (upstream response) into the run's replay fixture. nil ⇒ record mode off,
	// the capture path is a no-op (zero overhead, the forward is byte-for-byte unchanged).
	Recorder *ToolRecorder
}

// Proxy is the sidecar HTTP handler.
type Proxy struct {
	cfg     ProxyConfig
	reverse *httputil.ReverseProxy
}

type (
	targetCtxKey  struct{}
	credCtxKey    struct{}
	captureCtxKey struct{}
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
		// Record mode (M78, ADR 0071 §1/C1): capture the VERBATIM upstream response bytes for a
		// recorded tool call. The response carries NO credential (the OBO bearer is a REQUEST header,
		// injected by Rewrite; the upstream never echoes it back), so C4 holds. We buffer the body,
		// hand a copy to the recorder, and restore an identical reader so the agent sees byte-for-
		// byte the same response incl. SSE/streamable-http framing. nil-recorder / non-recorded call
		// ⇒ this hook is a no-op (nil capture in the context). Best-effort: a read error skips
		// capture, never fails the forward.
		ModifyResponse: p.captureResponse,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			cfg.Log.Error(err, "egress: upstream forward failed")
			writeError(w, http.StatusBadGateway, "upstream_unreachable", "could not reach the MCP server")
		},
	}
	return p
}

// captureResponse is the ReverseProxy ModifyResponse hook that records a recorded tool call's
// VERBATIM upstream response (M78, ADR 0071 §1/C1). It reads the per-request toolCapture from the
// context (nil ⇒ not a recorded call, no-op), buffers the response body (capped, best-effort),
// restores an identical body reader so the agent receives byte-for-byte the same response, and
// hands the whole interaction (pre-injection request + verbatim response) to the recorder keyed on
// the VERIFIED run id. It never returns an error — a record-path failure must not fail the forward.
func (p *Proxy) captureResponse(resp *http.Response) error {
	cap, _ := resp.Request.Context().Value(captureCtxKey{}).(*toolCapture)
	if cap == nil || p.cfg.Recorder == nil {
		return nil
	}
	body := resp.Body
	if body == nil {
		p.cfg.Recorder.capture(resp.Request.Context(), cap.runID, cap.callID, cap.toolName, cap.reqBody, nil)
		return nil
	}
	buffered, err := io.ReadAll(io.LimitReader(body, maxRecordBody+1))
	_ = body.Close()
	if err != nil || int64(len(buffered)) > maxRecordBody {
		// Could not fully buffer the response (read error or over the cap) — restore what we read so
		// the forward still streams, and skip capture for this call rather than store a truncated
		// response (a partial fixture would mis-replay). The request/response are never mutated.
		resp.Body = io.NopCloser(bytes.NewReader(buffered))
		p.cfg.Log.Info("egress: record: response body not fully buffered — skipping tool capture",
			"run", cap.runID, "tool", cap.toolName)
		return nil
	}
	// Restore an identical reader so the agent sees the response verbatim.
	resp.Body = io.NopCloser(bytes.NewReader(buffered))
	p.cfg.Recorder.capture(resp.Request.Context(), cap.runID, cap.callID, cap.toolName, cap.reqBody, buffered)
	return nil
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
	// Scoping gate (ADR 0033, m30.3): prefer the BOUNDARY check — a capability is redeemable here
	// iff its boundary matches this sidecar's registry, so a teammate's relayed capability is
	// accepted (team-OBO) but a different registry's is not. Fall back to the exact-agent check
	// only when no boundary is configured (a standalone agent, or a pre-ADR-0033 sidecar).
	switch {
	case p.cfg.ExpectedBoundary != "":
		if runCap.Boundary != p.cfg.ExpectedBoundary {
			writeError(w, http.StatusForbidden, "boundary_mismatch", "the run capability is scoped to a different trust boundary")
			return
		}
	case p.cfg.ExpectedAgent != "" && runCap.Agent != p.cfg.ExpectedAgent:
		// A capability minted for another agent must not be redeemable here.
		writeError(w, http.StatusForbidden, "agent_mismatch", "the run capability was minted for a different agent")
		return
	}

	// Resolve THIS user's OBO credential for THIS server, within the run's trust boundary
	// (ADR 0033): the capability carries the boundary (the invoking agent's registry, or the
	// agent when standalone), so an agent resolves only grants scoped to its own boundary.
	cred, err := p.cfg.Resolver.Resolve(r.Context(), p.cfg.Namespace, runCap.Boundary, route.Name, runCap.User)
	switch {
	case err == nil:
		// Have the invoking user's fresh token — inject it below.
	case errors.Is(err, credresolve.ErrNoCredential):
		// An open server — forward with no Authorization (cred stays empty).
	case errors.Is(err, credresolve.ErrConsentRequired):
		// The user must connect their own account (ADR 0029 §2). The structured error names
		// the SERVER so the runtime can surface a "Connect your account for <server>" CTA
		// (m25.9 consent contract) — the SDK maps it to a distinct consent_required outcome.
		writeConsentRequired(w, route.Name)
		return
	default:
		p.cfg.Log.Error(err, "egress: credential resolution failed", "server", route.Name)
		writeError(w, http.StatusBadGateway, "resolve_failed", "could not resolve the credential")
		return
	}

	// ── Record mode (M78, ADR 0071 §1/C1): buffer the request body PRE-INJECTION ──
	// This is the C4-safe capture point: we buffer the agent-visible request BODY here, BEFORE
	// reverse.ServeHTTP injects the OBO bearer (Rewrite sets the Authorization header on the
	// OUTBOUND request; the body is untouched). So the captured request carries no credential by
	// construction. Gated per-run by the X-Ctxmesh-Record header the SDK relays on a recorded
	// call; keyed on the VERIFIED runCap.RunID (never the relayed value) so a forged header cannot
	// mis-key another run's fixture. recorder nil / header absent ⇒ no buffering, zero overhead.
	var capture *toolCapture
	if p.cfg.Recorder != nil && r.Header.Get(RecordHeaderName) != "" && runCap.RunID != "" {
		capture = p.bufferRequestForRecord(r, runCap.RunID)
	}

	// Forward to the real upstream: rewrite the path to strip the /<server> prefix, then let
	// the ReverseProxy inject the credential + strip the capability (see Rewrite).
	forwardURL := *route.Target()
	r.URL.Path = remainder
	ctx := context.WithValue(r.Context(), targetCtxKey{}, &forwardURL)
	ctx = context.WithValue(ctx, credCtxKey{}, cred.Value)
	if capture != nil {
		ctx = context.WithValue(ctx, captureCtxKey{}, capture)
	}
	p.reverse.ServeHTTP(w, r.WithContext(ctx))
}

// bufferRequestForRecord drains + restores the request body so the record seam captures the
// agent-visible request bytes (pre-injection, C4-safe) while the forward still streams the exact
// bytes. It also best-effort parses the JSON-RPC tools/call message for the replay matchers
// (call id + tool name). Returns the capture state to stash in the forward context; nil on a body
// read error (capture is skipped rather than failing the tool call).
func (p *Proxy) bufferRequestForRecord(r *http.Request, runID string) *toolCapture {
	var buffered []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxRecordBody+1))
		_ = r.Body.Close()
		if err != nil || int64(len(b)) > maxRecordBody {
			// Restore what we read so the forward still streams, and skip capture (never fail the
			// call on a record-path read error / oversize body).
			r.Body = io.NopCloser(bytes.NewReader(b))
			p.cfg.Log.Info("egress: record: request body not fully buffered — skipping tool capture", "run", runID)
			return nil
		}
		buffered = b
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	callID, toolName := parseToolCall(buffered)
	return &toolCapture{runID: runID, callID: callID, toolName: toolName, reqBody: buffered}
}

// parseToolCall best-effort extracts the JSON-RPC tool-call id + tool name from an MCP
// tools/call request body for the replay matchers (ADR 0071 §2: primary CallID, fallback
// ToolName+ArgsHash). The body is a JSON-RPC message:
//
//	{"jsonrpc":"2.0","id":<n|str>,"method":"tools/call","params":{"name":"<tool>","arguments":{…}}}
//
// callID is the JSON-RPC id rendered as a string (numbers → their decimal form); toolName is
// params.name. Both degrade to "" on a non-tools/call or unparseable body — replay then falls back
// to name+args-hash / captured-order matching. It NEVER fails the call (the argsBody stored is the
// verbatim request regardless).
func parseToolCall(body []byte) (callID, toolName string) {
	if len(body) == 0 {
		return "", ""
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return "", ""
	}
	if msg.Method != "tools/call" {
		// A handshake / tools-list / notification, not a tool call — no matcher keys.
		return "", ""
	}
	return jsonRPCID(msg.ID), msg.Params.Name
}

// jsonRPCID renders a JSON-RPC id (a JSON number or string) to the stable string form the fixture
// stores as CallID. A string id is unquoted; a numeric id becomes its decimal string; null/absent
// yields "". Best-effort — a shape it cannot render yields "".
func jsonRPCID(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(trimmed, &n) == nil {
		return n.String()
	}
	return ""
}

// errorBody is the sidecar's structured error surface — a machine-readable code + a short
// message (+ the server, for consent_required). It NEVER carries token material.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Server  string `json:"server,omitempty"`
}

// writeError writes a JSON structured error with the given status.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: code, Message: message})
}

// writeConsentRequired writes the structured consent_required error, naming the server the
// user must connect their account to (the m25.9 consent contract).
func writeConsentRequired(w http.ResponseWriter, server string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(errorBody{
		Error:   "consent_required",
		Message: "connect your account to use this tool",
		Server:  server,
	})
}
