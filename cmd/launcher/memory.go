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

// The :2998 traced memory endpoint (M5, state-layer.md §"The :2998 launcher
// memory endpoint"). A second HTTP listener, started ONLY when
// MEMORY_BACKEND_ADDR is injected (i.e. the agent has spec.sessionMemory), that
// gives any agent — with no SDK — a language-agnostic, traced door to session
// memory. Every op emits a memory.get|put|append|search span.
//
// Storage encoding — Redis LIST of JSON-string entries (NOT one JSON-array
// blob). The spec (state-layer.md, "Concurrent turns") calls append "a single
// RPUSH-equivalent op"; a LIST makes that literally true. Append is a genuine
// atomic RPUSH (no read-modify-write, no WATCH, no lost-update race under
// concurrent turns); PUT replaces the whole list in one MULTI (DEL + RPUSH);
// GET assembles the JSON array from LRANGE. The HTTP contract is identical to a
// JSON-array blob: GET/search always return/scan a JSON array. TTL is (re)set
// on every write via EXPIRE inside the same pipeline so it never races the data.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ctxmesh/agentry/internal/runcap"
)

const (
	// defaultMemoryPort is the localhost port the memory listener binds when
	// MEMORY_PORT is unset. Injected by the controller as MEMORY_PORT=2998.
	defaultMemoryPort = 2998

	// memoryTTL is the dev-default lifetime of a conversation's context,
	// refreshed on every write (state-layer.md §Keying). Revisit with the M11
	// retention-policy work.
	memoryTTL = 24 * time.Hour

	// memoryOpTimeout bounds every backend round-trip (state-layer.md §Design:
	// "per-op timeout 2s"). A slow/hung Valkey must never block the request
	// path beyond this.
	memoryOpTimeout = 2 * time.Second

	// maxMemoryBody caps PUT/append bodies at 1MiB — the same bound as the
	// discovery sidecar's /control (state-layer.md §"Large contexts").
	maxMemoryBody = 1 << 20

	// maxConversationID bounds the caller-supplied conversation id. It lands in
	// both span attributes and Redis keys, so it must be short and path-safe.
	maxConversationID = 128

	// maxConversationEntries caps a conversation's stored entry count (m33.6, ADR 0036): append
	// LTRIMs to the last N so a long-lived (especially shared, m33.3) conversation cannot grow the
	// store without bound — the oldest entries are evicted (the summary hook is the extension point;
	// TTL still bounds age). The read-side replay window (SDK MAX_HISTORY_MESSAGES) is smaller still.
	maxConversationEntries = 500
)

// MemoryStore is the minimal backend surface the memory handlers need. It is an
// interface (not *redis.Client directly) so unit tests can drive the handlers
// against a fake without a real Redis, and so the concrete client stays behind
// a lazy-connect wrapper.
type MemoryStore interface {
	// Get returns every entry for key as raw JSON values, in insertion order.
	// A missing key yields an empty slice and no error (best-effort contract).
	Get(ctx context.Context, key string) ([]json.RawMessage, error)
	// Replace atomically overwrites key with entries and (re)sets its TTL.
	Replace(ctx context.Context, key string, entries []json.RawMessage, ttl time.Duration) error
	// ReplaceIfVersion is Replace guarded by optimistic concurrency (ADR 0036, m33.2): it overwrites
	// key ONLY if its current version (the list length) still equals expectedVersion, atomically.
	// conflict=true (no write) when another writer advanced the version since the caller's read — so
	// a stale rewrite can never silently clobber a concurrent append. Returns the new version.
	ReplaceIfVersion(
		ctx context.Context, key string, entries []json.RawMessage, expectedVersion int, ttl time.Duration,
	) (newVersion int, conflict bool, err error)
	// Append atomically appends one entry to key and (re)sets its TTL. It
	// returns the resulting entry count.
	Append(ctx context.Context, key string, entry json.RawMessage, ttl time.Duration) (int, error)
}

