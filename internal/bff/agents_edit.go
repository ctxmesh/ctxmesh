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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/expand"
)

// consoleFieldManager is the server-side-apply field-owner the console edit path
// applies under (ADR 0017 §6). It is DISTINCT from the controller's field manager
// so co-ownership is clean: the controller owns status + its derived fields; the
// console owns only the spec fields it manages. Applying under this owner with
// client.Apply + ForceOwnership means the console never PUT-clobbers controller-
// owned state — it only co-owns the fields it sends.
const consoleFieldManager = "agent-engine-console"

// envModelRoute / envSystemPrompt are the two container env vars the simplified
// spec models (see internal/expand): model.route → MODEL_ROUTE, systemPrompt →
// SYSTEM_PROMPT. They are the only env vars a degraded (annotation-less) edit may
// set; every other env var on the live object is hand-set outside the console.
const (
	envModelRoute   = "MODEL_ROUTE"
	envSystemPrompt = "SYSTEM_PROMPT"
)

// defaultExecutionModel is the CRD default for spec.executionModel — used to
// normalize an omitted value so an edit that doesn't send executionModel compares
// equal to a live object at the default (no false drift / no false rejection).
const defaultExecutionModel = "serving"

// safeEnvVars is the set of env-var names the degraded edit path is allowed to
// set. Any env var NOT in this set is hand-set outside the console and must be
// left intact by a degraded patch.
var safeEnvVars = map[string]bool{
	envModelRoute:   true,
	envSystemPrompt: true,
}

// AgentApplier is the narrow caller-scoped seam the edit path needs: read the
// live AgentDeployment and server-side-apply changes to it. It is satisfied by
// the same controller-runtime client.Client the create/detail paths use, so the
// real BFF passes the caller-scoped client (ADR 0011) and the K8s API server
// enforces the caller's RBAC on both the read and the apply. Narrowing to
// Get+Patch keeps the edit handler unit-testable with the fake client.
type AgentApplier interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
	Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error
}

// handleUpdateAgent serves PUT /api/agents/{ns}/{name} — the console edit path
// (ADR 0017). It is CALLER-SCOPED (ADR 0011): the live read AND the server-side
// apply run through the caller's own client, so a viewer's PUT surfaces the API
// server's real 403 (the BFF never pre-empts the decision and never falls back to
// its own SA). It has two modes, keyed on whether the live AgentDeployment carries
// the source-spec annotation stamped at create (m15.2):
//
//   - Mode A (annotation PRESENT) — full round-trip: the body is the edited
//     simplified spec; the BFF runs the same guards as create (no inline secrets,
//     size), re-runs the expand core, and server-side-applies each manifest under
//     the console field-manager, then re-stamps the (new) source-spec so the next
//     edit round-trips.
//   - Mode B (annotation ABSENT) — degraded safe-field patch: only image, scaling,
//     model route, and systemPrompt/env may change; a submitted spec that would
//     change any other modeled field is rejected with a teaching 400.
//
// Errors surface cleanly, never swallowed: missing token → 401 (before any K8s
// call), missing agent → 404, bad body / bad edit → 400, RBAC denial → 403, other
// API failure → 502.
func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	ns := strings.TrimSpace(r.PathValue("ns"))
	name := strings.TrimSpace(r.PathValue("name"))
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}

	body, err := readEditBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Read the live AgentDeployment CALLER-SCOPED. A Forbidden here is the API
	// server's real 403 on the caller (a viewer), surfaced honestly; a not-found is
	// a 404. This read also decides the mode: the source-spec annotation is the
	// console-managed marker.
	var live agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &live); err != nil {
		s.writeGetError(w, err, "agent")
		return
	}

	_, consoleManaged := live.Annotations[expand.AnnotationSourceSpec]
	if consoleManaged {
		s.editRoundTrip(w, r, caller, ns, name, body.AgentYAML)
		return
	}
	s.editDegraded(w, r, caller, &live, body.AgentYAML)
}

// UpdateAgentRequest is the PUT /api/agents/{ns}/{name} body: the edited
// simplified agent.yaml (the SAME shape create/expand consume). The browser never
// sends raw CRDs — only the simplified spec — so an edit cannot diverge from what
// the user previewed, exactly like create.
type UpdateAgentRequest struct {
	// AgentYAML is the edited simplified agent.yaml.
	AgentYAML string `json:"agentYAML"`
}

