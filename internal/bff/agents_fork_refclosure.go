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

// The fork ref-closure policy (m74.4, ADR 0068 §5) — the milestone's only genuinely-novel
// surface. When an agent is forked cross-namespace (m74.3), its source-spec's SAME-namespace
// name-references DANGLE: they point at a ModelRoute / ToolRegistry / PromptVersion / EvalSuite
// (and, secret-adjacent, a provider key) that lives in the ORIGIN namespace and is NEVER copied.
// This pass runs AFTER the create — the agent exists first — then, per ADR 0068 §5:
//
//	model.route  → absent in target ns → needsRebinding "model route: <name>" (secret-adjacent:
//	               a ModelRoute wraps a provider key / SecretBinding, never copied cross-ns).
//	tools[]      → absent in target ns → if the tool maps to a DISCOVERABLE published MCP server,
//	               compose M73 connect to materialize it (the flywheel compounding); else, or if
//	               the mapping is not cleanly determinable, needsRebinding "tool: <name>" (an
//	               honest flag beats a wrong materialize).
//	promptRef    → by-name PromptVersion absent in target ns → unresolvedRefs "prompt: <name>".
//	evalSuiteRef → by-name EvalSuite absent in target ns → unresolvedRefs "evalSuite: <name>".
//	inline prompt:/eval: blocks + git pointers → RIDE FREE (they create their own target-ns
//	               objects through the create path) — nothing to detect.
//
// A non-empty needsRebinding stamps the labelForkNeedsRebinding degraded label (done by the
// caller). Everything here is caller-scoped: ModelRoute / EvalSuite are CRD GETs on the caller's
// client (they can read their own ns); ToolRegistry / PromptVersion are Postgres-store reads
// behind the caller-scoped path. NO secret / token is ever copied — the closure only DETECTS +
// FLAGS + (for published tools) composes the existing NO-secret M73 connect.
//
// Failure-mode (ADR 0068 §5): the agent is already created, so a ref-closure error (a catalog
// read hiccup, a store outage) must NOT fail the whole fork — it degrades to flagging, never a
// 500. No swallowed errors: every degradation path logs.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// forkRefSpec is the subset of the source-spec the ref-closure inspects. The source-spec is
// canonical JSON (ADR 0017); only these fields carry same-namespace name-references. Inline
// prompt:/eval: blocks + git pointers are deliberately NOT modelled — they ride free (they
// create their own target-ns objects), so there is nothing to detect for them.
type forkRefSpec struct {
	// Model.Route is a ModelRoute name (the model/provider-key ref — secret-adjacent).
	Model *struct {
		Route string `json:"route"`
	} `json:"model"`
	// Tools are tool catalog names (each → an MCPToolBinding → a ToolRegistry in the target ns).
	Tools []string `json:"tools"`
	// PromptRef is a by-name PromptVersion (DANGLES cross-ns; the inline `prompt:` git block is
	// portable and is NOT this field).
	PromptRef string `json:"promptRef"`
	// EvalSuiteRef is a by-name EvalSuite (DANGLES cross-ns; the inline `eval:` block is portable).
	// v1 source-specs carry the eval suite as `eval.suite`; a future explicit `evalSuiteRef` field
	// is read too. Both are handled by closeEvalRef.
	EvalSuiteRef string `json:"evalSuiteRef"`
	Eval         *struct {
		Suite string `json:"suite"`
		// Dataset/scorers/threshold present ⇒ an INLINE eval block (portable, rides free). A bare
		// `suite` with no other inline fields is a by-name reference that dangles.
		Dataset   string          `json:"dataset"`
		Scorers   json.RawMessage `json:"scorers"`
		Threshold string          `json:"threshold"`
	} `json:"eval"`
}

