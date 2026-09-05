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

// Package main — the :2995 feedback ingest hook (M9, specs/eval-prompts-feedback.md §3).
//
// A launcher-local HTTP endpoint started ONLY when FEEDBACK_PORT / LANGFUSE_HOST
// are injected by the controller (the agent has been wired for feedback). It
// accepts:
//
//	POST /feedback  { "traceId": "<id>", "name": "<score-name>",
//	                  "value": <number|0..1|bool>, "comment": "<optional>" }
//	                → 202 Accepted  (score relayed to Langfuse on that trace)
//	                → 400           (missing traceId / malformed body)
//
// Langfuse is the store (ADR 0008): the hook maps {traceId,name,value,comment}
// to a Langfuse score create (POST <langfuse-host>/api/public/scores, HTTP
// basic auth public_key:secret_key). No platform-owned feedback datastore in v1;
// the full FeedbackStore CRD is phase 2.
//
// The ScoresClient interface is the mock⇄real seam: the real impl POSTs to
// Langfuse; unit tests inject a mock. Same swap-at-interface pattern as the m9.3
// prompt resolver and m9.4 scorer.
//
// Lifecycle discipline: same as the memory/:2998, AMP/:2997, and budget/:2996
// listeners — goroutine ListenAndServe, graceful Shutdown on child exit, the
// child-exit code still decides the process exit (this listener NEVER overrides
// it). nil when disabled (no LANGFUSE_HOST → unbudgeted / trace-only agents are
// unchanged).

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// defaultFeedbackPort is the localhost port the feedback listener binds when
// FEEDBACK_PORT is unset. Injected by the controller as FEEDBACK_PORT=2995.
// Reserved port per specs/agent-mesh.md; must NOT be :2996/:2997/:2998/:2999.
const defaultFeedbackPort = 2995

// feedbackMaxBody caps the request body read for a feedback POST. Feedback
// payloads are tiny (traceId + name + value + optional comment); 64KiB is a
// very generous ceiling that prevents a malicious/runaway client from exhausting
// launcher memory while reading.
const feedbackMaxBody = 64 << 10 // 64 KiB

// feedbackUpstreamTimeout bounds each Langfuse scores-API round-trip.
const feedbackUpstreamTimeout = 10 * time.Second

// feedbackConfig is the feedback-hook configuration parsed from env (M9).
type feedbackConfig struct {
	// LangfuseHost is the Langfuse base URL (LANGFUSE_HOST). The feedback hook
	// is started ONLY when this is non-empty (the agent has been wired for
	// feedback); absent → the listener is skipped entirely.
	LangfuseHost string
	// PublicKey / SecretKey are the Langfuse API credentials for HTTP basic
	// auth to the scores endpoint (LANGFUSE_SCORES_PUBLIC_KEY /
	// LANGFUSE_SCORES_SECRET_KEY). Deterministic DEV-ONLY fixed values injected
	// by the controller as STATIC env — NEVER valueFrom (the Knative ksvc
	// webhook landmine, M5.7). They match the dev Langfuse seeded by
	// `make -C harness dev-up M=3`.
	PublicKey string
	SecretKey string
	// Port is the localhost port the listener binds (FEEDBACK_PORT, default 2995).
	Port int
}

// loadFeedbackConfig parses the feedback-hook configuration from env.
//
// Environment variables:
//
//	LANGFUSE_HOST (gate): Langfuse base URL. Empty ⇒ the listener is NOT
//	  started; every other feedback env is then irrelevant.
//	FEEDBACK_PORT (optional): listener port (default 2995).
//	LANGFUSE_SCORES_PUBLIC_KEY: Langfuse public key for basic auth.
//	LANGFUSE_SCORES_SECRET_KEY: Langfuse secret key for basic auth.
//
// Like the memory/AMP/gateway loaders, it does NOT hard-fail on missing creds
// when the gate is set — a misconfigured credential degrades to an auth failure
// on the first Langfuse POST (a visible non-fatal error) rather than crashing
// the launcher.
func loadFeedbackConfig(lookup func(string) string) (feedbackConfig, error) {
	host := strings.TrimRight(lookup("LANGFUSE_HOST"), "/")
	if host == "" {
		return feedbackConfig{}, nil
	}

	port, err := parsePort(lookup("FEEDBACK_PORT"), defaultFeedbackPort)
	if err != nil {
		return feedbackConfig{}, fmt.Errorf("FEEDBACK_PORT: %w", err)
	}

	return feedbackConfig{
		LangfuseHost: host,
		PublicKey:    lookup("LANGFUSE_SCORES_PUBLIC_KEY"),
		SecretKey:    lookup("LANGFUSE_SCORES_SECRET_KEY"),
		Port:         port,
	}, nil
}

// FeedbackEnabled reports whether the :2995 feedback listener should be started —
// true iff a Langfuse host was injected.
func (c Config) FeedbackEnabled() bool {
	return c.Feedback.LangfuseHost != ""
}

