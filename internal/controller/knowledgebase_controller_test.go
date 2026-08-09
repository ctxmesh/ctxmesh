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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// ── helpers ──────────────────────────────────────────────────────────────────────────────────────

func newKBReconciler() *KnowledgeBaseReconciler {
	return &KnowledgeBaseReconciler{Client: k8sClient}
}

func reconcileKB(t *testing.T, r *KnowledgeBaseReconciler, name, namespace string) {
	t.Helper()
	_, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.NoError(t, err, "knowledgebase reconcile must not error (invalid specs go to status, not err)")
}

// mkKnowledgeBase creates a KnowledgeBase with the given spec and registers cleanup.
func mkKnowledgeBase(t *testing.T, name, namespace string, spec agentsv1beta1.KnowledgeBaseSpec) *agentsv1beta1.KnowledgeBase {
	t.Helper()
	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	t.Cleanup(func() {
		// Remove the finalizer before deletion so the cleanup does not block.
		var current agentsv1beta1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &current); err == nil {
			controllerutil.RemoveFinalizer(&current, kbFinalizer)
			_ = k8sClient.Update(testCtx, &current)
		}
		_ = k8sClient.Delete(testCtx, kb)
	})
	return kb
}

// kbValidatedCond returns the Validated condition from the KnowledgeBase status, or nil.
func kbValidatedCond(t *testing.T, name, namespace string) *metav1.Condition {
	t.Helper()
	var kb agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: namespace}, &kb))
	return apimeta.FindStatusCondition(kb.Status.Conditions, conditionKBValidated)
}

// validKBSpec returns a minimal valid KnowledgeBaseSpec for use in tests.
func validKBSpec() agentsv1beta1.KnowledgeBaseSpec {
	return agentsv1beta1.KnowledgeBaseSpec{
		EmbeddingRoute: "text-embedding-3-small",
		Source: agentsv1beta1.KnowledgeBaseSource{
			Type:              "objectStorePrefix",
			ObjectStorePrefix: "docs/acme/",
		},
		Chunking: agentsv1beta1.ChunkingConfig{
			Size:     512,
			Overlap:  64,
			Splitter: "recursive",
		},
	}
}

// ── tests ─────────────────────────────────────────────────────────────────────────────────────────

// TestKnowledgeBase_ValidSpec_IsValidated — a well-formed KnowledgeBase (non-empty embeddingRoute,
// sane chunking, objectStorePrefix source with prefix set) → Validated=True + phase=Pending.
func TestKnowledgeBase_ValidSpec_IsValidated(t *testing.T) {
	const ns = "default"
	mkKnowledgeBase(t, "kb-valid", ns, validKBSpec())
	reconcileKB(t, newKBReconciler(), "kb-valid", ns)

	var kb agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-valid", Namespace: ns}, &kb))

	cond := apimeta.FindStatusCondition(kb.Status.Conditions, conditionKBValidated)
	require.NotNil(t, cond, "Validated condition must be set after reconcile")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "valid spec must produce Validated=True")
	assert.Equal(t, reasonKBValidated, cond.Reason)
	assert.Equal(t, "Pending", kb.Status.Phase, "phase must be Pending after initial validate (ingestion is m68.6)")
	assert.Equal(t, kb.Generation, kb.Status.ObservedGeneration)
	assert.True(t, controllerutil.ContainsFinalizer(&kb, kbFinalizer), "finalizer must be registered")
}

// TestKnowledgeBase_UploadSource_IsValidated — source.type="upload" requires no companion field.
func TestKnowledgeBase_UploadSource_IsValidated(t *testing.T) {
	const ns = "default"
	spec := agentsv1beta1.KnowledgeBaseSpec{
		EmbeddingRoute: "text-embedding-3-small",
		Source:         agentsv1beta1.KnowledgeBaseSource{Type: "upload"},
		Chunking:       agentsv1beta1.ChunkingConfig{Size: 256, Overlap: 32, Splitter: "markdown"},
	}
	mkKnowledgeBase(t, "kb-upload", ns, spec)
	reconcileKB(t, newKBReconciler(), "kb-upload", ns)

	cond := kbValidatedCond(t, "kb-upload", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "upload-source KB must be Validated=True")
}

