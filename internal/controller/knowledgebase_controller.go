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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/knowledge"
	"github.com/ctxmesh/agent-engine/internal/objectstore"
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

// corpusStore is the narrow slice of the knowledge store the reconciler needs: the finalizer's DB-half GC
// (DeleteCorpus — drops the knowledge_chunks partition + the corpus-status row) and the status projection
// (GetCorpusStatus — the coarse ingestion outcome the executor wrote on cpDB). A narrow interface lets the
// envtest inject a fake without a real Postgres and keeps the reconciler decoupled from the write path.
type corpusStore interface {
	DeleteCorpus(ctx context.Context, namespace, knowledgeBase string) error
	GetCorpusStatus(ctx context.Context, namespace, knowledgeBase string) (knowledge.CorpusStatus, bool, error)
}

// prefixDeleter is the narrow slice of the durable object store the finalizer's bucket-half GC needs
// (DeletePrefix — purge every document under the KB's prefix). objectstore.ObjectStore satisfies it.
type prefixDeleter interface {
	DeletePrefix(ctx context.Context, prefix string) error
}

// ingestingRequeue is how long the reconciler waits before re-projecting KB.status while a corpus is Ingesting
// (there is no CRD watch on the off-request corpus-status row, so the controller polls the status channel while
// a run is in flight). A steady-state (Ready/Failed/…) corpus does not requeue — the next spec change or the
// BFF's Ingesting flip re-triggers it.
const ingestingRequeue = 10 * time.Second

// KnowledgeBaseReconciler validates the KnowledgeBase spec (m68.1), runs the finalizer's two-store GC (m68.10,
// ADR 0061 governance #3), and projects the ingestion outcome from the corpus-status channel onto KB.status
// (m68.10, ADR 0061 Fork 2). On a live object it: ensures the finalizer, validates the spec (embeddingRoute
// non-empty, chunking sane, source valid), then reconciles KB.status from the corpus-status row the ingestion
// executor wrote on cpDB. On deletion it GCs BOTH stores before releasing the finalizer.
//
// Store wiring (injected in cmd/main.go from the manager's existing cpDB + OBJECT_STORE_ADDR):
//   - Knowledge (cpDB): DeleteCorpus (finalizer DB half) + GetCorpusStatus (status projection). nil in a
//     deployment without cpDB → the finalizer skips the DB half with a WARN; status stays validate-only.
//   - ObjectStore (OBJECT_STORE_ADDR): DeletePrefix (finalizer bucket half). nil when unset (dev) → the
//     finalizer skips the bucket half with a WARN. A store that does not exist must not block deletion forever.
//
// What this controller still does NOT do (clean seams): it does not ingest/embed/write knowledge_chunks (m68.6,
// the ingestion executor), and the console reads the projected KB.status (m68.13) — it never reads cpDB directly.
type KnowledgeBaseReconciler struct {
	client.Client
	// Knowledge is the control-plane knowledge store (from cpDB). nil ⇒ the finalizer skips the DB-half GC and
	// the status projection is disabled (a deployment without CONTROLPLANE_DSN).
	Knowledge corpusStore
	// ObjectStore is the durable KB object store (from OBJECT_STORE_ADDR). nil ⇒ the finalizer skips the
	// bucket-half GC (a dev deployment without an object store).
	ObjectStore prefixDeleter
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

	// ── Deletion path: run the two-store GC before releasing the finalizer (governance #3) ──────
	if !kb.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&kb, kbFinalizer) {
			// GC BOTH stores. On any error the finalizer is NOT removed — the reconcile returns the
			// error and requeues, so a partial failure is retried and never orphans a store.
			if err := r.gcCorpus(ctx, &kb); err != nil {
				return ctrl.Result{}, err
			}
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

	// ── Valid spec → Validated=True (phase → Pending only on the very first reconcile) ─────────
	log.V(1).Info("KnowledgeBase validated", "knowledgebase", kb.Name,
		"embeddingRoute", kb.Spec.EmbeddingRoute, "sourceType", kb.Spec.Source.Type)
	if err := r.setStatus(ctx, &kb, metav1.ConditionTrue, reasonKBValidated,
		fmt.Sprintf("spec is valid: embeddingRoute=%q, source.type=%q, chunking size=%d/overlap=%d/splitter=%q",
			kb.Spec.EmbeddingRoute, kb.Spec.Source.Type, size, overlap, kb.Spec.Chunking.Splitter)); err != nil {
		return ctrl.Result{}, err
	}
	// ── Project the ingestion outcome from the corpus-status channel onto KB.status (Fork 2) ────
	return r.reconcileCorpusStatus(ctx, &kb)
}

