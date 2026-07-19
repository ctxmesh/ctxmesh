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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/agent-engine/internal/kedatypes"
)

// TestRunWorkerScaledObject_Wiring proves the durable run-worker autoscaler (m32.2): the built
// ScaledObject is accepted by KEDA's CRD schema in a real API server (envtest) and round-trips with
// the correct wiring — it targets the worker Deployment and scales on the queued-run backlog via
// KEDA's postgresql scaler, reading the DSN from the worker's own env (no duplicated Secret).
func TestRunWorkerScaledObject_Wiring(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "run-worker-wiring"}}
	require.NoError(t, k8sClient.Create(testCtx, ns))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, ns) })

	so := BuildRunWorkerScaledObject(RunWorkerScaleConfig{
		Namespace:      ns.Name,
		TargetName:     "run-worker",
		MinReplicas:    0, // scale-to-zero is safe: a durable run outlives its worker
		MaxReplicas:    8,
		QueueThreshold: 5,
	})
	require.NoError(t, k8sClient.Create(testCtx, so), "KEDA must accept the run-worker ScaledObject schema")
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, so) })

	var got kedatypes.ScaledObject
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(so), &got))

	require.NotNil(t, got.Spec.ScaleTargetRef)
	assert.Equal(t, "run-worker", got.Spec.ScaleTargetRef.Name, "scales the worker Deployment")
	assert.Equal(t, "Deployment", got.Spec.ScaleTargetRef.Kind)
	require.NotNil(t, got.Spec.MinReplicaCount)
	assert.Equal(t, int32(0), *got.Spec.MinReplicaCount, "scale-to-zero available")
	require.NotNil(t, got.Spec.MaxReplicaCount)
	assert.Equal(t, int32(8), *got.Spec.MaxReplicaCount)

	require.Len(t, got.Spec.Triggers, 1)
	tr := got.Spec.Triggers[0]
	assert.Equal(t, "postgresql", tr.Type, "queue-depth scaler")
	assert.Equal(t, "RUN_STORE_DSN", tr.Metadata["connectionFromEnv"], "DSN read from the worker's own env")
	assert.Contains(t, tr.Metadata["query"], "status = 'queued'", "scales on the queued-run backlog")
	assert.Equal(t, "5", tr.Metadata["targetQueryValue"])
}
