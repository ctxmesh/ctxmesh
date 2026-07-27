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

package statelayer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// maxMemoryBody caps a memory request body (1 MiB).
const maxMemoryBody = 1 << 20

// memoryScopeHeader lets the launcher request the shared team scratchpad. The
// proxy keys shared memory under the TOKEN's registry boundary, so a caller can
// only ever reach its own registry's shared space (never another tenant's).
const memoryScopeHeader = "X-Memory-Scope"

// Verifier verifies a run-capability token (satisfied by *runcap.Verifier).
type Verifier interface {
	Verify(token string) (runcap.Capability, error)
}

// Server is the state-layer proxy's HTTP handler (ADR 0050). It authenticates each
// request with the run-capability token, derives the access Scope SERVER-SIDE from
// the verified claims, and serves the memory API over the credentialed store.
type Server struct {
	store    MemoryStore
	verifier Verifier
	// devScope, when non-nil, is used for requests that carry no token — the
	// STATELAYER_DEV_MODE bypass (never enabled in production). It scopes by a
	// static dev identity without verification.
	devScope *Scope
	now      func() time.Time
}

// Options configures a Server.
type Options struct {
	Store    MemoryStore
	Verifier Verifier
	// DevAgent, when set, enables the dev bypass: unauthenticated requests are
	// scoped to this "<namespace>/<agent>" identity. NEVER set in production.
	DevAgent string
	Now      func() time.Time
}

// NewServer builds the proxy handler.
func NewServer(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("statelayer: Store is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Server{store: opts.Store, verifier: opts.Verifier, now: now}
	if strings.TrimSpace(opts.DevAgent) != "" {
		sc, err := scopeFromAgent(opts.DevAgent, "", false)
		if err != nil {
			return nil, fmt.Errorf("statelayer: invalid DevAgent: %w", err)
		}
		s.devScope = &sc
	}
	if opts.Verifier == nil && s.devScope == nil {
		return nil, errors.New("statelayer: a Verifier or DevAgent (dev bypass) is required")
	}
	return s, nil
}

// Handler returns the HTTP mux for the proxy.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /memory/{conversationId}", s.scoped(s.handleGet))
	mux.HandleFunc("PUT /memory/{conversationId}", s.scoped(s.handlePut))
	mux.HandleFunc("POST /memory/{conversationId}/append", s.scoped(s.handleAppend))
	mux.HandleFunc("GET /memory/{conversationId}/search", s.scoped(s.handleSearch))
	return mux
}

type scopedHandler func(ctx context.Context, sc Scope, convID string, w http.ResponseWriter, r *http.Request)

// scoped authenticates the request, derives the server-side Scope from the verified
// token (or the dev bypass), validates the conversation id, and dispatches.
func (s *Server) scoped(fn scopedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, ok := s.authorize(w, r)
		if !ok {
			return
		}
		convID := r.PathValue("conversationId")
		if err := validateConversationID(convID); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), memoryOpTimeout)
		defer cancel()
		fn(ctx, sc, convID, w, r)
	}
}

// authorize derives the request Scope. It verifies the Bearer run-capability token
// and derives the scope from the claims; when no token is present and the dev
// bypass is enabled, it uses the static dev scope. The shared-scope request is
// keyed under the TOKEN's registry, so it can never reach another tenant's data.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (Scope, bool) {
	wantShared := strings.EqualFold(strings.TrimSpace(r.Header.Get(memoryScopeHeader)), memoryScopeShared)
	token := bearerToken(r)
	if token == "" {
		if s.devScope != nil {
			return *s.devScope, true // dev bypass (no verification)
		}
		writeJSONError(w, http.StatusUnauthorized, "missing run-capability token")
		return Scope{}, false
	}
	if s.verifier == nil {
		writeJSONError(w, http.StatusUnauthorized, "token verification is not configured")
		return Scope{}, false
	}
	runCap, err := s.verifier.Verify(token)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid run-capability token")
		return Scope{}, false
	}
	sc, err := scopeFromAgent(runCap.Agent, runCap.Boundary, wantShared)
	if err != nil {
		// A valid token whose agent claim can't be scoped is a 403 (authenticated,
		// but not authorizable to any key space) — never a 500.
		writeJSONError(w, http.StatusForbidden, "token carries no usable agent scope")
		return Scope{}, false
	}
	return sc, true
}

