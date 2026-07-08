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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

// silentLogger returns a logger that discards all output (keeps test output clean).
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// withCancelContext returns a cancellable context and a cancel function.
// It attaches the context to the provided request via r.WithContext so the
// caller can cancel the SSE handler.
func withCancelContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithCancel(r.Context())
}

// postManifest sends a POST /control request with the given manifest and returns the response.
func postManifest(t *testing.T, handler http.Handler, m toolmanifest.Manifest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/control", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// getTools sends a GET /tools request and returns the decoded manifest.
func getTools(t *testing.T, handler http.Handler) toolmanifest.Manifest {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/tools", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /tools: status = %d, want 200", rr.Code)
	}
	var m toolmanifest.Manifest
	if err := json.NewDecoder(rr.Body).Decode(&m); err != nil {
		t.Fatalf("GET /tools: decode: %v", err)
	}
	return m
}

// ── /healthz ──────────────────────────────────────────────────────────────────

func TestHandleHealthz(t *testing.T) {
	t.Parallel()

	s := newServer(silentLogger())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("GET /healthz: status = %d, want 200", rr.Code)
	}
	if body := strings.TrimSpace(rr.Body.String()); body != "ok" {
		t.Errorf("GET /healthz: body = %q, want %q", body, "ok")
	}
}

// ── /tools ────────────────────────────────────────────────────────────────────

func TestHandleTools_EmptyInitial(t *testing.T) {
	t.Parallel()

	s := newServer(silentLogger())
	m := getTools(t, s.handler())

	if len(m.Tools) != 0 {
		t.Errorf("initial /tools: len(Tools) = %d, want 0", len(m.Tools))
	}
	if len(m.Version) != 8 {
		t.Errorf("initial /tools: version length = %d, want 8; got %q", len(m.Version), m.Version)
	}
}

func TestHandleTools_ReturnsCurrentManifest(t *testing.T) {
	t.Parallel()

	s := newServer(silentLogger())
	h := s.handler()

	want := toolmanifest.Manifest{Tools: []toolmanifest.Tool{
		{Name: "word-count", Mode: "remote", Endpoint: "http://wc.svc", Transport: "streamable-http"},
	}}

	rr := postManifest(t, h, want)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("POST /control: status = %d, want 204", rr.Code)
	}

	got := getTools(t, h)
	if got.Version == "" {
		t.Error("GET /tools after swap: version is empty")
	}
	if len(got.Tools) != 1 {
		t.Fatalf("GET /tools after swap: len(Tools) = %d, want 1", len(got.Tools))
	}
	if got.Tools[0].Name != "word-count" {
		t.Errorf("GET /tools after swap: Tools[0].Name = %q, want %q", got.Tools[0].Name, "word-count")
	}
}

// ── /control ─────────────────────────────────────────────────────────────────

func TestHandleControl_SwapReflectedInTools(t *testing.T) {
	t.Parallel()

	s := newServer(silentLogger())
	h := s.handler()

	m1 := toolmanifest.Manifest{Tools: []toolmanifest.Tool{
		{Name: "alpha", Mode: "remote", Endpoint: "http://alpha.svc", Transport: "streamable-http"},
	}}
	m2 := toolmanifest.Manifest{Tools: []toolmanifest.Tool{
		{Name: "beta", Mode: "remote", Endpoint: "http://beta.svc", Transport: "streamable-http"},
		{Name: "gamma", Mode: "sidecar", Endpoint: "http://localhost:3001", Transport: "streamable-http"},
	}}

	// Push m1.
	rr := postManifest(t, h, m1)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("POST /control (m1): status = %d, want 204", rr.Code)
	}
	got1 := getTools(t, h)
	if len(got1.Tools) != 1 || got1.Tools[0].Name != "alpha" {
		t.Errorf("after m1 swap: got %+v, want single tool 'alpha'", got1.Tools)
	}

	// Push m2.
	rr = postManifest(t, h, m2)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("POST /control (m2): status = %d, want 204", rr.Code)
	}
	got2 := getTools(t, h)
	if len(got2.Tools) != 2 {
		t.Fatalf("after m2 swap: len(Tools) = %d, want 2", len(got2.Tools))
	}
	// Tools must be sorted by name.
	if got2.Tools[0].Name != "beta" || got2.Tools[1].Name != "gamma" {
		t.Errorf("after m2 swap: tools = %v, want [beta gamma]", got2.Tools)
	}
	// Versions must differ.
	if got1.Version == got2.Version {
		t.Errorf("version unchanged after content change: %q", got1.Version)
	}
}

