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
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
)

// defaultVersion is reported by /api/health when no version is injected at
// build time.
const defaultVersion = "dev"

// indexHTML is the SPA entrypoint filename in the static build (dist/). Named
// once so the fallback path and the direct read reference the same literal.
const indexHTML = "index.html"

// Server is the BFF HTTP server: it serves the static SPA build and the /api
// surface (behind the M11 auth). It composes narrow seams (a CallerClientFactory
// that mints a per-request caller-scoped client-go client, an Authenticator for
// the M11 edge, optional Adapters for m12.5+) so each is independently testable.
//
// ADR 0011: every user-facing CRD read/write runs through a client the
// callerClients factory builds from the CALLER'S bearer token, so the K8s API
// server enforces the caller's own RBAC. The BFF holds no static SA client for
// user CRD ops — the confused-deputy gap is closed by construction.
type Server struct {
	callerClients CallerClientFactory
	scheme        *runtime.Scheme
	auth          Authenticator
	adapters      Adapters
	version       string
	log           logr.Logger

	// providerConnect is the ADR 0015 kill-switch. When false the connect
	// endpoints (POST/GET /api/providers, GET /api/providers/{name}/models) are
	// NOT registered and serve 404 — the UI falls back to reference-existing.
	// Default true (dev/trial); an operator sets it false on hardened installs.
	providerConnect bool
	// providerHTTP is the HTTP client the connect flow uses to validate a key +
	// fetch a provider's model list server-side. Nil → a bounded default client;
	// tests point it at an httptest fake provider.
	providerHTTP *http.Client

	// platformGenerationModels is the operator-pinned list of models the
	// create-from-prompt generation endpoint (ADR 0014) may use — the UI's model
	// dropdown source. Empty (the default) → generation uses the caller's connected
	// provider model with no pin. A values seam like the connect kill-switch (wired
	// from PLATFORM_GENERATION_MODELS in cmd/bff/main.go).
	platformGenerationModels []string

	// mcpEnabled is the BYO-MCP kill-switch (ADR 0016), mirroring the connect
	// kill-switch. When false the register/catalog endpoints (POST/GET
	// /api/mcpservers, GET /api/tools) are NOT registered and 404 — a hardened
	// install that forbids BYO MCP. Default true (dev/trial). Wired from
	// MCP_ENABLED in cmd/bff/main.go (Helm value bff.mcp.enabled).
	mcpEnabled bool
	// mcpRequireApproval is the ADR 0016 TRUST policy switch. Default false
	// (self-serve — a registered server's tools are immediately bindable). When
	// true (hardened) a freshly-registered server's tools are marked
	// pending-approval; the approval queue itself is M17 (here we only mark the
	// state). Wired from MCP_REQUIRE_APPROVAL (Helm value bff.mcp.requireApproval).
	mcpRequireApproval bool

	// static is the filesystem serving the Vite build (dist/). Nil disables
	// static serving (api-only mode, useful in tests).
	static fs.FS
}

