package bff

import (
	"context"
	"net/http"
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
)

func dryRunSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-key", Namespace: "team-a"},
		Data:       map[string][]byte{"apiKey": []byte("k")},
	}
}

// TestDryRunUpsertSurfacesADenial. The pre-flight exists so a denial lands BEFORE anything
// persists: createProviderObjects writes the Secret first and is not transactional, so a
// refusal on the third object used to strand a live credential with no route able to use it,
// while the caller was told the connect had failed.
//
// The denial must not be mistaken for the AlreadyExists signal, which is the one error that
// legitimately means "fall through to Update".
func TestDryRunUpsertSurfacesADenial(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Resource: "secrets"}, obj.GetName(), assert.AnError)
			},
		}).
		Build()

	_, err := dryRunUpsert(context.Background(), c, dryRunSecret())
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err),
		"a forbidden dry-run must surface as forbidden, never be swallowed as an existence check")
}

// TestDryRunUpsertPersistsNothing. A pre-flight that writes is not a pre-flight. Every call must
// carry DryRunAll, on the Update path as well as the Create path.
func TestDryRunUpsertPersistsNothing(t *testing.T) {
	t.Parallel()

	var creates, updates, dryRuns int
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				creates++
				for _, o := range opts {
					if o == client.DryRunAll {
						dryRuns++
					}
				}
				return w.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updates++
				for _, o := range opts {
					if o == client.DryRunAll {
						dryRuns++
					}
				}
				return w.Update(ctx, obj, opts...)
			},
		}).
		Build()

	ctx := context.Background()
	_, dryErr := dryRunUpsert(ctx, c, dryRunSecret())
	require.NoError(t, dryErr)
	assert.Equal(t, 1, creates)
	assert.Equal(t, 1, dryRuns, "the create path must carry DryRunAll")

	// Nothing was persisted, so a second pre-flight still takes the create path.
	var live corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "provider-key"}, &live)
	assert.True(t, apierrors.IsNotFound(err),
		"a dry-run must not persist the Secret — that is the stranded-credential bug")
}

// TestDryRunUpsertFollowsTheUpdatePathWhenTheObjectExists. AlreadyExists is not a denial: it
// says the real call would take Update, so the pre-flight must check THAT permission too.
// Stopping at the create answer would pass a caller who may create but not update, and the
// rotate path would then fail after the Secret had already been overwritten.
//
// AlreadyExists is injected rather than staged: the fake client does not raise it for a
// DRY-RUN create of an existing object, so staging one would silently exercise the create path
// and assert nothing.
func TestDryRunUpsertFollowsTheUpdatePathWhenTheObjectExists(t *testing.T) {
	t.Parallel()

	var updateDryRuns int
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewAlreadyExists(
					schema.GroupResource{Resource: "secrets"}, obj.GetName())
			},
			Update: func(_ context.Context, _ client.WithWatch, _ client.Object, opts ...client.UpdateOption) error {
				for _, o := range opts {
					if o == client.DryRunAll {
						updateDryRuns++
					}
				}
				return nil
			},
		}).
		Build()

	_, dryErr := dryRunUpsert(context.Background(), c, dryRunSecret())
	require.NoError(t, dryErr)
	assert.Equal(t, 1, updateDryRuns,
		"an existing object must be pre-flighted on the UPDATE path, which is what the real upsert takes")
}

// TestDryRunUpsertSurfacesAnUpdateDenial closes the other half: a caller who may CREATE but not
// UPDATE must be refused before the rotate overwrites a live key.
func TestDryRunUpsertSurfacesAnUpdateDenial(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewAlreadyExists(
					schema.GroupResource{Resource: "secrets"}, obj.GetName())
			},
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Resource: "secrets"}, obj.GetName(), assert.AnError)
			},
		}).
		Build()

	_, err := dryRunUpsert(context.Background(), c, dryRunSecret())
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err),
		"create-but-not-update must be refused before the rotate overwrites a live key")
}

func existingRoute() *agentsv1alpha1.ModelRoute {
	return &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: "custom", Namespace: "team-a",
			Labels: map[string]string{labelManagedBy: managedByConnect, labelProvider: providerCustom},
		},
		Spec: agentsv1alpha1.ModelRouteSpec{Providers: []agentsv1alpha1.ProviderRef{{
			Provider: "openai", Model: "old-model", Priority: 1, SecretBindingRef: "custom",
		}}},
	}
}

// TestUpsertObject_CustomResourceUpdatesWithItsLiveResourceVersion: the API server refuses a
// custom-resource update without metadata.resourceVersion, so re-connecting an existing provider
// reads the ModelRoute's resourceVersion with a metadata-only Get (never the full object) and
// updates with it.
func TestUpsertObject_CustomResourceUpdatesWithItsLiveResourceVersion(t *testing.T) {
	t.Parallel()

	var gets []client.Object
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(existingRoute()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				gets = append(gets, obj)
				return w.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	desired := existingRoute()
	desired.Spec.Providers[0].Model = "new-model"
	verb, err := upsertObject(context.Background(), c, desired)
	require.NoError(t, err)
	assert.Equal(t, verbUpdate, verb)
	require.Len(t, gets, 1, "one read, for the resourceVersion")
	assert.IsType(t, &metav1.PartialObjectMetadata{}, gets[0], "the read is metadata-only")

	var live agentsv1alpha1.ModelRoute
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(desired), &live))
	assert.Equal(t, "new-model", live.Spec.Providers[0].Model, "the route is refreshed")
}

