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
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// The connect-a-provider handlers (ADR 0015). All three are CALLER-SCOPED
// (ADR 0011): the Secret / SecretBinding / ModelRoute are created and read with
// the CALLER'S own client, so the K8s API server enforces the caller's RBAC — a
// viewer with no create on Secret/ModelRoute is denied by the API server and the
// 403 surfaces (never a BFF-SA fallback). The pasted key is validated once, then
// written ONLY into the Secret — it is never returned in a DTO and never logged.

// maxConnectRequestBytes bounds the connect body. The key + a couple of short
// strings are small; 64 KiB is a generous cap that stops a hostile large body.
const maxConnectRequestBytes = 64 << 10 // 64 KiB

// Discovery labels stamped on every object the connect flow creates, so
// GET /api/providers can list exactly the connect-managed routes/secrets and the
// provider is discoverable. Kept as constants so create + list agree.
const (
	// labelManagedBy marks an object as created by the connect flow.
	labelManagedBy = "app.kubernetes.io/managed-by"
	// managedByConnect is the value of labelManagedBy for connect-created objects.
	managedByConnect = "agent-engine-connect"
	// labelProvider records the LiteLLM provider prefix (for filtering/discovery).
	labelProvider = "agents.ctxmesh.ai/provider"
	// annDisplayName carries the human display name (an annotation, not a label,
	// so it is not constrained to label syntax).
	annDisplayName = "agents.ctxmesh.ai/display-name"
	// annBaseURL persists the caller's baseURL override (empty for public
	// endpoints), so the server-side re-probe (GET .../models) reaches the SAME
	// endpoint the connect probe validated — important for OpenAI-compatible /
	// self-hosted gateways. Not secret material.
	annBaseURL = "agents.ctxmesh.ai/base-url"
)

// secretKeyAPIKey is the key within the created Secret's data map that holds the
// provider API key. The SecretBinding's secretRef.key points at this.
const secretKeyAPIKey = "api-key"

// secretBackendKubernetes is the SecretBinding backend the connect flow uses —
// the same backend M2/expand produce (the only one supported in v1).
const secretBackendKubernetes = "kubernetes"

// defaultTenantRPM is the per-tenant rate limit stamped on a connect-created
// ModelRoute. A sane default the operator can later tune; keeps the route valid.
const defaultTenantRPM = 60

// rfc1123 sanitizes a provider/display string into a valid RFC-1123 subdomain
// name for use as an object name. Mirrors the expand name sanitizer's intent.
var rfc1123Invalid = regexp.MustCompile(`[^a-z0-9-]+`)

// providerRouteName derives the object name (Secret/SecretBinding/ModelRoute
// share it) from the provider id. The name is deterministic so re-connecting the
// same provider addresses the SAME objects — which the connect flow now UPSERTS
// (ADR 0018): re-connect rotates the key in place and succeeds, never a 409.
func providerRouteName(provider string) string {
	base := strings.ToLower(strings.TrimSpace(provider))
	base = rfc1123Invalid.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "provider"
	}
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	return base
}

// handleConnectProvider serves POST /api/providers (ADR 0015). It:
//  1. reads + validates the request body;
//  2. validates the pasted key against the provider with ONE live model-list
//     call (a bad key → a clean 401, never a 500);
//  3. creates — with the CALLER'S client — a Secret (the key), a SecretBinding
//     (logical name → Secret/key) and a ModelRoute (provider + models + the
//     binding ref), named/labeled for discovery.
//
// The key is used only to validate, then written ONLY into the Secret. It is
// never returned in the response and never logged. A viewer without create on
// Secret/ModelRoute is denied by the API server as the caller → the 403 surfaces.
func (s *Server) handleConnectProvider(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	req, perr := parseConnectRequest(raw)
	if perr != nil {
		writeError(w, perr.status, perr.msg)
		return
	}

	ns := req.Namespace
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// (2) Validate the key with a live probe. A bad key → 401 (honest), an
	// unreachable provider → 502 — never a 500, never a swallowed success. The
	// probe returns the initial model list the picker uses (no second round-trip).
	models, err := providerModels(r.Context(), s.providerHTTP, req.Provider, req.APIKey, req.BaseURL)
	if err != nil {
		if pe, isPE := isProviderError(err); isPE {
			writeError(w, pe.status, pe.msg)
			return
		}
		// Defensive: any non-typed error is an upstream fault, not a 500 on us.
		writeError(w, http.StatusBadGateway, "provider validation failed")
		return
	}

	// (3) Create the three objects with the CALLER'S client. The key goes ONLY
	// into the Secret; it is dropped from the request struct immediately after.
	name := providerRouteName(req.Provider)
	created, cErr := createProviderObjects(r.Context(), caller, providerCreateSpec{
		name:        name,
		namespace:   ns,
		provider:    req.Provider,
		displayName: req.displayNameOrDefault(),
		apiKey:      req.APIKey,
		baseURL:     strings.TrimSpace(req.BaseURL),
		models:      models,
	})
	if cErr != nil {
		// classifyCreateError already maps Forbidden→403, AlreadyExists→409, etc.
		// A viewer's denied create surfaces here as an honest 403, not swallowed.
		writeError(w, cErr.status, cErr.msg)
		return
	}

	writeJSON(w, http.StatusCreated, ConnectProviderResponse{
		Provider: ProviderSummary{
			Name:        name,
			Namespace:   ns,
			Provider:    strings.ToLower(strings.TrimSpace(req.Provider)),
			DisplayName: req.displayNameOrDefault(),
			Models:      models,
			SecretName:  name,
			// Freshly created — the route has not been reconciled Ready yet.
			Ready: false,
		},
		Created: created,
	})
}

