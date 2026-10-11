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
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/auditlog"
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
	managedByConnect = "ctxmesh-connect"
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
	// annProviderModels persists the FULL list of models the connect probe
	// discovered (comma-separated), so a later model picker can offer EVERY model
	// the provider serves — not just the one primaryModel() routes to. Without it
	// the list projection only knows the single routed model (the "8 of 9 models
	// vanished" gap). Not secret material — just model ids.
	annProviderModels = "agents.ctxmesh.ai/provider-models"
)

// secretKeyAPIKey is the key within the created Secret's data map that holds the
// provider API key. The SecretBinding's secretRef.key points at this.
const secretKeyAPIKey = "api-key"

const (
	// resourceKindProvider is the audit ResourceKind for a connect/rotate event (ADR 0056 §2).
	resourceKindProvider = "Provider"
	// providerNoun is the lowercase "provider" string reused as the route-name fallback, the
	// GET-error resource label, and the audit detail key — one const so the literal isn't repeated.
	providerNoun = "provider"
)

// secretKind names the Secret object kind for createError messages. (secretBindingKind
// and modelRouteKind live with their CRUD handlers, same package.)
const secretKind = "Secret"

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
		base = providerNoun
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

	// The namespace is the caller's to name. The silent fallback to "default" that used to sit
	// here meant the console wrote into a namespace the user had not selected, while the
	// capability probe asked about a different one again — probe, selection and write all
	// describing different operations. The fallback is kept for API compatibility (an older SPA
	// and any script that omits it), but it is now LOGGED, because "we picked a namespace for
	// you" is a fact a credential write should never hide.
	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = defaultCreateNamespace
		s.log.Info("connect provider: no namespace in the request, defaulting",
			"namespace", ns, "provider", req.Provider)
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
	// Objects are named by the CONNECTION (ADR 0026), so a second key for the same
	// provider type under a different connection name is a NEW connection, not a
	// rotation of the first.
	name := providerRouteName(req.connectionName())
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
		if cErr.status == http.StatusForbidden {
			// A denied connect is compliance-relevant ("who was refused") — record it, then 403.
			s.appendAudit(r.Context(), auditlog.Entry{
				Actor: s.auditActor(r.Context(), caller), Action: auditActionConnect,
				ResourceKind: resourceKindProvider, ResourceName: name, Namespace: ns, Outcome: auditOutcomeDenied,
				Detail: map[string]any{providerNoun: strings.ToLower(strings.TrimSpace(req.Provider))},
			})
		}
		writeError(w, cErr.status, cErr.msg)
		return
	}

	s.appendAudit(r.Context(), auditlog.Entry{
		Actor: s.auditActor(r.Context(), caller), Action: auditActionConnect,
		ResourceKind: resourceKindProvider, ResourceName: name, Namespace: ns,
		Detail: map[string]any{providerNoun: strings.ToLower(strings.TrimSpace(req.Provider))},
	})
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

// displayNameOrDefault returns the display name, defaulting to the connection id.
func (req *ConnectProviderRequest) displayNameOrDefault() string {
	if d := strings.TrimSpace(req.DisplayName); d != "" {
		return d
	}
	return req.connectionName()
}

