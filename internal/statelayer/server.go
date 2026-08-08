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
)

// maxMemoryBody caps a memory request body (1 MiB).
const maxMemoryBody = 1 << 20

// memoryScopeHeader lets the launcher request the shared team scratchpad. The
// proxy keys shared memory under the TOKEN's registry boundary, so a caller can
// only ever reach its own registry's shared space (never another tenant's).
const memoryScopeHeader = "X-Memory-Scope"

// RegistryResolver maps a pod identity (namespace + agent SA name) to the agent's
// registry id — the SERVER-TRUSTED shared-memory boundary (`mem:shared:{registry}:`),
// which used to be the runcap `bnd` claim (ADR 0052 §C6 RESOLUTION). It reads the
// registryIDLabel the controller stamps on the identity SA. nil (or "" registry) ⇒ a
// shared-scope request falls back to the private scope (no registry to key under). A
// non-nil error is an infra failure the caller MUST fail closed on (never key shared
// memory under a guessed/missing registry).
type RegistryResolver interface {
	Registry(ctx context.Context, namespace, serviceAccount string) (string, error)
}

// Server is the state-layer proxy's HTTP handler (ADR 0050; ADR 0052 §C6 RESOLUTION). It
// authenticates each request by the caller's POD identity (TokenReview), derives the
// access Scope SERVER-SIDE from the verified identity, and serves the memory API over the
// credentialed store.
type Server struct {
	store MemoryStore
	// tenants resolves a namespace → owning tenant id for the quota/async endpoints
	// (M53). nil in memory-only / local-dev deployments (no cluster config); the
	// quota endpoints report unavailable rather than guessing a tenant.
	tenants TenantResolver
	// podAuth authenticates a launcher's pod-identity token (cached TokenReview) for
	// the quota/async AND memory endpoints — all workload-scoped (ADR 0052 §C6
	// RESOLUTION). nil in memory-only deployments.
	podAuth PodAuthenticator
	// registries resolves a pod identity → registry id for SHARED-scope memory (the
	// server-trusted boundary the controller stamps on the agent SA). nil ⇒ shared
	// scope falls back to private.
	registries RegistryResolver
	// quota is the per-tenant model-quota accumulator (M53). nil in memory-only
	// deployments; the quota endpoints then report unavailable.
	quota QuotaStore
	// dedup is the async seen-set (M53). nil in memory-only deployments; the dedup
	// endpoint then reports unavailable (the launcher fails CLOSED).
	dedup DedupStore
	// devScope, when non-nil, is used for requests that carry no token — the
	// STATELAYER_DEV_MODE bypass (never enabled in production). It scopes by a
	// static dev identity without verification.
	devScope *Scope
	now      func() time.Time
}

