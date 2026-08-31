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
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// TestReconcile_FeedbackEnvInjected verifies that FEEDBACK_PORT +
// LANGFUSE_HOST + LANGFUSE_SCORES_PUBLIC_KEY + LANGFUSE_SCORES_SECRET_KEY are
// all injected as STATIC env (ValueFrom == nil) on every agent's user container.
// This is the tier1 no-valueFrom guard for the M9 feedback path (spec §3;
// mirrors TestReconcile_BudgetEnvInjected for the M8 budget path).
func TestReconcile_FeedbackEnvInjected(t *testing.T) {
	const (
		name      = "feedback-agent"
		namespace = "default"
	)

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/example-agent:latest",
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

	reconcileNN(t, newReconciler(), name, namespace)

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: name, Namespace: namespace}, &ksvc))
	userContainer := ksvc.Spec.Template.Spec.Containers[0]

	// Build env lookup map (plain-value env only; ValueFrom entries are flagged
	// separately below).
	envMap := make(map[string]string, len(userContainer.Env))
	for _, e := range userContainer.Env {
		if e.ValueFrom == nil {
			envMap[e.Name] = e.Value
		}
	}

	// ── FEEDBACK_PORT must be present and set to 2995 ────────────────────────
	require.Contains(t, envMap, "FEEDBACK_PORT",
		"FEEDBACK_PORT must be injected into every agent's user container")
	assert.Equal(t, "2995", envMap["FEEDBACK_PORT"],
		"FEEDBACK_PORT must be the reserved :2995 port")

	// ── LANGFUSE_HOST must be the in-cluster dev Langfuse URL ─────────────────
	require.Contains(t, envMap, "LANGFUSE_HOST",
		"LANGFUSE_HOST must be injected for the feedback relay")
	assert.Equal(t, "http://langfuse-web.langfuse.svc:3000", envMap["LANGFUSE_HOST"],
		"LANGFUSE_HOST must be the dev Langfuse in-cluster URL")

	// ── LANGFUSE_SCORES_PUBLIC_KEY / _SECRET_KEY must be the dev keys ─────────
	require.Contains(t, envMap, "LANGFUSE_SCORES_PUBLIC_KEY",
		"LANGFUSE_SCORES_PUBLIC_KEY must be injected")
	assert.Equal(t, "pk-lf-dev-00000000000000000000000000000000",
		envMap["LANGFUSE_SCORES_PUBLIC_KEY"],
		"LANGFUSE_SCORES_PUBLIC_KEY must be the deterministic dev key")

	require.Contains(t, envMap, "LANGFUSE_SCORES_SECRET_KEY",
		"LANGFUSE_SCORES_SECRET_KEY must be injected")
	assert.Equal(t, "sk-lf-dev-00000000000000000000000000000000",
		envMap["LANGFUSE_SCORES_SECRET_KEY"],
		"LANGFUSE_SCORES_SECRET_KEY must be the deterministic dev key")

	// ── Knative no-valueFrom guard (M5.7): ALL user-container env must be static
	// Values — not valueFrom. Covers all four feedback vars + every other injected
	// var in this reconcile (AGENT_PORT, MODEL_GATEWAY_URL, etc.).
	for _, e := range userContainer.Env {
		assert.Nil(t, e.ValueFrom,
			"ksvc env %q must be a static value (no valueFrom — Knative webhook landmine M5.7)", e.Name)
	}
}
