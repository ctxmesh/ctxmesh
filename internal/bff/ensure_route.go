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
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// managedByModelPicker marks a ModelRoute auto-created by the create-agent model
// picker (m21) — distinct from managedByConnect so listProviders (which filters on the
// connect label) never mistakes an auto route for a connected provider.
const managedByModelPicker = "agent-engine-model-picker"

// routeNameForModel derives a DETERMINISTIC RFC-1123 route name for a (connection, model)
// pair, so the same model always maps to the same route (idempotent reuse). e.g.
// (anthropic, claude-sonnet-5) → "anthropic-claude-sonnet-5"; a named connection
// (anthropic-prod, claude-sonnet-5) → "anthropic-prod-claude-sonnet-5". Capped at 63 chars.
func routeNameForModel(connection, model string) string {
	base := strings.ToLower(strings.TrimSpace(connection)) + "-" +
		strings.ToLower(strings.TrimSpace(model))
	base = rfc1123Invalid.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "model-route"
	}
	if len(base) > 63 {
		base = strings.Trim(base[:63], "-")
	}
	return base
}

// ensureRouteForModel get-or-creates a ModelRoute serving the given (connection, model),
// reusing the CONNECTION's SecretBinding + provider type, and returns the route name. It
// is the m21 seam (extended for named connections, ADR 0026) that lets a user pick a
// MODEL on a CONNECTION and have the platform manage the ROUTE:
//
//   - Idempotent — a route with the deterministic name already existing is REUSED, never
//     duplicated, so N agents on the same connection+model share ONE route.
//   - Resolves the provider TYPE + SecretBinding from the connection's connect route, so
//     the ensured route serves the right provider and reuses the connection's binding —
//     NO new Secret/binding, only a route. Falls back to treating the connection name AS
//     the provider type (the default/back-compat connection, or a mock/apiBase route).
//   - Caller-scoped (ADR 0011): the gets + create run as the caller, so RBAC is enforced.
//   - Labelled managedByModelPicker so it is NOT mistaken for a connected provider.
func ensureRouteForModel(ctx context.Context, caller client.Client, scheme *runtime.Scheme, namespace, connection, model string) (string, *createError) {
	connection = strings.ToLower(strings.TrimSpace(connection))
	model = strings.TrimSpace(model)
	if connection == "" || model == "" {
		return "", &createError{status: 400, msg: "connection and model are required to route a model"}
	}
	ns := namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// Resolve the connection's provider TYPE + SecretBinding from its connect route
	// (ADR 0026). Fallback (NotFound): the connection name IS the provider type — the
	// default connection, or a route predating named connections.
	providerType := connection
	bindingRef := providerRouteName(connection)
	var connRoute agentsv1alpha1.ModelRoute
	switch gerr := caller.Get(ctx, client.ObjectKey{Namespace: ns, Name: providerRouteName(connection)}, &connRoute); {
	case gerr == nil:
		if len(connRoute.Spec.Providers) > 0 {
			if p := connRoute.Spec.Providers[0]; p.Provider != "" {
				providerType = strings.ToLower(p.Provider)
				if p.SecretBindingRef != "" {
					bindingRef = p.SecretBindingRef
				}
			}
		}
	case apierrors.IsNotFound(gerr):
		// fall through with connection-as-type
	case apierrors.IsForbidden(gerr):
		return "", &createError{status: 403, msg: fmt.Sprintf("forbidden: not allowed to read connection %q", connection)}
	default:
		return "", &createError{status: 502, msg: fmt.Sprintf("failed to read connection %q: %v", connection, gerr)}
	}

	name := routeNameForModel(connection, model)

	// Reuse if the route already exists (idempotent — the common repeat case).
	var existing agentsv1alpha1.ModelRoute
	err := caller.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &existing)
	switch {
	case err == nil:
		return name, nil
	case apierrors.IsNotFound(err):
		// fall through to create
	case apierrors.IsForbidden(err):
		return "", &createError{status: 403, msg: fmt.Sprintf("forbidden: not allowed to read ModelRoute %q", name)}
	default:
		return "", &createError{status: 502, msg: fmt.Sprintf("failed to check ModelRoute %q: %v", name, err)}
	}

	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				labelManagedBy: managedByModelPicker,
				labelProvider:  providerType,
			},
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider:         providerType,
				Model:            model,
				Priority:         1,
				SecretBindingRef: bindingRef,
			}},
			RateLimit: &agentsv1alpha1.RateLimit{TenantRPM: defaultTenantRPM},
		},
	}
	if gerr := ensureGVK(route, scheme); gerr != nil {
		return "", &createError{status: 500, msg: "server misconfigured: cannot resolve model route kind"}
	}
	if cerr := caller.Create(ctx, route); cerr != nil {
		if apierrors.IsAlreadyExists(cerr) {
			return name, nil // raced with a concurrent create — reuse
		}
		return "", classifyCreateError(cerr, modelRouteKind, name)
	}
	return name, nil
}

// injectModelRoute sets model.route in a simplified agent.yaml to the given route, so
// a picked model's ensured route (ensureRouteForModel) becomes the agent's runtime
// model route BEFORE expand — the m21 seam that turns "pick a model" into a working
// route. It round-trips through a generic map so unrelated fields are preserved; the
// modelYAML schema is just {route}, so replacing the model block is complete.
func injectModelRoute(agentYAML []byte, routeName string) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(agentYAML, &doc); err != nil {
		return nil, fmt.Errorf("parsing agent.yaml: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	doc["model"] = map[string]any{"route": routeName}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-serializing agent.yaml: %w", err)
	}
	return out, nil
}
