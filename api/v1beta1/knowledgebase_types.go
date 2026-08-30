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

package v1beta1

// KnowledgeBase is a NEW type (M68, ADR 0061) with no v1alpha1 history, so it is born directly in the
// storage version (v1beta1) as a SINGLE-version CRD — no deprecated spoke, no conversion (the
// AgentTeam/GuardrailPolicy/Workflow precedent). The CRD-version-parity guard
// (hack/check-crd-version-parity.sh) skips single-version CRDs.

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// KnowledgeBaseSource describes where a KnowledgeBase's documents come from.
// For v1, two source types are supported: "objectStorePrefix" (documents already
// in the durable KB bucket at a given prefix) and "upload" (documents uploaded via
// the BFF upload endpoint, which lands them in the same durable KB bucket).
// The CRD carries refs only — NEVER inline document content (an etcd anti-pattern, ADR 0042).
type KnowledgeBaseSource struct {
	// type identifies the source mechanism. "objectStorePrefix" reads documents from a durable
	// object-store prefix (objectStorePrefix field must be set). "upload" reads documents from
	// the durable KB bucket populated via the BFF upload endpoint (no additional field required
	// in v1 — the bucket prefix is derived from the KB name).
	// +kubebuilder:validation:Enum=objectStorePrefix;upload
	Type string `json:"type"`

	// objectStorePrefix is the durable object-store prefix from which documents are fetched when
	// type="objectStorePrefix". The ObjectStore SPI resolves this against the configured store
	// (MinIO in dev, BYO-S3 in prod). Must be set when type="objectStorePrefix"; ignored otherwise.
	// +optional
	ObjectStorePrefix string `json:"objectStorePrefix,omitempty"`
}

// ChunkingConfig pins the corpus's chunking parameters at creation time. This is one of the two
// one-way doors of a KnowledgeBase (ADR 0061 governance #6): changing chunking parameters requires
// a full re-ingest, so they are immutable after the object is created (enforced via a CEL transition
// rule — +kubebuilder:validation:XValidation on the spec field below). Sensible defaults produce a
// good out-of-the-box RAG corpus; the knobs expose the most impactful parameters.
type ChunkingConfig struct {
	// size is the target chunk size in tokens. Defaults to 512. Must be greater than 0.
	// +kubebuilder:default=512
	// +kubebuilder:validation:Minimum=1
	// +optional
	Size int `json:"size,omitempty"`

	// overlap is the number of tokens to overlap between adjacent chunks. Defaults to 64.
	// Must be less than size (validated by the controller, not at admission time, so a partial
	// update that sets size and overlap separately does not produce a transient invalid state).
	// +kubebuilder:default=64
	// +kubebuilder:validation:Minimum=0
	// +optional
	Overlap int `json:"overlap,omitempty"`

	// splitter selects the text-splitting strategy. "recursive" (default) splits on a priority
	// sequence of delimiters ("\n\n", "\n", " ") — the best general-purpose strategy. "markdown"
	// splits on Markdown structural boundaries first (##, *, ` ``` `) and is preferred when the
	// corpus is predominantly Markdown documentation.
	// +kubebuilder:validation:Enum=recursive;markdown
	// +kubebuilder:default=recursive
	// +optional
	Splitter string `json:"splitter,omitempty"`
}

