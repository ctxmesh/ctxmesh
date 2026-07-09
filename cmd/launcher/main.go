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

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/otel"
)

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher: %v\n", err)
		os.Exit(1)
	}

	if err := validateEntrypoint(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "launcher: %v\n", err)
		os.Exit(1)
	}

	// ── OTel setup (best-effort, non-blocking) ────────────────────────────
	// If setup fails, the global provider remains no-op and requests still
	// succeed — tracing is best-effort per spec §8.2 / observability.md.
	ctx := context.Background()
	otelShutdown, otelErr := setupOTel(ctx, cfg.OTLPEndpoint)
	if otelErr != nil {
		fmt.Fprintf(os.Stderr, "launcher: otel init warning (tracing disabled): %v\n", otelErr)
		otelShutdown = func(context.Context) error { return nil }
	}

	// ── Reverse proxy ─────────────────────────────────────────────────────
	upstreamURL := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("127.0.0.1:%d", cfg.UpstreamPort),
	}
	// Use the global tracer/propagator set by setupOTel (or the noop
	// defaults if OTel init failed).
	tracer := otel.Tracer(tracerName)
	prop := otel.GetTextMapPropagator()
	handler := buildHandler(tracer, prop, upstreamURL, cfg)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.ProxyPort),
		Handler: handler,
	}

	// ── Memory endpoint (:2998) ───────────────────────────────────────────
	// Started ONLY when a backend was injected (the agent has a MemoryBinding).
	// It runs as a second listener beside the proxy with the SAME lifecycle
	// discipline (goroutine ListenAndServe; graceful Shutdown on child exit;
	// the child-exit still decides the process exit code — the memory listener
	// never overrides it). nil when disabled.
	var memSrv *http.Server
	if cfg.MemoryEnabled() {
		memSrv = &http.Server{
			Addr:    fmt.Sprintf(":%d", cfg.Memory.Port),
			Handler: newMemoryServer(newRedisStore(cfg.Memory.BackendAddr), cfg.Memory, tracer).handler(),
		}
	}

	// ── Child process ─────────────────────────────────────────────────────
	child, err := startChild(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher: %v\n", err)
		os.Exit(1)
	}

	mainPID := child.Process.Pid

	// Forward SIGTERM / SIGINT to the child.
	go forwardSignals(child)

	// PID-1 reaping loop: Wait4(-1) reaps the direct child and any orphaned
	// grandchildren reparented to us. When mainPID exits, its code is sent
	// here. cmd.Wait() is intentionally NOT called (reapAll owns all waitpid
	// calls to avoid a race between two waitpid callers on the same PID).
	childExitCh := make(chan int, 1)
	go func() {
		childExitCh <- reapAll(mainPID)
	}()

	// Start proxy (non-fatal if the port is briefly unavailable at startup —
	// Knative will retry the health probe).
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "launcher: proxy: %v\n", err)
		}
	}()

	// Start the memory listener (best-effort; a bind failure here must not take
	// the agent down — the agent's memory path is already best-effort).
	if memSrv != nil {
		go func() {
			if err := memSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "launcher: memory: %v\n", err)
			}
		}()
	}

	// ── Wait for child to exit, then shut down cleanly ────────────────────
	exitCode := <-childExitCh

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = srv.Shutdown(shutCtx)
	if memSrv != nil {
		_ = memSrv.Shutdown(shutCtx)
	}
	_ = otelShutdown(shutCtx)

	os.Exit(exitCode)
}