// readEditBody reads + JSON-decodes the PUT body under the shared size bound and
// validates the agentYAML is present. It returns a client-safe error message for
// a 400 on any failure so the handler never surfaces server internals.
func readEditBody(r *http.Request) (UpdateAgentRequest, error) {
	var req UpdateAgentRequest
	body, err := readLimitedBody(r)
	if err != nil {
		return req, errors.New("failed to read request body")
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return req, errors.New("invalid JSON body")
	}
	if strings.TrimSpace(req.AgentYAML) == "" {
		return req, errors.New("agentYAML is required")
	}
	return req, nil
}

// editRoundTrip is Mode A (ADR 0017): the agent is console-managed (source-spec
// annotation present), so the edited simplified spec is a full round-trip. It runs
// the SAME guards as create (canonicalize → reject inline secrets / oversize),
// re-runs the expand core, server-side-applies each manifest under the console
// field-manager (co-owning cleanly with the controller — NEVER a plain Update that
// clobbers controller-owned status/derived state), and re-stamps the new
// source-spec on the AgentDeployment so the next edit round-trips.
//
// Carry-forward (NOT this task): objects a prior version of expand emitted but the
// new spec no longer emits (e.g. a removed tool's MCPToolBinding) are left in
// place — orphan pruning is a later concern; this path never deletes anything.
func (s *Server) editRoundTrip(w http.ResponseWriter, r *http.Request, applier AgentApplier, ns, name, editedYAML string) {
	if err := applyEditedSpec(r.Context(), applier, s.scheme, []byte(editedYAML), ns, name); err != nil {
		s.writeEditError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, UpdateAgentResponse{Mode: editModeRoundTrip})
}

// applyEditedSpec runs the full Mode-A round-trip against the applier: canonicalize
// (guards) → expand → decode → stamp the new source-spec on the AgentDeployment →
// SSA-apply every manifest under the console field-manager. It refuses to apply a
// spec whose AgentDeployment name does not match the object being edited so a PUT
// can never rename/re-target the agent (the {name} in the URL is authoritative).
func applyEditedSpec(ctx context.Context, applier AgentApplier, scheme *runtime.Scheme, editedYAML []byte, ns, name string) error {
	if scheme == nil {
		return &createError{status: 500, msg: "server misconfigured: no scheme"}
	}

	// Same create-path guard: reject inline secrets + oversize BEFORE any apply, so
	// no object is touched when the spec is unstorable (ADR 0017 §2/§3).
	sourceSpec, sErr := canonicalizeSourceSpec(editedYAML)
	if sErr != nil {
		return sErr
	}

	manifests, err := expand.Expand(editedYAML)
	if err != nil {
		var xe *expand.Error
		if errors.As(err, &xe) {
			return &createError{status: 400, msg: err.Error()}
		}
		return &createError{status: 400, msg: fmt.Sprintf("invalid agent.yaml: %v", err)}
	}

	objs, err := decodeManifests(manifests, scheme)
	if err != nil {
		return &createError{status: 500, msg: fmt.Sprintf("decoding expanded manifests: %v", err)}
	}

	// The edit must target the SAME AgentDeployment as the URL — a PUT is not a
	// rename. Reject a spec whose primary object name differs so we never
	// apply-create a second agent under the wrong name.
	for _, d := range objs {
		if d.kind == agentDeploymentKind && d.obj.GetName() != name {
			return &createError{
				status: 400,
				msg:    fmt.Sprintf("edited spec names agent %q but the URL targets %q — rename is not supported", d.obj.GetName(), name),
			}
		}
	}

	// Re-stamp the NEW source-spec on the AgentDeployment so the next edit
	// round-trips from the just-submitted intent (ADR 0017 §1).
	stampSourceSpec(objs, sourceSpec)

	for _, d := range objs {
		d.obj.SetNamespace(ns)
		// SSA needs the object's GVK populated (client.Apply encodes apiVersion/kind).
		// decodeManifests decodes through the UniversalDeserializer, which sets
		// TypeMeta, but re-resolve from the scheme defensively so a stripped TypeMeta
		// can never produce an apply without a kind.
		if err := ensureGVK(d.obj, scheme); err != nil {
			return &createError{status: 500, msg: fmt.Sprintf("resolving kind for %s: %v", d.kind, err)}
		}
		// client.Apply (the apply-PATCH) is the server-side-apply write here: the
		// newer client.Client.Apply() needs a generated runtime.ApplyConfiguration our
		// CRDs don't have, so the patch-based apply is the supported path for typed
		// CRD SSA (ADR 0017 §6).
		if pErr := applier.Patch(ctx, d.obj, client.Apply, //nolint:staticcheck // typed-CRD SSA has no ApplyConfiguration; patch-apply is the supported path
			client.FieldOwner(consoleFieldManager), client.ForceOwnership); pErr != nil {
			return classifyApplyError(pErr, d.kind, d.obj.GetName())
		}
	}
	return nil
}