// redisStore is the production MemoryStore backed by go-redis. The client
// connects lazily (redis.NewClient does not dial), so constructing it when the
// backend is down is cheap and non-fatal — the first op surfaces the error.
type redisStore struct {
	rdb *redis.Client
}

func newRedisStore(addr string) *redisStore {
	return &redisStore{
		rdb: redis.NewClient(&redis.Options{
			Addr: addr,
			// A single op must never wait longer than memoryOpTimeout; the
			// per-op context deadline is the primary bound, these are belt-and-
			// braces so a dead backend fails fast rather than hanging a dial.
			DialTimeout:  memoryOpTimeout,
			ReadTimeout:  memoryOpTimeout,
			WriteTimeout: memoryOpTimeout,
		}),
	}
}

func (s *redisStore) Get(ctx context.Context, key string) ([]json.RawMessage, error) {
	vals, err := s.rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(vals))
	for _, v := range vals {
		out = append(out, json.RawMessage(v))
	}
	return out, nil
}

func (s *redisStore) Replace(ctx context.Context, key string, entries []json.RawMessage, ttl time.Duration) error {
	// One round-trip, one atomic unit: DEL then (optionally) RPUSH all entries
	// then EXPIRE. An empty PUT clears the conversation (DEL only) — a
	// subsequent GET correctly returns []. A pipeline is transactional under
	// TxPipeline (MULTI/EXEC), so a concurrent GET never observes a half-built
	// list.
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, key)
	if len(entries) > 0 {
		vals := make([]any, 0, len(entries))
		for _, e := range entries {
			vals = append(vals, string(e))
		}
		pipe.RPush(ctx, key, vals...)
		pipe.Expire(ctx, key, ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisStore) ReplaceIfVersion(
	ctx context.Context, key string, entries []json.RawMessage, expectedVersion int, ttl time.Duration,
) (int, bool, error) {
	// WATCH the key so a concurrent write (append or replace) between the LLEN read and the MULTI
	// aborts the EXEC (redis.TxFailedErr) — optimistic concurrency, no lock held. The version is the
	// list length (ADR 0036 permits length or a monotone rev); an append advances it, so a stale
	// replace's expectedVersion no longer matches and is rejected.
	var (
		newVersion int
		conflict   bool
	)
	txf := func(tx *redis.Tx) error {
		n, err := tx.LLen(ctx, key).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		if int(n) != expectedVersion {
			conflict = true
			return nil
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, key)
			if len(entries) > 0 {
				vals := make([]any, 0, len(entries))
				for _, e := range entries {
					vals = append(vals, string(e))
				}
				pipe.RPush(ctx, key, vals...)
				pipe.Expire(ctx, key, ttl)
			}
			return nil
		})
		if err != nil {
			return err
		}
		newVersion = len(entries)
		return nil
	}
	err := s.rdb.Watch(ctx, txf, key)
	if errors.Is(err, redis.TxFailedErr) {
		return 0, true, nil // the watched key changed under us — a conflict, retry-able
	}
	if err != nil {
		return 0, false, err
	}
	return newVersion, conflict, nil
}