func TestHandleControl_VersionRecomputedServerSide(t *testing.T) {
	t.Parallel()

	s := newServer(silentLogger())
	h := s.handler()

	// Send a manifest with a fake client-supplied version.
	m := toolmanifest.Manifest{
		Version: "fakefake",
		Tools: []toolmanifest.Tool{
			{Name: "tool-x", Mode: "remote", Endpoint: "http://x.svc", Transport: "streamable-http"},
		},
	}
	rr := postManifest(t, h, m)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("POST /control: status = %d, want 204", rr.Code)
	}

	got := getTools(t, h)
	if got.Version == "fakefake" {
		t.Error("server accepted client-supplied version instead of recomputing it")
	}
	if len(got.Version) != 8 {
		t.Errorf("version length = %d, want 8; got %q", len(got.Version), got.Version)
	}
}

func TestHandleControl_RejectsOversizedBody(t *testing.T) {
	t.Parallel()

	s := newServer(silentLogger())
	h := s.handler()

	// Build a body larger than 1 MiB.
	large := bytes.Repeat([]byte("x"), maxControlBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/control", bytes.NewReader(large))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: status = %d, want 413", rr.Code)
	}
}

func TestHandleControl_RejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	s := newServer(silentLogger())
	h := s.handler()

	req := httptest.NewRequest(http.MethodPost, "/control", strings.NewReader("{not json}"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: status = %d, want 400", rr.Code)
	}
}

// ── /events SSE ───────────────────────────────────────────────────────────────

// fakeResponseRecorder wraps httptest.ResponseRecorder and adds Flush support.
type fakeResponseRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func newFakeResponseRecorder() *fakeResponseRecorder {
	return &fakeResponseRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		flushed:          make(chan struct{}, 64),
	}
}

func (f *fakeResponseRecorder) Flush() {
	select {
	case f.flushed <- struct{}{}:
	default:
	}
}

func TestHandleEvents_ReceivesVersionOnSwap(t *testing.T) {
	t.Parallel()

	s := newServer(silentLogger())
	h := s.handler()

	// Start the SSE subscriber.
	sseRecorder := newFakeResponseRecorder()
	sseReq := httptest.NewRequest(http.MethodGet, "/events", nil)
	sseCtx, cancelSSE := withCancelContext(sseReq)
	defer cancelSSE()
	sseReq = sseReq.WithContext(sseCtx)

	sseDone := make(chan struct{})
	go func() {
		defer close(sseDone)
		h.ServeHTTP(sseRecorder, sseReq)
	}()

	// Wait for the handler to register the subscriber and flush headers.
	select {
	case <-sseRecorder.flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not flush headers within 2s")
	}

	// Push a manifest via /control.
	m := toolmanifest.Manifest{Tools: []toolmanifest.Tool{
		{Name: "echo", Mode: "remote", Endpoint: "http://echo.svc", Transport: "streamable-http"},
	}}
	rr := postManifest(t, h, m)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("POST /control: status = %d, want 204", rr.Code)
	}

	// Wait for the event to be flushed to the SSE stream.
	select {
	case <-sseRecorder.flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE event not flushed within 2s after /control")
	}

	// Disconnect the subscriber.
	cancelSSE()
	select {
	case <-sseDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler goroutine did not exit within 2s after cancel")
	}

	// Parse the SSE output.
	body := sseRecorder.Body.String()
	scanner := bufio.NewScanner(strings.NewReader(body))
	foundEvent := false
	for scanner.Scan() {
		line := scanner.Text()
		if version, ok := strings.CutPrefix(line, "data: "); ok {
			if len(version) == 8 {
				foundEvent = true
			} else {
				t.Errorf("SSE data line has unexpected version %q (want 8-hex)", version)
			}
		}
	}
	if !foundEvent {
		t.Errorf("no SSE event received; body was:\n%s", body)
	}
}

// TestHandleEvents_DisconnectCleansUp verifies that after an SSE subscriber
// disconnects, it is removed from the server's subscriber map (no leak).
func TestHandleEvents_DisconnectCleansUp(t *testing.T) {
	t.Parallel()

	s := newServer(silentLogger())
	h := s.handler()

	recorder := newFakeResponseRecorder()
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	ctx, cancel := withCancelContext(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(recorder, req)
	}()

	// Wait for handler to register and flush.
	select {
	case <-recorder.flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not flush headers within 2s")
	}

	// Verify the subscriber is registered.
	s.mu.RLock()
	before := len(s.subscribers)
	s.mu.RUnlock()
	if before != 1 {
		t.Errorf("expected 1 subscriber before disconnect, got %d", before)
	}

	// Disconnect.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler goroutine did not exit within 2s")
	}

	// Verify the subscriber is removed.
	s.mu.RLock()
	after := len(s.subscribers)
	s.mu.RUnlock()
	if after != 0 {
		t.Errorf("expected 0 subscribers after disconnect, got %d", after)
	}
}

