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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/objectstore"
	"github.com/ctxmesh/agent-engine/internal/run"
)

const (
	// maxDocumentUploadBytes is the maximum raw upload body the BFF accepts per
	// document (25 MiB). Over-limit requests are rejected with 413 before the body
	// is read into memory, so a hostile caller cannot exhaust the BFF's heap.
	maxDocumentUploadBytes = 25 * 1024 * 1024 // 25 MiB

	// kbNamespaceHeader is the optional request header the caller can set to
	// specify the KB's namespace. When absent, the BFF uses defaultCreateNamespace.
	kbNamespaceHeader = "X-Namespace"

	// kbSourceTypeUpload and kbSourceTypeObjectStorePrefix are the two supported
	// source.type values for a KnowledgeBase (ADR 0061 Fork 4 v1). "url" (SSRF-prone)
	// is DEFERRED.
	kbSourceTypeUpload            = "upload"
	kbSourceTypeObjectStorePrefix = "objectStorePrefix"
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

	// --- build the object key ---------------------------------------------------
	key := objectstore.KnowledgeKey(ns, kbName, filename)
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
func ResolveKBSources(
	ctx context.Context,
	store objectstore.ObjectStore,
	ns string,
	kb *agentsv1beta1.KnowledgeBase,
) ([]objectstore.ObjectInfo, error) {
	if store == nil {
		return nil, fmt.Errorf("document store not configured: OBJECT_STORE_ADDR must be set for ingestion")
	}
	switch kb.Spec.Source.Type {
	case kbSourceTypeUpload:
		prefix := objectstore.KnowledgePrefix(ns, kb.Name)
		return store.List(ctx, prefix)
	case kbSourceTypeObjectStorePrefix:
		prefix := kb.Spec.Source.ObjectStorePrefix
		if prefix == "" {
			return nil, fmt.Errorf("KnowledgeBase %q has source.type=objectStorePrefix but source.objectStorePrefix is empty", kb.Name)
		}
		return store.List(ctx, prefix)
	default:
		return nil, fmt.Errorf("KnowledgeBase %q has unsupported source.type %q (supported: upload, objectStorePrefix)", kb.Name, kb.Spec.Source.Type)
	}
}

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

	// Pin the ingestion spec: the resolved doc keys + content types + the corpus's embedding route + chunking.
	docs := make([]IngestionDoc, 0, len(infos))
	for _, info := range infos {
		docs = append(docs, IngestionDoc{
			Key:         info.Key,
			Filename:    info.Key, // the key's basename drives extraction dispatch when ContentType is generic
			ContentType: info.ContentType,
		})
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

	// Dispatch mode: leave `queued` for the worker pool. Dev/single-pod: run in-process so ingestion progresses
	// without a running worker pool (the workflows_handler precedent).
	if !s.runWorkerDispatch {
		go s.executeIngestion(contextWithConversationID(context.Background(), ""), runID)
	}

	writeJSON(w, http.StatusAccepted, IngestResponse{
		RunID:         runID,
		Status:        string(run.StatusQueued),
		DocumentCount: len(docs),
	})
}