func (s *redisStore) Append(ctx context.Context, key string, entry json.RawMessage, ttl time.Duration) (int, error) {
	// RPUSH (atomic) then LTRIM to the last maxConversationEntries — a size cap (m33.6) so the
	// conversation can't grow unbounded — then EXPIRE (TTL refresh), all in one round-trip. LLEN
	// after the trim is the true post-cap count returned.
	pipe := s.rdb.TxPipeline()
	pipe.RPush(ctx, key, string(entry))
	pipe.LTrim(ctx, key, -maxConversationEntries, -1)
	pipe.Expire(ctx, key, ttl)
	llen := pipe.LLen(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return int(llen.Val()), nil
}

// memoryConfig is the subset of configuration the memory server needs. Parsed
// from env alongside the launcher Config (see loadConfig / MemoryEnabled).
type memoryConfig struct {
	// BackendAddr is MEMORY_BACKEND_ADDR — the Valkey host:port. When empty the
	// listener is not started at all.
	BackendAddr string
	// Port is MEMORY_PORT (default 2998).
	Port int
	// Namespace and Agent form the key prefix mem:{namespace}/{agent}:{convId}.
	// The controller injects them; they scope one agent's conversations away
	// from every other agent sharing the Valkey.
	Namespace string
	Agent     string
	// Scope selects the key layout (ADR 0035, m33.3): "" / "session" = PRIVATE per-agent
	// (mem:{namespace}/{agent}:{convId}); "shared" = a team scratchpad keyed
	// mem:shared:{registry}:{convId} — readable/writable by every agent in the same registry
	// conversation. Injected as MEMORY_SCOPE by the controller.
	Scope string
	// Registry is AGENT_REGISTRY_ID — the trust boundary (ADR 0033) the shared scope keys under.
	// Required for the shared scope; empty ⇒ shared falls back to private (a visible misconfig).
	Registry string
	// ProxyURL is STATELAYER_PROXY_URL (M51, ADR 0050 §8): when set, the session/shared routes are
	// REVERSE-PROXIED to the control-plane state-layer proxy (which holds the Valkey credential and
	// enforces per-tenant scope server-side) instead of hitting Valkey directly. Empty ⇒ direct
	// (unchanged, migration phase 1 dual-mode). The launcher authenticates to the proxy with the POD's
	// projected SA token (ADR 0052 §C6 RESOLUTION) — session memory is workload-scoped, so the proxy
	// derives the (ns,agent) scope from the pod identity, not a per-user runcap.
	ProxyURL string
	// TokenPath is the mounted projected SA-token file (STATELAYER_TOKEN_PATH) the forward reads to
	// authenticate to the proxy. Empty ⇒ the default mount path (resolvePodTokenPath).
	TokenPath string
	// PerUser is MEMORY_PER_USER=true (M98, EU1a, ADR 0080): the launcher stamps an X-Memory-User
	// header (a hash of the VERIFIED runcap's user id) on the private-scope forward so the proxy keys
	// each end-user's session memory into an isolated bucket. Fail-SAFE — an async/eventing turn with
	// no runcap simply gets the agent-wide bucket (never a 401). Ignored for the shared scope. Requires
	// STATELAYER_PROXY_URL (the proxy composes the key) + MCP_CAPABILITY_PUBLIC_KEY (to verify the cap).
	PerUser bool
}

// memoryScopeShared is the MEMORY_SCOPE value that selects the shared team scratchpad.
const memoryScopeShared = "shared"

// memoryServer holds the per-listener dependencies: the backend store, the key
// prefix, and the tracer. It is safe for concurrent use — every field is
// read-only after construction and the store is concurrency-safe.
type memoryServer struct {
	store     MemoryStore
	prefix    string // "mem:{namespace}/{agent}:"
	agent     string // this agent's name — the AUTHORITATIVE writer attribution (m33.1)
	tracer    trace.Tracer
	longTerm  *longTermProxy  // optional (ADR 0045); nil ⇒ no long-term endpoints
	knowledge *knowledgeProxy // optional (ADR 0061 Fork 3); nil ⇒ no managed-RAG retrieval endpoint
	// forward, when non-nil (STATELAYER_PROXY_URL set), reverse-proxies the session/shared routes to
	// the control-plane state-layer proxy — the launcher holds no Valkey path (ADR 0050 §8 phase 1).
	forward *httputil.ReverseProxy
}

// buildMemoryHTTPServer builds the :2998 listener, or nil when no memory surface is enabled. It serves the
// session/shared (Valkey) routes when a backend is wired, the long-term (token-service proxy) routes when ltProxy
// is set, and the managed-RAG retrieval route when kbProxy is set — an agent may have any subset (ADR 0045, 0061).
func buildMemoryHTTPServer(
	cfg Config, tracer trace.Tracer, ltProxy *longTermProxy, kbProxy *knowledgeProxy,
) *http.Server {
	if !cfg.MemoryEnabled() && ltProxy == nil && kbProxy == nil {
		return nil
	}
	// The :2998 listener ALSO serves the long-term-memory and knowledge (RAG) proxies, not just session
	// memory. A knowledge-only (or LT-only) agent has no session-memory backend, so loadMemoryConfig
	// returned a ZERO memoryConfig (Port 0) — binding ":0" (a random port) instead of 2998, so the in-pod
	// SDK's localhost:2998 got "connection refused" and RAG-in-chat silently retrieved nothing (M117).
	// Default the port here so the listener is reachable whenever ANY of the three proxies is active.
	if cfg.Memory.Port == 0 {
		cfg.Memory.Port = defaultMemoryPort
	}
	// Build the direct-Valkey store only when a backend addr is wired. When ONLY the state-layer proxy
	// is set (phase 3, no MEMORY_BACKEND_ADDR), store stays nil and newMemoryServer forwards instead.
	var store MemoryStore
	if cfg.Memory.BackendAddr != "" {
		store = newRedisStore(cfg.Memory.BackendAddr)
	}
	return &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Memory.Port),
		Handler: newMemoryServer(store, cfg.Memory, tracer, ltProxy, kbProxy).handler(),
	}
}

