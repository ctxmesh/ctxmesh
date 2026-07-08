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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	defaultPort          = "2999"
	defaultToolsJSONPath = "/etc/agent/tools.json"
	shutdownTimeout      = 10 * time.Second
)

// readFile is a package-level variable so tests can replace it without
// touching the real filesystem.
var readFile = os.ReadFile

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	port := os.Getenv("DISCOVERY_PORT")
	if port == "" {
		port = defaultPort
	}

	toolsJSONPath := os.Getenv("TOOLS_JSON_PATH")
	if toolsJSONPath == "" {
		toolsJSONPath = defaultToolsJSONPath
	}

	srv := newServer(logger)
	srv.loadInitialManifest(toolsJSONPath)

	httpSrv := &http.Server{
		Addr:    fmt.Sprintf(":%s", port),
		Handler: srv.handler(),
	}

	// Graceful shutdown on SIGTERM (or SIGINT in dev).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		logger.Info("discovery sidecar listening", "port", port)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server error", "err", err)
			os.Exit(1)
		}
	}()

	<-sigCh
	logger.Info("discovery sidecar shutting down")

	// Close all SSE subscriber channels so their goroutines exit cleanly.
	srv.closeSubscribers()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		logger.Error("HTTP server shutdown error", "err", err)
	}
}