// Options configures a Server.
type Options struct {
	// CallerClients mints a per-request client.Client scoped to the caller's
	// bearer token (ADR 0011). Required for the CRD routes (GET/POST /api/agents,
	// GET /api/topology): they run as the caller so K8s RBAC enforces the M11
	// personas. Nil leaves those routes serving 501 (the caller-scoped seam is not
	// wired) — the BFF never falls back to its own SA for user CRD ops.
	CallerClients CallerClientFactory
	// Scheme decodes the expand-emitted CRD manifests into typed objects for the
	// apply path. Required when CallerClients is set (the agent CRDs must be
	// registered so the caller-scoped client can encode them).
	Scheme *runtime.Scheme
	// Auth gates /api requests (the M11 control-plane auth seam). Required.
	Auth Authenticator
	// Adapters are the optional server-side adapters (Langfuse/Prometheus/invoke/
	// expand). Nil entries serve 501 for their routes (m12.5+ wires them).
	Adapters Adapters
	// Version is reported by /api/health.
	Version string
	// StaticDir is the directory of the built SPA (dist/). Empty disables static
	// serving; the SPA is then served elsewhere (e.g. an nginx sidecar).
	StaticDir string
	// ProviderConnect is the ADR 0015 connect-a-provider kill-switch. When true
	// (the default for dev/trial) the connect endpoints are registered; when
	// false (a hardened install) they serve 404 and the UI falls back to
	// reference-existing. Wired from PROVIDER_CONNECT_ENABLED in cmd/bff/main.go.
	ProviderConnect bool
	// ProviderHTTP overrides the HTTP client the connect flow uses to reach the
	// provider (validation + model list + generation chat). Nil uses a bounded
	// default; tests inject a client pointed at an httptest fake provider.
	ProviderHTTP *http.Client
	// PlatformGenerationModels is the operator-pinned generation-model list (ADR
	// 0014) — the UI's model dropdown source. Empty (default) → generation uses the
	// caller's connected-provider model unpinned. Wired from
	// PLATFORM_GENERATION_MODELS in cmd/bff/main.go.
	PlatformGenerationModels []string
	// MCPEnabled is the BYO-MCP kill-switch (ADR 0016). When true (the default for
	// dev/trial) the register/catalog endpoints are registered; when false (a
	// hardened install) POST/GET /api/mcpservers + GET /api/tools 404. Wired from
	// MCP_ENABLED in cmd/bff/main.go.
	MCPEnabled bool
	// MCPRequireApproval is the ADR 0016 trust policy. When false (default,
	// self-serve) a registered server's tools are immediately bindable; when true
	// (hardened) they are marked pending-approval (the approval queue is M17). Wired
	// from MCP_REQUIRE_APPROVAL in cmd/bff/main.go.
	MCPRequireApproval bool
	// Log is the structured logger.
	Log logr.Logger
}

// NewServer builds a Server from Options. It does not start listening; the
// caller mounts Handler() on an http.Server.
func NewServer(opts Options) *Server {
	s := &Server{
		callerClients:            opts.CallerClients,
		scheme:                   opts.Scheme,
		auth:                     opts.Auth,
		adapters:                 opts.Adapters,
		version:                  opts.Version,
		providerConnect:          opts.ProviderConnect,
		providerHTTP:             opts.ProviderHTTP,
		platformGenerationModels: opts.PlatformGenerationModels,
		mcpEnabled:               opts.MCPEnabled,
		mcpRequireApproval:       opts.MCPRequireApproval,
		log:                      opts.Log,
	}
	if s.version == "" {
		s.version = defaultVersion
	}
	if opts.StaticDir != "" {
		s.static = os.DirFS(opts.StaticDir)
	}
	return s
}