// connectionName returns the named connection (ADR 0026), defaulting to the
// provider type when the caller didn't name it — so the existing single
// connection stays a connection named after its provider type (migration-free).
func (req *ConnectProviderRequest) connectionName() string {
	if c := strings.TrimSpace(req.Connection); c != "" {
		return strings.ToLower(c)
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
	// model is the route's model. Empty picks primaryModel(models), as a first connect does.
	model string
	// rateLimit is the route's rate limit. Nil stamps the connect default.
	rateLimit *agentsv1alpha1.RateLimit
	// mustExist makes every write an update of an object that already exists. A rotate replaces
	// what connect wrote and never creates, so a provider missing one of its objects is refused
	// by the pre-flight, before anything is written.
	mustExist bool
	// routeGeneration and bindingGeneration are the metadata.generation a rotate read. The rotate
	// probes the provider between that read and its writes, and a spec edited meanwhile (a re-pointed
	// endpoint) is refused rather than overwritten with what was read. Zero skips the check.
	routeGeneration, bindingGeneration int64
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
// leaves the earlier objects and the error names the one that failed. With spec.mustExist (a
// rotate) every write is an update, and a missing object is refused by the pre-flight.
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
	// model as the route's primary entry; the picker can later re-point it. The FULL
	// discovered model list is persisted as an annotation (annProviderModels) so the
	// picker can offer every model, not just the routed one — route-only annotation.
	model := spec.model
	if model == "" {
		model = primaryModel(spec.models)
	}
	rateLimit := spec.rateLimit
	if rateLimit == nil {
		rateLimit = &agentsv1alpha1.RateLimit{TenantRPM: defaultTenantRPM}
	}
	routeAnnotations := make(map[string]string, len(annotations)+1)
	maps.Copy(routeAnnotations, annotations)
	if len(spec.models) > 0 {
		routeAnnotations[annProviderModels] = strings.Join(spec.models, ",")
	}
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.name,
			Namespace:   spec.namespace,
			Labels:      labels,
			Annotations: routeAnnotations,
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				// The route's provider is the GATEWAY's word for the vendor (a LiteLLM
				// prefix), which is not always the console's word for it. "custom" is a
				// console identity — an OpenAI-compatible endpoint the operator named —
				// and LiteLLM has no such prefix; on the wire it is `openai/<model>`
				// aimed at apiBase. The label keeps the identity (routeProbeInputs reads
				// the label first), so the console still shows "custom" and re-probes it
				// as custom while the gateway sees a shape it understands.
				Provider:         routeProviderFor(provider),
				Model:            model,
				Priority:         1,
				SecretBindingRef: spec.name,
				// The base URL IS the identity of a custom provider, so it has to reach
				// the gateway, not just the re-probe annotation.
				APIBase: spec.baseURL,
			}},
			RateLimit: rateLimit,
		},
	}

	writes := []struct {
		kind       string
		o          client.Object
		generation int64
	}{
		{secretKind, secret, 0},
		{secretBindingKind, binding, spec.bindingGeneration},
		{modelRouteKind, route, spec.routeGeneration},
	}

	// PRE-FLIGHT the whole set before persisting any of it. The sequence is not transactional
	// and the Secret is written FIRST, so a denial on the third object used to leave a live
	// credential in the cluster with no route that can use it — the caller is told the connect
	// failed while their key sits there.
	//
	// A dry-run also answers what no SelfSubjectAccessReview can. An SSAR answers RBAC only;
	// this asks the API server whether the write would actually SUCCEED, which additionally
	// covers admission webhooks, ResourceQuota on Secret counts, and policy engines. The
	// capability probe gates the button; this gates the write.
	dryRun := func(ctx context.Context, w client.Client, obj client.Object, _ int64) (string, error) {
		return dryRunUpsert(ctx, w, obj)
	}
	write := func(ctx context.Context, w client.Client, obj client.Object, _ int64) (string, error) {
		return upsertObject(ctx, w, obj)
	}
	if spec.mustExist {
		dryRun = func(ctx context.Context, w client.Client, obj client.Object, generation int64) (string, error) {
			return updateExisting(ctx, w, obj.DeepCopyObject().(client.Object), generation, client.DryRunAll)
		}
		write = func(ctx context.Context, w client.Client, obj client.Object, generation int64) (string, error) {
			return updateExisting(ctx, w, obj, generation)
		}
	}
	for _, obj := range writes {
		if verb, err := dryRun(ctx, w, obj.o, obj.generation); err != nil {
			return nil, classifyProviderWriteError(err, verb, obj.kind, spec)
		}
	}

	created := make([]createdObject, 0, 3)
	for _, obj := range writes {
		if verb, err := write(ctx, w, obj.o, obj.generation); err != nil {
			return created, classifyProviderWriteError(err, verb, obj.kind, spec)
		}
		created = append(created, createdObject{
			Kind:      obj.kind,
			Name:      obj.o.GetName(),
			Namespace: obj.o.GetNamespace(),
		})
	}
	return created, nil
}

// notConnectManagedError refuses to overwrite a custom resource that connect did not write.
type notConnectManagedError struct {
	kind, name string
}

func (e *notConnectManagedError) Error() string {
	return fmt.Sprintf("%s %q exists and was not created by connecting a provider (it has no %s=%s label)",
		e.kind, e.name, labelManagedBy, managedByConnect)
}

