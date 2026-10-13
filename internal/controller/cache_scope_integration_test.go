//go:build integration

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/kedatypes"
)

// testScope is the production scope with the suite's platform namespace, which TestMain creates.
var testScope = CacheScope{PlatformNamespace: DefaultPlatformNamespace}

// legacyCache and legacyClient are the manager's cache and client as they were before the scope: every
// kind cluster-wide, Secrets uncached. The measurement runs them side by side with the scope.
func legacyCache(cfg *rest.Config, opts cache.Options) (cache.Cache, error) {
	return cache.New(cfg, opts)
}

func legacyClient(cfg *rest.Config, opts client.Options) (client.Client, error) {
	opts.Cache.DisableFor = []client.Object{&corev1.Secret{}}
	return client.New(cfg, opts)
}

type cacheEnv struct {
	cache  cache.Cache
	client client.Client
}

// startCache builds and starts a cache and the client over it, the way the manager does, and stops the
// cache when the test ends. rec, when set, records the informers the cache creates and its watches.
func startCache(t *testing.T, newCache cache.NewCacheFunc, newClient client.NewClientFunc, rec *informerRecorder) (cacheEnv, func()) {
	t.Helper()
	cfg := testEnv.Config
	httpClient, err := rest.HTTPClientFor(cfg)
	require.NoError(t, err)
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	require.NoError(t, err)
	opts := cache.Options{HTTPClient: httpClient, Scheme: k8sClient.Scheme(), Mapper: mapper}
	if rec != nil {
		opts.NewInformer = rec.newInformer
		watching := *httpClient
		rec.next = httpClient.Transport
		watching.Transport = rec
		opts.HTTPClient = &watching
	}
	c, err := newCache(cfg, opts)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(testCtx)
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			assert.NoError(t, <-done, "the cache must stop cleanly")
		})
	}
	t.Cleanup(stop)
	require.True(t, c.WaitForCacheSync(ctx))
	cl, err := newClient(cfg, client.Options{
		HTTPClient: httpClient, Scheme: k8sClient.Scheme(), Mapper: mapper, Cache: &client.CacheOptions{Reader: c},
	})
	require.NoError(t, err)
	return cacheEnv{cache: c, client: cl}, stop
}

func ensureNamespace(t *testing.T, name string) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(testCtx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	return name
}

// scopedKindSamples returns one minimal valid object per label-scoped kind, named name, in namespace ns.
func scopedKindSamples(ns, name string) []client.Object {
	meta := metav1.ObjectMeta{Name: name, Namespace: ns}
	pod := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example.test/agent:1"}}}
	jobPod := *pod.DeepCopy()
	jobPod.RestartPolicy = corev1.RestartPolicyNever
	sel := map[string]string{"app": name}
	return []client.Object{
		&corev1.ConfigMap{ObjectMeta: meta},
		&corev1.Service{ObjectMeta: meta, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}}},
		&corev1.ResourceQuota{ObjectMeta: meta, Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("1")},
		}},
		&appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: sel}, Spec: pod},
		}},
		&batchv1.Job{ObjectMeta: meta, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: jobPod}}},
		&batchv1.CronJob{ObjectMeta: meta, Spec: batchv1.CronJobSpec{
			Schedule:    "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: jobPod}}},
		}},
		&networkingv1.NetworkPolicy{ObjectMeta: meta},
		&servingv1.Service{ObjectMeta: meta, Spec: servingv1.ServiceSpec{ConfigurationSpec: servingv1.ConfigurationSpec{
			Template: servingv1.RevisionTemplateSpec{Spec: servingv1.RevisionSpec{PodSpec: pod}},
		}}},
		&eventingv1.Trigger{ObjectMeta: meta, Spec: eventingv1.TriggerSpec{
			Broker:     "default",
			Subscriber: duckv1.Destination{Ref: &duckv1.KReference{APIVersion: "v1", Kind: "Service", Name: name}},
		}},
		&eventingv1.Broker{ObjectMeta: meta},
		&kedatypes.ScaledObject{ObjectMeta: meta, Spec: kedatypes.ScaledObjectSpec{
			ScaleTargetRef: &kedatypes.ScaleTarget{Name: name},
			Triggers:       []kedatypes.ScaleTriggers{{Type: "cpu", Metadata: map[string]string{"value": "50"}}},
		}},
	}
}

