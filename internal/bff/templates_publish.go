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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/publishedartifact"
	"github.com/ctxmesh/ctxmesh/internal/expand"
)

// Publish = snapshot-at-publish (M74, m74.1, ADR 0068 §1). Publish cuts an IMMUTABLE, versioned release
// of an agent's source-spec into the published_artifacts table (the npm/OCI/Helm model — publish immutable
// versions, never a live pointer), so a later GET /api/templates (m74.2) + fork (m74.3) read a frozen
// snapshot rather than the drifting live agent. The write is CALLER-SCOPED with NO ADR 0011 exception:
// a caller-scoped GET of the AgentDeployment (exact parity with what the caller can `kubectl get`) reads
// its source-spec annotation, then a plain Postgres INSERT. There is no cross-namespace read, no new BFF-SA
// RBAC, and no controller change — the agent GET itself authorizes the publish.

// kindAgent is the only artifact kind publishable in v1 (teams / eval-suites join later — ADR 0068 §1/§8).
// It aliases nodeKindAgent (dto.go) so the "agent" literal has ONE home in the package.
const kindAgent = nodeKindAgent

// PublishTemplateRequest is the POST /api/templates body: publish an agent's current source-spec as an
// immutable template at the requested visibility. Publishing below `team` is meaningless (a private
// template is invisible to everyone) → rejected 400.
type PublishTemplateRequest struct {
	// Kind is the artifact kind. v1 accepts only "agent".
	Kind string `json:"kind"`
	// OriginNamespace / OriginName identify the AgentDeployment to snapshot (empty namespace → default).
	OriginNamespace string `json:"originNamespace"`
	OriginName      string `json:"originName"`
	// Visibility is the target tier: team, org, or public (ADR 0067 §1 enum; private rejected).
	Visibility string `json:"visibility"`
}

// PublishTemplateResponse reports the cut release: the assigned monotonic version + provenance + the
// content hash (a future "update available" check compares a fork's pinned hash/version against this).
// It carries NO spec body — publish is a write; the read DTO (m74.2) carries the spec.
type PublishTemplateResponse struct {
	Kind            string `json:"kind"`
	OriginNamespace string `json:"originNamespace"`
	OriginName      string `json:"originName"`
	Version         int    `json:"version"`
	Visibility      string `json:"visibility"`
	ContentHash     string `json:"contentHash"`
}

