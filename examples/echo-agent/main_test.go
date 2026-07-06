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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleInvoke_Echo verifies the M1 echo path (no MODEL_ROUTE set):
// POST /invoke returns {"agent":"echo-agent","echo":<body>}.
func TestHandleInvoke_Echo(t *testing.T) {
	t.Setenv("MODEL_ROUTE", "")

	body := "hello gateway"
	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handleInvoke(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp invokeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Agent != agentName {
		t.Errorf("agent = %q, want %q", resp.Agent, agentName)
	}
	if resp.Echo != body {
		t.Errorf("echo = %q, want %q", resp.Echo, body)
	}
	if resp.Completion != "" {
		t.Errorf("completion should be empty in echo path, got %q", resp.Completion)
	}
	if resp.Route != "" {
		t.Errorf("route should be empty in echo path, got %q", resp.Route)
	}
}

// TestHandleInvoke_MethodNotAllowed verifies that non-POST requests to /invoke
// return 405.
func TestHandleInvoke_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/invoke", nil)
	rec := httptest.NewRecorder()
	handleInvoke(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

// TestHandleInvoke_GatewayPath verifies the M2 gateway path: when MODEL_ROUTE
// is set and a fake gateway returns a valid OpenAI-style response, /invoke
// returns {"agent","completion","route"}.
func TestHandleInvoke_GatewayPath(t *testing.T) {
	const route = "mock-model"
	const completionText = "MOCK_OK response from fake gateway"

	// Fake gateway: asserts request shape and returns a minimal chat response.
	fakeGateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %q, want /chat/completions", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %q", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if auth := r.Header.Get("Authorization"); auth == "" {
			t.Error("Authorization header missing")
		}

		var reqBody chatRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if reqBody.Model != route {
			t.Errorf("model = %q, want %q", reqBody.Model, route)
		}
		if len(reqBody.Messages) != 1 || reqBody.Messages[0].Role != "user" {
			t.Errorf("unexpected messages: %+v", reqBody.Messages)
		}

		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": completionText}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer fakeGateway.Close()

	// Wire the env and swap the HTTP client.
	t.Setenv("MODEL_ROUTE", route)
	t.Setenv("MODEL_GATEWAY_URL", fakeGateway.URL)

	// Use the server's client so the test transport is shared.
	orig := gatewayClient
	gatewayClient = fakeGateway.Client()
	defer func() { gatewayClient = orig }()

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader("some prompt"))
	rec := httptest.NewRecorder()
	handleInvoke(rec, req)

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("expected 200, got %d: %s", rec.Code, body)
	}

	var resp invokeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Agent != agentName {
		t.Errorf("agent = %q, want %q", resp.Agent, agentName)
	}
	if resp.Completion != completionText {
		t.Errorf("completion = %q, want %q", resp.Completion, completionText)
	}
	if resp.Route != route {
		t.Errorf("route = %q, want %q", resp.Route, route)
	}
	if resp.Echo != "" {
		t.Errorf("echo should be empty in gateway path, got %q", resp.Echo)
	}
}

// TestHandleInvoke_GatewayError verifies that a gateway error (non-200) is
// surfaced as a 502 response with the upstream error text.
func TestHandleInvoke_GatewayError(t *testing.T) {
	const route = "broken-model"

	// Fake gateway that always returns 503.
	fakeGateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream overloaded", http.StatusServiceUnavailable)
	}))
	defer fakeGateway.Close()

	t.Setenv("MODEL_ROUTE", route)
	t.Setenv("MODEL_GATEWAY_URL", fakeGateway.URL)

	orig := gatewayClient
	gatewayClient = fakeGateway.Client()
	defer func() { gatewayClient = orig }()

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader("prompt"))
	rec := httptest.NewRecorder()
	handleInvoke(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}
}

// TestHandleInvoke_GatewayURLMissing verifies that a missing MODEL_GATEWAY_URL
// (with MODEL_ROUTE set) results in a 502.
func TestHandleInvoke_GatewayURLMissing(t *testing.T) {
	t.Setenv("MODEL_ROUTE", "some-route")
	t.Setenv("MODEL_GATEWAY_URL", "")

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader("prompt"))
	rec := httptest.NewRecorder()
	handleInvoke(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}
}

// TestHandleHealth verifies that /healthz returns 200.
func TestHandleHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handleHealth(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}
