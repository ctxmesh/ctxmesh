package bff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/auditlog"
)

// connectShapedCustomRoute is the ModelRoute connect writes for a custom (OpenAI-compatible)
// provider named "gateway" in "prod".
func connectShapedCustomRoute(baseURL string) *agentsv1alpha1.ModelRoute {
	return &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gateway", Namespace: "prod",
			Labels: map[string]string{labelManagedBy: managedByConnect, labelProvider: providerCustom},
			Annotations: map[string]string{
				annDisplayName: "Team gateway", annBaseURL: baseURL, annProviderModels: "mock-large,mock-small",
			},
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider: providerOpenAI, Model: "mock-large", Priority: 1,
				SecretBindingRef: "gateway", APIBase: baseURL,
			}},
			RateLimit: &agentsv1alpha1.RateLimit{TenantRPM: 600},
		},
	}
}

func TestRotateSpecFromRoute_CustomProviderKeepsItsEndpointAndRouting(t *testing.T) {
	t.Parallel()

	spec, cErr := rotateSpecFromRoute(connectShapedCustomRoute("https://llm.example.internal/v1"))
	require.Nil(t, cErr)
	assert.Equal(t, "gateway", spec.name)
	assert.Equal(t, "prod", spec.namespace)
	assert.Equal(t, providerCustom, spec.provider, "the console's id comes from the label, not the gateway's openai prefix")
	assert.Equal(t, "Team gateway", spec.displayName)
	assert.Equal(t, "https://llm.example.internal/v1", spec.baseURL, "a custom provider's endpoint is its identity")
	assert.Equal(t, "mock-large", spec.model, "a rotate changes the key, not the routed model")
	require.NotNil(t, spec.rateLimit)
	assert.Equal(t, int32(600), spec.rateLimit.TenantRPM, "an operator-tuned rate limit survives the rotate")
	assert.True(t, spec.mustExist, "a rotate updates what connect wrote and never creates")
	assert.Empty(t, spec.apiKey, "the key comes from the request, never from the cluster")
}

func TestRotateSpecFromRoute_FallsBackToTheRoutesAPIBase(t *testing.T) {
	t.Parallel()

	mr := connectShapedCustomRoute("https://llm.example.internal/v1")
	delete(mr.Annotations, annBaseURL)
	delete(mr.Annotations, annDisplayName)
	mr.Spec.RateLimit = nil

	spec, cErr := rotateSpecFromRoute(mr)
	require.Nil(t, cErr)
	assert.Equal(t, "https://llm.example.internal/v1", spec.baseURL)
	assert.Equal(t, "gateway", spec.displayName, "connect's default display name is the connection name")
	assert.Nil(t, spec.rateLimit, "no rate limit on the route stamps connect's default")
}

func TestRotateSpecFromRoute_RefusesARouteConnectDidNotWrite(t *testing.T) {
	t.Parallel()

	for name, edit := range map[string]func(*agentsv1alpha1.ModelRoute){
		"hand-authored": func(mr *agentsv1alpha1.ModelRoute) { delete(mr.Labels, labelManagedBy) },
		"model-picker":  func(mr *agentsv1alpha1.ModelRoute) { mr.Labels[labelManagedBy] = managedByModelPicker },
		"no provider":   func(mr *agentsv1alpha1.ModelRoute) { delete(mr.Labels, labelProvider) },
		"other binding": func(mr *agentsv1alpha1.ModelRoute) { mr.Spec.Providers[0].SecretBindingRef = "shared" },
		"a fallback added": func(mr *agentsv1alpha1.ModelRoute) {
			mr.Spec.Providers = append(mr.Spec.Providers, agentsv1alpha1.ProviderRef{
				Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 2, SecretBindingRef: "anthropic",
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mr := connectShapedCustomRoute("https://llm.example.internal/v1")
			edit(mr)
			_, cErr := rotateSpecFromRoute(mr)
			require.NotNil(t, cErr, "a route outside connect's shape must not be rebuilt")
			assert.Equal(t, http.StatusConflict, cErr.status)
			assert.Contains(t, cErr.msg, `provider "gateway" cannot be rotated`)
		})
	}
}

func TestCheckConnectBinding(t *testing.T) {
	t.Parallel()

	binding := func(secret, key string) *agentsv1alpha1.SecretBinding {
		return &agentsv1alpha1.SecretBinding{Spec: agentsv1alpha1.SecretBindingSpec{
			Backend:   secretBackendKubernetes,
			SecretRef: agentsv1alpha1.SecretKeyRef{Name: secret, Key: key},
		}}
	}
	assert.Nil(t, checkConnectBinding(binding("gateway", secretKeyAPIKey), "gateway"))

	for name, b := range map[string]*agentsv1alpha1.SecretBinding{
		"another Secret": binding("shared-keys", secretKeyAPIKey),
		"another key":    binding("gateway", "token"),
	} {
		cErr := checkConnectBinding(b, "gateway")
		require.NotNil(t, cErr, name)
		assert.Equal(t, http.StatusConflict, cErr.status, name)
	}
}

// seedConnectedCustomProvider is the triple connect writes for connectShapedCustomRoute.
func seedConnectedCustomProvider(baseURL string) []client.Object {
	route := connectShapedCustomRoute(baseURL)
	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "prod", Labels: route.Labels},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend:   secretBackendKubernetes,
			SecretRef: agentsv1alpha1.SecretKeyRef{Name: "gateway", Key: secretKeyAPIKey},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "prod", Labels: route.Labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{secretKeyAPIKey: []byte("sk-old")},
	}
	return []client.Object{route, binding, secret}
}