// handlePublishTemplate serves POST /api/templates (ADR 0068 §1). It:
//  1. caller-scoped GETs the AgentDeployment (originNamespace/originName) — the agent GET is the
//     authorization: a caller who cannot `get` the agent gets an honest 403/404, and no snapshot is cut;
//  2. reads its expand.AnnotationSourceSpec annotation — absent ⇒ an honest 400 (only console-authored
//     agents carry a source-spec and are publishable);
//  3. validates kind == "agent" and visibility ∈ {team, org, public} (private is meaningless → 400);
//  4. computes content_hash = sha256(canonical spec_json) and INSERTs the snapshot at version n+1.
//
// The store is nil-safe: a BFF without the published-artifact store serves 501, never a panic.
func (s *Server) handlePublishTemplate(w http.ResponseWriter, r *http.Request) {
	if s.publishedArtifactStore == nil {
		writeError(w, http.StatusNotImplemented, "template publishing is not configured (CONTROLPLANE_DSN unset)")
		return
	}
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req PublishTemplateRequest
	if jErr := json.Unmarshal(raw, &req); jErr != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}

	// v1 kind gate: only "agent" (teams / eval-suites are later — ADR 0068 §1/§8).
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = kindAgent // default the only supported kind so a minimal body works.
	}
	if kind != kindAgent {
		writeError(w, http.StatusBadRequest, `kind must be "agent" (teams and eval-suites are not yet publishable)`)
		return
	}

	name := strings.TrimSpace(req.OriginName)
	if name == "" {
		writeError(w, http.StatusBadRequest, "originName is required")
		return
	}
	ns := strings.TrimSpace(req.OriginNamespace)
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// Visibility gate: publish widens, so only team/org/public are valid targets; private is
	// meaningless (a private template is invisible to everyone) — reject with a teaching 400.
	vis := strings.TrimSpace(req.Visibility)
	if vis == "" {
		writeError(w, http.StatusBadRequest, "visibility is required (one of: team, org, public)")
		return
	}
	if vis == visibilityPrivate {
		writeError(w, http.StatusBadRequest, `visibility "private" cannot be published — a private template is invisible to everyone; use team, org, or public`)
		return
	}
	if vis != visibilityTeam && vis != visibilityOrg && vis != visibilityPublic {
		writeError(w, http.StatusBadRequest, "visibility must be one of: team, org, public")
		return
	}

	// (1) Caller-scoped GET the agent — this IS the authorization (parity with `kubectl get`).
	var ad agentsv1alpha1.AgentDeployment
	if gErr := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ad); gErr != nil {
		s.writeGetError(w, gErr, kindAgent)
		return
	}

	// (2) Read the source-spec annotation. Absent ⇒ the agent was authored outside the console
	// (kubectl / imported) and has no snapshot to publish — an honest 400, never a fabricated spec.
	specJSON := strings.TrimSpace(ad.Annotations[expand.AnnotationSourceSpec])
	if specJSON == "" {
		writeError(w, http.StatusBadRequest,
			"agent has no source-spec — only console-authored agents are publishable")
		return
	}

	// (4) content_hash over the canonical spec_json (ADR 0068 §6 staleness compare). The source-spec is
	// already canonical JSON (ADR 0017), so a byte-wise sha256 is stable across publishes of the same spec.
	sum := sha256.Sum256([]byte(specJSON))
	contentHash := hex.EncodeToString(sum[:])

	version, pErr := s.publishedArtifactStore.Publish(r.Context(), publishedartifact.PublishedArtifact{
		Kind:            kind,
		OriginNamespace: ns,
		OriginName:      name,
		SpecJSON:        json.RawMessage(specJSON),
		Visibility:      vis,
		ContentHash:     contentHash,
	})
	if pErr != nil {
		s.log.Error(pErr, "publish template failed", "namespace", ns, "name", name)
		writeError(w, http.StatusInternalServerError, "failed to publish the template")
		return
	}

	writeJSON(w, http.StatusCreated, PublishTemplateResponse{
		Kind:            kind,
		OriginNamespace: ns,
		OriginName:      name,
		Version:         version,
		Visibility:      vis,
		ContentHash:     contentHash,
	})
}

// handleUnpublishTemplate serves DELETE /api/templates/{kind}/{namespace}/{name} (ADR 0068 §1). It
// caller-scoped GETs the agent FIRST to authorize the caller (so a stranger cannot tombstone your
// template — only someone who can read the origin agent can unpublish it), then tombstones every version.
// Idempotent: tombstoning an absent / already-tombstoned artifact is a success (200), never a 500. The
// fork-time discoverability gate (m74.3) then 404s on a tombstoned artifact, exactly as M73 does.
//
// The store is nil-safe: a BFF without the published-artifact store serves 501, never a panic.
func (s *Server) handleUnpublishTemplate(w http.ResponseWriter, r *http.Request) {
	if s.publishedArtifactStore == nil {
		writeError(w, http.StatusNotImplemented, "template publishing is not configured (CONTROLPLANE_DSN unset)")
		return
	}
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	kind := strings.TrimSpace(r.PathValue("kind"))
	ns := strings.TrimSpace(r.PathValue("namespace"))
	name := strings.TrimSpace(r.PathValue("name"))
	if kind == "" || ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "kind, namespace, and name are required")
		return
	}
	if kind != kindAgent {
		writeError(w, http.StatusBadRequest, `kind must be "agent" (teams and eval-suites are not yet publishable)`)
		return
	}

	// Authorize the caller against the ORIGIN agent (caller-scoped GET). A caller who cannot read the
	// agent cannot unpublish its template — a stranger's 403/404 surfaces honestly. (A tombstone is the
	// unpublish; the read gate here is the ownership/authorization proof, mirroring publish.)
	var ad agentsv1alpha1.AgentDeployment
	if gErr := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ad); gErr != nil {
		s.writeGetError(w, gErr, kindAgent)
		return
	}

	if tErr := s.publishedArtifactStore.Tombstone(r.Context(), kind, ns, name); tErr != nil {
		s.log.Error(tErr, "unpublish template failed", "namespace", ns, "name", name)
		writeError(w, http.StatusInternalServerError, "failed to unpublish the template")
		return
	}
	// Idempotent success: whether or not a live artifact existed, the artifact is now unpublished.
	writeJSON(w, http.StatusOK, map[string]string{"status": "unpublished"})
}