// specChangedError refuses to overwrite a custom resource whose spec changed after the caller read it.
type specChangedError struct {
	kind, name string
}

func (e *specChangedError) Error() string {
	return fmt.Sprintf("its %s %q was changed while the key was being rotated; check it and try again", e.kind, e.name)
}

// classifyProviderWriteError maps a failed provider write to its HTTP answer. When the objects
// must already exist, a missing one is the provider being incomplete, not an upstream fault.
func classifyProviderWriteError(err error, verb, kind string, spec providerCreateSpec) *createError {
	if nc, ok := errors.AsType[*notConnectManagedError](err); ok {
		if spec.mustExist {
			return &createError{status: http.StatusConflict, msg: fmt.Sprintf("provider %q cannot be rotated: %v", spec.name, nc)}
		}
		return &createError{status: http.StatusConflict, msg: fmt.Sprintf(
			"%v; choose another connection name, or remove the %s first", nc, nc.kind)}
	}
	if sc, ok := errors.AsType[*specChangedError](err); ok {
		return &createError{status: http.StatusConflict, msg: fmt.Sprintf("provider %q cannot be rotated: %v", spec.name, sc)}
	}
	if spec.mustExist && apierrors.IsNotFound(err) {
		return &createError{status: http.StatusConflict, msg: fmt.Sprintf(
			"provider %q is incomplete: its %s %q does not exist; disconnect it and connect it again",
			spec.name, kind, spec.name)}
	}
	return classifyWriteError(err, verb, kind, spec.name)
}

// dryRunUpsert asks the API server whether the upsert would succeed, persisting nothing, and
// returns the verb of the call that answered.
//
// Create-then-Update mirrors upsertObject: a dry-run Create of an object that already exists
// returns AlreadyExists, which is not a denial — it is the signal that the real call would take
// the Update path, so the dry-run follows it there, with the same resourceVersion handling.
func dryRunUpsert(ctx context.Context, w client.Client, obj client.Object) (verb string, err error) {
	err = w.Create(ctx, obj.DeepCopyObject().(client.Object), client.DryRunAll)
	if err == nil || !apierrors.IsAlreadyExists(err) {
		return verbCreate, err
	}
	return updateExisting(ctx, w, obj.DeepCopyObject().(client.Object), 0, client.DryRunAll)
}

// upsertObject creates obj, or — if it already exists — updates it in place (the
// idempotent-connect contract, ADR 0018). The Secret's Data is replaced (rotating the
// key), the SecretBinding/ModelRoute Spec refreshed, status subresources untouched. Every
// call is CALLER-SCOPED, so the API server makes the authorization decision.
//
// The verb is threaded back to the caller so a denial names the verb that was actually
// refused — a Forbidden on update reported as "not allowed to create" sends a user to ask an
// admin for a grant they already hold.
func upsertObject(ctx context.Context, w client.Client, obj client.Object) (verb string, err error) {
	if err := w.Create(ctx, obj); err == nil || !apierrors.IsAlreadyExists(err) {
		return verbCreate, err
	}
	return updateExisting(ctx, w, obj, 0)
}

// updateExisting overwrites an existing object with obj. A rotate is an overwrite by intent:
// the caller states the new value, it does not merge with the old one.
//
// A Secret is updated unconditionally, with no resourceVersion and no prior read. Kubernetes
// allows that for core Secrets, and reading one would return its data, which would make the
// credential-writing role a credential-reading one (ADR 0136).
//
// A custom resource cannot be updated that way: the API server refuses a custom-resource update
// without metadata.resourceVersion. So its resourceVersion is read with a metadata-only Get,
// which needs only `get` on that kind and returns no spec, and the update is retried if the
// object changed in between (its controller writes status, which bumps the resourceVersion).
// The same read refuses an object connect did not write (notConnectManagedError): connection
// names are chosen by the user, and one can name a ModelRoute or SecretBinding made by hand or
// by GitOps. A non-zero wantGeneration also refuses an object whose spec changed since the caller
// read it (specChangedError). Status writes do not change the generation, so the controller's own
// status updates never trip it, and each conflict retry checks it again.
func updateExisting(ctx context.Context, w client.Client, obj client.Object, wantGeneration int64,
	opts ...client.UpdateOption,
) (string, error) {
	if _, isSecret := obj.(*corev1.Secret); isSecret {
		obj.SetResourceVersion("")
		return verbUpdate, w.Update(ctx, obj, opts...)
	}
	gvk, err := w.GroupVersionKindFor(obj)
	if err != nil {
		return verbUpdate, fmt.Errorf("resolving the kind of %q: %w", obj.GetName(), err)
	}
	verb := verbUpdate
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &metav1.PartialObjectMetadata{}
		live.SetGroupVersionKind(gvk)
		if err := w.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
			verb = verbGet
			return err
		}
		verb = verbUpdate
		if live.GetLabels()[labelManagedBy] != managedByConnect {
			return &notConnectManagedError{kind: gvk.Kind, name: obj.GetName()}
		}
		if wantGeneration != 0 && live.GetGeneration() != wantGeneration {
			return &specChangedError{kind: gvk.Kind, name: obj.GetName()}
		}
		obj.SetResourceVersion(live.GetResourceVersion())
		return w.Update(ctx, obj, opts...)
	})
	return verb, err
}

