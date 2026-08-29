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

package bff

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/auditlog"
	"github.com/ctxmesh/agent-engine/internal/controlplane/sharedrun"
	"github.com/ctxmesh/agent-engine/internal/run"
)

// Share-link constants (M75, m75.1, ADR 0069 §1).
const (
	// shareTokenBytes is the raw entropy of a share token: 32 bytes = 256 bits from crypto/rand,
	// base64url-encoded to ~43 chars. The token is returned ONCE at creation and never stored (only its
	// SHA-256 hash is persisted — a DB dump cannot mint live links).
	shareTokenBytes = 32
	// shareIDBytes is the raw entropy of the PUBLIC share id (the manage/revoke URL segment). Distinct
	// from the token so listing/revoking never handles the secret token material.
	shareIDBytes = 16

	// defaultShareTTLHours / maxShareTTLHours bound a share link's lifetime (ADR 0069 §1: default 7d, a
	// hard max of 90d). A caller ttlHours ≤ 0 defaults; a value over the cap is clamped to the cap.
	defaultShareTTLHours = 168  // 7 days
	maxShareTTLHours     = 2160 // 90 days

	// auditActionShareCreate / auditActionShareRevoke are the BFF audit actions for the share paper trail
	// (ADR 0069 §1: "who shared this?"). The token is NEVER in an audit row — only the run id + share id.
	auditActionShareCreate = "share.create"
	auditActionShareRevoke = "share.revoke"

	// auditKindSharedRun is the audit resource kind for a share event.
	auditKindSharedRun = "SharedRun"

	// errorCategory* are the COARSE, non-revealing error buckets the public projection surfaces instead of
	// the raw run error (ADR 0069 §2 — the raw string can echo input fragments / provider bodies).
	errorCategoryTimeout    = "timeout"
	errorCategoryCancelled  = "cancelled"
	errorCategoryGuardrail  = "guardrail"
	errorCategoryValidation = "validation"
	errorCategoryOther      = "error"

	// sharedRunNotFoundMsg is the SINGLE, non-revealing body the public read returns for EVERY failure —
	// missing token, malformed token, no row, revoked, expired, deleted run, and a not-configured store all
	// return this exact 404 (ADR 0069 §1/§2: no oracle that distinguishes the failure modes). A store error
	// is the only non-404 (500), and it too never echoes the underlying error.
	sharedRunNotFoundMsg = "shared run not found"

	// sharedRunRatePerIP / sharedRunBurstPerIP bound public-read attempts per client IP (token-bucket
	// hygiene — 256-bit tokens make brute force moot, but the endpoint is unauthenticated so it is not left
	// unbounded). A refill of 5/s with a burst of 20 is generous for a human loading a page and its assets
	// while giving a scanner nothing useful. Over budget → 429 (a non-oracle status: it says nothing about
	// whether the token was valid).
	sharedRunRatePerIP  = 5.0
	sharedRunBurstPerIP = 20.0

	// endUserRatePerIP / endUserBurstPerIP bound the UNAUTHENTICATED end-user surfaces per client IP
	// (M137/EU1c, ADR 0107): the tenant-IdP config probe (GET /api/end-user-auth-config) and the token
	// VERIFICATION entry of the run create + my-runs paths — where an attacker-supplied bearer forces an
	// OIDC verify (signature crypto + a possible JWKS refetch) before any identity exists. A generous
	// 10/s refill with a 60 burst never throttles real human chat (even dozens of users behind one NAT),
	// while bounding a single-IP flood. Over budget → 429 (a non-oracle status).
	endUserRatePerIP  = 10.0
	endUserBurstPerIP = 60.0

	// endUserCreateRatePerUser / endUserCreateBurstPerUser bound how fast a SINGLE verified end-user
	// identity can spawn runs (M137/EU1c, ADR 0107). Keyed on the end-user PRINCIPAL (oidc:<iss>#<sub>) —
	// NAT-proof, unlike the per-IP guard, so many users behind one IP are never collectively throttled and
	// one identity cannot flood run creation (each run is a durable worker dispatch + tenant spend). A
	// 2/s refill with a 30 burst is generous for human chat + rapid retries. Over budget → 429.
	endUserCreateRatePerUser  = 2.0
	endUserCreateBurstPerUser = 30.0
)

// durableRunStore is the OPTIONAL capability a run.Store implements to declare whether it survives a pod
// restart (M75, m75.1, ADR 0069 §1). The Postgres store answers true; the hot mem store answers false. A
// share into a non-durable store dies on restart (a broken link), so the mint refuses one. Kept as an
// optional interface (type-asserted) so the run.Store seam is not widened for every caller.
type durableRunStore interface {
	Durable() bool
}

