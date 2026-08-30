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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/controlplane/onlinescore"
)

// mkVersion creates an AgentVersion snapshot for `deployment` named `name`. The regression
// detector orders versions by CreationTimestamp, so tests create the baseline first, then the
// serving version, giving a deterministic "prior" resolution.
func mkVersion(t *testing.T, deployment, name, namespace string) *agentsv1alpha1.AgentVersion {
	t.Helper()
	av := &agentsv1alpha1.AgentVersion{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentVersionSpec{
			DeploymentName: deployment,
			Snapshot:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, av))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, av) })
	return av
}

// seedJudgeWindows upserts K judge windows for (agent, version) at the given per-window mean.
func seedJudgeWindows(t *testing.T, s onlinescore.Store, namespace, agent, version string, n int, means []float64) {
	t.Helper()
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	for i, mean := range means {
		require.NoError(t, s.UpsertAggregate(context.Background(), onlinescore.Aggregate{
			Namespace: namespace, AgentName: agent, AgentVersion: version,
			WindowStart: base.Add(time.Duration(-i) * time.Hour),
			Judge:       onlinescore.JudgeStats{Count: n, SumVal: mean * float64(n)},
		}))
	}
}

// regressionReconciler builds a detector reconciler backed by the envtest API server and the given
// in-memory online-score store.
func regressionReconciler(s onlinescore.Store) *RegressionDetectorReconciler {
	return &RegressionDetectorReconciler{
		Client:      k8sClient,
		OnlineScore: s,
	}
}

func reconcileRegression(t *testing.T, r *RegressionDetectorReconciler, name, namespace string) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err, "reconcile must not error")
	return res
}

func regressionCond(t *testing.T, name, namespace string) *metav1.Condition {
	t.Helper()
	var d agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &d))
	return apimeta.FindStatusCondition(d.Status.Conditions, conditionRegressionDetected)
}

// mkDeployWithVersion creates an AgentDeployment and stamps status.latestVersion = serving so the
// detector treats `serving` as the serving version.
func mkDeployWithVersion(t *testing.T, name, namespace, serving string) *agentsv1alpha1.AgentDeployment {
	t.Helper()
	d := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
	}
	require.NoError(t, k8sClient.Create(testCtx, d))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, d) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(d), d))
	d.Status.LatestVersion = serving
	require.NoError(t, k8sClient.Status().Update(testCtx, d))
	return d
}

// TestRegression_FiresConditionOnRealDeployment: a serving version with a persistent judge drop
// vs the prior version's baseline lands RegressionDetected=True on the AgentDeployment via a real
// reconcile through envtest.
func TestRegression_FiresConditionOnRealDeployment(t *testing.T) {
	const (
		name      = "rd-fire-agent"
		namespace = "default"
		baseV     = "rd-fire-agent-base0000"
		curV      = "rd-fire-agent-curr0000"
	)
	store := onlinescore.NewMemStore()

	// Baseline version created first (older), serving version second (newer).
	mkVersion(t, name, baseV, namespace)
	time.Sleep(1100 * time.Millisecond) // ensure a distinct CreationTimestamp (second granularity)
	mkVersion(t, name, curV, namespace)

	mkDeployWithVersion(t, name, namespace, curV)

	// Baseline healthy (0.90 over 200); current drops ~0.20 for K=3 consecutive windows.
	seedJudgeWindows(t, store, namespace, name, baseV, 200, []float64{0.90})
	seedJudgeWindows(t, store, namespace, name, curV, 100, []float64{0.71, 0.69, 0.70})

	reconcileRegression(t, regressionReconciler(store), name, namespace)

	cond := regressionCond(t, name, namespace)
	require.NotNil(t, cond, "RegressionDetected condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "must fire True on a persistent drop")
	assert.Equal(t, reasonRegressionDetected, cond.Reason)
	assert.Contains(t, cond.Message, baseV)
}

// TestRegression_HealthyClearsCondition: a healthy serving version records RegressionDetected=False.
func TestRegression_HealthyClearsCondition(t *testing.T) {
	const (
		name      = "rd-healthy-agent"
		namespace = "default"
		baseV     = "rd-healthy-agent-base00"
		curV      = "rd-healthy-agent-curr00"
	)
	store := onlinescore.NewMemStore()
	mkVersion(t, name, baseV, namespace)
	time.Sleep(1100 * time.Millisecond)
	mkVersion(t, name, curV, namespace)
	mkDeployWithVersion(t, name, namespace, curV)

	seedJudgeWindows(t, store, namespace, name, baseV, 200, []float64{0.85})
	seedJudgeWindows(t, store, namespace, name, curV, 100, []float64{0.86, 0.87, 0.85})

	reconcileRegression(t, regressionReconciler(store), name, namespace)

	cond := regressionCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "a healthy version records False")
	assert.Equal(t, reasonNoRegression, cond.Reason)
}

// TestRegression_SparseAbstains: sparse samples (< minSamples) abstain — Unknown, never a fired
// True or a cleared False on garbage.
func TestRegression_SparseAbstains(t *testing.T) {
	const (
		name      = "rd-sparse-agent"
		namespace = "default"
		baseV     = "rd-sparse-agent-base000"
		curV      = "rd-sparse-agent-curr000"
	)
	store := onlinescore.NewMemStore()
	mkVersion(t, name, baseV, namespace)
	time.Sleep(1100 * time.Millisecond)
	mkVersion(t, name, curV, namespace)
	mkDeployWithVersion(t, name, namespace, curV)

	// A big drop, but only a handful of samples per window → below minSamples.
	seedJudgeWindows(t, store, namespace, name, baseV, 200, []float64{0.90})
	seedJudgeWindows(t, store, namespace, name, curV, 5, []float64{0.40, 0.42, 0.38})

	reconcileRegression(t, regressionReconciler(store), name, namespace)

	cond := regressionCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status, "sparse data must abstain (Unknown), not fire")
	assert.Equal(t, reasonInsufficientData, cond.Reason)
}

// TestRegression_NoBaselineAbstains: a single-version deployment has no baseline → Unknown.
func TestRegression_NoBaselineAbstains(t *testing.T) {
	const (
		name      = "rd-nobase-agent"
		namespace = "default"
		curV      = "rd-nobase-agent-curr000"
	)
	store := onlinescore.NewMemStore()
	mkVersion(t, name, curV, namespace) // the ONLY version
	mkDeployWithVersion(t, name, namespace, curV)
	seedJudgeWindows(t, store, namespace, name, curV, 100, []float64{0.50, 0.51, 0.49})

	reconcileRegression(t, regressionReconciler(store), name, namespace)

	cond := regressionCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status)
	assert.Equal(t, reasonNoBaseline, cond.Reason)
}

// TestRegression_RequeuesPeriodically: the reconcile always schedules a periodic requeue so the
// verdict tracks advancing windows.
func TestRegression_RequeuesPeriodically(t *testing.T) {
	const (
		name      = "rd-requeue-agent"
		namespace = "default"
		curV      = "rd-requeue-agent-curr00"
	)
	store := onlinescore.NewMemStore()
	mkVersion(t, name, curV, namespace)
	mkDeployWithVersion(t, name, namespace, curV)

	res := reconcileRegression(t, regressionReconciler(store), name, namespace)
	assert.Equal(t, regressionRequeue, res.RequeueAfter, "must requeue periodically")
}
