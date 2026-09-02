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

package bff

// KnowledgeBase BFF surface (M68, ADR 0061 Fork 4):
//
//   POST /api/knowledgebases/{name}/documents
//     Stream a document into the durable KB object-store bucket. The named
//     KnowledgeBase must exist in the caller's namespace; the document is stored
//     at KnowledgeKey(ns, name, filename). Returns 201 + a JSON {documentRef,
//     key, size}. Unconfigured store → 501 (no OBJECT_STORE_ADDR).
//
// Source resolution seam (consumed by m68.6 ingestion executor):
//   ResolveKBSources(ctx, store, ns, kb) → []ObjectInfo
//   Resolves the document object keys the ingestion executor should read, based
//   on the KnowledgeBase's spec.source:
//     - "upload"            → store.List(KnowledgePrefix(ns, kb))
//     - "objectStorePrefix" → store.List(spec.source.objectStorePrefix)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/ingestion"
	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

const (
	// maxDocumentUploadBytes is the maximum raw upload body the BFF accepts per
	// document (25 MiB). Over-limit requests are rejected with 413 before the body
	// is read into memory, so a hostile caller cannot exhaust the BFF's heap.
	maxDocumentUploadBytes = 25 * 1024 * 1024 // 25 MiB

	// kbNamespaceHeader is the optional request header the caller can set to
	// specify the KB's namespace. When absent, the BFF uses defaultCreateNamespace.
	kbNamespaceHeader = "X-Namespace"

	// The supported source.type values moved to internal/ingestion with ResolveKBSources (M140.4) —
	// ingestion.SourceTypeUpload / ingestion.SourceTypeObjectStorePrefix are now the single definition.
)

// DocumentUploadResponse is returned (201) on a successful document upload.
type DocumentUploadResponse struct {
	// DocumentRef is the basename of the document (the sanitized filename).
	DocumentRef string `json:"documentRef"`
	// Key is the full object-store key the document was stored under.
	Key string `json:"key"`
	// Size is the number of bytes written.
	Size int64 `json:"size"`
}

