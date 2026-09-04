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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/promptversion"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
	"github.com/ctxmesh/ctxmesh/internal/expand"
	"github.com/ctxmesh/ctxmesh/internal/prompt"
)

// serializerCodec returns a decoder that reads a single-document manifest into
// the typed object its apiVersion/kind name in the scheme (the agent CRDs are
// registered by the caller). Used to turn each expand-emitted YAML doc into a
// typed client.Object for creation.
func serializerCodec(scheme *runtime.Scheme) runtime.Decoder {
	return serializer.NewCodecFactory(scheme).UniversalDeserializer()
}

// defaultCreateNamespace is used when the request supplies no namespace. The BFF
// runs single-tenant in the control-plane namespace by default; multi-namespace
// creation is an explicit opt-in via the request body's namespace field.
const defaultCreateNamespace = "default"

// createError classifies a create-path failure so the handler maps it to the
// right HTTP status without swallowing it. The apply path must never report a
// failed create as success.
type createError struct {
	// status is the HTTP status the handler should return.
	status int
	// msg is the client-safe message.
	msg string
}

func (e *createError) Error() string { return e.msg }

// createdObject is the flat identity DTO returned for one created CRD object.
type createdObject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// decodedObject pairs an expand-emitted object with its Kind, resolved from the
// scheme up front. The Kind is captured before Create (client-go may strip an
// object's TypeMeta on write) and lets the source-spec stamp target the primary
// AgentDeployment without re-resolving the Kind per object.
type decodedObject struct {
	obj  client.Object
	kind string
}

