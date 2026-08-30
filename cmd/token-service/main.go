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

// Command token-service is the CENTRAL TOKEN SERVICE (ADR 0030 §1, the scaling split). One
// Deployment in the locked credential namespace runs the credresolve backend behind an
// internal mTLS API; the per-pod egress sidecars delegate cache-miss resolution to it, so
// grant-Secret reads + OAuth refresh are singleflighted GLOBALLY (one refresh across the
// fleet), not per-pod — the two operations that hit ceilings are amortized here.
package main

import (
	"context"
	"crypto/tls"
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/controlplane"
	"github.com/ctxmesh/agentry/internal/controlplane/agentmemory"
	"github.com/ctxmesh/agentry/internal/controlplane/knowledge"
	"github.com/ctxmesh/agentry/internal/controlplane/toolregistry"
	"github.com/ctxmesh/agentry/internal/credplane"
	"github.com/ctxmesh/agentry/internal/credresolve"
	"github.com/ctxmesh/agentry/internal/credstore"
)

// mcpAuthTypeAnnotation MUST match internal/bff.annMCPAuthType — the non-secret annotation
// the register flow stamps on a server's ToolRegistry recording its auth tier. The central
// service reads it to decide consent-required (OAuth) vs no-credential (open) on a missing
// grant. (A single well-known key; a future consolidation could move MCP annotation keys to
// a shared package.)
const (
	mcpAuthTypeAnnotation = "agents.ctxmesh.ai/mcp-auth-type"
	oauthAuthType         = "oauth"
	// mcpScopeLabel / scopeOrgValue MUST match internal/bff.labelMCPScope + its "org" value:
	// the visibility/ownership label the register + admin-promote flows stamp. An org-scoped
	// server resolves the admin-set shared credential (ADR 0029 §2). Only an EXPLICIT org
	// label counts here (a grandfathered/absent label is not org for resolution — it falls
	// through to per-user consent, preserving today's behavior).
	mcpScopeLabel = "mcp.ctxmesh.ai/scope"
	scopeOrgValue = "org"
	// mcpCredentialSourceLabel / credentialSourceSharedValue MUST match
	// internal/bff.labelMCPCredentialSource + its credSourceShared value. Per ADR 0067 the
	// single `scope` label was split into two axes; credential-source is the REAL directive
	// for releasing the admin-set shared credential (the axis isOrgScoped actually cares
	// about). The legacy `scope == org` read is kept as a dual-read fallback for the
	// deprecation window (a pre-m73 row carries only the scope label).
	mcpCredentialSourceLabel    = "mcp.ctxmesh.ai/credential-source"
	credentialSourceSharedValue = "shared"
)

const (
	defaultListenAddr   = ":8443"
	readHeaderTimeout   = 10 * time.Second
	shutdownGracePeriod = 15 * time.Second
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("token-service")
	if err := run(log); err != nil {
		log.Error(err, "token-service exited with error")
		os.Exit(1)
	}
}

