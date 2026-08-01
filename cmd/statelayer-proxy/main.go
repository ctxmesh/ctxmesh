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

// Command statelayer-proxy is the control-plane state-layer proxy (M51, ADR 0050):
// it holds the Valkey credential and enforces per-tenant/agent memory scope
// SERVER-SIDE from the verified run-capability token, so agent workloads hold no
// credential and cannot name another tenant's key.
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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ctxmesh/agent-engine/internal/runcap"
	"github.com/ctxmesh/agent-engine/internal/statelayer"
)

// The proxy verifies TWO unrelated token types, each with its own audience:
//   - defaultCapabilityAudience — the runcap (Ed25519 JWT) audience for the memory
//     path (STATELAYER_CAPABILITY_AUDIENCE; ADR 0050 §2 + Amд 1).
//   - defaultPodAudience — the Kubernetes projected SA-token audience for the
//     quota/async paths (STATELAYER_POD_AUDIENCE; ADр 3). It MUST match the
//     `audiences` in the launcher's serviceAccountToken volume projection.
//
// They default to the same string but are validated by completely different
// mechanisms (signature vs TokenReview), so there is no cross-replay between them;
// kept as separate constants so an operator can diverge one without surprise.
const (
	defaultCapabilityAudience = "statelayer-proxy"
	defaultPodAudience        = "statelayer-proxy"
)

const (
	defaultListenAddr   = ":8080"
	readHeaderTimeout   = 10 * time.Second
	shutdownGracePeriod = 10 * time.Second
	cacheSyncTimeout    = 30 * time.Second
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("statelayer-proxy")
	if err := run(log); err != nil {
		log.Error(err, "statelayer-proxy exited with error")
		os.Exit(1)
	}
}

func run(log logr.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := strings.TrimSpace(os.Getenv("STATELAYER_ADDR"))
	if addr == "" {
		return errors.New("STATELAYER_ADDR is required (the Valkey host:port)")
	}
	store := statelayer.NewRedisStore(
		addr,
		strings.TrimSpace(os.Getenv("STATELAYER_USERNAME")),
		os.Getenv("STATELAYER_PASSWORD"), // may contain structural chars — do not trim
	)

	opts := statelayer.Options{Store: store}

	// The dev bypass (STATELAYER_DEV_AGENT="<ns>/<agent>") scopes unauthenticated
	// requests to a static identity — NEVER set in production.
	devAgent := strings.TrimSpace(os.Getenv("STATELAYER_DEV_AGENT"))
	opts.DevAgent = devAgent

	// Cluster-backed quota scoping (M53): the tenant resolver (namespace-label →
	// tenant, ADR 0050 §3 + Amд 2 Correction 2a) + the pod authenticator (cached
	// TokenReview, Amд 3), both from one in-cluster config. Best-effort — with no
	// cluster config (local dev/compose) both are disabled and the quota/async
	// endpoints report unavailable; memory still serves. Never fail the whole proxy
	// on their absence.
	if restCfg, cfgErr := ctrl.GetConfig(); cfgErr != nil {
		log.Info("no cluster config — quota/async endpoints disabled", "reason", cfgErr.Error())
	} else {
		if resolver, rerr := statelayer.StartNamespaceResolver(ctx, restCfg, cacheSyncTimeout); rerr != nil {
			if ctx.Err() != nil { // shutting down before the cache synced — not a real degradation
				log.Info("tenant resolver startup aborted by shutdown", "reason", rerr.Error())
			} else {
				log.Info("tenant resolution disabled — quota endpoints will be unavailable", "reason", rerr.Error())
			}
		} else {
			opts.TenantResolver = resolver
			log.Info("tenant resolver ready (namespace-label resolution)")
		}
		if podAuth, aerr := buildPodAuthenticator(restCfg); aerr != nil {
			log.Info("pod authentication disabled — quota endpoints will be unavailable", "reason", aerr.Error())
		} else {
			opts.PodAuthenticator = podAuth
			log.Info("pod authenticator ready (cached TokenReview)")
		}
	}

	// The capability verifier: REQUIRED in production (a distinct audience). Optional
	// only when the dev bypass is on (a local Compose/dev loop mints no token).
	pubB64 := strings.TrimSpace(os.Getenv("MCP_CAPABILITY_PUBLIC_KEY"))
	switch {
	case pubB64 != "":
		pub, err := runcap.DecodePublicKey(pubB64)
		if err != nil {
			return fmt.Errorf("decode MCP_CAPABILITY_PUBLIC_KEY: %w", err)
		}
		audience := strings.TrimSpace(os.Getenv("STATELAYER_CAPABILITY_AUDIENCE"))
		if audience == "" {
			audience = defaultCapabilityAudience
		}
		opts.Verifier = runcap.NewVerifier(pub, audience, nil)
	case devAgent == "":
		// No capability key AND no dev bypass: START anyway but refuse every request
		// (401). A fresh install deploys cleanly before the keypair is provisioned —
		// an idle proxy in migration phase 1 (ADR 0050 §8), not a CrashLoop.
		log.Info("no capability key and no dev bypass — proxy refuses all requests until the keypair is provisioned")
	}

	srv, err := statelayer.NewServer(opts)
	if err != nil {
		return err
	}

	listen := strings.TrimSpace(os.Getenv("STATELAYER_PROXY_ADDR"))
	if listen == "" {
		listen = defaultListenAddr
	}
	httpSrv := &http.Server{Addr: listen, Handler: srv.Handler(), ReadHeaderTimeout: readHeaderTimeout}

	errCh := make(chan error, 1)
	go func() {
		log.Info("statelayer-proxy listening", "addr", listen, "valkey", addr, "devBypass", devAgent != "")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal — draining")
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
		return httpSrv.Shutdown(sctx)
	}
}

// buildPodAuthenticator builds the cached-TokenReview PodAuthenticator (ADR 0050
// Amд 3) over a client-go clientset. The pod-token audience (STATELAYER_POD_AUDIENCE,
// default "statelayer-proxy") must match the audience the launcher's projected
// serviceAccountToken volume is minted for.
func buildPodAuthenticator(restCfg *rest.Config) (statelayer.PodAuthenticator, error) {
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	audience := strings.TrimSpace(os.Getenv("STATELAYER_POD_AUDIENCE"))
	if audience == "" {
		audience = defaultPodAudience
	}
	return statelayer.NewTokenReviewAuthenticator(clientset, audience, nil), nil
}