// gcCorpus runs the finalizer's two-store GC (ADR 0061 governance #3): drop the knowledge_chunks partition
// (+ the corpus-status row, both inside DeleteCorpus) and purge the durable bucket prefix. It is idempotent
// and survives partial failure — the caller removes the finalizer ONLY when this returns nil, so a failed half
// requeues and retries, never orphaning a store. An unconfigured store (nil, e.g. a dev deployment without
// cpDB / OBJECT_STORE_ADDR) is skipped with a WARN — a store that does not exist here must not block deletion.
func (r *KnowledgeBaseReconciler) gcCorpus(
	ctx context.Context, kb *agentsv1beta1.KnowledgeBase,
) error {
	log := logf.FromContext(ctx)
	ns := kb.Namespace
	if r.Knowledge != nil {
		if err := r.Knowledge.DeleteCorpus(ctx, ns, kb.Name); err != nil {
			return fmt.Errorf("finalizer GC: dropping knowledge_chunks partition for %s/%s: %w", ns, kb.Name, err)
		}
	} else {
		log.Info("finalizer GC: knowledge store not configured — skipping the DB-half (partition) GC",
			"knowledgebase", kb.Name)
	}
	if r.ObjectStore != nil {
		if err := r.ObjectStore.DeletePrefix(ctx, objectstore.KnowledgePrefix(ns, kb.Name)); err != nil {
			return fmt.Errorf("finalizer GC: purging object-store prefix for %s/%s: %w", ns, kb.Name, err)
		}
	} else {
		log.Info("finalizer GC: object store not configured — skipping the bucket-half GC",
			"knowledgebase", kb.Name)
	}
	return nil
}

// reconcileCorpusStatus projects the coarse ingestion outcome (the corpus-status row the ingestion executor
// wrote on cpDB) onto KB.status (ADR 0061 Fork 2 — a coarse projection, change-guarded, no write-storm). The
// terminal phase (Ready/PartiallyIngested/Failed/BudgetExceeded) + counts live in the row; the transient
// `Ingesting` phase is set by the BFF ingest endpoint (caller-scoped) and polled here until the terminal row
// appears. A deployment without a knowledge store (nil) leaves status validate-only.
func (r *KnowledgeBaseReconciler) reconcileCorpusStatus(
	ctx context.Context, kb *agentsv1beta1.KnowledgeBase,
) (ctrl.Result, error) {
	if r.Knowledge == nil {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)
	cs, found, err := r.Knowledge.GetCorpusStatus(ctx, kb.Namespace, kb.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reading corpus status for %s/%s: %w", kb.Namespace, kb.Name, err)
	}
	if !found {
		// No terminal ingestion outcome yet. If the BFF flipped us to Ingesting, poll for the row.
		if kb.Status.Phase == "Ingesting" {
			return ctrl.Result{RequeueAfter: ingestingRequeue}, nil
		}
		return ctrl.Result{}, nil
	}
	changed := false
	if cs.Phase != "" && kb.Status.Phase != cs.Phase {
		kb.Status.Phase = cs.Phase
		changed = true
	}
	if kb.Status.DocumentCount != int32(cs.DocumentCount) {
		kb.Status.DocumentCount = int32(cs.DocumentCount)
		changed = true
	}
	if kb.Status.ChunkCount != int32(cs.ChunkCount) {
		kb.Status.ChunkCount = int32(cs.ChunkCount)
		changed = true
	}
	if kb.Status.SizeBytes != cs.SizeBytes {
		kb.Status.SizeBytes = cs.SizeBytes
		changed = true
	}
	if kb.Status.IngestionRunRef != cs.IngestionRunID {
		kb.Status.IngestionRunRef = cs.IngestionRunID
		changed = true
	}
	if cs.LastIngestedAt != nil {
		t := metav1.NewTime(*cs.LastIngestedAt)
		if kb.Status.LastIngestedAt == nil || !kb.Status.LastIngestedAt.Equal(&t) {
			kb.Status.LastIngestedAt = &t
			changed = true
		}
	}
	if changed {
		if err := r.Status().Update(ctx, kb); err != nil {
			return ctrl.Result{}, fmt.Errorf("projecting corpus status onto %s/%s: %w", kb.Namespace, kb.Name, err)
		}
		log.V(1).Info("KnowledgeBase status projected from corpus-status channel",
			"knowledgebase", kb.Name, "phase", cs.Phase, "chunks", cs.ChunkCount)
	}
	return ctrl.Result{}, nil
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
	if status == metav1.ConditionTrue && kb.Status.Phase == "" {
		// Only stamp Pending on the FIRST validate (empty phase). Once ingestion has advanced the phase
		// (Ingesting via the BFF, or a terminal phase via the corpus-status projection), a re-validate must
		// NOT reset it to Pending — reconcileCorpusStatus owns the phase from then on.
		phase = "Pending"
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
