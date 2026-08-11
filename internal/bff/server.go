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
	"bytes"
	"html"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/agent-engine/internal/controlplane/agentmemory"
	"github.com/ctxmesh/agent-engine/internal/controlplane/alertstore"
	"github.com/ctxmesh/agent-engine/internal/controlplane/auditlog"
	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
	"github.com/ctxmesh/agent-engine/internal/controlplane/costrollup"
	"github.com/ctxmesh/agent-engine/internal/controlplane/dataset"
	"github.com/ctxmesh/agent-engine/internal/controlplane/knowledge"
	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/agent-engine/internal/controlplane/onlinescore"
	"github.com/ctxmesh/agent-engine/internal/controlplane/promptversion"
	"github.com/ctxmesh/agent-engine/internal/controlplane/publishedartifact"
	"github.com/ctxmesh/agent-engine/internal/controlplane/sharedrun"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
	"github.com/ctxmesh/agent-engine/internal/credplane"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
	"github.com/ctxmesh/agent-engine/internal/objectstore"
	"github.com/ctxmesh/agent-engine/internal/prompt"
	"github.com/ctxmesh/agent-engine/internal/run"
	"github.com/ctxmesh/agent-engine/internal/runcap"
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

	// runStore holds durable runs (ADR 0034 execution contract). Phase 1 is a hot in-memory
	// store; M32 swaps a durable backend behind the same seam. Always non-nil (defaulted).
	runStore run.Store

	// convStore holds the conversation → active-agent pointer (M67, ADR 0060 §5). Handoff (handoff_to)
	// terminates A's run and sets this pointer to B, so the conversation's NEXT turn routes to B. Always
	// non-nil (defaulted to a mem twin) — durable Postgres when a run-store DSN is set, so a handoff
	// survives a restart and the next turn (on any pod) routes to the active agent.
	convStore run.ConversationStore

	// promptStore, when set, is the control-plane Postgres store for PromptVersions (ADR 0042, m40.4).
	// During the migration window the BFF DUAL-WRITES: the PromptVersion CRD stays the source of truth
	// (RBAC-gated by the caller-scoped write) and the store is mirrored best-effort after each write, so
	// a store hiccup never fails a CRUD the CRD accepted. Reads still come from the CRD until backfill +
	// the read-switch land (m40.5). nil ⇒ CRD-only (CONTROLPLANE_DSN unset) — behaviour unchanged.
	promptStore promptversion.Store

	// toolRegistryStore is the control-plane Postgres store for ToolRegistries — the
	// source of truth (ToolRegistry is retired as a CRD, ADR 0044). Required for the
	// ToolRegistry + MCP-server APIs; nil ⇒ those endpoints return 501.
	toolRegistryStore toolregistry.Store

	// namespaceTenantStore is the read side of the namespace→tenant membership mirror (ADR 0067 §6,
	// m73.3). Used by GET /api/catalog to resolve tenant membership without BFF RBAC on namespaces
	// (ADR 0011). nil ⇒ the catalog degrades to own-ns + public only (fail-closed, never a panic).
	namespaceTenantStore namespacetenant.Store

	// publishedArtifactStore is the control-plane Postgres store for published_artifacts — the
	// immutable, versioned snapshot-at-publish table (M74, m74.1, ADR 0068 §1). POST /api/templates
	// snapshots an agent's source-spec into it (caller-scoped GET + INSERT, no BFF-SA RBAC); a later
	// GET /api/templates (m74.2) + fork (m74.3) read it. nil ⇒ the publish/unpublish endpoints return
	// 501 (CONTROLPLANE_DSN unset), never a panic.
	publishedArtifactStore publishedartifact.Store

	// sharedRunStore is the control-plane Postgres store for shared_runs — the single-run capability link
	// (M75, m75.1, ADR 0069 §1). POST /api/runs/{id}/shares mints a revocable, expiring share (caller-scoped
	// authz + a hash-only record); DELETE/GET manage it; the m75.2 public read looks a run up by token hash.
	// nil ⇒ the share endpoints return 501 (CONTROLPLANE_DSN unset), never a panic.
	sharedRunStore sharedrun.Store

	// sharedRunLimiter is the per-IP token-bucket that bounds the UNAUTHENTICATED public read
	// (GET /api/shared/runs/{token}, m75.2). 256-bit tokens make brute force moot, but the endpoint is
	// anonymous so it is not left unbounded — over budget → 429 (a non-oracle status). Always non-nil
	// (built in NewServer); its allow() is a no-op when disabled.
	sharedRunLimiter *ipRateLimiter

	// docStore is the durable KB object store (M68, ADR 0061 Fork 4) used by the
	// BFF document-upload endpoint and the m68.6 source-resolution seam. nil when
	// OBJECT_STORE_ADDR is unset — the upload endpoint returns 501 honestly rather
	// than panicking. Constructed once in cmd/bff/main.go.
	docStore objectstore.ObjectStore

	// knowledgeStore is the control-plane pgvector store for the managed RAG corpus (M68, ADR 0061 Fork 1),
	// built from cpDB. The ingestion executor (m68.6) WRITES it DIRECTLY — EnsureCorpus/Upsert/SweepOrphans —
	// because the run-worker is a trusted control-plane workload that holds the controlplane DSN (governance
	// #8: write-direct-from-worker). nil ⇒ the ingest endpoint + executor degrade honestly (501 / a clear
	// failed-run reason), never a panic. Constructed in cmd/bff/main.go from CONTROLPLANE_DSN.
	knowledgeStore knowledge.Store
	// embedder embeds chunk texts via the model gateway (M68, ADR 0061 Fork 2) for the ingestion executor's
	// direct write path (governance #8 — the trusted worker embeds + writes, agent pods do not). nil when
	// MODEL_GATEWAY_URL is unset ⇒ the ingest endpoint + executor degrade honestly. Constructed in
	// cmd/bff/main.go via credplane.NewGatewayEmbedder (the same gateway seam the token-service memory uses).
	embedder credplane.Embedder

	// datasetStore is the control-plane Postgres store for eval datasets (M69, ADR 0062 Fork 1), built from
	// cpDB. The dataset-export executor (m69.2) WRITES it DIRECTLY — EnsureDataset/AppendCase — copying
	// M66-redacted, traceId-lineaged cases out of Langfuse (governance #8: the run-worker is a trusted
	// control-plane workload holding cpDB + Langfuse creds; agent pods never touch this). nil ⇒ the export
	// endpoint + executor degrade honestly (501 / a clear failed-run reason), never a panic. Constructed in
	// cmd/bff/main.go from CONTROLPLANE_DSN.
	datasetStore dataset.Store

	// onlineStore is the control-plane Postgres store for per-agent-version online score aggregates
	// (M69, ADR 0062 Fork 2), built from cpDB. The online-scoring worker (m69.5) WRITES it DIRECTLY —
	// UpsertAggregate — folding production traces (operational + feedback + judge) into the
	// per-(namespace, agent, version, window) vector (governance #8: the trusted off-request worker holds
	// cpDB + Langfuse creds; agent pods never touch this). nil ⇒ the online-scoring worker does not start
	// (cmd/bff/main.go gates on it), so a missing store is an honest no-op, never a panic.
	onlineStore onlinescore.Store

	// rollupStore is the control-plane Postgres store for the durable cost-rollup ledger
	// (M70, ADR 0063 D1). The cost-rollup worker (m70.2) WRITES it DIRECTLY — Upsert — snapshotting
	// the ephemeral Valkey monthly-spend keys and the Langfuse per-agent cost breakdown into a
	// date-keyed series (governance #8: the trusted off-request worker holds cpDB + Langfuse + Valkey
	// creds; agent pods never touch this). nil ⇒ the worker is a safe no-op, never a panic.
	rollupStore costrollup.Store

	// judgeCounters bounds the online-scoring worker's SAMPLED judge to a per-(agent, day) cost cap
	// (m69.5, ADR 0062 Fork 2): an in-memory best-effort counter that resets lazily when the date rolls.
	// Always non-nil (constructed in NewServer) so the worker never nil-derefs it.
	judgeCounters *judgeCounter

	// onlineResolver resolves the PER-AGENT online-scoring policy from the agent's EvalSuite.online block
	// (ADR 0062 Fork 2, m69.6). nil ⇒ the online-scoring worker uses its process-wide config for every
	// agent (m69.5 back-compat); a real k8sOnlineConfigResolver (a read-only client over AgentDeployment +
	// EvalSuite) is wired in cmd/bff/main.go when the worker is enabled. A resolve error falls back to the
	// process defaults for that agent — never a fabricated verdict.
	onlineResolver OnlineConfigResolver

	// agentMemoryStore is the control-plane pgvector store for `agent`/long-term memory (ADR 0045) —
	// the console read path (list an agent's memories). nil ⇒ the memory endpoint returns 501.
	agentMemoryStore agentmemory.Store
	// auditStore appends the BFF's security events to the audit_log (ADR 0056, M63). nil ⇒ no-op.
	auditStore auditlog.Store
	// alertStore is the control-plane store for fired alerts (M70, ADR 0063 D2). The AlertPolicy
	// reconciler appends one Alert per false→true condition transition and resolves it on true→false.
	// The console reads it via GET /api/alerts. nil ⇒ the endpoint returns 501 (CONTROLPLANE_DSN absent).
	alertStore alertstore.Store

	// tenantUsage reads a tenant's LIVE quota consumption from the shared state-layer Valkey (M49). nil ⇒
	// the tenant usage endpoint returns 501 (no state-layer configured).
	tenantUsage TenantUsageReader

	// runControl publishes the run-scoped CONTROL marker to the shared state-layer Valkey on cancel
	// (m70.8, the real-kill cancel channel). nil ⇒ no STATELAYER_ADDR: cancel degrades to the durable
	// status flip alone (soft cancel), never an error — the marker is only the accelerator.
	runControl RunControlPublisher

	// authorizer gates a store-backed access (ADR 0042 Amendment 4, m43.4 reads / m44.2 writes): once the
	// API server is no longer in the path for a Postgres-backed entity, the BFF authorizes with a
	// caller-scoped SSAR (exact RBAC parity with the CRD path). Always non-nil (defaulted to
	// authz.SSARAuthorizer{}); tests inject a fake to drive allow/deny deterministically.
	authorizer authz.Authorizer
	// runWorkerDispatch, when true, makes POST /runs leave the run `queued` for a KEDA-scaled
	// worker pool to claim + execute (m32.2) instead of running it in-process. Requires a durable
	// runStore (a hot store is per-pod, so a worker on another pod could not see the run).
	runWorkerDispatch bool

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

	// consoleURL is the canonical, browser-reachable console origin (scheme://host[:port]),
	// e.g. "https://console.agents.example.com" — the ONE origin whose /api/mcp/oauth/callback
	// is registered with MCP providers (ADR 0040). MCP consent uses it as the server-controlled
	// redirect_uri for EVERY flow regardless of the initiating origin, so consent opened from an
	// agent's own hostname uses a redirect the provider recognizes; and it is the only origin the
	// cross-origin "connected" relay trusts. Empty ⇒ single-origin behaviour (the request's own
	// origin is the callback, no cross-origin relay). Trailing slash trimmed. Wired from
	// CONSOLE_URL in cmd/bff/main.go.
	consoleURL string

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

	// credentialNamespace + credentialClient move MCP grant Secrets out of the
	// per-request (agent) namespace into ONE RBAC-locked platform namespace (m25.1b,
	// ADR 0029 §7). When both are set, grant write/read/delete route through the
	// privileged, namespace-scoped credentialClient against credentialNamespace — a
	// bounded, deliberate exception to "the BFF SA never acts on user objects"
	// (cmd/bff/main.go): it touches ONLY Secrets in this locked namespace, keyed by
	// the authenticated caller's OWN identity, never a user CRD. When unset the BFF
	// keeps the legacy caller-scoped, per-namespace grant path (ADR 0011). Wired from
	// MCP_CREDENTIAL_NAMESPACE in cmd/bff/main.go.
	credentialNamespace string
	credentialClient    client.Client

	// capabilitySigner mints the short-lived RUN CAPABILITY (runcap, ADR 0030 §2) at an
	// authenticated /invoke: an EdDSA-signed JWT carrying the invoking user's hashed
	// identity that the credential plane verifies to resolve THAT user's OBO credential.
	// nil ⇒ minting DISABLED (no platform capability key configured) — the invoke carries
	// no capability and a downstream tool call resolves as unattended (org/public only),
	// never another user's grant. Built in NewServer from MCP_CAPABILITY_PRIVATE_KEY.
	capabilitySigner *runcap.Signer

	// tokenServiceURL is the base URL of the central token-service (e.g.
	// "http://token-service:8443"), used by the KB test-query endpoint (m68.13) to
	// proxy POST /v1/knowledge/search. Empty ⇒ the search endpoint returns 501 honestly
	// rather than panicking. Wired from TOKEN_SERVICE_URL in cmd/bff/main.go.
	tokenServiceURL string
	// tokenServiceClient is the mTLS http.Client the KB test-query endpoint uses to reach the
	// token-service (the same client the grant-delegation path uses). nil ⇒ http.DefaultClient
	// (dev, plain HTTP). The token-service serves mTLS in prod, so this must not be the default client.
	tokenServiceClient *http.Client

	// grantStore, when set, DELEGATES the OAuth-callback grant persist to the central
	// token-service (credplane.Client) so the grant lands in the CONFIG-SELECTED backend
	// (ADR 0032) — Postgres / remote-vault creds stay in the token-service, not this
	// user-facing BFF. nil ⇒ the BFF writes the grant Secret directly (the kubernetes
	// default / dev). Built in NewServer from TOKEN_SERVICE_URL.
	grantStore credresolve.GrantWriter

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
	// ConsoleURL is the canonical browser-reachable console origin used as the one
	// registered MCP-consent redirect_uri + the trusted cross-origin relay target
	// (ADR 0040). Empty ⇒ single-origin behaviour. Wired from CONSOLE_URL.
	ConsoleURL string
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
	// MCPCredentialNamespace is the locked platform namespace that holds every MCP
	// grant Secret when set (m25.1b, ADR 0029 §7). Empty ⇒ legacy per-request-namespace
	// grants. Wired from MCP_CREDENTIAL_NAMESPACE in cmd/bff/main.go.
	MCPCredentialNamespace string
	// CredentialClient is the privileged, namespace-scoped client used to write/read/
	// delete grant Secrets in MCPCredentialNamespace (the BFF's own SA — tenants have
	// no RBAC there). Built in cmd/bff/main.go only when MCPCredentialNamespace is set;
	// nil ⇒ the legacy caller-scoped path. See the credentialNamespace field note for
	// why this bounded SA use does not reopen the confused-deputy gap.
	CredentialClient client.Client
	// TokenServiceURL is the base URL of the central token-service (e.g.
	// "http://token-service:8443"). When set, the KB test-query endpoint proxies
	// POST /v1/knowledge/search to this URL (m68.13). Empty ⇒ the search endpoint
	// returns 501 honestly. Wired from TOKEN_SERVICE_URL in cmd/bff/main.go.
	TokenServiceURL string

	// TokenServiceHTTPClient is the mTLS client for the token-service (the KB test-query endpoint,
	// m68.13). nil ⇒ http.DefaultClient (dev). Wired from tokenServiceHTTPClient in cmd/bff/main.go.
	TokenServiceHTTPClient *http.Client

	// GrantStore, when set, DELEGATES the OAuth-callback grant persist to the central
	// token-service so grants land in the config-selected backend (ADR 0032). nil ⇒ the BFF
	// writes the grant Secret directly (kubernetes default). Built in cmd/bff/main.go from
	// TOKEN_SERVICE_URL (a credplane.Client over mTLS).
	GrantStore credresolve.GrantWriter
	// MCPCapabilityPrivateSeedB64 is the base64-encoded Ed25519 private seed the BFF signs
	// run capabilities with (runcap, ADR 0030 §2/§5) — a per-cluster platform Secret. Empty
	// ⇒ capability minting is DISABLED (an /invoke carries no capability header). Wired from
	// MCP_CAPABILITY_PRIVATE_KEY in cmd/bff/main.go.
	MCPCapabilityPrivateSeedB64 string
	// MCPCapabilityAudience is the credential-plane audience minted capabilities target
	// (their `aud`); the sidecar / central token service verify against the same value.
	// Defaults to defaultCapabilityAudience when empty. Wired from MCP_CAPABILITY_AUDIENCE.
	MCPCapabilityAudience string
	// PromptResolver is the OPTIONAL server-side resolver for git-pointer PromptVersions
	// (m17.8). When nil (the default), the diff endpoint returns an honest 501
	// ("prompt resolution not configured"). Wire a FixtureResolver in tests and a
	// real go-git Resolver in production. The BFF never stores resolved prompt content
	// beyond the diff response (ADR 0008).
	PromptResolver prompt.Resolver
	// Log is the structured logger.
	// RunStore backs the run-oriented execution contract (ADR 0034). Optional — a hot in-memory
	// store is used when nil (phase 1); M32 injects a durable store.
	RunStore run.Store
	// ConvStore backs the conversation → active-agent pointer for handoff (M67, ADR 0060 §5). Optional —
	// a hot in-memory twin is used when nil (dev/single-pod); a durable store is injected alongside the
	// durable RunStore (same Postgres) in cmd/bff/main.go so handoff routing survives a restart.
	ConvStore run.ConversationStore
	// PromptStore is the control-plane Postgres store for PromptVersions (ADR 0042, m40.4). Optional —
	// nil ⇒ CRD-only. Wired from CONTROLPLANE_DSN in cmd/bff/main.go.
	PromptStore promptversion.Store
	// ToolRegistryStore is the control-plane Postgres store for ToolRegistries — the
	// source of truth (ToolRegistry is retired as a CRD, ADR 0044). Wired from
	// CONTROLPLANE_DSN in cmd/bff/main.go; nil ⇒ the ToolRegistry/MCP APIs serve 501.
	ToolRegistryStore toolregistry.Store
	// NamespaceTenantStore is the read side of the namespace→tenant membership mirror (ADR 0067 §6,
	// m73.3). Wired from CONTROLPLANE_DSN in cmd/bff/main.go alongside ToolRegistryStore. nil ⇒
	// GET /api/catalog degrades to own-ns + public only (fail-closed), never a panic.
	NamespaceTenantStore namespacetenant.Store
	// PublishedArtifactStore is the control-plane Postgres store for published_artifacts — the
	// snapshot-at-publish table (M74, m74.1, ADR 0068 §1). Wired from CONTROLPLANE_DSN in
	// cmd/bff/main.go alongside NamespaceTenantStore. nil ⇒ POST/DELETE /api/templates return 501.
	PublishedArtifactStore publishedartifact.Store
	// SharedRunStore is the control-plane Postgres store for shared_runs — the single-run capability link
	// (M75, m75.1, ADR 0069 §1). Wired from CONTROLPLANE_DSN in cmd/bff/main.go alongside PublishedArtifactStore.
	// nil ⇒ the /api/runs/{id}/shares endpoints return 501.
	SharedRunStore sharedrun.Store
	// TenantUsage reads a tenant's live quota consumption from the shared state-layer Valkey (M49). Optional —
	// nil ⇒ the tenant usage endpoint returns 501.
	TenantUsage TenantUsageReader

	// RunControl publishes the run-scoped CONTROL marker to the shared state-layer Valkey on cancel (m70.8,
	// the real-kill cancel channel). Optional — nil ⇒ cancel degrades to the durable status flip alone
	// (soft cancel). Constructed in cmd/bff/main.go from STATELAYER_ADDR (the same addr the usage reader uses).
	RunControl RunControlPublisher

	// AgentMemoryStore is the control-plane pgvector store for long-term memory (ADR 0045). Optional —
	// nil ⇒ the console memory endpoint returns 501.
	AgentMemoryStore agentmemory.Store
	// AuditStore is the control-plane store the BFF appends security events to (connect / grant.create /
	// grant.revoke + denials) with the PRECISE authenticated actor (ADR 0056, M63). Optional — nil ⇒ the
	// BFF audit writes no-op (the controller still persists CRD mutations) and GET /api/audit returns 501.
	AuditStore auditlog.Store
	// AlertStore is the control-plane store for fired alerts (M70, ADR 0063 D2). Optional — nil ⇒
	// GET /api/alerts returns 501 (CONTROLPLANE_DSN absent). Constructed in cmd/bff/main.go from cpDB.
	AlertStore alertstore.Store
	// RunWorkerDispatch routes POST /runs execution to a KEDA-scaled worker pool (m32.2) instead of
	// running it in-process. Only meaningful with a durable RunStore; ignored otherwise.
	RunWorkerDispatch bool

	// DocStore is the durable KB object store (M68, ADR 0061 Fork 4). Optional — nil when
	// OBJECT_STORE_ADDR is unset; the BFF upload endpoint returns honest 501 rather than panicking.
	// Constructed in cmd/bff/main.go via objectstore.NewMinioStore().
	DocStore objectstore.ObjectStore

	// KnowledgeStore is the control-plane pgvector store for the managed RAG corpus (M68, ADR 0061 Fork 1),
	// which the ingestion executor writes directly (governance #8). Optional — nil ⇒ the ingest endpoint +
	// executor degrade honestly. Constructed in cmd/bff/main.go from CONTROLPLANE_DSN (cpDB).
	KnowledgeStore knowledge.Store
	// DatasetStore is the control-plane Postgres store for eval datasets (M69, ADR 0062 Fork 1), which the
	// dataset-export executor writes directly (governance #8). Optional — nil ⇒ the export endpoint +
	// executor degrade honestly (501 / a clear failed-run reason). Constructed in cmd/bff/main.go from cpDB.
	DatasetStore dataset.Store
	// OnlineStore is the control-plane Postgres store for online score aggregates (M69, ADR 0062 Fork 2),
	// which the online-scoring worker (m69.5) writes directly (governance #8). Optional — nil ⇒ the
	// online-scoring worker does not start (an honest no-op). Constructed in cmd/bff/main.go from cpDB.
	OnlineStore onlinescore.Store
	// OnlineResolver resolves the per-agent online-scoring policy from the EvalSuite.online block (ADR 0062
	// Fork 2, m69.6). Optional — nil ⇒ the worker uses its process-wide config for every agent (m69.5).
	// Constructed in cmd/bff/main.go (a read-only client over AgentDeployment + EvalSuite) only when the
	// online-scoring worker is enabled.
	OnlineResolver OnlineConfigResolver
	// RollupStore is the control-plane Postgres store for the durable cost-rollup ledger (M70, ADR 0063 D1),
	// which the cost-rollup worker (m70.2) writes directly (governance #8). Optional — nil ⇒ the worker is
	// a safe no-op. Constructed in cmd/bff/main.go from cpDB.
	RollupStore costrollup.Store
	// Embedder embeds chunk texts via the model gateway for the ingestion executor (M68, ADR 0061 Fork 2).
	// Optional — nil when MODEL_GATEWAY_URL is unset ⇒ the ingest endpoint + executor degrade honestly.
	// Constructed in cmd/bff/main.go via credplane.NewGatewayEmbedder.
	Embedder credplane.Embedder

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
		consoleURL:               strings.TrimRight(strings.TrimSpace(opts.ConsoleURL), "/"),
		providerConnect:          opts.ProviderConnect,
		providerHTTP:             opts.ProviderHTTP,
		platformGenerationModels: opts.PlatformGenerationModels,
		mcpEnabled:               opts.MCPEnabled,
		mcpRequireApproval:       opts.MCPRequireApproval,
		credentialNamespace:      opts.MCPCredentialNamespace,
		credentialClient:         opts.CredentialClient,
		tokenServiceURL:          strings.TrimRight(strings.TrimSpace(opts.TokenServiceURL), "/"),
		tokenServiceClient:       opts.TokenServiceHTTPClient,
		grantStore:               opts.GrantStore,
		oauthFlows:               newPendingOAuthStore(),
		promptResolver:           opts.PromptResolver,
		runStore:                 opts.RunStore,
		convStore:                opts.ConvStore,
		promptStore:              opts.PromptStore,
		toolRegistryStore:        opts.ToolRegistryStore,
		namespaceTenantStore:     opts.NamespaceTenantStore,
		publishedArtifactStore:   opts.PublishedArtifactStore,
		sharedRunStore:           opts.SharedRunStore,
		sharedRunLimiter:         newIPRateLimiter(sharedRunRatePerIP, sharedRunBurstPerIP),
		agentMemoryStore:         opts.AgentMemoryStore,
		auditStore:               opts.AuditStore,
		alertStore:               opts.AlertStore,
		tenantUsage:              opts.TenantUsage,
		runControl:               opts.RunControl,
		authorizer:               authz.SSARAuthorizer{},
		runWorkerDispatch:        opts.RunWorkerDispatch,
		docStore:                 opts.DocStore,
		knowledgeStore:           opts.KnowledgeStore,
		datasetStore:             opts.DatasetStore,
		onlineStore:              opts.OnlineStore,
		onlineResolver:           opts.OnlineResolver,
		rollupStore:              opts.RollupStore,
		embedder:                 opts.Embedder,
		judgeCounters:            &judgeCounter{},
		log:                      opts.Log,
	}
	if s.runStore == nil {
		s.runStore = run.NewMemStore()
	}
	if s.convStore == nil {
		s.convStore = run.NewMemConversationStore()
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
	if s.mcpEnabled && s.credentialNamespace == "" {
		opts.Log.Info("mcp: MCP_CREDENTIAL_NAMESPACE not set — MCP grant Secrets are stored in the per-request " +
			"(agent) namespace (legacy); set a locked platform namespace before production (ADR 0029 §7) so a " +
			"tenant cannot read another user's grant tokens.")
	}
	// Build the run-capability signer from the per-cluster platform key (runcap, ADR 0030
	// §2). Absent ⇒ minting stays disabled (an /invoke carries no capability). A malformed
	// seed is a hard misconfiguration surfaced at start-up, never a silent wrong-key.
	if seed := opts.MCPCapabilityPrivateSeedB64; seed != "" {
		priv, decErr := runcap.DecodePrivateSeed(seed)
		if decErr != nil {
			opts.Log.Error(decErr, "mcp: MCP_CAPABILITY_PRIVATE_KEY is set but invalid — run-capability minting DISABLED")
		} else {
			aud := opts.MCPCapabilityAudience
			if aud == "" {
				aud = defaultCapabilityAudience
			}
			s.capabilitySigner = runcap.NewSigner(priv, aud, nil)
		}
	} else if s.mcpEnabled {
		opts.Log.Info("mcp: MCP_CAPABILITY_PRIVATE_KEY not set — run capabilities are NOT minted; a tool call " +
			"resolves as unattended (org/public only) until the platform capability key + egress sidecar are deployed (ADR 0030).")
	}
	return s
}