// editDegraded is Mode B (ADR 0017): the agent is NOT console-managed (no
// source-spec annotation), so we have no captured intent to round-trip. We must
// NOT re-expand (that would regenerate/destroy children we didn't author). Instead
// we parse the submitted simplified spec, verify it changes ONLY safe fields
// (image, scaling, model route, systemPrompt/env) relative to the live object, and
// server-side-apply a MINIMAL object owning only those paths under the console
// field-manager.
//
// The rejection rule (see safeFieldEdit): a submitted non-safe field that is
// CLEARLY MODELED and whose value DIFFERS from the live object → 400 (teaching). A
// non-safe field that merely echoes the live value unchanged is tolerated (the UI
// may round-trip what it read). Everything the simplified spec doesn't model is
// ignored — the live object keeps it.
func (s *Server) editDegraded(w http.ResponseWriter, r *http.Request, applier AgentApplier, live *agentsv1alpha1.AgentDeployment, editedYAML string) {
	edit, err := parseSafeFieldEdit([]byte(editedYAML), live)
	if err != nil {
		s.writeEditError(w, err)
		return
	}

	patch := edit.applyOnto(live, s.scheme)
	// client.Apply (patch-apply) SSA — see the note in applyEditedSpec: typed-CRD
	// SSA has no ApplyConfiguration, so the patch-based apply is the supported path.
	if pErr := applier.Patch(r.Context(), patch, client.Apply, //nolint:staticcheck // typed-CRD SSA has no ApplyConfiguration; patch-apply is the supported path
		client.FieldOwner(consoleFieldManager), client.ForceOwnership); pErr != nil {
		s.writeEditError(w, classifyApplyError(pErr, agentDeploymentKind, live.Name))
		return
	}
	writeJSON(w, http.StatusOK, UpdateAgentResponse{Mode: editModeDegraded})
}

// UpdateAgentResponse is returned by PUT /api/agents/{ns}/{name} on success. Mode
// tells the UI which path ran ("roundTrip" for a console-managed agent, "degraded"
// for a safe-field patch of an annotation-less one) so it can message accordingly.
type UpdateAgentResponse struct {
	Mode string `json:"mode"`
}

const (
	editModeRoundTrip = "roundTrip"
	editModeDegraded  = "degraded"
)

// --- Mode B: safe-field extraction + rejection rule -------------------------

// safeFieldEdit holds the safe-field values extracted from a degraded edit's
// simplified spec, each with a "present" flag so an absent field leaves the live
// value untouched (a degraded patch never zeroes an unspecified safe field).
type safeFieldEdit struct {
	image        string
	imageSet     bool
	scaling      *agentsv1alpha1.ScalingSpec
	modelRoute   string
	modelSet     bool
	systemPrompt string
	promptSet    bool
}

// simplifiedEdit is the subset of the simplified agent.yaml the degraded path
// parses. It intentionally mirrors the create schema field names so the SAME body
// the UI submits parses here; the NON-safe fields are captured too, only to detect
// a disallowed change (see parseSafeFieldEdit). Unknown fields are ignored by the
// YAML decoder — a field the simplified spec doesn't model can't trip the guard.
type simplifiedEdit struct {
	Name           string          `json:"name"`
	Image          string          `json:"image"`
	ExecutionModel string          `json:"executionModel"`
	Runtime        string          `json:"runtime"`
	SystemPrompt   string          `json:"systemPrompt"`
	Model          *simplifiedMod  `json:"model"`
	Scaling        *simplifiedScl  `json:"scaling"`
	Resources      *simplifiedRes  `json:"resources"`
	Budget         json.RawMessage `json:"budget"`
	Eval           json.RawMessage `json:"eval"`
	Prompt         json.RawMessage `json:"prompt"`
	Tools          []string        `json:"tools"`
}