func kindOf(t *testing.T, obj k8sruntime.Object) string {
	t.Helper()
	gvk, err := apiutil.GVKForObject(obj, k8sClient.Scheme())
	require.NoError(t, err)
	return gvk.GroupKind().String()
}

// TestCacheScopeHoldsEveryKindTheControllerWrites is the invariant the scope rests on: what the
// controller writes, its own cache sees. A selector that does not match the stamped label makes the
// controller blind to its own objects' changes, while every read still succeeds through the fallback.
func TestCacheScopeHoldsEveryKindTheControllerWrites(t *testing.T) {
	ns := ensureNamespace(t, "cache-scope-kinds")
	env, _ := startCache(t, testScope.NewCache, testScope.NewClient, nil)
	samples := scopedKindSamples(ns, "written")
	require.Len(t, samples, len(labelScopedKinds()), "one sample per label-scoped kind")

	for i, written := range samples {
		unlabelled := scopedKindSamples(ns, "someone-elses")[i]
		marker := scopedKindSamples(ns, "marker")[i]
		t.Run(kindOf(t, written), func(t *testing.T) {
			require.NoError(t, env.client.Create(testCtx, written))
			t.Cleanup(func() { _ = k8sClient.Delete(testCtx, written) })
			require.Eventually(t, func() bool {
				fresh := written.DeepCopyObject().(client.Object)
				return env.cache.Get(testCtx, client.ObjectKeyFromObject(written), fresh) == nil
			}, 10*time.Second, 50*time.Millisecond, "the controller's own object must reach its cache")

			// Not vacuous: the cache really filters. An object written without the label is not cached;
			// the marker written after it is, so by then the watch has delivered both.
			require.NoError(t, k8sClient.Create(testCtx, unlabelled))
			t.Cleanup(func() { _ = k8sClient.Delete(testCtx, unlabelled) })
			require.NoError(t, env.client.Create(testCtx, marker))
			t.Cleanup(func() { _ = k8sClient.Delete(testCtx, marker) })
			require.Eventually(t, func() bool {
				fresh := marker.DeepCopyObject().(client.Object)
				return env.cache.Get(testCtx, client.ObjectKeyFromObject(marker), fresh) == nil
			}, 10*time.Second, 50*time.Millisecond)
			fresh := unlabelled.DeepCopyObject().(client.Object)
			err := env.cache.Get(testCtx, client.ObjectKeyFromObject(unlabelled), fresh)
			assert.True(t, apierrors.IsNotFound(err), "an object without the label must not be cached, got %v", err)
		})
	}
}