// Handler returns the fully-wired http.Handler: the /api mux (auth-gated) plus
// the SPA static handler as the fallback for everything else.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()

	// Health is unauthenticated (liveness/version probe; no cluster access).
	api.HandleFunc("GET /api/health", s.handleHealth)

	// Authenticated surface. The CRD routes run through the CALLER-SCOPED client
	// (ADR 0011): list/create/topology reflect exactly what the caller's own RBAC
	// permits, enforced by the K8s API server. They are wired only when the
	// caller-client factory is configured; honest 501 otherwise (the BFF never
	// falls back to its own SA for user CRD ops — that is the confused-deputy gap
	// this task closes). Create additionally needs the scheme to decode manifests.
	authed := http.NewServeMux()
	if s.callerClients != nil {
		authed.HandleFunc("GET /api/agents", s.handleListAgents)
		authed.HandleFunc("GET /api/topology", s.handleTopology)
		// RBAC-aware chrome (ADR 0012, ui-foundation §3). All three run through the
		// CALLER-SCOPED client — whoami/capabilities are DISPLAY-ONLY (they gate
		// nothing server-side; enforcement stays with K8s, ADR 0011), and namespaces
		// is a caller-scoped list. Gated on the same factor as the CRD routes: nil
		// factory → 501, never a fallback to the BFF SA.
		authed.HandleFunc("GET /api/whoami", s.handleWhoAmI)
		authed.HandleFunc("GET /api/capabilities", s.handleCapabilities)
		authed.HandleFunc("GET /api/namespaces", s.handleNamespaces)
		if s.scheme != nil {
			authed.HandleFunc("POST /api/agents", s.handleCreateAgent)
		} else {
			authed.Handle("POST /api/agents", notImplemented("config-builder apply"))
		}
	} else {
		authed.Handle("GET /api/agents", notImplemented("caller-scoped agent list"))
		authed.Handle("GET /api/topology", notImplemented("caller-scoped topology"))
		authed.Handle("GET /api/whoami", notImplemented("caller-scoped whoami"))
		authed.Handle("GET /api/capabilities", notImplemented("caller-scoped capabilities"))
		authed.Handle("GET /api/namespaces", notImplemented("caller-scoped namespaces"))
		authed.Handle("POST /api/agents", notImplemented("config-builder apply"))
	}

	// Langfuse-backed dashboard routes (recent runs, cost/usage, trace link).
	// Wired when the Langfuse adapter is present; honest 501 otherwise so the
	// routes stay discoverable. Registering the real handler only when wired
	// keeps the nil-adapter seam clean.
	if s.adapters.Langfuse != nil {
		authed.HandleFunc("GET /api/runs", s.handleRuns)
		authed.HandleFunc("GET /api/cost", s.handleCost)
		authed.HandleFunc("GET /api/traces/{id}", s.handleTraceLink)
	} else {
		authed.Handle("GET /api/runs", notImplemented("Langfuse runs adapter"))
		authed.Handle("GET /api/cost", notImplemented("Langfuse cost adapter"))
		authed.Handle("GET /api/traces/", notImplemented("Langfuse trace adapter"))
	}

	// Remaining adapter seams for m12.6–m12.7: mounted now (discoverable) but
	// honest 501 until their adapter is wired.
	if s.adapters.Prometheus == nil {
		authed.Handle("GET /api/metrics/", notImplemented("Prometheus adapter"))
	}
	// Playground invoke (m12.7): run a deployed agent, traced, and return its
	// traceId. Wired only when BOTH the InvokeAdapter (the pure-HTTP invoker) AND
	// the caller-client factory are present — the run is CALLER-SCOPED (the agent
	// lookup + dispatch act as the caller, ADR 0011), so it needs the caller-client
	// seam and must never fall back to the BFF SA. Honest 501 otherwise.
	if s.adapters.Invoke != nil && s.callerClients != nil {
		authed.HandleFunc("POST /api/invoke", s.handleInvoke)
	} else {
		authed.Handle("POST /api/invoke", notImplemented("Playground invoke"))
	}
	// Config-builder expand preview (m12.6): agent.yaml → CRD manifest(s). Wired
	// when the ExpandAdapter is present (it reuses the CLI expand core server-side);
	// honest 501 otherwise.
	if s.adapters.Expand != nil {
		authed.HandleFunc("POST /api/expand", s.handleExpand)
	} else {
		authed.Handle("POST /api/expand", notImplemented("config expand"))
	}

	// Create-from-prompt generation (m14.5, ADR 0014): a natural-language
	// description → a server-side chat/completions call through the CALLER'S
	// connected provider → the emitted simplified agent.yaml validated by the SAME
	// expand core → returned for REVIEW (never auto-applied). It needs BOTH seams:
	//   - the CALLER-CLIENT factory — the generation key is resolved caller-scoped
	//     (route → SecretBinding → Secret), so a viewer denied the Secret is a 403
	//     and the BFF never falls back to its own SA (ADR 0011); AND
	//   - the EXPAND adapter — the emitted YAML is validated through the one mapping
	//     (no divergent generator schema, ADR 0014).
	// Either missing → an honest 501. The generation chat reuses providerHTTP (nil
	// → a bounded default client).
	if s.callerClients != nil && s.adapters.Expand != nil {
		authed.HandleFunc("POST /api/agents/generate", s.handleGenerate)
	} else {
		authed.Handle("POST /api/agents/generate", notImplemented("create-from-prompt generation"))
	}

	// Connect-a-provider (m14.4, ADR 0015): validate a pasted key server-side and
	// create Secret + SecretBinding + ModelRoute with the CALLER'S client. Gated by
	// TWO factors, in order:
	//
	//  1. the Helm KILL-SWITCH (providerConnect). OFF → the routes are NOT
	//     registered, so they fall through to the SPA handler and 404 (feature-off);
	//     the UI then falls back to reference-existing. This is checked FIRST so a
	//     hardened install makes the endpoints genuinely absent.
	//  2. the CALLER-CLIENT factory (like every user-facing CRD route): nil → 501,
	//     never a BFF-SA fallback (ADR 0011).
	//
	// When both are satisfied the three connect endpoints run caller-scoped.
	if s.providerConnect {
		if s.callerClients != nil {
			authed.HandleFunc("POST /api/providers", s.handleConnectProvider)
			authed.HandleFunc("GET /api/providers", s.handleListProviders)
			authed.HandleFunc("GET /api/providers/{name}/models", s.handleProviderModels)
		} else {
			authed.Handle("POST /api/providers", notImplemented("caller-scoped provider connect"))
			authed.Handle("GET /api/providers", notImplemented("caller-scoped provider list"))
			authed.Handle("GET /api/providers/{name}/models", notImplemented("caller-scoped provider models"))
		}
	}

	// BYO-MCP register + tool catalog (m14.6, ADR 0016): probe a user's MCP server,
	// capture its tools (with inputSchema), store an optional key as a Secret, write
	// a user-added ToolRegistry entry per tool, and open per-server egress — all
	// CALLER-SCOPED. Gated by TWO factors, in order (the connect-flow pattern):
	//
	//  1. the Helm KILL-SWITCH (mcpEnabled). OFF → the routes are NOT registered, so
	//     they fall through to the SPA handler and 404 (feature-off) — a hardened
	//     install that forbids BYO MCP. Checked FIRST so the endpoints are genuinely
	//     absent.
	//  2. the CALLER-CLIENT factory (like every user-facing CRD route): nil → 501,
	//     never a BFF-SA fallback (ADR 0011). GET /api/tools reads ToolRegistries
	//     caller-scoped; POST /api/mcpservers writes caller-scoped.
	//
	// The trust policy (self-serve vs pending-approval) is the SEPARATE
	// mcpRequireApproval value, read inside the handler — it changes the entry
	// status, not the route wiring.
	if s.mcpEnabled {
		if s.callerClients != nil {
			authed.HandleFunc("POST /api/mcpservers", s.handleRegisterMCPServer)
			authed.HandleFunc("GET /api/mcpservers", s.handleListMCPServers)
			authed.HandleFunc("GET /api/tools", s.handleListTools)
		} else {
			authed.Handle("POST /api/mcpservers", notImplemented("caller-scoped MCP register"))
			authed.Handle("GET /api/mcpservers", notImplemented("caller-scoped MCP list"))
			authed.Handle("GET /api/tools", notImplemented("caller-scoped tool catalog"))
		}
	}

	api.Handle("/api/", s.requireAuth(authed))

	root := http.NewServeMux()
	root.Handle("/api/", api)
	root.Handle("/", s.spaHandler())
	return root
}

