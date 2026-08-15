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

	"github.com/ctxmesh/agent-engine/internal/runcap"
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
//
// Authz gate (m68.8, ADR 0061 Fork 3): the KNOWLEDGE_BASES roster (controller-injected env, un-forgeable) is the
// trust boundary — exactly like DELEGATE_ROSTER (delegate.go). The launcher verifies that the requested
// knowledgeBase is in the roster before forwarding; a model/SDK cannot forge KB membership. The roster also
// carries the per-KB embeddingRoute (the one-way door #1 from ADR 0061) so the SDK sends only {kb, query}
// and the launcher fills embeddingModel from the roster.

// enabledTrue is the truthy value the controller stamps on the *_ENABLED feature gates.
const enabledTrue = "true"

// Per-user KB subject-resolution errors (fail-closed — a per-user corpus is never searched under subject "").
// Kept as sentinels so the handler maps them to a single honest 4xx without leaking capability internals.
var (
	errKBPerUserNoVerifier    = errKBString("per-user knowledge base needs a capability verifier (MCP_CAPABILITY_PUBLIC_KEY unset)") //nolint:lll // a single clear operator-facing message
	errKBPerUserNoCapability  = errKBString("per-user knowledge base needs the run capability (" + runcap.HeaderName + ")")
	errKBPerUserBadCapability = errKBString("run capability verification failed")
	errKBPerUserNoUser        = errKBString("run capability carries no user identity")
)

type errKBString string

func (e errKBString) Error() string { return string(e) }

// Forwarded-payload JSON keys — they MUST match the token-service knowledgeSearchRequest tags
// (internal/credplane/knowledge.go): the launcher builds this request, the token-service decodes it.
const (
	wireKeyNamespace      = "namespace"
	wireKeyEmbeddingModel = "embeddingModel"
	wireKeySubject        = "subject"
)

// kbRosterEntry is the per-KB wire shape in the KNOWLEDGE_BASES env — matches the JSON the controller
// stamps from kbRosterEntry in knowledge_resolve.go. Kept as a local type here (the launcher package does
// not import the controller package — it reads the env at runtime). The controller may stamp additional
// fields the launcher does not consult (e.g. autoInject, ADR 0061 governance #5 / M10 — an SDK-side flag
// the in-pod SDK reads to decide auto-injection); json.Unmarshal ignores such unknown fields, so the gate
// is unaffected.
type kbRosterEntry struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace"`
	EmbeddingRoute string `json:"embeddingRoute"`
	PerUser        bool   `json:"perUser,omitempty"`
}

// kbGrant is the parsed per-KB grant the roster gate consults: the corpus's pinned embedding route
// (one-way door #1) and whether retrieval must be scoped to the invoking user's subject (ADR 0061 Fork 3).
type kbGrant struct {
	embeddingRoute string
	perUser        bool
}

type knowledgeProxy struct {
	tokenServiceURL string
	namespace       string
	// roster is the parsed KNOWLEDGE_BASES env: kbName → grant. It is the un-forgeable
	// membership gate (mirroring DELEGATE_ROSTER) — a model/SDK cannot add entries here.
	// nil means KNOWLEDGE_BASES was unset or unparseable (fail-safe: all KBs refused).
	roster map[string]kbGrant // kb name → {embeddingRoute, perUser}
	// verifier verifies the run capability so a per-user KB's retrieval is scoped to the invoking
	// user's already-hashed identity (the User claim). nil ⇒ per-user KB retrieval is REFUSED (never
	// degraded to org-wide subject "", which would cross-contaminate users) — mirrors longTermProxy.
	verifier *runcap.Verifier
	client   *http.Client
	tracer   trace.Tracer
	logf     func(string, ...any)
}