// lockedCredentials reports whether grant Secrets are consolidated into the RBAC-locked
// credential namespace (m25.1b): true only when BOTH a namespace and its privileged
// client are wired, so the coordinate + client decisions can never disagree. False ⇒
// the legacy caller-scoped, per-request-namespace grant path (ADR 0011).
func (s *Server) lockedCredentials() bool {
	return s.credentialNamespace != "" && s.credentialClient != nil
}

// grantCoordinates resolves a grant Secret's (namespace, name) for the current mode
// (locked → the credential namespace with the source ns folded in; legacy → the source
// namespace, original name). The single call the write/delete paths share so they land
// on the exact object the OBO resolver will later read.
func (s *Server) grantCoordinates(sourceNs, boundary, server, userHash string) (namespace, name string) {
	credNs := ""
	if s.lockedCredentials() {
		credNs = s.credentialNamespace
	}
	return grantSecretCoordinates(credNs, sourceNs, boundary, server, userHash)
}

// grantClient returns the client that writes/reads/deletes a grant Secret: the
// privileged credential-component client in locked mode (tenants have no RBAC in the
// credential namespace), else the caller-scoped client (legacy, ADR 0011).
func (s *Server) grantClient(caller client.Client) client.Client {
	if s.lockedCredentials() {
		return s.credentialClient
	}
	return caller
}