// handleUploadKBDocument serves POST /api/knowledgebases/{name}/documents.
//
// Body format: raw bytes, any content type; the document filename is taken from
// the ?filename query parameter (required) or the Content-Disposition header
// (filename= parameter). Raw body was chosen over multipart because the ingestion
// use case typically streams one document at a time and multipart adds framing
// overhead without value.
//
// Auth: caller-scoped bearer token (ADR 0011). The BFF looks up the named
// KnowledgeBase in the caller's namespace via the caller-scoped client, so K8s
// RBAC governs who can upload documents to a KB.
//
// Error responses (honest 4xx, ADR 0027):
//   - 400 — missing ?filename, filename sanitizes to empty, or no namespace
//   - 404 — KB does not exist in caller's namespace
//   - 413 — body exceeds maxDocumentUploadBytes
//   - 501 — object store not configured (OBJECT_STORE_ADDR not set)
//   - 502 — object store write failed
func (s *Server) handleUploadKBDocument(w http.ResponseWriter, r *http.Request) {
	// --- resolve caller-scoped client (ADR 0011) --------------------------------
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	// --- resolve namespace -------------------------------------------------------
	ns := r.Header.Get(kbNamespaceHeader)
	if ns == "" {
		ns = r.URL.Query().Get("namespace")
	}
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// --- resolve KB name from URL path ------------------------------------------
	kbName := r.PathValue("name")
	if kbName == "" {
		writeError(w, http.StatusBadRequest, "KB name is required in the URL path")
		return
	}

	// --- resolve filename --------------------------------------------------------
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		// Fall back to Content-Disposition: attachment; filename="foo.pdf"
		_, params := parseContentDisposition(r.Header.Get("Content-Disposition"))
		filename = params["filename"]
	}
	if filename == "" {
		writeError(w, http.StatusBadRequest, "?filename query parameter or Content-Disposition filename is required")
		return
	}

	// --- check upload size limit BEFORE reading body ----------------------------
	// http.MaxBytesReader limits the body and causes a 413-detectable error on
	// Read, so we detect it before streaming to the store.
	r.Body = http.MaxBytesReader(w, r.Body, maxDocumentUploadBytes)

	// --- object store gate -------------------------------------------------------
	if s.docStore == nil {
		writeError(w, http.StatusNotImplemented, "document store not configured: set OBJECT_STORE_ADDR to enable KB uploads")
		return
	}

	// --- KB existence check (caller-scoped, ADR 0011) ---------------------------
	var kb agentsv1beta1.KnowledgeBase
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: kbName}, &kb); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("KnowledgeBase %q not found in namespace %q", kbName, ns))
			return
		}
		if apierrors.IsForbidden(err) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to access KnowledgeBase %q", kbName))
			return
		}
		if apierrors.IsUnauthorized(err) {
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
			return
		}
		s.log.Error(err, "get KnowledgeBase failed", "ns", ns, "name", kbName)
		writeError(w, http.StatusInternalServerError, "failed to look up KnowledgeBase")
		return
	}

	// --- storage hard-cap gate (m80.3, ADR 0061 governance #7 hard enforcement) -------------------
	// Reject an upload to a tenant already at its corpus hard cap. The AT-CAP state is read from the
	// namespace→tenant mirror the Tenant controller PROJECTS (ADR 0011: the controller owns the
	// cross-namespace corpus aggregation; the BFF holds NO Tenant/agent-CRD cross-namespace RBAC and
	// does NOT aggregate here). This is bounded-eventually-consistent: a burst of uploads between two
	// reconciles can overshoot the cap by at most (burst × maxDocumentUploadBytes = 25 MiB) before the
	// next reconcile flips the flag — an accepted tradeoff for a storage guardrail (not a security
	// boundary). Fail-OPEN when the store is unconfigured / the namespace is unknown: the guard must
	// never wedge uploads for a namespace outside any tenant or before the mirror has converged.
	if s.namespaceTenantStore != nil {
		exceeded, _, capErr := s.namespaceTenantStore.StorageHardCapExceededFor(r.Context(), ns)
		if capErr != nil {
			// A store read error must not silently drop the guard, but must not fail-closed either
			// (a Postgres blip would wedge all uploads). Log + fail-open — the controller re-projects.
			s.log.Error(capErr, "upload: storage hard-cap lookup failed; allowing (fail-open)", "ns", ns, "kb", kbName)
		} else if exceeded {
			writeErrorCode(w, http.StatusRequestEntityTooLarge, errCodeStorageQuotaExceeded,
				fmt.Sprintf("tenant storage hard cap reached for namespace %q — the corpus is at or over its "+
					"configured limit; delete documents or raise the tenant's storage.corpusBytesHardCap before uploading more", ns))
			return
		}
	}

	// --- build the object key ---------------------------------------------------
	// Per-user KB (spec.perUser, ADR 0061 Fork 3): nest the uploading caller's server-derived subject
	// hash as a path segment so (a) the off-request ingestion executor can recover WHOSE bytes these are
	// (SubjectFromKey at ingest-create) and (b) two users uploading the same filename never collide on one
	// object key. The subject is the UN-FORGEABLE userGrantHash of the caller's SelfSubjectReview identity
	// (ADR 0045) — the SAME derivation the run capability's User claim uses, so a user's ingested chunks are
	// stamped with the exact subject their knowledge_search scopes by. A non-perUser KB is byte-for-byte
	// unchanged (org-wide key, no subject segment).
	var key string
	if kb.Spec.PerUser {
		username, uErr := callerUsername(r.Context(), caller)
		if uErr != nil {
			// Per-user attribution is fail-CLOSED: without a trusted caller identity we cannot stamp the
			// correct subject, and stamping the wrong (or empty) one would misattribute another user's corpus.
			s.log.Error(uErr, "upload: resolving caller identity for per-user KB failed", "ns", ns, "kb", kbName)
			writeError(w, http.StatusForbidden,
				"per-user KnowledgeBase upload requires a resolvable caller identity (the API server returned none)")
			return
		}
		key = objectstore.KnowledgeKeyForSubject(ns, kbName, userGrantHash(username), filename)
	} else {
		key = objectstore.KnowledgeKey(ns, kbName, filename)
	}
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// --- stream to store --------------------------------------------------------
	// Use an io.TeeReader to count bytes while streaming; detect the MaxBytesReader
	// limit error (http.ErrHandlerTimeout is a 413 sentinel in http package).
	counter := &countingReader{r: r.Body}
	if err := s.docStore.Put(r.Context(), key, counter, -1, contentType); err != nil {
		// Detect MaxBytesReader size limit (the error wraps max bytes exceeded).
		if isRequestBodyTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("document exceeds maximum upload size (%d MiB)", maxDocumentUploadBytes/(1024*1024)))
			return
		}
		s.log.Error(err, "upload KB document failed", "ns", ns, "kb", kbName, "key", key)
		writeError(w, http.StatusBadGateway, "failed to store document: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, DocumentUploadResponse{
		DocumentRef: filename,
		Key:         key,
		Size:        counter.n,
	})
}

