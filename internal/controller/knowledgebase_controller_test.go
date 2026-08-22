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
	"encoding/json"
	"fmt"
	"sync"
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
	"github.com/ctxmesh/agent-engine/internal/controlplane/knowledge"
	"github.com/ctxmesh/agent-engine/internal/objectstore"
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

// TestKnowledgeBase_PerUser_ImmutableAfterCreation proves M17 (ADR 0061 Fork 3): the CEL transition
// rule rejects flipping spec.perUser after creation — in BOTH directions (the has()-normalized
// compare treats an absent/omitted perUser as false, so a false→true flip is rejected too). Flipping
// it strands org-wide chunks in a now-per-user corpus or orphans per-user chunks in a now-org-wide
// one, so it is a one-way door like embeddingRoute/chunking. A no-op update is still allowed.
func TestKnowledgeBase_PerUser_ImmutableAfterCreation(t *testing.T) {
	const ns = "default"

	// false (org-wide, the default) → true is rejected.
	mkKnowledgeBase(t, "kb-peruser-on", ns, validKBSpec()) // perUser omitted == false
	var kb agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-peruser-on", Namespace: ns}, &kb))
	kb.Spec.PerUser = true
	err := k8sClient.Update(testCtx, &kb)
	require.Error(t, err, "flipping perUser false→true must be rejected (one-way door #3)")
	assert.Contains(t, err.Error(), "perUser", "the rejection should name perUser")

	// true → false is rejected too.
	mkKnowledgeBase(t, "kb-peruser-off", ns, perUserKBSpec(0))
	var kb2 agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-peruser-off", Namespace: ns}, &kb2))
	kb2.Spec.PerUser = false
	err = k8sClient.Update(testCtx, &kb2)
	require.Error(t, err, "flipping perUser true→false must be rejected (one-way door #3)")

	// A no-op update (perUser unchanged) is allowed — the rule is a transition guard, not a freeze.
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-peruser-off", Namespace: ns}, &kb2))
	kb2.Spec.DisplayName = "renamed"
	require.NoError(t, k8sClient.Update(testCtx, &kb2), "an update leaving perUser unchanged must be allowed")
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

// TestKnowledgeBinding_AutoInjectFlowsIntoRoster (m80.5, ADR 0061 governance #5 / M10): the
// per-BINDING autoInject flag on a KnowledgeBaseRef threads into the KNOWLEDGE_BASES roster JSON so
// the in-pod SDK knows which KBs to auto-inject. A ref WITHOUT autoInject omits the field (omitempty),
// keeping the roster byte-compatible for the tool-only case.
func TestKnowledgeBinding_AutoInjectFlowsIntoRoster(t *testing.T) {
	const ns = "default"
	const autoKB = "auto-kb"
	const toolKB = "tool-kb"
	const agentName = "kb-autoinject-agent"

	// Two KnowledgeBases, both resolvable.
	for _, name := range []string{autoKB, toolKB} {
		mkKnowledgeBase(t, name, ns, agentsv1beta1.KnowledgeBaseSpec{
			EmbeddingRoute: "text-embedding-3-small",
			Source:         agentsv1beta1.KnowledgeBaseSource{Type: "upload"},
			Chunking:       agentsv1beta1.ChunkingConfig{Size: 512, Overlap: 64, Splitter: "recursive"},
		})
		reconcileKB(t, newKBReconciler(), name, ns)
	}

	// The agent auto-injects one KB and leaves the other tool-only — a per-binding choice.
	agent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: ns},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "ghcr.io/ctxmesh/echo-agent:latest", ExecutionModel: "serving", Port: 8080,
			KnowledgeBases: []agentsv1alpha1.KnowledgeBaseRef{
				{Name: autoKB, AutoInject: true},
				{Name: toolKB}, // AutoInject unset ⇒ tool-only
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })

	reconcileNN(t, newReconciler(), agentName, ns)

	envMap := envByName(getKsvc(t, agentName, ns).Spec.Template.Spec.Containers[0].Env)
	rosterJSON, hasRoster := envMap["KNOWLEDGE_BASES"]
	require.True(t, hasRoster, "KNOWLEDGE_BASES env must be set")

	var roster []kbRosterEntry
	require.NoError(t, json.Unmarshal([]byte(rosterJSON), &roster))
	require.Len(t, roster, 2)

	byName := map[string]kbRosterEntry{}
	for _, e := range roster {
		byName[e.Name] = e
	}
	assert.True(t, byName[autoKB].AutoInject, "the auto-inject binding must carry autoInject=true in the roster")
	assert.False(t, byName[toolKB].AutoInject, "the tool-only binding must NOT carry autoInject")

	// omitempty proof: the tool-only entry serialises WITHOUT the autoInject key (byte-compatible with
	// the pre-M10 roster — no structural-digest churn for a no-auto-inject fleet).
	assert.NotContains(t, rosterJSON, `"name":"`+toolKB+`","namespace":"`+ns+`","embeddingRoute":"text-embedding-3-small","autoInject"`,
		"a tool-only roster entry must omit the autoInject field")
	assert.Contains(t, rosterJSON, `"autoInject":true`, "the auto-inject entry must stamp autoInject:true")
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

