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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/agentmemory"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
	"github.com/ctxmesh/agent-engine/internal/credplane"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
	"github.com/ctxmesh/agent-engine/internal/credstore"
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
	// isOrgScoped drives org-scope resolution (ADR 0029 §2): when the invoker has no
	// personal grant and the server is EXPLICITLY org-scoped, resolve the shared credential.
	isOrgScoped := func(ctx context.Context, ns, server string) (bool, error) {
		tr, err := getTR(ctx, ns, server)
		if err != nil || tr == nil {
			return false, err
		}
		return tr.Labels[mcpScopeLabel] == scopeOrgValue, nil
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
		tsServer.WithMemory(
			agentmemory.NewPostgresStore(cpDB),
			credplane.NewGatewayEmbedder(gwURL, os.Getenv("MODEL_GATEWAY_KEY"), &http.Client{Timeout: 30 * time.Second}),
		)
		log.Info("long-term memory enabled (ADR 0045): pgvector store + gateway embeddings", "gateway", gwURL)
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
	mtls := certFile != "" && keyFile != "" && caFile != "" && filesExist(certFile, keyFile, caFile)
	if mtls {
		tlsCfg, err := serverMTLS(certFile, keyFile, caFile)
		if err != nil {
			return err
		}
		srv.TLSConfig = tlsCfg
	} else {
		log.Info("WARNING: token-service running WITHOUT mTLS (no TOKEN_SERVICE_TLS_* files) — " +
			"the credential API is unauthenticated; provision platform certs before production (ADR 0030 §1)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Info("token-service listening", "addr", listenAddr, "mtls", mtls, "credentialNamespace", credentialNS)
		var err error
		if mtls {
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

// serverMTLS loads the mTLS server config from mounted cert/key/CA files.
func serverMTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	certPEM, keyPEM, caPEM, err := readTriple(certFile, keyFile, caFile)
	if err != nil {
		return nil, err
	}
	return credplane.ServerTLSConfig(certPEM, keyPEM, caPEM)
}

func readTriple(certFile, keyFile, caFile string) (cert, key, ca []byte, err error) {
	if cert, err = os.ReadFile(certFile); err != nil {
		return nil, nil, nil, fmt.Errorf("read tls cert: %w", err)
	}
	if key, err = os.ReadFile(keyFile); err != nil {
		return nil, nil, nil, fmt.Errorf("read tls key: %w", err)
	}
	if ca, err = os.ReadFile(caFile); err != nil {
		return nil, nil, nil, fmt.Errorf("read client CA: %w", err)
	}
	return cert, key, ca, nil
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