type simplifiedMod struct {
	Route string `json:"route"`
}

type simplifiedScl struct {
	Min int32 `json:"min"`
	Max int32 `json:"max"`
}

type simplifiedRes struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// parseSafeFieldEdit decodes the submitted simplified spec and enforces the
// degraded-mode rule (ADR 0017 §4). It:
//
//  1. rejects inline secrets in the submitted spec (the same no-secrets guard as
//     create — a degraded edit is still a spec the console handled);
//  2. rejects a CLEARLY-MODELED NON-safe field whose value DIFFERS from the live
//     object (executionModel, resources, budget, eval, prompt, tools) — the
//     "managed outside the UI" teaching 400;
//  3. extracts the safe-field values (image, scaling, model route, systemPrompt)
//     to apply, each with a present-flag so an absent field is left untouched.
//
// A non-safe field that merely ECHOES the live value unchanged does not trip (2):
// we compare the submitted value to what the live object already has, so a UI that
// round-trips the fields it read is tolerated; only a real attempted change to a
// field the console can't safely patch is rejected.
func parseSafeFieldEdit(editedYAML []byte, live *agentsv1alpha1.AgentDeployment) (safeFieldEdit, error) {
	var out safeFieldEdit

	// Reuse the create guard: no inline credential material in the submitted spec
	// (annotations aside, a degraded edit is still a spec the console accepted).
	if _, sErr := canonicalizeSourceSpec(editedYAML); sErr != nil {
		return out, sErr
	}

	// YAMLToJSON then decode: sigs.k8s.io/yaml round-trips YAML through
	// encoding/json, so we parse the simplified spec with the JSON tags above and an
	// unknown field is silently ignored (it can't be a modeled non-safe change).
	jsonBytes, err := yamlToJSON(editedYAML)
	if err != nil {
		return out, &createError{status: 400, msg: fmt.Sprintf("invalid agent.yaml: %v", err)}
	}
	var se simplifiedEdit
	if err := json.Unmarshal(jsonBytes, &se); err != nil {
		return out, &createError{status: 400, msg: fmt.Sprintf("invalid agent.yaml: %v", err)}
	}

	if rej := disallowedNonSafeChange(&se, live); rej != "" {
		return out, &createError{
			status: 400,
			msg: fmt.Sprintf(
				"this agent is managed outside the UI; only image, scaling, route, and system prompt can be edited here (attempted change to %s)",
				rej,
			),
		}
	}

	// Extract the safe fields to patch. A field absent from the submitted spec is
	// left untouched (present-flag false) — a degraded patch never zeroes a safe
	// field the user didn't send.
	if se.Image != "" {
		out.image, out.imageSet = se.Image, true
	}
	if se.Scaling != nil {
		out.scaling = &agentsv1alpha1.ScalingSpec{Min: se.Scaling.Min, Max: se.Scaling.Max}
	}
	if se.Model != nil && se.Model.Route != "" {
		out.modelRoute, out.modelSet = se.Model.Route, true
	}
	if se.SystemPrompt != "" {
		out.systemPrompt, out.promptSet = se.SystemPrompt, true
	}
	return out, nil
}

// disallowedNonSafeChange returns a human name for the first NON-safe field the
// submitted spec would CHANGE relative to the live object, or "" if the spec
// touches only safe fields (or echoes non-safe fields unchanged). The non-safe
// modeled fields are: executionModel, resources, budget, eval, prompt, and the
// managed tools list — none of which a degraded patch can safely apply (they
// regenerate children or roll a revision the console didn't author).
func disallowedNonSafeChange(se *simplifiedEdit, live *agentsv1alpha1.AgentDeployment) string {
	// executionModel: compare against the live value, treating "" and the CRD
	// default "serving" as equivalent so a UI echoing the effective value is fine.
	if se.ExecutionModel != "" && !execModelEqual(se.ExecutionModel, live.Spec.ExecutionModel) {
		return "executionModel"
	}
	if se.Resources != nil && resourcesChanged(se.Resources, live.Spec.Resources) {
		return "resources"
	}
	// budget/eval/prompt are structured blocks the degraded path can't map; ANY
	// non-empty submission of them is treated as an attempted change (the live
	// object's budget/evalSuiteRef/promptRef aren't in the simplified shape to
	// compare against, so we conservatively reject a present block rather than risk
	// a silent no-op that hides intent).
	if len(se.Budget) > 0 && !isJSONNull(se.Budget) {
		return "budget"
	}
	if len(se.Eval) > 0 && !isJSONNull(se.Eval) {
		return "eval"
	}
	if len(se.Prompt) > 0 && !isJSONNull(se.Prompt) {
		return "prompt"
	}
	if len(se.Tools) > 0 {
		return "tools" //nolint:goconst // user-facing field label, distinct from the json tag
	}
	return ""
}

