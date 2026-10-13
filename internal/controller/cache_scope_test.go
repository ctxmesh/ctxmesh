package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/kedatypes"
)

const scopeTestPlatformNS = "platform-ns"

func cacheScopeTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, agentsv1beta1.AddToScheme(s))
	require.NoError(t, servingv1.AddToScheme(s))
	require.NoError(t, eventingv1.AddToScheme(s))
	require.NoError(t, kedatypes.AddToScheme(s))
	return s
}

// restMapperServing maps every kind in the scheme except the ones listed: a cluster that does not serve them.
func restMapperServing(t *testing.T, s *runtime.Scheme, absent ...schema.GroupKind) meta.RESTMapper {
	t.Helper()
	m := meta.NewDefaultRESTMapper(nil)
	skip := map[schema.GroupKind]bool{}
	for _, gk := range absent {
		skip[gk] = true
	}
	for gvk := range s.AllKnownTypes() {
		if skip[gvk.GroupKind()] {
			continue
		}
		scope := meta.RESTScopeNamespace
		if gvk.Kind == "Namespace" || gvk.Kind == "ValidatingWebhookConfiguration" {
			scope = meta.RESTScopeRoot
		}
		m.Add(gvk, scope)
	}
	return m
}

func byObjectFor(t *testing.T, s *runtime.Scheme, entries map[client.Object]cache.ByObject) map[string]cache.ByObject {
	t.Helper()
	out := map[string]cache.ByObject{}
	for obj, bo := range entries {
		gvk, err := apiutil.GVKForObject(obj, s)
		require.NoError(t, err)
		out[gvk.GroupKind().String()] = bo
	}
	return out
}

func TestCacheScopeFiltersEveryScopedKindOnTheManagedLabel(t *testing.T) {
	s := cacheScopeTestScheme(t)
	scope := CacheScope{PlatformNamespace: scopeTestPlatformNS}
	entries, err := scope.byObject(s, restMapperServing(t, s))
	require.NoError(t, err)
	got := byObjectFor(t, s, entries)
	managed := labels.SelectorFromSet(labels.Set{ManagedByLabel: ManagedByValue}).String()

	for _, obj := range labelScopedKinds() {
		gvk, err := apiutil.GVKForObject(obj, s)
		require.NoError(t, err)
		bo, ok := got[gvk.GroupKind().String()]
		require.True(t, ok, "%s is label-scoped but has no cache entry: it would be cached cluster-wide", gvk.Kind)
		if bo.Namespaces == nil {
			require.NotNil(t, bo.Label, "%s must be filtered on the managed-by label", gvk.Kind)
			assert.Equal(t, managed, bo.Label.String(), "%s selector", gvk.Kind)
			continue
		}
		all, ok := bo.Namespaces[cache.AllNamespaces]
		require.True(t, ok, "%s lists namespaces but none is the catch-all: every other namespace would go uncached", gvk.Kind)
		assert.Equal(t, managed, all.LabelSelector.String(), "%s outside its unfiltered namespaces", gvk.Kind)
	}

	cm := got["ConfigMap"]
	require.Contains(t, cm.Namespaces, scopeTestPlatformNS, "the gateway ConfigMap is chart-created: the platform namespace is held in full")
	assert.True(t, cm.Namespaces[scopeTestPlatformNS].LabelSelector.Empty())
	require.Contains(t, cm.Namespaces, knativeServingNamespace)
	assert.True(t, cm.Namespaces[knativeServingNamespace].LabelSelector.Empty())
	assert.Equal(t, fields.OneTermEqualSelector("metadata.name", knativeFeaturesConfigMap).String(),
		cm.Namespaces[knativeServingNamespace].FieldSelector.String(),
		"only Knative's feature flags are read from knative-serving")

	dep := got["Deployment.apps"]
	require.Contains(t, dep.Namespaces, scopeTestPlatformNS, "the gateway Deployment is chart-created: the platform namespace is held in full")
	assert.True(t, dep.Namespaces[scopeTestPlatformNS].LabelSelector.Empty())

	secret, ok := got["Secret"]
	require.True(t, ok, "the Secret metadata watch needs its transform")
	assert.Nil(t, secret.Label, "the Secrets are the users': a label filter would hide every rotation")
	assert.NotNil(t, secret.Transform)

	for _, gk := range []string{"Namespace", "Revision.serving.knative.dev", "ValidatingWebhookConfiguration.admissionregistration.k8s.io"} {
		assert.NotContains(t, got, gk)
	}
	assert.NotContains(t, got, "ServiceAccount",
		"every agent reconcile reads its namespace's unlabelled default ServiceAccount: scoping it makes each read an API call")
}