// closeForkRefs runs the ADR 0068 §5 ref-closure over the source-spec of a just-forked agent in
// callerNS/localName. It returns (needsRebinding, unresolvedRefs, resolvedRefs) — all non-nil
// (JSON [] not null). resolvedRefs lists tool names that were auto-materialized via the M73
// compose-connect flywheel (U9, m76.3). It NEVER errors: a source-spec that does not parse, or
// any per-class read hiccup, degrades to a conservative flag / skip and logs, because the agent
// already exists.
func (s *Server) closeForkRefs(r *http.Request, caller client.Client, sourceSpec, callerNS, localName string) ([]string, []string, []string) {
	needsRebinding := []string{}
	unresolvedRefs := []string{}

	var spec forkRefSpec
	if err := json.Unmarshal([]byte(sourceSpec), &spec); err != nil {
		// The create already succeeded from this same spec, so this is nearly impossible; if it
		// happens, we cannot inspect refs — degrade to "no flags" (the create's own validation
		// already gated the spec) and log rather than fail the fork.
		s.log.Error(err, "fork ref-closure: source-spec did not parse; skipping ref-closure",
			"namespace", callerNS, "name", localName)
		return needsRebinding, unresolvedRefs, []string{}
	}

	ctx := r.Context()

	// 1. model.route — a ModelRoute (CRD) of that name in the target ns (caller-scoped GET).
	if route := modelRouteName(&spec); route != "" {
		if !s.targetHasModelRoute(ctx, caller, callerNS, route) {
			needsRebinding = append(needsRebinding, "model route: "+route)
		}
	}

	// 2. tools[] — each absent-in-target tool is best-effort materialized from a discoverable
	// published MCP server (compose M73 connect), else flagged for rebinding.
	// flagged lists tools that needed closure but could not be auto-wired;
	// resolved lists tools that were auto-materialized (the U9 flywheel moment).
	flaggedTools, resolvedTools := s.closeToolRefs(r, caller, spec.Tools, callerNS)
	needsRebinding = append(needsRebinding, flaggedTools...)

	// 3. promptRef — a by-name PromptVersion in the target ns (Postgres store).
	if spec.PromptRef != "" {
		if !s.targetHasPromptVersion(ctx, callerNS, spec.PromptRef) {
			unresolvedRefs = append(unresolvedRefs, "prompt: "+spec.PromptRef)
		}
	}

	// 4. evalSuiteRef — a by-name EvalSuite (CRD) in the target ns. Inline eval blocks ride free.
	if suite := danglingEvalSuiteRef(&spec); suite != "" {
		if !s.targetHasEvalSuite(ctx, caller, callerNS, suite) {
			unresolvedRefs = append(unresolvedRefs, "evalSuite: "+suite)
		}
	}

	// resolvedTools carries the tools auto-wired via compose-connect (U9 flywheel moment).
	// Return it directly — no separate resolvedRefs accumulation needed.
	return needsRebinding, unresolvedRefs, resolvedTools
}

// modelRouteName returns the source-spec's model.route (the ModelRoute name), or "".
func modelRouteName(spec *forkRefSpec) string {
	if spec.Model == nil {
		return ""
	}
	return strings.TrimSpace(spec.Model.Route)
}

// danglingEvalSuiteRef returns the by-name EvalSuite the source-spec references, or "" when the
// eval is INLINE (portable, rides free) or absent. An explicit evalSuiteRef field wins; otherwise
// a bare `eval.suite` with NO other inline fields (dataset/scorers/threshold) is a by-name ref
// that dangles — an inline `eval:` block (which carries those fields) creates its own EvalSuite in
// the target ns and must NOT be flagged.
func danglingEvalSuiteRef(spec *forkRefSpec) string {
	if ref := strings.TrimSpace(spec.EvalSuiteRef); ref != "" {
		return ref
	}
	if spec.Eval == nil {
		return ""
	}
	suite := strings.TrimSpace(spec.Eval.Suite)
	if suite == "" {
		return ""
	}
	// Inline block (has dataset/scorers/threshold) → portable, rides free → not a dangling ref.
	if spec.Eval.Dataset != "" || len(spec.Eval.Scorers) > 0 || spec.Eval.Threshold != "" {
		return ""
	}
	return suite
}