// ── Fake stores for finalizer / status tests ──────────────────────────────────────────────────────
//
// These fakes implement corpusStore and prefixDeleter (the narrow interfaces the KnowledgeBaseReconciler
// depends on) so the envtest can exercise the GC and status-projection paths without a real Postgres or
// object-store connection.

// fakeCorpusStore records DeleteCorpus calls and serves a canned GetCorpusStatus response. It is safe
// for concurrent use (the reconciler may be called from goroutines in future, and tests reuse it across
// reconcile invocations).
type fakeCorpusStore struct {
	mu sync.Mutex

	// DeleteCorpusArgs holds (namespace, knowledgeBase) pairs for each DeleteCorpus call.
	DeleteCorpusArgs [][2]string
	// DeleteCorpusErr, if non-nil, is returned by DeleteCorpus.
	DeleteCorpusErr error

	// GetStatusResult is the canned CorpusStatus returned by GetCorpusStatus.
	GetStatusResult knowledge.CorpusStatus
	// GetStatusFound controls the found return value of GetCorpusStatus.
	GetStatusFound bool
	// GetStatusErr, if non-nil, is returned by GetCorpusStatus.
	GetStatusErr error
}

func (f *fakeCorpusStore) DeleteCorpus(_ context.Context, namespace, kb string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeleteCorpusArgs = append(f.DeleteCorpusArgs, [2]string{namespace, kb})
	return f.DeleteCorpusErr
}

func (f *fakeCorpusStore) GetCorpusStatus(_ context.Context, _, _ string) (knowledge.CorpusStatus, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.GetStatusResult, f.GetStatusFound, f.GetStatusErr
}

// fakePrefixDeleter records DeletePrefix calls and can be configured to fail.
type fakePrefixDeleter struct {
	mu sync.Mutex

	// DeletePrefixArgs holds each prefix passed to DeletePrefix.
	DeletePrefixArgs []string
	// DeletePrefixErr, if non-nil, is returned by DeletePrefix.
	DeletePrefixErr error
}

func (f *fakePrefixDeleter) DeletePrefix(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeletePrefixArgs = append(f.DeletePrefixArgs, prefix)
	return f.DeletePrefixErr
}

// fakeIngestionRunReader serves a canned ingestion-run terminal status for the terminal-failed safety-net
// (M80, ADR 0061 Fork 2). It records the last runID looked up so a test can assert the controller consulted
// the KB's ingestionRunRef.
type fakeIngestionRunReader struct {
	mu sync.Mutex

	// Status is the canned run status returned by IngestionRunStatus.
	Status string
	// Found controls the found return value (false ⇒ the run row is gone / never existed).
	Found bool
	// Err, if non-nil, is returned by IngestionRunStatus.
	Err error
	// LastRunID records the last runID passed to IngestionRunStatus.
	LastRunID string
}

func (f *fakeIngestionRunReader) IngestionRunStatus(_ context.Context, runID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.LastRunID = runID
	return f.Status, f.Found, f.Err
}

// newKBReconcilerWith builds a KnowledgeBaseReconciler with the given fake stores wired in.
func newKBReconcilerWith(cs corpusStore, pd prefixDeleter) *KnowledgeBaseReconciler {
	return &KnowledgeBaseReconciler{Client: k8sClient, Knowledge: cs, ObjectStore: pd}
}