// grantSourceNSLabel is the source-namespace label value to stamp: the origin
// namespace in locked mode (the authoritative match key there), "" in legacy mode.
func (s *Server) grantSourceNSLabel(sourceNs string) string {
	if s.lockedCredentials() {
		return sourceNs
	}
	return ""
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
	// Shared-run public read (M75, m75.2, ADR 0069 §1/§2) — the platform's FIRST genuinely
	// UNAUTHENTICATED read surface. Mounted on the `api` mux DIRECTLY (a more specific pattern
	// than "/api/"), so it is NOT behind requireAuth: a logged-out visitor with only the share
	// token reads ONE run's allowlist projection. The token IS the capability; there is no caller.
	// Uniform 404 at every failure (no oracle), the newSharedRunView allowlist projection only,
	// no-referrer/noindex headers, per-IP rate limit, and NO token in any log (see handler). A
	// nil store (no cpDB) returns 404 — never 501 — so an anonymous caller cannot learn the
	// feature exists. Only GET is registered; a POST/DELETE/… on this path does not match the
	// GET-only pattern and falls to the "/api/" catch-all → requireAuth → 401 (no bearer). Either
	// way a non-GET verb NEVER reaches the projection — the read is GET-only.
	api.HandleFunc("GET /api/shared/runs/{token}", s.handleSharedRunPublic)

	s.registerSpawnRoute(api)
	s.registerHandoffRoute(api)
	// Guardrail block ingest (m66.9, ADR 0059 §9): capability-authorized durable compliance record.
	// Wired alongside the spawn edge — both are internal launcher-to-BFF endpoints authenticated on
	// the run capability, not a browser bearer token.
	s.registerGuardrailEventRoute(api)

	// Authenticated surface. The CRD routes run through the CALLER-SCOPED client
	// (ADR 0011): list/create/topology reflect exactly what the caller's own RBAC
	// permits, enforced by the K8s API server. They are wired only when the
	// caller-client factory is configured; honest 501 otherwise (the BFF never
	// falls back to its own SA for user CRD ops — that is the confused-deputy gap
	// this task closes). Create additionally needs the scheme to decode manifests.
	authed := http.NewServeMux()
	if s.callerClients != nil {
		authed.HandleFunc("GET /api/agents", s.handleListAgents)
		// Eval-gated deploys metric (M69, ADR 0062 governance #2): the PRD §5
		// ">50% of production deploys gated by an EvalSuite" counter. Caller-scoped
		// (ADR 0011): reads AgentDeployments through the caller's own token — the K8s
		// API server's RBAC decides visibility, not the BFF SA. No new RBAC grant.
		authed.HandleFunc("GET /api/metrics/eval-gated", s.handleEvalGatedMetric)
		// Agent detail + live log tail (m14.7, first-agent-flow.md §3). Both run
		// through the CALLER-SCOPED client (ADR 0011): the detail read + the SSE
		// pod-log stream act as the caller, so K8s RBAC governs them. The Go 1.22
		// ServeMux treats "GET /api/agents", "GET /api/agents/{ns}/{name}" and
		// ".../{name}/logs" as three DISTINCT patterns (the more specific wins), so
		// these never shadow the list route above or the create route below.
		authed.HandleFunc("GET /api/agents/{ns}/{name}", s.handleAgentDetail)
		authed.HandleFunc("GET /api/agents/{ns}/{name}/logs", s.handleAgentLogs)
		// Audit surface (M63, ADR 0056): the compliance persona reads the audit trail.
		// Caller-scoped SSAR on the `auditlogs` resource (persona gate); nil store ⇒ 501.
		authed.HandleFunc("GET /api/audit", s.handleListAudit)
		// Alerts feed (M70, ADR 0063 D2): the fired-alert console feed — newest-first,
		// namespace-scoped. Caller-scoped SSAR on `alertpolicies` (same resource the CRD
		// path enforced); nil store ⇒ 501 (CONTROLPLANE_DSN absent). Read-only.
		authed.HandleFunc("GET /api/alerts", s.handleListAlerts)
		// Cost forecast (M70, ADR 0063 D3): linear run-rate month-end projection from
		// the durable cost-rollup ledger. Caller-scoped SSAR on `costrollups` (persona
		// gate â no per-row leak). nil store â 501. ?tenant= required.
		authed.HandleFunc("GET /api/cost/forecast", s.handleCostForecast)
		// Cost chargeback (M70, ADR 0063 D3): per-day rollup export for a calendar month.
		// CSV when Accept: text/csv or ?format=csv, else JSON. Caller-scoped SSAR on
		// `costrollups`. nil store â 501. ?tenant= and ?period=YYYY-MM required.
		authed.HandleFunc("GET /api/cost/chargeback", s.handleCostChargeback)
		// Redaction-policy editor (m18.13, ADR 0019): read/replace the agent's custom
		// trace-redaction detectors. Both caller-scoped; the PUT is enforced by the
		// API server (a viewer without update is denied → 403).
		authed.HandleFunc("GET /api/agents/{ns}/{name}/tracepolicy", s.handleGetTracePolicy)
		authed.HandleFunc("PUT /api/agents/{ns}/{name}/tracepolicy", s.handleUpdateTracePolicy)
		// Long-term memory ENABLE surface (m49.3, closing the m49.1 pocket): configure the folded
		// spec.longTermMemory capability directly (the tracepolicy pattern), caller-scoped.
		authed.HandleFunc("GET /api/agents/{ns}/{name}/longtermmemory", s.handleGetLongTermMemoryConfig)
		authed.HandleFunc("PUT /api/agents/{ns}/{name}/longtermmemory", s.handleUpdateLongTermMemory)
		// Long-term memory viewer (m46.6, ADR 0045): list an agent's `agent`-scope memories. Caller-scoped
		// (the caller must be able to `get` the agent) then a store read. 501 when no memory store is wired.
		authed.HandleFunc("GET /api/agents/{ns}/{name}/memory", s.handleAgentMemory)
		// Online-score surface (m69.11, ADR 0062 Fork 2): the improvement-loop production signal for the
		// agent detail page. Caller-scoped (ADR 0011): caller-Get gates access, cpDB read returns the
		// 3-component (operational/feedback/judge) per-version aggregates. 501 when the store is absent.
		authed.HandleFunc("GET /api/agents/{ns}/{name}/online-score", s.handleAgentOnlineScore)
		// Rollback (m69.11, ADR 0062 Fork 4): set the agents.ctxmesh.ai/rollback=<version> annotation
		// on the AgentDeployment via the CALLER'S client (caller-scoped PATCH - ADR 0011). The rollback
		// controller (m69.8) actuates the guarded spec revert; this endpoint only sets the annotation.
		authed.HandleFunc("POST /api/agents/{ns}/{name}/rollback", s.handleAgentRollback)
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
		// Fork = install-from-template = the ONE create path (M74, m74.3, ADR 0068 §4/§6):
		// duplicate an OWN agent in-place or install a cross-namespace PUBLISHED one, always
		// into the CALLER's own namespace, stamping fork-origin provenance. The origin
		// source-spec is resolved caller-scoped (own-ns live GET) or from the published
		// snapshot (cross-ns, with a discoverability re-check gate → 404), then forked through
		// createAgentFromYAML — no parallel fork subsystem. Like the edit route it needs the
		// scheme (to decode/apply the expanded manifests), so it shares this scheme guard —
		// absent scheme → an honest 501 for both. The Go 1.22 ServeMux treats this sub-path
		// pattern as MORE SPECIFIC than the {ns}/{name} GET/PUT/DELETE routes below, so it
		// never shadows them.
		if s.scheme != nil {
			authed.HandleFunc("PUT /api/agents/{ns}/{name}", s.handleUpdateAgent)
			authed.HandleFunc("POST /api/agents/{ns}/{name}/fork", s.handleForkAgent)
		} else {
			authed.Handle("PUT /api/agents/{ns}/{name}", notImplemented("agent edit"))
			authed.Handle("POST /api/agents/{ns}/{name}/fork", notImplemented("agent fork"))
		}
		// Agent DELETE (m15.4, ADR 0017): remove the AgentDeployment via the
		// CALLER-SCOPED client (ADR 0011). Owned children are garbage-collected by
		// Kubernetes; independent references (agentRef-only, no ownerRef) are left in
		// place — orphan pruning is deferred. A viewer's DELETE surfaces the API
		// server's real 403; no RBAC pre-emption. The Go 1.22 ServeMux treats
		// "DELETE .../{ns}/{name}" as a DISTINCT pattern from the GET/PUT above.
		authed.HandleFunc("DELETE /api/agents/{ns}/{name}", s.handleDeleteAgent)
		// Publish a draft agent (ADR 0065 D1 — draft early, iterate live, publish
		// when done): removes the agents.ctxmesh.ai/stage=draft label from the
		// AgentDeployment so it becomes visible to the default list and team/registry
		// consumption. Idempotent (already-published → 200 no-op). Caller-scoped
		// (ADR 0011): the Get+Patch run through the caller's token; a viewer's Patch
		// surfaces as 403. The Go 1.22 ServeMux treats this sub-path pattern as MORE
		// SPECIFIC than "DELETE .../{ns}/{name}" and "GET .../{ns}/{name}", so it
		// never shadows those routes.
		authed.HandleFunc("POST /api/agents/{ns}/{name}/publish", s.handlePublishAgent)
		// Snapshot-at-publish: publish an agent's source-spec as an immutable, versioned
		// template into published_artifacts (M74, m74.1, ADR 0068 §1). POST snapshots the
		// caller-scoped source-spec (a GET of the agent authorizes it — no BFF-SA RBAC);
		// DELETE tombstones every version (idempotent). Both are nil-safe: a BFF without the
		// published-artifact store serves 501, never a panic. The {kind}/{namespace}/{name}
		// DELETE pattern is distinct from POST /api/templates so the two never conflict.
		authed.HandleFunc("POST /api/templates", s.handlePublishTemplate)
		authed.HandleFunc("DELETE /api/templates/{kind}/{namespace}/{name}", s.handleUnpublishTemplate)
		// Cross-tenant template gallery (M74, m74.2, ADR 0068 §2/§3): Go-embedded recipes ∪
		// published agents visible to the caller's tenant. Gate: caller-scoped SSAR `list
		// agentdeployments` in callerNS (membership proof — amended-ADR-0011 model; NO BFF-SA
		// RBAC grant; SelfSubjectAccessReview is a self-check the caller's token authorizes).
		// The Go 1.22 ServeMux treats "GET /api/templates" as distinct from POST + DELETE above.
		authed.HandleFunc("GET /api/templates", s.handleTemplates)
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

		// KnowledgeBase list + detail (m68.13): read the caller's KB CRs — name, phase, counts.
		// Caller-scoped (ADR 0011): the BFF lists/gets through the caller's own client so K8s RBAC
		// governs who can read KBs. 403 when denied; 404 on a missing KB (detail only).
		authed.HandleFunc("GET /api/knowledgebases", s.handleListKBs)
		authed.HandleFunc("GET /api/knowledgebases/{name}", s.handleGetKB)

		// KnowledgeBase test-query (m68.13): the console test-query panel — verify the KB exists
		// (caller-scoped, 404 if absent) then forward to the token-service /v1/knowledge/search
		// with the KB's embeddingRoute. Returns ranked chunks + citations. 501 when the token-service
		// is unconfigured (TOKEN_SERVICE_URL unset).
		authed.HandleFunc("POST /api/knowledgebases/{name}/search", s.handleSearchKB)

		// KnowledgeBase document upload (M68, ADR 0061 Fork 4): stream a raw document body
		// into the durable KB bucket at KnowledgeKey(ns, kbName, filename). Caller-scoped:
		// the BFF verifies the KB exists in the caller's namespace before writing (honest 404
		// when absent; 403 when RBAC denies the GET; 501 when OBJECT_STORE_ADDR is unset;
		// 413 when the body exceeds maxDocumentUploadBytes). Returns 201 + {documentRef, key, size}.
		authed.HandleFunc("POST /api/knowledgebases/{name}/documents", s.handleUploadKBDocument)

		// KnowledgeBase ingestion trigger (M68, ADR 0061 Fork 2): resolve the KB's source documents,
		// pin an IngestionSpec, and create a durable ingestion Run the worker drives (extract → chunk →
		// embed → upsert, cursor-resumable). Caller-scoped (404 when the KB is absent; 501 when the
		// knowledge store / embedder / object store is unwired). Returns 202 + {runId, status, documentCount}.
		authed.HandleFunc("POST /api/knowledgebases/{name}/ingest", s.handleIngestKB)

		// Dataset export trigger (M69, ADR 0062 Fork 1): pin an ExportSpec (target dataset + agent tag +
		// timerange) and create a durable export Run the worker drives — it copies production traces OUT of
		// Langfuse into the control-plane dataset store, M66-REDACTED (the PII P1), with source-trace lineage.
		// Caller-scoped (ADR 0011): the caller's own token gates who can trigger an export. 501 when the dataset
		// store / Langfuse adapter is unwired; 400 on a bad body. Returns 202 + {runId, status}.
		authed.HandleFunc("POST /api/datasets/{name}/export", s.handleExportDataset)

		// Dataset labeling API (M69, ADR 0062 Fork 5 — the improvement loop's human-labeling path).
		// Caller-scoped (ADR 0011): the caller must present a valid token; the author on a label append is
		// derived from the authenticated caller identity (SelfSubjectReview), never a client field.
		// All degrade honestly (501) when the dataset store is not configured.
		//
		// NOTE: Go 1.22 ServeMux: "POST /api/datasets/{name}/cases/from-run" is MORE SPECIFIC than
		// "POST /api/datasets/{name}/cases/{caseId}/labels" because the literal segment "from-run" is
		// longer than the wildcard, so the from-run route wins on that path and the label route never
		// sees it — the two patterns do not conflict.
		authed.HandleFunc("GET /api/datasets", s.handleListDatasets)
		authed.HandleFunc("GET /api/datasets/{name}/cases", s.handleListDatasetCases)
		authed.HandleFunc("POST /api/datasets/{name}/cases/{caseId}/labels", s.handleAppendLabel)
		authed.HandleFunc("POST /api/datasets/{name}/cases/from-run", s.handleFromRun)
		authed.HandleFunc("POST /api/datasets/{name}/pin", s.handlePinDataset)

		// Tenants (M47, ADR 0046): read-only, cluster-scoped, caller-scoped.
		authed.HandleFunc("GET /api/tenants", s.handleListTenants)
		// Batched live usage for ALL listable tenants (m54.5) — the near-cap indicator
		// on the list. Registered before the {name} routes; the literal "usage" segment
		// takes precedence over the {name} wildcard (Go 1.22 ServeMux).
		authed.HandleFunc("GET /api/tenants/usage", s.handleTenantUsageList)
		authed.HandleFunc("GET /api/tenants/{name}", s.handleGetTenant)
		// Live usage vs cap (M49, the M47-review P0): the tenant's current spend/rpm/inflight from Valkey.
		authed.HandleFunc("GET /api/tenants/{name}/usage", s.handleTenantUsage)
		// AgentTeams (M64, ADR 0057): read-only list of orchestration rosters, caller-scoped.
		authed.HandleFunc("GET /api/teams", s.handleListTeams)
		// Team create (ADR 0065 D4): caller-scoped CRD create from a reviewed AgentTeam YAML.
		// The Go 1.22 ServeMux distinguishes "POST /api/teams" from "GET /api/teams".
		authed.HandleFunc("POST /api/teams", s.handleCreateTeam)
		// Team generation (ADR 0065 D4): compose an AgentTeamSpec from existing registry members.
		// Caller-scoped, cost-tagged, NEVER auto-applies â returns spec + eligible members for review.
		// The Go 1.22 ServeMux treats "POST /api/teams/generate" as distinct from "GET /api/teams".
		authed.HandleFunc("POST /api/teams/generate", s.handleGenerateTeam)
		// GuardrailPolicies (m66.10, ADR 0059): read-only list of content-governance policies, caller-scoped.
		authed.HandleFunc("GET /api/guardrailpolicies", s.handleListGuardrailPolicies)
		// Workflows (m67.9, ADR 0060): read-only list of Workflow CRs, caller-scoped.
		authed.HandleFunc("GET /api/workflows", s.handleListWorkflows)
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
		// PUT /api/namespaces/{name}/display-name — set or clear the human-readable
		// display label on a namespace (ADR 0068 §7). Caller needs "update namespaces";
		// the API server enforces it — honest 403 if denied. "workspace" is UI-only.
		authed.HandleFunc("PUT /api/namespaces/{name}/display-name", s.handleSetNamespaceDisplayName)
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
		authed.Handle("POST /api/agents/{ns}/{name}/publish", notImplemented("caller-scoped agent publish"))
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
		authed.Handle("PUT /api/namespaces/{name}/display-name", notImplemented("caller-scoped namespace display-name"))
		authed.Handle("POST /api/agents", notImplemented("config-builder apply"))
		authed.Handle("GET /api/guardrailpolicies", notImplemented("caller-scoped guardrail policy list"))
		authed.Handle("GET /api/workflows", notImplemented("caller-scoped workflow list"))
		authed.Handle("GET /api/alerts", notImplemented("caller-scoped alerts feed"))
		authed.Handle("GET /api/knowledgebases", notImplemented("caller-scoped KB list"))
		authed.Handle("GET /api/knowledgebases/{name}", notImplemented("caller-scoped KB detail"))
		authed.Handle("POST /api/knowledgebases/{name}/search", notImplemented("caller-scoped KB test-query"))
		authed.Handle("POST /api/knowledgebases/{name}/documents", notImplemented("caller-scoped KB document upload"))
		authed.Handle("GET /api/agents/{ns}/{name}/online-score", notImplemented("caller-scoped online score"))
		authed.Handle("POST /api/agents/{ns}/{name}/rollback", notImplemented("caller-scoped agent rollback"))
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

	// Gateway ext-auth (ADR 0039): a token-authenticated hit to an AGENT URL gets OBO parity with
	// /api/invoke via an Envoy ext-auth call to the BFF (extracted to keep Handler under gocyclo).
	s.registerExtAuthRoutes(authed)

	s.registerRunRoutes(authed)
	s.registerShareRoutes(authed)
	s.registerWorkflowRunRoutes(authed)
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

	// Pure spec-editing (m71.1, extracted to keep Handler under gocyclo).
	s.registerRefineRoute(authed)

	// Recipe gallery (ADR 0066 D4): the curated set of Go-embedded simplified
	// agent.yaml starters a user one-clicks to pre-fill the create form. Recipes
	// carry no secrets and no caller-specific data — every authenticated caller
	// gets the same list. Wired on the authed mux (consistent with all other
	// console reads) but performs no cluster lookup.
	authed.HandleFunc("GET /api/recipes", s.handleListRecipes)

	// Check-requirements (ADR 0066 D3): a read-only advisory probe. Registered via a
	// helper (like registerRefineRoute) so its caller-scoped guard doesn't add a
	// branch to Handler()'s cyclomatic complexity.
	s.registerCheckRequirementsRoute(authed)

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
			// Deregister an MCP server (m26.3): tear down the whole register bundle
			// (Secret + SecretBinding + ToolRegistry + NetworkPolicy) caller-scoped, with
			// a personal-owner guard; the references route previews the delete-impact
			// (dependent bindings). The Go 1.22 ServeMux treats these as distinct from the
			// bare "GET/POST /api/mcpservers" (the more specific {ns}/{name} pattern wins).
			authed.HandleFunc("DELETE /api/mcpservers/{ns}/{name}", s.handleDeleteMCPServer)
			authed.HandleFunc("GET /api/mcpservers/{ns}/{name}/references", s.handleMCPServerReferences)
			authed.HandleFunc("GET /api/tools", s.handleListTools)
			// Cross-tenant MCP catalog (m73.4, ADR 0067 §6): the discovery-only list of org/public/team
			// MCP servers visible to the caller's tenant, without leaking private servers. A single
			// own-namespace SSAR gates entry (the sole authz); the store read uses the BFF's own cpDB
			// connection (the amended-ADR-0011 model — not the caller-scoped client). GET /api/catalog
			// is in the SAME mcpEnabled + callerClients gate as the other MCP endpoints.
			authed.HandleFunc("GET /api/catalog", s.handleCatalog)
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
			// Admin org-credential (m25.9, ADR 0029 §7): promote a server to org scope + set
			// its shared credential. Admin gate is RBAC-by-construction — the ToolRegistry
			// scope change is written caller-scoped (a viewer can't update it → 403).
			authed.HandleFunc("POST /api/mcp/org-credential", s.handleSetOrgCredential)
			// Tiered visibility publish (m73.5, ADR 0067 §5): widen a registered MCP server
			// to team / org / public visibility. Each tier is gated by a caller-scoped SSAR
			// (team → update toolregistries in ns; org → update tenants/<tenant>;
			// public → update tenants cluster-wide). Publish NEVER opens egress (m14.6 B1).
			authed.HandleFunc("POST /api/mcp/publish", s.handleMCPPublish)
			// Discover-then-materialize (m73.6, ADR 0067 §3): "Connect" imports a
			// catalog-discovered server's DEFINITION into the caller's OWN namespace so they
			// can use it with their OWN credential. The credential-safety crux: the
			// publisher's token NEVER crosses the namespace boundary — the copy is created
			// with NO credential material and the consumer OBO-connects their own later. A
			// security gate re-checks the origin is discoverable by the caller (else 404); the
			// origin read is a store read (amended-ADR-0011, like the catalog), the create is
			// caller-scoped (SSAR-gated by createMCPObjects). Same mcpEnabled + callerClients gate.
			authed.HandleFunc("POST /api/mcp/connect", s.handleMCPConnect)
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
			authed.Handle("DELETE /api/mcpservers/{ns}/{name}", notImplemented("caller-scoped MCP deregister"))
			authed.Handle("GET /api/mcpservers/{ns}/{name}/references", notImplemented("caller-scoped MCP references"))
			authed.Handle("GET /api/tools", notImplemented("caller-scoped tool catalog"))
			authed.Handle("POST /api/mcp/oauth/grant", notImplemented("caller-scoped MCP grant consent"))
			authed.Handle("DELETE /api/mcp/oauth/grant/{server}", notImplemented("caller-scoped MCP grant revoke"))
			authed.Handle("GET /api/mcp/approvals", notImplemented("caller-scoped MCP approval queue"))
			authed.Handle("POST /api/mcp/approvals/{ns}/{name}", notImplemented("caller-scoped MCP approve"))
			authed.Handle("POST /api/mcp/approvals/{ns}/{name}/reject", notImplemented("caller-scoped MCP reject"))
			authed.Handle("GET /api/catalog", notImplemented("caller-scoped MCP catalog"))
			authed.Handle("POST /api/mcp/publish", notImplemented("caller-scoped MCP publish"))
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
	root.Handle("/api/", s.restrictAgentOriginAPI(api)) // M39: shrink the API surface at agent origins
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
		// The SPA SHELL (root "/" or an explicit index.html) must ALWAYS be served
		// through serveIndex so it carries Cache-Control: no-cache — otherwise the
		// root path served index.html via FileServerFS (Last-Modified only), so a
		// browser cached a stale shell and kept loading an old asset bundle after a
		// new deploy. Only the content-hashed /assets/* are cacheable (FileServerFS).
		if name == "" || name == indexHTML {
			s.serveIndex(w, r)
			return
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
		s.serveIndex(w, r)
	})
}