// KnowledgeBaseSpec defines the desired state of a KnowledgeBase (ADR 0061).
//
// Two fields are pinned at create time and are immutable thereafter — the "one-way doors":
//  1. embeddingRoute (the corpus's embedding model): mixing embedding models in one HNSW index
//     yields plausible-looking garbage (the ADR 0045 silent-wrong-result trap); changing it
//     requires a full background re-ingest.
//  2. chunking (size/overlap/splitter): re-chunking requires re-ingest and drives retrieval quality.
//
// Both are enforced by CEL transition rules (the XValidation pattern used by AgentVersion and
// AgentRegistry in this codebase, preferred over a validating webhook).
//
// +kubebuilder:validation:XValidation:rule="self.embeddingRoute == oldSelf.embeddingRoute",message="embeddingRoute is immutable after creation (one-way door #1, ADR 0061): changing the embedding model requires a full re-ingest — delete and recreate the KnowledgeBase"
// +kubebuilder:validation:XValidation:rule="self.chunking.size == oldSelf.chunking.size && self.chunking.overlap == oldSelf.chunking.overlap && self.chunking.splitter == oldSelf.chunking.splitter",message="chunking parameters (size/overlap/splitter) are immutable after creation (one-way door #2, ADR 0061): re-chunking requires a full re-ingest — delete and recreate the KnowledgeBase"
// +kubebuilder:validation:XValidation:rule="(has(self.perUser) ? self.perUser : false) == (has(oldSelf.perUser) ? oldSelf.perUser : false)",message="perUser is immutable after creation (one-way door #3, ADR 0061 Fork 3 / m52.M17): flipping it strands org-wide chunks in a now-per-user corpus (subjectless keys skipped) or orphans per-user chunks in a now-org-wide one — delete and recreate the KnowledgeBase. The has()-normalized compare treats absent==false so a false→true flip is also rejected."
type KnowledgeBaseSpec struct {
	// displayName is an optional human-readable label for the KnowledgeBase, surfaced in
	// the console and audit log. It does not affect routing or retrieval.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// source describes where the KnowledgeBase's documents come from. The ingestion job
	// (m68.6) reads documents from this source; the source is a ref, never inline content.
	Source KnowledgeBaseSource `json:"source"`

	// embeddingRoute is the name of the gateway ModelRoute used to embed document chunks.
	// It MUST be set; it is IMMUTABLE after creation (one-way door #1, ADR 0061): the entire
	// knowledge_chunks HNSW index is keyed to this model's vector space — mixing models yields
	// silent wrong results. Changing it requires a full re-ingest (delete + recreate the KB).
	// +kubebuilder:validation:MinLength=1
	EmbeddingRoute string `json:"embeddingRoute"`

	// chunking pins the corpus's chunking parameters (size/overlap/splitter). IMMUTABLE after
	// creation (one-way door #2, ADR 0061). Sensible defaults (~512 tokens, ~64 overlap,
	// recursive splitter) produce a good out-of-the-box RAG corpus.
	// +optional
	Chunking ChunkingConfig `json:"chunking,omitempty"`

	// perUser enables per-user corpus scoping (isolates chunk retrieval to the invoking user's
	// hash, the ADR 0045 subject mandate). When true, ingestion stamps each uploading user's
	// server-derived subject hash on their chunks and retrieval scopes to the invoking user's hash,
	// so a user retrieves ONLY their own chunks; when false (default) the corpus is org-wide
	// (subject "", shared) — the byte-for-byte-unchanged legacy behaviour. The knowledge_chunks.subject
	// column exists from day one so enabling per-user needs no schema migration. Defaults to false.
	//
	// IMMUTABLE after creation (one-way door #3, ADR 0061 Fork 3 / m52.M17): the object-store key
	// layout differs (org-wide `knowledge/{ns}/{kb}/{doc}` vs per-user `.../{subject}/{doc}`), so
	// flipping it on a KB that already holds blobs is UNDEFINED — org-wide blobs are stranded (their
	// subjectless keys are skipped by per-user retrieval) or per-user blobs are orphaned. Enforced by
	// the spec-level CEL transition rule above. NOTE: a cluster HMAC-key rotation changes every
	// `userGrantHash`, orphaning every existing per-user subject — see specs/knowledge.md.
	// +optional
	PerUser bool `json:"perUser,omitempty"`

	// userStorageSoftCap is the WARN-ONLY per-user storage soft cap in bytes for a perUser corpus.
	// When a single user's ingested bytes exceed this value the controller reflects a
	// UserStorageSoftCapExceeded condition (True) on status — a visible signal, NEVER an
	// ingestion block (it mirrors the tenant corpusBytesSoftCap; the ingestion-blocking hard cap is
	// the tenant storage.corpusBytesHardCap, m80.3). 0 (unset) disables the per-user soft cap. Ignored
	// for an org-wide (perUser=false) corpus, which has no per-user accounting. (ADR 0061 Fork 3, m80.4)
	// +kubebuilder:validation:Minimum=0
	// +optional
	UserStorageSoftCap int64 `json:"userStorageSoftCap,omitempty"`

	// refreshInterval enables SCHEDULED re-ingest (M140.4): when set, the controller automatically re-runs
	// ingestion from the source once this interval elapses since the last successful ingest — so a corpus
	// backed by a changing source (an object-store prefix that gains/loses documents) stays current without a
	// manual POST /ingest. Ingestion is idempotent + cursor-resumable (unchanged content_hash = no-op; orphan
	// sweep drops removed docs), so a periodic re-ingest safely reconciles adds/changes/deletes. Unset ⇒ no
	// scheduled refresh (the byte-for-byte-unchanged manual/webhook-only behaviour). MUTABLE (unlike the
	// one-way-door fields) — the cadence can be tuned or turned off at any time. A guard interval floors it so a
	// fat-fingered tiny value can't hammer the source; the controller skips a refresh while an ingest is in flight.
	// +optional
	RefreshInterval *metav1.Duration `json:"refreshInterval,omitempty"`
}

