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

// Command egress-sidecar is the per-pod injecting egress proxy (ADR 0030 §1). It runs as a
// SEPARATE container beside the agent (user) container: the agent's MCP tool calls are
// pointed at it on localhost, and for each call it verifies the run capability, resolves
// the invoking user's OBO credential from the credential plane, injects it as a bearer, and
// forwards to the real MCP server — so the agent holds neither the token nor the real URL.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ctxmesh/agent-engine/internal/credplane"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
	"github.com/ctxmesh/agent-engine/internal/egress"
	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// defaultCapabilityAudience mirrors the BFF default (internal/bff) — the credential-plane
// audience run capabilities are minted for and this sidecar verifies against.
const defaultCapabilityAudience = "ctxmesh-credential-plane"

const (
	defaultListenAddr   = "127.0.0.1:8081"
	readHeaderTimeout   = 10 * time.Second
	shutdownGracePeriod = 10 * time.Second
	delegationTimeout   = 20 * time.Second
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("egress-sidecar")
	if err := run(log); err != nil {
		log.Error(err, "egress-sidecar exited with error")
		os.Exit(1)
	}
}

func run(log logr.Logger) error {
	// The public key the BFF's private key pairs with — REQUIRED (the sidecar cannot verify
	// a capability without it, and must never fail open).
	pubB64 := strings.TrimSpace(os.Getenv("MCP_CAPABILITY_PUBLIC_KEY"))
	if pubB64 == "" {
		return errors.New("MCP_CAPABILITY_PUBLIC_KEY is required (the platform capability public key)")
	}
	pub, err := runcap.DecodePublicKey(pubB64)
	if err != nil {
		return fmt.Errorf("decode MCP_CAPABILITY_PUBLIC_KEY: %w", err)
	}
	audience := strings.TrimSpace(os.Getenv("MCP_CAPABILITY_AUDIENCE"))
	if audience == "" {
		audience = defaultCapabilityAudience
	}

	// The routes the controller rendered from the agent's bindings (m25.8), delivered EITHER as a
	// hot-reloadable mounted ConfigMap file (EGRESS_ROUTES_FILE — J7: a remote-tool-URL edit takes
	// effect on the running sidecar WITHOUT a revision roll) OR, legacy, as the static EGRESS_ROUTES
	// env. At least one is required — with no routes the sidecar fronts nothing.
	var (
		routes       egress.RouteTable   // static (EGRESS_ROUTES) — used when no file is mounted.
		routesHolder *egress.RouteHolder // hot-reloadable (EGRESS_ROUTES_FILE) — supersedes when set.
	)
	routesFile := strings.TrimSpace(os.Getenv("EGRESS_ROUTES_FILE"))
	if routesFile != "" {
		routesHolder = &egress.RouteHolder{}
		if lErr := loadInitialRoutes(routesHolder, routesFile, log); lErr != nil {
			return fmt.Errorf("loading EGRESS_ROUTES_FILE %q: %w", routesFile, lErr)
		}
	} else {
		routesJSON := strings.TrimSpace(os.Getenv("EGRESS_ROUTES"))
		if routesJSON == "" {
			return errors.New("EGRESS_ROUTES or EGRESS_ROUTES_FILE is required (the JSON server route table)")
		}
		routes, err = egress.ParseRouteTable([]byte(routesJSON))
		if err != nil {
			return err
		}
	}

	// The agent's own namespace is the grant SOURCE namespace; the locked credential
	// namespace holds the grant Secrets. POD_NAMESPACE is injected by the downward API.
	podNS := strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	if podNS == "" {
		return errors.New("POD_NAMESPACE is required (the agent's namespace — the grant source)")
	}
	credentialNS := strings.TrimSpace(os.Getenv("MCP_CREDENTIAL_NAMESPACE"))
	// The agent identity this sidecar serves (ns/name); a capability minted for another
	// agent is rejected. Optional — empty skips the agent-scope check.
	expectedAgent := strings.TrimSpace(os.Getenv("EGRESS_AGENT"))
	// EGRESS_BOUNDARY is the agent's trust boundary (ADR 0033) — its registry, or "a:<ns>/<name>"
	// when standalone — injected by the controller. When set it is the scoping gate (supersedes
	// EGRESS_AGENT) so teammates in the same registry can redeem a relayed capability.
	expectedBoundary := strings.TrimSpace(os.Getenv("EGRESS_BOUNDARY"))

	listenAddr := strings.TrimSpace(os.Getenv("EGRESS_LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}

	resolver, err := buildResolver(log, routes, credentialNS)
	if err != nil {
		return err
	}

	// Record mode (M78, ADR 0071 §1/C1): a RECORD-CAPABLE sidecar (RECORD_CAPABLE=true, injected by
	// the controller for a spec.record agent) is the TOOL capture seam — it fronts every tool and
	// captures each recorded call's I/O into the run's replay fixture. Build the recorder here; it
	// FAILS CLOSED (C2) when OBJECT_STORE_ADDR is unset (no fixture sink) — a hard startup error, so
	// a record-capable sidecar never fronts all tools while silently capturing nothing. A
	// non-record-capable sidecar gets a nil recorder ⇒ the capture path is a no-op (byte-for-byte
	// the pre-M78 OBO behavior). The agent identity (EGRESS_AGENT, "ns/name") is fixture provenance.
	var recorder *egress.ToolRecorder
	if strings.EqualFold(strings.TrimSpace(os.Getenv("RECORD_CAPABLE")), "true") {
		rec, rerr := egress.NewToolRecorder(expectedAgent, func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) })
		if rerr != nil {
			return fmt.Errorf("egress-sidecar: record mode requested but not usable: %w", rerr)
		}
		recorder = rec
	}

	// Tool-call governance (M82, ADR 0074 §1): read + hold the resolved spec.runtime.toolPolicy the
	// controller mounts at TOOL_POLICY_FILE, and fsnotify-WATCH it for live reload. The proxy ENFORCES
	// it (deny 403 / require-approval voucher / fan-out ceiling). Absent/empty ⇒ nil policy (permissive,
	// the un-governed agent). A MALFORMED initial policy is a HARD startup error (C16, ADR 0087):
	// starting permissive on a broken policy would silently disable configured governance.
	policyHolder := &egress.PolicyHolder{}
	toolPolicyFile := strings.TrimSpace(os.Getenv("TOOL_POLICY_FILE"))
	if toolPolicyFile != "" {
		if lErr := loadInitialToolPolicy(policyHolder, toolPolicyFile, log); lErr != nil {
			return fmt.Errorf("loading TOOL_POLICY_FILE %q: %w", toolPolicyFile, lErr)
		}
	}

	proxy := egress.NewProxy(egress.ProxyConfig{
		Verifier:         runcap.NewVerifier(pub, audience, nil),
		Resolver:         resolver,
		Namespace:        podNS,
		ExpectedAgent:    expectedAgent,
		ExpectedBoundary: expectedBoundary,
		Routes:           routes,
		RoutesHolder:     routesHolder, // J7: nil ⇒ the static Routes; set ⇒ the hot-reloadable table.
		Log:              log,
		Recorder:         recorder,
		Policy:           policyHolder,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	// /metrics (J9): the per-(agent,tool) fan-out counter the proxy records at the tool-call chokepoint.
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/", proxy)

	srv := &http.Server{Addr: listenAddr, Handler: mux, ReadHeaderTimeout: readHeaderTimeout}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Tool-call governance (M82, ADR 0074 §1): fsnotify-watch the mounted tool-policy file so a
	// controller-driven policy edit (ConfigMap update in place) reloads live — no restart. The
	// watch is tied to the server lifecycle (ctx.Done closes it). Only when a file is mounted.
	if toolPolicyFile != "" {
		watchStop := make(chan struct{})
		go watchToolPolicy(policyHolder, toolPolicyFile, log, watchStop)
		defer close(watchStop)
	}

	// Routes hot-reload (J7): fsnotify-watch the mounted routes file so a controller-driven remote-tool-
	// URL edit (an in-place ConfigMap update) reloads live — no restart, no revision roll. Only when the
	// file-delivery seam is in use (EGRESS_ROUTES_FILE); the legacy static EGRESS_ROUTES env is fixed.
	if routesFile != "" {
		routesWatchStop := make(chan struct{})
		go watchRoutes(routesHolder, routesFile, log, routesWatchStop)
		defer close(routesWatchStop)
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("egress-sidecar listening", "addr", listenAddr, "routes", len(routes), "agent", expectedAgent)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Info("egress-sidecar shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// buildResolver returns the sidecar's credential resolver: a DELEGATING client to the
// central token service when TOKEN_SERVICE_URL is set (the scaling split — the global
// singleflight + refresh live there, ADR 0030 §1), else an EMBEDDED credresolve backend
// that reads grants directly (the first working cut). Both satisfy the same interface, so
// the proxy is unchanged either way.
func buildResolver(
	log logr.Logger, routes egress.RouteTable, credentialNS string,
) (credresolve.CredentialResolver, error) {
	if tokenServiceURL := strings.TrimSpace(os.Getenv("TOKEN_SERVICE_URL")); tokenServiceURL != "" {
		httpClient, err := delegatingHTTPClient(log)
		if err != nil {
			return nil, err
		}
		log.Info("egress-sidecar delegating resolution to the central token service", "url", tokenServiceURL)
		return credplane.NewClient(tokenServiceURL, httpClient), nil
	}

	// Embedded: read grants directly (the sidecar SA is RBAC-scoped to Secret get in the
	// credential namespace — Helm, m25.8).
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("build scheme: %w", err)
	}
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	k8sClient, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("build client: %w", err)
	}
	log.Info("egress-sidecar using an embedded credential backend (set TOKEN_SERVICE_URL to delegate)")
	return credresolve.NewK8sBackend(credresolve.K8sBackendConfig{
		Client:              k8sClient,
		CredentialNamespace: credentialNS,
		Exchanger:           &credresolve.HTTPTokenExchanger{},
		AuthTypeIsOAuth: func(_ context.Context, _, server string) (bool, error) {
			route, ok := routes[server]
			return ok && route.OAuth, nil
		},
		Audit: func(e credresolve.AuditEvent) {
			// Credential-class provenance (never a token) — ADR 0029 §7 R8.
			log.Info("egress grant use",
				"action", string(e.Action), "server", e.Server, "user", e.UserHash, "class", string(e.Class))
		},
	}), nil
}

// delegatingHTTPClient builds the HTTP client the sidecar uses to reach the central token
// service: mTLS from mounted certs when configured (EGRESS_MTLS_* + TOKEN_SERVICE_SERVER_NAME),
// else a plain client with a loud warning (dev only).
func delegatingHTTPClient(log logr.Logger) (*http.Client, error) {
	certFile := strings.TrimSpace(os.Getenv("EGRESS_MTLS_CERT_FILE"))
	keyFile := strings.TrimSpace(os.Getenv("EGRESS_MTLS_KEY_FILE"))
	caFile := strings.TrimSpace(os.Getenv("EGRESS_MTLS_CA_FILE"))
	serverName := strings.TrimSpace(os.Getenv("TOKEN_SERVICE_SERVER_NAME"))
	if certFile == "" || keyFile == "" || caFile == "" {
		log.Info("WARNING: delegating to the token service WITHOUT mTLS (no EGRESS_MTLS_* files) — " +
			"provision platform certs before production (ADR 0030 §1)")
		return &http.Client{Timeout: delegationTimeout}, nil
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("read mTLS cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read mTLS key: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read mTLS CA: %w", err)
	}
	tlsCfg, err := credplane.ClientTLSConfig(certPEM, keyPEM, caPEM, serverName)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}, Timeout: delegationTimeout}, nil
}