func newMemoryServer(
	store MemoryStore, cfg memoryConfig, tracer trace.Tracer, longTerm *longTermProxy, knowledge *knowledgeProxy,
) *memoryServer {
	// Shared scope (m33.3): key under the registry trust boundary so every agent in the
	// conversation reads/writes ONE scratchpad. It requires a registry — without one there is no
	// boundary to share within, so fall back to the private per-agent layout (the controller gates
	// the shared injection on membership, so this only bites a hand-rolled misconfig).
	prefix := fmt.Sprintf("mem:%s/%s:", cfg.Namespace, cfg.Agent)
	if cfg.Scope == memoryScopeShared && cfg.Registry != "" {
		prefix = fmt.Sprintf("mem:shared:%s:", cfg.Registry)
	}
	m := &memoryServer{
		store:     store,
		prefix:    prefix,
		agent:     cfg.Agent,
		tracer:    tracer,
		longTerm:  longTerm,
		knowledge: knowledge,
	}
	// Per-user session memory (M98, ADR 0080): build the run-capability verifier so the forward can
	// stamp X-Memory-User from a VERIFIED cap. Only needed for perUser private scope. Missing key ⇒
	// nil verifier ⇒ the forward degrades to the agent-wide bucket + logs (a visible misconfig, never
	// a cross-tenant leak — the users here already share one agent's trust boundary, product-grade).
	var capVerifier *runcap.Verifier
	if cfg.PerUser && cfg.Scope != memoryScopeShared {
		const degrade = "launcher: memory: per-user session requested but %s — session memory stays agent-wide\n"
		if pubB64 := strings.TrimSpace(os.Getenv("MCP_CAPABILITY_PUBLIC_KEY")); pubB64 == "" {
			fmt.Fprintf(os.Stderr, degrade, "MCP_CAPABILITY_PUBLIC_KEY unset")
		} else if pub, err := runcap.DecodePublicKey(pubB64); err != nil {
			fmt.Fprintf(os.Stderr, degrade, fmt.Sprintf("MCP_CAPABILITY_PUBLIC_KEY is bad (%v)", err))
		} else {
			capVerifier = runcap.NewVerifier(pub, strings.TrimSpace(os.Getenv("MCP_CAPABILITY_AUDIENCE")), nil)
		}
	}
	m.forward = buildStatelayerForward(cfg.ProxyURL, cfg.Scope == memoryScopeShared, cfg.TokenPath, capVerifier)
	return m
}

