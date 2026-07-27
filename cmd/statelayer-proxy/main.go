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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ctxmesh/agent-engine/internal/runcap"
	"github.com/ctxmesh/agent-engine/internal/statelayer"
)

// defaultAudience is the DISTINCT audience the proxy verifies (ADR 0050 §2 — a
// token minted for the credential plane is not replayable here). The BFF mints a
// state-layer token with this audience alongside the credential-plane one.
const defaultAudience = "statelayer-proxy"

const (
	defaultListenAddr   = ":8080"
	readHeaderTimeout   = 10 * time.Second
	shutdownGracePeriod = 10 * time.Second
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
			audience = defaultAudience
		}
		opts.Verifier = runcap.NewVerifier(pub, audience, nil)
	case devAgent == "":
		return errors.New("MCP_CAPABILITY_PUBLIC_KEY is required (unless STATELAYER_DEV_AGENT enables the dev bypass)")
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
