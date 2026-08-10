//go:build integration

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// newAPReconciler builds a minimal AlertPolicyReconciler backed by the envtest client.
func newAPReconciler() *AlertPolicyReconciler {
	return &AlertPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
}

// reconcileAP calls Reconcile for the named AlertPolicy and asserts no error.
func reconcileAP(t *testing.T, r *AlertPolicyReconciler, name, namespace string) {
	t.Helper()
	_, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err, "alertpolicy reconcile must not error")
}

// mkAlertPolicy creates an AlertPolicy in the envtest cluster and registers a cleanup.
func mkAlertPolicy(t *testing.T, name, namespace string, spec agentsv1beta1.AlertPolicySpec) *agentsv1beta1.AlertPolicy {
	t.Helper()
	ap := &agentsv1beta1.AlertPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
	require.NoError(t, k8sClient.Create(testCtx, ap))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, ap) })
	return ap
}

// validAPSpec returns a minimal valid AlertPolicySpec with one condition and one console channel.
func validAPSpec() agentsv1beta1.AlertPolicySpec {
	return agentsv1beta1.AlertPolicySpec{
		Selector: agentsv1beta1.AlertSelector{
			MatchLabels: map[string]string{"app": "my-agent"},
		},
		Conditions: []agentsv1beta1.AlertCondition{
			{
				Name:      "high-error-rate",
				Type:      "errorRate",
				Threshold: "0.05",
				Window:    "5m",
			},
		},
		Route: agentsv1beta1.AlertRoute{
			Channels: []agentsv1beta1.AlertChannel{
				{Type: "console"},
			},
		},
	}
}

// TestAlertPolicy_CreateGetRoundTrip verifies that:
//  1. An AlertPolicy with one condition + one console channel is accepted by the API server
//     (the CRD is installed and the spec round-trips through etcd).
//  2. The reconciler sets status.observedGeneration to match the object's generation.
func TestAlertPolicy_CreateGetRoundTrip(t *testing.T) {
	const ns = "default"
	const name = "ap-roundtrip"

	mkAlertPolicy(t, name, ns, validAPSpec())
	r := newAPReconciler()
	reconcileAP(t, r, name, ns)

	// Fetch the object post-reconcile and assert spec round-trip + status.
	var live agentsv1beta1.AlertPolicy
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: ns}, &live))

	// Spec round-trip: the condition and channel must survive the API-server / etcd round-trip.
	require.Len(t, live.Spec.Conditions, 1, "spec.conditions must have exactly one entry")
	assert.Equal(t, "high-error-rate", live.Spec.Conditions[0].Name)
	assert.Equal(t, "errorRate", live.Spec.Conditions[0].Type)
	assert.Equal(t, "0.05", live.Spec.Conditions[0].Threshold)
	assert.Equal(t, "5m", live.Spec.Conditions[0].Window)

	require.Len(t, live.Spec.Route.Channels, 1, "spec.route.channels must have exactly one entry")
	assert.Equal(t, "console", live.Spec.Route.Channels[0].Type)
	assert.Nil(t, live.Spec.Route.Channels[0].Webhook,
		"console channel must not carry a webhook sub-struct")

	// Selector round-trip.
	assert.Equal(t, map[string]string{"app": "my-agent"}, live.Spec.Selector.MatchLabels)

	// Status: the reconciler must have stamped observedGeneration.
	assert.Equal(t, live.Generation, live.Status.ObservedGeneration,
		"reconciler must stamp status.observedGeneration = metadata.generation")
}

// TestAlertPolicy_WebhookChannel verifies that a webhook channel (with URL and secretRef) survives
// the API-server round-trip and the reconciler does not error on it.
func TestAlertPolicy_WebhookChannel(t *testing.T) {
	const ns = "default"
	const name = "ap-webhook"

	spec := agentsv1beta1.AlertPolicySpec{
		Conditions: []agentsv1beta1.AlertCondition{
			{Name: "p95-latency", Type: "p95Latency", Threshold: "500", Window: "1h"},
		},
		Route: agentsv1beta1.AlertRoute{
			Channels: []agentsv1beta1.AlertChannel{
				{
					Type: "webhook",
					Webhook: &agentsv1beta1.WebhookChannel{
						URL:       "https://hooks.example.com/alert",
						SecretRef: "alert-signing-key",
					},
				},
			},
		},
	}
	mkAlertPolicy(t, name, ns, spec)
	reconcileAP(t, newAPReconciler(), name, ns)

	var live agentsv1beta1.AlertPolicy
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: ns}, &live))

	require.Len(t, live.Spec.Route.Channels, 1)
	ch := live.Spec.Route.Channels[0]
	assert.Equal(t, "webhook", ch.Type)
	require.NotNil(t, ch.Webhook, "webhook sub-struct must be present for a webhook channel")
	assert.Equal(t, "https://hooks.example.com/alert", ch.Webhook.URL)
	assert.Equal(t, "alert-signing-key", ch.Webhook.SecretRef)
	assert.Equal(t, live.Generation, live.Status.ObservedGeneration)
}

// TestAlertPolicy_NotFound_NoError verifies that reconciling a missing AlertPolicy is a no-op
// (returns nil, not an error) — the standard Kubernetes not-found idiom.
func TestAlertPolicy_NotFound_NoError(t *testing.T) {
	r := newAPReconciler()
	_, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: "default"},
	})
	require.NoError(t, err, "reconciling a missing AlertPolicy must return nil (not-found is not an error)")
}