// countingReader wraps an io.Reader and counts bytes read.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// isRequestBodyTooLarge detects the error produced by http.MaxBytesReader when
// the body exceeds the limit. The error type is *http.MaxBytesError in Go 1.19+.
func isRequestBodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	if err == nil {
		return false
	}
	// Walk the error chain.
	for e := err; e != nil; e = unwrapOnce(e) {
		if _, ok := e.(*http.MaxBytesError); ok {
			_ = mbe
			return true
		}
	}
	return false
}

// unwrapOnce unwraps one level of error wrapping; nil when there is no Unwrap.
func unwrapOnce(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		return u.Unwrap()
	}
	return nil
}

// parseContentDisposition parses a Content-Disposition header value into its
// disposition type and parameters (e.g. filename= value). This is a minimal
// parser covering the common cases — only ";" is treated as a separator and
// values may or may not be quoted.
func parseContentDisposition(h string) (disp string, params map[string]string) {
	params = make(map[string]string)
	if h == "" {
		return "", params
	}
	// Split on ";".
	parts := splitHeader(h, ';')
	if len(parts) == 0 {
		return "", params
	}
	disp = trimQuotes(trim(parts[0]))
	for _, part := range parts[1:] {
		part = trim(part)
		idx := indexByte(part, '=')
		if idx < 0 {
			continue
		}
		k := trim(part[:idx])
		v := trimQuotes(trim(part[idx+1:]))
		params[k] = v
	}
	return disp, params
}

