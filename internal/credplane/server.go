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

package credplane

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentmemory"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge"
	"github.com/ctxmesh/ctxmesh/internal/credresolve"
	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// maxRequestBytes bounds a delegation request body (a few short strings).
const maxRequestBytes = 1 << 16

// Server is the central token service's HTTP handler: it wraps a single credresolve
// backend so its cache + singleflight + writeback are GLOBAL across every delegating
// sidecar. It authenticates every caller by a reviewed projected token and acts only for
// the user, namespace and boundary that token and the forwarded run capability establish
// (ADR 0154); nothing in a request body is trusted on its own.
type Server struct {
	resolver credresolve.CredentialResolver
	auth     CallerAuth
	log      logr.Logger
	// Long-term memory (ADR 0045), optional — enabled via WithMemory. nil ⇒ the /v1/memory endpoints
	// answer errCodeUnsupported (started without CONTROLPLANE_DSN / a gateway).
	memStore agentmemory.Store
	embedder Embedder
	// Managed-RAG retrieval (ADR 0061 Fork 3), optional — enabled via WithKnowledge. nil ⇒ the
	// /v1/knowledge endpoint answers errCodeUnsupported. Shares the embedder with the memory endpoints.
	knowledgeStore knowledge.Store
	// Cross-encoder reranker (M140.2, ADR 0117), optional — enabled via WithReranker. nil OR
	// KNOWLEDGE_RERANK != "true" ⇒ retrieval returns the store's fusion order unchanged. An
	// ENHANCEMENT over retrieval: a rerank failure falls back to the store order.
	reranker Reranker
	// LLM query-rewriter (M140.3), optional — enabled via WithRewriter. nil OR
	// KNOWLEDGE_QUERY_REWRITE != "true" ⇒ the raw query is used unchanged. An ENHANCEMENT: a
	// rewrite failure falls back to the original query.
	rewriter QueryRewriter
}

// NewServer builds a Server over the given (single, shared) resolver, authenticating callers with auth.
func NewServer(resolver credresolve.CredentialResolver, auth CallerAuth, log logr.Logger) *Server {
	return &Server{resolver: resolver, auth: auth, log: log}
}

// Handler returns the HTTP mux for the internal API + health probes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathResolve, s.handleResolve)
	mux.HandleFunc(pathRevoke, s.handleRevoke)
	mux.HandleFunc(pathStore, s.handleStore)
	mux.HandleFunc(pathMemoryRemember, s.handleMemoryRemember)
	mux.HandleFunc(pathMemorySearch, s.handleMemorySearch)
	mux.HandleFunc(pathKnowledgeSearch, s.handleKnowledgeSearch)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	c, ok := s.authenticate(w, r, runcap.TokenServicePodAudience)
	if !ok {
		return
	}
	agent := c.agentName()
	if agent == "" {
		s.forbid(w, r, c, "only an agent's egress sidecar may resolve a credential")
		return
	}
	cp, ok := s.capability(w, r)
	if !ok {
		return
	}
	var req resolveRequest
	if !decode(w, r, &req) {
		return
	}
	if why := resolveScopeError(c, agent, cp, req); why != "" {
		s.forbid(w, r, c, why)
		return
	}

	// The user and boundary are the capability's; the namespace is the caller's.
	cred, err := s.resolver.Resolve(r.Context(), c.Namespace, cp.Boundary, req.Server, cp.User)
	switch {
	case err == nil:
		writeJSON(w, resolveResponse{Kind: cred.Kind, Value: cred.Value})
	case errors.Is(err, credresolve.ErrConsentRequired):
		writeJSON(w, resolveResponse{Error: errCodeConsentRequired})
	case errors.Is(err, credresolve.ErrNoCredential):
		writeJSON(w, resolveResponse{Error: errCodeNoCredential})
	default:
		// Log the real cause centrally; return only a stable code (never internals/token).
		s.log.Error(err, "credplane: resolve failed", "server", req.Server, "namespace", req.Namespace)
		writeJSON(w, resolveResponse{Error: errCodeInternal})
	}
}

