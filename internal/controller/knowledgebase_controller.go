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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
)

// kbFinalizer guards KnowledgeBase deletion so both stores (knowledge_chunks Postgres partition
// + the durable object-store bucket prefix) can be cleaned up before the object is removed
// (ADR 0061 governance #3, the MCPToolBinding/Tenant finalizer precedent).
const kbFinalizer = "knowledgebase.agents.ctxmesh.ai/finalizer"

// KnowledgeBase status condition types + reasons (M68, ADR 0061). Validated=True when the spec
// is structurally sound: embeddingRoute is non-empty, chunking is sane (size > 0, overlap < size),
// and source.type is valid with the required companion field present. Validated=False (Invalid)
// carries a specific reason so an operator can see exactly what is wrong directly on the object.
const (
	conditionKBValidated = "Validated"

	reasonKBValidated        = "Validated"
	reasonKBInvalidEmbedding = "InvalidEmbeddingRoute"
	reasonKBInvalidChunking  = "InvalidChunking"
	reasonKBInvalidSource    = "InvalidSource"
)

// KnowledgeBaseReconciler is a VALIDATE-ONLY controller (the GuardrailPolicy/Workflow pattern,
// ADR 0061, m68.1). It validates the KnowledgeBase spec — embeddingRoute non-empty, chunking sane
// (size > 0, overlap < size), source.type valid + required companion field present — then sets
// Validated=True + phase=Pending (ingestion hasn't run) or Validated=False + an Invalid condition.
// It also manages the kbFinalizer lifecycle.
//
// What this controller does NOT do (clean seams for later tasks):
//   - It does NOT ingest, embed, or write to knowledge_chunks (m68.6: the ingestion executor).
//   - It does NOT touch any object store (m68.6).
//   - The finalizer's two-store GC (knowledge_chunks partition + durable bucket prefix) is a
//     documented no-op placeholder here — the real GC lands in m68.10 (ADR 0061 governance #3).
//   - It does NOT bind a KnowledgeBase to an AgentDeployment (m68.8: AgentDeployment
//     spec.knowledgeBases[] + launcher roster gate injection).
type KnowledgeBaseReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=knowledgebases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=knowledgebases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=knowledgebases/finalizers,verbs=update

