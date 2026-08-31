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

// Command bff is the agentry Backend-for-Frontend: it serves the static
// Vite SPA build and the /api surface (client-go reads of the agent CRDs) behind
// the M11 control-plane auth. It reuses the controllers' client-go — the K8s
// credentials stay in this process; the browser never receives them (ADR 0010).
//
// Deployment: this runs as a small server in the control plane. It is NOT a Node
// runtime — the UI is static assets served from --static-dir.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver for the durable run store
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/asyncbus"
	"github.com/ctxmesh/agentry/internal/bff"
	"github.com/ctxmesh/agentry/internal/controlplane"
	"github.com/ctxmesh/agentry/internal/controlplane/agentcapability"
	"github.com/ctxmesh/agentry/internal/controlplane/agentmemory"
	"github.com/ctxmesh/agentry/internal/controlplane/alertstore"
	"github.com/ctxmesh/agentry/internal/controlplane/auditlog"
	"github.com/ctxmesh/agentry/internal/controlplane/costrollup"
	"github.com/ctxmesh/agentry/internal/controlplane/dataset"
	"github.com/ctxmesh/agentry/internal/controlplane/enduseragent"
	"github.com/ctxmesh/agentry/internal/controlplane/knowledge"
	"github.com/ctxmesh/agentry/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/agentry/internal/controlplane/onlinescore"
	"github.com/ctxmesh/agentry/internal/controlplane/promptversion"
	"github.com/ctxmesh/agentry/internal/controlplane/publishedartifact"
	"github.com/ctxmesh/agentry/internal/controlplane/sharedrun"
	"github.com/ctxmesh/agentry/internal/controlplane/toolregistry"
	"github.com/ctxmesh/agentry/internal/credplane"
	"github.com/ctxmesh/agentry/internal/credresolve"
	"github.com/ctxmesh/agentry/internal/dbpool"
	"github.com/ctxmesh/agentry/internal/enduseroidc"
	"github.com/ctxmesh/agentry/internal/objectstore"
	"github.com/ctxmesh/agentry/internal/ocr"
	"github.com/ctxmesh/agentry/internal/preflight"
	"github.com/ctxmesh/agentry/internal/prompt"
	runstore "github.com/ctxmesh/agentry/internal/run"
)

func main() {
	var (
		addr        string
		staticDir   string
		version     string
		preflightUp bool
		ensureKey   bool
	)
	flag.StringVar(&addr, "addr", ":9090", "The address the BFF listens on.")
	flag.StringVar(&staticDir, "static-dir", "ui/dist",
		"Directory of the built Vite SPA (dist/). Empty disables static serving.")
	flag.StringVar(&version, "version", "dev", "Version string reported by /api/health.")
	flag.BoolVar(&preflightUp, "preflight", false,
		"Run install config-coherence checks (fail LOUD on misconfig) and exit — "+
			"the Helm post-install hook / GA Gate A (ADR 0095).")
	flag.BoolVar(&ensureKey, "ensure-capability-key", false,
		"Generate the platform capability keypair into bff-capability iff absent (never re-key), restart "+
			"consumers, and exit — the Helm keygen hook / GA Gate A (ADR 0095).")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("bff")

	if ensureKey {
		os.Exit(runEnsureCapabilityKey(context.Background()))
	}
	if preflightUp {
		os.Exit(runPreflight(context.Background()))
	}

	if err := run(addr, staticDir, version, log); err != nil {
		log.Error(err, "BFF exited with error")
		os.Exit(1)
	}
}

// runPreflight validates that the install is coherently configured and returns a non-zero exit code
// (failing the Helm post-install hook) with actionable messages on any misconfiguration — so the
// "correct-when-configured, silent-when-not" failures the GA audit found surface at install, not at
// runtime (M124/Gate A, ADR 0095). The OBO-specific env is required only when OBO egress is enabled.
func runPreflight(ctx context.Context) int {
	log := ctrl.Log.WithName("bff.preflight")
	oboOn := strings.EqualFold(os.Getenv("MCP_OBO_EGRESS_ENABLED"), "true")
	required := map[string]string{
		// Always needed for a functional install (all derivable/defaulted by the chart, m124.3):
		"TOKEN_SERVICE_URL":        os.Getenv("TOKEN_SERVICE_URL"),
		"MCP_CREDENTIAL_NAMESPACE": os.Getenv("MCP_CREDENTIAL_NAMESPACE"),
		"COST_ROLLUP_ENABLED":      os.Getenv("COST_ROLLUP_ENABLED"),
	}
	if oboOn {
		// OBO tool calls additionally need the sidecar image + the capability public key:
		required["EGRESS_SIDECAR_IMAGE"] = os.Getenv("EGRESS_SIDECAR_IMAGE")
		required["MCP_CAPABILITY_PUBLIC_KEY"] = os.Getenv("MCP_CAPABILITY_PUBLIC_KEY")
	}
	cfg := preflight.Config{
		RequiredEnv:           required,
		CapabilityPrivateSeed: os.Getenv("MCP_CAPABILITY_PRIVATE_KEY"),
		CapabilityPublicKey:   os.Getenv("MCP_CAPABILITY_PUBLIC_KEY"),
		ControlPlaneDSN:       os.Getenv("CONTROLPLANE_DSN"),
	}
	ping := func(ctx context.Context, dsn string) error {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		db, err := controlplane.Connect(cctx, dsn) // sql.Open("pgx") + PingContext
		if err != nil {
			return err
		}
		return db.Close()
	}
	errs := preflight.Check(ctx, cfg, ping)
	if len(errs) == 0 {
		log.Info("preflight OK — install configuration is coherent")
		return 0
	}
	for _, e := range errs {
		log.Error(e, "preflight FAILED")
	}
	fmt.Fprintf(os.Stderr,
		"\npreflight FAILED with %d problem(s) — fix the above and reinstall. See GA Gate A / ADR 0095.\n", len(errs))
	return 1
}

// newPlatformScheme builds the scheme the caller-scoped client encodes/decodes the platform CRDs with
// (core + both agents API versions).
func newPlatformScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, agentsv1alpha1.AddToScheme, agentsv1beta1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("building the platform scheme: %w", err)
		}
	}
	return scheme, nil
}

