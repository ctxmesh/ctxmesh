// Unit tests for the langfuse-otlp Secret watch (no build tag — runs in make test / tier0).
package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/telemetry"
)

func TestLangfuseSecretRequests(t *testing.T) {
	agent := func(ns, name string) *agentsv1alpha1.AgentDeployment {
		return &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	}
	c := fake.NewClientBuilder().WithScheme(delegateScheme(t)).
		WithObjects(agent("team-a", "one"), agent("team-a", "two"), agent("team-b", "other")).Build()
	secret := func(ns, name string) *metav1.PartialObjectMetadata {
		return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	}

	got := langfuseSecretRequests(context.Background(), c, secret("team-a", telemetry.LangfuseSecretName))
	names := make([]types.NamespacedName, 0, len(got))
	for _, r := range got {
		names = append(names, r.NamespacedName)
	}
	require.Len(t, names, 2, "every agent in the Secret's namespace, and only that namespace")
	assert.ElementsMatch(t, []types.NamespacedName{
		{Namespace: "team-a", Name: "one"}, {Namespace: "team-a", Name: "two"},
	}, names)

	assert.Empty(t, langfuseSecretRequests(context.Background(), c, secret("team-a", "some-other-secret")),
		"the watch sees every Secret in the cluster; any other name must map to nothing")
}