// feedbackRequest is the JSON body accepted by POST /feedback.
type feedbackRequest struct {
	// TraceID is the Langfuse trace identifier — required; 400 on absent/empty.
	TraceID string `json:"traceId"`
	// Name is the score name (e.g. "thumbs-up", "accuracy").
	Name string `json:"name"`
	// Value is the raw score value (number, 0..1, or boolean coerced to 0/1).
	// json.Number is used so we can pass it through to Langfuse as-is without
	// a float64 precision round-trip.
	Value json.Number `json:"value"`
	// Comment is an optional human-readable note attached to the score.
	Comment string `json:"comment,omitempty"`
}

// langfuseScoreBody is the body of the POST /api/public/scores Langfuse call.
// Mirrors the Langfuse scores API (POST /api/public/scores):
// https://langfuse.com/docs/scores/custom — traceId, name, value, optional comment.
type langfuseScoreBody struct {
	TraceID string      `json:"traceId"`
	Name    string      `json:"name"`
	Value   json.Number `json:"value"`
	Comment string      `json:"comment,omitempty"`
}

// ScoresClient is the mock⇄real seam for posting a score to Langfuse. The real
// implementation (langfuseScoresClient) POSTs to the Langfuse scores API; unit
// tests inject a mockScoresClient. Same interface-swap pattern as the m9.3
// Resolver and m9.4 Scorer.
type ScoresClient interface {
	// CreateScore posts {traceId, name, value, comment} as a score on that
	// trace in Langfuse. Returns an error when the upstream is unreachable or
	// returns a non-2xx response — the error includes the HTTP status so the
	// caller can surface a 502/504 to the feedback poster.
	CreateScore(traceID, name string, value json.Number, comment string) error
}

// langfuseScoresClient is the real ScoresClient that POSTs to the Langfuse
// scores API with HTTP basic auth.
type langfuseScoresClient struct {
	baseURL   string // e.g. "http://langfuse-web.langfuse.svc:3000"
	publicKey string
	secretKey string
	client    *http.Client
}

func newLangfuseScoresClient(cfg feedbackConfig) *langfuseScoresClient {
	return &langfuseScoresClient{
		baseURL:   cfg.LangfuseHost,
		publicKey: cfg.PublicKey,
		secretKey: cfg.SecretKey,
		client:    &http.Client{Timeout: feedbackUpstreamTimeout, CheckRedirect: refuseRedirect},
	}
}

// CreateScore implements ScoresClient by POSTing to
// <baseURL>/api/public/scores with HTTP basic auth.
func (c *langfuseScoresClient) CreateScore(traceID, name string, value json.Number, comment string) error {
	body := langfuseScoreBody{
		TraceID: traceID,
		Name:    name,
		Value:   value,
		Comment: comment,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("feedback: marshal score body: %w", err)
	}

	url := c.baseURL + "/api/public/scores"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("feedback: build Langfuse request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.publicKey, c.secretKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("feedback: Langfuse scores POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body) // drain so connection can be reused

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("feedback: Langfuse scores POST returned %d", resp.StatusCode)
	}
	return nil
}

// feedbackServer is the HTTP server for the :2995 feedback endpoint.
type feedbackServer struct {
	scores ScoresClient
	logf   func(string, ...any)
}

func newFeedbackServer(cfg feedbackConfig, logf func(string, ...any)) *feedbackServer {
	return &feedbackServer{
		scores: newLangfuseScoresClient(cfg),
		logf:   logf,
	}
}

// newFeedbackServerWithClient builds a feedbackServer with an explicit
// ScoresClient — used in unit tests to inject a mock.
func newFeedbackServerWithClient(scores ScoresClient, logf func(string, ...any)) *feedbackServer {
	return &feedbackServer{scores: scores, logf: logf}
}

// handler returns the HTTP handler for the :2995 feedback listener.
func (s *feedbackServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/feedback", s.handleFeedback)
	return mux
}

// buildFeedbackServer constructs the :2995 http.Server when feedback is enabled
// (LANGFUSE_HOST was injected), or returns nil when it is not. Factored out of
// main() to keep its cyclomatic complexity within the project lint limit.
func buildFeedbackServer(cfg Config) *http.Server {
	if !cfg.FeedbackEnabled() {
		return nil
	}
	logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	return &http.Server{
		Addr:    loopbackAddr(cfg.Feedback.Port),
		Handler: newFeedbackServer(cfg.Feedback, logf).handler(),
	}
}

// handleFeedback handles POST /feedback.
func (s *feedbackServer) handleFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, feedbackMaxBody))
	if err != nil {
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}

	var req feedbackRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("malformed JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Missing traceId is a 400 — no silent drop (spec §3: "missing traceId → 400").
	if strings.TrimSpace(req.TraceID) == "" {
		http.Error(w, "traceId is required", http.StatusBadRequest)
		return
	}

	if err := s.scores.CreateScore(req.TraceID, req.Name, req.Value, req.Comment); err != nil {
		s.logf("launcher: feedback: Langfuse relay error: %v", err)
		// Surface as 502 (upstream/Langfuse error) — the caller can retry.
		http.Error(w, fmt.Sprintf("feedback relay error: %v", err), http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusAccepted) // 202 Accepted
}
