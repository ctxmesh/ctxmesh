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

// Command bff is the agent-engine Backend-for-Frontend: it serves the static
// Vite SPA build and the /api surface (client-go reads of the agent CRDs) behind
// the M11 control-plane auth. It reuses the controllers' client-go — the K8s
// credentials stay in this process; the browser never receives them (ADR 0010).
//
// Deployment: this runs as a small server in the control plane. It is NOT a Node
// runtime — the UI is static assets served from --static-dir.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/bff"
)

func main() {
	var (
		addr      string
		staticDir string
		version   string
	)
	flag.StringVar(&addr, "addr", ":9090", "The address the BFF listens on.")
	flag.StringVar(&staticDir, "static-dir", "ui/dist",
		"Directory of the built Vite SPA (dist/). Empty disables static serving.")
	flag.StringVar(&version, "version", "dev", "Version string reported by /api/health.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("bff")

	if err := run(addr, staticDir, version, log); err != nil {
		log.Error(err, "BFF exited with error")
		os.Exit(1)
	}
}

func run(addr, staticDir, version string, log logr.Logger) error {
	// Build the platform scheme (control-plane CRDs) so the caller-scoped client
	// can encode/decode the agent CRDs.
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	// The in-cluster rest.Config supplies the API-server host + cluster CA/TLS.
	// The BFF does NOT build a static client from it for user CRD ops: instead the
	// CallerClientFactory copies this config per request and swaps in the CALLER'S
	// bearer token (ADR 0011), so the K8s API server enforces the caller's own
	// RBAC (M11 personas). The BFF's own SA credential is never used to act on the
	// user's CRDs — closing the confused-deputy gap by construction.
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
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
	providerConnect := providerConnectEnabled(os.Getenv("PROVIDER_CONNECT_ENABLED"))
	if !providerConnect {
		log.Info("provider-connect disabled by PROVIDER_CONNECT_ENABLED=false; /api/providers routes serve 404")
	}

	// The create-from-prompt platform generation-model pin (ADR 0014). Empty (the
	// default) → generation uses the caller's connected-provider model unpinned. An
	// operator that wants a governed generation model sets a comma-separated list
	// (PLATFORM_GENERATION_MODELS) — the UI's model dropdown source and the allowed
	// set the generate endpoint enforces.
	platformGenModels := parseGenerationModels(os.Getenv("PLATFORM_GENERATION_MODELS"))
	if len(platformGenModels) > 0 {
		log.Info("platform generation models pinned", "models", platformGenModels)
	}

	// The BYO-MCP kill-switch + trust policy (ADR 0016). MCP_ENABLED defaults TRUE
	// (dev/trial); a hardened install sets it false to 404 the register/catalog
	// endpoints. MCP_REQUIRE_APPROVAL defaults FALSE (self-serve — tools are
	// immediately bindable); a hardened install sets it true to mark newly
	// registered tools pending-approval (the approval queue is M17). Same
	// "flag-from-env" pattern as the connect kill-switch.
	mcpEnabled := envEnabledDefaultTrue(os.Getenv("MCP_ENABLED"))
	if !mcpEnabled {
		log.Info("BYO-MCP disabled by MCP_ENABLED=false; /api/mcpservers + /api/tools serve 404")
	}
	mcpRequireApproval := envTrue(os.Getenv("MCP_REQUIRE_APPROVAL"))
	if mcpRequireApproval {
		log.Info("BYO-MCP hardened: registered MCP tools are marked pending-approval (MCP_REQUIRE_APPROVAL=true)")
	}

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

	// Console SSO advertisement (ADR 0020). OIDC_ENABLED=true + an issuer + a client
	// id → GET /api/authconfig tells the SPA to run Auth-Code+PKCE against Dex; else
	// the SPA uses token login (ADR 0012). The BFF holds NO OIDC secret (public client).
	oidcEnabled := envTrue(os.Getenv("OIDC_ENABLED"))
	oidcIssuer := strings.TrimSpace(os.Getenv("OIDC_ISSUER"))
	oidcClientID := strings.TrimSpace(os.Getenv("OIDC_CLIENT_ID"))
	if oidcEnabled {
		log.Info("console SSO enabled (ADR 0020): advertising OIDC at /api/authconfig",
			"issuer", oidcIssuer, "clientID", oidcClientID)
	}

	srv := bff.NewServer(bff.Options{
		CallerClients:            callerClients,
		Scheme:                   scheme,
		Auth:                     bff.BearerAuthenticator{},
		Adapters:                 adapters,
		Version:                  version,
		StaticDir:                staticDir,
		ProviderConnect:          providerConnect,
		PlatformGenerationModels: platformGenModels,
		MCPEnabled:               mcpEnabled,
		MCPRequireApproval:       mcpRequireApproval,
		MCPGrantHMACKey:          mcpGrantHMACKey,
		MCPCredentialNamespace:   mcpCredentialNamespace,
		CredentialClient:         credentialClient,
		OIDCEnabled:              oidcEnabled,
		OIDCIssuer:               oidcIssuer,
		OIDCClientID:             oidcClientID,
		Log:                      ctrl.Log.WithName("bff.server"),
	})

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("BFF listening", "addr", addr, "staticDir", staticDir)
		if serveErr := httpSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case serveErr := <-errCh:
		return serveErr
	}
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
//   - PROMETHEUS_URL [+ PROMETHEUS_TOKEN]                       → Prometheus adapter
func buildAdapters(log logr.Logger) bff.Adapters {
	var adapters bff.Adapters

	langfuse, err := bff.NewLangfuseAdapter(bff.LangfuseConfig{
		BaseURL:   os.Getenv("LANGFUSE_HOST"),
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
