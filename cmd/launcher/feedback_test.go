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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockScoresClient is a test-double ScoresClient that records calls and can be
// configured to return an error (simulating a Langfuse scores API failure).
type mockScoresClient struct {
	calls []mockScoreCall
	err   error
}

type mockScoreCall struct {
	traceID string
	name    string
	value   json.Number
	comment string
}

func (m *mockScoresClient) CreateScore(traceID, name string, value json.Number, comment string) error {
	m.calls = append(m.calls, mockScoreCall{traceID: traceID, name: name, value: value, comment: comment})
	return m.err
}

// newFeedbackTestServer builds a feedbackServer with a fresh mockScoresClient
// and returns both.
func newFeedbackTestServer() (*feedbackServer, *mockScoresClient) {
	mock := &mockScoresClient{}
	logf := func(string, ...any) {}
	srv := newFeedbackServerWithClient(mock, logf)
	return srv, mock
}

// TestFeedbackHandler_202OnValidRequest tests the happy path: a valid POST with
// traceId, name, and value returns 202 and relays exactly that call to the
// ScoresClient.
func TestFeedbackHandler_202OnValidRequest(t *testing.T) {
	srv, mock := newFeedbackTestServer()
	handler := srv.handler()

	body := `{"traceId":"trace-abc","name":"thumbs-up","value":1}`
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 call to ScoresClient, got %d", len(mock.calls))
	}
	call := mock.calls[0]
	if call.traceID != "trace-abc" {
		t.Errorf("traceID: got %q, want %q", call.traceID, "trace-abc")
	}
	if call.name != "thumbs-up" {
		t.Errorf("name: got %q, want %q", call.name, "thumbs-up")
	}
	if call.value != json.Number("1") {
		t.Errorf("value: got %q, want %q", call.value, "1")
	}
}

// TestFeedbackHandler_202WithComment verifies that the optional comment field
// is passed through to the ScoresClient.
func TestFeedbackHandler_202WithComment(t *testing.T) {
	srv, mock := newFeedbackTestServer()
	handler := srv.handler()

	body := `{"traceId":"t-1","name":"accuracy","value":0.85,"comment":"looks great"}`
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 call to ScoresClient, got %d", len(mock.calls))
	}
	if mock.calls[0].comment != "looks great" {
		t.Errorf("comment: got %q, want %q", mock.calls[0].comment, "looks great")
	}
}

// TestFeedbackHandler_400OnMissingTraceID verifies that a missing traceId
// returns 400 with no call to the ScoresClient — not a silent drop.
func TestFeedbackHandler_400OnMissingTraceID(t *testing.T) {
	srv, mock := newFeedbackTestServer()
	handler := srv.handler()

	body := `{"name":"score","value":0.5}`
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(mock.calls) != 0 {
		t.Errorf("expected no ScoresClient call on missing traceId, got %d", len(mock.calls))
	}
}

// TestFeedbackHandler_400OnEmptyTraceID verifies that an explicit empty-string
// traceId is treated as absent (400).
func TestFeedbackHandler_400OnEmptyTraceID(t *testing.T) {
	srv, mock := newFeedbackTestServer()
	handler := srv.handler()

	body := `{"traceId":"","name":"score","value":0.5}`
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(mock.calls) != 0 {
		t.Errorf("expected no ScoresClient call on empty traceId, got %d", len(mock.calls))
	}
}

// TestFeedbackHandler_400OnMalformedBody verifies that malformed JSON returns
// 400.
func TestFeedbackHandler_400OnMalformedBody(t *testing.T) {
	srv, mock := newFeedbackTestServer()
	handler := srv.handler()

	body := `not-valid-json`
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(mock.calls) != 0 {
		t.Errorf("expected no ScoresClient call on malformed body, got %d", len(mock.calls))
	}
}

// TestFeedbackHandler_502OnLangfuseError verifies that a ScoresClient error
// (e.g. Langfuse unreachable) returns 502 — the error is surfaced to the
// caller so it can retry, not dropped silently.
func TestFeedbackHandler_502OnLangfuseError(t *testing.T) {
	mock := &mockScoresClient{err: errors.New("connection refused")}
	logf := func(string, ...any) {}
	srv := newFeedbackServerWithClient(mock, logf)
	handler := srv.handler()

	body := `{"traceId":"t-fail","name":"score","value":1}`
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 on Langfuse error, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestFeedbackHandler_TraceIDToScoresClientMapping verifies the exact mapping:
// the traceId from the request body is passed as the traceID to CreateScore
// so the score lands on the correct Langfuse trace (spec §3 "correlation").
func TestFeedbackHandler_TraceIDToScoresClientMapping(t *testing.T) {
	srv, mock := newFeedbackTestServer()
	handler := srv.handler()

	const wantTraceID = "lf-trace-42"
	body := `{"traceId":"lf-trace-42","name":"relevance","value":0.9}`
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rr.Code)
	}
	if len(mock.calls) == 0 {
		t.Fatal("no calls to ScoresClient")
	}
	if mock.calls[0].traceID != wantTraceID {
		t.Errorf("traceID mismatch: got %q, want %q", mock.calls[0].traceID, wantTraceID)
	}
}

// TestLoadFeedbackConfig_DisabledWhenNoHost verifies the gate: when
// LANGFUSE_HOST is absent the config is empty and FeedbackEnabled returns false.
func TestLoadFeedbackConfig_DisabledWhenNoHost(t *testing.T) {
	cfg, err := loadFeedbackConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadFeedbackConfig unexpected error: %v", err)
	}
	c := Config{Feedback: cfg}
	if c.FeedbackEnabled() {
		t.Error("FeedbackEnabled must be false when LANGFUSE_HOST is absent")
	}
}

// TestLoadFeedbackConfig_EnabledWithHost verifies that LANGFUSE_HOST being set
// enables the listener and that FEEDBACK_PORT defaults to 2995.
func TestLoadFeedbackConfig_EnabledWithHost(t *testing.T) {
	env := map[string]string{
		"LANGFUSE_HOST":              "http://langfuse-web.langfuse.svc:3000",
		"LANGFUSE_SCORES_PUBLIC_KEY": "pk-lf-dev-00000000000000000000000000000000",
		"LANGFUSE_SCORES_SECRET_KEY": "sk-lf-dev-00000000000000000000000000000000",
	}
	lookup := func(k string) string { return env[k] }

	cfg, err := loadFeedbackConfig(lookup)
	if err != nil {
		t.Fatalf("loadFeedbackConfig unexpected error: %v", err)
	}
	c := Config{Feedback: cfg}
	if !c.FeedbackEnabled() {
		t.Error("FeedbackEnabled must be true when LANGFUSE_HOST is set")
	}
	if cfg.Port != defaultFeedbackPort {
		t.Errorf("default port: got %d, want %d", cfg.Port, defaultFeedbackPort)
	}
	if cfg.PublicKey != "pk-lf-dev-00000000000000000000000000000000" {
		t.Errorf("PublicKey: got %q", cfg.PublicKey)
	}
	// Trailing slash is stripped.
	if strings.HasSuffix(cfg.LangfuseHost, "/") {
		t.Errorf("LangfuseHost must not have trailing slash, got %q", cfg.LangfuseHost)
	}
}
