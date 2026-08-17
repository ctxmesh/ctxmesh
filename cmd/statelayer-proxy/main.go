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

	"github.com/ctxmesh/agent-engine/internal/statelayer"
)

// defaultPodAudience is the Kubernetes projected SA-token audience the proxy verifies for
// ALL paths — memory, quota, and async (ADR 0052 §C6 RESOLUTION unified them on pod
// identity; STATELAYER_POD_AUDIENCE). It MUST match the `audiences` in the launcher's
// serviceAccountToken volume projection.
const defaultPodAudience = "statelayer-proxy"

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
	username := strings.TrimSpace(os.Getenv("STATELAYER_USERNAME"))
	password := os.Getenv("STATELAYER_PASSWORD") // may contain structural chars — do not trim
	store := statelayer.NewRedisStore(addr, username, password)

	// The quota accumulator + async seen-set share the same credentialed Valkey
	// (ADR 0050 §5/§6, M53) — the proxy fronts all three call sites.
	opts := statelayer.Options{
		Store:      store,
		QuotaStore: statelayer.NewRedisQuotaStore(addr, username, password),
		SpawnStore: statelayer.NewRedisSpawnStore(addr, username, password),
		DedupStore: statelayer.NewRedisDedupStore(addr, username, password),
		// Run-control marker read (m70.8, real-kill cancel channel): the /control endpoint reads the
		// `run:{id}:control` verb the trusted BFF writes on cancel, over the SAME credentialed Valkey.
		ControlStore: statelayer.NewRedisControlStore(addr, username, password),
	}

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
		// SEC-6: the dev bypass (STATELAYER_DEV_AGENT) scopes UNAUTHENTICATED requests to a
		// static identity with no verification — it must NEVER run against a real cluster. A
		// cluster config is present here, so a set dev-agent is a production misconfiguration;
		// fail closed at startup rather than silently serving unauthenticated memory.
		if devAgent != "" {
			return fmt.Errorf("STATELAYER_DEV_AGENT (%q) is set alongside a real cluster config — "+
				"the dev bypass must never run in a cluster; unset it", devAgent)
		}
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
		// Shared-scope memory registry resolution (ADR 0052 §C6 RESOLUTION): reads the
		// registry the controller stamped on the agent identity SA. Best-effort — without
		// it, shared-scope memory falls back to the private scope (private memory is
		// unaffected). Never fail the whole proxy on its absence.
		if reg, rerr := buildRegistryResolver(restCfg); rerr != nil {
			log.Info("registry resolution disabled — shared-scope memory falls back to private", "reason", rerr.Error())
		} else {
			opts.RegistryResolver = reg
			log.Info("registry resolver ready (SA-label resolution)")
		}
	}

	// Memory auth is POD IDENTITY (ADR 0052 §C6 RESOLUTION) — the runcap verifier is
	// retired. Without a pod authenticator (no cluster config, logged above) AND without
	// the dev bypass, the proxy STARTS but refuses every memory request (401) rather than
	// CrashLooping — a fresh/memory-only install deploys cleanly.
	if opts.PodAuthenticator == nil && devAgent == "" {
		log.Info("no pod authenticator and no dev bypass — proxy refuses all memory requests until cluster auth is wired")
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

// buildRegistryResolver builds the SA-label RegistryResolver (ADR 0052 §C6 RESOLUTION)
// over a client-go clientset — it reads the registry the controller stamped on the agent
// identity SA to key SHARED-scope memory server-side.
func buildRegistryResolver(restCfg *rest.Config) (statelayer.RegistryResolver, error) {
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	return statelayer.NewSARegistryResolver(clientset), nil
}
