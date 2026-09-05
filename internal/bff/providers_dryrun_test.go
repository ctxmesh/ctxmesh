package bff

import (
	"context"
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

	err := dryRunUpsert(context.Background(), c, dryRunSecret())
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
	require.NoError(t, dryRunUpsert(ctx, c, dryRunSecret()))
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

	require.NoError(t, dryRunUpsert(context.Background(), c, dryRunSecret()))
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

	err := dryRunUpsert(context.Background(), c, dryRunSecret())
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err),
		"create-but-not-update must be refused before the rotate overwrites a live key")
}