// buildStatelayerForward builds the reverse proxy to the control-plane state-layer proxy (M51, ADR
// 0050 §8; ADR 0052 §C6 RESOLUTION), or nil when STATELAYER_PROXY_URL is unset (direct-Valkey,
// unchanged). The Director authenticates to the proxy with the POD's projected SA token
// (Authorization: Bearer) — session memory is workload-scoped, so the proxy derives ALL scope
// (namespace + agent, and the shared registry) SERVER-SIDE from the verified pod identity, NOT a
// per-user runcap. The token is re-read from tokenPath on EVERY request so a kubelet-rotated token is
// always current. The scope INTENT (shared vs private) rides X-Memory-Scope; the proxy still keys
// shared memory under the SA-derived registry, so a bad scope hint can't cross tenants. The run
// capability is stripped — the proxy doesn't consume it, and it must not leak onward.
func buildStatelayerForward(
	proxyURL string, shared bool, tokenPath string, verifier *runcap.Verifier,
) *httputil.ReverseProxy {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	target, err := url.Parse(proxyURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher: memory: bad STATELAYER_PROXY_URL (%v) — falling back to direct Valkey\n", err)
		return nil
	}
	tokenPath = resolvePodTokenPath(tokenPath)
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Authenticate as the POD (projected SA token). A read failure leaves no
			// Authorization ⇒ the proxy fails closed (401) rather than the launcher serving
			// an unauthenticated request; the error is surfaced on stderr, not swallowed.
			if tok, terr := readPodToken(tokenPath); terr == nil {
				req.Header.Set("Authorization", "Bearer "+tok)
			} else {
				fmt.Fprintf(os.Stderr, "launcher: memory: pod token unavailable (%v) — proxy will reject\n", terr)
			}
			// Per-user session memory (M98, ADR 0080). NEVER trust a client-supplied X-Memory-User —
			// strip it unconditionally, then re-stamp it ONLY from a VERIFIED runcap (so a compromised
			// agent process can't read another user's bucket by forging the header). Fail-SAFE: no /
			// invalid cap, or no user id, ⇒ no header ⇒ agent-wide bucket, never a 401 (ADR 0052
			// async-safety — an eventing/async turn carries no runcap). verifier is nil unless perUser
			// private scope is configured with a good capability key.
			req.Header.Del(memoryUserHeader)
			if verifier != nil {
				if tok := strings.TrimSpace(req.Header.Get(runcap.HeaderName)); tok != "" {
					if c, verr := verifier.Verify(tok); verr == nil && c.User != "" {
						req.Header.Set(memoryUserHeader, memoryUserBucket(c.User))
					}
				}
			}
			// The agent's runcap has no meaning to the proxy's memory path; never forward it.
			req.Header.Del(runcap.HeaderName)
			if shared {
				req.Header.Set("X-Memory-Scope", memoryScopeShared)
			}
		},
	}
}

// memoryUserHeader carries the per-user key segment to the state-layer proxy (M98, EU1a). The proxy
// validates it as bounded lowercase hex and prepends it to the conversation key.
const memoryUserHeader = "X-Memory-User"

// memoryUserBucket derives that segment from the runcap's already-hashed user id: a truncated sha256
// hex so the value is ALWAYS bounded lowercase hex (matching the proxy's validator) and the raw id
// never becomes key material. 128 bits is ample against collisions within one agent's user set.
func memoryUserBucket(user string) string {
	sum := sha256.Sum256([]byte(user))
	return hex.EncodeToString(sum[:])[:32]
}

// messageIDHeader carries a per-hop message id (ADR 0035, m33.4). When an append omits it, the
// launcher mints one so every attributed entry is addressable.
const messageIDHeader = "X-Message-Id"

// attributeEntry stamps server-AUTHORITATIVE attribution on a message-shaped memory entry (ADR
// 0036, m33.1): a JSON object carrying string `role` + `content` gains `agent` (THIS agent — the
// caller cannot forge another's name, which matters once a shared scope has many writers, m33.3),
// `messageId`, and `ts` (unix millis). Fields already present are preserved (idempotent — a replay
// keeps its original id/agent). A non-message entry (any other JSON) is stored verbatim, so the
// memory plane stays a general log and OLD opaque entries keep round-tripping (read-old/write-new).
func attributeEntry(entry json.RawMessage, agent, messageID string, now time.Time) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(entry, &obj); err != nil || obj == nil {
		return entry // not an object — verbatim
	}
	role, hasRole := obj["role"]
	content, hasContent := obj["content"]
	if !hasRole || !hasContent || !isJSONString(role) || !isJSONString(content) {
		return entry // not a message — verbatim
	}
	if _, ok := obj["agent"]; !ok {
		obj["agent"] = mustJSON(agent)
	}
	if _, ok := obj["messageId"]; !ok {
		obj["messageId"] = mustJSON(messageID)
	}
	if _, ok := obj["ts"]; !ok {
		obj["ts"] = mustJSON(now.UnixMilli())
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return entry // never fail the write on a re-marshal hiccup
	}
	return out
}