// failingMapper answers like mapper except for one kind, whose lookup fails the way an unreachable
// discovery endpoint does.
type failingMapper struct {
	meta.RESTMapper
	fail schema.GroupKind
}

func (m failingMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if gk == m.fail {
		return nil, errors.New("the server is currently unable to handle the request")
	}
	return m.RESTMapper.RESTMapping(gk, versions...)
}

func TestCacheScopeRefusesToGuessWhenDiscoveryFails(t *testing.T) {
	// "Not served" is a NoMatch answer. Any other failure is not an answer, and skipping the kind on it
	// would leave a kind the controller watches cached cluster-wide with nothing to say so.
	s := cacheScopeTestScheme(t)
	job := schema.GroupKind{Group: batchv1.GroupName, Kind: "Job"}
	_, err := CacheScope{}.byObject(s, failingMapper{RESTMapper: restMapperServing(t, s), fail: job})
	require.Error(t, err, "a discovery failure must stop the manager, not unscope the kind")
	assert.Contains(t, err.Error(), "Job")
	assert.False(t, meta.IsNoMatchError(err))
}

func TestCacheScopeSkipsKindsTheClusterDoesNotServe(t *testing.T) {
	// cache.New resolves every ByObject entry against the API and refuses to start on a kind it cannot
	// find, so an entry for an optional kind would stop the manager on a cluster without it (ADR 0141).
	s := cacheScopeTestScheme(t)
	optional := []client.Object{&eventingv1.Trigger{}, &eventingv1.Broker{}, &kedatypes.ScaledObject{}}
	absent := make([]schema.GroupKind, 0, len(optional))
	for _, obj := range optional {
		gvk, err := apiutil.GVKForObject(obj, s)
		require.NoError(t, err)
		absent = append(absent, gvk.GroupKind())
	}
	entries, err := CacheScope{}.byObject(s, restMapperServing(t, s, absent...))
	require.NoError(t, err)
	got := byObjectFor(t, s, entries)
	for _, gk := range absent {
		assert.NotContains(t, got, gk.String())
	}
	assert.Contains(t, got, "Job.batch")
	assert.Contains(t, got[schema.GroupKind{Kind: "ConfigMap"}.String()].Namespaces, DefaultPlatformNamespace,
		"an unset platform namespace means the default one")
}

func TestStripToIdentityKeepsWhatTheSecretWatchesRead(t *testing.T) {
	in := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name: "provider-key", Namespace: "team-a", ResourceVersion: "42",
		Labels:        map[string]string{"app": "x"},
		Annotations:   map[string]string{"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"key":"c2VjcmV0"}}`},
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
	}}
	out, err := stripToIdentity(in)
	require.NoError(t, err)
	m := out.(*metav1.PartialObjectMetadata)
	assert.Empty(t, m.Annotations, "a kubectl-applied Secret's data rides in its annotations")
	assert.Empty(t, m.ManagedFields)
	assert.Equal(t, "provider-key", m.Name)
	assert.Equal(t, "team-a", m.Namespace)
	assert.Equal(t, "42", m.ResourceVersion, "rotation is detected by resourceVersion")
	assert.Equal(t, map[string]string{"app": "x"}, m.Labels)

	again, err := stripToIdentity(out)
	require.NoError(t, err, "client-go may hand an already-transformed object back")
	assert.Equal(t, m, again)

	tomb := toolscache.DeletedFinalStateUnknown{Key: "team-a/provider-key"}
	got, err := stripToIdentity(tomb)
	require.NoError(t, err)
	assert.Equal(t, tomb, got)
}

// scopedFixture is a scopedClient over two fake clients: "cached" holds what the label-filtered cache
// would, "live" holds everything, as the API server does.
type scopedFixture struct {
	c      *scopedClient
	cached client.Client
	live   client.Client
}

func newScopedFixture(t *testing.T, cachedObjs, liveOnly []client.Object) scopedFixture {
	t.Helper()
	s := cacheScopeTestScheme(t)
	cached := fake.NewClientBuilder().WithScheme(s).WithObjects(cachedObjs...).Build()
	live := fake.NewClientBuilder().WithScheme(s).WithObjects(append(append([]client.Object{}, cachedObjs...), liveOnly...)...).Build()
	return scopedFixture{c: newScopedClient(CacheScope{PlatformNamespace: scopeTestPlatformNS}, cached, live), cached: cached, live: live}
}

func managedLabels() map[string]string { return map[string]string{ManagedByLabel: ManagedByValue} }

func agentOwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: agentsv1beta1.GroupVersion.String(), Kind: "AgentDeployment", Name: "a", UID: "u1",
		Controller: ptr.To(true),
	}
}

