package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/kedatypes"
)

// ManagedByLabel marks an object of a label-scoped kind that this controller writes. The manager caches
// those kinds only where they carry it, which is what lets the controller watch its own ConfigMaps,
// Services and Deployments without holding every one in the cluster. The client from
// CacheScope.NewClient stamps it on every create and update, so no reconciler sets it by hand.
const (
	ManagedByLabel = "agents.ctxmesh.ai/managed-by"
	ManagedByValue = "ctxmesh-controller"
)

// labelScopedKinds are the kinds the controller creates and then watches or reads back. A kind missing
// here is cached cluster-wide. Every read of these kinds goes through scopedClient, which keeps it exact;
// a read straight from mgr.GetCache() sees only the labelled objects.
//
// ServiceAccount stays out: every agent reconcile reads its namespace's default ServiceAccount, which
// carries no label, so scoping it would turn each of those reads into an API call. ServiceAccounts are
// small and bounded by the number of namespaces.
func labelScopedKinds() []client.Object {
	return []client.Object{
		&corev1.ConfigMap{},
		&corev1.Service{},
		&corev1.ResourceQuota{},
		&appsv1.Deployment{},
		&batchv1.Job{},
		&batchv1.CronJob{},
		&networkingv1.NetworkPolicy{},
		&servingv1.Service{},
		&eventingv1.Trigger{},
		&eventingv1.Broker{},
		&kedatypes.ScaledObject{},
	}
}

// uncachedKinds are read straight from the API server and never get an informer. Secrets, so no tenant's
// credential is mirrored into this process (SEC-3). Revisions, which Knative creates, so they carry no
// label of ours, and which only the prompt GC lists. The ValidatingWebhookConfiguration, which is read
// once at startup.
func uncachedKinds() []client.Object {
	return []client.Object{
		&corev1.Secret{},
		&servingv1.Revision{},
		&admissionregistrationv1.ValidatingWebhookConfiguration{},
	}
}

// CacheScope configures the manager's cache and client so the controller caches only what it needs.
// Pass NewCache and NewClient as ctrl.Options.NewCache and ctrl.Options.NewClient; they must be used
// together, because the cache's label selectors only see what the client stamps.
type CacheScope struct {
	// PlatformNamespace is the install namespace. Its ConfigMaps and Deployments are cached in full: the
	// chart creates the gateway's, and the ModelRoute reconciler reads and updates them every reconcile.
	// Empty means DefaultPlatformNamespace.
	PlatformNamespace string
}

func (s CacheScope) platformNamespace() string {
	if s.PlatformNamespace == "" {
		return DefaultPlatformNamespace
	}
	return s.PlatformNamespace
}

// unfilteredNamespaces returns, for a label-scoped kind, the namespaces whose objects the cache holds
// without the managed-by label, and on what terms. The controller reads these objects but did not create
// them, so they carry no label.
func (s CacheScope) unfilteredNamespaces(kind client.Object) map[string]cache.Config {
	everything := cache.Config{LabelSelector: labels.Everything(), FieldSelector: fields.Everything()}
	switch kind.(type) {
	case *corev1.ConfigMap:
		return map[string]cache.Config{
			// Knative's feature flags, which gate launcher injection and the egress redirect.
			knativeServingNamespace: {
				LabelSelector: labels.Everything(),
				FieldSelector: fields.OneTermEqualSelector("metadata.name", knativeFeaturesConfigMap),
			},
			s.platformNamespace(): everything,
		}
	case *appsv1.Deployment:
		return map[string]cache.Config{s.platformNamespace(): everything}
	}
	return nil
}

// labelFiltered reports whether the cache holds objects of kind in namespace only when they carry the
// managed-by label: there the controller must stamp what it writes, or its own watches miss it.
func (s CacheScope) labelFiltered(kind client.Object, namespace string) bool {
	_, unfiltered := s.unfilteredNamespaces(kind)[namespace]
	return !unfiltered
}