func isJSONString(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil
}

// newMessageID mints a short random per-hop message id (hex). crypto/rand failure is astronomically
// unlikely; on the off-chance, fall back to a timestamp so attribution still has a value.
func newMessageID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("m-%d", time.Now().UnixNano())
	}
	return "m-" + hex.EncodeToString(b[:])
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// key builds the full Redis key for a conversation. convID is validated by the
// caller (validateConversationID) before this runs.
func (m *memoryServer) key(convID string) string {
	return m.prefix + convID
}

// handler builds the HTTP mux for the memory endpoints. Go 1.22+ pattern
// routing gives us the {conversationId} path variable and per-method matching
// for free, so a single mux covers the whole contract.
func (m *memoryServer) handler() http.Handler {
	mux := http.NewServeMux()
	// /healthz reports only that the listener is up — the backend is NOT probed
	// (state-layer.md: healthz "200 when the listener is up (backend NOT
	// probed)"). A cheap, dependency-free liveness signal.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Session/shared conversation memory. When STATELAYER_PROXY_URL is set (m.forward), REVERSE-PROXY
	// these routes to the control-plane state-layer proxy (ADR 0050 §8 — the launcher holds no Valkey
	// path); else serve them from the local direct-Valkey store (unchanged). An agent may run with ONLY
	// long-term memory (neither store nor forward), so both are guarded.
	switch {
	case m.forward != nil:
		mux.Handle("GET /memory/{conversationId}", m.forward)
		mux.Handle("PUT /memory/{conversationId}", m.forward)
		mux.Handle("POST /memory/{conversationId}/append", m.forward)
		mux.Handle("GET /memory/{conversationId}/search", m.forward)
	case m.store != nil:
		mux.HandleFunc("GET /memory/{conversationId}", m.traced("get", m.handleGet))
		mux.HandleFunc("PUT /memory/{conversationId}", m.traced("put", m.handlePut))
		mux.HandleFunc("POST /memory/{conversationId}/append", m.traced("append", m.handleAppend))
		mux.HandleFunc("GET /memory/{conversationId}/search", m.traced("search", m.handleSearch))
	}
	// Long-term (agent-scope) memory (ADR 0045) — proxied to the token-service.
	if m.longTerm != nil {
		m.longTerm.register(mux)
	}
	// Managed-RAG retrieval (ADR 0061 Fork 3) — proxied to the token-service on the same listener.
	if m.knowledge != nil {
		m.knowledge.register(mux)
	}
	return mux
}

// memHandlerFunc is the shape of a memory endpoint handler once the traced
// wrapper has opened the span and validated the conversation id.
type memHandlerFunc func(ctx context.Context, span trace.Span, convID string, w http.ResponseWriter, r *http.Request)

// traced wraps a memory handler with the per-op span lifecycle shared by every
// endpoint: it opens the memory.<op> span, stamps conversation.id up front and
// latency_ms on the way out (deferred, so latency is recorded on error paths
// too — the spec requires a span with status Error even when the op fails),
// and rejects invalid conversation ids before they can reach a Redis key or
// span attribute downstream.
func (m *memoryServer) traced(op string, fn memHandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		convID := r.PathValue("conversationId")

		ctx, span := m.tracer.Start(r.Context(), "memory."+op)
		// LIFO defers: latency is stamped before End() exports the span.
		defer span.End()
		start := time.Now()
		defer func() {
			span.SetAttributes(attribute.Int64("latency_ms", time.Since(start).Milliseconds()))
		}()

		span.SetAttributes(attribute.String("conversation.id", convID))

		if err := validateConversationID(convID); err != nil {
			span.SetStatus(codes.Error, err.Error())
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		fn(ctx, span, convID, w, r)
	}
}