// displayNameOrDefault returns the display name, defaulting to the provider id.
func (req *ConnectProviderRequest) displayNameOrDefault() string {
	if d := strings.TrimSpace(req.DisplayName); d != "" {
		return d
	}
	return strings.ToLower(strings.TrimSpace(req.Provider))
}

// parseConnectRequest decodes + validates the connect body. It returns a typed
// *createError (status + client-safe message) on a bad request. The error NEVER
// contains the key.
func parseConnectRequest(raw []byte) (ConnectProviderRequest, *createError) {
	var req ConnectProviderRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, &createError{status: http.StatusBadRequest, msg: msgInvalidJSONBody}
	}
	if strings.TrimSpace(req.Provider) == "" {
		return req, &createError{status: http.StatusBadRequest, msg: "provider is required"}
	}
	if strings.TrimSpace(req.APIKey) == "" {
		return req, &createError{status: http.StatusBadRequest, msg: msgAPIKeyRequired}
	}
	return req, nil
}

// providerCreateSpec bundles the inputs for the three-object create so the
// creation logic is one testable unit.
type providerCreateSpec struct {
	name        string
	namespace   string
	provider    string
	displayName string
	apiKey      string
	baseURL     string
	models      []string
}

// createProviderObjects UPSERTS the Secret, SecretBinding, and ModelRoute (in
// that order — the ModelRoute references the SecretBinding which references the
// Secret) with the caller's client. It returns the flat identity of every object,
// or a typed *createError with the right HTTP status on the first failure.
//
// Idempotency (ADR 0018): the object names are deterministic, so re-connecting the
// same provider addresses the SAME objects. Each is created, or — if it already
// exists — UPDATED in place (rotating the Secret key / refreshing the route). A
// re-connect therefore succeeds and rotates, never a 409. The update is still
// CALLER-SCOPED: a viewer without update on Secret/ModelRoute is denied by the API
// server → the 403 surfaces (ADR 0011). A partial upsert (K8s is not transactional)
// leaves the earlier objects and the error names the one that failed.
func createProviderObjects(ctx context.Context, w client.Client, spec providerCreateSpec) ([]createdObject, *createError) {
	provider := strings.ToLower(strings.TrimSpace(spec.provider))
	labels := map[string]string{
		labelManagedBy: managedByConnect,
		labelProvider:  provider,
	}
	annotations := map[string]string{annDisplayName: spec.displayName}
	if spec.baseURL != "" {
		annotations[annBaseURL] = spec.baseURL
	}

	// (a) Secret — holds the pasted key. This is the ONLY object the key lands in.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.name,
			Namespace:   spec.namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{secretKeyAPIKey: []byte(spec.apiKey)},
	}

	// (b) SecretBinding — logical name → Secret/key (backend kubernetes, matching
	// the shape M2/expand produce).
	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.name,
			Namespace:   spec.namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend: secretBackendKubernetes,
			SecretRef: agentsv1alpha1.SecretKeyRef{
				Name: spec.name,
				Key:  secretKeyAPIKey,
			},
		},
	}

	// (c) ModelRoute — provider + model(s) + the SecretBinding ref. The route name
	// is the model alias agents call. We register the first (sorted) discovered
	// model as the route's primary entry; the picker can later re-point it.
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.name,
			Namespace:   spec.namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider:         provider,
				Model:            primaryModel(spec.models),
				Priority:         1,
				SecretBindingRef: spec.name,
			}},
			RateLimit: &agentsv1alpha1.RateLimit{TenantRPM: defaultTenantRPM},
		},
	}

	created := make([]createdObject, 0, 3)
	for _, obj := range []struct {
		kind string
		o    client.Object
	}{
		{"Secret", secret},
		{"SecretBinding", binding},
		{"ModelRoute", route},
	} {
		if err := upsertObject(ctx, w, obj.o); err != nil {
			return created, classifyCreateError(err, obj.kind, obj.o.GetName())
		}
		created = append(created, createdObject{
			Kind:      obj.kind,
			Name:      obj.o.GetName(),
			Namespace: obj.o.GetNamespace(),
		})
	}
	return created, nil
}

