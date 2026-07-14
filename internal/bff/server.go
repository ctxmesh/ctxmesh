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

	"github.com/ctxmesh/agent-engine/internal/prompt"
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

	// devMode is true when the BFF runs under `agent-engine dev --ui` (ADR 0021):
	// a local, single-developer substrate with NO cluster (callerClients nil →
	// cluster-only endpoints serve honest 501) and NO login wall. GET /api/devmode
	// exposes it so the SPA renders dev-mode chrome instead of the login gate.
	devMode bool
	// devInvokeEndpoint is the base URL of the single local Compose agent under
	// `dev --ui` (ADR 0021). When set, POST /api/invoke targets it directly (no
	// cluster resolution) so the Playground run works locally. Empty otherwise.
	devInvokeEndpoint string

	// oidc* advertise the console SSO config (ADR 0020) at GET /api/authconfig so
	// the SPA can run Auth-Code+PKCE against Dex. The BFF stays auth-transparent
	// (ADR 0011): it holds NO OIDC secret (the console is a public PKCE client) and
	// never validates tokens itself — it only tells the SPA the issuer + client id.
	// When oidcEnabled is false the SPA uses token login (ADR 0012).
	oidcEnabled  bool
	oidcIssuer   string
	oidcClientID string

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

	// oauthFlows is the SERVER-SIDE, short-TTL store of in-flight MCP OAuth 2.1
	// authorization flows (m17.2, ADR 0016), keyed by the CSRF `state`. It holds the
	// PKCE code_verifier + the registering caller's token — NEVER surfaced to the
	// browser. Always non-nil (initialized in NewServer) so the OAuth register +
	// callback handlers can use it whenever MCP is enabled.
	oauthFlows *pendingOAuthStore

	// promptResolver is the OPTIONAL server-side seam for resolving a git-pointer
	// PromptVersion (repo, ref, path) → prompt content (m17.8). It is nil by default
	// (no prompt resolution configured). When nil, the diff endpoint returns an honest
	// 501 ("prompt resolution not configured") rather than fabricating any content.
	// The BFF never stores resolved prompt content beyond the lifetime of a diff
	// request — git remains the source of truth (ADR 0008).
	// Wire via Options.PromptResolver at construction time (e.g. a FixtureResolver
	// in tests, a real go-git Resolver in production when available).
	promptResolver prompt.Resolver

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
	// DevMode marks the local `agent-engine dev --ui` substrate (ADR 0021): no
	// cluster (CallerClients nil), no login wall. Surfaced at GET /api/devmode so
	// the SPA renders dev chrome instead of the login gate.
	DevMode bool
	// DevInvokeEndpoint is the base URL of the local Compose agent (dev mode). When
	// set (with an Invoke adapter), POST /api/invoke targets it directly so the
	// Playground run works with no cluster. Ignored unless DevMode is on.
	DevInvokeEndpoint string
	// OIDCEnabled advertises console SSO (ADR 0020) at GET /api/authconfig. When
	// false the SPA uses token login (ADR 0012). OIDCIssuer/OIDCClientID are the Dex
	// issuer + the public PKCE client id the SPA needs to start the flow.
	OIDCEnabled  bool
	OIDCIssuer   string
	OIDCClientID string
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
	// MCPGrantHMACKey is the per-cluster key that salts the one-way user-identity hash
	// used as the grant Secret label + mcp-owner annotation (m25.1, ADR 0029 §7). When
	// non-empty, userGrantHash is HMAC-SHA256(user, key) — offline confirmation of a
	// low-entropy username/email is infeasible without the key. When empty (dev /
	// not-yet-hardened), it degrades to legacy unsalted SHA-256 with a start-up warning.
	// A production cluster MUST set it (a per-cluster platform Secret); wired from
	// MCP_GRANT_HMAC_KEY in cmd/bff/main.go. Immutable after start-up (changing it
	// re-keys all grants ⇒ re-consent).
	MCPGrantHMACKey []byte
	// PromptResolver is the OPTIONAL server-side resolver for git-pointer PromptVersions
	// (m17.8). When nil (the default), the diff endpoint returns an honest 501
	// ("prompt resolution not configured"). Wire a FixtureResolver in tests and a
	// real go-git Resolver in production. The BFF never stores resolved prompt content
	// beyond the diff response (ADR 0008).
	PromptResolver prompt.Resolver
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
		devMode:                  opts.DevMode,
		devInvokeEndpoint:        opts.DevInvokeEndpoint,
		oidcEnabled:              opts.OIDCEnabled,
		oidcIssuer:               opts.OIDCIssuer,
		oidcClientID:             opts.OIDCClientID,
		providerConnect:          opts.ProviderConnect,
		providerHTTP:             opts.ProviderHTTP,
		platformGenerationModels: opts.PlatformGenerationModels,
		mcpEnabled:               opts.MCPEnabled,
		mcpRequireApproval:       opts.MCPRequireApproval,
		oauthFlows:               newPendingOAuthStore(),
		promptResolver:           opts.PromptResolver,
		log:                      opts.Log,
	}
	if s.version == "" {
		s.version = defaultVersion
	}
	if opts.StaticDir != "" {
		s.static = os.DirFS(opts.StaticDir)
	}
	// Wire the per-cluster HMAC key that salts the one-way user-identity hash used for
	// grant Secret labels + the mcp-owner annotation (m25.1, ADR 0029 §7). Package-level
	// because userGrantHash is also called by the runtime resolver (not a *Server); the
	// key is cluster-global and set once. Empty ⇒ legacy unsalted SHA-256 (warned below).
	setGrantHMACKey(opts.MCPGrantHMACKey)
	if s.mcpEnabled && len(opts.MCPGrantHMACKey) == 0 {
		opts.Log.Info("mcp: MCP_GRANT_HMAC_KEY not set — user-identity hashes use legacy unsalted SHA-256; " +
			"set a per-cluster HMAC key before production (ADR 0029 §7). Changing it later re-keys grants (re-consent).")
	}
	return s
}