// TestUpsertObject_RetriesWhenTheResourceVersionMoves: the route's controller writes status, which
// bumps the resourceVersion between the read and the update. The conflict is retried with a fresh
// read, because a re-connect states the new value rather than merging with the old one.
func TestUpsertObject_RetriesWhenTheResourceVersionMoves(t *testing.T) {
	t.Parallel()

	var updates int
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(existingRoute()).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updates++
				if updates == 1 {
					return apierrors.NewConflict(schema.GroupResource{Resource: "modelroutes"}, obj.GetName(), assert.AnError)
				}
				return w.Update(ctx, obj, opts...)
			},
		}).
		Build()

	verb, err := upsertObject(context.Background(), c, existingRoute())
	require.NoError(t, err)
	assert.Equal(t, verbUpdate, verb)
	assert.Equal(t, 2, updates, "the conflict is retried once with a fresh resourceVersion")
}

// TestUpsertObject_NamesTheDeniedRead: a caller who may update the route but not read it is told
// which verb was refused.
func TestUpsertObject_NamesTheDeniedRead(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(existingRoute()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "modelroutes"}, key.Name, assert.AnError)
			},
		}).
		Build()

	verb, err := upsertObject(context.Background(), c, existingRoute())
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err))
	assert.Equal(t, verbGet, verb)
	assert.Contains(t, classifyWriteError(err, verb, modelRouteKind, "custom").msg, "not allowed to get")
}

// TestDryRunUpsert_CustomResourceCarriesItsLiveResourceVersion: the pre-flight takes the same
// update path as the real upsert, so an existing custom resource is dry-run updated with its
// resourceVersion, not refused for lacking one.
func TestDryRunUpsert_CustomResourceCarriesItsLiveResourceVersion(t *testing.T) {
	t.Parallel()

	var updateRVs []string
	var dryRuns int
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(existingRoute()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "modelroutes"}, obj.GetName())
			},
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updateRVs = append(updateRVs, obj.GetResourceVersion())
				for _, o := range opts {
					if o == client.DryRunAll {
						dryRuns++
					}
				}
				return nil
			},
		}).
		Build()

	desired := existingRoute()
	verb, err := dryRunUpsert(context.Background(), c, desired)
	require.NoError(t, err)
	assert.Equal(t, verbUpdate, verb)
	require.Len(t, updateRVs, 1)
	assert.NotEmpty(t, updateRVs[0], "the dry-run update carries the live resourceVersion")
	assert.Equal(t, 1, dryRuns, "the update is a dry-run")
	assert.Empty(t, desired.GetResourceVersion(), "the pre-flight does not touch the caller's object")
}

// TestUpsertObject_RefusesACustomResourceConnectDidNotWrite: a connection name is the user's choice
// and can name a ModelRoute or SecretBinding made by hand or by GitOps. Connect, the pre-flight
// included, refuses to overwrite it, and a rotate refuses the same way.
func TestUpsertObject_RefusesACustomResourceConnectDidNotWrite(t *testing.T) {
	t.Parallel()

	handMade := map[string]client.Object{
		modelRouteKind: &agentsv1alpha1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name: "prod-llm", Namespace: "team-a",
				Labels: map[string]string{"app.kubernetes.io/instance": "argocd-prod"},
			},
			Spec: agentsv1alpha1.ModelRouteSpec{Providers: []agentsv1alpha1.ProviderRef{
				{Provider: "openai", Model: "gpt-5", Priority: 1, SecretBindingRef: "openai"},
				{Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 2, SecretBindingRef: "anthropic"},
			}},
		},
		secretBindingKind: &agentsv1alpha1.SecretBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-llm", Namespace: "team-a"},
			Spec: agentsv1alpha1.SecretBindingSpec{
				Backend:   secretBackendKubernetes,
				SecretRef: agentsv1alpha1.SecretKeyRef{Name: "vault-synced", Key: "token"},
			},
		},
	}
	for kind, existing := range handMade {
		for path, write := range map[string]func(context.Context, client.Client, client.Object) (string, error){
			"upsert":  upsertObject,
			"dry-run": dryRunUpsert,
		} {
			t.Run(kind+"/"+path, func(t *testing.T) {
				t.Parallel()
				var updates int
				c := fake.NewClientBuilder().WithScheme(testScheme(t)).
					WithObjects(existing.DeepCopyObject().(client.Object)).
					WithInterceptorFuncs(interceptor.Funcs{
						// The fake client's dry-run Create returns nil without checking anything; the
						// API server answers AlreadyExists, which sends the pre-flight to the update.
						Create: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
							if path == "dry-run" {
								return apierrors.NewAlreadyExists(schema.GroupResource{}, obj.GetName())
							}
							return w.Create(ctx, obj, opts...)
						},
						Update: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
							updates++
							return w.Update(ctx, obj, opts...)
						},
					}).Build()
				desired := existing.DeepCopyObject().(client.Object)
				desired.SetLabels(map[string]string{labelManagedBy: managedByConnect, labelProvider: providerCustom})

				verb, err := write(context.Background(), c, desired)
				var nc *notConnectManagedError
				require.ErrorAs(t, err, &nc)
				assert.Zero(t, updates, "nothing is written over an object connect did not write")

				ce := classifyProviderWriteError(err, verb, kind, providerCreateSpec{name: "prod-llm"})
				assert.Equal(t, http.StatusConflict, ce.status)
				assert.Contains(t, ce.msg, kind+` "prod-llm" exists and was not created by connecting a provider`)
				assert.Contains(t, ce.msg, "choose another connection name")
				ce = classifyProviderWriteError(err, verb, kind, providerCreateSpec{name: "prod-llm", mustExist: true})
				assert.Equal(t, http.StatusConflict, ce.status)
				assert.Contains(t, ce.msg, `provider "prod-llm" cannot be rotated`)

				live := existing.DeepCopyObject().(client.Object)
				require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(existing), live))
				assert.Equal(t, existing.GetLabels(), live.GetLabels(), "the object is untouched")
			})
		}
	}
}
