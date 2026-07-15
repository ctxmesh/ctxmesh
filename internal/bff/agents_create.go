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

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/expand"
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
	scheme *runtime.Scheme,
	agentYAML []byte,
	namespace string,
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

	objs, err := decodeManifests(manifests, scheme)
	if err != nil {
		// The expand output failed to decode into typed objects — that is a server
		// bug (the mapping produced something the scheme rejects), not the caller's.
		return nil, &createError{status: 500, msg: fmt.Sprintf("decoding expanded manifests: %v", err)}
	}

	// Point each generated MCPToolBinding at the ToolRegistry its tool ACTUALLY lives
	// in (m25 S18): expand hardcodes a single default registry ("default-tools"), but
	// BYO-MCP tools live in per-server registries (e.g. "scalekit-mcp-server"), so the
	// default reference is RegistryNotFound and the binding never goes Ready. Best-
	// effort: a tool not found in any registry keeps expand's default.
	rewriteBindingRegistries(objs, toolRegistryIndex(ctx, reader, ns))

	// Stamp the source-spec annotation on the primary AgentDeployment only, before
	// it is created, so the edit source of truth rides the object from birth.
	stampSourceSpec(objs, sourceSpec)

	created := make([]createdObject, 0, len(objs))
	for _, d := range objs {
		d.obj.SetNamespace(ns)
		if cErr := w.Create(ctx, d.obj); cErr != nil {
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

// toolRegistryIndex lists the namespace's ToolRegistries and maps each tool NAME to
// the registry that catalogs it, so a generated MCPToolBinding can reference the
// registry the tool actually lives in (m25 S18). Registries are sorted by name so a
// tool name present in more than one registry resolves deterministically (first wins;
// bare-name collisions are unavoidable and rare given per-server registries). A list
// error → nil map (the caller keeps expand's default rather than failing the create).
func toolRegistryIndex(ctx context.Context, reader AgentReader, ns string) map[string]string {
	if reader == nil {
		return nil
	}
	var list agentsv1alpha1.ToolRegistryList
	if err := reader.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil
	}
	regs := list.Items
	slices.SortFunc(regs, func(a, b agentsv1alpha1.ToolRegistry) int {
		return strings.Compare(a.Name, b.Name)
	})
	idx := make(map[string]string)
	for i := range regs {
		for _, t := range regs[i].Spec.Tools {
			if _, seen := idx[t.Name]; !seen {
				idx[t.Name] = regs[i].Name
			}
		}
	}
	return idx
}

// rewriteBindingRegistries points each generated MCPToolBinding at the registry its
// tool actually lives in (m25 S18), overriding expand's hardcoded default so the
// binding can go Ready. A tool absent from every registry keeps the default (the
// binding will still be RegistryNotFound, but that is now an honest "no such tool"
// rather than a systemic mis-reference).
func rewriteBindingRegistries(objs []decodedObject, idx map[string]string) {
	if len(idx) == 0 {
		return
	}
	for _, d := range objs {
		binding, ok := d.obj.(*agentsv1alpha1.MCPToolBinding)
		if !ok {
			continue
		}
		if reg, found := idx[binding.Spec.ToolName]; found {
			binding.Spec.RegistryRef = reg
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
	switch {
	case apierrors.IsAlreadyExists(err):
		return &createError{status: 409, msg: fmt.Sprintf("%s %q already exists", kind, name)}
	case apierrors.IsForbidden(err):
		// M11 RBAC denial (e.g. a viewer persona) — surface the 403, do not 500.
		return &createError{status: 403, msg: fmt.Sprintf("forbidden: not allowed to create %s %q", kind, name)}
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return &createError{status: 400, msg: fmt.Sprintf("%s %q rejected: %v", kind, name, err)}
	default:
		return &createError{status: 502, msg: fmt.Sprintf("failed to create %s %q: %v", kind, name, err)}
	}
}