// targetHasModelRoute reports whether a ModelRoute of that name exists in the target ns, via a
// caller-scoped GET (the caller can read their own ns). A not-found → false (needs rebinding); any
// OTHER read error → true (fail-OPEN: do not flag a healthy-but-unreadable route as broken, since
// the create already succeeded — log it so the hiccup is visible).
func (s *Server) targetHasModelRoute(ctx context.Context, caller client.Client, ns, name string) bool {
	var mr agentsv1alpha1.ModelRoute
	err := caller.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &mr)
	if err == nil {
		return true
	}
	if apierrors.IsNotFound(err) {
		return false
	}
	s.log.Error(err, "fork ref-closure: ModelRoute existence check failed; not flagging",
		"namespace", ns, "name", name)
	return true
}

// targetHasEvalSuite reports whether an EvalSuite of that name exists in the target ns (caller-
// scoped GET). Same fail-OPEN degradation as targetHasModelRoute.
func (s *Server) targetHasEvalSuite(ctx context.Context, caller client.Client, ns, name string) bool {
	var es agentsv1alpha1.EvalSuite
	err := caller.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &es)
	if err == nil {
		return true
	}
	if apierrors.IsNotFound(err) {
		return false
	}
	s.log.Error(err, "fork ref-closure: EvalSuite existence check failed; not flagging",
		"namespace", ns, "name", name)
	return true
}

// targetHasPromptVersion reports whether a PromptVersion of that name exists in the target ns via
// the Postgres store (PromptVersion is Postgres-of-record, ADR 0044). A nil store or a not-found →
// false (unresolved); any OTHER store error → true (fail-OPEN, logged).
func (s *Server) targetHasPromptVersion(ctx context.Context, ns, name string) bool {
	if s.promptStore == nil {
		// No store wired ⇒ we cannot confirm; treat as unresolved so the ref is surfaced honestly.
		return false
	}
	_, err := s.promptStore.Get(ctx, ns, name)
	if err == nil {
		return true
	}
	if errors.Is(err, controlplane.ErrNotFound) {
		return false
	}
	s.log.Error(err, "fork ref-closure: PromptVersion existence check failed; not flagging",
		"namespace", ns, "name", name)
	return true
}

// closeToolRefs runs the ADR 0068 §5 tool class over the source-spec's tools[]. For each tool NOT
// already resolvable in the target ns, it best-effort composes M73 connect to materialize a
// discoverable published MCP server that offers the tool (the flywheel compounding); a tool with
// no cleanly-determinable published origin is flagged "tool: <name>" — an honest flag beats a
// wrong materialize. Returns (flagged, resolved): flagged lists the needsRebinding entries for
// tools that could not be auto-wired; resolved lists tool names that were auto-materialized via
// compose-connect (the U9 flywheel moment — the UI celebrates these). Never nil-panics.
func (s *Server) closeToolRefs(r *http.Request, caller client.Client, tools []string, callerNS string) (flagged []string, resolved []string) {
	flagged = []string{}
	resolved = []string{}
	if len(tools) == 0 {
		return flagged, resolved
	}
	ctx := r.Context()

	// Tools ALREADY available in the target ns need no closure. The target-ns index maps every
	// tool name a locally-registered ToolRegistry offers → its registry (the same index the
	// create path uses to bind tools). A nil index (no store / read error) → empty map, so every
	// tool is treated as absent and routed through the published-catalog lookup below.
	localIdx := toolRegistryIndex(ctx, s.toolRegistryStore, callerNS)

	// The published-catalog origin index (tool name → the published server that offers it),
	// scoped to what THIS caller may discover. Built once (one catalog read) for all tools.
	pubIdx := s.publishedToolOrigins(r, callerNS)

	for _, raw := range tools {
		tool := strings.TrimSpace(raw)
		if tool == "" {
			continue
		}
		if _, ok := localIdx[tool]; ok {
			continue // already resolvable locally — nothing to close.
		}
		origin, ok := pubIdx[tool]
		if !ok {
			// Not offered by any discoverable published server (or the mapping is not cleanly
			// determinable) → flag for rebinding rather than guess.
			flagged = append(flagged, "tool: "+tool)
			continue
		}
		// Compose M73 connect: materialize the published server into the caller's ns with NO
		// credential (the crux). A local copy under the origin's sanitized name; an idempotent
		// re-connect (already materialized) is a benign success. A materialize FAILURE degrades
		// to flagging (the tool is still unresolved) — never fail the fork.
		localName := mcpServerName(origin.name)
		if _, mErr := s.materializePublishedMCP(r, caller, origin.namespace, origin.name, localName, callerNS); mErr != nil {
			s.log.Error(mErr, "fork ref-closure: compose-connect for published tool failed; flagging",
				"tool", tool, "originNamespace", origin.namespace, "originName", origin.name)
			flagged = append(flagged, "tool: "+tool)
			continue
		}
		// Auto-materialize succeeded — record it in resolved so the UI can celebrate.
		resolved = append(resolved, tool)
	}
	return flagged, resolved
}