// createAgentFromYAML expands a simplified agent.yaml through the SAME mapping
// the CLI/preview use, decodes each emitted CRD document into a typed object via
// the scheme, stamps the namespace, and creates each via the writer (client-go).
// It returns the flat identity of every created object, in emit order
// (EvalSuite/PromptVersion first, AgentDeployment last), so the SPA can confirm
// exactly what landed.
//
// Errors are typed *createError with an HTTP status: a bad agent.yaml → 400 (via
// the expand error kind), an already-exists → 409, an RBAC denial → 403, any
// other API failure → 502. A partial multi-object create returns the first
// failure — the objects created before it stay (K8s create is not
// transactional); the error names the object that failed so the operator can
// reconcile.
func createAgentFromYAML(
	ctx context.Context,
	w AgentWriter,
	reader AgentReader,
	promptStore promptversion.Store,
	regStore toolregistry.Store,
	scheme *runtime.Scheme,
	agentYAML []byte,
	namespace string,
	callerOwner string,
	opts ...createAgentOptions,
) ([]createdObject, error) {
	if scheme == nil {
		return nil, &createError{status: 500, msg: "server misconfigured: no scheme"}
	}
	ns := namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// Canonicalize the submitted simplified spec to the source-spec annotation
	// value (ADR 0017) BEFORE any create: this rejects inline secrets and an
	// oversize spec with a teaching 4xx so no object is created when the spec is
	// unstorable. A console create always carries this annotation; kubectl-created
	// agents won't, and are treated as "managed outside the UI" on edit.
	sourceSpec, sErr := canonicalizeSourceSpec(agentYAML)
	if sErr != nil {
		return nil, sErr
	}

	manifests, err := expand.Expand(agentYAML)
	if err != nil {
		// A parse/validation failure is the caller's input problem → 400.
		var xe *expand.Error
		if errors.As(err, &xe) {
			return nil, &createError{status: 400, msg: err.Error()}
		}
		return nil, &createError{status: 400, msg: fmt.Sprintf("invalid agent.yaml: %v", err)}
	}

	// Partition the expand output BEFORE decoding (ADR 0044 Amendment 2): PromptVersion is retired to
	// Postgres, so its `kind: PromptVersion` docs are extracted scheme-free (no CRD type to decode into) and
	// the rest are decoded as CRDs.
	crdDocs, pvInfos, err := partitionManifests(manifests)
	if err != nil {
		return nil, &createError{status: 500, msg: fmt.Sprintf("splitting expanded manifests: %v", err)}
	}

	created := make([]createdObject, 0, len(pvInfos)+2)

	// The inline `prompt:` block → a PromptVersion in the STORE (not the K8s API; there is no CRD) so
	// promptRef resolution below + the console reads find it. NOT best-effort: a validation/conflict error
	// fails the create with an honest status (nothing is created yet, so nothing is left dangling).
	for _, pv := range pvInfos {
		if cErr := createPromptVersionInStore(ctx, promptStore, pv, ns); cErr != nil {
			return created, cErr
		}
		created = append(created, createdObject{Kind: promptVersionKind, Name: pv.Name, Namespace: ns})
	}

	objs, err := decodeManifests(crdDocs, scheme)
	if err != nil {
		// The expand output failed to decode into typed objects — that is a server
		// bug (the mapping produced something the scheme rejects), not the caller's.
		return created, &createError{status: 500, msg: fmt.Sprintf("decoding expanded manifests: %v", err)}
	}

	// Point each generated MCPToolBinding at the ToolRegistry its tool ACTUALLY lives
	// in (m25 S18): expand hardcodes a single default registry ("default-tools"), but
	// BYO-MCP tools live in per-server registries (e.g. "scalekit-mcp-server"), so the
	// default reference is RegistryNotFound and the binding never goes Ready. Best-
	// effort: a tool not found in any registry keeps expand's default.
	idx := toolRegistryIndex(ctx, regStore, ns)
	rewriteBindingRegistries(objs, idx)
	// Bind-time owner guard (ADR 0029 edge case): a personal MCP server may be bound to an
	// agent ONLY by its owner — so a non-owner never gets a mid-run consent CTA for a server
	// they cannot even see. Refused before any object is created.
	if gErr := checkBindingOwnership(objs, idx, callerOwner); gErr != nil {
		return created, gErr
	}

	// Stamp the source-spec annotation on the primary AgentDeployment only, before
	// it is created, so the edit source of truth rides the object from birth.
	stampSourceSpec(objs, sourceSpec)

	// Draft stage (ADR 0065 D1): when the caller requested a draft create, stamp the
	// agents.ctxmesh.ai/stage=draft label on the AgentDeployment before creation so
	// the object is born as a draft. Default (no opts) → normal create, no label.
	if len(opts) > 0 && opts[0].draft {
		stampDraftLabel(objs)
	}

	// Compose-and-denormalize the prompt (ADR 0042, m40.3): resolve the agent's promptRef to its git
	// pointer (from the Postgres store — including a PromptVersion just created above) and stamp it as an
	// annotation, so the controller reconciles self-contained. CREATE is best-effort — an unresolved stamp
	// is ignored: a fresh object carries no stale annotation, and the controller surfaces an unstamped
	// promptRef at reconcile as PromptPointerMissing.
	_ = stampResolvedPrompt(ctx, promptStore, objs, ns)

	agentName := agentDeploymentName(objs)

	// Lazily-loaded name→agentRef of the namespace's existing MCPToolBindings — fetched
	// ONLY if a create 409s, so the common fresh-create path never pays for the List.
	var existingBindingRefs map[string]string
	bindingAgentRef := func(name string) (string, *createError) {
		if existingBindingRefs == nil {
			list, lErr := listMCPToolBindings(ctx, reader, client.InNamespace(ns))
			if lErr != nil {
				return "", &createError{status: 502, msg: fmt.Sprintf("failed to check existing bindings: %v", lErr)}
			}
			existingBindingRefs = make(map[string]string, len(list.Items))
			for i := range list.Items {
				existingBindingRefs[list.Items[i].Name] = list.Items[i].Spec.AgentRef
			}
		}
		return existingBindingRefs[name], nil
	}

	for _, d := range objs {
		d.obj.SetNamespace(ns)
		if cErr := w.Create(ctx, d.obj); cErr != nil {
			// Idempotent adopt (m26.6): a leftover generated MCPToolBinding from a prior
			// create of THIS agent is SAFE to skip rather than 409-abort the whole create
			// (the recreate-after-orphan wedge). Its EXISTING copy must target the same
			// agent; anything else — the primary AgentDeployment, or a binding targeting a
			// DIFFERENT agent — is a real conflict and still fails.
			if apierrors.IsAlreadyExists(cErr) && agentName != "" {
				if _, isBinding := d.obj.(*agentsv1alpha1.MCPToolBinding); isBinding {
					ref, rErr := bindingAgentRef(d.obj.GetName())
					if rErr != nil {
						return created, rErr
					}
					if ref == agentName {
						created = append(created, createdObject{Kind: d.kind, Name: d.obj.GetName(), Namespace: ns})
						continue
					}
				}
			}
			return created, classifyCreateError(cErr, d.kind, d.obj.GetName())
		}
		created = append(created, createdObject{
			Kind:      d.kind,
			Name:      d.obj.GetName(),
			Namespace: d.obj.GetNamespace(),
		})
	}
	return created, nil
}