// TestCacheScopeReadsFindObjectsTheControllerDidNotLabel covers every read of a label-scoped kind whose
// object the controller did not create, or created before the label existed.
func TestCacheScopeReadsFindObjectsTheControllerDidNotLabel(t *testing.T) {
	ns := ensureNamespace(t, "cache-scope-reads")
	env, _ := startCache(t, testScope.NewCache, testScope.NewClient, nil)
	notInCache := func(t *testing.T, obj client.Object) {
		t.Helper()
		fresh := obj.DeepCopyObject().(client.Object)
		err := env.cache.Get(testCtx, client.ObjectKeyFromObject(obj), fresh)
		require.True(t, apierrors.IsNotFound(err), "the fixture must be invisible to the cache, or this proves nothing; got %v", err)
	}
	create := func(t *testing.T, obj client.Object) {
		t.Helper()
		require.NoError(t, k8sClient.Create(testCtx, obj))
		t.Cleanup(func() { _ = k8sClient.Delete(testCtx, obj) })
	}

	t.Run("the namespace default ServiceAccount, read every reconcile for its pull secrets", func(t *testing.T) {
		sa := &corev1.ServiceAccount{
			ObjectMeta:       metav1.ObjectMeta{Name: defaultServiceAccountName, Namespace: ns},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}},
		}
		create(t, sa)
		require.Eventually(t, func() bool {
			return env.cache.Get(testCtx, client.ObjectKeyFromObject(sa), &corev1.ServiceAccount{}) == nil
		}, 10*time.Second, 50*time.Millisecond, "it stays cached, or every agent reconcile costs an API call")
		var got corev1.ServiceAccount
		require.NoError(t, env.client.Get(testCtx, client.ObjectKeyFromObject(sa), &got))
		assert.Equal(t, sa.ImagePullSecrets, got.ImagePullSecrets)
	})

	t.Run("a user's ConfigMap", func(t *testing.T) {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "user-settings", Namespace: ns}, Data: map[string]string{"k": "v"}}
		create(t, cm)
		notInCache(t, cm)
		var got corev1.ConfigMap
		require.NoError(t, env.client.Get(testCtx, client.ObjectKeyFromObject(cm), &got))
		assert.Equal(t, "v", got.Data["k"])
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(cm), &got))
		assert.NotContains(t, got.Labels, ManagedByLabel)
	})

	t.Run("an object the controller wrote before the label existed", func(t *testing.T) {
		agent := &agentsv1beta1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "pre-label", Namespace: ns},
			Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "example.test/agent:1", Port: 8080},
		}
		create(t, agent)
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "pre-label-tools", Namespace: ns}}
		require.NoError(t, ctrl.SetControllerReference(agent, cm, k8sClient.Scheme()))
		create(t, cm)
		notInCache(t, cm)

		var got corev1.ConfigMap
		require.NoError(t, env.client.Get(testCtx, client.ObjectKeyFromObject(cm), &got))
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(cm), &got))
		assert.Equal(t, ManagedByValue, got.Labels[ManagedByLabel], "the read labels what the controller owns")
		require.Eventually(t, func() bool {
			return env.cache.Get(testCtx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}) == nil
		}, 10*time.Second, 50*time.Millisecond, "and from then on its changes reach the cache")
	})

	t.Run("an owned object whose label was removed", func(t *testing.T) {
		agent := &agentsv1beta1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "relabel", Namespace: ns},
			Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "example.test/agent:1", Port: 8080},
		}
		create(t, agent)
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "relabel-routes", Namespace: ns}}
		require.NoError(t, ctrl.SetControllerReference(agent, cm, k8sClient.Scheme()))
		require.NoError(t, env.client.Create(testCtx, cm))
		t.Cleanup(func() { _ = k8sClient.Delete(testCtx, cm) })
		inCache := func() bool { return env.cache.Get(testCtx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}) == nil }
		require.Eventually(t, inCache, 10*time.Second, 50*time.Millisecond)

		var stripped corev1.ConfigMap
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(cm), &stripped))
		delete(stripped.Labels, ManagedByLabel)
		require.NoError(t, k8sClient.Update(testCtx, &stripped))
		require.Eventually(t, func() bool { return !inCache() }, 10*time.Second, 50*time.Millisecond,
			"without the label the object leaves the cache, and its changes stop reaching the controller")

		var got corev1.ConfigMap
		require.NoError(t, env.client.Get(testCtx, client.ObjectKeyFromObject(cm), &got), "the next read still finds it")
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(cm), &got))
		assert.Equal(t, ManagedByValue, got.Labels[ManagedByLabel], "and puts the label back")
		require.Eventually(t, inCache, 10*time.Second, 50*time.Millisecond, "so it is cached again")
	})

	t.Run("a Tenant's quota, which has no owner in its namespace", func(t *testing.T) {
		rq := &corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: tenantQuotaName, Namespace: ns, Labels: map[string]string{agentsv1beta1.TenantLabel: "acme"}},
			Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("5")}},
		}
		create(t, rq)
		notInCache(t, rq)
		var got corev1.ResourceQuota
		require.NoError(t, env.client.Get(testCtx, client.ObjectKeyFromObject(rq), &got))
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(rq), &got))
		assert.Equal(t, ManagedByValue, got.Labels[ManagedByLabel])
	})

	t.Run("Knative's feature flags", func(t *testing.T) {
		ensureNamespace(t, knativeServingNamespace)
		features := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: knativeFeaturesConfigMap, Namespace: knativeServingNamespace}}
		if err := k8sClient.Create(testCtx, features); err == nil {
			t.Cleanup(func() { _ = k8sClient.Delete(testCtx, features) })
		} else {
			require.True(t, apierrors.IsAlreadyExists(err), "creating config-features: %v", err)
		}
		require.Eventually(t, func() bool {
			return env.cache.Get(testCtx, client.ObjectKeyFromObject(features), &corev1.ConfigMap{}) == nil
		}, 10*time.Second, 50*time.Millisecond, "config-features is read every agent reconcile: it stays cached")

		other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "config-network-test", Namespace: knativeServingNamespace}}
		create(t, other)
		notInCache(t, other)
		require.NoError(t, env.client.Get(testCtx, client.ObjectKeyFromObject(other), &corev1.ConfigMap{}))
	})

	t.Run("the chart's objects in the platform namespace", func(t *testing.T) {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cache-scope-chart-config", Namespace: DefaultPlatformNamespace}}
		create(t, cm)
		require.Eventually(t, func() bool {
			return env.cache.Get(testCtx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}) == nil
		}, 10*time.Second, 50*time.Millisecond, "the gateway's ConfigMap is the chart's, unlabelled, and read every ModelRoute reconcile")
		var got corev1.ConfigMap
		require.NoError(t, env.client.Get(testCtx, client.ObjectKeyFromObject(cm), &got))
		assert.NotContains(t, got.Labels, ManagedByLabel)
	})

	t.Run("a prompt ConfigMap list includes the unlabelled ones", func(t *testing.T) {
		old := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "lister-prompt-old", Namespace: ns, Labels: map[string]string{promptAgentLabel: "lister"},
		}}
		create(t, old)
		var cms corev1.ConfigMapList
		require.NoError(t, env.client.List(testCtx, &cms, client.InNamespace(ns), client.MatchingLabels{promptAgentLabel: "lister"}))
		require.Len(t, cms.Items, 1)
		assert.Equal(t, old.Name, cms.Items[0].Name)
	})
}