// defaultPrimaryModel is the fallback model name returned by primaryModel when the
// provider returns no models (rare — an authenticated empty list). A non-empty
// literal is required so the ModelRoute stays schema-valid (Model MinLength).
const defaultPrimaryModel = "default"

// smallTierRe / flagshipRe classify a model name for picking a sensible DEFAULT
// (M22 / U-default-model). The discovered list arrives sorted, so the old
// `models[0]` defaulted to whatever sorted first — e.g. `claude-fable-5`, a
// small/experimental tier — not a flagship. These heuristics avoid that.
var (
	smallTierRe = regexp.MustCompile(`(?i)(haiku|fable|mini|nano|small|lite|flash|embed|tiny|instant)`)
	flagshipRe  = regexp.MustCompile(`(?i)(opus|sonnet|gpt-5|gpt-4|large|ultra|pro|max)`)
)

// primaryModel picks the connection's DEFAULT route model from the discovered
// list — a **flagship**, not the alphabetically-first (M22, ADR 0026). The picker
// still lets any connected model be chosen per-agent (ADR 0025), so this only sets
// the connection's default. Falls back to the provider id when the list is empty
// (rare — an authenticated empty list) so the ModelRoute stays schema-valid.
func primaryModel(models []string) string {
	if len(models) == 0 {
		return defaultPrimaryModel
	}
	var flagship, midTier string
	for _, m := range models {
		switch {
		case smallTierRe.MatchString(m):
			continue // never default to a small/experimental tier
		case flagshipRe.MatchString(m) && flagship == "":
			flagship = m
		case midTier == "":
			midTier = m
		}
	}
	switch {
	case flagship != "":
		return flagship
	case midTier != "":
		return midTier
	default:
		return models[0] // all small-tier — better than nothing
	}
}