// NewCache builds the manager cache with the label-scoped kinds filtered to the managed-by label. A kind
// the cluster does not serve (Knative Eventing and KEDA are optional) gets no entry, because cache.New
// resolves every entry against the API and refuses to start on one it cannot find. A discovery failure
// is returned: guessing "not served" would leave a kind the controller watches cached cluster-wide.
func (s CacheScope) NewCache(cfg *rest.Config, opts cache.Options) (cache.Cache, error) {
	byObject, err := s.byObject(opts.Scheme, opts.Mapper)
	if err != nil {
		return nil, err
	}
	merged := make(map[client.Object]cache.ByObject, len(opts.ByObject)+len(byObject))
	taken := map[schema.GroupVersionKind]bool{}
	for obj, bo := range opts.ByObject {
		gvk, err := apiutil.GVKForObject(obj, opts.Scheme)
		if err != nil {
			return nil, fmt.Errorf("cache scope: resolving %T: %w", obj, err)
		}
		taken[gvk] = true
		merged[obj] = bo
	}
	for obj, bo := range byObject {
		gvk, err := apiutil.GVKForObject(obj, opts.Scheme)
		if err != nil {
			return nil, fmt.Errorf("cache scope: resolving %T: %w", obj, err)
		}
		if taken[gvk] {
			return nil, fmt.Errorf("cache scope: %s already has cache options; the scope cannot also set them", gvk)
		}
		merged[obj] = bo
	}
	opts.ByObject = merged
	return cache.New(cfg, opts)
}

func (s CacheScope) byObject(scheme *runtime.Scheme, mapper meta.RESTMapper) (map[client.Object]cache.ByObject, error) {
	if scheme == nil || mapper == nil {
		return nil, errors.New("cache scope: the cache options need a scheme and a RESTMapper")
	}
	managed := labels.SelectorFromSet(labels.Set{ManagedByLabel: ManagedByValue})
	out := map[client.Object]cache.ByObject{
		// The Secret watches (gateway rotation, the per-namespace Langfuse Secret) are metadata-only and
		// must stay cluster-wide: the Secrets are the users', in any namespace, and unlabelled. The handlers
		// read only name and namespace, so the cache keeps nothing else of value. Annotations go because a
		// Secret applied with kubectl carries its whole manifest, data included, in last-applied-configuration.
		&corev1.Secret{}: {Transform: stripToIdentity},
	}
	log := ctrl.Log.WithName("cache-scope")
	for _, obj := range labelScopedKinds() {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			// Not in the scheme: the manager can neither watch nor read it.
			continue
		}
		if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			if !meta.IsNoMatchError(err) {
				return nil, fmt.Errorf("cache scope: cannot tell whether the cluster serves %s: %w", gvk, err)
			}
			log.Info("not scoping a kind the cluster does not serve; it is not watched either", "kind", gvk.String())
			continue
		}
		unfiltered := s.unfilteredNamespaces(obj)
		if len(unfiltered) == 0 {
			out[obj] = cache.ByObject{Label: managed}
			continue
		}
		namespaces := map[string]cache.Config{cache.AllNamespaces: {LabelSelector: managed}}
		maps.Copy(namespaces, unfiltered)
		out[obj] = cache.ByObject{Namespaces: namespaces}
	}
	return out, nil
}

// stripToIdentity drops a cached object's annotations and managed fields. It must stay idempotent and
// tolerate non-objects: client-go also hands transforms DeletedFinalStateUnknown tombstones.
func stripToIdentity(obj any) (any, error) {
	if m, ok := obj.(metav1.Object); ok {
		m.SetAnnotations(nil)
		m.SetManagedFields(nil)
	}
	return obj, nil
}

// NewClient builds the manager client: cache-backed reads for every kind but uncachedKinds, and
// scopedClient's handling of the label-scoped kinds.
func (s CacheScope) NewClient(cfg *rest.Config, opts client.Options) (client.Client, error) {
	cacheOpts := client.CacheOptions{}
	if opts.Cache != nil {
		cacheOpts = *opts.Cache
	}
	cacheOpts.DisableFor = append(slices.Clone(cacheOpts.DisableFor), uncachedKinds()...)
	opts.Cache = &cacheOpts
	cached, err := client.New(cfg, opts)
	if err != nil {
		return nil, err
	}
	live, err := client.New(cfg, client.Options{HTTPClient: opts.HTTPClient, Scheme: opts.Scheme, Mapper: opts.Mapper})
	if err != nil {
		return nil, err
	}
	return newScopedClient(s, cached, live), nil
}