// serveIndex writes dist/index.html (the SPA shell). Used for client-side routes
// and when the requested asset does not exist on disk. The caller (spaHandler)
// has already applied the SPA security headers. When the request is for an agent's
// OWN hostname (the edge set agentChatboxHeader, m37.3), it injects the agent-pin
// meta so the SPA boots straight into that agent's chatbox instead of the console.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(s.static, indexHTML)
	if err != nil {
		http.Error(w, "UI not built", http.StatusNotFound)
		return
	}
	if pin := agentPinForRequest(r); pin != "" {
		data = injectHeadMeta(data, "agent-pin", pin) // m37.3: boot the single-agent chatbox
	}
	// ADR 0040: tell the SPA the canonical console origin so a chatbox at an agent hostname trusts the
	// cross-origin "connected" relay message from the MCP-consent callback (which runs at that origin).
	if s.consoleURL != "" {
		data = injectHeadMeta(data, "mcp-callback-origin", s.consoleURL)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// index.html must never be cached so a new build's asset hashes are picked
	// up immediately.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// agentChatboxHeader is the flag the edge sets on requests to an agent's OWN hostname (m37.3): the
// agents-app HTTPRoute injects it (RequestHeaderModifier set), so the BFF serves the single-agent
// CHATBOX (the SPA pinned to the host's agent) rather than the console. Set by the edge — a client on
// the agents-app route cannot forge a different agent, since `set` overwrites any inbound value.
const agentChatboxHeader = "X-Ctxmesh-Agent-Chatbox"

// agentOriginAPIAllowlist is the ONLY /api/* endpoints the standalone per-agent chatbox needs — every
// call its reachable components make (the chat + MCP consent + agent detail + trace link + login/session
// boot). Method-scoped: e.g. GET on an agent is the detail read, but its mutations, sub-resources
// (/logs, /runs), and the whole rest of the console API (secrets, model routes, registries, topology,
// cost, runs, evals, prompts, config, providers, the agents LIST, …) are absent → 404 at agent origins.
var agentOriginAPIAllowlist = []string{
	"GET /api/authconfig",
	"GET /api/whoami",
	"GET /api/devmode",
	"GET /api/health",
	"GET /api/capabilities",
	"GET /api/namespaces",
	"GET /api/agents/{ns}/{name}",
	"POST /api/invoke",
	"POST /api/mcp/oauth/grant",
	"GET /api/mcp/oauth/callback",
	"GET /api/mcp/oauth/client-metadata",
	"GET /api/traces/{id}",
	"GET /api/traces/{id}/detail",
}

// restrictAgentOriginAPI shrinks the console API surface reachable at an agent's OWN hostname (M39
// hardening). When the edge marks the request as an agent-origin one (agentChatboxHeader — set by the
// agents-app route, unforgeable by a client), only the chatbox allowlist proceeds; every other /api/*
// is 404'd (not 403 — don't confirm an endpoint exists). No effect on the console origin, where the
// header is absent. This is defense-in-depth ON TOP of RBAC (a token still only reaches what it may) —
// it removes the wider surface, so an agent URL isn't a second front door to the whole operator console.
// Matching reuses Go's method+pattern router, so the allowlist stays declarative and precise.
func (s *Server) restrictAgentOriginAPI(next http.Handler) http.Handler {
	allow := http.NewServeMux()
	noop := func(http.ResponseWriter, *http.Request) {}
	for _, pat := range agentOriginAPIAllowlist {
		allow.HandleFunc(pat, noop)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(agentChatboxHeader) != "" {
			if _, pattern := allow.Handler(r); pattern == "" {
				http.NotFound(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// agentPinForRequest returns "namespace/name" when this request is for an agent's own hostname (the
// edge set agentChatboxHeader), else "". The agent is derived from the forwarded host — the same
// <agent>.<ns>.<baseDomain> shape the ext-auth edge parses (ADR 0039).
func agentPinForRequest(r *http.Request) string {
	if r.Header.Get(agentChatboxHeader) == "" {
		return ""
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	agent, ns := parseAgentFromHost(host)
	if agent == "" || ns == "" {
		return ""
	}
	return ns + "/" + agent
}

// injectHeadMeta inserts `<meta name="<name>" content="<content>">` at the start of the SPA shell's
// <head> so the boot code can read it — used for the agent-pin (m37.3, boot the single-agent chatbox)
// and the mcp-callback-origin (m38.3/ADR 0040, the trusted cross-origin relay origin). Meta, not a
// script — the CSP forbids inline scripts. Best-effort: with no <head> the shell is served unchanged.
// content is HTML-attribute-escaped defensively (in practice a validated DNS name or a config origin).
func injectHeadMeta(index []byte, name, content string) []byte {
	const head = "<head>"
	i := bytes.Index(index, []byte(head))
	if i < 0 {
		return index
	}
	meta := []byte(`<meta name="` + name + `" content="` + html.EscapeString(content) + `">`)
	at := i + len(head)
	out := make([]byte, 0, len(index)+len(meta))
	out = append(out, index[:at]...)
	out = append(out, meta...)
	out = append(out, index[at:]...)
	return out
}