// CreateShareRequest is the POST /api/runs/{id}/shares body. includeContent opts the public projection
// into the run's Input + Messages + full Error (ADR 0069 §2); ttlHours overrides the default 7d expiry
// (clamped to the 90d cap). Both optional.
type CreateShareRequest struct {
	IncludeContent bool `json:"includeContent"`
	TTLHours       int  `json:"ttlHours,omitempty"`
}

// CreateShareResponse returns the freshly minted share. The token is present EXACTLY ONCE here and is
// never retrievable again (it is stored only as a SHA-256 hash). URL is a convenience path the console can
// render; the public read route (GET /api/shared/runs/{token}) is m75.2.
type CreateShareResponse struct {
	ID             string    `json:"id"`
	Token          string    `json:"token"`
	URL            string    `json:"url"`
	ExpiresAt      time.Time `json:"expiresAt"`
	IncludeContent bool      `json:"includeContent"`
}

// ShareSummary is the token-FREE manage-list DTO (GET /api/runs/{id}/shares). It NEVER carries the token
// or its hash — only the public id + metadata a manager needs to reason about + revoke a link.
type ShareSummary struct {
	ID             string    `json:"id"`
	CreatedAt      time.Time `json:"createdAt"`
	ExpiresAt      time.Time `json:"expiresAt"`
	Revoked        bool      `json:"revoked"`
	IncludeContent bool      `json:"includeContent"`
}

// registerShareRoutes mounts the caller-scoped mint/revoke/list surface for share links (M75, m75.1,
// ADR 0069 §1) on the authed mux. Like registerRunRoutes it requires the caller-scoped cluster path (the
// mint authorizes the caller against the run's agent through their own client, ADR 0011). Absent the
// cluster path or the store → honest 501. (The UNAUTHENTICATED public read is m75.2, mounted elsewhere.)
func (s *Server) registerShareRoutes(authed *http.ServeMux) {
	if s.callerClients != nil {
		authed.HandleFunc("POST /api/runs/{id}/shares", s.handleCreateShare)
		authed.HandleFunc("GET /api/runs/{id}/shares", s.handleListShares)
		authed.HandleFunc("DELETE /api/runs/{id}/shares/{shareId}", s.handleRevokeShare)
		// V13 (M112): the caller-scoped "my active shares" view — the caller's shares across ALL runs.
		authed.HandleFunc("GET /api/my/shares", s.handleMyShares)
		return
	}
	authed.Handle("POST /api/runs/{id}/shares", notImplemented("run shares"))
	authed.Handle("GET /api/runs/{id}/shares", notImplemented("run shares"))
	authed.Handle("DELETE /api/runs/{id}/shares/{shareId}", notImplemented("run shares"))
	authed.Handle("GET /api/my/shares", notImplemented("run shares"))
}