// stampResolvedPrompt denormalizes the agent's prompt (ADR 0042, m40.3): it resolves the primary
// AgentDeployment's spec.promptRef to the PromptVersion's git pointer — from the Postgres store, since
// PromptVersion is retired (ADR 0044) — and stamps it as the prompt.ResolvedPromptAnnotation, so the
// controller reconciles the prompt WITHOUT reading a PromptVersion.
//
// It returns a *createError when a set promptRef does NOT resolve (400 not-found / 501 no store / 502
// read error). CALLERS DIFFER (m40.3 review): the CREATE path IGNORES that error (best-effort — a fresh
// object carries no stale annotation, and the controller surfaces the unstamped ref as PromptPointerMissing).
// The EDIT path MUST propagate it: an edit that leaves the stamp unresolved would let the controller keep
// serving the CREATE-time prompt (a wrong prompt, silently), so a bad-ref edit is rejected atomically
// BEFORE the SSA apply — nothing changes, no stale annotation.
func stampResolvedPrompt(ctx context.Context, promptStore promptversion.Store, objs []decodedObject, ns string) *createError {
	var agent *agentsv1alpha1.AgentDeployment
	for _, d := range objs {
		if o, ok := d.obj.(*agentsv1alpha1.AgentDeployment); ok {
			agent = o
		}
	}
	if agent == nil || agent.Spec.PromptRef == "" {
		return nil
	}
	if promptStore == nil {
		return &createError{status: 501, msg: "resolving promptRef requires the control-plane store (CONTROLPLANE_DSN)"}
	}

	pv, err := promptStore.Get(ctx, ns, agent.Spec.PromptRef)
	if err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			return &createError{status: 400, msg: fmt.Sprintf("promptRef %q does not resolve to a PromptVersion in namespace %q", agent.Spec.PromptRef, ns)}
		}
		return &createError{status: 502, msg: fmt.Sprintf("resolving promptRef %q: %v", agent.Spec.PromptRef, err)}
	}

	raw, err := json.Marshal(prompt.ResolvedPointer{
		Name: agent.Spec.PromptRef, Repo: pv.Repo, Ref: pv.Ref, Path: pv.Path,
	})
	if err != nil {
		return &createError{status: 500, msg: fmt.Sprintf("stamping resolved prompt: %v", err)}
	}
	ann := agent.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[prompt.ResolvedPromptAnnotation] = string(raw)
	agent.SetAnnotations(ann)
	return nil
}

// promptVersionInfo is the lightweight, SCHEME-FREE projection of an inline PromptVersion manifest — just the
// fields the store needs. PromptVersion is retired to Postgres (ADR 0044 Amendment 2), so the BFF routes it
// to the store WITHOUT the runtime scheme (there is no CRD type to decode into).
type promptVersionInfo struct {
	Name      string
	Namespace string
	Repo      string
	Ref       string
	Path      string
	Labels    map[string]string
}

// partitionManifests splits the expand YAML output into the PromptVersion docs (extracted scheme-free →
// promptVersionInfo, routed to the store) and the remaining CRD docs (reassembled for decodeManifests). The
// pre-filter (ADR 0044 Amendment 2) is what lets the PromptVersion CRD type leave the scheme entirely — the
// BFF never decodes a `kind: PromptVersion` doc.
func partitionManifests(manifests []byte) (crdDocs []byte, pvs []promptVersionInfo, err error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bufio.NewReader(bytes.NewReader(manifests)), 4096)
	var kept [][]byte
	for {
		var raw runtime.RawExtension
		if derr := dec.Decode(&raw); derr != nil {
			if errors.Is(derr, io.EOF) {
				break
			}
			return nil, nil, fmt.Errorf("splitting YAML documents: %w", derr)
		}
		if len(bytes.TrimSpace(raw.Raw)) == 0 {
			continue
		}
		// raw.Raw is JSON (the YAML decoder converts). Read only the kind + the PromptVersion fields.
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Git struct {
					Repo string `json:"repo"`
					Ref  string `json:"ref"`
					Path string `json:"path"`
				} `json:"git"`
			} `json:"spec"`
		}
		if uerr := json.Unmarshal(raw.Raw, &head); uerr != nil {
			return nil, nil, fmt.Errorf("reading manifest kind: %w", uerr)
		}
		if head.Kind == promptVersionKind {
			pvs = append(pvs, promptVersionInfo{
				Name: head.Metadata.Name, Namespace: head.Metadata.Namespace,
				Repo: head.Spec.Git.Repo, Ref: head.Spec.Git.Ref, Path: head.Spec.Git.Path,
				Labels: head.Metadata.Labels,
			})
			continue
		}
		kept = append(kept, raw.Raw)
	}
	if len(kept) > 0 {
		crdDocs = bytes.Join(kept, []byte("\n---\n"))
	}
	return crdDocs, pvs, nil
}