// Handler returns the fully-wired http.Handler: the /api mux (auth-gated) plus
// the SPA static handler as the fallback for everything else.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()

	// Health is unauthenticated (liveness/version probe; no cluster access).
	api.HandleFunc("GET /api/health", s.handleHealth)
	// Dev-mode probe (ADR 0021) — unauthenticated so the SPA can read it before any
	// session exists. {devMode:true} → the `dev --ui` local substrate (no login wall,
	// cluster surfaces honestly degraded); {devMode:false} → the normal cluster BFF.
	api.HandleFunc("GET /api/devmode", s.handleDevMode)
	// Auth config (ADR 0020) — unauthenticated so the login page can read it before a
	// session. Tells the SPA whether OIDC/SSO is available (issuer + public PKCE client
	// id) so it offers "Sign in with SSO"; token login (ADR 0012) is the fallback.
	api.HandleFunc("GET /api/authconfig", s.handleAuthConfig)

	// Authenticated surface. The CRD routes run through the CALLER-SCOPED client
	// (ADR 0011): list/create/topology reflect exactly what the caller's own RBAC
	// permits, enforced by the K8s API server. They are wired only when the
	// caller-client factory is configured; honest 501 otherwise (the BFF never
	// falls back to its own SA for user CRD ops — that is the confused-deputy gap
	// this task closes). Create additionally needs the scheme to decode manifests.
	authed := http.NewServeMux()
	if s.callerClients != nil {
		authed.HandleFunc("GET /api/agents", s.handleListAgents)
		// Agent detail + live log tail (m14.7, first-agent-flow.md §3). Both run
		// through the CALLER-SCOPED client (ADR 0011): the detail read + the SSE
		// pod-log stream act as the caller, so K8s RBAC governs them. The Go 1.22
		// ServeMux treats "GET /api/agents", "GET /api/agents/{ns}/{name}" and
		// ".../{name}/logs" as three DISTINCT patterns (the more specific wins), so
		// these never shadow the list route above or the create route below.
		authed.HandleFunc("GET /api/agents/{ns}/{name}", s.handleAgentDetail)
		authed.HandleFunc("GET /api/agents/{ns}/{name}/logs", s.handleAgentLogs)
		// Redaction-policy editor (m18.13, ADR 0019): read/replace the agent's custom
		// trace-redaction detectors. Both caller-scoped; the PUT is enforced by the
		// API server (a viewer without update is denied → 403).
		authed.HandleFunc("GET /api/agents/{ns}/{name}/tracepolicy", s.handleGetTracePolicy)
		authed.HandleFunc("PUT /api/agents/{ns}/{name}/tracepolicy", s.handleUpdateTracePolicy)
		// Per-agent recent runs (m15.9, first-agent-flow.md §3): the bounded run
		// history for ONE agent. CALLER-SCOPED existence check (the caller must be
		// able to `get` the agent) THEN a server-side Langfuse fetch filtered to the
		// `agent:<ns>/<name>` trace identity tag (cross-namespace correct). The Go 1.22
		// ServeMux treats ".../{name}/runs" as MORE SPECIFIC than "GET .../{ns}/{name}"
		// so it never shadows the detail GET. Wired only when the Langfuse adapter is
		// present; absent → an honest 501 (m14.8 degrade), never a 500.
		if s.adapters.Langfuse != nil {
			authed.HandleFunc("GET /api/agents/{ns}/{name}/runs", s.handleAgentRuns)
		} else {
			authed.Handle("GET /api/agents/{ns}/{name}/runs", notImplemented("Langfuse per-agent runs adapter"))
		}
		// Agent EDIT (m15.3, ADR 0017): PUT the edited simplified spec. Two modes,
		// keyed on the source-spec annotation — a full expand+SSA round-trip for a
		// console-managed agent, a degraded safe-field SSA patch for an annotation-
		// less (kubectl-created) one. It runs through the SAME CALLER-SCOPED client
		// (ADR 0011): a viewer's PUT surfaces the API server's real 403, and the write
		// is SSA-only under a console field-manager (never a controller-clobbering
		// Update). The Go 1.22 ServeMux treats "GET .../{ns}/{name}" and
		// "PUT .../{ns}/{name}" as distinct method+pattern routes, so this is additive
		// beside the detail GET. It needs the scheme (to decode/apply manifests); when
		// the scheme is absent the route serves an honest 501 below.
		if s.scheme != nil {
			authed.HandleFunc("PUT /api/agents/{ns}/{name}", s.handleUpdateAgent)
		} else {
			authed.Handle("PUT /api/agents/{ns}/{name}", notImplemented("agent edit"))
		}
		// Agent DELETE (m15.4, ADR 0017): remove the AgentDeployment via the
		// CALLER-SCOPED client (ADR 0011). Owned children are garbage-collected by
		// Kubernetes; independent references (agentRef-only, no ownerRef) are left in
		// place — orphan pruning is deferred. A viewer's DELETE surfaces the API
		// server's real 403; no RBAC pre-emption. The Go 1.22 ServeMux treats
		// "DELETE .../{ns}/{name}" as a DISTINCT pattern from the GET/PUT above.
		authed.HandleFunc("DELETE /api/agents/{ns}/{name}", s.handleDeleteAgent)
		// Delete-impact preview (m15.4, ADR 0017): lists MCPToolBinding,
		// AgentScalingPolicy, and MemoryBinding in the namespace that reference the
		// named agent by spec.agentRef, classifying each as GC'd (owned) or orphan
		// (independent reference). The Go 1.22 ServeMux treats this sub-path pattern
		// as MORE SPECIFIC than "GET .../{ns}/{name}" and so it never shadows the
		// detail GET above.
		authed.HandleFunc("GET /api/agents/{ns}/{name}/references", s.handleAgentReferences)
		authed.HandleFunc("GET /api/usedby", s.handleUsedBy)
		authed.HandleFunc("GET /api/topology", s.handleTopology)
		// ModelRoute CRUD (m15.5): direct edit — no expand, no source-spec
		// annotation. Five endpoints following the list contract for GET /list and
		// the SSA-under-console-field-manager pattern for PUT. The scheme is needed
		// for SSA (ensureGVK); when absent the write routes serve 501 honestly.
		authed.HandleFunc("GET /api/modelroutes", s.handleListModelRoutes)
		authed.HandleFunc("GET /api/modelroutes/{ns}/{name}", s.handleGetModelRoute)
		if s.scheme != nil {
			authed.HandleFunc("POST /api/modelroutes", s.handleCreateModelRoute)
			authed.HandleFunc("PUT /api/modelroutes/{ns}/{name}", s.handleUpdateModelRoute)
		} else {
			authed.Handle("POST /api/modelroutes", notImplemented("model route create"))
			authed.Handle("PUT /api/modelroutes/{ns}/{name}", notImplemented("model route update"))
		}
		authed.HandleFunc("DELETE /api/modelroutes/{ns}/{name}", s.handleDeleteModelRoute)
		// SecretBinding CRUD (m15.6): direct edit — no expand, no source-spec
		// annotation. Five endpoints following the list contract for GET /list and
		// the SSA-under-console-field-manager pattern for PUT.
		//
		// SECURITY (ADR 0015): the BFF NEVER reads the referenced Kubernetes Secret
		// to return its data. Every DTO projects only the SecretBinding CRD's own
		// fields — which Secret name, which key — plus status. The credential value
		// lives only in the Secret; it never flows through this API surface.
		//
		// The scheme is needed for SSA (ensureGVK); when absent the write routes
		// serve 501 honestly. The GET routes do not need the scheme.
		authed.HandleFunc("GET /api/secretbindings", s.handleListSecretBindings)
		authed.HandleFunc("GET /api/secretbindings/{ns}/{name}", s.handleGetSecretBinding)
		if s.scheme != nil {
			authed.HandleFunc("POST /api/secretbindings", s.handleCreateSecretBinding)
			authed.HandleFunc("PUT /api/secretbindings/{ns}/{name}", s.handleUpdateSecretBinding)
		} else {
			authed.Handle("POST /api/secretbindings", notImplemented("secret binding create"))
			authed.Handle("PUT /api/secretbindings/{ns}/{name}", notImplemented("secret binding update"))
		}
		authed.HandleFunc("DELETE /api/secretbindings/{ns}/{name}", s.handleDeleteSecretBinding)
		// AgentRegistry CRUD (m15.7): direct edit — no expand, no source-spec
		// annotation. Five endpoints following the list contract for GET /list and
		// the SSA-under-console-field-manager pattern for PUT.
		//
		// SECURITY INVARIANT: the console CANNOT alter the egress posture. The SSA
		// apply object carries only the AgentRegistry spec (memberSelector, guards,
		// roles); the controller-owned NetworkPolicy (M6 whitelist + M11 default-deny)
		// is never touched. There is no egress/allowlist field in any DTO.
		//
		// registryId is immutable after creation (CRD XValidation). The PUT body has
		// no registryId field, so an edit cannot change it: a submitted value is
		// ignored and the live value is preserved (the PUT returns 200). Immutability
		// holds by construction, not by an API-server rejection.
		//
		// The scheme is needed for SSA (ensureGVK); when absent the write routes
		// serve 501 honestly. The GET routes do not need the scheme.
		authed.HandleFunc("GET /api/agentregistries", s.handleListAgentRegistries)
		authed.HandleFunc("GET /api/agentregistries/{ns}/{name}", s.handleGetAgentRegistry)
		if s.scheme != nil {
			authed.HandleFunc("POST /api/agentregistries", s.handleCreateAgentRegistry)
			authed.HandleFunc("PUT /api/agentregistries/{ns}/{name}", s.handleUpdateAgentRegistry)
		} else {
			authed.Handle("POST /api/agentregistries", notImplemented("agent registry create"))
			authed.Handle("PUT /api/agentregistries/{ns}/{name}", notImplemented("agent registry update"))
		}
		authed.HandleFunc("DELETE /api/agentregistries/{ns}/{name}", s.handleDeleteAgentRegistry)
		// ToolRegistry CRUD (m17.5): direct edit of the operator-curated tool catalog.
		// Five endpoints following the list contract for GET /list and SSA for PUT/POST.
		// IMPORTANT: the m14.6 GET /api/tools merged catalog (distinct route, distinct
		// handler) is NOT affected — these routes are /api/toolregistries, not /api/tools.
		// The PUT preserves each tool entry's approvalStatus (controller/approval-owned);
		// the console CRUD edits the curated fields, never the approval state.
		// The scheme is needed for SSA (ensureGVK); when absent the write routes serve 501.
		authed.HandleFunc("GET /api/toolregistries", s.handleListToolRegistries)
		authed.HandleFunc("GET /api/toolregistries/{ns}/{name}", s.handleGetToolRegistry)
		if s.scheme != nil {
			authed.HandleFunc("POST /api/toolregistries", s.handleCreateToolRegistry)
			authed.HandleFunc("PUT /api/toolregistries/{ns}/{name}", s.handleUpdateToolRegistry)
		} else {
			authed.Handle("POST /api/toolregistries", notImplemented("tool registry create"))
			authed.Handle("PUT /api/toolregistries/{ns}/{name}", notImplemented("tool registry update"))
		}
		authed.HandleFunc("DELETE /api/toolregistries/{ns}/{name}", s.handleDeleteToolRegistry)
		// MCPToolBinding CRUD (m17.5): direct edit of the tool-to-agent bindings.
		// Five endpoints following the list contract + SSA. The detail DTO surfaces
		// the HONEST hot-update propagation status: "propagated" only when the
		// controller's Ready=True (tool registered, pin-matched, rendered + pushed to
		// the discovery sidecar); failure reason when Ready=False (UnregisteredTool /
		// RegistryMismatch); "pending" when the condition is absent. The console NEVER
		// reports "propagated" unless the controller confirms it — the m16 honest-contract
		// lesson. SSA never clobbles the controller's status/Ready condition.
		authed.HandleFunc("GET /api/mcptoolbindings", s.handleListMCPToolBindings)
		authed.HandleFunc("GET /api/mcptoolbindings/{ns}/{name}", s.handleGetMCPToolBinding)
		if s.scheme != nil {
			authed.HandleFunc("POST /api/mcptoolbindings", s.handleCreateMCPToolBinding)
			authed.HandleFunc("PUT /api/mcptoolbindings/{ns}/{name}", s.handleUpdateMCPToolBinding)
		} else {
			authed.Handle("POST /api/mcptoolbindings", notImplemented("MCP tool binding create"))
			authed.Handle("PUT /api/mcptoolbindings/{ns}/{name}", notImplemented("MCP tool binding update"))
		}
		authed.HandleFunc("DELETE /api/mcptoolbindings/{ns}/{name}", s.handleDeleteMCPToolBinding)
		// MemoryBinding CRUD (m17.6): direct edit of memory backend bindings for
		// AgentDeployments. Five endpoints following the list contract + SSA.
		// agentRef is NOT CRD-immutable (no oldSelf XValidation) — a PUT that
		// changes agentRef is accepted and applied by the API server. The BFF does
		// not enforce immutability because the CRD does not.
		// The scheme is needed for SSA (ensureGVK); when absent the write routes
		// serve 501 honestly.
		authed.HandleFunc("GET /api/memorybindings", s.handleListMemoryBindings)
		authed.HandleFunc("GET /api/memorybindings/{ns}/{name}", s.handleGetMemoryBinding)
		if s.scheme != nil {
			authed.HandleFunc("POST /api/memorybindings", s.handleCreateMemoryBinding)
			authed.HandleFunc("PUT /api/memorybindings/{ns}/{name}", s.handleUpdateMemoryBinding)
		} else {
			authed.Handle("POST /api/memorybindings", notImplemented("memory binding create"))
			authed.Handle("PUT /api/memorybindings/{ns}/{name}", notImplemented("memory binding update"))
		}
		authed.HandleFunc("DELETE /api/memorybindings/{ns}/{name}", s.handleDeleteMemoryBinding)
		// AgentScalingPolicy CRUD (m17.6): direct edit of elastic scaling rules for
		// AgentDeployments. Five endpoints following the list contract + SSA.
		// CRD XValidations (max>=min, schedule required when trigger=schedule) are
		// enforced by the API server — rejections surface as honest 422 with the
		// server's message. The BFF does not re-implement these rules.
		// agentRef is NOT CRD-immutable — a PUT that changes agentRef is accepted
		// and applied. The scheme is needed for SSA; absent → 501.
		authed.HandleFunc("GET /api/agentscalingpolicies", s.handleListAgentScalingPolicies)
		authed.HandleFunc("GET /api/agentscalingpolicies/{ns}/{name}", s.handleGetAgentScalingPolicy)
		if s.scheme != nil {
			authed.HandleFunc("POST /api/agentscalingpolicies", s.handleCreateAgentScalingPolicy)
			authed.HandleFunc("PUT /api/agentscalingpolicies/{ns}/{name}", s.handleUpdateAgentScalingPolicy)
		} else {
			authed.Handle("POST /api/agentscalingpolicies", notImplemented("agent scaling policy create"))
			authed.Handle("PUT /api/agentscalingpolicies/{ns}/{name}", notImplemented("agent scaling policy update"))
		}
		authed.HandleFunc("DELETE /api/agentscalingpolicies/{ns}/{name}", s.handleDeleteAgentScalingPolicy)
		// EvalSuite CRUD (m17.7): direct edit of eval gate configurations.
		// The detail DTO surfaces dataset/scorers/gate/threshold + the controller's
		// status.conditions (gate/pass/block outcome). The /results sub-resource
		// merges the CRD status with Langfuse scores honestly: scoresAvailable:false
		// + reason when Langfuse is absent or traceId not supplied; real scores when
		// both are present. Controller status.conditions are NEVER clobbered (SSA spec-only).
		authed.HandleFunc("GET /api/evalsuites", s.handleListEvalSuites)
		authed.HandleFunc("GET /api/evalsuites/{ns}/{name}", s.handleGetEvalSuite)
		// Results always wired: degrades honestly (scoresAvailable:false) when Langfuse absent.
		authed.HandleFunc("GET /api/evalsuites/{ns}/{name}/results", s.handleGetEvalSuiteResults)
		if s.scheme != nil {
			authed.HandleFunc("POST /api/evalsuites", s.handleCreateEvalSuite)
			authed.HandleFunc("PUT /api/evalsuites/{ns}/{name}", s.handleUpdateEvalSuite)
		} else {
			authed.Handle("POST /api/evalsuites", notImplemented("eval suite create"))
			authed.Handle("PUT /api/evalsuites/{ns}/{name}", notImplemented("eval suite update"))
		}
		authed.HandleFunc("DELETE /api/evalsuites/{ns}/{name}", s.handleDeleteEvalSuite)
		// PromptVersion CRUD + diff (m17.8): direct edit of git-backed prompt pointers.
		// The detail DTO surfaces git (repo/ref/path) + status.conditions.
		// The /diff sub-resource resolves the two PromptVersions' git pointers via the
		// optional prompt Resolver and returns a TEXTUAL line diff of the resolved content.
		// If no resolver is configured → the diff endpoint returns an honest 501 (never
		// fabricates content). ErrNotFound → 404; transient resolve failure → 502.
		// Controller status.conditions are NEVER clobbered (SSA spec-only applies).
		authed.HandleFunc("GET /api/promptversions", s.handleListPromptVersions)
		authed.HandleFunc("GET /api/promptversions/{ns}/{name}", s.handleGetPromptVersion)
		// Diff is always wired (degrades to 501 when no resolver configured).
		authed.HandleFunc("GET /api/promptversions/{ns}/{name}/diff", s.handlePromptVersionDiff)
		if s.scheme != nil {
			authed.HandleFunc("POST /api/promptversions", s.handleCreatePromptVersion)
			authed.HandleFunc("PUT /api/promptversions/{ns}/{name}", s.handleUpdatePromptVersion)
		} else {
			authed.Handle("POST /api/promptversions", notImplemented("prompt version create"))
			authed.Handle("PUT /api/promptversions/{ns}/{name}", notImplemented("prompt version update"))
		}
		authed.HandleFunc("DELETE /api/promptversions/{ns}/{name}", s.handleDeletePromptVersion)
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
		authed.Handle("GET /api/agents/{ns}/{name}", notImplemented("caller-scoped agent detail"))
		authed.Handle("GET /api/agents/{ns}/{name}/logs", notImplemented("caller-scoped agent logs"))
		authed.Handle("GET /api/agents/{ns}/{name}/runs", notImplemented("caller-scoped agent runs"))
		authed.Handle("PUT /api/agents/{ns}/{name}", notImplemented("caller-scoped agent edit"))
		authed.Handle("DELETE /api/agents/{ns}/{name}", notImplemented("caller-scoped agent delete"))
		authed.Handle("GET /api/agents/{ns}/{name}/references", notImplemented("caller-scoped agent references"))
		authed.Handle("GET /api/topology", notImplemented("caller-scoped topology"))
		authed.Handle("GET /api/modelroutes", notImplemented("caller-scoped model route list"))
		authed.Handle("GET /api/modelroutes/{ns}/{name}", notImplemented("caller-scoped model route detail"))
		authed.Handle("POST /api/modelroutes", notImplemented("caller-scoped model route create"))
		authed.Handle("PUT /api/modelroutes/{ns}/{name}", notImplemented("caller-scoped model route update"))
		authed.Handle("DELETE /api/modelroutes/{ns}/{name}", notImplemented("caller-scoped model route delete"))
		authed.Handle("GET /api/secretbindings", notImplemented("caller-scoped secret binding list"))
		authed.Handle("GET /api/secretbindings/{ns}/{name}", notImplemented("caller-scoped secret binding detail"))
		authed.Handle("POST /api/secretbindings", notImplemented("caller-scoped secret binding create"))
		authed.Handle("PUT /api/secretbindings/{ns}/{name}", notImplemented("caller-scoped secret binding update"))
		authed.Handle("DELETE /api/secretbindings/{ns}/{name}", notImplemented("caller-scoped secret binding delete"))
		authed.Handle("GET /api/agentregistries", notImplemented("caller-scoped agent registry list"))
		authed.Handle("GET /api/agentregistries/{ns}/{name}", notImplemented("caller-scoped agent registry detail"))
		authed.Handle("POST /api/agentregistries", notImplemented("caller-scoped agent registry create"))
		authed.Handle("PUT /api/agentregistries/{ns}/{name}", notImplemented("caller-scoped agent registry update"))
		authed.Handle("DELETE /api/agentregistries/{ns}/{name}", notImplemented("caller-scoped agent registry delete"))
		authed.Handle("GET /api/toolregistries", notImplemented("caller-scoped tool registry list"))
		authed.Handle("GET /api/toolregistries/{ns}/{name}", notImplemented("caller-scoped tool registry detail"))
		authed.Handle("POST /api/toolregistries", notImplemented("caller-scoped tool registry create"))
		authed.Handle("PUT /api/toolregistries/{ns}/{name}", notImplemented("caller-scoped tool registry update"))
		authed.Handle("DELETE /api/toolregistries/{ns}/{name}", notImplemented("caller-scoped tool registry delete"))
		authed.Handle("GET /api/mcptoolbindings", notImplemented("caller-scoped MCP tool binding list"))
		authed.Handle("GET /api/mcptoolbindings/{ns}/{name}", notImplemented("caller-scoped MCP tool binding detail"))
		authed.Handle("POST /api/mcptoolbindings", notImplemented("caller-scoped MCP tool binding create"))
		authed.Handle("PUT /api/mcptoolbindings/{ns}/{name}", notImplemented("caller-scoped MCP tool binding update"))
		authed.Handle("DELETE /api/mcptoolbindings/{ns}/{name}", notImplemented("caller-scoped MCP tool binding delete"))
		authed.Handle("GET /api/memorybindings", notImplemented("caller-scoped memory binding list"))
		authed.Handle("GET /api/memorybindings/{ns}/{name}", notImplemented("caller-scoped memory binding detail"))
		authed.Handle("POST /api/memorybindings", notImplemented("caller-scoped memory binding create"))
		authed.Handle("PUT /api/memorybindings/{ns}/{name}", notImplemented("caller-scoped memory binding update"))
		authed.Handle("DELETE /api/memorybindings/{ns}/{name}", notImplemented("caller-scoped memory binding delete"))
		authed.Handle("GET /api/agentscalingpolicies", notImplemented("caller-scoped agent scaling policy list"))
		authed.Handle("GET /api/agentscalingpolicies/{ns}/{name}", notImplemented("caller-scoped agent scaling policy detail"))
		authed.Handle("POST /api/agentscalingpolicies", notImplemented("caller-scoped agent scaling policy create"))
		authed.Handle("PUT /api/agentscalingpolicies/{ns}/{name}", notImplemented("caller-scoped agent scaling policy update"))
		authed.Handle("DELETE /api/agentscalingpolicies/{ns}/{name}", notImplemented("caller-scoped agent scaling policy delete"))
		authed.Handle("GET /api/evalsuites", notImplemented("caller-scoped eval suite list"))
		authed.Handle("GET /api/evalsuites/{ns}/{name}", notImplemented("caller-scoped eval suite detail"))
		authed.Handle("GET /api/evalsuites/{ns}/{name}/results", notImplemented("caller-scoped eval suite results"))
		authed.Handle("POST /api/evalsuites", notImplemented("caller-scoped eval suite create"))
		authed.Handle("PUT /api/evalsuites/{ns}/{name}", notImplemented("caller-scoped eval suite update"))
		authed.Handle("DELETE /api/evalsuites/{ns}/{name}", notImplemented("caller-scoped eval suite delete"))
		authed.Handle("GET /api/promptversions", notImplemented("caller-scoped prompt version list"))
		authed.Handle("GET /api/promptversions/{ns}/{name}", notImplemented("caller-scoped prompt version detail"))
		authed.Handle("GET /api/promptversions/{ns}/{name}/diff", notImplemented("caller-scoped prompt version diff"))
		authed.Handle("POST /api/promptversions", notImplemented("caller-scoped prompt version create"))
		authed.Handle("PUT /api/promptversions/{ns}/{name}", notImplemented("caller-scoped prompt version update"))
		authed.Handle("DELETE /api/promptversions/{ns}/{name}", notImplemented("caller-scoped prompt version delete"))
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
		// Run inspector (m14.8, first-agent-flow.md §3/§5): the flat span summary for
		// one trace (trace + observations). The Go 1.22 ServeMux treats
		// "GET /api/traces/{id}" and "GET /api/traces/{id}/detail" as DISTINCT
		// patterns (the more specific "/detail" wins), so this is additive and never
		// shadows the embed-URL route above.
		authed.HandleFunc("GET /api/traces/{id}/detail", s.handleTraceDetail)
		// Feedback / scores browser (m16.4): reads Langfuse scores for one trace
		// (GET /api/public/scores?traceId=) and returns them as the feedback panel's
		// flat score list. Requires ?traceId; missing → 400. Langfuse absent → 501
		// (registered by the else branch below).
		authed.HandleFunc("GET /api/feedback", s.handleFeedback)
		// Cost breakdown by agent (m16.5): aggregates a bounded recent window of
		// Langfuse traces by agent:<ns>/<name> tag and returns per-agent cost/usage.
		// ?by=agent is required; any other `by` value → 400. Requires ?by=agent;
		// other values are explicitly rejected (honest contract). Langfuse absent →
		// 501 (registered by the else branch below).
		authed.HandleFunc("GET /api/cost/breakdown", s.handleCostBreakdown)
	} else {
		authed.Handle("GET /api/runs", notImplemented("Langfuse runs adapter"))
		authed.Handle("GET /api/cost", notImplemented("Langfuse cost adapter"))
		authed.Handle("GET /api/traces/", notImplemented("Langfuse trace adapter"))
		authed.Handle("GET /api/feedback", notImplemented("Langfuse feedback adapter"))
		authed.Handle("GET /api/cost/breakdown", notImplemented("Langfuse cost breakdown adapter"))
	}

	// Remaining adapter seams for m12.6–m12.7: mounted now (discoverable) but
	// honest 501 until their adapter is wired.
	if s.adapters.Prometheus == nil {
		authed.Handle("GET /api/metrics/", notImplemented("Prometheus adapter"))
	}
	// Playground invoke (m12.7): run a deployed agent, traced, and return its
	// traceId. The CLUSTER path is wired only when BOTH the InvokeAdapter (the
	// pure-HTTP invoker) AND the caller-client factory are present — the run is
	// CALLER-SCOPED (the agent lookup + dispatch act as the caller, ADR 0011), so it
	// needs the caller-client seam and must never fall back to the BFF SA.
	//
	// The DEV path (ADR 0021) has no cluster: when devMode + a devInvokeEndpoint are
	// set, POST /api/invoke targets the single local Compose agent directly (no RBAC
	// to scope — it's a single local developer). Honest 501 when neither is wired.
	switch {
	case s.devMode && s.adapters.Invoke != nil && s.devInvokeEndpoint != "":
		authed.HandleFunc("POST /api/invoke", s.handleDevInvoke)
	case s.adapters.Invoke != nil && s.callerClients != nil:
		authed.HandleFunc("POST /api/invoke", s.handleInvoke)
	default:
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
			authed.HandleFunc("POST /api/providers/{name}/rotate", s.handleRotateProviderKey)
			authed.HandleFunc("DELETE /api/providers/{name}", s.handleDisconnectProvider)
		} else {
			authed.Handle("POST /api/providers", notImplemented("caller-scoped provider connect"))
			authed.Handle("GET /api/providers", notImplemented("caller-scoped provider list"))
			authed.Handle("GET /api/providers/{name}/models", notImplemented("caller-scoped provider models"))
			authed.Handle("POST /api/providers/{name}/rotate", notImplemented("caller-scoped provider key rotation"))
			authed.Handle("DELETE /api/providers/{name}", notImplemented("caller-scoped provider disconnect"))
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
			// Per-user on-behalf-of grants (m17.3, ADR 0016 §5): a user consents to an
			// OAuth MCP server → THEIR (user, server) grant is stored; a user revokes
			// their OWN grant. Both are CALLER-SCOPED — the invoking user's identity is
			// resolved from their token (SelfSubjectReview) and the grant Secret writes
			// run as that user, so a user can only touch their own grant. The consent
			// POST starts the m17.2 PKCE flow (marked with the caller's hashed identity)
			// and completes at the shared OAuth callback. The Go 1.22 ServeMux treats
			// "POST /api/mcp/oauth/grant" and "DELETE /api/mcp/oauth/grant/{server}" as
			// distinct patterns.
			authed.HandleFunc("POST /api/mcp/oauth/grant", s.beginMCPGrantConsent)
			authed.HandleFunc("DELETE /api/mcp/oauth/grant/{server}", s.handleRevokeMCPGrant)
			// MCP approval queue (m17.4, ADR 0016 §3): the operator-facing surface for
			// the HARDENED trust mode. GET lists the pending BYO servers awaiting
			// approval; POST .../{ns}/{name} APPROVES one (flips its ToolRegistry entries
			// pending→approved AND opens the per-server egress the register flow withheld —
			// the ONLY transition that opens egress, preserving the m14.6 B1 invariant);
			// POST .../{ns}/{name}/reject DENIES one (removes the pending catalog entry, so
			// it stays non-bindable with no egress). All CALLER-SCOPED (ADR 0011): the
			// approve UPDATE / reject DELETE run as the caller, so a non-operator's action
			// is the API server's real 403 — no bypass. In self-serve mode nothing is
			// pending, so the queue is empty/inert but the endpoints exist and behave
			// honestly. The Go 1.22 ServeMux treats "POST .../{ns}/{name}" and
			// "POST .../{ns}/{name}/reject" as DISTINCT patterns (the more specific
			// "/reject" wins), and both are additive beside the "GET /api/mcp/approvals"
			// list route.
			authed.HandleFunc("GET /api/mcp/approvals", s.handleListMCPApprovals)
			authed.HandleFunc("POST /api/mcp/approvals/{ns}/{name}", s.handleApproveMCP)
			authed.HandleFunc("POST /api/mcp/approvals/{ns}/{name}/reject", s.handleRejectMCP)
		} else {
			authed.Handle("POST /api/mcpservers", notImplemented("caller-scoped MCP register"))
			authed.Handle("GET /api/mcpservers", notImplemented("caller-scoped MCP list"))
			authed.Handle("GET /api/tools", notImplemented("caller-scoped tool catalog"))
			authed.Handle("POST /api/mcp/oauth/grant", notImplemented("caller-scoped MCP grant consent"))
			authed.Handle("DELETE /api/mcp/oauth/grant/{server}", notImplemented("caller-scoped MCP grant revoke"))
			authed.Handle("GET /api/mcp/approvals", notImplemented("caller-scoped MCP approval queue"))
			authed.Handle("POST /api/mcp/approvals/{ns}/{name}", notImplemented("caller-scoped MCP approve"))
			authed.Handle("POST /api/mcp/approvals/{ns}/{name}/reject", notImplemented("caller-scoped MCP reject"))
		}
	}

	api.Handle("/api/", s.requireAuth(authed))

	// MCP OAuth 2.1 callback (m17.2, ADR 0016). Registered on the `api` mux
	// DIRECTLY (a more specific pattern than "/api/"), so it is NOT behind
	// requireAuth's bearer gate — a top-level browser redirect from the
	// authorization server carries no Authorization header. Its authentication is
	// the unguessable, single-use `state` (CSRF token) that resolves the pending
	// flow; the flow itself carries the registering user's token, so the resulting
	// K8s writes run CALLER-SCOPED (ADR 0011). The BFF exchanges the code for tokens
	// SERVER-SIDE and stores them in a Secret — tokens NEVER reach the browser. It
	// is gated by the mcpEnabled kill-switch (like the other MCP routes) so a
	// hardened (MCP-disabled) install serves 404 (the route is simply absent and
	// falls through to the SPA). callerClients is required to complete the K8s
	// writes; absent → honest 501.
	if s.mcpEnabled {
		// The CIMD (Client ID Metadata Document, ADR 0028) is a PUBLIC static doc a
		// CIMD-capable authorization server dereferences to identify this console as
		// an OAuth client (client_id == this URL). No caller auth — the auth server
		// fetches it, not the user — and no callerClients needed (it writes nothing).
		api.HandleFunc("GET /api/mcp/oauth/client-metadata", s.handleMCPOAuthClientMetadata)
		if s.callerClients != nil {
			api.HandleFunc("GET /api/mcp/oauth/callback", s.handleMCPOAuthCallback)
		} else {
			api.Handle("GET /api/mcp/oauth/callback", notImplemented("caller-scoped MCP OAuth callback"))
		}
	}

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