// recordedInformer is one informer the cache created.
type recordedInformer struct {
	kind     string
	informer toolscache.SharedIndexInformer
}

// informerRecorder records every informer a cache creates (as its NewInformer) and every watch it opens
// (as its transport), the second with the selectors it actually sends.
type informerRecorder struct {
	t       *testing.T
	next    http.RoundTripper
	mu      sync.Mutex
	all     []*recordedInformer
	watches map[string]bool
}

func (r *informerRecorder) newInformer(lw toolscache.ListerWatcher, obj k8sruntime.Object, resync time.Duration,
	indexers toolscache.Indexers,
) toolscache.SharedIndexInformer {
	rec := &recordedInformer{}
	if m, ok := obj.(*metav1.PartialObjectMetadata); ok {
		rec.kind = m.GroupVersionKind().GroupKind().String() + " (metadata)"
	} else {
		rec.kind = kindOf(r.t, obj)
	}
	rec.informer = toolscache.NewSharedIndexInformer(lw, obj, resync, indexers)
	r.mu.Lock()
	r.all = append(r.all, rec)
	r.mu.Unlock()
	return rec.informer
}

func (r *informerRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if q := req.URL.Query(); q.Get("watch") == "true" {
		r.mu.Lock()
		if r.watches == nil {
			r.watches = map[string]bool{}
		}
		r.watches[fmt.Sprintf("%s labelSelector=%q fieldSelector=%q", req.URL.Path, q.Get("labelSelector"), q.Get("fieldSelector"))] = true
		r.mu.Unlock()
	}
	return r.next.RoundTrip(req)
}