// KnowledgeBaseStatus defines the observed state of a KnowledgeBase. Populated by the m68.1
// validate-only controller (phase/conditions) and updated by the m68.6 ingestion executor
// (documentCount/chunkCount/sizeBytes/lastIngestedAt/ingestionRunRef).
type KnowledgeBaseStatus struct {
	// phase is the coarse corpus lifecycle phase. Transitions:
	//   Pending         → validated but no ingestion run has started yet (initial state).
	//   Ingesting       → an ingestion run is in progress.
	//   Ready           → the last ingestion run completed successfully; corpus is queryable.
	//   PartiallyIngested → some documents extracted < N chars and were flagged (ADR 0061 §5);
	//                       the corpus is queryable but incomplete.
	//   Failed          → the last ingestion run failed for a non-budget reason.
	//   BudgetExceeded  → the last ingestion run was halted by a tenant budget 402 (fail-soft,
	//                     resumable — the ingestion cursor is preserved, ADR 0061 Fork 2).
	// +kubebuilder:validation:Enum=Pending;Ingesting;Ready;PartiallyIngested;Failed;BudgetExceeded
	// +optional
	Phase string `json:"phase,omitempty"`

	// observedGeneration is the .metadata.generation that was last fully reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// documentCount is the number of source documents in the corpus as of the last successful
	// ingestion run. Populated by the m68.6 ingestion executor.
	// +optional
	DocumentCount int32 `json:"documentCount,omitempty"`

	// chunkCount is the number of stored chunks in knowledge_chunks as of the last successful
	// ingestion run. Populated by the m68.6 ingestion executor.
	// +optional
	ChunkCount int32 `json:"chunkCount,omitempty"`

	// sizeBytes is the total bytes of raw source documents in the durable KB bucket as of the
	// last successful ingestion run. Reported for tenant storage soft-cap accounting (ADR 0061
	// governance #7); hard storage-quota enforcement is deferred (m52 Theme M).
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// lastIngestedAt is the timestamp of the last successfully completed ingestion run.
	// +optional
	LastIngestedAt *metav1.Time `json:"lastIngestedAt,omitempty"`

	// ingestionRunRef is the name of the current or last ingestion Run in the run store.
	// Populated by the m68.6 ingestion executor; useful for debugging and status correlation.
	// +optional
	IngestionRunRef string `json:"ingestionRunRef,omitempty"`

	// lastScheduledIngestAt is when the controller last STARTED a scheduled re-ingest (M140.4), stamped on
	// each auto-refresh attempt — distinct from lastIngestedAt (which only advances on SUCCESS). The refresh
	// predicate keys next-due off max(lastIngestedAt, lastScheduledIngestAt) + spec.refreshInterval, so a
	// scheduled run that fails (lastIngestedAt never advances) is retried exactly once per interval — never
	// hotter — instead of hot-looping. nil until the first scheduled refresh fires.
	// +optional
	LastScheduledIngestAt *metav1.Time `json:"lastScheduledIngestAt,omitempty"`

	// conditions reflect the KnowledgeBase's reconciliation state.
	//   Validated=True  → the spec is valid (embeddingRoute non-empty, chunking sane, source valid).
	//   Validated=False → the spec is invalid; reason + message carry the details.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=kb,categories={agents}
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Chunks",type="integer",JSONPath=".status.chunkCount"
// +kubebuilder:printcolumn:name="Validated",type="string",JSONPath=".status.conditions[?(@.type=='Validated')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=knowledgebases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=knowledgebases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=knowledgebases/finalizers,verbs=update

// KnowledgeBase is a namespaced managed RAG corpus: a user uploads documents; the platform
// chunks, embeds, indexes, and serves them for retrieval-augmented generation (M68, ADR 0061).
// It is a SINGLE-version CRD born directly in v1beta1 — no v1alpha1 history, no conversion webhook.
//
// Key design properties:
//   - embeddingRoute and chunking parameters are IMMUTABLE after creation (the two one-way doors,
//     enforced by CEL transition rules). Changing either requires delete + recreate.
//   - A finalizer (knowledgebase.agents.ctxmesh.ai/finalizer) guards deletion so the two-store GC
//     (knowledge_chunks partition + durable bucket prefix) runs before the object is removed
//     (ADR 0061 governance #3). The real two-store GC lands in m68.10; m68.1 registers the
//     finalizer and removes it as a documented no-op placeholder.
//   - The spec carries refs only, never inline document content (an etcd anti-pattern, ADR 0042).
//   - v1 scope: org-wide corpora only (perUser ingestion gated off); text/MD/HTML/born-digital PDF
//     ingestion; objectStorePrefix + upload sources.
type KnowledgeBase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KnowledgeBaseSpec   `json:"spec"`
	Status KnowledgeBaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KnowledgeBaseList contains a list of KnowledgeBase.
type KnowledgeBaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBase `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &KnowledgeBase{}, &KnowledgeBaseList{})
		return nil
	})
}