// createPromptVersionInStore writes an inline-created PromptVersion (the agent.yaml `prompt:` block) to the
// control-plane store — validated, atomic (409 on a duplicate). ADR 0044.
func createPromptVersionInStore(ctx context.Context, promptStore promptversion.Store, pv promptVersionInfo, ns string) *createError {
	if promptStore == nil {
		return &createError{status: 501, msg: "creating a prompt version requires the control-plane store (CONTROLPLANE_DSN)"}
	}
	// Namespace comes from the create request (ns), NOT pv.Namespace — the whole agent create is scoped to
	// one namespace, matching how the CRD path derived it.
	rec := promptversion.PromptVersion{
		Namespace: ns, Name: pv.Name,
		Repo: pv.Repo, Ref: pv.Ref, Path: pv.Path, Labels: pv.Labels,
	}
	if vErr := promptversion.Validate(rec); vErr != nil {
		return &createError{status: 422, msg: strings.TrimPrefix(vErr.Error(), "controlplane: invalid: ")}
	}
	if _, cErr := promptStore.Create(ctx, rec); cErr != nil {
		if errors.Is(cErr, controlplane.ErrConflict) {
			return &createError{status: 409, msg: fmt.Sprintf("prompt version %q already exists", pv.Name)}
		}
		return &createError{status: 502, msg: fmt.Sprintf("creating prompt version %q: %v", pv.Name, cErr)}
	}
	return nil
}

// agentDeploymentName returns the name of the AgentDeployment among the decoded objects
// (the agent being created), or "" when none is present. Used to recognize a leftover
// MCPToolBinding of the SAME agent for idempotent create (m26.6).
func agentDeploymentName(objs []decodedObject) string {
	for _, d := range objs {
		if ad, ok := d.obj.(*agentsv1alpha1.AgentDeployment); ok {
			return ad.Name
		}
	}
	return ""
}

// toolLoc is where a catalog tool actually lives: its ToolRegistry and the server
// URL/image the registry PINS it to (ToolEntry.URL/Image). A generated binding must
// match both the registry AND the pin, or the controller reports RegistryNotFound /
// RegistryMismatch (m25 S18).
type toolLoc struct {
	registry string
	url      string
	image    string
	// scope / owner mirror the registry's visibility labels (ADR 0029 §1) so the create
	// flow can enforce the bind-time owner guard: a personal server may be bound only by
	// its owner (a non-owner invoker gets an honest terminal error, not a consent CTA).
	scope string
	owner string
}

// toolRegistryIndex lists the namespace's ToolRegistries from the Postgres store
// (ToolRegistry is retired as a CRD, ADR 0044) and maps each tool NAME to its
// registry + pinned URL/image, so a generated MCPToolBinding can reference the
// registry the tool actually lives in AND the pin it must use. Registries are
// sorted by name so a tool present in more than one resolves deterministically
// (first wins). A nil store or a list error → nil map (the caller keeps expand's
// default rather than failing the create).
func toolRegistryIndex(ctx context.Context, store toolregistry.Store, ns string) map[string]toolLoc {
	if store == nil {
		return nil
	}
	var regs []toolregistry.ToolRegistry
	token := ""
	for {
		page, err := store.List(ctx, controlplane.ListOptions{
			Namespace: ns, PageSize: controlplane.MaxPageSize, PageToken: token,
		})
		if err != nil {
			return nil
		}
		regs = append(regs, page.Items...)
		if page.NextPage == "" {
			break
		}
		token = page.NextPage
	}
	slices.SortFunc(regs, func(a, b toolregistry.ToolRegistry) int {
		return strings.Compare(a.Name, b.Name)
	})
	idx := make(map[string]toolLoc)
	for i := range regs {
		for _, t := range regs[i].Tools {
			if _, seen := idx[t.Name]; !seen {
				idx[t.Name] = toolLoc{
					registry: regs[i].Name, url: t.URL, image: t.Image,
					scope: regs[i].Labels[labelMCPScope], owner: regs[i].Labels[labelMCPOwner],
				}
			}
		}
	}
	return idx
}