// applyOnto builds the MINIMAL apply object (an AgentDeployment carrying only the
// identity + the safe fields being set) for SSA under the console field-manager.
// Because SSA co-ownership is per-field, sending only the safe paths means the
// console owns exactly those and never clobbers a field it didn't send (image is
// required by the CRD schema, so we always carry the live image when the edit
// leaves it unset — an apply object without image would be rejected at admission).
//
// Env is a whole-list field (listType default → atomic under SSA), so a partial
// env apply would drop the live env. We therefore rebuild the FULL env list:
// preserve every hand-set (non-safe) env var verbatim and overlay only the safe
// MODEL_ROUTE / SYSTEM_PROMPT entries the edit changed.
func (e safeFieldEdit) applyOnto(live *agentsv1alpha1.AgentDeployment, scheme *runtime.Scheme) *agentsv1alpha1.AgentDeployment {
	out := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: live.Name, Namespace: live.Namespace},
	}
	_ = ensureGVK(out, scheme)

	// image: apply the edited value, else carry the live image (the CRD requires a
	// non-empty image, so the minimal apply object must always include it).
	if e.imageSet {
		out.Spec.Image = e.image
	} else {
		out.Spec.Image = live.Spec.Image
	}

	// scaling: apply the edited bounds, else carry the live scaling so SSA doesn't
	// drop it (only set when the live object had one — a nil stays nil).
	if e.scaling != nil {
		out.Spec.Scaling = e.scaling
	} else if live.Spec.Scaling != nil {
		out.Spec.Scaling = &agentsv1alpha1.ScalingSpec{Min: live.Spec.Scaling.Min, Max: live.Spec.Scaling.Max}
	}

	out.Spec.Env = mergeSafeEnv(live.Spec.Env, e)
	return out
}