func splitHeader(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// -------------------------------------------------------------------------
// Source resolution seam (m68.6 ingestion executor interface)
// -------------------------------------------------------------------------

// ResolveKBSources returns the list of object keys the m68.6 ingestion executor
// should read for the given KnowledgeBase. It is the SOLE seam between the
// durable object store and the ingestion worker — the ingestion executor calls
// this function to discover documents, then calls store.Get for each key.
//
// Supported source types (ADR 0061 Fork 4 v1):
//   - "upload":            documents previously uploaded via POST
//     /api/knowledgebases/{name}/documents land in the
//     durable bucket under KnowledgePrefix(ns, kbName).
//   - "objectStorePrefix": documents already in the durable bucket at an
//     operator-supplied prefix (kb.Spec.Source.ObjectStorePrefix).
//
// "url" (SSRF-prone) is DEFERRED — not implemented here.
//
// The ingestion executor (m68.6) should call:
//
//	keys, err := bff.ResolveKBSources(ctx, store, ns, &kb)
//	for _, info := range keys {
//	    rc, err := store.Get(ctx, info.Key)
//	    // ... chunk, embed, index ...
//	}
//
// ResolveKBSources is the canonical source enumerator, now in internal/ingestion (M140.4) — aliased here so
// the existing BFF callers are unchanged while the KB controller's scheduled re-ingest shares the SAME
// implementation (one source of truth, no drift).
var ResolveKBSources = ingestion.ResolveKBSources

// -------------------------------------------------------------------------
// Ingest trigger endpoint (m68.6)
// -------------------------------------------------------------------------

// IngestResponse is returned (202) when an ingestion run is created.
type IngestResponse struct {
	// RunID is the durable ingestion Run's id — pollable via GET /api/runs/{id} (and the SSE stream).
	RunID string `json:"runId"`
	// Status is the run's initial status ("queued").
	Status string `json:"status"`
	// DocumentCount is the number of documents resolved + pinned for this ingestion.
	DocumentCount int `json:"documentCount"`
}

// handleIngestKB serves POST /api/knowledgebases/{name}/ingest (m68.6, ADR 0061 Fork 2). It is what the console
// (m68.13) and a user call to (re-)ingest a corpus. Caller-scoped (ADR 0011): it resolves the KB through the
// caller's own client (K8s RBAC gates who can trigger ingestion), resolves the document list from the source,
// PINS an IngestionSpec (source/embeddingRoute/chunking/doc-keys — the snapshot the off-request executor drives),
// and creates a durable ingestion Run (queued for the worker pool in dispatch mode, or executed in-process in
// dev — the workflows_handler create-path precedent). Returns 202 + the run id.
//
// Honest errors (ADR 0027):
//   - 400 — missing KB name
//   - 404 — KB not found in the caller's namespace
//   - 403/401 — RBAC denial / token rejected
//   - 501 — knowledge store / embedder / object store not configured (ingestion is unwired)
//   - 422 — the source cannot be resolved (bad source.type / empty prefix / no documents)
func (s *Server) handleIngestKB(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	// Ingestion is only meaningful when the direct-write path is wired (governance #8: the worker embeds +
	// writes knowledge_chunks directly). Degrade honestly (the DocStore-nil→501 pattern) rather than create a
	// run that would immediately fail in the executor.
	if s.knowledgeStore == nil || s.embedder == nil {
		writeError(w, http.StatusNotImplemented,
			"ingestion not configured: set CONTROLPLANE_DSN (knowledge store) and MODEL_GATEWAY_URL (embedder)")
		return
	}
	if s.docStore == nil {
		writeError(w, http.StatusNotImplemented,
			"document store not configured: set OBJECT_STORE_ADDR to enable KB ingestion")
		return
	}

	ns := r.Header.Get(kbNamespaceHeader)
	if ns == "" {
		ns = r.URL.Query().Get("namespace")
	}
	if ns == "" {
		ns = defaultCreateNamespace
	}

	kbName := r.PathValue("name")
	if kbName == "" {
		writeError(w, http.StatusBadRequest, "KB name is required in the URL path")
		return
	}

	// Resolve the KB (caller-scoped, 404 if absent) — the same RBAC gate as the upload endpoint.
	var kb agentsv1beta1.KnowledgeBase
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: kbName}, &kb); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, fmt.Sprintf("KnowledgeBase %q not found in namespace %q", kbName, ns))
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to access KnowledgeBase %q", kbName))
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "get KnowledgeBase failed", "ns", ns, "name", kbName)
			writeError(w, http.StatusInternalServerError, "failed to look up KnowledgeBase")
		}
		return
	}

	// Resolve the document list from the source (upload prefix / objectStorePrefix) — snapshotted at create.
	infos, err := ResolveKBSources(r.Context(), s.docStore, ns, &kb)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "failed to resolve KB sources: "+err.Error())
		return
	}

	// An ingest that resolves NO documents is refused, not accepted (M148/m148.11).
	//
	// It used to return 202 with documentCount:0 and the run then SUCCEEDED having done
	// nothing — so the console showed an ingested KB, and retrieval's later emptiness
	// looked like a model or embedding problem rather than a corpus that was never read.
	// Found live: an ingest issued in the same second as the last upload resolved zero
	// documents, reported success, and wrote no chunks.
	//
	// Ingesting nothing is never what the caller meant. 422 says so while the caller is
	// still watching, which is the whole difference.
	if len(infos) == 0 {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("KnowledgeBase %q has no documents to ingest. Upload documents first; "+
				"if you just uploaded, retry — a document is ingestible only once the object "+
				"store lists it.", kbName))
		return
	}

	// Pin the ingestion spec: the resolved doc keys + content types + the corpus's embedding route + chunking.
	// For a PER-USER KB (ADR 0061 Fork 3) recover each document's owner subject from its object key (the subject
	// is nested as a path segment at upload — KnowledgeKeyForSubject) and pin it per-document, so the off-request
	// executor stamps the correct owner on that document's chunks. A perUser doc whose key carries NO subject
	// segment is fail-CLOSED skipped: it predates per-user attribution (or was written org-wide) and must not be
	// silently ingested as some default subject. A non-perUser KB pins Subject "" (org-wide, unchanged).
	docs := make([]IngestionDoc, 0, len(infos))
	var skippedUnattributed int
	for _, info := range infos {
		subject := ""
		if kb.Spec.PerUser {
			subject = objectstore.SubjectFromKey(ns, kbName, info.Key)
			if subject == "" {
				// No recoverable owner on a per-user corpus → do not misattribute; skip this document.
				skippedUnattributed++
				continue
			}
		}
		docs = append(docs, IngestionDoc{
			Key:         info.Key,
			Filename:    info.Key, // the key's basename drives extraction dispatch when ContentType is generic
			ContentType: info.ContentType,
			Subject:     subject,
		})
	}
	if skippedUnattributed > 0 {
		s.log.Info("ingest: skipped per-user documents with no recoverable owner subject in their key",
			"ns", ns, "kb", kbName, "skipped", skippedUnattributed)
	}
	spec := IngestionSpec{
		Namespace:      ns,
		KnowledgeBase:  kbName,
		EmbeddingRoute: kb.Spec.EmbeddingRoute,
		Chunking:       kb.Spec.Chunking,
		Documents:      docs,
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		s.log.Error(err, "marshal ingestion spec", "ns", ns, "kb", kbName)
		writeError(w, http.StatusInternalServerError, "failed to pin the ingestion spec")
		return
	}

	runID, err := randToken(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to mint a run id")
		return
	}

	// Create the ingestion Run. Its Agent is the KB name (the audit/display label — an ingestion run has no
	// agent; the executor drives it off IngestionSpec, never off Agent). No conversation, no OBO-to-a-model.
	rn := run.New(runID, ns, kbName, nil, "", time.Now())
	rn.IngestionRef = kbName
	rn.IngestionSpec = string(specJSON)
	if err := s.runStore.Create(rn); err != nil {
		s.log.Error(err, "create ingestion run failed", "ns", ns, "kb", kbName)
		writeError(w, http.StatusInternalServerError, "failed to create the ingestion run")
		return
	}

	// Mark the corpus Ingesting on KB.status via the CALLER'S client (ADR 0061 Fork 2: the ingest endpoint is
	// caller-scoped and holds KB RBAC — unlike the off-request executor, which has no KB-status RBAC and instead
	// records the terminal outcome on the corpus-status row). Set phase=Ingesting + ingestionRunRef so the console
	// shows an in-flight ingestion immediately; the controller's periodic reconcile then projects the terminal
	// phase from the corpus-status row. Best-effort: a status-write failure does not fail the (already-created)
	// run — the terminal phase still lands via the controller.
	setKBIngesting(r.Context(), caller, &kb, runID)

	// Left `queued` for the worker pool — one run path since M143.1 (ADR 0125).
	writeJSON(w, http.StatusAccepted, IngestResponse{
		RunID:         runID,
		Status:        string(run.StatusQueued),
		DocumentCount: len(docs),
	})
}