// useLikeTheController starts, in env, the informers the manager starts for kinds other than ctxmesh's
// own, the way it starts them. The list comes from reading the code: the Owns and Watches in every
// SetupWithManager, and the Get and List calls the reconcilers make on those kinds. The thirteen
// ctxmesh kinds are watched cluster-wide either way and are left out.
func useLikeTheController(t *testing.T, env cacheEnv, ns string) {
	t.Helper()
	ctx := testCtx
	watched := []client.Object{
		&servingv1.Service{}, &eventingv1.Trigger{}, &eventingv1.Broker{}, &batchv1.Job{}, &batchv1.CronJob{},
		&appsv1.Deployment{}, &corev1.Service{}, &networkingv1.NetworkPolicy{}, &kedatypes.ScaledObject{},
		&corev1.Namespace{},
		&metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}},
	}
	for _, obj := range watched {
		_, err := env.cache.GetInformer(ctx, obj)
		require.NoError(t, err, "watching %T", obj)
	}
	reads := []struct {
		key client.ObjectKey
		obj client.Object
	}{
		{client.ObjectKey{Namespace: ns, Name: "absent"}, &corev1.ConfigMap{}},
		{client.ObjectKey{Namespace: DefaultPlatformNamespace, Name: "absent-gateway-config"}, &corev1.ConfigMap{}},
		{client.ObjectKey{Namespace: knativeServingNamespace, Name: knativeFeaturesConfigMap}, &corev1.ConfigMap{}},
		{client.ObjectKey{Namespace: ns, Name: "absent"}, &corev1.ServiceAccount{}},
		{client.ObjectKey{Namespace: ns, Name: "absent"}, &corev1.ResourceQuota{}},
		{client.ObjectKey{Namespace: ns, Name: "absent"}, &corev1.Secret{}},
		{client.ObjectKey{Name: "absent"}, &admissionregistrationv1.ValidatingWebhookConfiguration{}},
	}
	for _, rd := range reads {
		if err := env.client.Get(ctx, rd.key, rd.obj); err != nil {
			require.True(t, apierrors.IsNotFound(err), "reading %T %s: %v", rd.obj, rd.key, err)
		}
	}
	require.NoError(t, env.client.List(ctx, &servingv1.RevisionList{}, client.InNamespace(ns)))
}

// cacheFootprint is what one cache held once synced.
type cacheFootprint struct {
	informers int
	kinds     int
	objects   int
	unrelated int
	bytes     int
	heapBytes int64
	rows      []string
	watches   []string
	// unrelated objects held, and their size as JSON, per kind
	unrelatedByKind      map[string]int
	unrelatedBytesByKind map[string]int
}

func (f cacheFootprint) unrelatedBytes() int {
	total := 0
	for _, b := range f.unrelatedBytesByKind {
		total += b
	}
	return total
}

func heapInUse() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

func measureCache(t *testing.T, newCache cache.NewCacheFunc, newClient client.NewClientFunc, ns, unrelatedPrefix string) cacheFootprint {
	t.Helper()
	rec := &informerRecorder{t: t}
	before := heapInUse()
	env, stop := startCache(t, newCache, newClient, rec)
	useLikeTheController(t, env, ns)
	require.True(t, env.cache.WaitForCacheSync(testCtx))
	after := heapInUse()

	fp := cacheFootprint{heapBytes: after - before, unrelatedByKind: map[string]int{}, unrelatedBytesByKind: map[string]int{}}
	kinds := map[string]bool{}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, r := range rec.all {
		fp.informers++
		kinds[strings.TrimSuffix(r.kind, " (metadata)")] = true
		items := r.informer.GetStore().List()
		unrelated, size := 0, 0
		for _, item := range items {
			b, err := json.Marshal(item)
			require.NoError(t, err)
			size += len(b)
			if m, ok := item.(metav1.Object); ok && strings.HasPrefix(m.GetNamespace(), unrelatedPrefix) {
				unrelated++
				fp.unrelatedBytesByKind[r.kind] += len(b)
			}
		}
		fp.objects += len(items)
		fp.unrelated += unrelated
		fp.bytes += size
		fp.unrelatedByKind[r.kind] += unrelated
		fp.rows = append(fp.rows, fmt.Sprintf("%-60s objects=%4d unrelated=%4d bytes=%7d", r.kind, len(items), unrelated, size))
	}
	for w := range rec.watches {
		fp.watches = append(fp.watches, w)
	}
	fp.kinds = len(kinds)
	slices.Sort(fp.rows)
	slices.Sort(fp.watches)
	stop()
	return fp
}