// upsertObject creates obj, or — if it already exists — updates it in place (the
// idempotent-connect contract, ADR 0018). On AlreadyExists it fetches the live
// object for its resourceVersion/UID, carries them onto the desired object, and
// Updates: the Secret's Data is replaced (rotating the key), the SecretBinding/
// ModelRoute Spec is refreshed — while status subresources are untouched. Every
// call is CALLER-SCOPED; a caller without update is denied by the API server and
// the error propagates unchanged for classifyCreateError to map (Forbidden→403).
func upsertObject(ctx context.Context, w client.Client, obj client.Object) error {
	err := w.Create(ctx, obj)
	if err == nil || !apierrors.IsAlreadyExists(err) {
		return err
	}
	// Exists → read the live object to obtain its resourceVersion/UID, then Update
	// the desired object in place. A fresh copy of the SAME concrete type is the
	// Get target (Get overwrites it with cluster state).
	live, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return err // unreachable for our typed objects; keep the AlreadyExists
	}
	if getErr := w.Get(ctx, client.ObjectKeyFromObject(obj), live); getErr != nil {
		return getErr
	}
	obj.SetResourceVersion(live.GetResourceVersion())
	obj.SetUID(live.GetUID())
	return w.Update(ctx, obj)
}

// defaultPrimaryModel is the fallback model name returned by primaryModel when the
// provider returns no models (rare — an authenticated empty list). A non-empty
// literal is required so the ModelRoute stays schema-valid (Model MinLength).
const defaultPrimaryModel = "default"

// primaryModel picks the route's primary model from the discovered list. When
// the provider returned no models (rare — an authenticated empty list), it falls
// back to the provider id so the ModelRoute stays schema-valid (Model MinLength).
func primaryModel(models []string) string {
	if len(models) > 0 {
		return models[0]
	}
	return defaultPrimaryModel
}

// handleListProviders serves GET /api/providers — the connected providers, read
// through the CALLER-SCOPED client. It lists the connect-managed ModelRoutes
// (labelled managed-by=agent-engine-connect) and projects each onto the flat
// ProviderSummary — NO secret material, only the Secret NAME as a reference.
// Providers is [] (not null) for the empty case; a Forbidden on the list surfaces
// as 403, never a swallowed empty list.
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	namespace := r.URL.Query().Get("namespace")

	opts := []client.ListOption{client.MatchingLabels{labelManagedBy: managedByConnect}}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}

	var routes agentsv1alpha1.ModelRouteList
	if err := caller.List(r.Context(), &routes, opts...); err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list connected providers failed")
		writeError(w, http.StatusInternalServerError, "failed to list providers")
		return
	}

	summaries := make([]ProviderSummary, 0, len(routes.Items))
	for i := range routes.Items {
		summaries = append(summaries, providerSummaryFromRoute(&routes.Items[i]))
	}
	// Deterministic order for stable rendering + tests.
	slices.SortFunc(summaries, func(a, b ProviderSummary) int { return strings.Compare(a.Name, b.Name) })

	writeJSON(w, http.StatusOK, ProviderListResponse{Providers: summaries, Items: summaries})
}

// providerSummaryFromRoute projects a connect-managed ModelRoute onto the flat
// ProviderSummary. It carries only NON-secret material: the route/provider names,
// the models, the Secret NAME (a reference), and the Ready condition. The key is
// never touched here — it lives in the Secret, which this read never opens.
func providerSummaryFromRoute(mr *agentsv1alpha1.ModelRoute) ProviderSummary {
	provider := mr.Labels[labelProvider]
	models := make([]string, 0, len(mr.Spec.Providers))
	secretName := ""
	for _, p := range mr.Spec.Providers {
		if p.Model != "" {
			models = append(models, p.Model)
		}
		if secretName == "" && p.SecretBindingRef != "" {
			// The SecretBinding shares the route name in the connect flow; expose it
			// as the reference (a NAME only, never the key).
			secretName = p.SecretBindingRef
		}
	}
	ready := false
	if c := apimeta.FindStatusCondition(mr.Status.Conditions, "Ready"); c != nil {
		ready = c.Status == metav1.ConditionTrue
	}
	return ProviderSummary{
		Name:        mr.Name,
		Namespace:   mr.Namespace,
		Provider:    provider,
		DisplayName: mr.Annotations[annDisplayName],
		Models:      models,
		SecretName:  secretName,
		Ready:       ready,
	}
}