// validateConversationID enforces that the caller-supplied id is short and
// path/key-safe. It lands in Redis keys and span attributes, so control
// characters, path separators, and Redis-key structure chars are rejected.
func validateConversationID(id string) error {
	if id == "" {
		return errors.New("conversationId is required")
	}
	if len(id) > maxConversationID {
		return fmt.Errorf("conversationId too long (max %d)", maxConversationID)
	}
	for _, r := range id {
		// Printable, no whitespace, no path/key separators. Keeping this an
		// allow-ish denylist of structural chars is enough: the id is opaque to
		// us but must not break key layout (":" / "/") or path routing.
		if r < 0x20 || r == 0x7f {
			return errors.New("conversationId contains control characters")
		}
		switch r {
		case '/', ':', ' ', '\t', '\n', '\r':
			return fmt.Errorf("conversationId contains disallowed character %q", r)
		}
	}
	return nil
}

// writeJSONError writes a JSON {"error": msg} body with the given status. It is
// the single error shape across the endpoint (best-effort contract).
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encode a fixed-shape struct so msg is JSON-escaped correctly.
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg})
}

// writeJSON writes v as a JSON body with status 200.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Marshal first so a rare encode failure does not leave a half-written
	// body under a 200 header.
	b, err := json.Marshal(v)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "encode response: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// backendError records the error on the span and writes a 502 — the Valkey
// backend is unreachable/erroring and the agent must treat memory as
// best-effort (state-layer.md §"Backend down").
func backendError(w http.ResponseWriter, span trace.Span, err error) {
	span.SetStatus(codes.Error, err.Error())
	writeJSONError(w, http.StatusBadGateway, "memory backend: "+err.Error())
}

func (m *memoryServer) handleGet(
	ctx context.Context, span trace.Span, convID string, w http.ResponseWriter, _ *http.Request,
) {
	opCtx, cancel := context.WithTimeout(ctx, memoryOpTimeout)
	defer cancel()

	entries, err := m.store.Get(opCtx, m.key(convID))
	if err != nil {
		backendError(w, span, err)
		return
	}
	span.SetAttributes(attribute.Int("memory.entries", len(entries)))
	// Missing key → empty list → serialize as [] (state-layer.md: "empty array
	// if none"). The store always returns a non-nil zero-len slice for a
	// missing key, and json.Marshal of that is "[]"; guard defensively anyway.
	if entries == nil {
		entries = []json.RawMessage{}
	}
	// The ETag is the conversation version (the entry count, ADR 0036 m33.2). A subsequent
	// If-Match PUT uses it for a compare-and-set replace, so a stale rewrite can't clobber a
	// concurrent append.
	w.Header().Set("ETag", strconv.Itoa(len(entries)))
	writeJSON(w, entries)
}