// Reconcile validates a KnowledgeBase spec, manages its finalizer, and records the result on status.
func (r *KnowledgeBaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var kb agentsv1beta1.KnowledgeBase
	if err := r.Get(ctx, req.NamespacedName, &kb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // deleted — finalizer already released or object was never created
		}
		return ctrl.Result{}, fmt.Errorf("fetching KnowledgeBase: %w", err)
	}

	// ── Deletion path: the finalizer is set; perform GC before releasing it ─────────────────────
	if !kb.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&kb, kbFinalizer) {
			// GC of knowledge_chunks partition + the durable bucket prefix lands in m68.10
			// (ADR 0061 governance #3). Here we remove the finalizer immediately so deletion
			// is not blocked, but the seam is established from day one. The real two-store
			// cleanup (DROP PARTITION + object-store prefix purge) will be wired here in m68.10.
			log.V(1).Info("KnowledgeBase deletion: releasing finalizer (two-store GC is m68.10)",
				"knowledgebase", kb.Name)
			controllerutil.RemoveFinalizer(&kb, kbFinalizer)
			if err := r.Update(ctx, &kb); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing KnowledgeBase finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// ── Ensure the finalizer is registered on every live object ──────────────────────────────────
	if controllerutil.AddFinalizer(&kb, kbFinalizer) {
		if err := r.Update(ctx, &kb); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding KnowledgeBase finalizer: %w", err)
		}
	}

	// ── Spec validation ────────────────────────────────────────────────────────────────────────
	// 1. embeddingRoute must be non-empty (it is the one-way door #1; the CRD marker enforces
	//    MinLength=1, but we surface a clear status condition if it is somehow empty).
	if kb.Spec.EmbeddingRoute == "" {
		log.V(1).Info("KnowledgeBase invalid: embeddingRoute is empty", "knowledgebase", kb.Name)
		return ctrl.Result{}, r.setStatus(ctx, &kb, metav1.ConditionFalse, reasonKBInvalidEmbedding,
			"spec.embeddingRoute must be set: it is the corpus embedding model (one-way door #1, ADR 0061) "+
				"and cannot be changed after creation")
	}

	// 2. Chunking must be sane: size > 0 (default 512 if unset), overlap < size.
	size := kb.Spec.Chunking.Size
	if size == 0 {
		size = 512 // the kubebuilder:default is applied by the API server; handle zero defensively
	}
	overlap := kb.Spec.Chunking.Overlap
	if size <= 0 {
		log.V(1).Info("KnowledgeBase invalid: chunking.size must be > 0", "knowledgebase", kb.Name, "size", size)
		return ctrl.Result{}, r.setStatus(ctx, &kb, metav1.ConditionFalse, reasonKBInvalidChunking,
			fmt.Sprintf("spec.chunking.size must be > 0 (got %d)", size))
	}
	if overlap >= size {
		log.V(1).Info("KnowledgeBase invalid: chunking.overlap must be < size",
			"knowledgebase", kb.Name, "size", size, "overlap", overlap)
		return ctrl.Result{}, r.setStatus(ctx, &kb, metav1.ConditionFalse, reasonKBInvalidChunking,
			fmt.Sprintf("spec.chunking.overlap (%d) must be less than spec.chunking.size (%d)", overlap, size))
	}

	// 3. source.type must be one of the valid values, and the required companion field must be present.
	switch kb.Spec.Source.Type {
	case "objectStorePrefix":
		if kb.Spec.Source.ObjectStorePrefix == "" {
			log.V(1).Info("KnowledgeBase invalid: objectStorePrefix required for objectStorePrefix source",
				"knowledgebase", kb.Name)
			return ctrl.Result{}, r.setStatus(ctx, &kb, metav1.ConditionFalse, reasonKBInvalidSource,
				"spec.source.objectStorePrefix must be set when spec.source.type is \"objectStorePrefix\"")
		}
	case "upload":
		// No companion field required in v1: the durable KB bucket prefix is derived from the KB name.
	default:
		log.V(1).Info("KnowledgeBase invalid: unknown source type",
			"knowledgebase", kb.Name, "sourceType", kb.Spec.Source.Type)
		return ctrl.Result{}, r.setStatus(ctx, &kb, metav1.ConditionFalse, reasonKBInvalidSource,
			fmt.Sprintf("spec.source.type %q is not valid; supported values: objectStorePrefix, upload",
				kb.Spec.Source.Type))
	}

	// ── Valid spec → Pending (ingestion not yet run, that is m68.6) ───────────────────────────
	log.V(1).Info("KnowledgeBase validated", "knowledgebase", kb.Name,
		"embeddingRoute", kb.Spec.EmbeddingRoute, "sourceType", kb.Spec.Source.Type)
	return ctrl.Result{}, r.setStatus(ctx, &kb, metav1.ConditionTrue, reasonKBValidated,
		fmt.Sprintf("spec is valid: embeddingRoute=%q, source.type=%q, chunking size=%d/overlap=%d/splitter=%q",
			kb.Spec.EmbeddingRoute, kb.Spec.Source.Type, size, overlap, kb.Spec.Chunking.Splitter))
}

// setStatus writes the Validated condition + phase + observedGeneration, only when something
// changed (returns the update error so a conflict requeues rather than leaving status stale).
// On a valid spec, phase is set to "Pending" (the ingestion executor, m68.6, will advance it).
// On an invalid spec, phase is left blank (the corpus is not usable).
func (r *KnowledgeBaseReconciler) setStatus(
	ctx context.Context,
	kb *agentsv1beta1.KnowledgeBase,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	phase := ""
	if status == metav1.ConditionTrue {
		phase = "Pending" // ingestion hasn't run yet; the executor (m68.6) will advance this
	}
	condChanged := apimeta.SetStatusCondition(&kb.Status.Conditions, metav1.Condition{
		Type:               conditionKBValidated,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: kb.Generation,
	})
	phaseChanged := kb.Status.Phase != phase && phase != ""
	genChanged := kb.Status.ObservedGeneration != kb.Generation
	if !condChanged && !phaseChanged && !genChanged {
		return nil
	}
	if phase != "" {
		kb.Status.Phase = phase
	}
	kb.Status.ObservedGeneration = kb.Generation
	if err := r.Status().Update(ctx, kb); err != nil {
		return fmt.Errorf("updating KnowledgeBase status: %w", err)
	}
	return nil
}

// SetupWithManager wires the controller: reconcile on KnowledgeBase changes only. KnowledgeBase
// does not reference other CRDs in this validate-only phase — cross-object watches will be added
// when the binding (m68.8: AgentDeployment.spec.knowledgeBases[]) is implemented.
func (r *KnowledgeBaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1beta1.KnowledgeBase{}).
		Named("knowledgebase").
		Complete(r)
}
