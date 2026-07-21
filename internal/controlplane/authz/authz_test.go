package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// fakeCaller returns a caller-scoped client whose SelfSubjectAccessReview.Create is answered by `allow`
// — the API server's RBAC decision, faked. Mirrors the ssarInterceptor pattern in internal/bff tests.
func fakeCaller(t *testing.T, allow func(a Action) bool) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, authzv1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			ssar, ok := obj.(*authzv1.SelfSubjectAccessReview)
			require.True(t, ok, "expected a SelfSubjectAccessReview create, got %T", obj)
			ra := ssar.Spec.ResourceAttributes
			ssar.Status.Allowed = allow(Action{
				Verb: ra.Verb, Group: ra.Group, Resource: ra.Resource, Namespace: ra.Namespace, Name: ra.Name,
			})
			return nil
		},
	}).Build()
}

const pvResource = "promptversions"

func TestSSARAuthorizer_AllowsAndDenies(t *testing.T) {
	az := SSARAuthorizer{}
	ctx := context.Background()
	// The RBAC decision: the caller may read/write promptversions in "default" only.
	caller := fakeCaller(t, func(a Action) bool {
		return a.Resource == pvResource && a.Namespace == "default"
	})

	// Allowed in the granted namespace.
	assert.NoError(t, az.Authorize(ctx, caller, Action{
		Verb: VerbGet, Group: "agents.ctxmesh.ai", Resource: pvResource, Namespace: "default",
	}))
	assert.NoError(t, az.Authorize(ctx, caller, Action{
		Verb: VerbCreate, Group: "agents.ctxmesh.ai", Resource: pvResource, Namespace: "default",
	}))

	// Denied in another namespace → ErrForbidden (not a 500).
	err := az.Authorize(ctx, caller, Action{Verb: VerbDelete, Resource: pvResource, Namespace: "other"})
	assert.ErrorIs(t, err, ErrForbidden)

	// Denied for a different resource.
	err = az.Authorize(ctx, caller, Action{Verb: VerbGet, Resource: "secrets", Namespace: "default"})
	assert.ErrorIs(t, err, ErrForbidden)
}

func TestSSARAuthorizer_APIErrorPropagates(t *testing.T) {
	az := SSARAuthorizer{}
	scheme := runtime.NewScheme()
	require.NoError(t, authzv1.AddToScheme(scheme))
	boom := errors.New("api server unreachable")
	caller := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return boom
		},
	}).Build()

	// An API failure must NOT be treated as forbidden (fail closed on the caller's behalf, but surface
	// the real error — the handler maps it to 500, never a silent allow).
	err := az.Authorize(context.Background(), caller, Action{Verb: VerbGet, Resource: pvResource, Namespace: "default"})
	assert.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, ErrForbidden)
}