// postAs sends one authenticated POST through the BFF's handler.
func postAs(s *Server, path string, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer user-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// rotateGateway rotates the seeded "gateway" provider to theTestKey.
func rotateGateway(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(RotateProviderKeyRequest{APIKey: theTestKey, Namespace: "prod"})
	require.NoError(t, err)
	return postAs(s, "/api/providers/gateway/rotate", body)
}

// TestRotateProviderKey_IssuesExactlyTheFlowVerbs ties the handler to the flow registry the
// console's Rotate button reads. A verb the handler issues and the flow omits enables a button the
// API server refuses — the shipped defect, where the handler read the Secret and no persona may.
func TestRotateProviderKey_IssuesExactlyTheFlowVerbs(t *testing.T) {
	t.Parallel()

	prov := fakeProvider(t, "mock-large", "mock-small")
	issued := map[string]map[string]bool{}
	record := func(w client.WithWatch, obj client.Object, verb string) {
		gvk, err := w.GroupVersionKindFor(obj)
		require.NoError(t, err)
		res := strings.ToLower(gvk.Kind) + "s"
		if issued[res] == nil {
			issued[res] = map[string]bool{}
		}
		issued[res][verb] = true
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(seedConnectedCustomProvider(prov.URL)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				record(w, obj, verbGet)
				return w.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				record(w, obj, verbCreate)
				return w.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				record(w, obj, verbUpdate)
				return w.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, w client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				record(w, obj, strings.ToLower(http.MethodPatch))
				return w.Patch(ctx, obj, p, opts...)
			},
			Delete: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				record(w, obj, strings.ToLower(http.MethodDelete))
				return w.Delete(ctx, obj, opts...)
			},
		}).Build()
	s, _, lb := newConnectServer(t, c)

	rec := rotateGateway(t, s)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), theTestKey)
	assert.NotContains(t, lb.String(), theTestKey)

	want := map[string]map[string]bool{}
	for _, f := range consoleFlows {
		if f.name != "rotateProviderKey" {
			continue
		}
		for _, n := range f.needs {
			want[n.resource] = map[string]bool{}
			for _, v := range n.verbs {
				want[n.resource][v] = true
			}
		}
	}
	require.NotEmpty(t, want, "the rotateProviderKey flow must exist")
	// The audit row's actor lookup. A refusal there records the actor as unknown and never fails the
	// rotate, so it is not a verb the flow needs.
	assert.True(t, issued["selfsubjectreviews"][verbCreate], "the audit row resolves the caller")
	delete(issued, "selfsubjectreviews")
	assert.Equal(t, want, issued, "the rotate handler must issue exactly the verbs its flow declares")
	assert.False(t, issued[resSecrets][verbGet], "the Secret is never read (ADR 0136)")

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "gateway", Namespace: "prod"}, &got))
	assert.Equal(t, theTestKey, string(got.Data[secretKeyAPIKey]))
}

// TestRotateProviderKey_KeepsTheRoutesEndpointModelAndRateLimit: the route is rewritten through
// connect's write path, so everything connect would otherwise default must come from the live route.
func TestRotateProviderKey_KeepsTheRoutesEndpointModelAndRateLimit(t *testing.T) {
	t.Parallel()

	prov := fakeProvider(t, "mock-flagship-pro", "mock-large", "mock-small")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(seedConnectedCustomProvider(prov.URL)...).Build()
	s, _, _ := newConnectServer(t, c)

	rec := rotateGateway(t, s)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var summary ProviderSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &summary))
	assert.Equal(t, []string{"mock-flagship-pro", "mock-large", "mock-small"}, summary.Models)

	var route agentsv1alpha1.ModelRoute
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "gateway", Namespace: "prod"}, &route))
	require.Len(t, route.Spec.Providers, 1)
	p := route.Spec.Providers[0]
	assert.Equal(t, prov.URL, p.APIBase)
	assert.Equal(t, providerOpenAI, p.Provider)
	assert.Equal(t, "mock-large", p.Model, "the routed model is kept, not re-picked from the new list")
	assert.Equal(t, "gateway", p.SecretBindingRef)
	require.NotNil(t, route.Spec.RateLimit)
	assert.Equal(t, int32(600), route.Spec.RateLimit.TenantRPM)
	assert.Equal(t, providerCustom, route.Labels[labelProvider])
	assert.Equal(t, "Team gateway", route.Annotations[annDisplayName])
	assert.Equal(t, "mock-flagship-pro,mock-large,mock-small", route.Annotations[annProviderModels])
}

