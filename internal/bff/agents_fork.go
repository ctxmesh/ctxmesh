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

// Fork = install-from-template = ONE create path (m74.3, ADR 0068 §4/§6). A fork of an
// agent — whether a duplicate-in-place of your OWN agent or a cross-namespace install of
// a PUBLISHED one — funnels through the SAME create path as POST /api/agents
// (createAgentFromYAML → internal/expand → caller-scoped create). There is NO parallel
// fork subsystem: the fork resolves a canonical source-spec, then hands it to the one
// create path in the caller's OWN namespace, stamping provenance labels so a future
// "update available" check (ADR 0068 §6) is a pure SQL comparison.
//
// Authz model (mirrors mcp_connect.go — ADR 0068 §4 amending ADR 0011):
//   - Own-namespace fork (duplicate-in-place): a caller-scoped GET of the live
//     AgentDeployment reads its source-spec (parity with `kubectl get`). No source-spec
//     ⇒ 400 (kubectl-authored agents are not forkable).
//   - Cross-namespace fork: the caller has NO RBAC in the origin namespace (ADR 0011),
//     so the ONLY readable copy is the immutable PUBLISHED snapshot (published_artifacts,
//     read on the BFF's own store connection). A SECURITY GATE re-verifies the snapshot
//     is DISCOVERABLE by this caller (public / org-in-caller's-tenant) BEFORE forking —
//     undiscoverable / absent / tombstoned ⇒ 404 (never 403; fail-closed on a store error).
//   - The create itself runs on the CALLER's own client (caller-scoped), so the K8s API
//     server / store SSAR enforces the caller's RBAC in their OWN namespace — no BFF-SA grant.
//
// The ref-closure (secrets / tools / prompts — ADR 0068 §5) is m74.4: this task copies the
// source-spec AS-IS and returns EMPTY needsRebinding / unresolvedRefs (the contract arrays
// the UI already keys on, filled in the next task).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/expand"
)

// Fork provenance labels (ADR 0068 §6). Stamped on the forked AgentDeployment so the copy
// records WHERE it came from richly enough to un-freeze later (a v2 staleness badge / diff).
// They are metadata-only — a fork is a FROZEN one-time snapshot (no watch/sync back to the
// origin) and no runtime path keys on them. NON-secret: a namespace + name + a published
// version + a content hash, never any credential.
const (
	// labelForkOriginNamespace / labelForkOriginName record the origin agent's identity.
	labelForkOriginNamespace = "agents.ctxmesh.ai/fork-origin-namespace"
	labelForkOriginName      = "agents.ctxmesh.ai/fork-origin-name"
	// labelForkOriginVersion is the published_artifacts row version the fork pinned (a
	// cross-namespace fork). Empty for a duplicate-in-place (the live own-ns agent has no
	// published version) — so "update available" (ADR 0068 §6) compares this against the
	// latest published version with no live-agent read.
	labelForkOriginVersion = "agents.ctxmesh.ai/fork-origin-version"
	// labelForkContentHash is the sha256 of the canonical source-spec the fork was cut from,
	// so the staleness compare is a pure hash equality (fork's pinned hash vs latest published).
	labelForkContentHash = "agents.ctxmesh.ai/fork-content-hash"
	// labelForkNeedsRebinding marks a forked agent whose ref-closure (ADR 0068 §5) left at
	// least one same-namespace name-reference unresolvable in the target namespace (a model
	// route, a non-published tool, or a secret binding — never copied cross-namespace). It is
	// stamped "true" so the agent detail (m74.6) can surface a degraded/needs-attention state
	// rather than the user discovering a crash-loop. Metadata-only; no runtime path keys on it.
	labelForkNeedsRebinding = "agents.ctxmesh.ai/fork-needs-rebinding"
	// labelValueTrue is the stamped value of labelForkNeedsRebinding (a present-and-"true"
	// label). Named so the literal is not repeated (goconst).
	labelValueTrue = "true"
)

// forkProvenance is the origin identity + pinned version/hash a fork stamps onto its copy.
// version/contentHash are empty for a duplicate-in-place (no published snapshot).
type forkProvenance struct {
	originNamespace string
	originName      string
	version         string
	contentHash     string
}

