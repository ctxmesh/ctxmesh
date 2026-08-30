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
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ctxmesh/agentry/internal/runcap"
)

// Long-term memory (ADR 0045): the launcher exposes memory.remember / memory.search_agent that PROXY to the
// token-service (the sole holder of the pgvector store + CONTROLPLANE_DSN — agent pods hold no DB creds). The
// agent's SDK forwards its run capability; for per-user memory the launcher VERIFIES it and uses the invoking
// user's already-hashed identity as the store subject, so one user's memories never reach another's context.
type longTermProxy struct {
	tokenServiceURL string
	namespace       string
	agent           string
	scope           string // agentmemory.ScopeAgent | ScopeAgentUser
	embeddingModel  string
	verifier        *runcap.Verifier // nil ⇒ per-user memory is refused (no way to trust the user id)
	client          *http.Client
	tracer          trace.Tracer
	logf            func(string, ...any)
}

const (
	scopeAgent     = "agent"
	scopeAgentUser = "agent_user"
)

// newLongTermProxy builds the proxy from the controller-injected env, or nil when long-term memory is off
// (MEMORY_LONGTERM_ENABLED unset) or misconfigured (no token-service URL) — a visible no-op, never a panic.
func newLongTermProxy(logf func(string, ...any), tracer trace.Tracer) *longTermProxy {
	if strings.TrimSpace(os.Getenv("MEMORY_LONGTERM_ENABLED")) != "true" {
		return nil
	}
	tsURL := strings.TrimSpace(os.Getenv("TOKEN_SERVICE_URL"))
	if tsURL == "" {
		logf("launcher: long-term memory enabled but TOKEN_SERVICE_URL unset — disabling")
		return nil
	}
	scope := strings.TrimSpace(os.Getenv("MEMORY_LONGTERM_SCOPE"))
	if scope == "" {
		scope = scopeAgent
	}
	ns := strings.TrimSpace(os.Getenv("MEMORY_KEY_NAMESPACE"))
	if ns == "" {
		ns = strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	}
	p := &longTermProxy{
		tokenServiceURL: strings.TrimRight(tsURL, "/"),
		namespace:       ns,
		agent:           strings.TrimSpace(os.Getenv("AGENT_NAME")),
		scope:           scope,
		embeddingModel:  strings.TrimSpace(os.Getenv("EMBEDDING_ROUTE")),
		client:          &http.Client{Timeout: 30 * time.Second, CheckRedirect: refuseRedirect},
		tracer:          tracer,
		logf:            logf,
	}
	// A verifier is needed only for per-user scope (to trust the invoking user's id). Reuse the OBO
	// capability public key + audience the platform already provisions.
	if pubB64 := strings.TrimSpace(os.Getenv("MCP_CAPABILITY_PUBLIC_KEY")); pubB64 != "" {
		if pub, err := runcap.DecodePublicKey(pubB64); err == nil {
			p.verifier = runcap.NewVerifier(pub, strings.TrimSpace(os.Getenv("MCP_CAPABILITY_AUDIENCE")), nil)
		} else {
			logf("launcher: long-term memory: bad MCP_CAPABILITY_PUBLIC_KEY (%v) — per-user memory refused", err)
		}
	}
	return p
}

// register wires the two long-term endpoints onto the memory mux.
func (p *longTermProxy) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /memory/agent/remember", p.handleRemember)
	mux.HandleFunc("POST /memory/agent/search", p.handleSearch)
}

// subjectFor resolves the store subject: "" for agent-wide, or the invoking user's hashed id (from the
// verified run capability) for per-user. Returns an error the handler maps to a 4xx when per-user can't be
// satisfied — fail-closed, never fall back to agent-wide (that would cross-contaminate users).
func (p *longTermProxy) subjectFor(r *http.Request) (string, error) {
	if p.scope != scopeAgentUser {
		return "", nil
	}
	if p.verifier == nil {
		return "", fmt.Errorf("per-user long-term memory needs a capability verifier (MCP_CAPABILITY_PUBLIC_KEY unset)")
	}
	token := r.Header.Get(runcap.HeaderName)
	if token == "" {
		return "", fmt.Errorf("per-user long-term memory needs the run capability (%s)", runcap.HeaderName)
	}
	c, err := p.verifier.Verify(token)
	if err != nil {
		return "", fmt.Errorf("run capability verification failed: %w", err)
	}
	if c.User == "" {
		return "", fmt.Errorf("run capability carries no user identity")
	}
	return c.User, nil
}

type rememberBody struct {
	Content string            `json:"content"`
	Tags    map[string]string `json:"tags,omitempty"`
}