// TestRotateProviderKey_IncompleteProviderIsRefusedBeforeAnyWrite: a rotate never creates, so a
// provider whose Secret is gone is refused by the pre-flight with nothing persisted.
func TestRotateProviderKey_IncompleteProviderIsRefusedBeforeAnyWrite(t *testing.T) {
	t.Parallel()

	prov := fakeProvider(t, "mock-large")
	seed := seedConnectedCustomProvider(prov.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(seed[0], seed[1]).Build()
	before := map[string]string{}
	for _, o := range []client.Object{&agentsv1alpha1.ModelRoute{}, &agentsv1alpha1.SecretBinding{}} {
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "gateway", Namespace: "prod"}, o))
		before[fmt.Sprintf("%T", o)] = o.GetResourceVersion()
	}
	s, _, _ := newConnectServer(t, c)

	rec := rotateGateway(t, s)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `its Secret \"gateway\" does not exist`)

	err := c.Get(context.Background(), client.ObjectKey{Name: "gateway", Namespace: "prod"}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "a rotate must not create the missing Secret; got %v", err)
	for _, o := range []client.Object{&agentsv1alpha1.ModelRoute{}, &agentsv1alpha1.SecretBinding{}} {
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "gateway", Namespace: "prod"}, o))
		assert.Equal(t, before[fmt.Sprintf("%T", o)], o.GetResourceVersion(),
			"%T must be untouched by a refused rotate", o)
	}
}

// TestRotateProviderKey_RefusesWithoutProbingOrWriting: a route connect did not write is refused
// before the new key leaves the BFF.
func TestRotateProviderKey_RefusesWithoutProbingOrWriting(t *testing.T) {
	t.Parallel()

	var probes atomic.Int32
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"mock-large"}]}`))
	}))
	t.Cleanup(prov.Close)

	for name, seed := range map[string]func() []client.Object{
		"hand-authored route": func() []client.Object {
			objs := seedConnectedCustomProvider(prov.URL)
			delete(objs[0].(*agentsv1alpha1.ModelRoute).Labels, labelManagedBy)
			return objs
		},
		"binding re-pointed": func() []client.Object {
			objs := seedConnectedCustomProvider(prov.URL)
			objs[1].(*agentsv1alpha1.SecretBinding).Spec.SecretRef.Name = "shared-keys"
			return objs
		},
		"binding missing": func() []client.Object {
			objs := seedConnectedCustomProvider(prov.URL)
			return []client.Object{objs[0], objs[2]}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(seed()...).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "any"}, obj.GetName(),
							assert.AnError)
					},
				}).Build()
			s, _, _ := newConnectServer(t, c)
			before := probes.Load()

			rec := rotateGateway(t, s)
			assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "gateway")
			assert.Equal(t, before, probes.Load(), "the new key must not be sent upstream for a refused rotate")
		})
	}
}

// TestRotateProviderKey_WritesAnAuditRow: a rotate lands in the audit log as a connect with
// rotate=true, when it succeeds and when the API server refuses it. The new key is never in the row.
func TestRotateProviderKey_WritesAnAuditRow(t *testing.T) {
	t.Parallel()

	forbidden := func(obj client.Object) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "any"}, obj.GetName(), assert.AnError)
	}
	for name, tc := range map[string]struct {
		funcs        interceptor.Funcs
		wantCode     int
		wantOutcome  string
		wantProvider bool
	}{
		"success": {wantCode: http.StatusOK, wantOutcome: auditOutcomeSuccess, wantProvider: true},
		"write denied": {
			funcs: interceptor.Funcs{
				Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
					return forbidden(obj)
				},
			},
			wantCode: http.StatusForbidden, wantOutcome: auditOutcomeDenied, wantProvider: true,
		},
		"route read denied": {
			funcs: interceptor.Funcs{
				Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isRoute := obj.(*agentsv1alpha1.ModelRoute); isRoute {
						return forbidden(obj)
					}
					return w.Get(ctx, key, obj, opts...)
				},
			},
			wantCode: http.StatusForbidden, wantOutcome: auditOutcomeDenied,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			prov := fakeProvider(t, "mock-large")
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithObjects(seedConnectedCustomProvider(prov.URL)...).WithInterceptorFuncs(tc.funcs).Build()
			s, _, _ := newConnectServer(t, c)
			store := auditlog.NewMemStore()
			s.auditStore = store

			rec := rotateGateway(t, s)
			require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())

			page, err := store.List(context.Background(), auditlog.Query{})
			require.NoError(t, err)
			require.Len(t, page.Items, 1, "one audit row per rotate")
			got := page.Items[0]
			assert.Equal(t, "bff", got.Source)
			assert.Equal(t, auditActionConnect, got.Action)
			assert.Equal(t, resourceKindProvider, got.ResourceKind)
			assert.Equal(t, "gateway", got.ResourceName)
			assert.Equal(t, "prod", got.Namespace)
			assert.Equal(t, tc.wantOutcome, got.Outcome)
			assert.Equal(t, true, got.Detail["rotate"])
			if tc.wantProvider {
				assert.Equal(t, providerCustom, got.Detail[providerNoun])
			}
			assert.NotContains(t, fmt.Sprint(got.Detail), theTestKey)
		})
	}
}
