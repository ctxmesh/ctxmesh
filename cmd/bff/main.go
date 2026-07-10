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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
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

	srv := bff.NewServer(bff.Options{
		CallerClients: callerClients,
		Scheme:        scheme,
		Auth:          bff.BearerAuthenticator{},
		Adapters:      adapters,
		Version:       version,
		StaticDir:     staticDir,
		Log:           ctrl.Log.WithName("bff.server"),
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