func (m *memoryServer) handlePut(
	ctx context.Context, span trace.Span, convID string, w http.ResponseWriter, r *http.Request,
) {
	body, err := readCappedBody(w, r)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		writeJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}

	// The PUT body MUST be a JSON array; each element is stored as one entry.
	var entries []json.RawMessage
	if err := json.Unmarshal(body, &entries); err != nil {
		span.SetStatus(codes.Error, err.Error())
		writeJSONError(w, http.StatusBadRequest, "body must be a JSON array: "+err.Error())
		return
	}

	opCtx, cancel := context.WithTimeout(ctx, memoryOpTimeout)
	defer cancel()

	// Optimistic-concurrency replace (ADR 0036, m33.2): an If-Match header (the version from a prior
	// GET's ETag) makes this a compare-and-set — a stale rewrite (the version moved under us, e.g. a
	// concurrent append) is a 412, so the caller re-reads + retries rather than silently clobbering.
	// No If-Match keeps the legacy unconditional replace (last-writer-wins) for a simple full set.
	if ifMatch := strings.TrimSpace(r.Header.Get("If-Match")); ifMatch != "" {
		expected, convErr := strconv.Atoi(strings.Trim(ifMatch, `"`))
		if convErr != nil {
			span.SetStatus(codes.Error, "bad If-Match")
			writeJSONError(w, http.StatusBadRequest, "If-Match must be an integer version (a prior ETag)")
			return
		}
		newVersion, conflict, repErr := m.store.ReplaceIfVersion(opCtx, m.key(convID), entries, expected, memoryTTL)
		if repErr != nil {
			backendError(w, span, repErr)
			return
		}
		if conflict {
			span.SetAttributes(attribute.Bool("memory.conflict", true))
			writeJSONError(w, http.StatusPreconditionFailed,
				"version conflict: the conversation changed since your read — re-read and retry")
			return
		}
		w.Header().Set("ETag", strconv.Itoa(newVersion))
		span.SetAttributes(attribute.Int("memory.entries", len(entries)))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := m.store.Replace(opCtx, m.key(convID), entries, memoryTTL); err != nil {
		backendError(w, span, err)
		return
	}
	w.Header().Set("ETag", strconv.Itoa(len(entries)))
	span.SetAttributes(attribute.Int("memory.entries", len(entries)))
	w.WriteHeader(http.StatusNoContent)
}

func (m *memoryServer) handleAppend(
	ctx context.Context, span trace.Span, convID string, w http.ResponseWriter, r *http.Request,
) {
	body, err := readCappedBody(w, r)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		writeJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}

	// The append body MUST be a single JSON value (object, array, string,
	// number, bool, or null). It is stored verbatim as one entry, compacted so
	// stored entries are canonical and substring search is predictable.
	// json.Compact rejects invalid JSON, so it doubles as the validator.
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		span.SetStatus(codes.Error, err.Error())
		writeJSONError(w, http.StatusBadRequest, "body must be a single valid JSON value: "+err.Error())
		return
	}
	// Stamp server-authoritative attribution on a message entry (m33.1): the per-hop messageId
	// (ADR 0035) rides X-Message-Id when the caller/A2A sets it, else the launcher mints one.
	messageID := strings.TrimSpace(r.Header.Get(messageIDHeader))
	if messageID == "" {
		messageID = newMessageID()
	}
	entry := attributeEntry(json.RawMessage(compact.Bytes()), m.agent, messageID, time.Now())

	opCtx, cancel := context.WithTimeout(ctx, memoryOpTimeout)
	defer cancel()

	n, err := m.store.Append(opCtx, m.key(convID), entry, memoryTTL)
	if err != nil {
		backendError(w, span, err)
		return
	}
	span.SetAttributes(attribute.Int("memory.entries", n))
	w.WriteHeader(http.StatusNoContent)
}

func (m *memoryServer) handleSearch(
	ctx context.Context, span trace.Span, convID string, w http.ResponseWriter, r *http.Request,
) {
	q := r.URL.Query().Get("q")

	opCtx, cancel := context.WithTimeout(ctx, memoryOpTimeout)
	defer cancel()

	entries, err := m.store.Get(opCtx, m.key(convID))
	if err != nil {
		backendError(w, span, err)
		return
	}

	// v1 search = naive substring match over the serialized entry
	// (state-layer.md: "naive substring match over serialized entries;
	// documented dev-grade"). An empty q matches everything (returns the full
	// context) — a caller wanting nothing simply omits the call.
	matches := make([]json.RawMessage, 0, len(entries))
	for _, e := range entries {
		if q == "" || strings.Contains(string(e), q) {
			matches = append(matches, e)
		}
	}
	span.SetAttributes(attribute.Int("memory.entries", len(matches)))
	writeJSON(w, matches)
}

// readCappedBody reads the request body under a 1MiB cap via MaxBytesReader.
// Exceeding the cap yields an error the caller maps to 413.
func readCappedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMemoryBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, fmt.Errorf("request body exceeds %d-byte limit", maxMemoryBody)
		}
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}
