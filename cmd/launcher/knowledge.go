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
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Managed-RAG retrieval (ADR 0061 Fork 3 + governance #8): the launcher exposes knowledge.search that PROXIES to
// the token-service (the sole holder of the pgvector knowledge store + CONTROLPLANE_DSN — agent pods hold no DB
// creds). This mirrors longTermProxy (memory_longterm.go): the READ goes via this proxy over the same trusted
// (mTLS + NetworkPolicy) channel; the WRITE path is the run-worker holding the store directly (m68.6). The proxy
// forwards the caller's request synchronously and returns the token-service's ranked, provenance-carrying results.
//
// The corpus is org-wide in v1 (subject ""), so this proxy does NOT resolve a per-user subject — a per-user KB
// (m68.8+) will reuse longTermProxy.subjectFor's verified-capability discipline before it is switched on. No raw
// user id is ever trusted from the request.
// enabledTrue is the truthy value the controller stamps on the *_ENABLED feature gates.
const enabledTrue = "true"

// Forwarded-payload JSON keys — they MUST match the token-service knowledgeSearchRequest tags
// (internal/credplane/knowledge.go): the launcher builds this request, the token-service decodes it.
const (
	wireKeyNamespace      = "namespace"
	wireKeyEmbeddingModel = "embeddingModel"
	wireKeySubject        = "subject"
)

type knowledgeProxy struct {
	tokenServiceURL string
	namespace       string
	client          *http.Client
	tracer          trace.Tracer
	logf            func(string, ...any)
}

// newKnowledgeProxy builds the proxy from the controller-injected env, or nil when managed RAG is off
// (KNOWLEDGE_BASE_ENABLED not "true") or misconfigured (no token-service URL) — a visible no-op, never a panic
// (mirrors newLongTermProxy). Reuses TOKEN_SERVICE_URL / MEMORY_KEY_NAMESPACE / POD_NAMESPACE the platform already
// injects for the long-term-memory proxy on the same :2998 listener.
func newKnowledgeProxy(logf func(string, ...any), tracer trace.Tracer) *knowledgeProxy {
	if strings.TrimSpace(os.Getenv("KNOWLEDGE_BASE_ENABLED")) != enabledTrue {
		return nil
	}
	tsURL := strings.TrimSpace(os.Getenv("TOKEN_SERVICE_URL"))
	if tsURL == "" {
		logf("launcher: knowledge base enabled but TOKEN_SERVICE_URL unset — disabling")
		return nil
	}
	ns := strings.TrimSpace(os.Getenv("MEMORY_KEY_NAMESPACE"))
	if ns == "" {
		ns = strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	}
	return &knowledgeProxy{
		tokenServiceURL: strings.TrimRight(tsURL, "/"),
		namespace:       ns,
		client:          &http.Client{Timeout: 30 * time.Second},
		tracer:          tracer,
		logf:            logf,
	}
}

// register wires the retrieval endpoint onto the memory mux (the same :2998 server as /memory/agent/search).
func (p *knowledgeProxy) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /knowledge/search", p.handleSearch)
}

// knowledgeSearchBody is the launcher-facing request. In m68.7 the caller (the SDK / synthetic tool, m68.9)
// supplies both KnowledgeBase and EmbeddingModel.
//
// SEAM (m68.8): the launcher will fill EmbeddingModel — and gate KnowledgeBase — from its injected KNOWLEDGE_BASES
// roster env (the per-KB {name, embeddingModel} the controller stamps onto the pod), so the SDK can send only
// {knowledgeBase, query}. The launcher must NOT trust a client-chosen model against a corpus that was ingested
// with a different one (the one-way door), and must reject a KnowledgeBase the pod is not granted. Neither the
// roster lookup nor the grant gate is implemented here — this task leaves the field on the wire and the seam open.
type knowledgeSearchBody struct {
	KnowledgeBase  string  `json:"knowledgeBase"`
	Query          string  `json:"query"`
	TopK           int     `json:"topK,omitempty"`
	Threshold      float64 `json:"threshold,omitempty"`
	EmbeddingModel string  `json:"embeddingModel"`
}

// handleSearch accepts {knowledgeBase, query, topK, threshold, embeddingModel} and returns the token-service's
// ranked, provenance-carrying results synchronously, wrapped in a knowledge.search span (ADR 0061 — a wrong answer
// from stale/irrelevant retrieved context must be debuggable in the trace: the KB, query top_k, threshold, hit
// count, and top score are recorded, mirroring the memory.agent_recall span).
func (p *knowledgeProxy) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx, span := p.tracer.Start(r.Context(), "knowledge.search")
	defer span.End()

	var body knowledgeSearchBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.KnowledgeBase) == "" {
		http.Error(w, "knowledgeBase is required", http.StatusBadRequest)
		return
	}
	span.SetAttributes(
		attribute.String("knowledge.base", body.KnowledgeBase),
		attribute.Int("knowledge.top_k", body.TopK),
		attribute.Float64("knowledge.threshold", body.Threshold),
	)
	// v1 corpora are org-wide ⇒ subject "" (never a raw client-supplied user id). See the type doc for the
	// m68.8 per-user seam. embeddingModel comes from the request in m68.7; m68.8 will source it from the roster.
	payload := map[string]any{
		wireKeyNamespace: p.namespace, "knowledgeBase": body.KnowledgeBase, wireKeySubject: "",
		"query": body.Query, "topK": body.TopK, "threshold": body.Threshold, wireKeyEmbeddingModel: body.EmbeddingModel,
	}
	var out json.RawMessage
	if err := p.post(ctx, "/v1/knowledge/search", payload, &out); err != nil {
		span.SetStatus(codes.Error, err.Error())
		p.logf("launcher: knowledge search forward failed: %v", err)
		http.Error(w, "knowledge search failed", http.StatusBadGateway)
		return
	}
	var parsed struct {
		Results []struct {
			Score float64 `json:"score"`
		} `json:"results"`
	}
	_ = json.Unmarshal(out, &parsed)
	span.SetAttributes(attribute.Int("knowledge.hits", len(parsed.Results)))
	if len(parsed.Results) > 0 {
		span.SetAttributes(attribute.Float64("knowledge.top_score", parsed.Results[0].Score))
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// post sends payload to the token-service path and decodes the response body into out. It reuses the shared
// postToTokenService helper (the memory proxy's forwarding contract) so both read proxies keep one wire shape.
func (p *knowledgeProxy) post(ctx context.Context, path string, payload any, out *json.RawMessage) error {
	return postToTokenService(ctx, p.client, p.tokenServiceURL, path, payload, out)
}
