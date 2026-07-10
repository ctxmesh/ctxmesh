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

package controller

import (
	"context"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

const (
	// toolsConfigMapSuffix names the per-agent durable-backing ConfigMap
	// (<agent>-tools) that holds tools.json (specs/mcp-tools.md).
	toolsConfigMapSuffix = "-tools"

	// toolsConfigMapKey is the data key inside the <agent>-tools ConfigMap.
	toolsConfigMapKey = "tools.json"

	// Ready-condition reasons for MCPToolBinding validation.
	reasonBound            = "Bound"
	reasonUnregisteredTool = "UnregisteredTool"
	reasonRegistryMismatch = "RegistryMismatch"
	reasonRegistryNotFound = "RegistryNotFound"

	// knativeServiceLabel is the label Knative stamps on an agent's pods,
	// used to list ready pods for the manifest push.
	knativeServiceLabel = "serving.knative.dev/service"
)

// toolsConfigMapName returns the durable-backing ConfigMap name for an agent.
func toolsConfigMapName(agentName string) string {
	return agentName + toolsConfigMapSuffix
}

// bindingValidation is the per-binding validation outcome: whether it is
// admitted into the manifest, and if not, the reason/message for its Ready
// condition.
type bindingValidation struct {
	Valid   bool
	Reason  string
	Message string
}

// validateBinding checks a single binding against the ToolRegistry set:
//
//   - registry exists                 → else RegistryNotFound
//   - toolName is a catalog entry     → else UnregisteredTool
//   - image/url matches the entry pin → else RegistryMismatch
//
// registries maps registry name → ToolRegistry (same namespace as the binding).
// An empty pin field on the entry means "any value allowed" for that field.
func validateBinding(b *agentsv1alpha1.MCPToolBinding, registries map[string]agentsv1alpha1.ToolRegistry) bindingValidation {
	reg, ok := registries[b.Spec.RegistryRef]
	if !ok {
		return bindingValidation{
			Reason:  reasonRegistryNotFound,
			Message: "referenced ToolRegistry " + b.Spec.RegistryRef + " does not exist",
		}
	}

	var entry *agentsv1alpha1.ToolEntry
	for i := range reg.Spec.Tools {
		if reg.Spec.Tools[i].Name == b.Spec.ToolName {
			entry = &reg.Spec.Tools[i]
			break
		}
	}
	if entry == nil {
		return bindingValidation{
			Reason: reasonUnregisteredTool,
			Message: "tool " + b.Spec.ToolName + " is not in ToolRegistry " +
				b.Spec.RegistryRef,
		}
	}

	// Pin matching: only the field relevant to the binding's mode is checked.
	if b.Spec.Mode == toolmanifest.ModeSidecar {
		if entry.Image != "" && entry.Image != b.Spec.Server.Image {
			return bindingValidation{
				Reason:  reasonRegistryMismatch,
				Message: "image " + b.Spec.Server.Image + " does not match the registry pin " + entry.Image,
			}
		}
	} else { // remote
		if entry.URL != "" && entry.URL != b.Spec.Server.URL {
			return bindingValidation{
				Reason:  reasonRegistryMismatch,
				Message: "url " + b.Spec.Server.URL + " does not match the registry pin " + entry.URL,
			}
		}
	}

	return bindingValidation{Valid: true, Reason: reasonBound, Message: "tool registered, pin-matched, and rendered into the agent manifest"}
}

// registryInputSchema returns the argument JSON Schema stored on the catalog
// entry a valid binding references (ToolEntry.InputSchema, captured from the MCP
// server's tools/list, m14.6). It is returned VERBATIM as raw JSON so the
// manifest — and in turn the managed loop (m14.6b) — hands the model exact
// tool-call parameters. Returns nil when the registry, the entry, or the schema
// is absent (curated/legacy entries): the render omits inputSchema and the SDK
// falls back to a permissive schema. Callers pass only bindings validateBinding
// already admitted, so the lookups are expected to hit — the nil returns are the
// graceful-absence path, not an error.
func registryInputSchema(
	b *agentsv1alpha1.MCPToolBinding,
	registries map[string]agentsv1alpha1.ToolRegistry,
) []byte {
	reg, ok := registries[b.Spec.RegistryRef]
	if !ok {
		return nil
	}
	for i := range reg.Spec.Tools {
		if reg.Spec.Tools[i].Name == b.Spec.ToolName {
			if schema := reg.Spec.Tools[i].InputSchema; schema != nil {
				return schema.Raw
			}
			return nil
		}
	}
	return nil
}

// listAgentBindings returns the agent's live bindings, sorted by binding name
// for deterministic port/container assignment. It lists all bindings in the
// namespace and filters by agentRef IN MEMORY (no field index exists for this
// — the envtest suite drives the reconcilers with a raw uncached client that
// has no field indexer, and the apiserver rejects field selectors on arbitrary
// CRD fields; client-side filtering serves both clients identically).
//
// Terminating bindings (deletionTimestamp set, held by bindingFinalizer) are
// EXCLUDED: from the moment a delete lands, the manifest, the ConfigMap, the
// push, and the pod template must all converge to the world without that
// binding — both controllers resolve through here, so the exclusion is
// consistent everywhere.
func listAgentBindings(
	ctx context.Context,
	c client.Client,
	namespace, agentName string,
) ([]agentsv1alpha1.MCPToolBinding, error) {
	var all agentsv1alpha1.MCPToolBindingList
	if err := c.List(ctx, &all, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := make([]agentsv1alpha1.MCPToolBinding, 0, len(all.Items))
	for i := range all.Items {
		if all.Items[i].Spec.AgentRef == agentName && all.Items[i].DeletionTimestamp.IsZero() {
			out = append(out, all.Items[i])
		}
	}
	slices.SortFunc(out, func(a, b agentsv1alpha1.MCPToolBinding) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// resolveAgentBindings lists an agent's live bindings (client-side filtered by
// agentRef),
// loads the referenced registries once each, validates every binding, and
// returns:
//
//   - the sorted list of valid render bindings (for the manifest / pod template),
//   - a per-binding validation map keyed by binding name (for status).
//
// It performs all reads with c; callers in either controller share the logic so
// the manifest the binding controller pushes and the pod template the
// AgentDeployment reconciler renders are computed identically.
func resolveAgentBindings(
	ctx context.Context,
	c client.Client,
	namespace, agentName string,
) (valid []toolmanifest.Binding, validations map[string]bindingValidation, err error) {
	bindings, err := listAgentBindings(ctx, c, namespace, agentName)
	if err != nil {
		return nil, nil, err
	}

	// Load each referenced registry once.
	registries := map[string]agentsv1alpha1.ToolRegistry{}
	for i := range bindings {
		ref := bindings[i].Spec.RegistryRef
		if _, seen := registries[ref]; seen {
			continue
		}
		var reg agentsv1alpha1.ToolRegistry
		getErr := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref}, &reg)
		if getErr == nil {
			registries[ref] = reg
		}
		// A missing registry is left absent from the map → validateBinding
		// reports RegistryNotFound. Any non-NotFound error still leaves it
		// absent; the binding is reported unresolved rather than failing the
		// whole reconcile (mirrors the ModelRoute controller's tolerant reads).
	}

	validations = make(map[string]bindingValidation, len(bindings))
	for i := range bindings {
		b := &bindings[i]
		v := validateBinding(b, registries)
		validations[b.Name] = v
		if !v.Valid {
			continue
		}
		valid = append(valid, toolmanifest.Binding{
			BindingName: b.Name,
			ToolName:    b.Spec.ToolName,
			Mode:        b.Spec.Mode,
			Image:       b.Spec.Server.Image,
			URL:         b.Spec.Server.URL,
			// Carry the matched catalog entry's argument JSON Schema verbatim
			// into the manifest so the managed loop (m14.6b) can hand a real
			// model exact tool-call parameters. Nil when the entry has none
			// (curated/legacy) → manifest omits it → SDK permissive fallback.
			InputSchema: registryInputSchema(b, registries),
		})
	}
	return valid, validations, nil
}