// reconcileKBExpectErr calls Reconcile and asserts it returns an error. Used for partial-failure tests.
func reconcileKBExpectErr(t *testing.T, r *KnowledgeBaseReconciler, name, namespace string) {
	t.Helper()
	_, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	require.Error(t, err, "reconcile must return an error when a store GC fails")
}

// ── m68.10 finalizer + status tests ──────────────────────────────────────────────────────────────

// TestKnowledgeBase_FinalizerGCsBothStores: on deletion the finalizer calls DeleteCorpus on the
// corpus store AND DeletePrefix on the object store with the right key, then removes the finalizer.
func TestKnowledgeBase_FinalizerGCsBothStores(t *testing.T) {
	const ns = "default"
	const name = "kb-gc-both"

	fakeKnowledge := &fakeCorpusStore{}
	fakeOS := &fakePrefixDeleter{}
	r := newKBReconcilerWith(fakeKnowledge, fakeOS)

	// Create the KB and reconcile once to add the finalizer.
	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       validKBSpec(),
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	reconcileKB(t, r, name, ns)

	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &live))
	require.True(t, controllerutil.ContainsFinalizer(&live, kbFinalizer),
		"finalizer must be present after first reconcile")

	// Delete the KB — it enters Terminating, blocked by the finalizer.
	require.NoError(t, k8sClient.Delete(testCtx, &live))

	// Reconcile on the terminating object: GC must fire + finalizer must be released.
	reconcileKB(t, r, name, ns)

	// Both stores must have been called.
	fakeKnowledge.mu.Lock()
	defer fakeKnowledge.mu.Unlock()
	require.Len(t, fakeKnowledge.DeleteCorpusArgs, 1,
		"DeleteCorpus must be called exactly once during the finalizer GC")
	assert.Equal(t, [2]string{ns, name}, fakeKnowledge.DeleteCorpusArgs[0],
		"DeleteCorpus must receive the correct (namespace, kb) pair")

	fakeOS.mu.Lock()
	defer fakeOS.mu.Unlock()
	require.Len(t, fakeOS.DeletePrefixArgs, 1,
		"DeletePrefix must be called exactly once during the finalizer GC")
	wantPrefix := objectstore.KnowledgePrefix(ns, name)
	assert.Equal(t, wantPrefix, fakeOS.DeletePrefixArgs[0],
		"DeletePrefix must receive the KnowledgePrefix for the deleted KB")

	// The object must be gone once the finalizer is released.
	var gone agentsv1beta1.KnowledgeBase
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &gone)
	assert.True(t, apierrors.IsNotFound(err),
		"KnowledgeBase must be garbage-collected once the finalizer is released")
}

// TestKnowledgeBase_FinalizerPartialFailureRequeues: when the object-store DeletePrefix fails the
// reconcile must return an error (so it requeues), and the finalizer must still be present. Once the
// store succeeds the finalizer is removed.
func TestKnowledgeBase_FinalizerPartialFailureRequeues(t *testing.T) {
	const ns = "default"
	const name = "kb-gc-partial"

	fakeKnowledge := &fakeCorpusStore{}
	fakeOS := &fakePrefixDeleter{DeletePrefixErr: fmt.Errorf("object store unavailable")}
	r := newKBReconcilerWith(fakeKnowledge, fakeOS)

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       validKBSpec(),
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	reconcileKB(t, r, name, ns)

	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &live))
	require.NoError(t, k8sClient.Delete(testCtx, &live))

	// First reconcile: object store fails → error returned, finalizer stays.
	reconcileKBExpectErr(t, r, name, ns)

	var terminating agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &terminating))
	assert.True(t, controllerutil.ContainsFinalizer(&terminating, kbFinalizer),
		"finalizer must remain when a store GC fails (so the delete is retried on next reconcile)")

	// Fix the store error; second reconcile must succeed and release the finalizer.
	fakeOS.mu.Lock()
	fakeOS.DeletePrefixErr = nil
	fakeOS.mu.Unlock()
	reconcileKB(t, r, name, ns)

	var gone agentsv1beta1.KnowledgeBase
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &gone)
	assert.True(t, apierrors.IsNotFound(err),
		"KnowledgeBase must be deleted once the store GC succeeds on retry")
}