// TestKnowledgeBase_EmptyEmbeddingRoute_IsInvalid — embeddingRoute="" → Validated=False / InvalidEmbeddingRoute.
func TestKnowledgeBase_EmptyEmbeddingRoute_IsInvalid(t *testing.T) {
	const ns = "default"
	spec := agentsv1beta1.KnowledgeBaseSpec{
		// EmbeddingRoute intentionally left blank (the API server min-length marker enforces it in
		// real admission, but the controller must surface a clear status too).
		Source:   agentsv1beta1.KnowledgeBaseSource{Type: "upload"},
		Chunking: agentsv1beta1.ChunkingConfig{Size: 512, Overlap: 64, Splitter: "recursive"},
	}
	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-no-embed", Namespace: ns},
		Spec:       spec,
	}
	// Bypass the CRD admission validation (envtest does not enforce CEL validators by default,
	// so we can create this object to test the controller's own validation path).
	require.NoError(t, k8sClient.Create(testCtx, kb))
	t.Cleanup(func() {
		var current agentsv1beta1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-no-embed", Namespace: ns}, &current); err == nil {
			controllerutil.RemoveFinalizer(&current, kbFinalizer)
			_ = k8sClient.Update(testCtx, &current)
		}
		_ = k8sClient.Delete(testCtx, kb)
	})

	reconcileKB(t, newKBReconciler(), "kb-no-embed", ns)

	cond := kbValidatedCond(t, "kb-no-embed", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "empty embeddingRoute must produce Validated=False")
	assert.Equal(t, reasonKBInvalidEmbedding, cond.Reason)
}

// TestKnowledgeBase_OverlapGESize_IsInvalid — chunking.overlap >= chunking.size → Validated=False / InvalidChunking.
func TestKnowledgeBase_OverlapGESize_IsInvalid(t *testing.T) {
	const ns = "default"
	spec := agentsv1beta1.KnowledgeBaseSpec{
		EmbeddingRoute: "text-embedding-3-small",
		Source:         agentsv1beta1.KnowledgeBaseSource{Type: "upload"},
		Chunking: agentsv1beta1.ChunkingConfig{
			Size:     128,
			Overlap:  128, // equal to size — must be < size
			Splitter: "recursive",
		},
	}
	mkKnowledgeBase(t, "kb-bad-chunking", ns, spec)
	reconcileKB(t, newKBReconciler(), "kb-bad-chunking", ns)

	cond := kbValidatedCond(t, "kb-bad-chunking", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "overlap >= size must produce Validated=False")
	assert.Equal(t, reasonKBInvalidChunking, cond.Reason)
	assert.Contains(t, cond.Message, "overlap")
}

// TestKnowledgeBase_ObjectStorePrefixMissing_IsInvalid — type="objectStorePrefix" without
// objectStorePrefix → Validated=False / InvalidSource.
func TestKnowledgeBase_ObjectStorePrefixMissing_IsInvalid(t *testing.T) {
	const ns = "default"
	spec := agentsv1beta1.KnowledgeBaseSpec{
		EmbeddingRoute: "text-embedding-3-small",
		Source: agentsv1beta1.KnowledgeBaseSource{
			Type: "objectStorePrefix",
			// ObjectStorePrefix intentionally omitted
		},
		Chunking: agentsv1beta1.ChunkingConfig{Size: 512, Overlap: 64, Splitter: "recursive"},
	}
	mkKnowledgeBase(t, "kb-no-prefix", ns, spec)
	reconcileKB(t, newKBReconciler(), "kb-no-prefix", ns)

	cond := kbValidatedCond(t, "kb-no-prefix", ns)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "objectStorePrefix source without prefix must produce Validated=False")
	assert.Equal(t, reasonKBInvalidSource, cond.Reason)
	assert.Contains(t, cond.Message, "objectStorePrefix")
}

// TestKnowledgeBase_FinalizerLifecycle — the finalizer is added on reconcile of a live object and
// removed on deletion (the GC seam established for m68.10). After the finalizer is released the
// object must be garbage-collected.
func TestKnowledgeBase_FinalizerLifecycle(t *testing.T) {
	const ns = "default"

	// Create and reconcile (adds the finalizer + sets Validated).
	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-finalizer", Namespace: ns},
		Spec:       validKBSpec(),
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))

	r := newKBReconciler()
	reconcileKB(t, r, "kb-finalizer", ns)

	// The finalizer must be present after the first reconcile.
	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-finalizer", Namespace: ns}, &live))
	assert.True(t, controllerutil.ContainsFinalizer(&live, kbFinalizer),
		"reconcile must have added the finalizer (deletion convergence depends on it)")

	// Initiate deletion — the object enters Terminating but is blocked by the finalizer.
	require.NoError(t, k8sClient.Delete(testCtx, &live))

	// One reconcile on the terminating object removes the finalizer (the m68.10 GC no-op).
	reconcileKB(t, r, "kb-finalizer", ns)

	// The object must be gone once the finalizer is released.
	var gone agentsv1beta1.KnowledgeBase
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-finalizer", Namespace: ns}, &gone)
	assert.True(t, apierrors.IsNotFound(err),
		"KnowledgeBase must be deleted once the finalizer is released (m68.10 GC seam is in place)")
}