// consoleFeatureFlags are the env-driven kill-switches + pins the BFF resolves once at start-up and
// hands to the server config: the connect-a-provider switch (ADR 0015), the platform generation-model
// pin (ADR 0014), the BYO-MCP switch + trust policy (ADR 0016), and the console SSO advertisement
// (ADR 0020). Each defaults to the dev/trial posture; a hardened install narrows it through the chart.
type consoleFeatureFlags struct {
	providerConnect    bool
	platformGenModels  []string
	mcpEnabled         bool
	mcpRequireApproval bool
	oidcEnabled        bool
	oidcIssuer         string
	oidcClientID       string
}

// readConsoleFeatureFlags resolves those flags from the environment, logging each non-default posture
// so an install's effective stance is visible in the start-up log rather than inferred from behaviour.
func readConsoleFeatureFlags(log logr.Logger) consoleFeatureFlags {
	f := consoleFeatureFlags{
		providerConnect:    providerConnectEnabled(os.Getenv("PROVIDER_CONNECT_ENABLED")),
		platformGenModels:  parseGenerationModels(os.Getenv("PLATFORM_GENERATION_MODELS")),
		mcpEnabled:         envEnabledDefaultTrue(os.Getenv("MCP_ENABLED")),
		mcpRequireApproval: envTrue(os.Getenv("MCP_REQUIRE_APPROVAL")),
		oidcEnabled:        envTrue(os.Getenv("OIDC_ENABLED")),
		oidcIssuer:         strings.TrimSpace(os.Getenv("OIDC_ISSUER")),
		oidcClientID:       strings.TrimSpace(os.Getenv("OIDC_CLIENT_ID")),
	}
	if !f.providerConnect {
		log.Info("provider-connect disabled by PROVIDER_CONNECT_ENABLED=false; /api/providers routes serve 404")
	}
	if len(f.platformGenModels) > 0 {
		log.Info("platform generation models pinned", "models", f.platformGenModels)
	}
	if !f.mcpEnabled {
		log.Info("BYO-MCP disabled by MCP_ENABLED=false; /api/mcpservers + /api/tools serve 404")
	}
	if f.mcpRequireApproval {
		log.Info("BYO-MCP hardened: registered MCP tools are marked pending-approval (MCP_REQUIRE_APPROVAL=true)")
	}
	if f.oidcEnabled {
		log.Info("console SSO enabled (ADR 0020): advertising OIDC at /api/authconfig",
			"issuer", f.oidcIssuer, "clientID", f.oidcClientID)
	}
	return f
}

