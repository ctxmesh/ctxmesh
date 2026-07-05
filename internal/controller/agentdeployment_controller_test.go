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

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctx-mesh/agent-engine/api/v1alpha1"
)

// TestAgentDeploymentReconciler_Stub verifies that the reconciler compiles and
// its empty Reconcile stub returns without error. Full reconciliation logic
// (AgentVersion + Knative Service) is implemented in m1.5.
func TestAgentDeploymentReconciler_Stub(t *testing.T) {
	const (
		name      = "stub-agentdeployment"
		namespace = "default"
	)

	resource := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctx-mesh/example-agent:latest",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, resource))
	t.Cleanup(func() {
		_ = k8sClient.Delete(testCtx, resource)
	})

	r := &AgentDeploymentReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
	}

	result, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
}