const memoryScopeShared = "shared"

func bearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

func (s *Server) handleGet(ctx context.Context, sc Scope, convID string, w http.ResponseWriter, _ *http.Request) {
	entries, err := s.store.Get(ctx, sc.key(convID))
	if err != nil {
		backendError(w, err)
		return
	}
	if entries == nil {
		entries = []json.RawMessage{}
	}
	w.Header().Set("ETag", strconv.Itoa(len(entries)))
	writeJSON(w, entries)
}

func (s *Server) handlePut(ctx context.Context, sc Scope, convID string, w http.ResponseWriter, r *http.Request) {
	body, err := readCappedBody(w, r)
	if err != nil {
		writeJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(body, &entries); err != nil {
		writeJSONError(w, http.StatusBadRequest, "body must be a JSON array: "+err.Error())
		return
	}
	// Optimistic-concurrency replace (ADR 0036 / ADR 0050 §7) — one atomic proxy op.
	if ifMatch := strings.TrimSpace(r.Header.Get("If-Match")); ifMatch != "" {
		expected, convErr := strconv.Atoi(strings.Trim(ifMatch, `"`))
		if convErr != nil {
			writeJSONError(w, http.StatusBadRequest, "If-Match must be an integer version (a prior ETag)")
			return
		}
		newVersion, conflict, repErr := s.store.ReplaceIfVersion(ctx, sc.key(convID), entries, expected, memoryTTL)
		if repErr != nil {
			backendError(w, repErr)
			return
		}
		if conflict {
			writeJSONError(w, http.StatusPreconditionFailed,
				"version conflict: the conversation changed since your read — re-read and retry")
			return
		}
		w.Header().Set("ETag", strconv.Itoa(newVersion))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.store.Replace(ctx, sc.key(convID), entries, memoryTTL); err != nil {
		backendError(w, err)
		return
	}
	w.Header().Set("ETag", strconv.Itoa(len(entries)))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAppend(ctx context.Context, sc Scope, convID string, w http.ResponseWriter, r *http.Request) {
	body, err := readCappedBody(w, r)
	if err != nil {
		writeJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "body must be a single valid JSON value: "+err.Error())
		return
	}
	messageID := strings.TrimSpace(r.Header.Get(messageIDHeader))
	if messageID == "" {
		messageID = newMessageID()
	}
	// Attribution is SERVER-authoritative: the agent comes from sc (the token), and
	// any caller-supplied `agent` field is overwritten (ADR 0050 §1).
	entry := attributeEntry(json.RawMessage(compact.Bytes()), sc.agent, messageID, s.now())
	if _, err := s.store.Append(ctx, sc.key(convID), entry, memoryTTL); err != nil {
		backendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSearch(ctx context.Context, sc Scope, convID string, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	entries, err := s.store.Get(ctx, sc.key(convID))
	if err != nil {
		backendError(w, err)
		return
	}
	matches := make([]json.RawMessage, 0, len(entries))
	for _, e := range entries {
		if q == "" || strings.Contains(string(e), q) {
			matches = append(matches, e)
		}
	}
	writeJSON(w, matches)
}

func readCappedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMemoryBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, fmt.Errorf("request body exceeds %d-byte limit", maxMemoryBody)
		}
		return nil, err
	}
	return body, nil
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "encode response: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// backendError → 502: the caller treats memory as best-effort (fail-open, ADR 0050 §5).
func backendError(w http.ResponseWriter, err error) {
	writeJSONError(w, http.StatusBadGateway, "memory backend: "+err.Error())
}