func run(addr, staticDir, version string, log logr.Logger) error {
	// Build the platform scheme (control-plane CRDs) so the caller-scoped client
	// can encode/decode the agent CRDs.
	scheme, err := newPlatformScheme()
	if err != nil {
		return err
	}

	// The in-cluster rest.Config supplies the API-server host + cluster CA/TLS.
	// The BFF does NOT build a static client from it for user CRD ops: instead the
	// CallerClientFactory copies this config per request and swaps in the CALLER'S
	// bearer token (ADR 0011), so the K8s API server enforces the caller's own
	// RBAC (M11 personas). The BFF's own SA credential is never used to act on the
	// user's CRDs — closing the confused-deputy gap by construction.
	cfg, cfgErr := ctrl.GetConfig()
	if cfgErr != nil {
		return cfgErr
	}
	callerClients := bff.NewCallerClientFactory(cfg, scheme)

	// Build the optional server-side adapters from the injected environment. The
	// Langfuse/Prometheus credentials stay in THIS process — they are never sent
	// to the browser (ADR 0010). A missing/incomplete config leaves the adapter
	// nil, and the server serves an honest 501 for its routes rather than wiring
	// a half-configured client.
	adapters := buildAdapters(log)
	// The config-builder expand adapter (m12.6) is a pure transform reusing the
	// CLI expand core — always available, no external creds.
	adapters.Expand = bff.NewExpandAdapter()
	// The Playground invoke adapter (m12.7) is a pure HTTP invoker — no creds, no
	// cluster access. The caller-scoped handler resolves the agent endpoint and the
	// adapter POSTs /invoke with a minted traceparent (the run stays caller-scoped,
	// ADR 0011). Always available.
	adapters.Invoke = bff.NewInvokeAdapter(bff.InvokeAdapterConfig{})

	// The connect-a-provider kill-switch (ADR 0015). Default TRUE (dev/trial); a
	// hardened install sets PROVIDER_CONNECT_ENABLED=false via the chart so the
	// connect endpoints 404 and the UI falls back to reference-existing. Read from
	// env with the same "flag-from-env" pattern the adapters use.
	flags := readConsoleFeatureFlags(log)

	// The create-from-prompt platform generation-model pin (ADR 0014). Empty (the
	// default) → generation uses the caller's connected-provider model unpinned. An
	// operator that wants a governed generation model sets a comma-separated list
	// (PLATFORM_GENERATION_MODELS) — the UI's model dropdown source and the allowed
	// set the generate endpoint enforces.

	// The BYO-MCP kill-switch + trust policy (ADR 0016). MCP_ENABLED defaults TRUE
	// (dev/trial); a hardened install sets it false to 404 the register/catalog
	// endpoints. MCP_REQUIRE_APPROVAL defaults FALSE (self-serve — tools are
	// immediately bindable); a hardened install sets it true to mark newly
	// registered tools pending-approval (the approval queue is M17). Same
	// "flag-from-env" pattern as the connect kill-switch.

	// Per-cluster HMAC key for the one-way user-identity hash on grant Secrets +
	// the mcp-owner annotation (m25.1, ADR 0029 §7). A production cluster mounts a
	// platform Secret here; absent, the BFF warns and degrades to unsalted SHA-256.
	mcpGrantHMACKey := []byte(os.Getenv("MCP_GRANT_HMAC_KEY"))

	// Locked platform namespace for MCP grant Secrets (m25.1b, ADR 0029 §7). When set,
	// grant write/read/delete route through a PRIVILEGED, namespace-scoped client built
	// from the BFF's own in-cluster config (its SA) — a bounded, deliberate exception to
	// the confused-deputy stance above: it touches ONLY Secrets in this locked namespace
	// (which tenants cannot read), keyed by the authenticated caller's own identity,
	// never a user CRD. Absent, the BFF keeps the legacy per-request-namespace grant
	// path (ADR 0011) and warns. RBAC for the namespace ships in the Helm chart.
	mcpCredentialNamespace := strings.TrimSpace(os.Getenv("MCP_CREDENTIAL_NAMESPACE"))
	var credentialClient client.Client
	if mcpCredentialNamespace != "" {
		credentialClient, err = client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			return fmt.Errorf("build privileged credential client: %w", err)
		}
	}

	// Per-cluster platform keypair for the run capability (runcap, ADR 0030 §2/§5): the
	// base64-encoded Ed25519 private seed the BFF signs run capabilities with, plus the
	// credential-plane audience they target (verifiers check it). A prod cluster mounts a
	// platform Secret here; absent, capability minting is disabled and a tool call resolves
	// as unattended (org/public only).
	mcpCapabilitySeed := strings.TrimSpace(os.Getenv("MCP_CAPABILITY_PRIVATE_KEY"))
	mcpCapabilityAudience := strings.TrimSpace(os.Getenv("MCP_CAPABILITY_AUDIENCE"))

	// Console SSO advertisement (ADR 0020). OIDC_ENABLED=true + an issuer + a client
	// id → GET /api/authconfig tells the SPA to run Auth-Code+PKCE against Dex; else
	// the SPA uses token login (ADR 0012). The BFF holds NO OIDC secret (public client).

	// End-user OIDC (M137/EU1b, ADR 0106): a distinct per-tenant IdP for the standalone /chat runtime.
	// The verifier is ALWAYS constructed — it does nothing until a request targets a tenant with
	// spec.endUserIdentity.enabled. SSRF posture on issuer/JWKS fetches: a private/loopback issuer IP is
	// denied unless opted in (in-cluster / dev IdPs). SERVICE_ACCOUNT_ISSUER is refused as an end-user
	// issuer (a K8s-trust collision — an end-user token must never gain K8s trust).
	endUserVerifier := enduseroidc.NewVerifier(enduseroidc.Options{
		AllowPrivateIssuer: envTrue(os.Getenv("END_USER_OIDC_ALLOW_PRIVATE_ISSUER")),
		AllowLoopback:      envTrue(os.Getenv("END_USER_OIDC_ALLOW_LOOPBACK")),
	})
	saIssuer := strings.TrimSpace(os.Getenv("SERVICE_ACCOUNT_ISSUER"))

	// SPI write path (ADR 0032): when TOKEN_SERVICE_URL is set, the OAuth callback DELEGATES
	// grant persistence to the central token-service so grants land in the config-selected
	// backend and DB/vault creds stay out of this user-facing BFF. mTLS engages when the
	// BFF_TOKEN_SERVICE_TLS_* files are present (the same platform material the sidecars use);
	// absent ⇒ plain HTTP (dev). Unset TOKEN_SERVICE_URL ⇒ the BFF writes the grant Secret directly.
	var grantStore credresolve.GrantWriter
	var tsHTTPClient *http.Client // reused by the KB test-query endpoint (m68.13)
	if tsURL := strings.TrimSpace(os.Getenv("TOKEN_SERVICE_URL")); tsURL != "" {
		httpClient, tlsErr := tokenServiceHTTPClient(tsURL)
		if tlsErr != nil {
			return fmt.Errorf("build token-service mTLS client: %w", tlsErr)
		}
		tsHTTPClient = httpClient
		grantStore = credplane.NewClient(tsURL, httpClient)
		log.Info("MCP grant writes delegate to the token-service (SPI write path)", "url", tsURL, "mtls", httpClient != nil)
	}

	// Durable run store (ADR 0034 §durability, m32.1): when RUN_STORE_DSN names a Postgres, runs
	// persist there and survive a BFF restart/reschedule (a reconnecting client replays from the
	// durable event log). Absent ⇒ the hot in-memory store (dev/single-pod), which the server
	// defaults to. Aligned with the credential state layer so one Postgres backs both.
	var runStore runstore.Store
	var convStore runstore.ConversationStore
	if runDSN := strings.TrimSpace(os.Getenv("RUN_STORE_DSN")); runDSN != "" {
		db, dbErr := sql.Open("pgx", runDSN)
		if dbErr != nil {
			return fmt.Errorf("open run-store postgres: %w", dbErr)
		}
		dbpool.Apply(db, "RUN_STORE_MAX_OPEN_CONNS", 15) // F-8: bound the run-store pool (ADR 0097)
		defer func() { _ = db.Close() }()
		runStore, err = runstore.NewPostgresStore(context.Background(), db)
		if err != nil {
			return fmt.Errorf("init durable run store: %w", err)
		}
		// The conversation → active-agent pointer for handoff (m67.6, ADR 0060 §5) shares the SAME
		// Postgres so a transfer survives a restart and the next turn (on any pod) routes to the active
		// agent. Absent RUN_STORE_DSN ⇒ a hot mem twin (the server defaults to it), matching the run store.
		convStore, err = runstore.NewPostgresConversationStore(context.Background(), db)
		if err != nil {
			return fmt.Errorf("init durable conversation store: %w", err)
		}
		log.Info("durable run store enabled (ADR 0034): runs persist to Postgres")
	}

	// Control-plane store (ADR 0042): PromptVersion and ToolRegistry are retired to Postgres
	// (ADR 0044) — the API server is no longer in their path, so CONTROLPLANE_DSN is REQUIRED,
	// matching the operator (cmd/main.go) and token-service. Without it the BFF would 501 every
	// ToolRegistry/PromptVersion/MCP endpoint, so fail loud at start-up instead. OpenDB runs the
	// goose migrations (with a session lock) at start-up.
	cpDSN := strings.TrimSpace(os.Getenv("CONTROLPLANE_DSN"))
	if cpDSN == "" {
		return errors.New("CONTROLPLANE_DSN is required: PromptVersion + ToolRegistry are retired to Postgres (ADR 0044)")
	}
	cpDB, cpErr := controlplane.OpenDB(context.Background(), cpDSN)
	if cpErr != nil {
		return fmt.Errorf("open control-plane postgres: %w", cpErr)
	}
	defer func() { _ = cpDB.Close() }()
	promptStore := promptversion.NewPostgresStore(cpDB)
	toolStore := toolregistry.NewPostgresStore(cpDB)                   // shares the handle + migrations
	nsTenantStore := namespacetenant.NewPostgresStore(cpDB)            // m73.4: namespace→tenant mirror for catalog
	publishedArtifactStore := publishedartifact.NewPostgresStore(cpDB) // m74.1: snapshot-at-publish templates
	sharedRunStore := sharedrun.NewPostgresStore(cpDB)                 // m75.1: single-run capability share links
	log.Info("control-plane store enabled (ADR 0042/0044): PromptVersions + ToolRegistries served from Postgres")

	// Worker-path dispatch (ADR 0034, m32.2): RUN_WORKER_DISPATCH makes POST /runs leave runs
	// `queued` for a KEDA-scaled worker pool (this pod, or a dedicated worker Deployment) to claim +
	// execute — decoupled from the request. Only meaningful with a durable run store.
	runWorkerDispatch := envTrue(os.Getenv("RUN_WORKER_DISPATCH"))
	if runWorkerDispatch && runStore == nil {
		return errors.New("RUN_WORKER_DISPATCH requires a durable run store (set RUN_STORE_DSN)")
	}

	// Live tenant-usage reader (M49): read-only connection to the shared state-layer Valkey so the console
	// can show a tenant's current spend/rpm/inflight vs the cap. Optional — absent ⇒ the usage endpoint 501s.
	var tenantUsage bff.TenantUsageReader
	// Run-control publisher (m70.8, real-kill cancel channel): the trusted BFF SETs `run:{id}:control=cancel`
	// to the SAME shared state-layer Valkey on cancel, so the launcher gateway can abort the in-flight model
	// call. Absent STATELAYER_ADDR ⇒ nil ⇒ cancel degrades to the durable soft-cancel status flip alone.
	var runControl bff.RunControlPublisher
	if addr := strings.TrimSpace(os.Getenv("STATELAYER_ADDR")); addr != "" {
		tenantUsage = bff.NewRedisTenantUsageReader(addr)
		runControl = bff.NewRedisRunControlPublisher(addr)
	}

	// Workflow node endpoints are resolved + pinned at CREATE time, caller-scoped (m67.13, ADR 0011/0060):
	// each node agentRef → the agent's status.URL is resolved through the CALLER'S client at run-create and
	// pinned onto the run (run.NodeEndpoints), exactly as a single run pins its Endpoint. The off-request
	// workflow executor then launches nodes off the PINNED endpoints — so the BFF needs NO agent-CRD RBAC
	// (config/bff/role.yaml is `rules: []`, ADR 0011) and there is no privileged off-request cluster client.

	// Durable KB object store (M68, ADR 0061 Fork 4): OBJECT_STORE_ADDR unset ⇒ nil ⇒ the upload endpoint
	// returns an honest 501 (see newDocStore — it is typed-nil-safe).
	docStore, err := newDocStore()
	if err != nil {
		return fmt.Errorf("init durable KB object store: %w", err)
	}

	// KB ingestion direct-write path (M68, ADR 0061 governance #8): the run-worker is a TRUSTED control-plane
	// workload that already holds the controlplane DSN, so the ingestion executor (m68.6) EMBEDS + WRITES
	// knowledge_chunks DIRECTLY — no token-service proxy hop (agent pods never do this; they read via proxy).
	//   - the knowledge store rides the already-open cpDB (the same pgvector Postgres as memory).
	//   - the gateway embedder is built from MODEL_GATEWAY_URL, exactly as the token-service builds its memory
	//     embedder (the same seam). Absent ⇒ nil ⇒ the ingest endpoint + executor degrade honestly (501 / a
	//     clear failed-run reason), never a panic.
	knowledgeStore := knowledge.NewPostgresStore(cpDB)
	ingestEmbedder := newIngestEmbedder(log)

	// The durable async backend for A2A hops (M141.4, ADR 0121). Built once and used for BOTH halves:
	// the publish edge an agent hands a hop to, and the dispatcher that delivers it. Absent config ⇒
	// nil ⇒ neither is wired, and Knative Eventing stays the only async path (today's behaviour).
	asyncBus := newAsyncBus(log)
	if asyncBus != nil {
		defer func() { _ = asyncBus.Close() }()
	}

	// Scanned-PDF OCR (M140.5, ADR 0119): the executor OCRs an image-only PDF via this offline service when its
	// born-digital text is insufficient. Wired from INGEST_OCR_URL; unset ⇒ nil ⇒ a scanned PDF stays honestly
	// PartiallyIngested (strictly additive).
	ingestOCR := newIngestOCR(log)

	// Dataset store (M69, ADR 0062 Fork 1): rides the same cpDB (migration 0007 applied by controlplane.Migrate).
	// The dataset-export executor (m69.2) writes it directly (governance #8: the trusted run-worker holds cpDB +
	// Langfuse creds), copying M66-redacted, traceId-lineaged cases out of Langfuse. Paired with the Langfuse
	// adapter in `adapters` (built below) as the export read source.
	datasetStore := dataset.NewPostgresStore(cpDB)

	// Online-score store (M69, ADR 0062 Fork 2): rides the same cpDB (migration 0008 applied by
	// controlplane.Migrate). The online-scoring worker (m69.5) writes it directly (governance #8: the
	// trusted off-request worker holds cpDB + Langfuse creds), folding production traces into the
	// per-(namespace, agent, version, window) online-score vector.
	onlineStore := onlinescore.NewPostgresStore(cpDB)

	// Cost-rollup store (M70, ADR 0063 D1): rides the same cpDB (migration 0009 applied by
	// controlplane.Migrate). The cost-rollup worker (m70.2) writes it directly (governance #8: the
	// trusted off-request worker holds cpDB + Valkey reach), snapshotting the ephemeral per-tenant
	// Valkey monthly-spend keys into the durable cost_rollups ledger.
	rollupStore := costrollup.NewPostgresStore(cpDB)

	// Per-agent online-config resolver (M69, ADR 0062 Fork 2; m84.3 completes m69.6): the online-scoring
	// worker consults it per agent to enable/tune the judge from that agent's EvalSuite.online policy. The
	// policy is resolved by the CONTROLLER (which legitimately holds evalsuites RBAC) into a per-(ns, agent)
	// cpDB row; the worker READS that row here (cpDB — the SAME store it writes aggregates to). The BFF
	// deliberately does NOT read AgentDeployment/EvalSuite via its own SA: ADR 0011 keeps the BFF's SA at
	// `rules: []` (the confused-deputy blast-radius stance; m69.6 REVERTED a BFF-SA agent-CRD ClusterRole
	// that broke this). A missing/disabled row ⇒ (nil, nil): the worker falls back to its process defaults
	// (judge OFF) — the fail-safe. Wired from the SAME cpDB store as onlineStore: no new dep, no agent-CRD RBAC.
	onlineResolver := bff.NewDBOnlineConfigResolver(onlineStore)

	srv := bff.NewServer(bff.Options{
		TokenServiceURL:        strings.TrimSpace(os.Getenv("TOKEN_SERVICE_URL")),
		TokenServiceHTTPClient: tsHTTPClient,
		GrantStore:             grantStore,
		TenantUsage:            tenantUsage,
		RunControl:             runControl,
		RunStore:               runStore,
		DocStore:               docStore,
		KnowledgeStore:         knowledgeStore,
		DatasetStore:           datasetStore,
		OnlineStore:            onlineStore,
		OnlineResolver:         onlineResolver,
		RollupStore:            rollupStore,
		Embedder:               ingestEmbedder,
		OCR:                    ingestOCR,
		// Capability discovery (M141, ADR 0120): the registry the discovery edge ranks over (the
		// CONTROLLER writes it — the BFF only reads, so no new RBAC), plus the offline models it ranks
		// with. It reuses `ingestEmbedder`: discovery embeds through the SAME gateway seam as ingestion.
		AgentCapabilities:       agentcapability.NewPostgresStore(cpDB),
		DiscoveryReranker:       discoveryReranker(log),
		DiscoveryEmbeddingRoute: strings.TrimSpace(os.Getenv("DISCOVERY_EMBEDDING_ROUTE")),
		// Async A2A backend (M141.4, ADR 0121): the durable bus an agent hands a hop to. nil ⇒ the
		// publish edge is absent and the dispatcher never starts, so Knative Eventing remains the only
		// async path — exactly today's behaviour.
		AsyncPublisher: asyncBus,
		// Sender-constrained run capabilities (M142.5, ADR 0124). The bind store rides the shared
		// state-layer Valkey so "already bound" is the same answer on every replica; without an addr
		// there is no exchange edge, and capabilities stay bearer.
		RuncapBind:               runcapBindStore(log),
		RequireProofOfPossession: strings.TrimSpace(os.Getenv("RUNCAP_REQUIRE_POP")) == "true",
		ConvStore:                convStore,
		PromptStore:              promptStore,
		// Production git-pointer prompt resolver (m121.3, ADR 0008) — the drop-in for the
		// fixture Resolver, so GET /api/promptversions/{ns}/{name}/diff resolves REAL content
		// from git (github.com raw). PROMPT_GIT_TOKEN (a PAT via a Secret, never committed)
		// authorises private repos; empty ⇒ public repos only. Git stays the source of truth.
		PromptResolver:              prompt.NewHTTPResolver(strings.TrimSpace(os.Getenv("PROMPT_GIT_TOKEN"))),
		ToolRegistryStore:           toolStore,
		NamespaceTenantStore:        nsTenantStore,
		EndUserVerifier:             endUserVerifier,
		SAIssuer:                    saIssuer,
		EndUserAgentStore:           enduseragent.NewPostgresStore(cpDB),
		PublishedArtifactStore:      publishedArtifactStore,
		SharedRunStore:              sharedRunStore,
		AgentMemoryStore:            agentmemory.NewPostgresStore(cpDB),
		AuditStore:                  auditlog.NewPostgresStore(cpDB),
		AlertStore:                  alertstore.NewPostgresStore(cpDB),
		RunWorkerDispatch:           runWorkerDispatch,
		CallerClients:               callerClients,
		Scheme:                      scheme,
		Auth:                        bff.BearerAuthenticator{},
		Adapters:                    adapters,
		Version:                     version,
		StaticDir:                   staticDir,
		ProviderConnect:             flags.providerConnect,
		PlatformGenerationModels:    flags.platformGenModels,
		MCPEnabled:                  flags.mcpEnabled,
		MCPRequireApproval:          flags.mcpRequireApproval,
		MCPGrantHMACKey:             mcpGrantHMACKey,
		MCPCredentialNamespace:      mcpCredentialNamespace,
		CredentialClient:            credentialClient,
		MCPCapabilityPrivateSeedB64: mcpCapabilitySeed,
		MCPCapabilityAudience:       mcpCapabilityAudience,
		OIDCEnabled:                 flags.oidcEnabled,
		OIDCIssuer:                  flags.oidcIssuer,
		OIDCClientID:                flags.oidcClientID,
		ConsoleURL:                  os.Getenv("CONSOLE_URL"), // ADR 0040: canonical MCP-consent callback + relay origin
		Log:                         ctrl.Log.WithName("bff.server"),
	})

	// Fail CLOSED on a missing security-critical key (M124/Gate A, ADR 0095 §2): when the operator
	// intends per-user OBO (MCP_OBO_REQUIRED — the chart wires it from oboEgress.enabled) but run-
	// capability minting is disabled, REFUSE to serve. Else OBO tool calls silently downgrade to the
	// shared org/public credential reporting success. Placed before the run-worker pool starts, so a
	// durable worker (same process) refuses too. A no-OBO install (the default) is unaffected.
	oboRequired := envTrue(os.Getenv("MCP_OBO_REQUIRED"))
	if err := bff.OBOMintingPrecondition(oboRequired, srv.CapabilityMintingEnabled()); err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// waitWorkers (F-1, M125/ADR 0097) joins the run-worker loops on shutdown so held leases are released
	// before the process exits (else a peer waits a full lease-TTL to reclaim). nil when no pool runs.
	var waitWorkers func()
	// Run-worker pool (ADR 0034, m32.2): RUN_WORKER_CONCURRENCY>0 runs N claim loops in THIS process
	// that drain the durable queue. A dedicated worker Deployment sets this (with serving idle); the
	// front-end BFF can also run a few. Stops claiming when the shutdown signal fires.
	if n := envInt(os.Getenv("RUN_WORKER_CONCURRENCY")); n > 0 {
		if runStore == nil {
			return errors.New("RUN_WORKER_CONCURRENCY requires a durable run store (set RUN_STORE_DSN)")
		}
		waitWorkers = srv.StartRunWorkers(ctx, bff.RunWorkerConfig{Concurrency: n})
		log.Info("run-worker pool started (ADR 0034)", "concurrency", n)
	}

	maybeStartOnlineScorer(ctx, srv, adapters)
	maybeStartCostRollupWorker(ctx, srv)
	// Async A2A dispatcher (M141.4, ADR 0121): consumes durable hops and delivers them over HTTP, so the
	// agent-side contract is unchanged by the backend swap. No bus ⇒ no dispatcher.
	if asyncBus != nil {
		srv.StartAsyncDispatcher(ctx, bff.AsyncDispatcherConfig{Subscriber: asyncBus})
		log.Info("async A2A dispatcher started (ADR 0121)")
	}
	maybeStartRecipeOverlay(ctx, srv)

	errCh := make(chan error, 1)
	go func() {
		log.Info("BFF listening", "addr", addr, "staticDir", staticDir)
		if serveErr := httpSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()

	// Private metrics listener (M128/Gate E): the run-pipeline SLIs on METRICS_ADDR
	// (default :9090), OFF the public edge (ADR 0041) — a separate mux/server so /metrics
	// never rides the SPA/api listener. METRICS_ADDR="off" disables it.
	// Default :9092 — a DISTINCT port from the public BFF listener (:9090) so the two never
	// collide (the metrics surface is private; ADR 0041).
	metricsSrv := startMetricsListener(srv, log)

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if metricsSrv != nil {
			_ = metricsSrv.Shutdown(shutdownCtx)
		}
		httpErr := httpSrv.Shutdown(shutdownCtx)
		// F-1: wait (bounded) for the worker loops to release their leases before exiting, so a peer
		// reclaims promptly instead of after a full lease-TTL. The D4 drain grace ran concurrently with
		// the HTTP shutdown above; the bound guards against a non-ctx-honoring executor outlasting it.
		if waitWorkers != nil {
			done := make(chan struct{})
			go func() { waitWorkers(); close(done) }()
			select {
			case <-done:
				log.Info("run-worker pool drained (leases released)")
			case <-time.After(15 * time.Second): // ~drain grace (10s) + buffer
				log.Info("run-worker drain timed out; exiting anyway")
			}
		}
		return httpErr
	case serveErr := <-errCh:
		return serveErr
	}
}