// ForkAgentRequest is the POST /api/agents/{ns}/{name}/fork body. The {ns}/{name} path is
// the ORIGIN; the fork always lands in the CALLER's OWN namespace (a fork writes into your
// own namespace by construction — there is no target-namespace field). Name is the OPTIONAL
// local name for the copy (default = the origin name).
type ForkAgentRequest struct {
	// Name is the OPTIONAL local name for the forked copy in the caller's namespace.
	// Empty → the origin name is reused.
	Name string `json:"name"`
	// Namespace is the CALLER's own namespace — where the fork is created. Empty → the
	// default namespace. This is NOT a way to write into someone else's namespace: the
	// create runs caller-scoped, so a write into a namespace the caller has no RBAC in is
	// the API server's real 403.
	Namespace string `json:"namespace"`
}

// ForkAgentResponse reports the fork outcome. Status is "forked" for a fresh fork or
// "already-forked" for an idempotent re-fork of the SAME origin. Created is the flat
// identity of every CRD object the fork created. needsRebinding / unresolvedRefs are the
// ADR 0068 §5 ref-closure contract — EMPTY in m74.3 (the source-spec is copied as-is; the
// per-class closure is m74.4).
type ForkAgentResponse struct {
	Status         string          `json:"status"`
	Agent          AgentSummary    `json:"agent"`
	Created        []createdObject `json:"created"`
	NeedsRebinding []string        `json:"needsRebinding"`
	UnresolvedRefs []string        `json:"unresolvedRefs"`
}