// publishedOrigin is the (namespace, name) of a published MCP server that offers a given tool.
type publishedOrigin struct {
	namespace string
	name      string
}

// publishedToolOrigins builds a tool-name → discoverable-published-server index for callerNS, from
// the cross-tenant catalog (the same population M73 connect materializes from). It resolves the
// caller's tenant members (mirroring handleCatalog), lists the catalog rows visible to callerNS,
// and maps each server's tools to it. A tool offered by MORE THAN ONE server is AMBIGUOUS — the
// mapping is not cleanly determinable, so it is EXCLUDED from the index (the caller then flags it
// for rebinding rather than materializing an arbitrary one — an honest flag beats a wrong
// materialize, ADR 0068 §5). A nil store or any read error → an empty index (every tool flagged),
// logged — the fork is not failed.
func (s *Server) publishedToolOrigins(r *http.Request, callerNS string) map[string]publishedOrigin {
	idx := map[string]publishedOrigin{}
	if s.toolRegistryStore == nil {
		return idx
	}
	ctx := r.Context()

	members := s.callerTenantMembers(r, callerNS)
	rows, err := s.toolRegistryStore.ListCatalog(ctx, callerNS, members)
	if err != nil {
		s.log.Error(err, "fork ref-closure: catalog read failed; no tools will auto-materialize",
			"namespace", callerNS)
		return idx
	}
	// Deterministic order so an ambiguity decision is stable across runs.
	slices.SortStableFunc(rows, func(a, b toolregistry.ToolRegistry) int {
		if a.Namespace != b.Namespace {
			return strings.Compare(a.Namespace, b.Namespace)
		}
		return strings.Compare(a.Name, b.Name)
	})
	ambiguous := map[string]bool{}
	for i := range rows {
		row := &rows[i]
		for _, t := range row.Tools {
			name := strings.TrimSpace(t.Name)
			if name == "" || ambiguous[name] {
				continue
			}
			if existing, seen := idx[name]; seen {
				// A second server offers the same tool → ambiguous. Drop it: the mapping is not
				// cleanly determinable, so the caller flags it rather than picking one.
				if existing.namespace != row.Namespace || existing.name != row.Name {
					delete(idx, name)
					ambiguous[name] = true
				}
				continue
			}
			idx[name] = publishedOrigin{namespace: row.Namespace, name: row.Name}
		}
	}
	return idx
}

// callerTenantMembers resolves the namespace set the caller's tenant spans, for a catalog read
// (mirroring handleCatalog's fail-closed degradation): a nil / erroring tenant store → own-ns
// only (public + own-ns catalog rows still resolve). callerNS is always included.
func (s *Server) callerTenantMembers(r *http.Request, callerNS string) []string {
	members := []string{callerNS}
	if s.namespaceTenantStore == nil {
		return members
	}
	ctx := r.Context()
	tenant, ok, err := s.namespaceTenantStore.TenantOf(ctx, callerNS)
	if err != nil || !ok {
		return members
	}
	ms, mErr := s.namespaceTenantStore.MembersOf(ctx, tenant)
	if mErr != nil {
		return members
	}
	if !slices.Contains(ms, callerNS) {
		ms = append(ms, callerNS)
	}
	return ms
}