// TestKnowledgeBase_StatusProjection: a live valid KB with a canned GetCorpusStatus response (Phase,
// ChunkCount, DocumentCount, SizeBytes) must have those values projected onto KB.status by reconcile.
func TestKnowledgeBase_StatusProjection(t *testing.T) {
	const ns = "default"
	const name = "kb-status-proj"

	fakeKnowledge := &fakeCorpusStore{
		GetStatusFound: true,
		GetStatusResult: knowledge.CorpusStatus{
			Namespace: ns, KnowledgeBase: name,
			Phase: "Ready", DocumentCount: 2, ChunkCount: 7, SizeBytes: 1234,
			IngestionRunID: "run-proj",
		},
	}
	r := newKBReconcilerWith(fakeKnowledge, nil)

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       validKBSpec(),
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	t.Cleanup(func() {
		var cur agentsv1beta1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &cur); err == nil {
			controllerutil.RemoveFinalizer(&cur, kbFinalizer)
			_ = k8sClient.Update(testCtx, &cur)
		}
		_ = k8sClient.Delete(testCtx, kb)
	})

	reconcileKB(t, r, name, ns)

	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &live))
	assert.Equal(t, "Ready", live.Status.Phase,
		"reconcile must project the corpus-status Phase onto KB.status.phase")
	assert.Equal(t, int32(7), live.Status.ChunkCount,
		"reconcile must project ChunkCount onto KB.status.chunkCount")
	assert.Equal(t, int32(2), live.Status.DocumentCount,
		"reconcile must project DocumentCount onto KB.status.documentCount")
	assert.Equal(t, int64(1234), live.Status.SizeBytes,
		"reconcile must project SizeBytes onto KB.status.sizeBytes")
	assert.Equal(t, "run-proj", live.Status.IngestionRunRef,
		"reconcile must project IngestionRunID onto KB.status.ingestionRunRef")
}

// perUserKBSpec is validKBSpec with perUser + a per-user storage soft cap set (m80.4).
func perUserKBSpec(softCap int64) agentsv1beta1.KnowledgeBaseSpec {
	spec := validKBSpec()
	spec.PerUser = true
	spec.UserStorageSoftCap = softCap
	return spec
}

// TestKnowledgeBase_UserStorageSoftCap_ExceededCondition (m80.4, ADR 0061 Fork 3): a perUser KB with a
// configured userStorageSoftCap whose corpus-status per-subject aggregation shows a user OVER the cap must
// get UserStorageSoftCapExceeded=True (WARN-only, ingestion never blocked); a within-cap corpus gets False.
func TestKnowledgeBase_UserStorageSoftCap_ExceededCondition(t *testing.T) {
	const ns = "default"
	const name = "kb-peruser-softcap"

	// alice is over the 1000-byte cap; bob is within it.
	fakeKnowledge := &fakeCorpusStore{
		GetStatusFound: true,
		GetStatusResult: knowledge.CorpusStatus{
			Namespace: ns, KnowledgeBase: name, Phase: "Ready", DocumentCount: 3, ChunkCount: 9,
			SizeBytes: 1700, IngestionRunID: "run-pu",
			SizePerSubject: map[string]int64{"u-alice": 1500, "u-bob": 200},
		},
	}
	r := newKBReconcilerWith(fakeKnowledge, nil)

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       perUserKBSpec(1000),
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	t.Cleanup(func() {
		var cur agentsv1beta1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &cur); err == nil {
			controllerutil.RemoveFinalizer(&cur, kbFinalizer)
			_ = k8sClient.Update(testCtx, &cur)
		}
		_ = k8sClient.Delete(testCtx, kb)
	})

	reconcileKB(t, r, name, ns)

	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &live))
	cond := apimeta.FindStatusCondition(live.Status.Conditions, conditionKBUserStorageSoftCapExceeded)
	require.NotNil(t, cond, "a perUser KB with a soft cap must carry the UserStorageSoftCapExceeded condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a user over the per-user soft cap must set UserStorageSoftCapExceeded=True")
	assert.Contains(t, cond.Message, "u-alice", "the condition must name the over-cap user")

	// Now bring alice back within the cap → the condition must flip to False (WARN cleared).
	fakeKnowledge.mu.Lock()
	fakeKnowledge.GetStatusResult.SizePerSubject = map[string]int64{"u-alice": 300, "u-bob": 200}
	fakeKnowledge.mu.Unlock()

	reconcileKB(t, r, name, ns)
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &live))
	cond = apimeta.FindStatusCondition(live.Status.Conditions, conditionKBUserStorageSoftCapExceeded)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"once all users are within the cap the condition must flip to False")
}

