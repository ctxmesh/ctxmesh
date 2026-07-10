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
	// Build the client-go-backed read client, reusing the controllers' scheme so
	// the agent CRDs are decodable. Credentials come from in-cluster config or
	// the local kubeconfig — server-side only.
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	srv := bff.NewServer(bff.Options{
		Reader:    k8s,
		Auth:      bff.BearerAuthenticator{},
		Version:   version,
		StaticDir: staticDir,
		Log:       ctrl.Log.WithName("bff.server"),
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