// contentSecurityPolicy is the strict CSP served with the SPA (ADR 0012).
// sessionStorage holds the caller's bearer token, so an XSS foothold could
// exfiltrate it; the CSP is the primary mitigation (alongside short-TTL tokens).
// Each directive is deliberately minimal — only what the Vite build genuinely
// needs (the built index.html loads a same-origin module script + a same-origin
// stylesheet from /assets; no third-party origins, no inline <script>). Why each
// directive exists:
//
//	default-src 'self'      deny everything by default; only same-origin, which
//	                        alone blocks third-party script/connect/img/etc.
//	script-src 'self'       the bundle is same-origin /assets/*.js only; NO
//	                        'unsafe-inline'/'unsafe-eval' — inline scripts and
//	                        eval are forbidden (the strongest anti-XSS lever).
//	style-src 'self' 'unsafe-inline'
//	                        same-origin CSS + inline style ATTRIBUTES, which
//	                        React emits for style-prop elements (the topology
//	                        graph positions nodes via element style). No
//	                        third-party stylesheets. Revisit at OIDC/M18 —
//	                        a nonce could drop 'unsafe-inline'.
//	img-src 'self' data:    same-origin images + data: URIs (inline SVG icons /
//	                        tiny data-URL assets Vite may inline).
//	font-src 'self'         fonts are same-origin only (none are third-party).
//	connect-src 'self'      XHR/fetch (the /api/* calls) only to the serving
//	                        origin — the bearer token can never be POSTed to a
//	                        third party even if injected script tried.
//	frame-ancestors 'none'  the console is never framed (clickjacking guard).
//	base-uri 'self'         a <base> injection can't repoint relative asset URLs.
//	form-action 'self'      a stolen form can't submit off-origin.
//	object-src 'none'       no plugins/embeds (legacy XSS vector).
//
// It is set on EVERY SPA response (the index document AND the hashed assets),
// never on /api/* responses (those keep their own headers — the SPA handler is a
// separate branch of the root mux). Kept as a single const so the value and the
// unit test reference the exact same string.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"object-src 'none'"