// startMetricsListener starts the PRIVATE metrics listener (M128/Gate E) on METRICS_ADDR and returns its
// server so the shutdown path can close it (nil when disabled). Default :9092 — deliberately DISTINCT
// from the public BFF listener (:9090) so /metrics never rides the SPA/api edge (ADR 0041);
// METRICS_ADDR="off" disables it entirely. Extracted from run() to keep its cyclomatic complexity down,
// the same reason as maybeStartOnlineScorer below.
func startMetricsListener(srv *bff.Server, log logr.Logger) *http.Server {
	metricsAddr := strings.TrimSpace(os.Getenv("METRICS_ADDR"))
	if metricsAddr == "" {
		metricsAddr = ":9092"
	}
	if metricsAddr == "off" {
		return nil
	}
	mmux := http.NewServeMux()
	mmux.Handle("/metrics", srv.MetricsHandler())
	metricsSrv := &http.Server{Addr: metricsAddr, Handler: mmux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("BFF metrics listening", "addr", metricsAddr)
		if serveErr := metricsSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error(serveErr, "metrics listener failed")
		}
	}()
	return metricsSrv
}

// maybeStartOnlineScorer starts the online-scoring worker (ADR 0062 Fork 2, m69.5) when
// ONLINE_SCORER_ENABLED=1 AND a Langfuse adapter is wired. The periodic reconciler folds production
// traces into the per-(namespace, agent, version, window) online-score aggregates; without Langfuse
// (post-hoc scoring reads traces) it could only no-op, so we gate on it to avoid starting a goroutine
// that can never do work — logging the honest reason. The worker stops on ctx cancellation (the shutdown
// signal). Extracted from run() to keep its cyclomatic complexity down; the logger is derived here
// (ctrl.Log) rather than passed, so the signature stays context-first without mixing ctx + logger.
func maybeStartOnlineScorer(ctx context.Context, srv *bff.Server, adapters bff.Adapters) {
	log := ctrl.Log.WithName("bff.online-scorer")
	if os.Getenv("ONLINE_SCORER_ENABLED") != "1" {
		return
	}
	if adapters.Langfuse == nil {
		log.Info("ONLINE_SCORER_ENABLED set but Langfuse not configured; online-scoring worker NOT started")
		return
	}
	srv.StartOnlineScorer(ctx, bff.OnlineScorerConfig{})
	log.Info("online-scoring worker started (ADR 0062 Fork 2)")
}