// TestHandleControl_ConcurrentWithShutdownDoesNotPanic is the regression test
// for the send-on-closed-channel panic: /control pushes racing server shutdown
// must never panic, in any interleaving. The old implementation closed
// per-subscriber channels in shutdown while handleControl sent on a snapshot
// of them outside the lock — a push landing in that window crashed the
// process. Run under -race.
func TestHandleControl_ConcurrentWithShutdownDoesNotPanic(t *testing.T) {
	t.Parallel()

	s := newServer(silentLogger())
	h := s.handler()

	// Register several SSE subscribers so the broadcast path has real targets.
	const subscriberCount = 4
	var sseWG sync.WaitGroup
	for range subscriberCount {
		rec := newFakeResponseRecorder()
		req := httptest.NewRequest(http.MethodGet, "/events", nil)
		sseWG.Go(func() {
			h.ServeHTTP(rec, req)
		})
		select {
		case <-rec.flushed:
		case <-time.After(2 * time.Second):
			t.Fatal("SSE handler did not flush headers within 2s")
		}
	}

	// Hammer /control from several goroutines; shutdown fires mid-flight so
	// broadcasts interleave with the shutdown signal on both sides.
	const (
		pushers            = 8
		pushesPerGoroutine = 300
	)
	var pushed atomic.Int64
	var pushWG sync.WaitGroup
	for i := range pushers {
		pushWG.Add(1)
		go func(id int) {
			defer pushWG.Done()
			for n := range pushesPerGoroutine {
				m := toolmanifest.Manifest{Tools: []toolmanifest.Tool{{
					Name:      fmt.Sprintf("tool-%d-%d", id, n),
					Mode:      "remote",
					Endpoint:  "http://x.svc",
					Transport: "streamable-http",
				}}}
				body, err := json.Marshal(m)
				if err != nil {
					t.Errorf("marshal manifest: %v", err)
					return
				}
				req := httptest.NewRequest(http.MethodPost, "/control", bytes.NewReader(body))
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req) // must not panic, before or after shutdown
				if rr.Code != http.StatusNoContent {
					t.Errorf("POST /control during shutdown race: status = %d, want 204", rr.Code)
					return
				}
				pushed.Add(1)
			}
		}(i)
	}

	// Trigger shutdown once pushes are demonstrably in flight, so the bulk of
	// the broadcasts land after the signal.
	for pushed.Load() < pushers {
		time.Sleep(time.Millisecond)
	}
	s.signalShutdown()
	s.signalShutdown() // idempotent — second call must be a no-op

	pushWG.Wait()

	// All SSE handlers must exit via the done signal (bounded wait) and clean
	// their map entries.
	handlersDone := make(chan struct{})
	go func() {
		sseWG.Wait()
		close(handlersDone)
	}()
	select {
	case <-handlersDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handlers did not exit within 2s after shutdown signal")
	}

	s.mu.RLock()
	remaining := len(s.subscribers)
	s.mu.RUnlock()
	if remaining != 0 {
		t.Errorf("expected 0 subscribers after shutdown, got %d", remaining)
	}
}

// ── cold-start ────────────────────────────────────────────────────────────────

// TestLoadInitialManifest groups cold-start tests that swap the readFile
// package variable. They must NOT run in parallel with each other to avoid
// races on the shared variable.
func TestLoadInitialManifest(t *testing.T) {
	t.Run("file_loaded", func(t *testing.T) {
		// Provide a valid tools.json payload via the readFile hook.
		manifest := toolmanifest.Manifest{Tools: []toolmanifest.Tool{
			{Name: "cold-start-tool", Mode: "remote", Endpoint: "http://cs.svc", Transport: "streamable-http"},
		}}
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}

		old := readFile
		t.Cleanup(func() { readFile = old })
		readFile = func(_ string) ([]byte, error) { return data, nil }

		s := newServer(silentLogger())
		s.loadInitialManifest("/etc/agent/tools.json")

		got := getTools(t, s.handler())
		if len(got.Tools) != 1 || got.Tools[0].Name != "cold-start-tool" {
			t.Errorf("cold-start: got tools %+v, want [cold-start-tool]", got.Tools)
		}
	})

	t.Run("missing_file_uses_empty", func(t *testing.T) {
		old := readFile
		t.Cleanup(func() { readFile = old })
		readFile = func(_ string) ([]byte, error) {
			return nil, &os.PathError{Op: "open", Path: "/etc/agent/tools.json", Err: os.ErrNotExist}
		}

		s := newServer(silentLogger())
		s.loadInitialManifest("/etc/agent/tools.json")

		got := getTools(t, s.handler())
		if len(got.Tools) != 0 {
			t.Errorf("missing file: expected empty manifest, got %+v", got.Tools)
		}
	})

	t.Run("parse_error_uses_empty", func(t *testing.T) {
		old := readFile
		t.Cleanup(func() { readFile = old })
		readFile = func(_ string) ([]byte, error) { return []byte("{not valid json"), nil }

		s := newServer(silentLogger())
		s.loadInitialManifest("/etc/agent/tools.json")

		got := getTools(t, s.handler())
		if len(got.Tools) != 0 {
			t.Errorf("parse error: expected empty manifest, got %+v", got.Tools)
		}
	})
}