// TestKnowledgeBase_OrgWide_NoUserStorageCondition (m80.4): an ORG-WIDE KB (perUser=false) must NOT grow a
// UserStorageSoftCapExceeded condition even if the corpus-status carries per-subject data — the !perUser
// path is byte-for-byte unchanged (no per-user accounting condition).
func TestKnowledgeBase_OrgWide_NoUserStorageCondition(t *testing.T) {
	const ns = "default"
	const name = "kb-orgwide-nocond"

	fakeKnowledge := &fakeCorpusStore{
		GetStatusFound: true,
		GetStatusResult: knowledge.CorpusStatus{
			Namespace: ns, KnowledgeBase: name, Phase: "Ready", DocumentCount: 1, ChunkCount: 3,
			SizeBytes: 500, IngestionRunID: "run-org",
			// Even if some stray per-subject data were present, an org-wide KB must ignore it.
			SizePerSubject: map[string]int64{"u-stray": 9999},
		},
	}
	r := newKBReconcilerWith(fakeKnowledge, nil)

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       validKBSpec(), // perUser=false, no soft cap
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	t.Cleanup(func() {
		var cur agentsv1beta1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &cur); err == nil {
			controllerutil.RemoveFinalizer(&cur, kbFinalizer)
			_ = k8sClient.Update(testCtx, &cur)
		}
		_ = k8sClient.Delete(testCtx, kb)
	})

	reconcileKB(t, r, name, ns)

	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &live))
	assert.Nil(t, apimeta.FindStatusCondition(live.Status.Conditions, conditionKBUserStorageSoftCapExceeded),
		"an org-wide KB must never carry the per-user storage condition (byte-for-byte unchanged)")
}

// TestKnowledgeBase_StatusProjection_IngestingRequeue: when GetCorpusStatus returns found=false AND
// the KB.status.Phase is already "Ingesting" (set by the BFF endpoint), the reconcile must return a
// RequeueAfter > 0 (so the controller polls for the terminal row to appear).
func TestKnowledgeBase_StatusProjection_IngestingRequeue(t *testing.T) {
	const ns = "default"
	const name = "kb-ingesting-requeue"

	// GetCorpusStatus returns found=false — the executor has not written a terminal row yet.
	fakeKnowledge := &fakeCorpusStore{GetStatusFound: false}
	r := newKBReconcilerWith(fakeKnowledge, nil)

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       validKBSpec(),
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	t.Cleanup(func() {
		var cur agentsv1beta1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &cur); err == nil {
			controllerutil.RemoveFinalizer(&cur, kbFinalizer)
			_ = k8sClient.Update(testCtx, &cur)
		}
		_ = k8sClient.Delete(testCtx, kb)
	})

	// First reconcile: adds finalizer + validates spec + sets status.phase="Pending".
	reconcileKB(t, r, name, ns)

	// Simulate the BFF setting phase=Ingesting on the status sub-resource.
	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &live))
	live.Status.Phase = "Ingesting"
	require.NoError(t, k8sClient.Status().Update(testCtx, &live))

	// Second reconcile: GetCorpusStatus returns found=false + phase is "Ingesting" → must requeue.
	result, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
	require.NoError(t, err, "reconcile must not error during an Ingesting poll")
	assert.Positive(t, result.RequeueAfter,
		"reconcile must return RequeueAfter > 0 while status is Ingesting and no terminal row is found")
}