// resolveScopeError is why a resolve is refused, or "". The capability decides the user and the boundary,
// the reviewed token the namespace, and every name the request carries must agree with them.
func resolveScopeError(c caller, agent string, cp runcap.Capability, req resolveRequest) string {
	switch {
	case req.Namespace != c.Namespace:
		return "the request names another namespace than the caller's"
	case req.UserHash != cp.User:
		return "the request names another user than the run capability"
	case req.Boundary != cp.Boundary:
		return "the request names another boundary than the run capability"
	case actorNamespace(cp) != c.Namespace:
		return "the run capability's agent is in another namespace"
	}
	if ns, _, isRegistry := credresolve.ParseRegistryBoundary(cp.Boundary); isRegistry {
		if ns != c.Namespace {
			return "the run capability's boundary is in another namespace"
		}
		return ""
	}
	ns, standalone, isAgent := credresolve.ParseAgentBoundary(cp.Boundary)
	switch {
	case !isAgent:
		return "the run capability carries no namespaced boundary"
	case ns != c.Namespace:
		return "the run capability's boundary is in another namespace"
	case standalone != agent:
		return "the run capability's boundary is another agent's"
	}
	return ""
}

// platformOnly authenticates a request only a platform caller (the BFF) may make.
func (s *Server) platformOnly(w http.ResponseWriter, r *http.Request) bool {
	c, ok := s.authenticate(w, r, runcap.TokenServicePodAudience)
	if !ok {
		return false
	}
	if !c.platform {
		s.forbid(w, r, c, "only the platform may store or revoke a grant")
		return false
	}
	return true
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.platformOnly(w, r) {
		return
	}
	var req revokeRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.resolver.Revoke(r.Context(), req.Namespace, req.Boundary, req.Server, req.UserHash); err != nil {
		s.log.Error(err, "credplane: revoke failed", "server", req.Server, "namespace", req.Namespace)
		writeJSON(w, revokeResponse{Error: errCodeInternal})
		return
	}
	writeJSON(w, revokeResponse{})
}

func (s *Server) handleStore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.platformOnly(w, r) {
		return
	}
	writer, ok := s.resolver.(credresolve.GrantWriter)
	if !ok {
		// The wrapped backend cannot persist grants (e.g. a read-only resolver) — fail
		// closed with a stable code, never a partial write.
		writeJSON(w, storeResponse{Error: errCodeUnsupported})
		return
	}
	var req storeRequest
	if !decode(w, r, &req) {
		return
	}
	g := credresolve.Grant{
		Tokens:    credresolve.Tokens{AccessToken: req.AccessToken, RefreshToken: req.RefreshToken},
		Config:    credresolve.OAuthConfig{TokenEndpoint: req.TokenEndpoint, ClientID: req.ClientID, RevocationEndpoint: req.RevocationEndpoint},
		ServerURL: req.ServerURL,
	}
	if req.ExpiresAtUnix > 0 {
		g.Tokens.ExpiresAt = time.Unix(req.ExpiresAtUnix, 0)
	}
	if err := writer.StoreGrant(r.Context(), req.Namespace, req.Boundary, req.Server, req.UserHash, g); err != nil {
		s.log.Error(err, "credplane: store failed", "server", req.Server, "namespace", req.Namespace)
		writeJSON(w, storeResponse{Error: errCodeInternal})
		return
	}
	writeJSON(w, storeResponse{})
}

// decode reads a bounded JSON body into v, writing a 400 and returning false on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	return true
}

// writeJSON writes v as a 200 JSON response. The RPC always answers 200 — a SEMANTIC error
// (consent_required / no_credential / internal) is carried in the body's error field, so the
// client distinguishes "the RPC failed" (non-200 / transport) from "the answer is an error".
func writeJSON(w http.ResponseWriter, v any) {
	// Marshal FIRST so an encode failure (e.g. a NaN/Inf score, m52.G7) surfaces as a visible 500 instead
	// of a silent 200 + empty body — the latter reads as success to the caller and is undebuggable (it took
	// a direct probe to find the knowledge-search 200/0-bytes). Only write the 200 once we have bytes.
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"internal: response encode failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