// mergeSafeEnv rebuilds the full env list for a degraded apply: every hand-set
// (non-safe) env var is preserved in its original order; the safe MODEL_ROUTE /
// SYSTEM_PROMPT vars are overlaid with the edited values (or the live value when
// the edit left them unset), appended after the preserved vars in a deterministic
// order. Env is an atomic list under SSA, so the apply must carry the WHOLE list
// or it would drop the live entries.
func mergeSafeEnv(liveEnv []corev1.EnvVar, e safeFieldEdit) []corev1.EnvVar {
	// Preserve non-safe (hand-set) vars verbatim, in order.
	out := make([]corev1.EnvVar, 0, len(liveEnv)+2)
	liveVal := map[string]string{}
	for _, ev := range liveEnv {
		if safeEnvVars[ev.Name] {
			liveVal[ev.Name] = ev.Value
			continue
		}
		out = append(out, ev)
	}

	// MODEL_ROUTE: edited value wins; else keep the live value if it had one.
	route, hadRoute := liveVal[envModelRoute]
	if e.modelSet {
		route, hadRoute = e.modelRoute, true
	}
	if hadRoute && route != "" {
		out = append(out, corev1.EnvVar{Name: envModelRoute, Value: route})
	}

	// SYSTEM_PROMPT: edited value wins; else keep the live value if it had one.
	prompt, hadPrompt := liveVal[envSystemPrompt]
	if e.promptSet {
		prompt, hadPrompt = e.systemPrompt, true
	}
	if hadPrompt && prompt != "" {
		out = append(out, corev1.EnvVar{Name: envSystemPrompt, Value: prompt})
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// --- drift + managed-outside-UI (detail DTO, ADR 0017 §4/§5) ----------------

// editModeFlags computes the two edit-mode flags for the detail DTO:
//   - managedOutsideUI: the source-spec annotation is ABSENT (a kubectl-created
//     agent) — a purely mechanical fact.
//   - drift: the annotation is PRESENT but the live AgentDeployment's console-
//     managed spec fields diverge from what re-expanding the stored source-spec
//     would produce (someone kubectl-patched a console-created agent).
//
// Drift SCOPE (documented for the caller): we re-expand the stored source-spec and
// compare the fields the console manages — image, executionModel, scaling, and the
// safe env vars (MODEL_ROUTE / SYSTEM_PROMPT). Non-console fields the controller
// derives are deliberately NOT compared (they aren't console-owned, so they can't
// "drift" from the source-spec). A stored spec that fails to re-expand (should not
// happen — we canonicalized it at write) is treated as "no drift" rather than
// surfacing an error on a read.
func (s *Server) editModeFlags(live *agentsv1alpha1.AgentDeployment) (managedOutsideUI, drift bool) {
	stored, present := live.Annotations[expand.AnnotationSourceSpec]
	if !present {
		return true, false
	}
	return false, s.driftFromSourceSpec(stored, live)
}

// driftFromSourceSpec re-expands the stored source-spec and reports whether the
// live AgentDeployment's console-managed spec fields diverge from the expanded
// intent. See editModeFlags for the compared-field scope. Any error re-expanding /
// decoding the stored spec → false (no drift) so the detail read never fails on a
// stale annotation.
func (s *Server) driftFromSourceSpec(stored string, live *agentsv1alpha1.AgentDeployment) bool {
	if s.scheme == nil {
		return false
	}
	manifests, err := expand.Expand([]byte(stored))
	if err != nil {
		return false
	}
	objs, err := decodeManifests(manifests, s.scheme)
	if err != nil {
		return false
	}
	for _, d := range objs {
		if d.kind != agentDeploymentKind {
			continue
		}
		want, ok := d.obj.(*agentsv1alpha1.AgentDeployment)
		if !ok {
			return false
		}
		return managedSpecDiverged(want, live)
	}
	return false
}

// managedSpecDiverged reports whether the live object's console-managed spec
// fields differ from the expanded (want) object's: image, executionModel
// (default-normalized), scaling bounds, and the safe env vars. This is the drift
// comparison — deliberately scoped to what the console owns.
func managedSpecDiverged(want, live *agentsv1alpha1.AgentDeployment) bool {
	if want.Spec.Image != live.Spec.Image {
		return true
	}
	if !execModelEqual(want.Spec.ExecutionModel, live.Spec.ExecutionModel) {
		return true
	}
	if scalingChanged(want.Spec.Scaling, live.Spec.Scaling) {
		return true
	}
	if safeEnvChanged(want.Spec.Env, live.Spec.Env) {
		return true
	}
	return false
}

// --- shared helpers ---------------------------------------------------------

// ensureGVK stamps the object's GroupVersionKind from the scheme when absent. SSA
// (client.Apply) encodes apiVersion/kind from the object's TypeMeta, so a stripped
// GVK would produce an apply the API server rejects; this guarantees it is set.
func ensureGVK(obj client.Object, scheme *runtime.Scheme) error {
	if !obj.GetObjectKind().GroupVersionKind().Empty() {
		return nil
	}
	gvks, _, err := scheme.ObjectKinds(obj)
	if err != nil {
		return err
	}
	if len(gvks) == 0 {
		return fmt.Errorf("no kind registered for %T", obj)
	}
	obj.GetObjectKind().SetGroupVersionKind(gvks[0])
	return nil
}

// classifyApplyError maps a caller-scoped server-side-apply failure to a typed
// *createError with the right HTTP status. It mirrors classifyCreateError but for
// the apply path: an already-exists cannot occur (apply is upsert), a Forbidden is
// the caller's RBAC denial (403), an Invalid/BadRequest is a bad manifest (400),
// anything else is an upstream API failure (502).
func classifyApplyError(err error, kind, name string) *createError {
	switch {
	case apierrors.IsForbidden(err):
		return &createError{status: 403, msg: fmt.Sprintf("forbidden: not allowed to update %s %q", kind, name)}
	case apierrors.IsNotFound(err):
		return &createError{status: 404, msg: fmt.Sprintf("%s %q not found", kind, name)}
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return &createError{status: 400, msg: fmt.Sprintf("%s %q rejected: %v", kind, name, err)}
	case apierrors.IsConflict(err):
		// An SSA field-ownership conflict (another manager owns a field we sent
		// without ForceOwnership). We DO force ownership, so this is unexpected; map
		// it to 409 with the detail so it is never a silent 500.
		return &createError{status: 409, msg: fmt.Sprintf("%s %q apply conflict: %v", kind, name, err)}
	default:
		return &createError{status: 502, msg: fmt.Sprintf("failed to update %s %q: %v", kind, name, err)}
	}
}

// writeEditError writes the HTTP response for an edit-path error. A typed
// *createError carries its own status + client-safe message (5xx are logged with
// the underlying detail; 4xx are the caller's input/permission and need no log);
// an unclassified error is a 500.
func (s *Server) writeEditError(w http.ResponseWriter, err error) {
	var ce *createError
	if errors.As(err, &ce) {
		if ce.status >= 500 {
			s.log.Error(err, "update agent failed")
		}
		writeError(w, ce.status, ce.msg)
		return
	}
	s.log.Error(err, "update agent failed (unclassified)")
	writeError(w, http.StatusInternalServerError, "failed to update agent")
}

// readLimitedBody reads the request body under the shared maxAgentYAMLBytes bound
// so a single request can never force us to buffer an unbounded body.
func readLimitedBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, maxAgentYAMLBytes))
}