func TestScopedClientGetFindsWhatTheCacheCannotSee(t *testing.T) {
	ctx := context.Background()
	labelled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a-otel", Namespace: "team", Labels: managedLabels()}}
	user := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "user-config", Namespace: "team"}}
	preLabel := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "a-tools", Namespace: "team", OwnerReferences: []metav1.OwnerReference{agentOwnerRef()},
	}}
	defaultSA := &corev1.ServiceAccount{
		ObjectMeta:       metav1.ObjectMeta{Name: "default", Namespace: "team"},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}},
	}
	tenantLabels := map[string]string{agentsv1beta1.TenantLabel: "acme"}
	tenantQuota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: tenantQuotaName, Namespace: "team", Labels: tenantLabels}}
	tenantPolicy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: tenantNetworkPolicyName, Namespace: "team", Labels: tenantLabels,
	}}
	otherQuota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "team-quota", Namespace: "team", Labels: tenantLabels}}
	tenantLabelledCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "team-settings", Namespace: "team", Labels: tenantLabels}}
	notController := agentOwnerRef()
	notController.Controller = ptr.To(false)
	referencing := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "referencing", Namespace: "team", OwnerReferences: []metav1.OwnerReference{notController},
	}}
	gatewayCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "ctxmesh-gateway-config", Namespace: scopeTestPlatformNS, OwnerReferences: []metav1.OwnerReference{agentOwnerRef()},
	}}
	agent := &agentsv1beta1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "not-cached-yet", Namespace: "team"}}
	f := newScopedFixture(t, []client.Object{labelled, defaultSA}, []client.Object{
		user, preLabel, tenantQuota, tenantPolicy, otherQuota, tenantLabelledCM, referencing, gatewayCM, agent,
	})

	var cm corev1.ConfigMap
	require.NoError(t, f.c.Get(ctx, client.ObjectKeyFromObject(labelled), &cm), "a labelled object is a cache hit")

	require.NoError(t, f.c.Get(ctx, client.ObjectKeyFromObject(user), &cm), "a user's unlabelled ConfigMap must still be found")
	assert.NotContains(t, cm.Labels, ManagedByLabel, "the controller must not label an object it does not own")
	require.NoError(t, f.live.Get(ctx, client.ObjectKeyFromObject(user), &cm))
	assert.NotContains(t, cm.Labels, ManagedByLabel)

	var sa corev1.ServiceAccount
	require.NoError(t, f.c.Get(ctx, client.ObjectKeyFromObject(defaultSA), &sa),
		"the namespace default ServiceAccount is read for its pull secrets, from the cache")
	assert.Equal(t, defaultSA.ImagePullSecrets, sa.ImagePullSecrets)

	require.NoError(t, f.c.Get(ctx, client.ObjectKeyFromObject(preLabel), &cm), "an object written before the label existed must be found")
	assert.Equal(t, ManagedByValue, cm.Labels[ManagedByLabel], "and labelled, so its changes reach the controller's watches")
	require.NoError(t, f.live.Get(ctx, client.ObjectKeyFromObject(preLabel), &cm))
	assert.Equal(t, ManagedByValue, cm.Labels[ManagedByLabel], "the label is persisted, not only set on the returned copy")

	labelledInLive := func(obj client.Object) bool {
		t.Helper()
		got := obj.DeepCopyObject().(client.Object)
		require.NoError(t, f.c.Get(ctx, client.ObjectKeyFromObject(obj), got), "%T %s must be found", obj, obj.GetName())
		require.NoError(t, f.live.Get(ctx, client.ObjectKeyFromObject(obj), got))
		return got.GetLabels()[ManagedByLabel] == ManagedByValue
	}
	assert.True(t, labelledInLive(tenantQuota), "a Tenant's quota is the controller's, though it has no owner")
	assert.True(t, labelledInLive(tenantPolicy), "so is a Tenant's NetworkPolicy")
	assert.False(t, labelledInLive(otherQuota), "a quota the Tenant controller does not write is not its own")
	assert.False(t, labelledInLive(tenantLabelledCM), "nor is anything else that carries the tenant label")
	assert.False(t, labelledInLive(referencing), "an owner reference that is not the controller reference does not make it ours")

	require.NoError(t, f.c.Get(ctx, client.ObjectKeyFromObject(gatewayCM), &cm))
	require.NoError(t, f.live.Get(ctx, client.ObjectKeyFromObject(gatewayCM), &cm))
	assert.NotContains(t, cm.Labels, ManagedByLabel, "the platform namespace is cached in full: nothing there needs the label")

	err := f.c.Get(ctx, client.ObjectKey{Namespace: "team", Name: "absent"}, &cm)
	assert.True(t, apierrors.IsNotFound(err), "an object that does not exist is still NotFound, got %v", err)

	var ad agentsv1beta1.AgentDeployment
	err = f.c.Get(ctx, client.ObjectKeyFromObject(agent), &ad)
	assert.True(t, apierrors.IsNotFound(err), "a kind cached in full answers from the cache alone, got %v", err)
}