// TestKnowledgeBase_StatusProjection_PendingPollsForCorpusStatus (M117 / m52.G7a): the INLINE ingest
// path (RUN_WORKER_DISPATCH=false) runs AS THE CALLER, who cannot update knowledgebases/status, so it
// can NEVER flip the KB to Ingesting. A Pending (validated) KB with no terminal row yet must therefore
// STILL requeue to poll the corpus-status channel — else a completed inline ingest would sit Pending
// forever (the bug the M116 audit hit).
func TestKnowledgeBase_StatusProjection_PendingPollsForCorpusStatus(t *testing.T) {
	const ns = "default"
	const name = "kb-pending-poll"

	fakeKnowledge := &fakeCorpusStore{GetStatusFound: false} // no terminal row yet
	r := newKBReconcilerWith(fakeKnowledge, nil)

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       validKBSpec(),
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	t.Cleanup(func() {
		var cur agentsv1beta1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &cur); err == nil {
			controllerutil.RemoveFinalizer(&cur, kbFinalizer)
			_ = k8sClient.Update(testCtx, &cur)
		}
		_ = k8sClient.Delete(testCtx, kb)
	})

	// First reconcile: validate → phase Pending; found=false + Pending → must requeue (poll), NOT stop.
	result, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter,
		"a Pending KB with no terminal corpus-status row must requeue to poll (the caller can't flip Ingesting)")
}

// TestKnowledgeBase_StatusProjection_PendingProjectsReady (M117 / m52.G7a): when a completed inline
// ingest has written a terminal corpus-status row, the controller must project phase=Ready onto a KB
// that is still merely Pending (never flipped to Ingesting), WITHOUT needing the caller's status flip.
func TestKnowledgeBase_StatusProjection_PendingProjectsReady(t *testing.T) {
	const ns = "default"
	const name = "kb-pending-ready"

	fakeKnowledge := &fakeCorpusStore{
		GetStatusFound: true,
		GetStatusResult: knowledge.CorpusStatus{
			Phase:         "Ready",
			ChunkCount:    3,
			DocumentCount: 3,
		},
	}
	r := newKBReconcilerWith(fakeKnowledge, nil)

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       validKBSpec(),
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	t.Cleanup(func() {
		var cur agentsv1beta1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &cur); err == nil {
			controllerutil.RemoveFinalizer(&cur, kbFinalizer)
			_ = k8sClient.Update(testCtx, &cur)
		}
		_ = k8sClient.Delete(testCtx, kb)
	})

	// One reconcile: validate → Pending, then project the terminal corpus-status → Ready (no flip needed).
	reconcileKB(t, r, name, ns)

	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &live))
	assert.Equal(t, "Ready", live.Status.Phase,
		"a Pending KB must project phase=Ready from the corpus-status channel without an Ingesting flip")
	assert.Equal(t, int32(3), live.Status.ChunkCount)
}

// TestKnowledgeBase_StuckIngesting_SafetyNetProjectsFailed (M80, ADR 0061 Fork 2): the controller safety-net.
// When the KB is stuck at phase Ingesting, NO corpus-status row exists (an out-of-band ingestion failure that
// never wrote the status channel), but the referenced ingestionRunRef run is terminal-`failed`, the reconcile
// must project KB.status.phase=Failed (NOT keep polling Ingesting forever) — the m68.14 stuck-Ingesting bug.
func TestKnowledgeBase_StuckIngesting_SafetyNetProjectsFailed(t *testing.T) {
	const ns = "default"
	const name = "kb-stuck-safetynet"

	// GetCorpusStatus returns found=false — the executor never wrote a terminal row (the out-of-band failure).
	fakeKnowledge := &fakeCorpusStore{GetStatusFound: false}
	// The referenced ingestion run terminated `failed`.
	fakeRuns := &fakeIngestionRunReader{Status: "failed", Found: true}
	r := &KnowledgeBaseReconciler{Client: k8sClient, Knowledge: fakeKnowledge, IngestionRuns: fakeRuns}

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       validKBSpec(),
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	t.Cleanup(func() {
		var cur agentsv1beta1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &cur); err == nil {
			controllerutil.RemoveFinalizer(&cur, kbFinalizer)
			_ = k8sClient.Update(testCtx, &cur)
		}
		_ = k8sClient.Delete(testCtx, kb)
	})

	// First reconcile: adds finalizer + validates.
	reconcileKB(t, r, name, ns)

	// Simulate the BFF setting phase=Ingesting AND the ingestionRunRef (the run the executor launched).
	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &live))
	live.Status.Phase = "Ingesting"
	live.Status.IngestionRunRef = "ing-run-oob"
	require.NoError(t, k8sClient.Status().Update(testCtx, &live))

	// Second reconcile: no corpus-status row + phase Ingesting + the referenced run is terminal-failed → the
	// safety-net must project Failed and NOT requeue (the run is terminal; there is nothing left to poll).
	result, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
	require.NoError(t, err, "the safety-net reconcile must not error")
	assert.Zero(t, result.RequeueAfter,
		"a terminal-failed run un-sticks the KB: the reconcile must stop polling Ingesting")

	var after agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &after))
	assert.Equal(t, "Failed", after.Status.Phase,
		"the safety-net must project Failed when the referenced ingestion run terminated failed out-of-band")
	assert.Equal(t, "ing-run-oob", fakeRuns.LastRunID,
		"the safety-net must consult the KB's ingestionRunRef")
}