// maybeStartCostRollupWorker starts the cost-rollup worker (ADR 0063 D1, m70.2) when
// COST_ROLLUP_ENABLED=1. The periodic reconciler snapshots the ephemeral per-tenant Valkey monthly-spend
// keys into the durable cost_rollups ledger once per tick (~1h). It degrades gracefully: it is a no-op
// when STATELAYER_ADDR (the Valkey addr) is unset. The worker stops on ctx cancellation (the shutdown
// signal). Extracted from run() to keep its cyclomatic complexity down; the logger is derived (ctrl.Log).
func maybeStartCostRollupWorker(ctx context.Context, srv *bff.Server) {
	log := ctrl.Log.WithName("bff.cost-rollup")
	if os.Getenv("COST_ROLLUP_ENABLED") != "1" {
		return
	}
	cfg := bff.CostRollupConfig{
		ValKeyAddr: strings.TrimSpace(os.Getenv("STATELAYER_ADDR")),
	}
	srv.StartCostRollupWorker(ctx, cfg)
	log.Info("cost-rollup worker started (ADR 0063 D1)", "valKeyAddr", cfg.ValKeyAddr)
}

// maybeStartRecipeOverlay (S1): when a recipes ConfigMap is mounted (RECIPES_OVERLAY_DIR set), hot-reload
// the operator's custom recipes from that dir and merge them over the Go-embedded defaults in
// GET /api/recipes. Opt-in — absent env ⇒ embedded-only (fail-closed). No k8s-client read → no RBAC.
func maybeStartRecipeOverlay(ctx context.Context, srv *bff.Server) {
	overlayDir := strings.TrimSpace(os.Getenv("RECIPES_OVERLAY_DIR"))
	if overlayDir == "" {
		return
	}
	go srv.StartRecipeOverlayWatcher(overlayDir, ctx.Done())
	ctrl.Log.WithName("bff.recipe-overlay").Info("operator recipe overlay enabled", "dir", overlayDir)
}

