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

// echo-agent is a reference agent implementation that satisfies the M1 runtime
// contract (specs/agent-serving.md §"Runtime contract subset (M1)"):
//
//   - POST /invoke   — echoes the request body as a JSON response (no LLM call until M2)
//   - GET  /healthz  — liveness probe; returns 200 ok
//   - GET  /readyz   — readiness probe; returns 200 ok
//
// The server listens on $AGENT_PORT (default 8080) and shuts down gracefully
// on SIGTERM. It is designed to run behind the launcher (cmd/launcher) which
// exec's it as PID 1's replacement process.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	port := os.Getenv("AGENT_PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port

	mux := http.NewServeMux()
	mux.HandleFunc("/invoke", handleInvoke)
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/readyz", handleHealth)

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown: wait for SIGTERM (Kubernetes) or SIGINT (local dev).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	go func() {
		log.Printf("echo-agent: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("echo-agent: serve error: %v", err)
		}
	}()

	// Block until a shutdown signal arrives.
	<-ctx.Done()
	log.Println("echo-agent: shutdown signal received")

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("echo-agent: graceful shutdown error: %v", err)
	}
	log.Println("echo-agent: stopped")
}

// invokeResponse is the JSON payload returned by POST /invoke.
type invokeResponse struct {
	Agent string `json:"agent"`
	Echo  string `json:"echo"`
	Model string `json:"model"`
}

// handleInvoke handles POST /invoke. It reads the request body and echoes it
// back as a JSON object. Non-POST methods are rejected with 405.
func handleInvoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed — use POST", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusInternalServerError)
		return
	}
	defer func() { _ = r.Body.Close() }()

	resp := invokeResponse{
		Agent: "echo-agent",
		Echo:  string(body),
		Model: "none-until-M2",
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("echo-agent: encode response: %v", err)
	}
}

// handleHealth handles GET /healthz and GET /readyz. Both return 200 ok;
// splitting them into separate probes allows independent liveness vs readiness
// logic in future milestones without changing the URL contract.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	if _, err := fmt.Fprintln(w, "ok"); err != nil {
		log.Printf("echo-agent: health response write error: %v", err)
	}
}
