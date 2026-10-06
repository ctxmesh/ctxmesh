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

	"github.com/ctxmesh/ctxmesh/internal/telemetry"
)

// TestReconcile_FeedbackEnvInjected: an agent whose namespace holds the langfuse-otlp Secret gets
// the feedback hook — FEEDBACK_PORT and LANGFUSE_HOST as plain values, the scores keys as
// references to that Secret, never as literals.
func TestReconcile_FeedbackEnvInjected(t *testing.T) {
	const (
		name      = "feedback-agent"
		namespace = "feedback-with-secret"
	)
	createTraceExportNamespace(t, namespace)
	createLangfuseSecret(t, namespace)

	ksvc, _ := reconcileTraceExportAgent(t, name, namespace)
	userContainer := ksvc.Spec.Template.Spec.Containers[0]

	byName := make(map[string]corev1.EnvVar, len(userContainer.Env))
	for _, e := range userContainer.Env {
		byName[e.Name] = e
	}

	require.Contains(t, byName, "FEEDBACK_PORT", "the feedback hook must be enabled")
	assert.Equal(t, "2995", byName["FEEDBACK_PORT"].Value, "FEEDBACK_PORT must be the reserved :2995 port")
	require.Contains(t, byName, "LANGFUSE_HOST", "LANGFUSE_HOST must be injected for the feedback relay")
	assert.Equal(t, "http://langfuse-web.langfuse.svc:3000", byName["LANGFUSE_HOST"].Value)

	for name, key := range map[string]string{
		"LANGFUSE_SCORES_PUBLIC_KEY": "public-key",
		"LANGFUSE_SCORES_SECRET_KEY": "secret-key",
	} {
		e, ok := byName[name]
		require.True(t, ok, "%s must be injected", name)
		assert.Empty(t, e.Value, "%s must not be a literal", name)
		require.NotNil(t, e.ValueFrom, "%s must come from the Secret", name)
		require.NotNil(t, e.ValueFrom.SecretKeyRef, "%s must be a secretKeyRef", name)
		assert.Equal(t, telemetry.LangfuseSecretName, e.ValueFrom.SecretKeyRef.Name)
		assert.Equal(t, key, e.ValueFrom.SecretKeyRef.Key)
	}
	assertNoInlineCredential(t, ksvc)

	// Knative's ksvc webhook rejects the downward API (fieldRef / resourceFieldRef) and accepts
	// secretKeyRef (server dry-run, 2026-10-05). This guard used to forbid every valueFrom, which
	// is what kept the scores keys as literals; it now forbids exactly the kinds the webhook
	// rejects, which is the landmine it exists for.
	for _, e := range userContainer.Env {
		if e.ValueFrom == nil {
			continue
		}
		assert.Nil(t, e.ValueFrom.FieldRef, "ksvc env %q: Knative rejects fieldRef", e.Name)
		assert.Nil(t, e.ValueFrom.ResourceFieldRef, "ksvc env %q: Knative rejects resourceFieldRef", e.Name)
	}
}

// Without the Secret the hook is off: a secretKeyRef to a missing Secret would stop the pod from
// starting, so the keys are not referenced at all.
func TestReconcile_FeedbackEnvAbsentWithoutSecret(t *testing.T) {
	const (
		name      = "feedback-agent-nosecret"
		namespace = "feedback-without-secret"
	)
	createTraceExportNamespace(t, namespace)

	ksvc, _ := reconcileTraceExportAgent(t, name, namespace)
	for _, e := range ksvc.Spec.Template.Spec.Containers[0].Env {
		assert.NotContains(t, []string{"FEEDBACK_PORT", "LANGFUSE_HOST", "LANGFUSE_SCORES_PUBLIC_KEY", "LANGFUSE_SCORES_SECRET_KEY"},
			e.Name, "no langfuse-otlp Secret in the namespace, so no feedback hook")
	}
}