// TestCacheScopeDoesNotCacheUnrelatedObjects measures the manager's cache with and without the scope
// over the same cluster: 500 ConfigMaps, 500 Secrets, 100 Services and 50 Deployments in ten namespaces
// the controller has nothing to do with, beside a few objects it wrote.
func TestCacheScopeDoesNotCacheUnrelatedObjects(t *testing.T) {
	const (
		prefix        = "unrelated-"
		namespaces    = 10
		perNamespace  = 50
		svcPerNS      = 10
		deploysPerNS  = 5
		workers       = 16
		lastAppliedOf = `{"apiVersion":"v1","kind":"Secret","data":{"token":"c2VjcmV0LXZhbHVl"}}`
	)
	ctrlNS := ensureNamespace(t, "cache-scope-measure")
	fixtures := make([]client.Object, 0, namespaces*(2*perNamespace+svcPerNS+deploysPerNS))
	for n := range namespaces {
		ns := ensureNamespace(t, fmt.Sprintf("%s%d", prefix, n))
		sel := map[string]string{"app": "unrelated"}
		for i := range perNamespace {
			fixtures = append(fixtures,
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cm-%d", i), Namespace: ns},
					Data:       map[string]string{"settings.yaml": strings.Repeat("x", 512)},
				},
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("secret-%d", i), Namespace: ns,
						Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": lastAppliedOf},
					},
					Data: map[string][]byte{"token": []byte("secret-value")},
				},
			)
		}
		for i := range svcPerNS {
			fixtures = append(fixtures, &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("svc-%d", i), Namespace: ns},
				Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
			})
		}
		for i := range deploysPerNS {
			fixtures = append(fixtures, &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("app-%d", i), Namespace: ns},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: sel},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: sel},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.test/app:1"}}},
					},
				},
			})
		}
	}
	createAll(t, fixtures, workers)
	t.Cleanup(func() {
		for n := range namespaces {
			ns := client.InNamespace(fmt.Sprintf("%s%d", prefix, n))
			for _, obj := range []client.Object{&corev1.ConfigMap{}, &corev1.Secret{}, &corev1.Service{}, &appsv1.Deployment{}} {
				_ = k8sClient.DeleteAllOf(testCtx, obj, ns)
			}
		}
	})

	// What the controller itself wrote: these must stay cached.
	writer, stopWriter := startCache(t, testScope.NewCache, testScope.NewClient, nil)
	for _, obj := range scopedKindSamples(ctrlNS, "ours") {
		require.NoError(t, writer.client.Create(testCtx, obj))
		t.Cleanup(func() { _ = k8sClient.Delete(testCtx, obj) })
	}
	stopWriter()

	before := measureCache(t, legacyCache, legacyClient, ctrlNS, prefix)
	after := measureCache(t, testScope.NewCache, testScope.NewClient, ctrlNS, prefix)

	for _, m := range []struct {
		name string
		fp   cacheFootprint
	}{{"before (cluster-wide)", before}, {"after (scoped)", after}} {
		t.Logf("%s: %d informers over %d kinds; %d objects held, %d of them unrelated; %d bytes as JSON (%d unrelated); heap retained %d KiB",
			m.name, m.fp.informers, m.fp.kinds, m.fp.objects, m.fp.unrelated, m.fp.bytes, m.fp.unrelatedBytes(), m.fp.heapBytes/1024)
		for _, row := range m.fp.rows {
			t.Logf("  informer %s", row)
		}
		for _, w := range m.fp.watches {
			t.Logf("  watch    %s", w)
		}
	}

	require.Equal(t, namespaces*perNamespace, before.unrelatedByKind["ConfigMap"],
		"the fixture must be what the cluster-wide cache holds, or the comparison proves nothing")
	for _, kind := range []string{"ConfigMap", "Service", "Deployment.apps"} {
		assert.Zero(t, after.unrelatedByKind[kind], "no unrelated %s may be cached", kind)
	}
	const secrets = "Secret (metadata)"
	assert.Equal(t, namespaces*perNamespace, after.unrelatedByKind[secrets],
		"the Secret watch stays cluster-wide: the Secrets it follows are the users', unlabelled, in any namespace")
	assert.Equal(t, after.unrelated, after.unrelatedByKind[secrets], "the Secret watch is the only unrelated thing left")
	assert.Less(t, after.unrelatedBytesByKind[secrets], before.unrelatedBytesByKind[secrets]/2,
		"of each unrelated Secret the cache keeps only its identity, not the manifest kubectl left in its annotations")
	assert.Less(t, after.bytes, before.bytes)
	for _, row := range after.rows {
		if strings.HasPrefix(row, "Revision.") || strings.HasPrefix(row, "ValidatingWebhookConfiguration.") {
			t.Errorf("a kind read live must not get an informer: %s", row)
		}
	}
}