// handleForkAgent serves POST /api/agents/{ns}/{name}/fork (m74.3, ADR 0068 §4/§6). See the
// file header for the model. It:
//  1. resolves the ORIGIN source-spec — own-ns: a caller-scoped GET of the live agent;
//     cross-ns: the published snapshot (with a discoverability re-check gate → 404);
//  2. provenance-matched idempotency: a same-name agent already in the caller's ns whose
//     fork-origin labels MATCH this origin → 200 already-forked; absent / DIFFERENT origin → 409;
//  3. forks via the shared helper (the ONE create path) in the caller's namespace, stamping
//     provenance.
func (s *Server) handleForkAgent(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	originNS := strings.TrimSpace(r.PathValue("ns"))
	originName := strings.TrimSpace(r.PathValue("name"))
	if originNS == "" || originName == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req ForkAgentRequest
	// An empty body is a valid fork (name/namespace default) — only a malformed body is a 400.
	if len(strings.TrimSpace(string(raw))) > 0 {
		if jErr := json.Unmarshal(raw, &req); jErr != nil {
			writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
			return
		}
	}

	callerNS := strings.TrimSpace(req.Namespace)
	if callerNS == "" {
		callerNS = defaultCreateNamespace
	}
	localName := strings.TrimSpace(req.Name)
	if localName == "" {
		localName = originName
	}

	// 1. Resolve the origin source-spec + provenance.
	sourceSpec, prov, rErr := s.resolveForkOrigin(r, caller, originNS, originName, callerNS)
	if rErr != nil {
		writeError(w, rErr.status, rErr.msg)
		return
	}

	// 2. Provenance-matched idempotency (ADR 0068 §4 — NOT M73's name-only): a same-name agent
	// already in the caller's namespace is only a benign re-fork when its fork-origin labels
	// MATCH this origin. A name collision with NO fork labels / a DIFFERENT origin is a real
	// conflict (a 200 there would silently lie about what the caller now owns) → 409.
	var existing agentsv1alpha1.AgentDeployment
	if gErr := caller.Get(r.Context(), client.ObjectKey{Namespace: callerNS, Name: localName}, &existing); gErr == nil {
		if forkOriginMatches(&existing, prov) {
			writeJSON(w, http.StatusOK, ForkAgentResponse{
				Status:         "already-forked",
				Agent:          newAgentSummary(&existing),
				Created:        []createdObject{},
				NeedsRebinding: []string{},
				UnresolvedRefs: []string{},
			})
			return
		}
		writeError(w, http.StatusConflict,
			"an agent of that name already exists in your namespace and is not a fork of this origin — choose a different name")
		return
	} else if !apierrors.IsNotFound(gErr) {
		// A read error other than not-found (e.g. a viewer's 403 on the target ns) surfaces
		// honestly rather than racing into a create that would fail anyway.
		s.writeGetError(w, gErr, kindAgent)
		return
	}

	// 3. Fork via the shared helper (the ONE create path), caller-scoped, in the caller's ns.
	resp, fErr := s.forkFromSourceSpec(r, caller, sourceSpec, prov, callerNS, localName)
	if fErr != nil {
		writeError(w, fErr.status, fErr.msg)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// resolveForkOrigin resolves the canonical source-spec + provenance for a fork of
// originNS/originName by a caller in callerNS (ADR 0068 §4). Own-namespace: a caller-scoped
// GET of the live agent's source-spec (absent ⇒ 400). Cross-namespace: the published
// snapshot, gated by the same discoverability predicate as MCP connect (undiscoverable /
// absent / tombstoned ⇒ 404; fail-closed on a store error).
func (s *Server) resolveForkOrigin(r *http.Request, caller client.Client, originNS, originName, callerNS string) (string, forkProvenance, *createError) {
	// Own-namespace (duplicate-in-place): read the live agent caller-scoped. The GET is the
	// authorization (parity with `kubectl get`); a viewer's 403 / a missing agent's 404 surface
	// honestly through writeGetError-shaped statuses.
	if originNS == callerNS {
		var ad agentsv1alpha1.AgentDeployment
		if gErr := caller.Get(r.Context(), client.ObjectKey{Namespace: originNS, Name: originName}, &ad); gErr != nil {
			return "", forkProvenance{}, getErrorToCreateError(gErr, kindAgent)
		}
		spec := strings.TrimSpace(ad.Annotations[expand.AnnotationSourceSpec])
		if spec == "" {
			return "", forkProvenance{}, &createError{
				status: http.StatusBadRequest,
				msg:    "this agent has no source-spec and cannot be forked — only console-authored agents are forkable",
			}
		}
		// Duplicate-in-place has no published version: version/contentHash stay empty so the
		// staleness compare (ADR 0068 §6) treats it as unpinned.
		return spec, forkProvenance{originNamespace: originNS, originName: originName}, nil
	}

	// Cross-namespace: the caller cannot read the live foreign agent (ADR 0011) — the only
	// readable copy is the immutable published snapshot.
	if s.publishedArtifactStore == nil {
		return "", forkProvenance{}, &createError{
			status: http.StatusNotImplemented,
			msg:    "forking a published template requires the control-plane store (CONTROLPLANE_DSN unset)",
		}
	}
	art, found, gErr := s.publishedArtifactStore.GetLatest(r.Context(), kindAgent, originNS, originName)
	if gErr != nil || !found {
		// A store error must NOT confirm existence; fail-closed to the same 404 as an
		// undiscoverable / tombstoned artifact (indistinguishable to the caller).
		return "", forkProvenance{}, &createError{status: http.StatusNotFound, msg: "no such forkable agent"}
	}

	// SECURITY GATE — re-verify the snapshot is DISCOVERABLE by this caller (public /
	// org-in-caller's-tenant), mirroring mcp_connect.go. Undiscoverable → 404 (never 403 —
	// a 403 would confirm the agent exists to a caller who may not see it).
	if !s.publishedDiscoverable(r, art.Visibility, originNS, callerNS) {
		return "", forkProvenance{}, &createError{status: http.StatusNotFound, msg: "no such forkable agent"}
	}

	prov := forkProvenance{
		originNamespace: originNS,
		originName:      originName,
		version:         strconv.Itoa(art.Version),
		contentHash:     art.ContentHash,
	}
	return string(art.SpecJSON), prov, nil
}

// forkFromSourceSpec is the shared fork helper (ADR 0068 §4): it creates the forked agent
// through the ONE create path (createAgentFromYAML) in the target namespace under localName,
// caller-scoped, then stamps the provenance labels on the created AgentDeployment. AFTER the
// create it runs the ref-closure pass (m74.4, ADR 0068 §5) over the source-spec — detecting
// same-namespace name-references that dangle cross-namespace and returning them in
// needsRebinding / unresolvedRefs, best-effort composing M73 connect for published tools, and
// stamping a degraded label when any rebinding is needed — so a fork never silently creates a
// crash-looping agent.
func (s *Server) forkFromSourceSpec(r *http.Request, caller client.Client, sourceSpec string, prov forkProvenance, callerNS, localName string) (ForkAgentResponse, *createError) {
	ctx := r.Context()

	// Rename the source-spec's agent to the local name so the fork lands under localName (a
	// cross-namespace fork of "assistant" may be installed locally as "my-assistant"). A map
	// overlay preserves every field the fork doesn't touch (crucially `tools`, ADR 0017).
	renamed, rErr := renameSourceSpec(sourceSpec, localName)
	if rErr != nil {
		return ForkAgentResponse{}, rErr
	}

	// The creator's identity, for the bind-time owner guard (ADR 0029) — same as create.
	callerOwner := ""
	if username, uErr := callerUsername(ctx, caller); uErr == nil {
		callerOwner = userGrantHash(username)
	}

	created, cErr := createAgentFromYAML(ctx, caller, caller, s.promptStore, s.toolRegistryStore, s.scheme, renamed, callerNS, callerOwner)
	if cErr != nil {
		var ce *createError
		if isCreateError(cErr, &ce) {
			return ForkAgentResponse{}, ce
		}
		return ForkAgentResponse{}, &createError{status: http.StatusInternalServerError, msg: "failed to fork the agent"}
	}

	// Stamp the provenance labels on the created AgentDeployment (caller-scoped label patch —
	// the create path already validated + built the object; a patch keeps the labels atomic
	// with the fork identity without threading a fork-specific option through the create core).
	if pErr := s.stampForkProvenance(ctx, caller, callerNS, localName, prov); pErr != nil {
		return ForkAgentResponse{}, pErr
	}

	// Ref-closure pass (ADR 0068 §5) — AFTER the create: the agent exists first, then we
	// detect the source-spec's dangling same-namespace refs, best-effort materialize published
	// tools (compose M73 connect), and flag the rest. A ref-closure error must NOT fail the
	// fork (the agent is already created) — closeForkRefs degrades to flagging, never 500s.
	needsRebinding, unresolvedRefs := s.closeForkRefs(r, caller, sourceSpec, callerNS, localName)

	// Degraded status (ADR 0068 §5): a non-empty needsRebinding means the fork can't reach a
	// model / a tool until the user connects one — stamp the needs-rebinding label so the agent
	// detail (m74.6) surfaces a degraded/needs-attention state. Best-effort: a label-stamp
	// failure does not fail the fork (the arrays already carry the honest signal).
	if len(needsRebinding) > 0 {
		if lErr := s.stampForkNeedsRebinding(ctx, caller, callerNS, localName); lErr != nil {
			s.log.Error(lErr, "fork: stamping needs-rebinding label failed", "namespace", callerNS, "name", localName)
		}
	}

	// Re-read the just-created (now labelled) agent for the summary — parity with what landed.
	summary := AgentSummary{Name: localName, Namespace: callerNS}
	var forked agentsv1alpha1.AgentDeployment
	if gErr := caller.Get(ctx, client.ObjectKey{Namespace: callerNS, Name: localName}, &forked); gErr == nil {
		summary = newAgentSummary(&forked)
	}

	return ForkAgentResponse{
		Status:         "forked",
		Agent:          summary,
		Created:        created,
		NeedsRebinding: needsRebinding,
		UnresolvedRefs: unresolvedRefs,
	}, nil
}

// stampForkProvenance patches the fork-origin labels onto the forked AgentDeployment,
// caller-scoped. A merge patch touches only the labels — it never clobbers spec/status.
// Empty version/contentHash (a duplicate-in-place) are stamped as empty-string labels so the
// origin identity is always recorded and the staleness compare has a defined "unpinned" value.
func (s *Server) stampForkProvenance(ctx context.Context, caller client.Client, ns, name string, prov forkProvenance) *createError {
	var ad agentsv1alpha1.AgentDeployment
	if gErr := caller.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &ad); gErr != nil {
		return getErrorToCreateError(gErr, kindAgent)
	}
	base := ad.DeepCopy()
	lbls := ad.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[labelForkOriginNamespace] = prov.originNamespace
	lbls[labelForkOriginName] = prov.originName
	lbls[labelForkOriginVersion] = prov.version
	lbls[labelForkContentHash] = prov.contentHash
	ad.SetLabels(lbls)
	if pErr := caller.Patch(ctx, &ad, client.MergeFrom(base)); pErr != nil {
		return classifyApplyError(pErr, agentDeploymentKind, name)
	}
	return nil
}

// stampForkNeedsRebinding patches the degraded-status label (labelForkNeedsRebinding="true")
// onto the forked AgentDeployment, caller-scoped (ADR 0068 §5). A merge patch touches only the
// label — it never clobbers spec/status. Called only when the ref-closure left at least one
// unresolvable rebinding, so the agent detail can surface a needs-attention state. Errors are
// returned (not swallowed) for the caller to log; a stamp failure does not fail the fork.
func (s *Server) stampForkNeedsRebinding(ctx context.Context, caller client.Client, ns, name string) error {
	var ad agentsv1alpha1.AgentDeployment
	if gErr := caller.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &ad); gErr != nil {
		return gErr
	}
	base := ad.DeepCopy()
	lbls := ad.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[labelForkNeedsRebinding] = labelValueTrue
	ad.SetLabels(lbls)
	return caller.Patch(ctx, &ad, client.MergeFrom(base))
}

