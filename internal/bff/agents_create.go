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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

	created := make([]createdObject, 0, len(objs))
	for _, obj := range objs {
		// Capture the Kind BEFORE Create: client-go's create path may clear the
		// object's TypeMeta, so we resolve the Kind from the scheme up front.
		kind := kindOf(obj, scheme)
		obj.SetNamespace(ns)
		if cErr := w.Create(ctx, obj); cErr != nil {
			return created, classifyCreateError(cErr, kind, obj.GetName())
		}
		created = append(created, createdObject{
			Kind:      kind,
			Name:      obj.GetName(),
			Namespace: obj.GetNamespace(),
		})
	}
	return created, nil
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
// into typed client.Objects using the scheme. The order is preserved so the
// AgentDeployment is created after its referenced EvalSuite/PromptVersion.
func decodeManifests(manifests []byte, scheme *runtime.Scheme) ([]client.Object, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bufio.NewReader(bytes.NewReader(manifests)), 4096)
	codec := serializerCodec(scheme)

	var out []client.Object
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
		out = append(out, co)
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
