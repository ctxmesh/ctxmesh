//go:build integration

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// An agent run is a session, not a request (ADR 0147). Without a revision timeout Knative ends every run
// at 300 seconds while the platform allows 600; without a concurrency field nobody can bound how many
// runs one pod holds.
func TestReconcile_RunSessionScaling(t *testing.T) {
	const namespace = "default"
	for _, tc := range []struct {
		name        string
		scaling     *agentsv1beta1.ScalingSpec
		resources   *agentsv1beta1.AgentResources
		wantCC      *int64
		wantTimeout int64
		wantTBC     string
	}{
		{name: "session-defaults", scaling: nil, wantCC: nil, wantTimeout: 600, wantTBC: "0"},
		// Bounded concurrency keeps the activator in the path unless told otherwise, or a burst beyond
		// the replicas' slots is dropped instead of queued.
		{
			name: "session-bounded", scaling: &agentsv1beta1.ScalingSpec{Max: 4, Concurrency: ptr.To(int32(2))},
			resources: &agentsv1beta1.AgentResources{CPU: resource.MustParse("250m"), Memory: resource.MustParse("512Mi")},
			wantCC:    ptr.To(int64(2)), wantTimeout: 600, wantTBC: "-1",
		},
		{name: "session-explicit", scaling: &agentsv1beta1.ScalingSpec{
			Max: 5, Concurrency: ptr.To(int32(1)), TimeoutSeconds: ptr.To(int64(900)), TargetBurstCapacity: ptr.To(int32(-1)),
		}, wantCC: ptr.To(int64(1)), wantTimeout: 900, wantTBC: "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deploy := &agentsv1beta1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: namespace},
				Spec: agentsv1beta1.AgentDeploymentSpec{
					Image: "ghcr.io/ctxmesh/example-agent:latest", Scaling: tc.scaling, Resources: tc.resources,
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

// A replica that holds several runs at once must say what they need: the scheduler places a pod only by
// its requests, so an unsized replica with concurrency 4 lands where one run fits and is OOM-killed
// with four (ADR 0147). The API server refuses the spec rather than the controller guessing a size.
func TestAgentDeployment_ConcurrencyAboveOneRequiresResources(t *testing.T) {
	const namespace = "default"
	sized := &agentsv1beta1.AgentResources{CPU: resource.MustParse("500m"), Memory: resource.MustParse("1Gi")}
	for _, tc := range []struct {
		name      string
		cc        *int32
		resources *agentsv1beta1.AgentResources
		admitted  bool
	}{
		{name: "cc-unset", admitted: true},
		{name: "cc-zero", cc: ptr.To(int32(0)), admitted: true},
		{name: "cc-one", cc: ptr.To(int32(1)), admitted: true},
		{name: "cc-two-unsized", cc: ptr.To(int32(2)), admitted: false},
		{name: "cc-two-memory-only", cc: ptr.To(int32(2)),
			resources: &agentsv1beta1.AgentResources{Memory: resource.MustParse("1Gi")}, admitted: false},
		{name: "cc-two-cpu-only", cc: ptr.To(int32(2)),
			resources: &agentsv1beta1.AgentResources{CPU: resource.MustParse("500m")}, admitted: false},
		{name: "cc-two-sized", cc: ptr.To(int32(2)), resources: sized, admitted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deploy := &agentsv1beta1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "rs-" + tc.name, Namespace: namespace},
				Spec: agentsv1beta1.AgentDeploymentSpec{
					Image:     "ghcr.io/ctxmesh/example-agent:latest",
					Scaling:   &agentsv1beta1.ScalingSpec{Max: 3, Concurrency: tc.cc},
					Resources: tc.resources,
				},
			}
			err := k8sClient.Create(testCtx, deploy)
			if tc.admitted {
				require.NoError(t, err)
				t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
				return
			}
			require.Error(t, err)
			assert.True(t, apierrors.IsInvalid(err), "refused by validation, got %v", err)
			assert.Contains(t, err.Error(), "resources.cpu and resources.memory are required when scaling.concurrency is greater than 1")
		})
	}

	// Raising concurrency on an existing, unsized agent is refused the same way: the rule holds on update.
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rs-raised", Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest", Scaling: &agentsv1beta1.ScalingSpec{Max: 3, Concurrency: ptr.To(int32(1))},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	deploy.Spec.Scaling.Concurrency = ptr.To(int32(8))
	err := k8sClient.Update(testCtx, deploy)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "got %v", err)
}
