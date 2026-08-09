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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
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
func TestKnowledgeBase_EmptyEmbeddingRoute_RejectedAtAdmission(t *testing.T) {
	const ns = "default"
	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-no-embed", Namespace: ns},
		Spec: agentsv1beta1.KnowledgeBaseSpec{
			// EmbeddingRoute intentionally blank. It is one-way door #1 (the corpus embedding
			// model), guarded by the CRD's structural `+kubebuilder:validation:MinLength=1`
			// marker — a STRUCTURAL OpenAPI validator the API server ALWAYS enforces (unlike a
			// CEL XValidation rule, which is feature-gated). So the empty route is rejected at
			// admission and the object never persists; the controller's defensive
			// EmbeddingRoute=="" branch is unreachable belt-and-suspenders. This test asserts
			// the real enforcement point: admission.
			Source:   agentsv1beta1.KnowledgeBaseSource{Type: "upload"},
			Chunking: agentsv1beta1.ChunkingConfig{Size: 512, Overlap: 64, Splitter: "recursive"},
		},
	}
	err := k8sClient.Create(testCtx, kb)
	require.Error(t, err, "empty embeddingRoute must be rejected at admission (MinLength=1, one-way door #1)")
	assert.Contains(t, err.Error(), "embeddingRoute", "the admission rejection should name the offending field")
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

// ── AgentDeployment knowledge-base binding tests (m68.8) ──────────────────────────────────────────
//
// These tests verify that spec.knowledgeBases[] is resolved by the AgentDeployment controller and
// injected into the ksvc env as KNOWLEDGE_BASE_ENABLED + KNOWLEDGE_BASES (ADR 0061 Fork 3).

// TestKnowledgeBinding_ResolvesEnvVars: an AgentDeployment with spec.knowledgeBases pointing to an
// existing KnowledgeBase → KNOWLEDGE_BASE_ENABLED=true + KNOWLEDGE_BASES roster with the resolved
// embeddingRoute injected into the ksvc.
func TestKnowledgeBinding_ResolvesEnvVars(t *testing.T) {
	const ns = "default"
	const kbName = "docs-kb"
	const agentName = "kb-bound-agent"

	// 1. Create the KnowledgeBase and reconcile it (adds finalizer + Validated condition).
	kb := mkKnowledgeBase(t, kbName, ns, agentsv1beta1.KnowledgeBaseSpec{
		EmbeddingRoute: "text-embedding-3-small",
		Source:         agentsv1beta1.KnowledgeBaseSource{Type: "upload"},
		Chunking:       agentsv1beta1.ChunkingConfig{Size: 512, Overlap: 64, Splitter: "recursive"},
	})
	reconcileKB(t, newKBReconciler(), kbName, ns)
	_ = kb // validated in the reconcile; we just need the CR to exist

	// 2. Create an AgentDeployment that references the KB.
	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: ns},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/echo-agent:latest", ExecutionModel: "serving", Port: 8080,
			KnowledgeBases: []agentsv1alpha1.KnowledgeBaseRef{
				{Name: kbName}, // namespace defaults to the agent's ns
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })

	reconcileNN(t, newReconciler(), agentName, ns)

	envMap := envByName(getKsvc(t, agentName, ns).Spec.Template.Spec.Containers[0].Env)

	assert.Equal(t, "true", envMap["KNOWLEDGE_BASE_ENABLED"],
		"KNOWLEDGE_BASE_ENABLED must be true when spec.knowledgeBases is non-empty")

	rosterJSON, hasRoster := envMap["KNOWLEDGE_BASES"]
	require.True(t, hasRoster, "KNOWLEDGE_BASES env must be set")

	var roster []kbRosterEntry
	require.NoError(t, json.Unmarshal([]byte(rosterJSON), &roster),
		"KNOWLEDGE_BASES must be valid JSON")
	require.Len(t, roster, 1, "roster must contain exactly one entry")
	assert.Equal(t, kbName, roster[0].Name)
	assert.Equal(t, ns, roster[0].Namespace)
	assert.Equal(t, "text-embedding-3-small", roster[0].EmbeddingRoute,
		"embeddingRoute must be resolved from the KnowledgeBase CR")
}

// TestKnowledgeBinding_DanglingRef_SetsCondition: when a spec.knowledgeBases[] ref points to a
// non-existent KnowledgeBase, the controller sets a KnowledgeBasesResolved=False condition and
// does NOT inject KNOWLEDGE_BASE_ENABLED (all refs dangling → proxy stays off).
func TestKnowledgeBinding_DanglingRef_SetsCondition(t *testing.T) {
	const ns = "default"
	const agentName = "kb-dangling-agent"

	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: ns},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/echo-agent:latest", ExecutionModel: "serving", Port: 8080,
			KnowledgeBases: []agentsv1alpha1.KnowledgeBaseRef{
				{Name: "does-not-exist"},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })

	reconcileNN(t, newReconciler(), agentName, ns)

	// Condition check: KnowledgeBasesResolved must be False/DanglingRef.
	var liveAgent agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: ns}, &liveAgent))
	cond := apimeta.FindStatusCondition(liveAgent.Status.Conditions, "KnowledgeBasesResolved")
	require.NotNil(t, cond, "KnowledgeBasesResolved condition must be set for a dangling ref")
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"KnowledgeBasesResolved must be False for a dangling ref")
	assert.Equal(t, "DanglingRef", cond.Reason)
	assert.Contains(t, cond.Message, "does-not-exist",
		"condition message must identify the missing KB")

	// Env check: no KNOWLEDGE_BASE_ENABLED (all refs dangling → proxy stays off).
	envMap := envByName(getKsvc(t, agentName, ns).Spec.Template.Spec.Containers[0].Env)
	_, hasEnabled := envMap["KNOWLEDGE_BASE_ENABLED"]
	assert.False(t, hasEnabled, "KNOWLEDGE_BASE_ENABLED must NOT be set when all KB refs are dangling")
	_, hasRoster := envMap["KNOWLEDGE_BASES"]
	assert.False(t, hasRoster, "KNOWLEDGE_BASES must NOT be set when all KB refs are dangling")
}

// TestKnowledgeBinding_NoKnowledgeBases_NoByteDrift: an AgentDeployment with no knowledgeBases
// must NOT inject KNOWLEDGE_BASE_ENABLED or KNOWLEDGE_BASES — byte-compatible with pre-M68.
func TestKnowledgeBinding_NoKnowledgeBases_NoByteDrift(t *testing.T) {
	const ns = "default"
	const agentName = "kb-none-agent"

	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: ns},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/echo-agent:latest", ExecutionModel: "serving", Port: 8080,
			// KnowledgeBases intentionally absent
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })

	reconcileNN(t, newReconciler(), agentName, ns)

	envMap := envByName(getKsvc(t, agentName, ns).Spec.Template.Spec.Containers[0].Env)
	_, hasEnabled := envMap["KNOWLEDGE_BASE_ENABLED"]
	assert.False(t, hasEnabled, "no knowledgeBases → KNOWLEDGE_BASE_ENABLED must not be injected")
	_, hasRoster := envMap["KNOWLEDGE_BASES"]
	assert.False(t, hasRoster, "no knowledgeBases → KNOWLEDGE_BASES must not be injected")
}