// authorizeRunAccess is the caller-scoped authorization gate for a single run (ADR 0011). Every user-facing
// run read/mutate — GET a run, cancel it, stream its events, view its trace detail / feedback, and mint /
// list / revoke a public share of it — MUST prove the caller can access that run first, on the caller's OWN
// token (never the BFF SA, which has rules:[]). This is strictly stronger than "presence of a bearer" (the
// edge auth alone never validates the token; only a real K8s API call on the caller token does — see
// auth.go BearerAuthenticator). It returns the resolved run + ok. runID may be a run.ID or a traceId (the
// trace-detail page identifies a run by traceId); both resolve here.
//
// allowOwner selects the policy:
//   - true (run READS/cancel/events/trace/feedback): the run's CREATOR (CallerUsername, resolved via a
//     token-validating SelfSubjectReview) is authorized to read their own run even without current RBAC on
//     the backing resource; a non-creator falls to the RBAC gate (operator-investigation).
//   - false (share MINT/list/revoke): creator-match is NOT sufficient — delegating PUBLIC access requires
//     CURRENT RBAC on the run's backing resource (ADR 0069 §1: an operator with read on the agent may share a
//     run they did not personally create; a former creator who lost access may not). Only the RBAC gate runs.
//
// A forbidden/absent backing resource → 403/404 (uniform with resolveAgent, no oracle on whether the run or
// the backing resource is the missing one).
func (s *Server) authorizeRunAccess(w http.ResponseWriter, r *http.Request, caller client.Client, runID string, allowOwner bool) (*run.Run, bool) {
	// Resolve the run by its internal ID first; if not found, fall back to traceId. The
	// trace-detail page (/traces/:id) identifies a run by traceId, which is DISTINCT from
	// run.ID, so the Share button on that page passes a traceId — not the internal ID. We
	// resolve by either so the mint works in both contexts. The stored shared_runs.run_id is
	// ALWAYS the real run.ID (rn.ID below), never the traceId.
	rn, err := s.runStore.Get(runID)
	if err != nil {
		if !errors.Is(err, run.ErrNotFound) {
			s.log.Error(err, "authorize run access: run store read failed", "id", runID)
			writeError(w, http.StatusInternalServerError, "failed to read run")
			return nil, false
		}
		// Not found by run.ID — try traceId fallback.
		rn, err = s.runStore.GetByTraceID(runID)
		if err != nil {
			s.log.Error(err, "authorize run access: run store trace-id lookup failed", "traceId", runID)
			writeError(w, http.StatusInternalServerError, "failed to read run")
			return nil, false
		}
		if rn == nil {
			writeError(w, http.StatusNotFound, "run not found")
			return nil, false
		}
	}
	// M137/EU1b (ADR 0107 Q3): a verified END-USER is authorized ONLY by ownership — NEVER a K8s client
	// (structural separation: an end-user bearer must never reach a SelfSubjectReview/TokenReview).
	// Classify against the RUN's namespace/tenant, before touching the (lazy, still-inert) caller client:
	// a non-owner — or a token that isn't a valid end-user for the run's tenant — gets a uniform 404 (no
	// oracle). A console token is not a valid end-user here, so it falls through to the K8s path below.
	if principal, _, isEndUser, _ := s.resolveEndUserPrincipal(r.Context(), r, rn.Namespace); isEndUser {
		if allowOwner && rn.CallerUsername != "" && rn.CallerUsername == principal {
			return rn, true
		}
		writeError(w, http.StatusNotFound, "run not found")
		return nil, false
	}

	// Caller-scoped authz (ADR 0011) — two gates, both on the CALLER's own token (never the BFF SA):
	//
	// (1) Ownership. callerUsername issues a SelfSubjectReview, which BOTH validates the token AND yields the
	//     caller identity; a match against the run's persisted CallerUsername authorizes UNIFORMLY for every
	//     run kind — agent, workflow-CR instance, inline (CR-less) workflow, and spawned/handoff sub-runs
	//     (all persist CallerUsername). This is the gate that shuts the cross-tenant-id leak: another
	//     principal's token yields a different username. A genuine token rejection short-circuits to 401.
	//     Any OTHER SelfSubjectReview failure (e.g. a cluster/test client without the virtual resource) is
	//     treated as "identity unknown" and falls THROUGH to (2): ownership is an ADDITIONAL allow, never the
	//     sole boundary — the RBAC Get in (2) is itself a token-validating, authorizing API call.
	//
	// (2) RBAC on the backing resource. A caller who did not create the run may still read it if their OWN
	//     RBAC grants read on the run's backing CR — the operator-investigation path (uniform with resolveAgent:
	//     403 forbidden / 404 absent, no oracle).
	if allowOwner {
		if username, uErr := callerUsername(r.Context(), caller); uErr == nil {
			if rn.CallerUsername != "" && rn.CallerUsername == username {
				return rn, true
			}
		} else if apierrors.IsUnauthorized(uErr) {
			writeError(w, http.StatusUnauthorized, "unauthorized: token rejected by the API server")
			return nil, false
		}
	}
	if !s.callerCanReadRunBacking(w, r, caller, rn) {
		return nil, false
	}
	return rn, true
}

// callerCanReadRunBacking authorizes a NON-creator caller to read a run through their OWN RBAC on the run's
// backing resource (ADR 0011): the Workflow CR for a workflow instance, otherwise the run's AgentDeployment.
// An inline (CR-less) workflow run has no backing CR, so a non-creator is denied (404 — no oracle, uniform
// with a missing run). It writes the error response and returns false on denial/absence.
func (s *Server) callerCanReadRunBacking(w http.ResponseWriter, r *http.Request, caller client.Client, rn *run.Run) bool {
	var (
		obj  client.Object
		name = rn.Agent
	)
	switch {
	case rn.WorkflowRef != "":
		// A workflow instance: authorize against the Workflow CR (rn.Agent is the CR name, but be explicit).
		obj = &agentsv1beta1.Workflow{}
		name = rn.WorkflowRef
	case rn.Agent == inlineWorkflowAgentLabel:
		// CR-less inline workflow plan: no backing resource exists to authorize against, so a non-creator
		// cannot prove access. Fail closed (404, no oracle).
		writeError(w, http.StatusNotFound, "run not found")
		return false
	default:
		obj = &agentsv1alpha1.AgentDeployment{}
	}
	if gErr := caller.Get(r.Context(), client.ObjectKey{Name: name, Namespace: rn.Namespace}, obj); gErr != nil {
		switch {
		case apierrors.IsForbidden(gErr):
			writeError(w, http.StatusForbidden, "forbidden: you do not have access to this run")
		case apierrors.IsUnauthorized(gErr):
			writeError(w, http.StatusUnauthorized, "unauthorized: token rejected by the API server")
		case apierrors.IsNotFound(gErr):
			// The backing agent/workflow is gone — the caller cannot prove access. 404 (uniform with a
			// missing run: no oracle on whether the run or the backing resource is the missing one).
			writeError(w, http.StatusNotFound, "run not found")
		default:
			s.log.Error(gErr, "authorize run access: backing-resource check failed", "run", rn.ID, "agent", rn.Agent, "workflowRef", rn.WorkflowRef, "namespace", rn.Namespace)
			writeError(w, http.StatusInternalServerError, "failed to authorize run access")
		}
		return false
	}
	return true
}