func run(log logr.Logger) error {
	credentialNS := strings.TrimSpace(os.Getenv("MCP_CREDENTIAL_NAMESPACE"))

	// In-cluster client: reads grant Secrets (credresolve) + ToolRegistry auth-type.
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("build scheme: %w", err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("add CRD scheme: %w", err)
	}
	if err := agentsv1beta1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("adding agents/v1beta1 to scheme: %w", err)
	}
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}
	k8sClient, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	// getTR fetches a server's ToolRegistry auth-type / org-scope; a missing registry
	// ⇒ (nil, nil), which the callers treat conservatively (not-OAuth / not-org) so a
	// transient absence never masquerades as a consent prompt or an org fallback.
	//
	// ToolRegistry is retired to Postgres (ADR 0044 / M45): the CRD is gone, so this
	// per-tool-call read comes from the control-plane store behind a short-TTL,
	// serve-stale-on-error cache that bounds the blast radius of a Postgres blip on
	// the OBO hot path (Fable's design gate). CONTROLPLANE_DSN is required.
	cpDSN := strings.TrimSpace(os.Getenv("CONTROLPLANE_DSN"))
	if cpDSN == "" {
		return errors.New("CONTROLPLANE_DSN is required: ToolRegistry is retired to Postgres (ADR 0044)")
	}
	cpDB, dbErr := controlplane.Connect(context.Background(), cpDSN)
	if dbErr != nil {
		return fmt.Errorf("connect control-plane postgres for ToolRegistry reads: %w", dbErr)
	}
	defer func() { _ = cpDB.Close() }()
	trSource := newToolRegistrySource(
		toolregistry.NewPostgresStore(cpDB), defaultToolRegistryCacheTTL, log.WithName("toolregistry"))
	getTR := trSource.getTR
	log.Info("ToolRegistry served from Postgres (ADR 0044): CRD retired; short-TTL serve-stale cache")
	authTypeIsOAuth := func(ctx context.Context, ns, server string) (bool, error) {
		tr, err := getTR(ctx, ns, server)
		if err != nil || tr == nil {
			return false, err
		}
		return tr.Annotations[mcpAuthTypeAnnotation] == oauthAuthType, nil
	}
	// isOrgScoped drives shared-credential resolution (ADR 0029 §2, refined by ADR 0067): when the
	// invoker has no personal grant and the server's credential-source is `shared`, resolve the
	// admin-set shared credential. Per ADR 0067 credential-source is the real axis; the legacy
	// `scope == org` read is kept as a dual-read fallback for the deprecation window (orgScopedFromLabels).
	isOrgScoped := func(ctx context.Context, ns, server string) (bool, error) {
		tr, err := getTR(ctx, ns, server)
		if err != nil || tr == nil {
			return false, err
		}
		return orgScopedFromLabels(tr.Labels), nil
	}

	// The credential backend is CONFIG-SELECTED per the CredentialStore / ClusterCredentialStore
	// CRDs (ADR 0032); with no CRD present it is the built-in kubernetes backend, so existing
	// installs are unchanged. Backends are shared per resolved config, so the cache + singleflight
	// + optimistic writeback stay global across every delegating sidecar (ADR 0030 §1).
	auditFn := func(e credresolve.AuditEvent) {
		log.Info("grant use", "action", string(e.Action), "server", e.Server, "user", e.UserHash, "class", string(e.Class))
	}
	router := credstore.NewRouter(k8sClient, credstore.Deps{
		Client:                     k8sClient,
		DefaultCredentialNamespace: credentialNS,
		Exchanger:                  &credresolve.HTTPTokenExchanger{},
		AuthTypeIsOAuth:            authTypeIsOAuth,
		IsOrgScoped:                isOrgScoped,
		Audit:                      auditFn,
	})

	// A legacy k8s backend — the source for a one-time migration and the fallback for the
	// dual-read cutover window (m28.2, ADR 0032).
	k8sBackend := credresolve.NewK8sBackend(credresolve.K8sBackendConfig{
		Client:              k8sClient,
		CredentialNamespace: credentialNS,
		Exchanger:           &credresolve.HTTPTokenExchanger{},
		AuthTypeIsOAuth:     authTypeIsOAuth,
		OrgCredential:       credresolve.NewOrgCredentialFunc(k8sClient, credentialNS, isOrgScoped),
		Audit:               auditFn,
	})

	// One-time backfill: TOKEN_SERVICE_MIGRATE_GRANTS=true lifts every legacy k8s grant into
	// the config-selected backend (per namespace via the Router), logs the count, and exits.
	if envTrue("TOKEN_SERVICE_MIGRATE_GRANTS") {
		n, mErr := credstore.Migrate(context.Background(), k8sBackend, router)
		if mErr != nil {
			return fmt.Errorf("migrate grants: %w", mErr)
		}
		log.Info("grant migration complete", "migrated", n)
		return nil
	}

	// Dual-read cutover window: TOKEN_SERVICE_DUAL_READ=true resolves fall back to the legacy
	// k8s backend on a miss, so a cutover to a non-kubernetes backend loses no connected account.
	var resolver credresolve.CredentialResolver = router
	if envTrue("TOKEN_SERVICE_DUAL_READ") {
		resolver = credstore.NewDualRead(router, k8sBackend)
		log.Info("dual-read enabled: legacy k8s grants still resolve during the migration window")
	}

	tsServer := credplane.NewServer(resolver, log)
	// Long-term memory (ADR 0045): the token-service is the sole holder of the pgvector store + embeds via
	// the gateway; agent launchers proxy memory.remember/search_agent here (no DB creds in agent pods). Enabled
	// when the model gateway is reachable — reuses the already-open control-plane DB (cpDB).
	if gwURL := strings.TrimSpace(os.Getenv("MODEL_GATEWAY_URL")); gwURL != "" {
		embedder := credplane.NewGatewayEmbedder(
			gwURL, os.Getenv("MODEL_GATEWAY_KEY"), &http.Client{Timeout: 30 * time.Second})
		tsServer.WithMemory(agentmemory.NewPostgresStore(cpDB), embedder)
		log.Info("long-term memory enabled (ADR 0045): pgvector store + gateway embeddings", "gateway", gwURL)
		// Managed-RAG retrieval (ADR 0061 Fork 3 + governance #8): the token-service is the sole holder of the
		// pgvector knowledge store; agent launchers proxy the READ path here (no DB creds in agent pods). The
		// ingestion WRITE path is the run-worker holding its own store (m68.6). Reuses the same control-plane DB
		// (cpDB) + gateway embedder as long-term memory — enabled on the same gateway-reachable condition.
		tsServer.WithKnowledge(knowledge.NewPostgresStore(cpDB), embedder)
		log.Info("managed-RAG retrieval enabled (ADR 0061): pgvector knowledge store + gateway embeddings", "gateway", gwURL)
	}
	handler := tsServer.Handler()
	listenAddr := envOr("TOKEN_SERVICE_LISTEN_ADDR", defaultListenAddr)
	srv := &http.Server{Addr: listenAddr, Handler: handler, ReadHeaderTimeout: readHeaderTimeout}

	// mTLS: only platform sidecars holding a client cert from the platform CA may call the
	// credential API. Certs are mounted from a Secret (Helm, m25.8). Absent ⇒ plain HTTP with
	// a LOUD warning (dev only — never run the credential API without mTLS in production).
	certFile := strings.TrimSpace(os.Getenv("TOKEN_SERVICE_TLS_CERT_FILE"))
	keyFile := strings.TrimSpace(os.Getenv("TOKEN_SERVICE_TLS_KEY_FILE"))
	caFile := strings.TrimSpace(os.Getenv("TOKEN_SERVICE_CLIENT_CA_FILE"))
	// mTLS engages only when all three files are set AND present. The Helm manifest always
	// points the env at an OPTIONAL cert Secret mount, so an install that has not provisioned
	// platform certs degrades to plain HTTP (dev) instead of crash-looping — the operator
	// drops in the Secret to switch mTLS on with no manifest change.
	// TLS postures (M128/Gate E, ADR 0102 §3):
	//   - serving = a serving cert+key is provisioned → the wire is encrypted + the server is verifiable.
	//   - E-2 (mutual): a client CA is ALSO provisioned → REQUIRE + verify a client cert.
	//   - E-1 (server-auth): serving cert, no client CA → present the serving cert, don't require a client
	//     cert (client identity is the app-layer run capability, ADR 0030). The honest GA bar.
	serving := certFile != "" && keyFile != "" && filesExist(certFile, keyFile)
	clientCAPresent := caFile != "" && filesExist(caFile)
	// clientAuth knob: default = require iff a client CA is provisioned (NEVER downgrade an operator who
	// deployed mutual material — ADR 0102 §3); TOKEN_SERVICE_CLIENT_AUTH=require|none forces it.
	requireClient := clientCAPresent
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKEN_SERVICE_CLIENT_AUTH"))) {
	case "require":
		requireClient = true
	case "none":
		requireClient = false
	}
	if requireClient && !clientCAPresent {
		return fmt.Errorf("TOKEN_SERVICE_CLIENT_AUTH=require but no client CA is provisioned " +
			"(missing/absent TOKEN_SERVICE_CLIENT_CA_FILE) — refusing to start a mutual-mTLS server without a client CA")
	}
	// SEC-5: fail CLOSED when TLS is required. The credential plane dispenses third-party user
	// credentials — a SILENT downgrade to plain HTTP in production is unacceptable. Production sets
	// TOKEN_SERVICE_TLS_REQUIRED=true; E-1 (server-auth) SATISFIES it — the wire is encrypted.
	tlsRequired := strings.EqualFold(strings.TrimSpace(os.Getenv("TOKEN_SERVICE_TLS_REQUIRED")), "true")
	if tlsRequired && !serving {
		return fmt.Errorf("TOKEN_SERVICE_TLS_REQUIRED=true but no serving cert is provisioned " +
			"(missing/absent TOKEN_SERVICE_TLS_CERT_FILE/KEY_FILE) — refusing to serve the credential API over plain HTTP")
	}
	var certWatcher *certwatcher.CertWatcher
	if serving {
		tlsCfg, watcher, err := serverTLS(certFile, keyFile, caFile, requireClient)
		if err != nil {
			return err
		}
		srv.TLSConfig = tlsCfg
		certWatcher = watcher
		posture := "server-auth (E-1)"
		if requireClient {
			posture = "mutual-mTLS (E-2)"
		}
		log.Info("token-service TLS enabled", "posture", posture)
	} else {
		log.Info("WARNING: token-service running WITHOUT TLS (no TOKEN_SERVICE_TLS_CERT_FILE/KEY_FILE) — " +
			"the credential API is unauthenticated; provision platform certs before production (ADR 0030 §1 / ADR 0102 §3)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Hot-reload the serving cert on rotation (M128/ADR 0102): the watcher runs until ctx is
	// cancelled (shutdown); a watch error is logged, not fatal — the last-good cert keeps serving.
	if certWatcher != nil {
		go func() {
			if err := certWatcher.Start(ctx); err != nil {
				log.Error(err, "token-service: cert watcher stopped")
			}
		}()
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("token-service listening", "addr", listenAddr, "tls", serving, "credentialNamespace", credentialNS)
		var err error
		if serving {
			err = srv.ListenAndServeTLS("", "") // certs come from TLSConfig
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Info("token-service shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// orgScopedFromLabels reports whether a server's store-record labels direct the runtime to release
// the admin-set SHARED credential (ADR 0029 §2, refined by ADR 0067). It DUAL-READs the two-axis
// taxonomy: the authoritative signal is credentialSource == shared; the legacy scope == org read is
// the fallback for the deprecation window, so a pre-m73 row (scope-only) AND a post-migration row
// (credential-source only) AND a dual-write-window row (both) all resolve the shared credential. A
// private/byo-oauth/none server, or one with neither label, is not org-scoped (fail-closed to
// per-user consent). Extracted as a pure function so the survives-the-relabel proof is a plain unit test.
func orgScopedFromLabels(labels map[string]string) bool {
	return labels[mcpCredentialSourceLabel] == credentialSourceSharedValue ||
		labels[mcpScopeLabel] == scopeOrgValue
}

// serverTLS builds the token-service's server TLS config (E-1 server-auth OR E-2 mutual, per
// requireClient) AND a CertWatcher that hot-reloads the SERVING cert on rotation (M128/Gate E,
// ADR 0102 §1/§3): the platform cert-controller rotates the serving leaf ~every 90d, and a
// one-shot load would then serve a stale/expired cert until a pod restart. GetCertificate (from
// the watcher) is consulted at each TLS handshake, so a rotated cert is picked up with no restart
// + no dropped established connections. When requireClient (E-2), the client CA is loaded once for
// mutual verification. The caller MUST Start the returned watcher with a context.
func serverTLS(certFile, keyFile, caFile string, requireClient bool) (*tls.Config, *certwatcher.CertWatcher, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read tls cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read tls key: %w", err)
	}
	var cfg *tls.Config
	if requireClient {
		caPEM, rErr := os.ReadFile(caFile)
		if rErr != nil {
			return nil, nil, fmt.Errorf("read client CA: %w", rErr)
		}
		cfg, err = credplane.ServerTLSConfig(certPEM, keyPEM, caPEM) // E-2 mutual (RequireAndVerifyClientCert)
	} else {
		cfg, err = credplane.ServerTLSConfigServerAuth(certPEM, keyPEM) // E-1 server-auth (NoClientCert)
	}
	if err != nil {
		return nil, nil, err
	}
	watcher, err := certwatcher.New(certFile, keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("token-service: init cert watcher: %w", err)
	}
	// GetCertificate takes precedence over the static Certificates at handshake time; clearing
	// Certificates makes the watcher the single source of the live serving cert.
	cfg.Certificates = nil
	cfg.GetCertificate = watcher.GetCertificate
	return cfg, watcher, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// envTrue reports whether an env var is a truthy string.
func envTrue(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// filesExist reports whether every path is a readable regular file — used to gate mTLS on
// an OPTIONAL cert Secret actually being mounted (absent ⇒ degrade to plain HTTP, not crash).
func filesExist(paths ...string) bool {
	for _, p := range paths {
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			return false
		}
	}
	return true
}