// providerConnectEnabled resolves the connect-a-provider kill-switch from its
// env value. The feature defaults to ENABLED (dev/trial, ADR 0015): an empty or
// unrecognized value → true. Only an explicit falsey value ("false"/"0"/"no",
// case-insensitive) disables it — the way a hardened install opts out. Kept
// permissive-by-default so a fresh `helm install` gives the console the connect
// flow with no extra config.
func providerConnectEnabled(raw string) bool {
	return envEnabledDefaultTrue(raw)
}

// envEnabledDefaultTrue resolves a boolean feature flag that defaults to ENABLED:
// an empty or unrecognized value → true; only an explicit falsey value
// ("false"/"0"/"no"/"off", case-insensitive) disables it. Used for the
// connect-a-provider and BYO-MCP kill-switches (permissive-by-default so a fresh
// install gets the console features with no extra config).
func envEnabledDefaultTrue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

// envTrue resolves a boolean flag that defaults to FALSE: only an explicit truthy
// value ("true"/"1"/"yes"/"on", case-insensitive) enables it. Used for the
// BYO-MCP hardened trust policy (MCP_REQUIRE_APPROVAL), off by default so the
// self-serve aha stays frictionless.
func envTrue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// envInt parses a non-negative int env value; a blank or malformed value is 0 (feature off).
func envInt(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// newDocStore builds the durable KB object store (M68, ADR 0061 Fork 4) from OBJECT_STORE_ADDR.
// It is typed-nil-safe: when OBJECT_STORE_ADDR is unset, NewMinioStore returns a nil *minioKBStore,
// which this returns as a genuine nil ObjectStore interface (never a typed-nil wrapped in the
// interface) so the upload handler's `docStore == nil` 501-gate works. Unset ⇒ (nil, nil).
func newDocStore() (objectstore.ObjectStore, error) {
	ms, err := objectstore.NewMinioStore()
	if err != nil {
		return nil, err
	}
	if ms == nil {
		return nil, nil
	}
	return ms, nil
}

// discoveryReranker wires the OPTIONAL cross-encoder that re-scores capability-discovery candidates
// (M141, ADR 0120; the reranker itself is ADR 0117). It reads DISCOVERY_RERANK_URL and falls back to
// KNOWLEDGE_RERANK_URL, so an operator running ONE offline rerank service gets discovery reranking for
// free rather than having to point a second variable at the same pod. Unset ⇒ nil ⇒ cosine-only ranking,
// which is a complete answer, just a coarser one — rerank is fail-open by construction.
func discoveryReranker(log logr.Logger) credplane.Reranker {
	rerankURL := strings.TrimSpace(os.Getenv("DISCOVERY_RERANK_URL"))
	if rerankURL == "" {
		rerankURL = strings.TrimSpace(os.Getenv("KNOWLEDGE_RERANK_URL"))
	}
	if rerankURL == "" {
		log.Info("capability discovery: no rerank service wired — ranking is cosine-only (M141)")
		return nil
	}
	log.Info("capability discovery: cross-encoder rerank wired (ADR 0117)", "reranker", rerankURL)
	return credplane.NewHTTPReranker(rerankURL, nil)
}

// runcapBindStore builds the capability→key bind store over the shared state-layer Valkey (M142.5,
// ADR 0124). It must be SHARED across BFF replicas: the binding is single-use, and a per-replica record
// would let the same capability be bound once on each replica — which is not a boundary at all. No
// STATELAYER_ADDR ⇒ nil ⇒ the exchange edge is not registered and capabilities remain bearer tokens.
func runcapBindStore(log logr.Logger) bff.RuncapBindStore {
	addr := strings.TrimSpace(os.Getenv("STATELAYER_ADDR"))
	if addr == "" {
		log.Info("run-capability binding DISABLED: STATELAYER_ADDR unset — capabilities stay bearer tokens")
		return nil
	}
	log.Info("run-capability binding enabled (ADR 0124): single-use exchange over the state layer")
	return bff.NewRedisRuncapBindStore(addr,
		strings.TrimSpace(os.Getenv("STATELAYER_USERNAME")), os.Getenv("STATELAYER_PASSWORD"))
}

// newIngestOCR wires the offline OCR service for scanned-PDF ingestion (M140.5, ADR 0119) from
// INGEST_OCR_URL. Unset ⇒ nil ⇒ a scanned PDF stays honestly PartiallyIngested (strictly additive).
func newIngestOCR(log logr.Logger) ocr.OCR {
	ocrURL := strings.TrimSpace(os.Getenv("INGEST_OCR_URL"))
	if ocrURL == "" {
		return nil
	}
	log.Info("scanned-PDF OCR enabled (M140.5): OCR service wired", "ocr", ocrURL)
	return ocr.NewHTTPOCR(ocrURL, nil)
}

// newAsyncBus connects the durable async backend for A2A hops (M141.4, ADR 0121) when ASYNC_BACKEND
// names one. Today that is "jetstream" (NATS_URL, optional NATS_CREDENTIALS_FILE); the seam keeps another
// broker a config swap rather than a code change.
//
// A configured-but-unreachable broker returns nil with a LOUD log rather than crashing the BFF: async A2A
// is one feature among many here, and taking the whole console down because a broker is late to start
// would be a worse outcome than the honest degrade (the publish edge is simply absent, so a producer gets
// a clear 404 instead of a silent black hole).
func newAsyncBus(log logr.Logger) *asyncbus.JetStreamBus {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("ASYNC_BACKEND")))
	if backend == "" {
		return nil
	}
	if backend != "jetstream" {
		log.Info("async A2A backend NOT started: unknown ASYNC_BACKEND", "backend", backend)
		return nil
	}
	natsURL := strings.TrimSpace(os.Getenv("NATS_URL"))
	if natsURL == "" {
		log.Info("async A2A backend NOT started: ASYNC_BACKEND=jetstream needs NATS_URL")
		return nil
	}
	// A bounded connect: a broker that never answers must not hold up BFF start-up.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bus, err := asyncbus.NewJetStream(ctx, asyncbus.JetStreamOptions{
		URL:             natsURL,
		CredentialsFile: strings.TrimSpace(os.Getenv("NATS_CREDENTIALS_FILE")),
		Replicas:        envInt(os.Getenv("NATS_STREAM_REPLICAS")),
	})
	if err != nil {
		log.Error(err, "async A2A backend NOT started: the broker is unreachable — "+
			"async hops cannot be published until it is", "url", natsURL)
		return nil
	}
	log.Info("async A2A backend connected (ADR 0121): durable hops enabled",
		"backend", backend, "url", natsURL)
	return bus
}

