//go:build integration

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// An agent run is a session, not a request (ADR 0147). Without a revision timeout Knative ends every run
// at 300 seconds while the platform allows 600; without a concurrency field nobody can bound how many
// runs one pod holds.
func TestReconcile_RunSessionScaling(t *testing.T) {
	const namespace = "default"
	for _, tc := range []struct {
		name        string
		scaling     *agentsv1alpha1.ScalingSpec
		wantCC      *int64
		wantTimeout int64
		wantTBC     string
	}{
		{name: "session-defaults", scaling: nil, wantCC: nil, wantTimeout: 600, wantTBC: "0"},
		// Bounded concurrency keeps the activator in the path unless told otherwise, or a burst beyond
		// the replicas' slots is dropped instead of queued.
		{
			name: "session-bounded", scaling: &agentsv1alpha1.ScalingSpec{Max: 4, Concurrency: ptr.To(int32(2))},
			wantCC: ptr.To(int64(2)), wantTimeout: 600, wantTBC: "-1",
		},
		{name: "session-explicit", scaling: &agentsv1alpha1.ScalingSpec{
			Max: 5, Concurrency: ptr.To(int32(1)), TimeoutSeconds: ptr.To(int64(900)), TargetBurstCapacity: ptr.To(int32(-1)),
		}, wantCC: ptr.To(int64(1)), wantTimeout: 900, wantTBC: "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deploy := &agentsv1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: namespace},
				Spec: agentsv1alpha1.AgentDeploymentSpec{
					Image: "ghcr.io/ctxmesh/example-agent:latest", Scaling: tc.scaling,
				},
			}
			require.NoError(t, k8sClient.Create(testCtx, deploy))
			t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
			reconcileNN(t, newReconciler(), tc.name, namespace)

			var ksvc servingv1.Service
			require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: tc.name, Namespace: namespace}, &ksvc))
			rev := ksvc.Spec.Template
			assert.Equal(t, tc.wantCC, rev.Spec.ContainerConcurrency, "containerConcurrency")
			require.NotNil(t, rev.Spec.TimeoutSeconds, "a run timeout is always set; unset, Knative ends every run at 300s")
			assert.Equal(t, tc.wantTimeout, *rev.Spec.TimeoutSeconds)
			require.NotNil(t, rev.Spec.ResponseStartTimeoutSeconds,
				"a run that answers only when done must have the whole run to send its first byte")
			assert.Equal(t, tc.wantTimeout, *rev.Spec.ResponseStartTimeoutSeconds)
			assert.Equal(t, tc.wantTBC, rev.Annotations["autoscaling.knative.dev/target-burst-capacity"])
		})
	}
}