// handleListProviders serves GET /api/providers — the connected providers, read
// through the CALLER-SCOPED client. It lists the connect-managed ModelRoutes
// (labelled managed-by=ctxmesh-connect) and projects each onto the flat
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

	// The full model list the connect probe discovered, persisted at connect time
	// (annProviderModels), so the picker offers EVERY model — not just the routed one.
	models := make([]string, 0)
	if csv := strings.TrimSpace(mr.Annotations[annProviderModels]); csv != "" {
		for m := range strings.SplitSeq(csv, ",") {
			if m = strings.TrimSpace(m); m != "" {
				models = append(models, m)
			}
		}
	}

	secretName := ""
	for _, p := range mr.Spec.Providers {
		if secretName == "" && p.SecretBindingRef != "" {
			// The SecretBinding shares the route name in the connect flow; expose it
			// as the reference (a NAME only, never the key).
			secretName = p.SecretBindingRef
		}
	}
	// Fall back to the routed model(s) for routes created before the annotation
	// existed (or hand-authored routes), so the picker is never empty.
	if len(models) == 0 {
		for _, p := range mr.Spec.Providers {
			if p.Model != "" {
				models = append(models, p.Model)
			}
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
		s.writeGetError(w, err, providerNoun)
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

// handleRotateProviderKey serves POST /api/providers/{name}/rotate (ADR 0018). A rotate is a
// reconnect with a new key: the provider's identity is recovered from its connect-managed
// ModelRoute, the new key is validated with one live probe exactly as connect does, and the three
// objects connect wrote are rewritten through the same pre-flighted write path.
//
// The stored Secret is never read. Reading a Secret returns its data, so no shipped persona may
// get Secrets (ADR 0136), and the Secret connect writes holds only the key, so nothing in it needs
// carrying over. Every call is CALLER-SCOPED (ADR 0011).
func (s *Server) handleRotateProviderKey(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing provider name")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req RotateProviderKeyRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSONBody)
		return
	}
	if strings.TrimSpace(req.APIKey) == "" {
		writeError(w, http.StatusBadRequest, msgAPIKeyRequired)
		return
	}
	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// A rotate is audited as a connect with rotate=true (ADR 0056 §2): success after the write,
	// denied on any 403. The controller's Auditor watches custom resources only, so without this row
	// the Secret holding the new key changes unaudited.
	audit := func(provider, outcome string) {
		detail := map[string]any{"rotate": true}
		if provider != "" {
			detail[providerNoun] = strings.ToLower(provider)
		}
		s.appendAudit(r.Context(), auditlog.Entry{
			Actor: s.auditActor(r.Context(), caller), Action: auditActionConnect,
			ResourceKind: resourceKindProvider, ResourceName: name, Namespace: ns, Outcome: outcome,
			Detail: detail,
		})
	}

	var route agentsv1alpha1.ModelRoute
	if err := caller.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, &route); err != nil {
		if apierrors.IsForbidden(err) {
			audit("", auditOutcomeDenied)
		}
		s.writeGetError(w, err, providerNoun)
		return
	}
	spec, cErr := rotateSpecFromRoute(&route)
	if cErr != nil {
		writeError(w, cErr.status, cErr.msg)
		return
	}
	var binding agentsv1alpha1.SecretBinding
	if err := caller.Get(r.Context(), client.ObjectKey{Name: spec.name, Namespace: ns}, &binding); err != nil {
		if apierrors.IsForbidden(err) {
			audit(spec.provider, auditOutcomeDenied)
		}
		if apierrors.IsNotFound(err) {
			ce := classifyProviderWriteError(err, verbGet, secretBindingKind, spec)
			writeError(w, ce.status, ce.msg)
			return
		}
		s.writeGetError(w, err, "secret binding")
		return
	}
	if cErr := checkConnectBinding(&binding, spec.name); cErr != nil {
		writeError(w, cErr.status, cErr.msg)
		return
	}
	spec.bindingGeneration = binding.Generation

	// Validate the NEW key with one live probe, as connect does — a bad key is refused before
	// anything is written, never a silent rotate to a broken credential.
	models, err := providerModels(r.Context(), s.providerHTTP, spec.provider, req.APIKey, spec.baseURL)
	if err != nil {
		if pe, isPE := isProviderError(err); isPE {
			writeError(w, pe.status, pe.msg)
			return
		}
		writeError(w, http.StatusBadGateway, "provider validation failed")
		return
	}
	spec.apiKey = req.APIKey
	spec.models = models

	written, cErr := createProviderObjects(r.Context(), caller, spec)
	if cErr != nil {
		switch {
		case cErr.status == http.StatusForbidden:
			audit(spec.provider, auditOutcomeDenied)
		case len(written) > 0:
			// The pre-flight passed and a later write failed: the key may already have changed.
			audit(spec.provider, auditOutcomeError)
		}
		writeError(w, cErr.status, cErr.msg)
		return
	}
	audit(spec.provider, "")

	// The refreshed provider summary (freshly probed models). No key.
	summary := providerSummaryFromRoute(&route)
	summary.Models = models
	writeJSON(w, http.StatusOK, summary)
}

