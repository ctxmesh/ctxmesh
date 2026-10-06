//go:build integration

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// An agent that names a model route says whether that route can be called now, without that ever
// changing its own Ready; an agent that names none carries no such condition.
func TestReconcile_ModelRouteReadyMirrorsTheNamedRoute(t *testing.T) {
	const ns = "default"
	mkAgent := func(name, route string) *agentsv1alpha1.AgentDeployment {
		d := &agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: agentsv1alpha1.AgentDeploymentSpec{
				Image: "ghcr.io/ctxmesh/example-agent:latest", ExecutionModel: "serving", Port: 8080,
			},
		}
		if route != "" {
			d.Spec.Env = []corev1.EnvVar{{Name: envModelRoute, Value: route}}
		}
		require.NoError(t, k8sClient.Create(testCtx, d))
		t.Cleanup(func() { _ = k8sClient.Delete(testCtx, d) })
		return d
	}
	routeCond := func(name string) *metav1.Condition {
		var d agentsv1alpha1.AgentDeployment
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: name}, &d))
		return apimeta.FindStatusCondition(d.Status.Conditions, conditionModelRouteReady)
	}
	r := newReconciler()

	mkAgent("mrr-missing", "mrr-no-such-route")
	reconcileNN(t, r, "mrr-missing", ns)
	c := routeCond("mrr-missing")
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, "RouteNotFound", c.Reason)

	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "mrr-route", Namespace: ns},
		Spec: agentsv1alpha1.ModelRouteSpec{Providers: []agentsv1alpha1.ProviderRef{
			{Provider: "mock", Model: "mock-default", Priority: 1},
		}},
	}
	require.NoError(t, k8sClient.Create(testCtx, route))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, route) })
	setRoute := func(status metav1.ConditionStatus, reason string) {
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(route), route))
		apimeta.SetStatusCondition(&route.Status.Conditions, metav1.Condition{
			Type: conditionReady, Status: status, Reason: reason, Message: "m",
		})
		require.NoError(t, k8sClient.Status().Update(testCtx, route))
	}

	setRoute(metav1.ConditionFalse, reasonNotYetServed)
	mkAgent("mrr-new", "mrr-route")
	reconcileNN(t, r, "mrr-new", ns)
	c = routeCond("mrr-new")
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, reasonNotYetServed, c.Reason)

	setRoute(metav1.ConditionTrue, reasonServed)
	reconcileNN(t, r, "mrr-new", ns)
	c = routeCond("mrr-new")
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, reasonServed, c.Reason)

	mkAgent("mrr-none", "")
	reconcileNN(t, r, "mrr-none", ns)
	assert.Nil(t, routeCond("mrr-none"), "an agent that names no route has no ModelRouteReady")
}
