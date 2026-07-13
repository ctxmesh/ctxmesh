package bff

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

func TestInjectModelRoute(t *testing.T) {
	in := []byte("name: my-agent\nimage: echo:1\nmodel:\n  route: old-route\n")
	out, err := injectModelRoute(in, "anthropic-claude-sonnet-5")
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(out, &doc))
	// Unrelated fields are preserved…
	assert.Equal(t, "my-agent", doc["name"])
	assert.Equal(t, "echo:1", doc["image"])
	// …and model.route is set to the ensured route (overriding any prior value).
	m, ok := doc["model"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "anthropic-claude-sonnet-5", m["route"])
}

func TestRouteNameForModel(t *testing.T) {
	assert.Equal(t, "anthropic-claude-sonnet-5",
		routeNameForModel("anthropic", "claude-sonnet-5"))
	// Lower-cased + sanitized to RFC-1123.
	assert.Equal(t, "anthropic-claude-sonnet-5",
		routeNameForModel("Anthropic", "Claude-Sonnet-5"))
	// Long model ids are capped to 63 chars (a valid object name).
	long := routeNameForModel("openai", strings.Repeat("gpt-4o-", 20))
	assert.LessOrEqual(t, len(long), 63)
	// Empty → a safe default.
	assert.Equal(t, "model-route", routeNameForModel("", ""))
}

// TestEnsureRouteForModel pins the m21 seam: a picked (provider, model) get-or-creates
// a ModelRoute serving it, reusing the provider's connect SecretBinding, idempotently.
func TestEnsureRouteForModel(t *testing.T) {
	ctx := context.Background()

	t.Run("creates a route serving the model, reusing the provider binding", func(t *testing.T) {
		scheme := testScheme(t)
		c := fake.NewClientBuilder().WithScheme(scheme).Build()

		name, cerr := ensureRouteForModel(ctx, c, scheme, "default", "anthropic", "claude-sonnet-5")
		require.Nil(t, cerr)
		assert.Equal(t, "anthropic-claude-sonnet-5", name)

		var mr agentsv1alpha1.ModelRoute
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &mr))
		require.Len(t, mr.Spec.Providers, 1)
		assert.Equal(t, "anthropic", mr.Spec.Providers[0].Provider)
		assert.Equal(t, "claude-sonnet-5", mr.Spec.Providers[0].Model)
		// Reuses the provider's connect SecretBinding — no new Secret/binding.
		assert.Equal(t, "anthropic", mr.Spec.Providers[0].SecretBindingRef)
		// Labelled as picker-managed, NOT connect-managed (so listProviders ignores it).
		assert.Equal(t, managedByModelPicker, mr.Labels[labelManagedBy])
	})

	t.Run("idempotent: a second call reuses the same route, no duplicate", func(t *testing.T) {
		scheme := testScheme(t)
		c := fake.NewClientBuilder().WithScheme(scheme).Build()

		n1, e1 := ensureRouteForModel(ctx, c, scheme, "default", "anthropic", "claude-opus-4-8")
		n2, e2 := ensureRouteForModel(ctx, c, scheme, "default", "anthropic", "claude-opus-4-8")
		require.Nil(t, e1)
		require.Nil(t, e2)
		assert.Equal(t, n1, n2)

		var list agentsv1alpha1.ModelRouteList
		require.NoError(t, c.List(ctx, &list))
		assert.Len(t, list.Items, 1, "the same (provider, model) must reuse ONE route")
	})

	t.Run("missing provider/model is a 400, not a phantom route", func(t *testing.T) {
		scheme := testScheme(t)
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		_, cerr := ensureRouteForModel(ctx, c, scheme, "default", "anthropic", "")
		require.NotNil(t, cerr)
		assert.Equal(t, 400, cerr.status)
	})

	t.Run("named connection: resolves the provider TYPE + binding from the connection route (ADR 0026)", func(t *testing.T) {
		scheme := testScheme(t)
		// A named connection "anthropic-prod" of provider type "anthropic", with its
		// own SecretBinding (named after the connection by the connect flow).
		connRoute := &agentsv1alpha1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "anthropic-prod",
				Namespace: "default",
				Labels:    map[string]string{labelManagedBy: managedByConnect},
			},
			Spec: agentsv1alpha1.ModelRouteSpec{
				Providers: []agentsv1alpha1.ProviderRef{{
					Provider:         "anthropic",
					Model:            "claude-opus-4-8",
					Priority:         1,
					SecretBindingRef: "anthropic-prod",
				}},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(connRoute).Build()

		name, cerr := ensureRouteForModel(ctx, c, scheme, "default", "anthropic-prod", "claude-sonnet-5")
		require.Nil(t, cerr)
		// The route name keys on the CONNECTION, not the provider type.
		assert.Equal(t, "anthropic-prod-claude-sonnet-5", name)

		var mr agentsv1alpha1.ModelRoute
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &mr))
		require.Len(t, mr.Spec.Providers, 1)
		// The ensured route serves the connection's provider TYPE...
		assert.Equal(t, "anthropic", mr.Spec.Providers[0].Provider)
		assert.Equal(t, "claude-sonnet-5", mr.Spec.Providers[0].Model)
		// ...and reuses the CONNECTION's binding (not a per-type one).
		assert.Equal(t, "anthropic-prod", mr.Spec.Providers[0].SecretBindingRef)
	})
}