func TestScopedClientListsScopedKindsFromTheAPIServer(t *testing.T) {
	ctx := context.Background()
	labelled := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "team", Labels: map[string]string{
		ManagedByLabel: ManagedByValue, promptAgentLabel: "a",
	}}}
	preLabel := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "p0", Namespace: "team", Labels: map[string]string{promptAgentLabel: "a"}}}
	cachedAgent := &agentsv1beta1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "team"}}
	liveAgent := &agentsv1beta1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "team"}}
	f := newScopedFixture(t, []client.Object{labelled, cachedAgent}, []client.Object{preLabel, liveAgent})

	var cms corev1.ConfigMapList
	require.NoError(t, f.c.List(ctx, &cms, client.InNamespace("team"), client.MatchingLabels{promptAgentLabel: "a"}))
	names := make([]string, 0, len(cms.Items))
	for _, cm := range cms.Items {
		names = append(names, cm.Name)
	}
	assert.ElementsMatch(t, []string{"p0", "p1"}, names,
		"the prompt GC must see a prompt ConfigMap written before the label existed, or it is never pruned")

	var agents agentsv1beta1.AgentDeploymentList
	require.NoError(t, f.c.List(ctx, &agents, client.InNamespace("team")))
	require.Len(t, agents.Items, 1, "a kind cached in full lists from the cache")
	assert.Equal(t, "a", agents.Items[0].Name)
}

func TestScopedClientStampsWhatTheControllerWrites(t *testing.T) {
	ctx := context.Background()
	f := newScopedFixture(t, nil, nil)

	writes := []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "team"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "team"}},
		&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "rq", Namespace: "team"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: "team"}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "team"}},
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "cron", Namespace: "team"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "np", Namespace: "team"}},
		&servingv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "ksvc", Namespace: "team"}},
		&eventingv1.Trigger{ObjectMeta: metav1.ObjectMeta{Name: "trig", Namespace: "team"}},
		&eventingv1.Broker{ObjectMeta: metav1.ObjectMeta{Name: "broker", Namespace: "team"}},
		&kedatypes.ScaledObject{ObjectMeta: metav1.ObjectMeta{Name: "so", Namespace: "team"}},
	}
	require.Len(t, writes, len(labelScopedKinds()), "one write per label-scoped kind")
	for _, obj := range writes {
		require.NoError(t, f.c.Create(ctx, obj))
		assert.Equal(t, ManagedByValue, obj.GetLabels()[ManagedByLabel], "Create must stamp %T", obj)
	}

	existing := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "upd", Namespace: "team", Labels: map[string]string{"keep": "me"}}}
	require.NoError(t, f.cached.Create(ctx, existing))
	existing.Data = map[string]string{"k": "v"}
	require.NoError(t, f.c.Update(ctx, existing))
	var got corev1.ConfigMap
	require.NoError(t, f.cached.Get(ctx, client.ObjectKeyFromObject(existing), &got))
	assert.Equal(t, map[string]string{"keep": "me", ManagedByLabel: ManagedByValue}, got.Labels,
		"Update stamps the label and keeps every other")

	patched := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "patched", Namespace: "team"}}
	require.NoError(t, f.cached.Create(ctx, patched))
	base := patched.DeepCopy()
	patched.Annotations = map[string]string{"a": "b"}
	require.NoError(t, f.c.Patch(ctx, patched, client.MergeFrom(base)))
	var job batchv1.Job
	require.NoError(t, f.cached.Get(ctx, client.ObjectKeyFromObject(patched), &job))
	assert.Equal(t, ManagedByValue, job.Labels[ManagedByLabel], "a merge patch built from the object carries the label")

	platform := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ctxmesh-gateway", Namespace: scopeTestPlatformNS}}
	require.NoError(t, f.c.Create(ctx, platform))
	assert.NotContains(t, platform.Labels, ManagedByLabel, "the chart's objects in the platform namespace are cached without it")

	for _, obj := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "team"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "sa", Namespace: "team"}},
		&agentsv1beta1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "ad", Namespace: "team"}},
	} {
		require.NoError(t, f.c.Create(ctx, obj))
		assert.NotContains(t, obj.GetLabels(), ManagedByLabel, "%T is not label-scoped", obj)
	}
}
