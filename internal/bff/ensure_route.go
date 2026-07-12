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

// routeNameForModel derives a DETERMINISTIC RFC-1123 route name for a (provider, model)
// pair, so the same model always maps to the same route (idempotent reuse). e.g.
// (anthropic, claude-sonnet-5) → "anthropic-claude-sonnet-5". Capped at 63 chars.
func routeNameForModel(provider, model string) string {
	base := strings.ToLower(strings.TrimSpace(provider)) + "-" +
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

// ensureRouteForModel get-or-creates a ModelRoute serving the given (provider, model),
// reusing the provider's connect SecretBinding, and returns the route name. It is the
// m21 seam that lets a user pick a MODEL and have the platform manage the ROUTE:
//
//   - Idempotent — a route with the deterministic name already existing is REUSED, never
//     duplicated, so N agents on the same model share ONE route.
//   - Reuses the provider's SecretBinding (named after the provider by the connect flow);
//     it creates NO new Secret/binding — only a route.
//   - Caller-scoped (ADR 0011): the get + create run as the caller, so RBAC is enforced
//     (a caller who can't create ModelRoutes gets an honest 403).
//   - Labelled managedByModelPicker so it is NOT mistaken for a connected provider.
//
//nolint:unparam // namespace varies once m21.3 wires this into the create/generate handler.
func ensureRouteForModel(ctx context.Context, caller client.Client, scheme *runtime.Scheme, namespace, provider, model string) (string, *createError) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return "", &createError{status: 400, msg: "provider and model are required to route a model"}
	}
	ns := namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}
	name := routeNameForModel(provider, model)

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
				labelProvider:  provider,
			},
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider: provider,
				Model:    model,
				Priority: 1,
				// The connect flow names the provider's SecretBinding after the provider
				// route name; reuse it so no new Secret/binding is created — only a route.
				SecretBindingRef: providerRouteName(provider),
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