// setKBIngesting sets phase=Ingesting + ingestionRunRef on a KnowledgeBase's status via the caller's client
// (the caller holds KB-status RBAC; ADR 0061 Fork 2). Change-guarded (never a no-op write-storm) and
// best-effort: a conflict/failure is logged, not returned — the controller's reconcile is the authoritative
// projector of the terminal phase, so a missed Ingesting flip only delays the console's in-flight indicator.
func setKBIngesting(ctx context.Context, caller client.Client, kb *agentsv1beta1.KnowledgeBase, runID string) {
	if kb.Status.Phase == "Ingesting" && kb.Status.IngestionRunRef == runID {
		return // already reflecting this run — no write.
	}
	kb.Status.Phase = "Ingesting"
	kb.Status.IngestionRunRef = runID
	if err := caller.Status().Update(ctx, kb); err != nil {
		logf.FromContext(ctx).Info("ingest: could not mark KB Ingesting (non-fatal; the controller projects the terminal phase)",
			"kb", kb.Name, "ns", kb.Namespace, "err", err.Error())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Read + test-query endpoints (m68.13)
// ─────────────────────────────────────────────────────────────────────────────

// KBSummary is the BFF's flat projection of one KnowledgeBase for the console list (m68.13).
// It is a DISPLAY-ONLY DTO — no inline document content (the CRD stores none), no etcd anti-pattern.
type KBSummary struct {
	Name           string  `json:"name"`
	Namespace      string  `json:"namespace"`
	Phase          string  `json:"phase"`
	ChunkCount     int32   `json:"chunkCount"`
	DocumentCount  int32   `json:"documentCount"`
	SizeBytes      int64   `json:"sizeBytes"`
	LastIngestedAt *string `json:"lastIngestedAt,omitempty"` // RFC3339 or absent
	EmbeddingRoute string  `json:"embeddingRoute"`
}

// KBCondition is one status condition projected from metav1.Condition.
type KBCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// KBDetail extends KBSummary with full spec + conditions for the detail page (m68.13).
type KBDetail struct {
	KBSummary
	DisplayName     string        `json:"displayName,omitempty"`
	SourceType      string        `json:"sourceType"`
	ChunkSize       int           `json:"chunkSize"`
	ChunkOverlap    int           `json:"chunkOverlap"`
	ChunkSplitter   string        `json:"chunkSplitter"`
	IngestionRunRef string        `json:"ingestionRunRef,omitempty"`
	Conditions      []KBCondition `json:"conditions"`
}

// KBListResponse is the list-contract DTO for GET /api/knowledgebases.
type KBListResponse struct {
	Items []KBSummary `json:"items"`
}

// kbSummaryFrom projects a KnowledgeBase CRD into a KBSummary.
func kbSummaryFrom(kb agentsv1beta1.KnowledgeBase) KBSummary {
	s := KBSummary{
		Name:           kb.Name,
		Namespace:      kb.Namespace,
		Phase:          kb.Status.Phase,
		ChunkCount:     kb.Status.ChunkCount,
		DocumentCount:  kb.Status.DocumentCount,
		SizeBytes:      kb.Status.SizeBytes,
		EmbeddingRoute: kb.Spec.EmbeddingRoute,
	}
	if kb.Status.LastIngestedAt != nil {
		ts := kb.Status.LastIngestedAt.UTC().Format(time.RFC3339)
		s.LastIngestedAt = &ts
	}
	return s
}

// kbDetailFrom projects a KnowledgeBase CRD into a KBDetail.
func kbDetailFrom(kb agentsv1beta1.KnowledgeBase) KBDetail {
	d := KBDetail{
		KBSummary:       kbSummaryFrom(kb),
		DisplayName:     kb.Spec.DisplayName,
		SourceType:      kb.Spec.Source.Type,
		ChunkSize:       kb.Spec.Chunking.Size,
		ChunkOverlap:    kb.Spec.Chunking.Overlap,
		ChunkSplitter:   kb.Spec.Chunking.Splitter,
		IngestionRunRef: kb.Status.IngestionRunRef,
	}
	d.Conditions = make([]KBCondition, 0, len(kb.Status.Conditions))
	for _, c := range kb.Status.Conditions {
		d.Conditions = append(d.Conditions, KBCondition{
			Type:               c.Type,
			Status:             string(c.Status),
			Reason:             c.Reason,
			Message:            c.Message,
			LastTransitionTime: c.LastTransitionTime.UTC().Format(time.RFC3339),
		})
	}
	return d
}

// handleListKBs serves GET /api/knowledgebases (m68.13).
//
// Lists KnowledgeBases in the caller's namespace(s) (caller-scoped, ADR 0011).
// Returns a KBListResponse. Supports ?namespace= to scope to one namespace.
func (s *Server) handleListKBs(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	ns := r.URL.Query().Get("namespace")

	var kbList agentsv1beta1.KnowledgeBaseList
	listOpts := []client.ListOption{}
	if ns != "" {
		listOpts = append(listOpts, client.InNamespace(ns))
	}
	if err := caller.List(r.Context(), &kbList, listOpts...); err != nil {
		if apierrors.IsForbidden(err) {
			writeError(w, http.StatusForbidden, "forbidden: not allowed to list KnowledgeBases")
			return
		}
		if apierrors.IsUnauthorized(err) {
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
			return
		}
		s.log.Error(err, "list KnowledgeBases failed")
		writeError(w, http.StatusInternalServerError, "failed to list KnowledgeBases")
		return
	}

	items := make([]KBSummary, 0, len(kbList.Items))
	for _, kb := range kbList.Items {
		items = append(items, kbSummaryFrom(kb))
	}
	writeJSON(w, http.StatusOK, KBListResponse{Items: items})
}

// handleGetKB serves GET /api/knowledgebases/{name} (m68.13).
//
// Returns a KBDetail for the named KnowledgeBase in the caller's namespace.
// 404 when absent or RBAC-hidden; 403 when explicitly denied (ADR 0011).
func (s *Server) handleGetKB(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	kbName := r.PathValue("name")
	if kbName == "" {
		writeError(w, http.StatusBadRequest, "KB name is required in the URL path")
		return
	}

	ns := r.Header.Get(kbNamespaceHeader)
	if ns == "" {
		ns = r.URL.Query().Get("namespace")
	}
	if ns == "" {
		ns = defaultCreateNamespace
	}

	var kb agentsv1beta1.KnowledgeBase
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: kbName}, &kb); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, fmt.Sprintf("KnowledgeBase %q not found in namespace %q", kbName, ns))
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to access KnowledgeBase %q", kbName))
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "get KnowledgeBase failed", "ns", ns, "name", kbName)
			writeError(w, http.StatusInternalServerError, "failed to look up KnowledgeBase")
		}
		return
	}

	writeJSON(w, http.StatusOK, kbDetailFrom(kb))
}