// TestKnowledgeBase_StuckIngesting_SafetyNetKeepsPollingWhenRunNotTerminal (M80): the safety-net must NOT
// fire while the referenced run is still running (or terminal-succeeded, whose Ready needs the row's counts).
// It keeps polling (RequeueAfter > 0) and leaves phase Ingesting — proving the catch-all is narrow.
func TestKnowledgeBase_StuckIngesting_SafetyNetKeepsPollingWhenRunNotTerminal(t *testing.T) {
	const ns = "default"
	const name = "kb-stuck-still-running"

	fakeKnowledge := &fakeCorpusStore{GetStatusFound: false}
	// The referenced run is still `running` — the ingestion is genuinely in flight, not stuck.
	fakeRuns := &fakeIngestionRunReader{Status: "running", Found: true}
	r := &KnowledgeBaseReconciler{Client: k8sClient, Knowledge: fakeKnowledge, IngestionRuns: fakeRuns}

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       validKBSpec(),
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))
	t.Cleanup(func() {
		var cur agentsv1beta1.KnowledgeBase
		if err := k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &cur); err == nil {
			controllerutil.RemoveFinalizer(&cur, kbFinalizer)
			_ = k8sClient.Update(testCtx, &cur)
		}
		_ = k8sClient.Delete(testCtx, kb)
	})

	reconcileKB(t, r, name, ns)

	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &live))
	live.Status.Phase = "Ingesting"
	live.Status.IngestionRunRef = "ing-run-live"
	require.NoError(t, k8sClient.Status().Update(testCtx, &live))

	result, err := r.Reconcile(testCtx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
	require.NoError(t, err, "the safety-net reconcile must not error while the run is in flight")
	assert.Positive(t, result.RequeueAfter,
		"a still-running ingestion run must keep polling (the safety-net must not fire prematurely)")

	var after agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &after))
	assert.Equal(t, "Ingesting", after.Status.Phase,
		"the safety-net must leave phase Ingesting while the referenced run is not terminal-failed")
}

// TestKnowledgeBase_FinalizerLifecycle_NilStores: the existing test (nil stores → skip GC → finalizer
// removed). Confirms that both new and old code paths coexist: nil stores skip GC, non-nil stores GC.
// (This test mirrors the pre-existing TestKnowledgeBase_FinalizerLifecycle exactly; it is reproduced
// here so the reader sees the nil-store / non-nil-store pair side by side.)
func TestKnowledgeBase_FinalizerLifecycle_NilStores(t *testing.T) {
	const ns = "default"

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-nil-stores", Namespace: ns},
		Spec:       validKBSpec(),
	}
	require.NoError(t, k8sClient.Create(testCtx, kb))

	r := newKBReconciler() // nil Knowledge + nil ObjectStore
	reconcileKB(t, r, "kb-nil-stores", ns)

	var live agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-nil-stores", Namespace: ns}, &live))
	assert.True(t, controllerutil.ContainsFinalizer(&live, kbFinalizer),
		"finalizer must be added even when stores are nil")

	require.NoError(t, k8sClient.Delete(testCtx, &live))
	reconcileKB(t, r, "kb-nil-stores", ns)

	var gone agentsv1beta1.KnowledgeBase
	err := k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-nil-stores", Namespace: ns}, &gone)
	assert.True(t, apierrors.IsNotFound(err),
		"nil stores must not block deletion — the finalizer must be released")
}