// Options configures a Server.
type Options struct {
	Store MemoryStore
	// TenantResolver maps a namespace → owning tenant id for the quota/async
	// endpoints (M53). Optional: nil ⇒ the quota endpoints report unavailable.
	TenantResolver TenantResolver
	// PodAuthenticator verifies a launcher's pod-identity token for the quota/async
	// AND memory endpoints (ADR 0052 §C6 RESOLUTION). Optional: nil ⇒ those endpoints
	// report unavailable.
	PodAuthenticator PodAuthenticator
	// RegistryResolver resolves the SHARED-scope memory boundary from a pod identity
	// (ADR 0052 §C6 RESOLUTION). Optional: nil ⇒ shared-scope requests fall back to
	// the private scope.
	RegistryResolver RegistryResolver
	// QuotaStore is the per-tenant model-quota accumulator (M53). Optional: nil ⇒ the
	// quota endpoints report unavailable.
	QuotaStore QuotaStore
	// DedupStore is the async seen-set (M53). Optional: nil ⇒ the dedup endpoint
	// reports unavailable.
	DedupStore DedupStore
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
	s := &Server{
		store:      opts.Store,
		tenants:    opts.TenantResolver,
		podAuth:    opts.PodAuthenticator,
		registries: opts.RegistryResolver,
		quota:      opts.QuotaStore,
		dedup:      opts.DedupStore,
		now:        now,
	}
	if strings.TrimSpace(opts.DevAgent) != "" {
		sc, err := scopeFromAgent(opts.DevAgent, "", false)
		if err != nil {
			return nil, fmt.Errorf("statelayer: invalid DevAgent: %w", err)
		}
		s.devScope = &sc
	}
	// A PodAuthenticator or dev bypass is REQUIRED to serve memory requests, but the
	// server still STARTS without one (it refuses every request with 401) so a fresh
	// install deploys cleanly before cluster auth is wired — an idle proxy, not a
	// CrashLoop. authorize() enforces the deny; NewServer never fails on a missing
	// authenticator.
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
	// Quota endpoints (M53) — pod-identity authenticated, tenant-scoped SERVER-SIDE.
	mux.HandleFunc("POST /quota/rpm", s.handleQuotaRPM)
	mux.HandleFunc("GET /quota/spend", s.handleQuotaGetSpend)
	mux.HandleFunc("POST /quota/spend", s.handleQuotaAddSpend)
	mux.HandleFunc("POST /quota/slot", s.handleQuotaAcquireSlot)
	mux.HandleFunc("DELETE /quota/slot", s.handleQuotaReleaseSlot)
	// Async dedup (M53) — pod-identity authenticated, namespace-scoped SERVER-SIDE.
	mux.HandleFunc("POST /dedup", s.handleDedup)
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

// authorize derives the request Scope from the pod's IDENTITY (ADR 0052 §C6
// RESOLUTION): session memory is workload-scoped, so it authenticates the launcher's
// projected ServiceAccount token (TokenReview, via podAuth) and derives the (namespace,
// agent) scope SERVER-SIDE from the verified SA name — never from caller input. The
// runcap is gone from this path, so a compromised proxy has no user credential token to
// replay at the credential plane. A shared-scope request is keyed under the registry the
// controller stamped on the SA, so it can never reach another tenant's data.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (Scope, bool) {
	wantShared := strings.EqualFold(strings.TrimSpace(r.Header.Get(memoryScopeHeader)), memoryScopeShared)
	token := bearerToken(r)
	if token == "" {
		if s.devScope != nil {
			return *s.devScope, true // dev bypass (no verification)
		}
		writeJSONError(w, http.StatusUnauthorized, "missing pod-identity token")
		return Scope{}, false
	}
	if s.podAuth == nil {
		writeJSONError(w, http.StatusUnauthorized, "pod authentication is not configured")
		return Scope{}, false
	}
	id, err := s.podAuth.Identity(r.Context(), token)
	if err != nil {
		if errors.Is(err, ErrTokenRejected) {
			writeJSONError(w, http.StatusUnauthorized, "invalid pod-identity token")
		} else {
			// TokenReview infra failure (apiserver unreachable) — fail closed, but as
			// 503 so the launcher retries rather than treating it as a hard rejection.
			writeJSONError(w, http.StatusServiceUnavailable, "pod authentication unavailable")
		}
		return Scope{}, false
	}
	agent, ok := agentNameFromSA(id.ServiceAccount)
	if !ok {
		// A verified pod token that is NOT a per-agent identity SA (agent-<name>) has no
		// agent scope — 403 (authenticated, not authorizable), never a guessed key.
		writeJSONError(w, http.StatusForbidden, "pod identity is not an agent (expected the agent-<name> ServiceAccount)")
		return Scope{}, false
	}
	// Shared scope needs a registry boundary; resolve it SERVER-SIDE from the SA. A
	// resolver failure fails CLOSED (503) — we never key shared memory under a missing
	// registry, which would silently split the team scratchpad or cross a boundary.
	// When NO resolver is wired (memory-only/dev, or a production build failure) a shared
	// request falls back to the private scope — safe (private is never a cross-tenant
	// leak); the absence is logged once at startup by cmd/statelayer-proxy, so this is
	// not a silent-in-production footgun.
	var boundary string
	if wantShared && s.registries != nil {
		reg, rErr := s.registries.Registry(r.Context(), id.Namespace, id.ServiceAccount)
		if rErr != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "registry resolution unavailable")
			return Scope{}, false
		}
		if reg != "" {
			boundary = registryBoundaryPrefix + reg
		}
		// reg == "" (agent is not a registry member) → boundary stays empty →
		// scopeFromAgent falls back to the private scope (matches prior behavior).
	}
	// id.Namespace is a Kubernetes namespace (DNS-1123, no "/") and agent is the
	// prefix-stripped SA name, so the "<ns>/<agent>" join splits back cleanly in
	// scopeFromAgent — the defense-in-depth check there 403s rather than mis-keys anyway.
	sc, err := scopeFromAgent(id.Namespace+"/"+agent, boundary, wantShared)
	if err != nil {
		writeJSONError(w, http.StatusForbidden, "pod identity carries no usable agent scope")
		return Scope{}, false
	}
	return sc, true
}

// agentSAPrefix is the per-agent identity ServiceAccount prefix the controller mints
// ("agent-<name>", ADR 0052 §C6 RESOLUTION). The proxy recovers the agent name by
// stripping it — so the memory key stays `mem:{ns}/{name}:`, byte-identical to the
// runcap path it replaces (no key migration).
const agentSAPrefix = "agent-"

// registryBoundaryPrefix is the "r:<registry>" boundary form scopeFromAgent expects for
// a shared-scope request (mirrors the runcap `bnd` it replaces).
const registryBoundaryPrefix = "r:"

// agentNameFromSA recovers the agent name from a per-agent identity SA name
// ("agent-<name>" → "<name>"). ok is false when the SA is not an agent identity (no
// prefix, or an empty remainder) — such a principal has no memory scope.
func agentNameFromSA(sa string) (string, bool) {
	name, ok := strings.CutPrefix(sa, agentSAPrefix)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

const memoryScopeShared = "shared"

// resolveTenant maps a namespace to its owning tenant id via the configured
// resolver (M53 quota/async paths). It returns ("", false) when no resolver is
// configured (memory-only deployment) OR the namespace is untenanted — the caller
// treats both as "no tenant quota applies". A non-nil error is an infrastructure
// failure the caller must surface (never silently treat as untenanted).
func (s *Server) resolveTenant(ctx context.Context, namespace string) (id string, ok bool, err error) {
	if s.tenants == nil {
		return "", false, nil
	}
	id, err = s.tenants.TenantID(ctx, namespace)
	if err != nil {
		return "", false, err
	}
	return id, id != "", nil
}

// authenticatePod verifies a launcher's pod-identity token and returns its
// namespace (M53 quota/async paths). It returns (ns, nil) on success;
// (\"\", ErrTokenRejected) for an invalid token; and (\"\", errPodAuthUnavailable)
// when no authenticator is configured (memory-only deployment) — distinct from an
// auth-infra error so the handler can 503 either way but log the cause.
func (s *Server) authenticatePod(ctx context.Context, token string) (string, error) {
	if s.podAuth == nil {
		return "", errPodAuthUnavailable
	}
	return s.podAuth.Namespace(ctx, token)
}

// errPodAuthUnavailable signals the proxy has no pod authenticator wired (no
// cluster config) — the quota endpoints are unavailable, not a rejection.
var errPodAuthUnavailable = errors.New("statelayer: pod authentication is not configured")

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