// checkBindingOwnership enforces the bind-time owner guard (ADR 0029 edge case): a binding
// whose tool lives on a PERSONAL server may be created only by that server's owner. It
// returns a 403 createError on the first violation, naming the offending tool. callerOwner
// is the creator's userGrantHash ("" when unresolved — then any personal bind is refused,
// fail-closed). Public / org / grandfathered (no scope) servers are unrestricted.
func checkBindingOwnership(objs []decodedObject, idx map[string]toolLoc, callerOwner string) *createError {
	for _, d := range objs {
		binding, ok := d.obj.(*agentsv1alpha1.MCPToolBinding)
		if !ok {
			continue
		}
		loc, found := idx[binding.Spec.ToolName]
		if !found || loc.scope != scopePersonal {
			continue
		}
		if callerOwner == "" || loc.owner != callerOwner {
			return &createError{
				status: 403,
				msg:    fmt.Sprintf("tool %q is on a personal MCP server you do not own — only its owner can bind it", binding.Spec.ToolName),
			}
		}
	}
	return nil
}

// rewriteBindingRegistries points each generated MCPToolBinding at the registry its
// tool actually lives in AND the URL/image that registry pins (m25 S18), overriding
// expand's hardcoded default + placeholder server so the binding can go Ready. A tool
// absent from every registry keeps expand's defaults (an honest "no such tool" rather
// than a systemic mis-reference).
func rewriteBindingRegistries(objs []decodedObject, idx map[string]toolLoc) {
	if len(idx) == 0 {
		return
	}
	for _, d := range objs {
		binding, ok := d.obj.(*agentsv1alpha1.MCPToolBinding)
		if !ok {
			continue
		}
		loc, found := idx[binding.Spec.ToolName]
		if !found {
			continue
		}
		binding.Spec.RegistryRef = loc.registry
		// Match the registry's pin so the controller's pin check passes: an empty pin
		// means "any", so only override when the registry actually pins a value.
		if loc.url != "" {
			binding.Spec.Server.URL = loc.url
		}
		if loc.image != "" {
			binding.Spec.Server.Image = loc.image
		}
	}
}

// kindOf resolves an object's Kind: its own TypeMeta first (populated by the
// deserializer), falling back to the scheme registration. Resolved before Create
// because client-go may strip TypeMeta from the object it writes.
func kindOf(obj client.Object, scheme *runtime.Scheme) string {
	if k := obj.GetObjectKind().GroupVersionKind().Kind; k != "" {
		return k
	}
	if gvks, _, err := scheme.ObjectKinds(obj); err == nil && len(gvks) > 0 {
		return gvks[0].Kind
	}
	return ""
}

// decodeManifests splits a multi-document YAML manifest set (the expand output)
// into typed client.Objects using the scheme, each paired with its Kind resolved
// up front (client-go may strip TypeMeta on Create). The order is preserved so
// the AgentDeployment is created after its referenced EvalSuite/PromptVersion.
func decodeManifests(manifests []byte, scheme *runtime.Scheme) ([]decodedObject, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bufio.NewReader(bytes.NewReader(manifests)), 4096)
	codec := serializerCodec(scheme)

	var out []decodedObject
	for {
		var raw runtime.RawExtension
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("splitting YAML documents: %w", err)
		}
		if len(bytes.TrimSpace(raw.Raw)) == 0 {
			continue
		}
		obj, _, err := codec.Decode(raw.Raw, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("decoding manifest: %w", err)
		}
		co, ok := obj.(client.Object)
		if !ok {
			return nil, fmt.Errorf("decoded object %T is not a client.Object", obj)
		}
		out = append(out, decodedObject{obj: co, kind: kindOf(co, scheme)})
	}
	if len(out) == 0 {
		return nil, errors.New("no manifests to create")
	}
	return out, nil
}

// classifyCreateError maps a client-go create failure to a typed *createError
// with the appropriate HTTP status. The object identity is folded into the
// message so a partial multi-object apply names the doc that failed.
func classifyCreateError(err error, kind, name string) *createError {
	return classifyWriteError(err, "create", kind, name)
}

// classifyWriteError is classifyCreateError with the VERB named. An upsert can be denied on
// its update rather than its create, and reporting that as "not allowed to create" sends a
// user to ask an admin for a permission they already hold — which is exactly what happened
// while building M155's credential role.
func classifyWriteError(err error, verb, kind, name string) *createError {
	switch {
	case apierrors.IsAlreadyExists(err):
		return &createError{status: 409, msg: fmt.Sprintf("%s %q already exists", kind, name)}
	case apierrors.IsForbidden(err):
		// M11 RBAC denial (e.g. a viewer persona) — surface the 403, do not 500.
		return &createError{status: 403, msg: fmt.Sprintf("forbidden: not allowed to %s %s %q", verb, kind, name)}
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return &createError{status: 400, msg: fmt.Sprintf("%s %q rejected: %v", kind, name, err)}
	default:
		return &createError{status: 502, msg: fmt.Sprintf("failed to %s %s %q: %v", verb, kind, name, err)}
	}
}
