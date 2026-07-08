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

// Package main implements the tool-discovery sidecar service.
//
// The sidecar exposes a small HTTP API on DISCOVERY_PORT (default 2999):
//
//	GET  /tools   — current manifest (JSON, Normalize'd)
//	GET  /events  — SSE stream; one event per manifest swap (data: version\n\n)
//	POST /control — full-manifest replace pushed by the binding controller
//	GET  /healthz — 200 ok
//
// Cold start: if TOOLS_JSON_PATH (default /etc/agent/tools.json) exists it is
// loaded as the initial manifest. Parse errors are logged and ignored — an
// empty manifest is served instead. The controller will push a fresh manifest
// via /control once it reconciles.
//
// Graceful shutdown on SIGTERM: a server-wide done channel signals all SSE
// handlers to exit. Per-subscriber channels are never closed — an in-flight
// /control broadcast may still hold references to them, and sending on a
// closed channel panics. Orphaned channels are garbage-collected once their
// handler exits and the broadcast snapshot is dropped.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

const (
	// maxControlBodyBytes is the maximum accepted /control request body size (1 MiB).
	maxControlBodyBytes = 1 << 20
)

// server holds the live manifest and the SSE subscriber registry.
// manifest and subscribers are protected by mu; logger is immutable after
// construction; done is closed exactly once (via shutdownOnce) to signal all
// SSE handlers to exit.
type server struct {
	mu           sync.RWMutex
	manifest     toolmanifest.Manifest
	subscribers  map[chan string]struct{}
	logger       *slog.Logger
	done         chan struct{}
	shutdownOnce sync.Once
}

// newServer constructs a server with an empty initial manifest.
func newServer(logger *slog.Logger) *server {
	return &server{
		manifest:    toolmanifest.Normalize(toolmanifest.Manifest{}),
		subscribers: make(map[chan string]struct{}),
		logger:      logger,
		done:        make(chan struct{}),
	}
}

// loadInitialManifest attempts to read path and install it as the initial
// manifest. Errors are logged at warn level and ignored — the server continues
// with an empty manifest so the controller push path still works.
func (s *server) loadInitialManifest(path string) {
	data, err := readFile(path)
	if err != nil {
		s.logger.Warn("cold-start: tools.json not readable — using empty manifest", "path", path, "err", err)
		return
	}

	var m toolmanifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		s.logger.Warn("cold-start: tools.json parse error — using empty manifest", "path", path, "err", err)
		return
	}

	normalized := toolmanifest.Normalize(m)
	s.mu.Lock()
	s.manifest = normalized
	s.mu.Unlock()
	s.logger.Info("cold-start: loaded tools.json",
		"path", path, "version", normalized.Version, "tools", len(normalized.Tools))
}

// handler returns the HTTP mux for the discovery server.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /tools", s.handleTools)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("POST /control", s.handleControl)
	return mux
}

// handleHealthz serves GET /healthz → 200 ok.
func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "ok")
}

// handleTools serves GET /tools → current manifest JSON.
func (s *server) handleTools(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	m := s.manifest
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	if err := enc.Encode(m); err != nil {
		s.logger.Error("GET /tools: encode error", "err", err)
	}
}

// handleEvents serves GET /events as an SSE stream.
//
// Each manifest swap sends: data: <version>\n\n
//
// The subscriber channel is buffered (size 8) so a slow reader doesn't block
// the /control handler. If the buffer is full, the event is dropped for that
// subscriber (the subscriber receives the next event instead; the manifest is
// always available via GET /tools). On disconnect or server shutdown (s.done
// closed) the subscriber is removed and the goroutine exits. The subscriber
// channel itself is never closed — see the package comment on shutdown safety.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ch := make(chan string, 8)

	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.subscribers, ch)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			// Server is shutting down.
			return
		case version := <-ch:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", version); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleControl serves POST /control.
//
// Request body: Manifest JSON (≤1 MiB). The server Normalize's the manifest
// (recomputes version server-side; ignores client-supplied version), atomically
// swaps the live manifest, and broadcasts the new version to all SSE
// subscribers. Responds 204 No Content on success.
func (s *server) handleControl(w http.ResponseWriter, r *http.Request) {
	lr := &io.LimitedReader{R: r.Body, N: maxControlBodyBytes + 1}
	body, err := io.ReadAll(lr)
	if err != nil {
		s.logger.Error("POST /control: read body error", "err", err)
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	if lr.N == 0 {
		// N reaches zero when the body meets or exceeds the limit.
		s.logger.Warn("POST /control: body exceeds 1 MiB limit — rejected")
		http.Error(w, "request body too large (limit 1 MiB)", http.StatusRequestEntityTooLarge)
		return
	}

	var m toolmanifest.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		s.logger.Warn("POST /control: invalid JSON body", "err", err)
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	normalized := toolmanifest.Normalize(m)

	s.mu.Lock()
	s.manifest = normalized
	subs := make([]chan string, 0, len(s.subscribers))
	for ch := range s.subscribers {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	// Broadcast outside the lock so slow subscribers don't hold the lock.
	for _, ch := range subs {
		select {
		case ch <- normalized.Version:
		default:
			// Subscriber buffer full — drop this event for this subscriber.
		}
	}

	s.logger.Info("POST /control: manifest updated", "version", normalized.Version, "tools", len(normalized.Tools))
	w.WriteHeader(http.StatusNoContent)
}

// signalShutdown signals all SSE handleEvents goroutines to exit by closing
// the server-wide done channel. Per-subscriber channels are deliberately NOT
// closed: an in-flight /control broadcast may hold a snapshot of them, and
// sending on a closed channel panics. Handlers exit via the done signal, their
// deferred cleanup removes them from the map, and the channels are
// garbage-collected. Idempotent — safe to call any number of times, in any
// order relative to http.Server.Shutdown.
func (s *server) signalShutdown() {
	s.shutdownOnce.Do(func() { close(s.done) })
}
