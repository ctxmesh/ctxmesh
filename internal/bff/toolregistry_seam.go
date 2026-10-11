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
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// The ToolRegistry read seam (M45 / ADR 0044, the full-retirement phase). The MCP
// subsystem reads a server's ToolRegistry all over (register, approval, OAuth,
// grant, org, delete). Retiring the CRD means those reads leave the K8s API, so
// this seam routes them to the Postgres store — behind a caller-scoped SSAR for
// exact RBAC parity — and shapes store/authz errors as k8s apierrors so every
// caller's existing error handling (apierrors.IsNotFound / classifyReadError /
// writeMCPReadError) works unchanged.
//
// Gated on retireToolRegistry (NOT store-wired): the MCP subsystem's reads AND
// writes cut over to Postgres ATOMICALLY at the single RETIRE_TR flip, so there is
// never a dual-write window where a CRD write hasn't yet reached the store a read
// consults (read-after-write matters in the register→configure→approve flows).

// toolRegistryGroupResource is the GroupResource for the k8s-shaped errors the seam
// synthesizes.
func toolRegistryGroupResource() schema.GroupResource {
	return schema.GroupResource{Group: agentsv1beta1.GroupVersion.Group, Resource: resourceToolRegistries}
}

// storeToolRegistryToCRD projects a control-plane store row into the CRD shape the
// MCP subsystem reads — the BFF twin of the controller's storeRegistryToCRD. The
// MCP flows read spec.tools (name/image/url/description/source/approvalStatus/
// inputSchema), the annotations (OAuth-client config, ADR 0028), and the labels
// (scope/owner, ADR 0029). ToolRegistry has no controller, so status stays empty —
// no MCP read path consults it.
func storeToolRegistryToCRD(r *toolregistry.ToolRegistry) *agentsv1beta1.ToolRegistry {
	out := &agentsv1beta1.ToolRegistry{}
	out.Namespace = r.Namespace
	out.Name = r.Name
	if len(r.Annotations) > 0 {
		out.Annotations = r.Annotations
	}
	if len(r.Labels) > 0 {
		out.Labels = r.Labels
	}
	if len(r.Tools) > 0 {
		out.Spec.Tools = make([]agentsv1beta1.ToolEntry, len(r.Tools))
		for i := range r.Tools {
			e := r.Tools[i]
			te := agentsv1beta1.ToolEntry{
				Name: e.Name, Image: e.Image, URL: e.URL,
				Description: e.Description, Source: e.Source, ApprovalStatus: e.ApprovalStatus,
			}
			if len(e.InputSchema) > 0 {
				te.InputSchema = &runtime.RawExtension{Raw: e.InputSchema}
			}
			out.Spec.Tools[i] = te
		}
	}
	return out
}

// toolRegistryReadErrAsAPIError maps store/authz read errors to k8s apierrors so
// MCP callers stay unchanged: a denial → Forbidden (403), a miss → NotFound (404),
// anything else → itself (callers' default 500 — fail-closed on a store/SSAR error).
func toolRegistryReadErrAsAPIError(err error, name string) error {
	switch {
	case errors.Is(err, authz.ErrForbidden):
		return apierrors.NewForbidden(toolRegistryGroupResource(), name, err)
	case errors.Is(err, controlplane.ErrNotFound):
		return apierrors.NewNotFound(toolRegistryGroupResource(), name)
	default:
		return err
	}
}

// mcpGetToolRegistry reads one server's ToolRegistry from the Postgres store
// (projected to the CRD shape) behind a caller-scoped SSAR (RBAC parity). Store /
// authz errors are shaped as k8s apierrors so the MCP callers' handling is
// unchanged (ToolRegistry is retired, ADR 0044 — there is no CRD to read).
func (s *Server) mcpGetToolRegistry(ctx context.Context, caller client.Client, ns, name string) (*agentsv1beta1.ToolRegistry, error) {
	if err := s.authorizeStore(ctx, caller, authz.VerbGet, resourceToolRegistries, ns, name); err != nil {
		return nil, toolRegistryReadErrAsAPIError(err, name)
	}
	r, err := s.toolRegistryStore.Get(ctx, ns, name)
	if err != nil {
		return nil, toolRegistryReadErrAsAPIError(err, name)
	}
	return storeToolRegistryToCRD(r), nil
}

// mcpListToolRegistries lists ToolRegistries from the store (SSAR VerbList + paged,
// projected), optionally namespace-scoped and label-filtered (e.g.
// managed-by=ctxmesh-mcp for the BYO-server surfaces). labels are AND-ed
// equality filters, matching the store's ListOptions.Labels.
//
// An empty ns lists every namespace the caller may list registries in. A caller allowed
// cluster-wide gets them all in one check; a caller bound per namespace (the chart's
// default RoleBindings, which never grant a cluster-wide list) gets the registries of each
// namespace where their own `list toolregistries` is allowed, checked once per namespace
// the store holds. Only allowed namespaces reach the result, so this says nothing about the
// others. A caller allowed in none of them gets the cluster-wide denial: an empty list would
// claim they can see the catalog and it is empty.
func (s *Server) mcpListToolRegistries(ctx context.Context, caller client.Client, ns string, labels map[string]string) (*agentsv1beta1.ToolRegistryList, error) {
	var allowed map[string]bool // nil ⇒ every listed namespace is allowed
	denied := s.authorizeStore(ctx, caller, authz.VerbList, resourceToolRegistries, ns, "")
	if denied != nil {
		if ns != "" || !errors.Is(denied, authz.ErrForbidden) {
			return nil, toolRegistryReadErrAsAPIError(denied, "")
		}
		allowed = map[string]bool{}
	}
	mayList := func(namespace string) (bool, error) {
		if allowed == nil {
			return true, nil
		}
		if ok, seen := allowed[namespace]; seen {
			return ok, nil
		}
		err := s.authorizeStore(ctx, caller, authz.VerbList, resourceToolRegistries, namespace, "")
		switch {
		case err == nil:
			allowed[namespace] = true
		case errors.Is(err, authz.ErrForbidden):
			allowed[namespace] = false
		default:
			return false, toolRegistryReadErrAsAPIError(err, "")
		}
		return allowed[namespace], nil
	}

	out := &agentsv1beta1.ToolRegistryList{}
	token := ""
	for {
		page, err := s.toolRegistryStore.List(ctx, controlplane.ListOptions{
			Namespace: ns, Labels: labels, PageSize: controlplane.MaxPageSize, PageToken: token,
		})
		if err != nil {
			return nil, toolRegistryReadErrAsAPIError(err, "")
		}
		for i := range page.Items {
			ok, mErr := mayList(page.Items[i].Namespace)
			if mErr != nil {
				return nil, mErr
			}
			if ok {
				out.Items = append(out.Items, *storeToolRegistryToCRD(&page.Items[i]))
			}
		}
		if page.NextPage == "" {
			break
		}
		token = page.NextPage
	}
	// Every allowed namespace was probed because it holds a registry, so none allowed ⇔ none listed.
	if allowed != nil && len(out.Items) == 0 {
		return nil, toolRegistryReadErrAsAPIError(denied, "")
	}
	return out, nil
}