// parseKnowledgeRoster extracts the knowledge-base roster from the KNOWLEDGE_BASES env value (a JSON array
// of {"name","namespace","embeddingRoute","perUser"} stamped by the controller). Mirrors parseRosterNames
// (delegate.go): a malformed/empty env → nil (an empty roster refuses all KBs — the fail-safe for an
// unconfigured launcher).
func parseKnowledgeRoster(raw string) map[string]kbGrant {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var entries []kbRosterEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	out := make(map[string]kbGrant, len(entries))
	for _, e := range entries {
		if n := strings.TrimSpace(e.Name); n != "" {
			out[n] = kbGrant{embeddingRoute: e.EmbeddingRoute, perUser: e.PerUser}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
	p := &knowledgeProxy{
		tokenServiceURL: strings.TrimRight(tsURL, "/"),
		namespace:       ns,
		roster:          parseKnowledgeRoster(os.Getenv("KNOWLEDGE_BASES")),
		client:          &http.Client{Timeout: 30 * time.Second},
		tracer:          tracer,
		logf:            logf,
	}
	// A verifier is needed only to scope a per-user KB to the invoking user's id — reuse the OBO
	// capability public key + audience the platform already provisions (mirrors newLongTermProxy).
	// Without it, a per-user KB search is refused (never degraded to org-wide, which would leak
	// across users); org-wide KBs are unaffected.
	if pubB64 := strings.TrimSpace(os.Getenv("MCP_CAPABILITY_PUBLIC_KEY")); pubB64 != "" {
		if pub, err := runcap.DecodePublicKey(pubB64); err == nil {
			p.verifier = runcap.NewVerifier(pub, strings.TrimSpace(os.Getenv("MCP_CAPABILITY_AUDIENCE")), nil)
		} else {
			logf("launcher: knowledge: bad MCP_CAPABILITY_PUBLIC_KEY (%v) — per-user KB retrieval refused", err)
		}
	}
	return p
}

// subjectFor resolves the store subject for a KB search: "" for an org-wide corpus, or the invoking user's
// already-hashed identity (from the verified run capability's User claim) for a per-user corpus. It returns an
// error the handler maps to a 4xx when per-user cannot be satisfied — fail-CLOSED, never a fall back to "" (which
// would let one user retrieve another's chunks). This is the exact discipline longTermProxy.subjectFor uses for
// per-user memory, so a user's KB chunks (stamped at ingest with userGrantHash) are retrieved under the SAME hash.
func (p *knowledgeProxy) subjectFor(r *http.Request, perUser bool) (string, error) {
	if !perUser {
		return "", nil
	}
	if p.verifier == nil {
		return "", errKBPerUserNoVerifier
	}
	token := r.Header.Get(runcap.HeaderName)
	if token == "" {
		return "", errKBPerUserNoCapability
	}
	c, err := p.verifier.Verify(token)
	if err != nil {
		return "", errKBPerUserBadCapability
	}
	if c.User == "" {
		return "", errKBPerUserNoUser
	}
	return c.User, nil
}

// register wires the retrieval endpoint onto the memory mux (the same :2998 server as /memory/agent/search).
func (p *knowledgeProxy) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /knowledge/search", p.handleSearch)
}

// knowledgeSearchBody is the launcher-facing request (m68.8 seam closed).
// The SDK / synthetic tool (m68.9) sends only {knowledgeBase, query, topK, threshold}; the launcher
// fills embeddingModel from the roster (the one-way door #1 guarantee: the corpus's model is pinned
// by the controller, not chosen by the SDK). An explicit embeddingModel in the request is accepted as
// an override ONLY when the roster lookup returns an empty embeddingRoute (rare, future-proof) — if the
// roster carries a non-empty embeddingRoute it always wins (the controller stamped the corpus's model).
type knowledgeSearchBody struct {
	KnowledgeBase  string  `json:"knowledgeBase"`
	Query          string  `json:"query"`
	TopK           int     `json:"topK,omitempty"`
	Threshold      float64 `json:"threshold,omitempty"`
	EmbeddingModel string  `json:"embeddingModel,omitempty"` // request override; roster wins when non-empty
}

// handleSearch accepts {knowledgeBase, query, topK, threshold} and returns the token-service's
// ranked, provenance-carrying results synchronously, wrapped in a knowledge.search span (ADR 0061 — a wrong answer
// from stale/irrelevant retrieved context must be debuggable in the trace: the KB, query top_k, threshold, hit
// count, and top score are recorded, mirroring the memory.agent_recall span).
//
// Roster gate (m68.8): the requested knowledgeBase MUST be in the injected KNOWLEDGE_BASES roster.
// An ungranted or misspelled KB → 403 Forbidden. An empty roster → 403 for all (fail-safe, mirrors
// inRoster's empty-roster behaviour in handoff.go — an unconfigured launcher admits nothing).
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

	// ── Roster gate (the un-forgeable trust boundary) ───────────────────────────────────────────
	// Mirror of inRoster (handoff.go): an EMPTY roster refuses all KBs — a launcher without a
	// KNOWLEDGE_BASES env is misconfigured, and the fail-safe is to refuse rather than forward
	// unscoped queries. This differs from the DELEGATE_ROSTER empty-admits-all semantics (handoff.go
	// line 164) because KBs are additive capabilities, not team-roster membership: an empty KB
	// roster means the controller injected no grants, so no KB is accessible. We explicitly chose
	// fail-safe here over the delegate's fail-open convenience.
	grant, granted := p.roster[body.KnowledgeBase]
	if !granted {
		http.Error(w, "knowledge base not granted to this agent", http.StatusForbidden)
		return
	}
	// Fill embeddingModel from the roster (the one-way door #1 guarantee). The roster carries the
	// model the corpus was ingested with; using any other model would yield plausible-wrong results.
	// An explicit request embeddingModel is accepted only when the roster entry is empty (edge case).
	embeddingModel := grant.embeddingRoute
	if embeddingModel == "" {
		embeddingModel = body.EmbeddingModel // fallback to request override (empty roster entry only)
	}

	// Scope the search subject: org-wide corpora ⇒ subject "" (never a client-supplied user id); a per-user
	// corpus ⇒ the invoking user's already-hashed identity from the verified run capability (ADR 0061 Fork 3).
	// Fail-CLOSED — a per-user KB whose subject cannot be resolved is refused, never searched under "".
	subject, err := p.subjectFor(r, grant.perUser)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	span.SetAttributes(
		attribute.String("knowledge.base", body.KnowledgeBase),
		attribute.Int("knowledge.top_k", body.TopK),
		attribute.Float64("knowledge.threshold", body.Threshold),
		attribute.Bool("knowledge.per_user", grant.perUser),
	)
	payload := map[string]any{
		wireKeyNamespace: p.namespace, "knowledgeBase": body.KnowledgeBase, wireKeySubject: subject,
		"query": body.Query, "topK": body.TopK, "threshold": body.Threshold, wireKeyEmbeddingModel: embeddingModel,
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
			Score       float64 `json:"score"`
			DocumentRef string  `json:"documentRef"`
		} `json:"results"`
	}
	_ = json.Unmarshal(out, &parsed)
	span.SetAttributes(attribute.Int("knowledge.hits", len(parsed.Results)))
	if len(parsed.Results) > 0 {
		span.SetAttributes(attribute.Float64("knowledge.top_score", parsed.Results[0].Score))
	}
	// document_refs: a bounded, deduplicated list of returned document refs — the M46 mandate so
	// a wrong retrieval is attributable in the trace (ADR 0061 governance #4). PII: doc refs are
	// document identifiers (paths / titles), not user content — the same category as KB name / score.
	// Raw query text is NOT recorded here (mirroring memory.agent_recall's PII discipline).
	seenRefs := make(map[string]bool, len(parsed.Results))
	docRefs := make([]string, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		if r.DocumentRef != "" && !seenRefs[r.DocumentRef] {
			seenRefs[r.DocumentRef] = true
			docRefs = append(docRefs, r.DocumentRef)
		}
	}
	if len(docRefs) > 0 {
		span.SetAttributes(attribute.StringSlice("knowledge.document_refs", docRefs))
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// post sends payload to the token-service path and decodes the response body into out. It reuses the shared
// postToTokenService helper (the memory proxy's forwarding contract) so both read proxies keep one wire shape.
func (p *knowledgeProxy) post(ctx context.Context, path string, payload any, out *json.RawMessage) error {
	return postToTokenService(ctx, p.client, p.tokenServiceURL, path, payload, out)
}