// kbSearchRequest is the console TEST-QUERY body (m68.13, POST /api/knowledgebases/{name}/search).
type kbSearchRequest struct {
	Query     string  `json:"query"`
	TopK      int     `json:"topK,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
}

// kbSearchHit is one result chunk as projected by the BFF (the m68.11 citation surface).
type kbSearchHit struct {
	Content     string  `json:"content"`
	DocumentRef string  `json:"documentRef"`
	ChunkIndex  int     `json:"chunkIndex"`
	Score       float64 `json:"score"`
	Truncated   bool    `json:"truncated,omitempty"`
}

// kbSearchResponse is the BFF response for POST /api/knowledgebases/{name}/search.
type kbSearchResponse struct {
	Results []kbSearchHit `json:"results"`
}

// handleSearchKB serves POST /api/knowledgebases/{name}/search (m68.13).
//
// The console TEST-QUERY panel: verify the KB exists (caller-scoped, 404 if absent), then forward
// to the token-service POST /v1/knowledge/search with the KB's embeddingRoute as embeddingModel.
// Returns ranked chunks with citation fields (documentRef, chunkIndex, score, content snippet).
//
// Token-service unconfigured → honest 501 (never a panic).
// KB not found → 404. Caller denied → 403.
func (s *Server) handleSearchKB(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	// Degrade honestly when the token-service is not configured.
	if s.tokenServiceURL == "" {
		writeError(w, http.StatusNotImplemented,
			"knowledge search not configured: set TOKEN_SERVICE_URL to enable KB test-query")
		return
	}

	kbName := r.PathValue("name")
	if kbName == "" {
		writeError(w, http.StatusBadRequest, "KB name is required in the URL path")
		return
	}

	ns := r.Header.Get(kbNamespaceHeader)
	if ns == "" {
		ns = r.URL.Query().Get("namespace")
	}
	if ns == "" {
		ns = defaultCreateNamespace
	}

	// KB existence + embeddingRoute resolution (caller-scoped, ADR 0011).
	var kb agentsv1beta1.KnowledgeBase
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: kbName}, &kb); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			writeError(w, http.StatusNotFound, fmt.Sprintf("KnowledgeBase %q not found in namespace %q", kbName, ns))
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, fmt.Sprintf("forbidden: not allowed to access KnowledgeBase %q", kbName))
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, msgTokenRejected)
		default:
			s.log.Error(err, "get KnowledgeBase failed", "ns", ns, "name", kbName)
			writeError(w, http.StatusInternalServerError, "failed to look up KnowledgeBase")
		}
		return
	}

	// Decode the test-query request body.
	var req kbSearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}

	// For a PER-USER KB (ADR 0061 Fork 3), scope the console test-query to the CALLER'S OWN subject hash so a
	// user's test-query retrieves only their own chunks — the exact same server-derived userGrantHash the agent
	// retrieval path scopes by, and the same hash the upload path stamped. Fail-CLOSED: if the caller identity
	// cannot be resolved we refuse rather than search under "" (which would leak org-wide/other-user chunks).
	// An org-wide KB keeps subject "" (unchanged).
	subject := ""
	if kb.Spec.PerUser {
		username, uErr := callerUsername(r.Context(), caller)
		if uErr != nil {
			s.log.Error(uErr, "search: resolving caller identity for per-user KB failed", "ns", ns, "kb", kbName)
			writeError(w, http.StatusForbidden,
				"per-user KnowledgeBase search requires a resolvable caller identity (the API server returned none)")
			return
		}
		subject = userGrantHash(username)
	}

	// Build the token-service search request, using the KB's embeddingRoute as the embedding model.
	// This mirrors the knowledgeSearchRequest shape in internal/credplane/knowledge.go.
	tsReq := struct {
		Namespace      string  `json:"namespace"`
		KnowledgeBase  string  `json:"knowledgeBase"`
		Subject        string  `json:"subject,omitempty"`
		Query          string  `json:"query"`
		TopK           int     `json:"topK,omitempty"`
		Threshold      float64 `json:"threshold,omitempty"`
		EmbeddingModel string  `json:"embeddingModel"`
	}{
		Namespace:      ns,
		KnowledgeBase:  kbName,
		Subject:        subject,
		Query:          req.Query,
		TopK:           req.TopK,
		Threshold:      req.Threshold,
		EmbeddingModel: kb.Spec.EmbeddingRoute,
	}
	tsReqJSON, err := json.Marshal(tsReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal search request")
		return
	}

	// Forward to the token-service /v1/knowledge/search (the same path the launcher uses).
	tsURL := s.tokenServiceURL + "/v1/knowledge/search"
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, tsURL, bytes.NewReader(tsReqJSON))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build token-service request")
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Use the BFF's mTLS token-service client (the same platform material the grant-delegation path
	// uses) — the token-service serves on an mTLS port in prod, so http.DefaultClient would fail the
	// TLS handshake. Fall back to the default client only in dev (no BFF_TOKEN_SERVICE_TLS_* ⇒ plain HTTP).
	tsClient := s.tokenServiceClient
	if tsClient == nil {
		tsClient = http.DefaultClient
	}
	resp, err := tsClient.Do(httpReq)
	if err != nil {
		s.log.Error(err, "knowledge search: token-service request failed", "ns", ns, "kb", kbName)
		writeError(w, http.StatusBadGateway, "knowledge search failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// The token-service returns knowledgeSearchResponse{Results, Error}. We project it
	// to the BFF's kbSearchResponse (same shape, subset of fields for the console).
	var tsResp struct {
		Results []struct {
			Content     string  `json:"content"`
			DocumentRef string  `json:"documentRef"`
			ChunkIndex  int     `json:"chunkIndex"`
			Score       float64 `json:"score"`
			Truncated   bool    `json:"truncated,omitempty"`
		} `json:"results"`
		Error string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tsResp); err != nil {
		s.log.Error(err, "knowledge search: failed to decode token-service response", "ns", ns, "kb", kbName)
		writeError(w, http.StatusBadGateway, "knowledge search: bad response from token-service")
		return
	}

	if tsResp.Error != "" {
		// The token-service returned an application-level error (e.g. "unsupported").
		writeError(w, http.StatusBadGateway, "knowledge search: "+tsResp.Error)
		return
	}

	hits := make([]kbSearchHit, 0, len(tsResp.Results))
	for _, r := range tsResp.Results {
		hits = append(hits, kbSearchHit{
			Content:     r.Content,
			DocumentRef: r.DocumentRef,
			ChunkIndex:  r.ChunkIndex,
			Score:       r.Score,
			Truncated:   r.Truncated,
		})
	}
	writeJSON(w, http.StatusOK, kbSearchResponse{Results: hits})
}