// yamlToJSON converts YAML bytes to JSON via sigs.k8s.io/yaml (the same encoder
// canonicalizeSourceSpec uses), so the degraded path parses the simplified spec
// through encoding/json tags with unknown fields ignored.
func yamlToJSON(y []byte) ([]byte, error) {
	return sigsyaml.YAMLToJSON(y)
}

// isJSONNull reports whether a raw JSON message is a literal null (an absent block
// the YAML→JSON conversion rendered as null), so an omitted budget/eval/prompt
// doesn't count as a modeled non-safe change.
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// execModelEqual compares two executionModel values treating "" as the CRD default
// "serving", so a spec that omits executionModel and a live object at the default
// are equal (no drift / no disallowed change).
func execModelEqual(a, b string) bool {
	return normExecModel(a) == normExecModel(b)
}

func normExecModel(v string) string {
	if v == "" {
		return defaultExecutionModel
	}
	return v
}

// resourcesChanged reports whether the submitted resources block differs from the
// live object's. An empty submitted CPU/Memory field is treated as "not asserting
// that dimension" and does not count as a change (only a non-empty, differing
// value trips it).
func resourcesChanged(sub *simplifiedRes, live *agentsv1alpha1.AgentResources) bool {
	liveCPU, liveMem := "", ""
	if live != nil {
		liveCPU = live.CPU.String()
		liveMem = live.Memory.String()
	}
	if sub.CPU != "" && sub.CPU != liveCPU {
		return true
	}
	if sub.Memory != "" && sub.Memory != liveMem {
		return true
	}
	return false
}

// scalingChanged reports whether two scaling specs differ, normalizing a nil spec
// to the CRD defaults (min=0, max=3) so an omitted block compares equal to an
// explicit default block.
func scalingChanged(a, b *agentsv1alpha1.ScalingSpec) bool {
	aMin, aMax := scalingBounds(a)
	bMin, bMax := scalingBounds(b)
	return aMin != bMin || aMax != bMax
}

func scalingBounds(s *agentsv1alpha1.ScalingSpec) (min, max int32) {
	if s == nil {
		return 0, 3 // CRD defaults
	}
	max = s.Max
	if max == 0 {
		max = 3 // CRD default when unset
	}
	return s.Min, max
}

// safeEnvChanged reports whether the SAFE env vars (MODEL_ROUTE / SYSTEM_PROMPT)
// differ between two env lists, ignoring every non-safe (hand-set) var — the drift
// comparison is scoped to the fields the console manages.
func safeEnvChanged(a, b []corev1.EnvVar) bool {
	ma, mb := safeEnvMap(a), safeEnvMap(b)
	return ma[envModelRoute] != mb[envModelRoute] || ma[envSystemPrompt] != mb[envSystemPrompt]
}

func safeEnvMap(env []corev1.EnvVar) map[string]string {
	out := map[string]string{}
	for _, ev := range env {
		if safeEnvVars[ev.Name] {
			out[ev.Name] = ev.Value
		}
	}
	return out
}