// rotateSpecFromRoute recovers what connect wrote for a provider from its ModelRoute: the name the
// three objects share, the namespace, the console's provider id (label), the display name
// (annotation), and the route's endpoint, model and rate limit, which a rotate keeps — it changes
// the key, not the routing. The endpoint is spec.providers[0].apiBase, the one the gateway calls;
// the base-url annotation is only a fallback for a route without one, and the rewrite refreshes it.
//
// A route connect did not write, or one edited out of the shape connect writes, is refused rather
// than rebuilt: the rewrite would overwrite whatever made it different.
func rotateSpecFromRoute(mr *agentsv1alpha1.ModelRoute) (providerCreateSpec, *createError) {
	refuse := func(why string) (providerCreateSpec, *createError) {
		return providerCreateSpec{}, &createError{
			status: http.StatusConflict,
			msg:    fmt.Sprintf("provider %q cannot be rotated: %s", mr.Name, why),
		}
	}
	if mr.Labels[labelManagedBy] != managedByConnect {
		return refuse(fmt.Sprintf("its ModelRoute was not created by the console's connect flow (no %s=%s label)",
			labelManagedBy, managedByConnect))
	}
	provider := strings.TrimSpace(mr.Labels[labelProvider])
	if provider == "" {
		return refuse(fmt.Sprintf("its ModelRoute has no %s label naming the provider", labelProvider))
	}
	if len(mr.Spec.Providers) != 1 || mr.Spec.Providers[0].SecretBindingRef != mr.Name {
		return refuse(fmt.Sprintf("its ModelRoute was changed after connect; it must route one provider "+
			"through the SecretBinding %q", mr.Name))
	}
	ref := mr.Spec.Providers[0]
	baseURL := ref.APIBase
	if baseURL == "" {
		baseURL = mr.Annotations[annBaseURL]
	}
	displayName := mr.Annotations[annDisplayName]
	if displayName == "" {
		displayName = mr.Name
	}
	return providerCreateSpec{
		name:            mr.Name,
		namespace:       mr.Namespace,
		provider:        provider,
		displayName:     displayName,
		baseURL:         baseURL,
		model:           ref.Model,
		rateLimit:       mr.Spec.RateLimit.DeepCopy(),
		mustExist:       true,
		routeGeneration: mr.Generation,
	}, nil
}

// checkConnectBinding refuses a SecretBinding that connect did not write, or that no longer points
// where connect pointed it. A rotate writes the key into the Secret connect created; if the binding
// names another Secret, that write would not change the key the provider uses.
func checkConnectBinding(b *agentsv1alpha1.SecretBinding, name string) *createError {
	if b.Labels[labelManagedBy] != managedByConnect {
		return &createError{
			status: http.StatusConflict,
			msg: fmt.Sprintf("provider %q cannot be rotated: %v", name,
				&notConnectManagedError{kind: secretBindingKind, name: name}),
		}
	}
	if b.Spec.Backend == secretBackendKubernetes && b.Spec.SecretRef.Name == name && b.Spec.SecretRef.Key == secretKeyAPIKey {
		return nil
	}
	return &createError{
		status: http.StatusConflict,
		msg: fmt.Sprintf("provider %q cannot be rotated: its SecretBinding was changed after connect; "+
			"it must reference key %q of the Secret %q", name, secretKeyAPIKey, name),
	}
}

// handleDisconnectProvider serves DELETE /api/providers/{name} (ADR 0018). It
// deletes the connect-managed ModelRoute + SecretBinding + Secret with the CALLER'S
// client (caller-scoped; a caller without delete is denied → 403). A missing object
// is tolerated (NotFound) so a partial prior state still cleans up fully.
func (s *Server) handleDisconnectProvider(w http.ResponseWriter, r *http.Request) {
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

	// Delete route → binding → secret (dependents first). Tolerate NotFound so a
	// partial prior state cleans up; surface Forbidden as an honest 403.
	for _, obj := range []struct {
		kind string
		o    client.Object
	}{
		{modelRouteKind, &agentsv1alpha1.ModelRoute{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}},
		{secretBindingKind, &agentsv1alpha1.SecretBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}},
		{secretKind, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}},
	} {
		if err := caller.Delete(r.Context(), obj.o); err != nil && !apierrors.IsNotFound(err) {
			ce := classifyCreateError(err, obj.kind, name)
			writeError(w, ce.status, ce.msg)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// routeProviderFor maps a console provider id to the LiteLLM provider prefix the
// gateway renders. Only "custom" differs: it is an OpenAI-compatible endpoint,
// addressed as openai/<model> plus api_base.
func routeProviderFor(provider string) string {
	if provider == providerCustom {
		return providerOpenAI
	}
	return provider
}