// forkOriginMatches reports whether an existing local agent is a fork of the SAME origin as
// prov — the provenance-matched idempotency test (ADR 0068 §4). A matching origin (same
// namespace + name) makes a re-fork a benign 200 already-forked; anything else (no fork
// labels, or a DIFFERENT origin) is a name collision that must NOT silently 200-lie.
func forkOriginMatches(ad *agentsv1alpha1.AgentDeployment, prov forkProvenance) bool {
	lbls := ad.GetLabels()
	if lbls == nil {
		return false
	}
	return lbls[labelForkOriginNamespace] == prov.originNamespace &&
		lbls[labelForkOriginName] == prov.originName
}

// publishedDiscoverable applies the ADR 0067 §6 / ADR 0068 §4 catalog visibility predicate to
// a published artifact's visibility for a caller in callerNS — the same rule as
// mcpConnectDiscoverable, keyed on a visibility string (the published snapshot carries the
// visibility, not a CRD): own-ns always; public always; org only when originNS is in the
// caller's tenant; team/private in a foreign namespace never. Fail-closed on a nil / erroring
// tenant store (own-ns + public only), matching handleCatalog.
func (s *Server) publishedDiscoverable(r *http.Request, visibility, originNS, callerNS string) bool {
	if originNS == callerNS {
		return true
	}
	switch visibility {
	case visibilityPublic:
		return true
	case visibilityOrg:
		return s.namespaceInCallerTenant(r, callerNS, originNS)
	default:
		// team / private in a foreign namespace: never discoverable (the own-ns case is
		// handled above). A published artifact is never below `team` (the publish gate rejects
		// private), so `team` here means a foreign-namespace team artifact → hidden.
		return false
	}
}

// renameSourceSpec overlays a new `name` onto a canonical source-spec (JSON), returning the
// spec expand consumes. A map overlay — not a typed struct — preserves every field the fork
// doesn't touch (crucially `tools`), mirroring mergeEditOntoSourceSpec.
func renameSourceSpec(sourceSpec, name string) ([]byte, *createError) {
	var spec map[string]any
	if err := json.Unmarshal([]byte(sourceSpec), &spec); err != nil {
		return nil, &createError{status: http.StatusInternalServerError, msg: "the source spec could not be read for forking"}
	}
	spec["name"] = name
	out, err := json.Marshal(spec)
	if err != nil {
		return nil, &createError{status: http.StatusInternalServerError, msg: "re-encoding the forked spec failed"}
	}
	return out, nil
}