// handleCreateShare serves POST /api/runs/{id}/shares — MINT a single-run capability link (M75, m75.1,
// ADR 0069 §1). It authorizes the caller against the run's agent (caller-scoped, above), refuses a
// non-durable run store, generates a 256-bit token + records ONLY its SHA-256 hash, audit-logs the create
// (never the token), and returns the token EXACTLY ONCE.
func (s *Server) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.sharedRunStore == nil {
		writeError(w, http.StatusNotImplemented, "share store not configured: set CONTROLPLANE_DSN to enable share links")
		return
	}
	runID := r.PathValue("id")

	rn, ok := s.authorizeRunAccess(w, r, caller, runID, false)
	if !ok {
		return
	}

	// Only Postgres-backed runs are shareable: a share into the hot mem store dies on restart, leaving a
	// dead public link (ADR 0069 §1). Detect via the optional durableRunStore capability; a store that
	// does not implement it (or reports non-durable) is refused with a clear 409.
	if ds, isDurable := s.runStore.(durableRunStore); !isDurable || !ds.Durable() {
		writeError(w, http.StatusConflict,
			"this run is not durably stored (set RUN_STORE_DSN): a share link into the hot store would break on restart")
		return
	}

	var req CreateShareRequest
	// The body is optional (an empty POST = metadata-only, default TTL). Decode leniently: an empty body
	// (io.EOF) leaves the zero request; only malformed JSON is a 400.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}

	ttlHours := req.TTLHours
	if ttlHours <= 0 {
		ttlHours = defaultShareTTLHours
	}
	if ttlHours > maxShareTTLHours {
		ttlHours = maxShareTTLHours
	}

	// A 256-bit token from crypto/rand (base64url, ~43 chars) + its SHA-256 hash (the ONLY thing stored)
	// + a random public share id. A crypto/rand failure is a hard 500 (never a weak fallback).
	tokenBytes := make([]byte, shareTokenBytes)
	if _, err := rand.Read(tokenBytes); err != nil {
		s.log.Error(err, "share: could not generate token")
		writeError(w, http.StatusInternalServerError, "failed to generate a share token")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	tokenHash := hashShareToken(token)

	idBytes := make([]byte, shareIDBytes)
	if _, err := rand.Read(idBytes); err != nil {
		s.log.Error(err, "share: could not generate share id")
		writeError(w, http.StatusInternalServerError, "failed to generate a share id")
		return
	}
	shareID := base64.RawURLEncoding.EncodeToString(idBytes)

	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(ttlHours) * time.Hour)

	// The audit actor is the PRECISE authenticated caller (SelfSubjectReview) — an empty identity falls
	// back to "unknown" for the audit row, but we still record who minted the share (the paper trail).
	createdBy := s.auditActor(r.Context(), caller)

	rec := sharedrun.SharedRun{
		ID:             shareID,
		TokenHash:      tokenHash,
		RunID:          rn.ID,
		Namespace:      rn.Namespace,
		Agent:          rn.Agent, // V16: snapshot the agent so "my shares" is recognizable (cross-DB join impossible)
		CreatedBy:      createdBy,
		CreatedAt:      now,
		ExpiresAt:      expiresAt,
		IncludeContent: req.IncludeContent,
	}
	if err := s.sharedRunStore.Create(r.Context(), rec); err != nil {
		s.log.Error(err, "share: could not persist share", "run", rn.ID)
		writeError(w, http.StatusInternalServerError, "failed to create the share link")
		return
	}

	// Audit-log the create AFTER the effect (record-after-effect). The token is NEVER in an audit row —
	// only the run id + share id + includeContent flag (ADR 0069 §1).
	s.appendAudit(r.Context(), auditlog.Entry{
		Actor:        createdBy,
		Action:       auditActionShareCreate,
		ResourceKind: auditKindSharedRun,
		ResourceName: shareID,
		Namespace:    rn.Namespace,
		TraceID:      rn.TraceID,
		Detail: map[string]any{
			"runId":          rn.ID,
			"includeContent": req.IncludeContent,
			"expiresAt":      expiresAt,
		},
	})

	writeJSON(w, http.StatusCreated, CreateShareResponse{
		ID:             shareID,
		Token:          token, // the ONLY time the token is ever returned
		URL:            "/api/shared/runs/" + token,
		ExpiresAt:      expiresAt,
		IncludeContent: req.IncludeContent,
	})
}