// newIngestEmbedder builds the gateway embedder the KB ingestion executor uses to embed chunk texts directly
// (M68, ADR 0061 Fork 2 / governance #8 — the trusted worker embeds + writes, agent pods do not). It reads the
// gateway base URL from MODEL_GATEWAY_URL (the same seam the token-service memory embedder uses) + the optional
// MODEL_GATEWAY_KEY. Unset ⇒ nil, so the ingest endpoint + executor degrade honestly (501 / a failed-run reason).
func newIngestEmbedder(log logr.Logger) credplane.Embedder {
	gwURL := strings.TrimSpace(os.Getenv("MODEL_GATEWAY_URL"))
	if gwURL == "" {
		log.Info("KB ingestion embedder DISABLED: MODEL_GATEWAY_URL unset — the ingest endpoint returns 501")
		return nil
	}
	log.Info("KB ingestion embedder enabled (ADR 0061 Fork 2): gateway embeddings, direct-write from worker",
		"gateway", gwURL)
	return credplane.NewGatewayEmbedder(gwURL, os.Getenv("MODEL_GATEWAY_KEY"),
		&http.Client{Timeout: 60 * time.Second})
}

// tokenServiceHTTPClient builds the http.Client the BFF uses to delegate grant writes to
// the token-service. When the BFF_TOKEN_SERVICE_TLS_* files are all present it is mTLS
// (the platform client cert + CA); otherwise it returns nil so credplane.NewClient uses a
// plain default client (dev only — the token-service also degrades to plain HTTP).
func tokenServiceHTTPClient(tsURL string) (*http.Client, error) {
	certFile := strings.TrimSpace(os.Getenv("BFF_TOKEN_SERVICE_TLS_CERT_FILE"))
	keyFile := strings.TrimSpace(os.Getenv("BFF_TOKEN_SERVICE_TLS_KEY_FILE"))
	caFile := strings.TrimSpace(os.Getenv("BFF_TOKEN_SERVICE_CA_FILE"))
	if certFile == "" || keyFile == "" || caFile == "" {
		return nil, nil
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("read client cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read client key: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	serverName := ""
	if u, uErr := url.Parse(tsURL); uErr == nil {
		serverName = u.Hostname()
	}
	tlsCfg, err := credplane.ClientTLSConfig(certPEM, keyPEM, caPEM, serverName)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

// parseGenerationModels splits the PLATFORM_GENERATION_MODELS env (a
// comma-separated list) into the pinned generation-model list (ADR 0014). Empty
// or whitespace-only entries are dropped; an empty env → nil (unpinned — the
// default). Kept permissive so a fresh install needs no extra config.
func parseGenerationModels(raw string) []string {
	var out []string
	for m := range strings.SplitSeq(raw, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// buildAdapters constructs the optional server-side adapters from environment
// variables (the same server-side creds the collector/M9 use). A missing or
// incomplete config leaves that adapter nil — the server then serves 501 for its
// routes instead of a broken client. Credentials read here stay in-process.
//
// Env:
//   - LANGFUSE_HOST / LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY → Langfuse adapter
//     (LANGFUSE_HOST = in-cluster API host; server-side only)
//   - LANGFUSE_UI_URL → external, browser-reachable Langfuse root for the trace
//     link-out (ADR 0038). Optional; falls back to LANGFUSE_HOST when unset.
//   - PROMETHEUS_URL [+ PROMETHEUS_TOKEN]                       → Prometheus adapter
func buildAdapters(log logr.Logger) bff.Adapters {
	var adapters bff.Adapters

	langfuse, err := bff.NewLangfuseAdapter(bff.LangfuseConfig{
		BaseURL:   os.Getenv("LANGFUSE_HOST"),
		UIBaseURL: os.Getenv("LANGFUSE_UI_URL"),
		PublicKey: os.Getenv("LANGFUSE_PUBLIC_KEY"),
		SecretKey: os.Getenv("LANGFUSE_SECRET_KEY"),
	})
	if err != nil {
		log.Info("Langfuse adapter not configured; /api/runs|cost|traces serve 501", "reason", err.Error())
	} else {
		adapters.Langfuse = langfuse
	}

	prom, err := bff.NewPrometheusAdapter(bff.PrometheusConfig{
		BaseURL:     os.Getenv("PROMETHEUS_URL"),
		BearerToken: os.Getenv("PROMETHEUS_TOKEN"),
	})
	if err != nil {
		log.Info("Prometheus adapter not configured; /api/cost omits latency/scale series", "reason", err.Error())
	} else {
		adapters.Prometheus = prom
	}

	return adapters
}