// handleProviderModels serves GET /api/providers/{name}/models — the provider's
// live model list, proxied SERVER-SIDE using the STORED key. It reads the
// connect-managed ModelRoute (for the provider id + SecretBinding), resolves the
// SecretBinding → Secret, reads the key from the Secret, and re-probes the
// provider — all with the CALLER'S client. The key is used only for the probe;
// the response carries the model list and NO secret material.
func (s *Server) handleProviderModels(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing provider name")
		return
	}
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// Read the route to learn the provider id + which SecretBinding holds the key.
	var route agentsv1alpha1.ModelRoute
	if err := caller.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, &route); err != nil {
		s.writeGetError(w, err, "provider")
		return
	}
	provider, secretName, baseURL := routeProbeInputs(&route)
	if secretName == "" {
		writeError(w, http.StatusConflict, "provider has no secret binding to read the key from")
		return
	}

	// Resolve the SecretBinding → Secret name/key (backend kubernetes).
	var binding agentsv1alpha1.SecretBinding
	if err := caller.Get(r.Context(), client.ObjectKey{Name: secretName, Namespace: ns}, &binding); err != nil {
		s.writeGetError(w, err, "secret binding")
		return
	}

	// Read the key from the Secret (with the CALLER'S client — a viewer without
	// secret read is denied by the API server → 403 surfaces).
	var secret corev1.Secret
	if err := caller.Get(r.Context(), client.ObjectKey{
		Name:      binding.Spec.SecretRef.Name,
		Namespace: ns,
	}, &secret); err != nil {
		s.writeGetError(w, err, "secret")
		return
	}
	keyBytes, ok := secret.Data[binding.Spec.SecretRef.Key]
	if !ok || len(keyBytes) == 0 {
		writeError(w, http.StatusConflict, "the provider secret is missing its api-key")
		return
	}

	// Re-probe the provider server-side with the stored key. The key is used only
	// for this request; the response is the model list, no secret material.
	models, err := providerModels(r.Context(), s.providerHTTP, provider, string(keyBytes), baseURL)
	if err != nil {
		if pe, isPE := isProviderError(err); isPE {
			writeError(w, pe.status, pe.msg)
			return
		}
		writeError(w, http.StatusBadGateway, "provider model list failed")
		return
	}

	writeJSON(w, http.StatusOK, ProviderModelsResponse{Provider: provider, Models: models})
}

// routeProbeInputs pulls the provider id, the SecretBinding ref, and an optional
// baseURL from a connect-managed ModelRoute for the re-probe. The provider id
// comes from the route's first provider entry (falling back to the label).
func routeProbeInputs(mr *agentsv1alpha1.ModelRoute) (provider, secretBindingRef, baseURL string) {
	provider = mr.Labels[labelProvider]
	for _, p := range mr.Spec.Providers {
		if provider == "" {
			provider = p.Provider
		}
		if secretBindingRef == "" {
			secretBindingRef = p.SecretBindingRef
		}
	}
	// baseURL is the caller's connect-time override (persisted as an annotation);
	// empty → the probe falls back to the provider default (public endpoint).
	return provider, secretBindingRef, mr.Annotations[annBaseURL]
}

// writeGetError maps a caller-scoped Get failure to an honest HTTP status: a
// Forbidden → 403 (a viewer denied by the API server), Unauthorized → 401,
// NotFound → 404 (named with the resource kind), anything else → 500. what names
// the resource kind for the 404 message.
func (s *Server) writeGetError(w http.ResponseWriter, err error, what string) {
	switch {
	case apierrors.IsForbidden(err):
		writeError(w, http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to read the %s", what))
	case apierrors.IsUnauthorized(err):
		writeError(w, http.StatusUnauthorized, "unauthorized: token rejected by the API server")
	case apierrors.IsNotFound(err):
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s not found", what))
	default:
		s.log.Error(err, "read failed", "resource", what)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to read the %s", what))
	}
}