// handleRemember accepts {content, tags}, returns 202 immediately, and forwards to the token-service in the
// background (best-effort, mirroring session append — a memory write must never block the agent's turn).
func (p *longTermProxy) handleRemember(w http.ResponseWriter, r *http.Request) {
	subject, err := p.subjectFor(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body rememberBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}
	// Tag the memory with its originating run's trace id (m54.3) — the SDK propagates
	// the active run's W3C traceparent on remember, so the console can back-link each
	// remembered fact to the trace that produced it. Best-effort: no traceparent (a
	// remember outside a traced run) ⇒ no tag, no error.
	tags := body.Tags
	if traceID := traceIDFromTraceparent(r.Header.Get("traceparent")); traceID != "" {
		if tags == nil {
			tags = map[string]string{}
		}
		tags["traceId"] = traceID
	}
	payload := map[string]any{
		"namespace": p.namespace, "agentName": p.agent, "scope": p.scope, "subject": subject,
		"content": body.Content, "tags": tags, "embeddingModel": p.embeddingModel,
	}
	// Fire-and-forget with a detached context so returning 202 does not cancel the embed+store.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.post(ctx, "/v1/memory/remember", payload, nil); err != nil {
			p.logf("launcher: long-term remember forward failed (best-effort): %v", err)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
}

// traceIDFromTraceparent extracts the 32-hex trace-id from a W3C traceparent
// header ("<version>-<trace-id>-<parent-id>-<flags>", e.g. "00-<32hex>-<16hex>-01").
// It returns "" for a malformed/absent header or the all-zero trace id (the OTel
// invalid trace). The trace id equals the Langfuse trace id the console links to
// (both are the OTel trace id).
func traceIDFromTraceparent(tp string) string {
	parts := strings.Split(strings.TrimSpace(tp), "-")
	if len(parts) < 4 {
		return ""
	}
	traceID := parts[1]
	// 32 hex chars, and not the all-zero OTel invalid trace id.
	if len(traceID) != 32 || traceID == strings.Repeat("0", 32) {
		return ""
	}
	if _, err := hex.DecodeString(traceID); err != nil {
		return ""
	}
	return traceID
}

type searchBody struct {
	Query     string  `json:"query"`
	TopK      int     `json:"topK,omitempty"`
	Threshold float32 `json:"threshold,omitempty"`
}

// handleSearch accepts {query, topK, threshold} and returns the token-service's ranked results synchronously,
// wrapped in a memory.agent_recall span (ADR 0045 — a wrong answer from stale/irrelevant retrieved context
// must be debuggable in the trace: the query, scope, hit count, top score, and threshold are all recorded).
func (p *longTermProxy) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx, span := p.tracer.Start(r.Context(), "memory.agent_recall")
	defer span.End()

	subject, err := p.subjectFor(r)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body searchBody
	if !decodeJSON(w, r, &body) {
		return
	}
	span.SetAttributes(
		attribute.String("memory.scope", p.scope),
		attribute.Int("memory.top_k", body.TopK),
		attribute.Float64("memory.threshold", float64(body.Threshold)),
	)
	payload := map[string]any{
		"namespace": p.namespace, "agentName": p.agent, "scope": p.scope, "subject": subject,
		"query": body.Query, "topK": body.TopK, "threshold": body.Threshold, "embeddingModel": p.embeddingModel,
	}
	var out json.RawMessage
	if err := p.post(ctx, "/v1/memory/search", payload, &out); err != nil {
		span.SetStatus(codes.Error, err.Error())
		p.logf("launcher: long-term search forward failed: %v", err)
		http.Error(w, "long-term memory search failed", http.StatusBadGateway)
		return
	}
	var parsed struct {
		Results []struct {
			Score float32 `json:"score"`
		} `json:"results"`
	}
	_ = json.Unmarshal(out, &parsed)
	span.SetAttributes(attribute.Int("memory.hits", len(parsed.Results)))
	if len(parsed.Results) > 0 {
		span.SetAttributes(attribute.Float64("memory.top_score", float64(parsed.Results[0].Score)))
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// post sends payload to the token-service path, optionally decoding the response body into out.
func (p *longTermProxy) post(ctx context.Context, path string, payload any, out *json.RawMessage) error {
	return postToTokenService(ctx, p.client, p.tokenServiceURL, path, payload, out)
}

// postToTokenService POSTs a JSON payload to baseURL+path over client and, on a 200, optionally copies the
// (bounded) response body into out. It is the ONE forwarding contract the launcher's token-service read proxies
// share (long-term memory + managed-RAG knowledge), so both surfaces marshal, header, bound, and error identically
// — a non-200 is a returned error (never swallowed), and out is untouched on failure.
func postToTokenService(
	ctx context.Context, client *http.Client, baseURL, path string, payload any, out *json.RawMessage,
) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call token-service: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token-service %s status %d", path, resp.StatusCode)
	}
	if out != nil {
		*out = raw
	}
	return nil
}

// decodeJSON reads a bounded JSON body into v; writes a 400 + returns false on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || json.Unmarshal(raw, v) != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}
