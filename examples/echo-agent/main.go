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

// echo-agent is a reference agent implementation that satisfies the M2 runtime
// contract (specs/agent-serving.md §"Runtime contract subset"):
//
//   - POST /invoke   — when MODEL_ROUTE is set, calls $MODEL_GATEWAY_URL/chat/completions
//     (OpenAI-style) and returns {"agent":"echo-agent","completion":<text>,"route":<alias>};
//     without MODEL_ROUTE, echoes the request body unchanged (M1 behaviour).
//   - GET  /healthz  — liveness probe; returns 200 ok
//   - GET  /readyz   — readiness probe; returns 200 ok
//
// The server listens on $AGENT_PORT (default 8080) and shuts down gracefully
// on SIGTERM. It is designed to run behind the launcher (cmd/launcher) which
// exec's it as PID 1's replacement process.
package main

import (
	"bytes"
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

// agentName is the value returned in every /invoke response's "agent" field.
const agentName = "echo-agent"

// invokeResponse is the JSON payload returned by POST /invoke when MODEL_ROUTE is set.
type invokeResponse struct {
	Agent      string `json:"agent"`
	Completion string `json:"completion,omitempty"`
	Route      string `json:"route,omitempty"`
	Echo       string `json:"echo,omitempty"`
}

// chatRequest is the OpenAI-compatible request body sent to the model gateway.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

// chatMessage is a single message in an OpenAI-style conversation.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the OpenAI-compatible response from the model gateway.
// Only the fields needed to extract the completion text are parsed.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// gatewayClient is a package-level HTTP client for gateway calls. It is a
// variable so tests can substitute a custom transport backed by httptest.Server.
var gatewayClient = &http.Client{Timeout: 30 * time.Second}

// handleInvoke handles POST /invoke.
//   - With MODEL_ROUTE set: POSTs to $MODEL_GATEWAY_URL/chat/completions and
//     returns {"agent","completion","route"}.
//   - Without MODEL_ROUTE: echoes the request body as {"agent","echo"} (M1 behaviour).
//
// Non-POST methods are rejected with 405.
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

	route := os.Getenv("MODEL_ROUTE")
	if route == "" {
		// M1 echo path — MODEL_ROUTE not configured.
		resp := invokeResponse{
			Agent: agentName,
			Echo:  string(body),
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("echo-agent: encode response: %v", err)
		}
		return
	}

	// Gateway path — call the model gateway.
	completion, err := callGateway(r.Context(), route, string(body))
	if err != nil {
		http.Error(w, fmt.Sprintf("gateway error: %v", err), http.StatusBadGateway)
		return
	}

	resp := invokeResponse{
		Agent:      agentName,
		Completion: completion,
		Route:      route,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("echo-agent: encode response: %v", err)
	}
}

// callGateway POSTs an OpenAI-compatible chat/completions request to the model
// gateway and returns the content of the first choice message.
// The Authorization header carries a dummy bearer token; LiteLLM in dev mode
// accepts any value.
func callGateway(ctx context.Context, route, userContent string) (string, error) {
	gatewayURL := os.Getenv("MODEL_GATEWAY_URL")
	if gatewayURL == "" {
		return "", fmt.Errorf("MODEL_GATEWAY_URL not set")
	}

	reqBody := chatRequest{
		Model: route,
		Messages: []chatMessage{
			{Role: "user", Content: userContent},
		},
	}
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshalling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gatewayURL+"/chat/completions", bytes.NewReader(reqBytes))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Dummy bearer token — LiteLLM in dev mode accepts any value (no auth configured).
	httpReq.Header.Set("Authorization", "Bearer dummy")

	resp, err := gatewayClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("gateway request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading gateway response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gateway returned %d: %s", resp.StatusCode, string(respBytes))
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBytes, &chatResp); err != nil {
		return "", fmt.Errorf("parsing gateway response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("gateway returned no choices")
	}
	return chatResp.Choices[0].Message.Content, nil
}

// handleHealth handles GET /healthz and GET /readyz. Both return 200 ok;
// splitting them into separate probes allows independent liveness vs readiness
// logic in future milestones without changing the URL contract.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	if _, err := fmt.Fprintln(w, "ok"); err != nil {
		log.Printf("echo-agent: health response write error: %v", err)
	}
}