func newScopedClient(s CacheScope, cached, live client.Client) *scopedClient {
	kinds := map[schema.GroupKind]client.Object{}
	for _, obj := range labelScopedKinds() {
		if gvk, err := cached.GroupVersionKindFor(obj); err == nil {
			kinds[gvk.GroupKind()] = obj
		}
	}
	return &scopedClient{Client: cached, live: live, scope: s, kinds: kinds}
}

// scopedClient keeps every read of a label-scoped kind exact although the cache holds only labelled
// objects of it:
//   - Get serves from the cache, and a NotFound there is not an answer: the object may predate the label,
//     or be one the controller reads but did not create (a namespace's default ServiceAccount). Those are
//     read from the API server, and one the controller owns is labelled so its changes reach the watches.
//   - List always goes to the API server, since a cached list would silently omit unlabelled objects.
//   - Create, Update and Patch stamp the label, so whatever the controller writes stays visible to it.
type scopedClient struct {
	client.Client
	live  client.Client
	scope CacheScope
	// kinds maps each label-scoped kind to an empty object of it.
	kinds map[schema.GroupKind]client.Object
}

// scopedKind returns an empty object of obj's kind when that kind is label-scoped, else nil. A list
// resolves to its items' kind.
func (c *scopedClient) scopedKind(obj runtime.Object, isList bool) client.Object {
	gvk, err := c.GroupVersionKindFor(obj)
	if err != nil {
		return nil
	}
	gk := gvk.GroupKind()
	if isList {
		gk.Kind = strings.TrimSuffix(gk.Kind, "List")
	}
	return c.kinds[gk]
}

// filtered reports whether the cache holds obj's kind in obj's namespace only when labelled.
func (c *scopedClient) filtered(obj client.Object) bool {
	kind := c.scopedKind(obj, false)
	return kind != nil && c.scope.labelFiltered(kind, obj.GetNamespace())
}

func stampManaged(obj client.Object) {
	l := obj.GetLabels()
	if l[ManagedByLabel] == ManagedByValue {
		return
	}
	if l == nil {
		l = map[string]string{}
	}
	l[ManagedByLabel] = ManagedByValue
	obj.SetLabels(l)
}

func (c *scopedClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if !apierrors.IsNotFound(err) {
		return err
	}
	if c.scopedKind(obj, false) == nil {
		return err
	}
	if err := c.live.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	return c.adopt(ctx, obj)
}

// adopt labels an object the controller owns but its cache cannot see. Anything else it read but did not
// create is left as is.
func (c *scopedClient) adopt(ctx context.Context, obj client.Object) error {
	if !c.filtered(obj) || obj.GetLabels()[ManagedByLabel] == ManagedByValue || !ownedByController(obj) {
		return nil
	}
	base, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("labelling %T %s/%s for the controller's cache: not a client.Object", obj, obj.GetNamespace(), obj.GetName())
	}
	stampManaged(obj)
	if err := c.live.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("labelling %T %s/%s for the controller's cache: %w", obj, obj.GetNamespace(), obj.GetName(), err)
	}
	return nil
}

// ownedByController reports whether obj is one the controller manages: one whose controller reference is
// one of the controller's kinds, or the quota and NetworkPolicy a Tenant stamps into each member namespace,
// which carry the tenant label and no owner reference.
func ownedByController(obj client.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err == nil && gv.Group == agentsv1beta1.GroupVersion.Group && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	if obj.GetLabels()[agentsv1beta1.TenantLabel] == "" {
		return false
	}
	switch obj.(type) {
	case *corev1.ResourceQuota:
		return obj.GetName() == tenantQuotaName
	case *networkingv1.NetworkPolicy:
		return obj.GetName() == tenantNetworkPolicyName
	}
	return false
}

func (c *scopedClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.scopedKind(list, true) != nil {
		return c.live.List(ctx, list, opts...)
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *scopedClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.filtered(obj) {
		stampManaged(obj)
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *scopedClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.filtered(obj) {
		stampManaged(obj)
	}
	return c.Client.Update(ctx, obj, opts...)
}

// Patch stamps obj before the patch is computed, which carries the label in a patch built from the object
// (client.MergeFrom, client.Apply); a raw patch carries only what its author wrote.
func (c *scopedClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.filtered(obj) {
		stampManaged(obj)
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}