// createAll creates objs through the raw client, concurrently, tolerating ones left by an earlier run.
func createAll(t *testing.T, objs []client.Object, workers int) {
	t.Helper()
	work := make(chan client.Object)
	errs := make(chan error, len(objs))
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for obj := range work {
				if err := k8sClient.Create(testCtx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
					errs <- fmt.Errorf("creating %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
				}
			}
		})
	}
	for _, obj := range objs {
		work <- obj
	}
	close(work)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// TestCacheScopeDeliversTheEventsTheReconcilerNeeds runs the AgentDeployment reconciler on a manager wired
// as cmd/main.go wires it. An owned workload's deletion is seen only through the watch on its kind, so
// its re-creation proves the scoped cache still delivers the event, for an object labelled at birth and
// for one an earlier controller wrote without the label.
func TestCacheScopeDeliversTheEventsTheReconcilerNeeds(t *testing.T) {
	ns := ensureNamespace(t, "cache-scope-events")

	// An agent reconciled by a controller that predates the label: its Job carries none.
	upgraded := createExecAgent(t, "upgraded-job-agent", ns, "job", nil)
	reconcileNN(t, newReconciler(), upgraded.Name, ns)
	jobKey := client.ObjectKey{Namespace: ns, Name: upgraded.Name}
	var job batchv1.Job
	require.NoError(t, k8sClient.Get(testCtx, jobKey, &job))
	require.NotContains(t, job.Labels, ManagedByLabel, "the fixture must start unlabelled")

	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 k8sClient.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		NewCache: func(cfg *rest.Config, opts cache.Options) (cache.Cache, error) {
			// Only this namespace: the manager must not reconcile what other tests leave behind.
			opts.DefaultNamespaces = map[string]cache.Config{ns: {}}
			return testScope.NewCache(cfg, opts)
		},
		NewClient: testScope.NewClient,
	})
	require.NoError(t, err)
	r := newReconciler()
	r.Client = mgr.GetClient()
	r.APIReader = mgr.GetAPIReader()
	r.Scheme = mgr.GetScheme()
	require.NoError(t, r.SetupWithManager(mgr))
	ctx, cancel := context.WithCancel(testCtx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done, "the manager must stop cleanly")
	})

	labelled := func(obj client.Object) func() bool {
		return func() bool {
			fresh := obj.DeepCopyObject().(client.Object)
			return k8sClient.Get(testCtx, client.ObjectKeyFromObject(obj), fresh) == nil &&
				fresh.GetLabels()[ManagedByLabel] == ManagedByValue
		}
	}
	recreated := func(obj client.Object, uid string) func() bool {
		return func() bool {
			fresh := obj.DeepCopyObject().(client.Object)
			return k8sClient.Get(testCtx, client.ObjectKeyFromObject(obj), fresh) == nil && string(fresh.GetUID()) != uid
		}
	}
	// deleteWhenSettled deletes obj once the reconciler has stopped writing it, so nothing but the watch on
	// its kind can bring it back.
	deleteWhenSettled := func(t *testing.T, obj client.Object) string {
		t.Helper()
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(obj), obj))
		for {
			rv := obj.GetResourceVersion()
			time.Sleep(time.Second)
			require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(obj), obj))
			if obj.GetResourceVersion() == rv {
				break
			}
		}
		uid := string(obj.GetUID())
		require.NoError(t, k8sClient.Delete(testCtx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)))
		return uid
	}

	t.Run("a Job written before the label", func(t *testing.T) {
		obj := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: ns}}
		require.Eventually(t, labelled(obj), 30*time.Second, 100*time.Millisecond,
			"the first reconcile after the upgrade must label the Job it reads")
		uid := deleteWhenSettled(t, obj)
		require.Eventually(t, recreated(obj, uid), 30*time.Second, 100*time.Millisecond,
			"the Job's deletion must reach the reconciler, which re-creates it")
	})

	t.Run("a Knative Service written by this controller", func(t *testing.T) {
		agent := createExecAgent(t, "scoped-serving-agent", ns, "serving", nil)
		obj := &servingv1.Service{ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: ns}}
		require.Eventually(t, labelled(obj), 30*time.Second, 100*time.Millisecond, "the ksvc is labelled at birth")
		uid := deleteWhenSettled(t, obj)
		require.Eventually(t, recreated(obj, uid), 30*time.Second, 100*time.Millisecond,
			"the ksvc's deletion must reach the reconciler, which re-creates it")
	})
}