// setSPASecurityHeaders applies the SPA's security headers to a response. Called
// on every static/index response (not /api). CSP is the sessionStorage-token XSS
// mitigation (ADR 0012); the companions are cheap, standard hardening.
func setSPASecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	// Belt-and-braces alongside the CSP frame-ancestors directive (older UAs).
	h.Set("X-Frame-Options", "DENY")
	// Don't let the browser MIME-sniff a static asset into an executable type.
	h.Set("X-Content-Type-Options", "nosniff")
	// Don't leak the console URL (which may carry a namespace) to third parties.
	h.Set("Referrer-Policy", "no-referrer")
}

// spaHandler serves the static Vite build. Requests for real files are served
// verbatim; any other path (a client-side route like /agents) falls back to
// index.html so the React router can handle it (SPA history-mode routing).
// When no static FS is configured, it 404s (api-only mode). Every served
// response carries the strict SPA security headers (CSP et al., ADR 0012).
func (s *Server) spaHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.static == nil {
			http.NotFound(w, r)
			return
		}
		// Security headers on every SPA response — the document AND its assets.
		setSPASecurityHeaders(w)
		// Clean the request path to a filesystem path (io/fs uses no leading /).
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = indexHTML
		}
		if f, err := s.static.Open(name); err == nil {
			// A directory request falls through to index.html (SPA route).
			if info, statErr := f.Stat(); statErr == nil && !info.IsDir() {
				_ = f.Close()
				http.FileServerFS(s.static).ServeHTTP(w, r)
				return
			}
			_ = f.Close()
		}
		// Not a real file → serve the SPA entrypoint for client-side routing.
		s.serveIndex(w)
	})
}

// serveIndex writes dist/index.html (the SPA shell). Used for client-side routes
// and when the requested asset does not exist on disk. The caller (spaHandler)
// has already applied the SPA security headers.
func (s *Server) serveIndex(w http.ResponseWriter) {
	data, err := fs.ReadFile(s.static, indexHTML)
	if err != nil {
		http.Error(w, "UI not built", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// index.html must never be cached so a new build's asset hashes are picked
	// up immediately.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
