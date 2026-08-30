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

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

// TestVersionSkewConversion proves the v1alpha1 ⇄ v1beta1 API graduation (ADR 0037, M34) end to end
// against a real API server (envtest): an object CREATED as v1alpha1 is READABLE as v1beta1 (and a
// v1beta1 write is readable as v1alpha1) — the two versions are served together, so an operator's
// existing v1alpha1 manifests keep working while v1beta1 is available. The graduation is
// field-identical, so the API server serves both with the default (None) conversion strategy.
func TestVersionSkewConversion(t *testing.T) {
	const (
		namespace = "default"
		name      = "skew-sb"
	)

	// Create as v1alpha1.
	a := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend:   "kubernetes",
			SecretRef: agentsv1alpha1.SecretKeyRef{Name: "anthropic-secret", Key: "api-key"},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, a))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, a) })

	// Read the SAME object as v1beta1 — the API server serves the other version.
	var b agentsv1beta1.SecretBinding
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &b))
	assert.Equal(t, "kubernetes", b.Spec.Backend, "the v1alpha1 object reads back as v1beta1")
	assert.Equal(t, "anthropic-secret", b.Spec.SecretRef.Name, "the secretRef survives the version skew")

	// A v1beta1 write is likewise readable as v1alpha1.
	b.Spec.SecretRef.Key = "rotated-key"
	require.NoError(t, k8sClient.Update(testCtx, &b))
	var a2 agentsv1alpha1.SecretBinding
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &a2))
	assert.Equal(t, "rotated-key", a2.Spec.SecretRef.Key, "the v1beta1 update reads back as v1alpha1")
}