// handleListShares serves GET /api/runs/{id}/shares — the manage list (ADR 0069 §1: a public link without
// a kill switch is a worse feature). Caller-scoped authz, then the token-FREE ShareSummary DTOs.
func (s *Server) handleListShares(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.sharedRunStore == nil {
		writeError(w, http.StatusNotImplemented, "share store not configured: set CONTROLPLANE_DSN to enable share links")
		return
	}
	runID := r.PathValue("id")

	rn, ok := s.authorizeRunAccess(w, r, caller, runID, false)
	if !ok {
		return
	}

	recs, err := s.sharedRunStore.ListForRun(r.Context(), rn.ID)
	if err != nil {
		s.log.Error(err, "share: could not list shares", "run", runID)
		writeError(w, http.StatusInternalServerError, "failed to list share links")
		return
	}

	// Project to the token-FREE DTO — the token/hash must NEVER reach the client.
	out := make([]ShareSummary, 0, len(recs))
	for _, rec := range recs {
		out = append(out, ShareSummary{
			ID:             rec.ID,
			CreatedAt:      rec.CreatedAt,
			ExpiresAt:      rec.ExpiresAt,
			Revoked:        rec.Revoked,
			IncludeContent: rec.IncludeContent,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeShare serves DELETE /api/runs/{id}/shares/{shareId} — kill a share link (ADR 0069 §1).
// Caller-scoped authz, an idempotent store.Revoke, and an audit row. Idempotent: revoking an absent /
// already-revoked share is still a 200 (never a 404-oracle on share existence).
func (s *Server) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.sharedRunStore == nil {
		writeError(w, http.StatusNotImplemented, "share store not configured: set CONTROLPLANE_DSN to enable share links")
		return
	}
	runID := r.PathValue("id")
	shareID := r.PathValue("shareId")

	rn, ok := s.authorizeRunAccess(w, r, caller, runID, false)
	if !ok {
		return
	}

	if err := s.sharedRunStore.Revoke(r.Context(), shareID); err != nil {
		s.log.Error(err, "share: could not revoke share", "run", runID, "share", shareID)
		writeError(w, http.StatusInternalServerError, "failed to revoke the share link")
		return
	}

	s.appendAudit(r.Context(), auditlog.Entry{
		Actor:        s.auditActor(r.Context(), caller),
		Action:       auditActionShareRevoke,
		ResourceKind: auditKindSharedRun,
		ResourceName: shareID,
		Namespace:    rn.Namespace,
		TraceID:      rn.TraceID,
		Detail:       map[string]any{"runId": rn.ID},
	})

	w.WriteHeader(http.StatusNoContent)
}

// MySharesItem is the token-FREE "my active shares" DTO (GET /api/my/shares, V13). Unlike ShareSummary
// (the per-run manage list, where the run is the URL), it carries RunID + Namespace so the console can
// group by run and drive the EXISTING per-run revoke (DELETE /api/runs/{runId}/shares/{id}) from one
// place, plus a derived Status (live | revoked | expired) so the UI honestly badges "what did I expose?".
// It NEVER carries the token or its hash.
type MySharesItem struct {
	ID             string    `json:"id"`
	RunID          string    `json:"runId"`
	Namespace      string    `json:"namespace"`
	Agent          string    `json:"agent"` // V16: the run's agent (snapshotted at mint) so the caller recognizes the run
	CreatedAt      time.Time `json:"createdAt"`
	ExpiresAt      time.Time `json:"expiresAt"`
	Status         string    `json:"status"` // "live" | "revoked" | "expired"
	IncludeContent bool      `json:"includeContent"`
}

// shareStatus derives the honest lifecycle status of a share at time now — revoked takes precedence over
// expiry (a link revoked before it expired is "revoked", not "expired").
func shareStatus(rec sharedrun.SharedRun, now time.Time) string {
	switch {
	case rec.Revoked:
		return "revoked"
	case !now.Before(rec.ExpiresAt):
		return "expired"
	default:
		return "live"
	}
}

// handleMyShares serves GET /api/my/shares — the caller-scoped "my active shares" view (V13): every share
// the CALLER minted across all runs, so they can review + revoke their live links from one place. There is
// no single run to authorize against here, so the caller-scoping (ADR 0011) IS the identity: the list is
// keyed on the caller's VALIDATED username (auditActor → a token-validating SelfSubjectReview), the same
// value Create stored as CreatedBy. An unresolved identity ("unknown") is refused (401) so the unattributed
// bucket is never listed — fail-closed, not a cross-principal leak. The token/hash never reaches the client.
func (s *Server) handleMyShares(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.sharedRunStore == nil {
		writeError(w, http.StatusNotImplemented, "share store not configured: set CONTROLPLANE_DSN to enable share links")
		return
	}
	actor := s.auditActor(r.Context(), caller)
	if actor == "" || actor == actorUnknown {
		writeError(w, http.StatusUnauthorized, "could not resolve your identity")
		return
	}

	recs, err := s.sharedRunStore.ListByCreator(r.Context(), actor)
	if err != nil {
		s.log.Error(err, "share: could not list my shares", "actor", actor)
		writeError(w, http.StatusInternalServerError, "failed to list your share links")
		return
	}

	now := time.Now()
	out := make([]MySharesItem, 0, len(recs))
	for _, rec := range recs {
		out = append(out, MySharesItem{
			ID:             rec.ID,
			RunID:          rec.RunID,
			Namespace:      rec.Namespace,
			Agent:          rec.Agent,
			CreatedAt:      rec.CreatedAt,
			ExpiresAt:      rec.ExpiresAt,
			Status:         shareStatus(rec, now),
			IncludeContent: rec.IncludeContent,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// hashShareToken computes the SHA-256 (hex) of a share token — the ONLY representation of the token the
// platform ever persists (ADR 0069 §1). The m75.2 public read hashes the presented token and looks the
// row up by this value.
func hashShareToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------------------------------
// The UNAUTHENTICATED public read (m75.2, ADR 0069 §1/§2).
// ---------------------------------------------------------------------------------------------------
//
// GET /api/shared/runs/{token} is the platform's FIRST genuinely unauthenticated read surface — it is
// mounted on the `api` (UNAUTHED) mux, NOT behind requireAuth, and there is NO caller (writeSharedRunError
// never consults an identity). The security posture is:
//
//   - Uniform 404 at EVERY failure (writeSharedRunError): missing/malformed token, no row, revoked,
//     expired, deleted run, and a not-configured store are indistinguishable — no oracle, no timing tell.
//     A store error is the ONLY non-404 (500), and it too never echoes the underlying error.
//   - The response is ALWAYS the newSharedRunView allowlist projection (m75.1) — the *run.Run is never
//     marshalled directly (ADR 0069 §2). The token presented is hashed with hashShareToken (m75.1) and the
//     row looked up by that hash; the raw token is never stored and never logged.
//   - Security headers (Referrer-Policy: no-referrer, X-Robots-Tag: noindex) keep the in-URL token from
//     leaking via Referer or search indexing.
//   - A per-IP token bucket bounds attempts (thin brute-force hygiene; a valid token is a 256-bit secret).

// writeSharedRunError is the SOLE 404 writer for the public read. Every failure funnels through it so the
// body + status are byte-identical — a caller cannot distinguish "no such token" from "revoked" from
// "expired" from "the run was deleted" (ADR 0069 §1: no oracle). It also sets the no-leak headers so even
// an error response does not leak the token via Referer/indexing.
func writeSharedRunError(w http.ResponseWriter) {
	setSharedRunHeaders(w)
	writeError(w, http.StatusNotFound, sharedRunNotFoundMsg)
}

// setSharedRunHeaders stamps the no-leak headers on a shared-run response (success OR failure): the token
// sits in the URL, so Referrer-Policy: no-referrer stops it leaking via the Referer header on any outbound
// link/asset, and X-Robots-Tag: noindex keeps a crawled link out of a search index (ADR 0069 §2).
func setSharedRunHeaders(w http.ResponseWriter) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex")
}

// handleSharedRunPublic serves GET /api/shared/runs/{token} — the unauthenticated public read (m75.2, ADR
// 0069 §1/§2). It hashes the presented token, looks up the raw row, and returns the newSharedRunView
// allowlist projection ONLY for a live share of an existing run. Every other outcome is a uniform 404.
//
// Nil-store choice (deliberate): with no cpDB the share store is nil, and this returns 404 — NOT 501. An
// unauthenticated caller must not learn the feature exists or is misconfigured; 501 would be an oracle
// ("shares are a thing here, just not wired") on an anonymous surface. 404 == "no such shared run", which
// is exactly the truth from the caller's side.
func (s *Server) handleSharedRunPublic(w http.ResponseWriter, r *http.Request) {
	// Rate-limit BEFORE any store work — a scanner gets a cheap 429, never touches the DB.
	if !s.sharedRunLimiter.allow(clientIP(r)) {
		setSharedRunHeaders(w)
		writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}

	// Nil store (no cpDB) → 404, not 501: never reveal the feature to an anonymous caller.
	if s.sharedRunStore == nil {
		writeSharedRunError(w)
		return
	}

	token := r.PathValue("token")
	if token == "" {
		writeSharedRunError(w)
		return
	}

	// Reuse m75.1's hashing — the token is stored ONLY as this hash (never reimplemented differently).
	tokenHash := hashShareToken(token)

	share, found, err := s.sharedRunStore.GetByTokenHash(r.Context(), tokenHash)
	if err != nil {
		// A store error is a 500, not a 404 (it is not "no such share") — but the error is NEVER echoed to
		// the caller, and only a hash PREFIX (never the raw token) is logged.
		s.log.Error(err, "shared run: lookup failed", "tokenHashPrefix", tokenHashPrefix(tokenHash))
		setSharedRunHeaders(w)
		writeError(w, http.StatusInternalServerError, "failed to read the shared run")
		return
	}
	if !found {
		writeSharedRunError(w) // no row — uniform 404
		return
	}
	if !share.IsLive(time.Now()) {
		writeSharedRunError(w) // revoked or expired — SAME 404 as a missing token (no oracle)
		return
	}

	rn, err := s.runStore.Get(share.RunID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			writeSharedRunError(w) // the run was deleted — a dead link cascades to the SAME 404 (intended)
			return
		}
		s.log.Error(err, "shared run: run store read failed", "tokenHashPrefix", tokenHashPrefix(tokenHash))
		setSharedRunHeaders(w)
		writeError(w, http.StatusInternalServerError, "failed to read the shared run")
		return
	}
	if rn == nil { // defensive: a (nil, nil) store contract still 404s, never panics on the deref below
		writeSharedRunError(w)
		return
	}

	// The SOLE path from a run.Run to the unauthenticated route: the m75.1 allowlist projection, gated by
	// the share's includeContent flag. The run DTO is NEVER marshalled directly (ADR 0069 §2).
	setSharedRunHeaders(w)
	writeJSON(w, http.StatusOK, newSharedRunView(rn, share.IncludeContent))
}

// tokenHashPrefix returns the first 8 hex chars of a token hash for a log line — enough to correlate a
// request in the logs, NEVER enough to reconstruct the token (which is a 256-bit secret and is itself
// never logged). The raw token must never appear in any log.
func tokenHashPrefix(tokenHash string) string {
	if len(tokenHash) < 8 {
		return tokenHash
	}
	return tokenHash[:8]
}

// clientIP extracts the best-effort client IP for the per-IP rate limiter. It prefers the LAST hop of
// X-Forwarded-For when present (the edge appends the real client), else the RemoteAddr host. This is
// rate-limit-only; it is NOT an authorization input (there is no caller to authorize).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------------------------------
// A minimal in-memory per-IP token-bucket limiter (stdlib only — no new dep).
// ---------------------------------------------------------------------------------------------------

// ipRateLimiter is a tiny per-key token-bucket limiter for the unauthenticated public read. It is
// deliberately minimal (a map of buckets refilled lazily on access) — enough to deny a scanner an
// unbounded endpoint without pulling in a dependency. Buckets are kept in a bounded map; when the map
// grows past maxBuckets an admission-time sweep drops FULL (idle) buckets, so a churn of distinct IPs
// cannot grow memory without bound.
type ipRateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*tokenBucket
	rate       float64 // tokens added per second
	burst      float64 // bucket capacity
	maxBuckets int
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// newIPRateLimiter builds a per-IP token-bucket limiter with the given steady-state rate (tokens/sec) and
// burst (capacity). A zero/negative burst disables limiting (allow always) — used so tests and the
// nil-config path never accidentally throttle.
func newIPRateLimiter(rate, burst float64) *ipRateLimiter {
	return &ipRateLimiter{
		buckets:    make(map[string]*tokenBucket),
		rate:       rate,
		burst:      burst,
		maxBuckets: 4096,
	}
}

// allow reports whether a request from key may proceed, consuming one token. It refills the bucket by the
// elapsed time since its last touch (capped at burst) and admits when at least one token remains. A
// disabled limiter (burst <= 0) always admits.
func (l *ipRateLimiter) allow(key string) bool {
	if l == nil || l.burst <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxBuckets {
			l.sweepFullLocked(now)
		}
		l.buckets[key] = &tokenBucket{tokens: l.burst - 1, last: now}
		return true
	}
	// Refill by elapsed time, capped at burst.
	elapsed := now.Sub(b.last).Seconds()
	b.tokens = minFloat(l.burst, b.tokens+elapsed*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepFullLocked drops buckets that have refilled to capacity (idle since their last use), reclaiming
// memory under a churn of distinct IPs. Caller holds l.mu.
func (l *ipRateLimiter) sweepFullLocked(now time.Time) {
	for k, b := range l.buckets {
		if minFloat(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate) >= l.burst {
			delete(l.buckets, k)
		}
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------------------------------
// The allowlist projection (ADR 0069 §2 — the #1 security guard).
// ---------------------------------------------------------------------------------------------------

// SharedRunView is the PUBLIC projection of a run served over the unauthenticated share route (m75.2). It
// is built field-by-field from a *run.Run by newSharedRunView — the run DTO is NEVER marshaled directly.
// This is the crux of ADR 0069 §2: projection-by-allowlist, not projection-by-blocklist. A future field
// added to run.Run (or nested in a Message) can only reach this route by being ADDED here on purpose — and
// the golden key-set test (TestSharedRunView_ExactKeySet) fails the build the moment the JSON key set
// drifts, so a silent leak through the unauthenticated door is impossible.
//
// DELIBERATELY OMITTED (never marshal these): traceId, conversationId, spawn/handoff lineage
// (parentRunId/rootRunId/spawnDepth/handedOffTo/handoffSourceRunId), and everything json:"-" on run.Run
// (CallerUsername, Boundary, Endpoint, SpecSnapshot, …). The raw Error string is NEVER surfaced by default
// (it is a sleeper that echoes input fragments / provider bodies) — only a coarse ErrorCategory bucket.
type SharedRunView struct {
	// Always present — metadata + status + structure (ADR 0069 §2 default projection).
	ID           string    `json:"id"`
	Namespace    string    `json:"namespace"`
	Agent        string    `json:"agent"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	MessageCount int       `json:"messageCount"`
	MessageRoles []string  `json:"messageRoles"`
	// ErrorCategory is a COARSE bucket ("", "timeout", "cancelled", "guardrail", "validation", "error"),
	// never the raw error string (which can echo input fragments / provider bodies / connection strings).
	ErrorCategory string `json:"errorCategory"`

	// Content-gated — present ONLY when the share was minted with includeContent=true (ADR 0069 §2). Input
	// is user content of the same sensitivity class as the transcript, so it is gated TOGETHER with the
	// messages + the full error. omitempty keeps the default (metadata-only) projection's key set stable.
	Input    json.RawMessage `json:"input,omitempty"`
	Messages []run.Message   `json:"messages,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// newSharedRunView builds the public projection from a run, allowlisting field-by-field. When
// includeContent is false, ONLY metadata + status + structure + the coarse error category are populated;
// when true, the run's Input + Messages + full Error are added TOGETHER (ADR 0069 §2). This constructor is
// the SOLE path from a run.Run to the unauthenticated route; m75.2's public read calls it.
func newSharedRunView(rn *run.Run, includeContent bool) SharedRunView {
	roles := make([]string, 0, len(rn.Messages))
	for _, m := range rn.Messages {
		roles = append(roles, m.Role)
	}
	view := SharedRunView{
		ID:            rn.ID,
		Namespace:     rn.Namespace,
		Agent:         rn.Agent,
		Status:        string(rn.Status),
		CreatedAt:     rn.CreatedAt,
		UpdatedAt:     rn.UpdatedAt,
		MessageCount:  len(rn.Messages),
		MessageRoles:  roles,
		ErrorCategory: categorizeError(rn.Error),
	}
	if includeContent {
		view.Input = rn.Input
		view.Messages = rn.Messages
		view.Error = rn.Error
	}
	return view
}

// categorizeError maps a raw run error string to a COARSE, non-revealing bucket (ADR 0069 §2). The raw
// string is NEVER surfaced by default because it can echo input fragments, provider response bodies, or
// connection strings. The buckets are intentionally crude — enough for a viewer to know "it failed and
// roughly why" without leaking content. An empty error → "" (no failure).
func categorizeError(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "context deadline"):
		return errorCategoryTimeout
	case strings.Contains(lower, "cancel"):
		return errorCategoryCancelled
	case strings.Contains(lower, "guardrail") || strings.Contains(lower, "blocked by policy") || strings.Contains(lower, "policy"):
		return errorCategoryGuardrail
	case strings.Contains(lower, "schema") || strings.Contains(lower, "validation") || strings.Contains(lower, "invalid"):
		return errorCategoryValidation
	default:
		return errorCategoryOther
	}
}
