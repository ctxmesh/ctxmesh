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

// Tests for the KnowledgeBases BFF endpoints (m68.13):
//   POST /api/knowledgebases/{name}/documents (handleUploadKBDocument)
//   POST /api/knowledgebases/{name}/ingest (handleIngestKB)
//   GET  /api/knowledgebases             (handleListKBs)
//   GET  /api/knowledgebases/{name}      (handleGetKB)
//   POST /api/knowledgebases/{name}/search (handleSearchKB)
//   ResolveKBSources
//
// Coverage:
//   - Upload happy path: 201 + correct JSON, object stored under the right key.
//   - Upload unknown KB: 404 (KB does not exist in the caller's namespace).
//   - Upload over-limit body: 413 (body exceeds maxDocumentUploadBytes).
//   - Upload unconfigured store: 501 (docStore nil — no OBJECT_STORE_ADDR).
//   - Upload missing filename: 400.
//   - ResolveKBSources: "upload" and "objectStorePrefix" paths.
//   - List KBs: returns KBs in the caller's namespace.
//   - Get KB: 404 on absent KB; returns KBDetail on success.
//   - Search KB: forwards to a fake token-service + returns citations.
//   - Search KB unconfigured: 501 when TOKEN_SERVICE_URL is unset.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/knowledge"
	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/agent-engine/internal/objectstore"
	"github.com/ctxmesh/agent-engine/internal/run"
)

const kbNS = "team-kb"

// --- fixture helpers --------------------------------------------------------

func mockKnowledgeBase(name, ns string) *agentsv1beta1.KnowledgeBase {
	return &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1beta1.KnowledgeBaseSpec{
			EmbeddingRoute: "text-embedding-route",
			Source: agentsv1beta1.KnowledgeBaseSource{
				Type: "upload",
			},
		},
	}
}

// postUpload sends POST /api/knowledgebases/{name}/documents with the given
// body and ?filename query param. Returns (status, body).
func postUpload(t *testing.T, s *Server, kbName, ns, filename, contentType string, body []byte) (int, []byte) {
	t.Helper()
	rawURL := "/api/knowledgebases/" + kbName + "/documents?filename=" + filename
	if ns != "" {
		rawURL += "&namespace=" + ns
	}
	req := httptest.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// --- tests ------------------------------------------------------------------

func TestUploadKBDocument_HappyPath(t *testing.T) {
	docStore := objectstore.NewMemObjectStore()
	kb := mockKnowledgeBase("my-kb", kbNS)
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()

	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		DocStore:      docStore,
	})

	content := []byte("This is a test document.")
	code, body := postUpload(t, s, "my-kb", kbNS, "guide.md", "text/markdown", content)

	require.Equal(t, http.StatusCreated, code, "expected 201; body: %s", string(body))

	var resp DocumentUploadResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Equal(t, "guide.md", resp.DocumentRef)
	assert.Equal(t, int64(len(content)), resp.Size)

	// Verify the object landed in the store under the correct key.
	expectedKey := objectstore.KnowledgeKey(kbNS, "my-kb", "guide.md")
	assert.Equal(t, expectedKey, resp.Key)

	rc, err := docStore.Get(context.Background(), expectedKey)
	require.NoError(t, err)
	stored, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, content, stored)
}

// TestUploadKBDocument_StorageHardCapRejects413 proves an upload to a tenant AT its storage hard cap is
// rejected with HTTP 413 + the typed storage_quota_exceeded code, and NOTHING is written to the store
// (m80.3). The at-cap state is the controller's PROJECTED flag read from the namespace→tenant mirror.
func TestUploadKBDocument_StorageHardCapRejects413(t *testing.T) {
	docStore := objectstore.NewMemObjectStore()
	kb := mockKnowledgeBase("my-kb", kbNS)
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()

	nsStore := namespacetenant.NewMemStore()
	require.NoError(t, nsStore.SetMembers(context.Background(), "tenant-atcap", []string{kbNS}))
	require.NoError(t, nsStore.SetStorageHardCapExceeded(context.Background(), "tenant-atcap", true))

	s := NewServer(Options{
		CallerClients:        newFakeFactory(fc),
		Scheme:               sc,
		Auth:                 AllowAll{},
		Log:                  logr.Discard(),
		DocStore:             docStore,
		NamespaceTenantStore: nsStore,
	})

	code, body := postUpload(t, s, "my-kb", kbNS, "guide.md", "text/markdown", []byte("blocked content"))
	require.Equal(t, http.StatusRequestEntityTooLarge, code, "an at-cap upload must be 413; body: %s", string(body))

	var resp errorBody
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Equal(t, errCodeStorageQuotaExceeded, resp.Code, "the 413 must carry the typed storage_quota_exceeded code")

	// The rejected upload wrote nothing to the store.
	_, err := docStore.Get(context.Background(), objectstore.KnowledgeKey(kbNS, "my-kb", "guide.md"))
	require.Error(t, err, "a rejected upload must not have persisted the object")
}

// TestUploadKBDocument_StorageHardCapUnsetAllows proves the hard-cap gate is inert when the tenant is NOT
// at the cap (the unset/under-cap default) — the upload proceeds byte-for-byte as before (201).
func TestUploadKBDocument_StorageHardCapUnsetAllows(t *testing.T) {
	docStore := objectstore.NewMemObjectStore()
	kb := mockKnowledgeBase("my-kb", kbNS)
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()

	nsStore := namespacetenant.NewMemStore()
	// The tenant owns the namespace but is under cap (no SetStorageHardCapExceeded call).
	require.NoError(t, nsStore.SetMembers(context.Background(), "tenant-undercap", []string{kbNS}))

	s := NewServer(Options{
		CallerClients:        newFakeFactory(fc),
		Scheme:               sc,
		Auth:                 AllowAll{},
		Log:                  logr.Discard(),
		DocStore:             docStore,
		NamespaceTenantStore: nsStore,
	})

	content := []byte("allowed content")
	code, body := postUpload(t, s, "my-kb", kbNS, "guide.md", "text/markdown", content)
	require.Equal(t, http.StatusCreated, code, "an under-cap upload must succeed (201); body: %s", string(body))

	stored, err := docStore.Get(context.Background(), objectstore.KnowledgeKey(kbNS, "my-kb", "guide.md"))
	require.NoError(t, err)
	got, _ := io.ReadAll(stored)
	_ = stored.Close()
	assert.Equal(t, content, got)
}

func TestUploadKBDocument_UnknownKB_Returns404(t *testing.T) {
	docStore := objectstore.NewMemObjectStore()
	sc := testScheme(t)
	// No KB objects in the fake store.
	fc := fake.NewClientBuilder().WithScheme(sc).Build()

	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		DocStore:      docStore,
	})

	code, body := postUpload(t, s, "ghost-kb", kbNS, "doc.txt", "text/plain", []byte("hello"))
	assert.Equal(t, http.StatusNotFound, code, "expected 404 for missing KB; body: %s", string(body))

	// Store must still be empty.
	items, err := docStore.List(context.Background(), "knowledge/")
	require.NoError(t, err)
	assert.Empty(t, items, "no object must be written when the KB does not exist")
}

func TestUploadKBDocument_OverLimit_Returns413(t *testing.T) {
	docStore := objectstore.NewMemObjectStore()
	kb := mockKnowledgeBase("my-kb", kbNS)
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()

	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		DocStore:      docStore,
	})

	// Build a body that is 1 byte over the limit.
	oversized := make([]byte, maxDocumentUploadBytes+1)
	for i := range oversized {
		oversized[i] = 'A'
	}

	code, body := postUpload(t, s, "my-kb", kbNS, "big.bin", "application/octet-stream", oversized)
	assert.Equal(t, http.StatusRequestEntityTooLarge, code, "expected 413 for over-limit body; body: %s", string(body))
}

func TestUploadKBDocument_UnconfiguredStore_Returns501(t *testing.T) {
	kb := mockKnowledgeBase("my-kb", kbNS)
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()

	// DocStore intentionally nil — OBJECT_STORE_ADDR not set.
	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		DocStore:      nil,
	})

	code, body := postUpload(t, s, "my-kb", kbNS, "doc.txt", "text/plain", []byte("hello"))
	assert.Equal(t, http.StatusNotImplemented, code, "expected 501 when store is unconfigured; body: %s", string(body))
}

func TestUploadKBDocument_MissingFilename_Returns400(t *testing.T) {
	docStore := objectstore.NewMemObjectStore()
	kb := mockKnowledgeBase("my-kb", kbNS)
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()

	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		DocStore:      docStore,
	})

	// No ?filename in the URL, no Content-Disposition.
	req := httptest.NewRequest(http.MethodPost, "/api/knowledgebases/my-kb/documents?namespace="+kbNS,
		strings.NewReader("content"))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 for missing filename; body: %s", rec.Body.String())
}

func TestUploadKBDocument_ContentDispositionFilename(t *testing.T) {
	docStore := objectstore.NewMemObjectStore()
	kb := mockKnowledgeBase("my-kb", kbNS)
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()

	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		DocStore:      docStore,
	})

	// No ?filename but Content-Disposition carries the filename.
	req := httptest.NewRequest(http.MethodPost, "/api/knowledgebases/my-kb/documents?namespace="+kbNS,
		strings.NewReader("content"))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Disposition", `attachment; filename="via-header.txt"`)
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "expected 201; body: %s", rec.Body.String())

	var resp DocumentUploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "via-header.txt", resp.DocumentRef)
}

// TestUploadKBDocument_CrossNamespace verifies the upload works for a KB in a
// non-default namespace (exercises the ns param of postUpload and the name param
// of mockKnowledgeBase with different values so the helpers are not flagged
// as always receiving the same arguments).
func TestUploadKBDocument_CrossNamespace(t *testing.T) {
	docStore := objectstore.NewMemObjectStore()
	altNS := "alt-namespace"
	kb := mockKnowledgeBase("alt-kb", altNS)
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()

	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
		DocStore:      docStore,
	})

	content := []byte("cross-namespace content")
	code, body := postUpload(t, s, "alt-kb", altNS, "report.pdf", "application/pdf", content)
	require.Equal(t, http.StatusCreated, code, "expected 201; body: %s", string(body))

	var resp DocumentUploadResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Equal(t, objectstore.KnowledgeKey(altNS, "alt-kb", "report.pdf"), resp.Key)
}

// --- ResolveKBSources -------------------------------------------------------

func TestResolveKBSources_Upload(t *testing.T) {
	store := objectstore.NewMemObjectStore()
	ctx := context.Background()

	// Pre-populate two documents in the upload namespace.
	_ = store.Put(ctx, objectstore.KnowledgeKey("ns", "kb", "a.txt"), strings.NewReader("a"), 1, "text/plain")
	_ = store.Put(ctx, objectstore.KnowledgeKey("ns", "kb", "b.txt"), strings.NewReader("b"), 1, "text/plain")
	// Extra document in a different KB — must NOT appear.
	_ = store.Put(ctx, objectstore.KnowledgeKey("ns", "other-kb", "c.txt"), strings.NewReader("c"), 1, "text/plain")

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb", Namespace: "ns"},
		Spec:       agentsv1beta1.KnowledgeBaseSpec{Source: agentsv1beta1.KnowledgeBaseSource{Type: "upload"}},
	}

	items, err := ResolveKBSources(ctx, store, "ns", kb)
	require.NoError(t, err)
	require.Len(t, items, 2)

	keys := make(map[string]bool)
	for _, it := range items {
		keys[it.Key] = true
	}
	assert.True(t, keys[objectstore.KnowledgeKey("ns", "kb", "a.txt")])
	assert.True(t, keys[objectstore.KnowledgeKey("ns", "kb", "b.txt")])
}

func TestResolveKBSources_ObjectStorePrefix(t *testing.T) {
	store := objectstore.NewMemObjectStore()
	ctx := context.Background()

	// Pre-populate at a BYO prefix.
	_ = store.Put(ctx, "raw-data/docs/report.pdf", strings.NewReader("pdf"), 3, "application/pdf")
	_ = store.Put(ctx, "raw-data/docs/notes.txt", strings.NewReader("notes"), 5, "text/plain")

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb", Namespace: "ns"},
		Spec: agentsv1beta1.KnowledgeBaseSpec{
			Source: agentsv1beta1.KnowledgeBaseSource{
				Type:              "objectStorePrefix",
				ObjectStorePrefix: "raw-data/docs/",
			},
		},
	}

	items, err := ResolveKBSources(ctx, store, "ns", kb)
	require.NoError(t, err)
	require.Len(t, items, 2)
}

func TestResolveKBSources_NilStore_ReturnsError(t *testing.T) {
	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb"},
		Spec:       agentsv1beta1.KnowledgeBaseSpec{Source: agentsv1beta1.KnowledgeBaseSource{Type: "upload"}},
	}
	_, err := ResolveKBSources(context.Background(), nil, "ns", kb)
	require.Error(t, err)
}

func TestResolveKBSources_UnsupportedType_ReturnsError(t *testing.T) {
	store := objectstore.NewMemObjectStore()
	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb"},
		Spec:       agentsv1beta1.KnowledgeBaseSpec{Source: agentsv1beta1.KnowledgeBaseSource{Type: "url"}},
	}
	_, err := ResolveKBSources(context.Background(), store, "ns", kb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "url")
}

func TestResolveKBSources_ObjectStorePrefixEmpty_ReturnsError(t *testing.T) {
	store := objectstore.NewMemObjectStore()
	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb"},
		Spec: agentsv1beta1.KnowledgeBaseSpec{
			Source: agentsv1beta1.KnowledgeBaseSource{
				Type:              "objectStorePrefix",
				ObjectStorePrefix: "", // deliberately empty
			},
		},
	}
	_, err := ResolveKBSources(context.Background(), store, "ns", kb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// --- ingest trigger endpoint (m68.6) ----------------------------------------

// postIngest sends POST /api/knowledgebases/{name}/ingest and returns (status, body).
//
//nolint:unparam // ns is part of the helper's contract; all current callers happen to use kbNS.
func postIngest(t *testing.T, s *Server, kbName, ns string) (int, []byte) {
	t.Helper()
	rawURL := "/api/knowledgebases/" + kbName + "/ingest"
	if ns != "" {
		rawURL += "?namespace=" + ns
	}
	req := httptest.NewRequest(http.MethodPost, rawURL, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// newIngestEndpointServer builds a Server with the KB present + the full ingestion pipeline wired (dev/in-process
// mode: runWorkerDispatch defaults false, so the created run executes in-process synchronously via a goroutine).
func newIngestEndpointServer(t *testing.T, kb *agentsv1beta1.KnowledgeBase) (*Server, *objectstore.MemObjectStore) {
	t.Helper()
	sc := testScheme(t)
	builder := fake.NewClientBuilder().WithScheme(sc)
	if kb != nil {
		builder = builder.WithObjects(kb)
	}
	fc := builder.Build()
	docStore := objectstore.NewMemObjectStore()
	s := NewServer(Options{
		CallerClients:  newFakeFactory(fc),
		Scheme:         sc,
		Auth:           AllowAll{},
		Log:            logr.Discard(),
		DocStore:       docStore,
		KnowledgeStore: knowledge.NewMemStore(),
		Embedder:       newMockEmbedder(),
	})
	return s, docStore
}

func TestIngestKB_HappyPath_CreatesRun(t *testing.T) {
	kb := mockKnowledgeBase("my-kb", kbNS)
	kb.Spec.EmbeddingRoute = "embed-v1"
	s, docStore := newIngestEndpointServer(t, kb)

	// Seed a document under the KB's upload prefix so ResolveKBSources finds it.
	key := objectstore.KnowledgeKey(kbNS, "my-kb", "guide.md")
	require.NoError(t, docStore.Put(context.Background(), key, bytes.NewReader([]byte("The quick brown fox jumps over the lazy dog, repeatedly.")), -1, "text/markdown"))

	code, body := postIngest(t, s, "my-kb", kbNS)
	require.Equal(t, http.StatusAccepted, code, "expected 202; body: %s", string(body))

	var resp IngestResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.NotEmpty(t, resp.RunID)
	assert.Equal(t, string(run.StatusQueued), resp.Status)
	assert.Equal(t, 1, resp.DocumentCount)

	// The run exists in the store as an ingestion job with the pinned spec.
	rn, err := s.runStore.Get(resp.RunID)
	require.NoError(t, err)
	assert.True(t, rn.IsIngestionJob())
	assert.Equal(t, "my-kb", rn.IngestionRef)
	var spec IngestionSpec
	require.NoError(t, json.Unmarshal([]byte(rn.IngestionSpec), &spec))
	assert.Equal(t, "embed-v1", spec.EmbeddingRoute)
	require.Len(t, spec.Documents, 1)
	assert.Equal(t, key, spec.Documents[0].Key)
}

func TestIngestKB_UnknownKB_Returns404(t *testing.T) {
	s, _ := newIngestEndpointServer(t, nil) // no KB
	code, body := postIngest(t, s, "ghost-kb", kbNS)
	assert.Equal(t, http.StatusNotFound, code, "expected 404; body: %s", string(body))
}

func TestIngestKB_Unwired_Returns501(t *testing.T) {
	kb := mockKnowledgeBase("my-kb", kbNS)
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()
	// No KnowledgeStore / Embedder / DocStore wired.
	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
	})
	code, body := postIngest(t, s, "my-kb", kbNS)
	assert.Equal(t, http.StatusNotImplemented, code, "expected 501 when ingestion is unwired; body: %s", string(body))
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests for read + test-query endpoints (m68.13)
// ─────────────────────────────────────────────────────────────────────────────

// listKBs sends GET /api/knowledgebases and returns (status, body).
func listKBs(t *testing.T, s *Server, ns string) (int, []byte) {
	t.Helper()
	rawURL := "/api/knowledgebases"
	if ns != "" {
		rawURL += "?namespace=" + ns
	}
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// getKB sends GET /api/knowledgebases/{name} and returns (status, body).
func getKB(t *testing.T, s *Server, name, ns string) (int, []byte) {
	t.Helper()
	rawURL := "/api/knowledgebases/" + name
	if ns != "" {
		rawURL += "?namespace=" + ns
	}
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// searchKB sends POST /api/knowledgebases/{name}/search and returns (status, body).
//
//nolint:unparam // ns is part of the helper's contract; all current callers happen to use kbNS.
func searchKB(t *testing.T, s *Server, name, ns string, reqBody []byte) (int, []byte) {
	t.Helper()
	rawURL := "/api/knowledgebases/" + name + "/search"
	if ns != "" {
		rawURL += "?namespace=" + ns
	}
	req := httptest.NewRequest(http.MethodPost, rawURL, bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestListKBs_ReturnsSummaries(t *testing.T) {
	kb1 := mockKnowledgeBase("kb-alpha", kbNS)
	kb1.Status.Phase = "Ready"
	kb1.Status.ChunkCount = 42
	kb2 := mockKnowledgeBase("kb-beta", kbNS)
	kb2.Status.Phase = "Pending"

	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).
		WithStatusSubresource(&agentsv1beta1.KnowledgeBase{}).
		WithObjects(kb1, kb2).Build()

	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
	})

	code, body := listKBs(t, s, kbNS)
	require.Equal(t, http.StatusOK, code, "expected 200; body: %s", string(body))

	var resp KBListResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Len(t, resp.Items, 2, "expected 2 KBs")

	names := map[string]bool{}
	for _, item := range resp.Items {
		names[item.Name] = true
	}
	assert.True(t, names["kb-alpha"])
	assert.True(t, names["kb-beta"])
}

func TestGetKB_ReturnsDetail(t *testing.T) {
	kb := mockKnowledgeBase("detail-kb", kbNS)
	kb.Spec.DisplayName = "My Detail KB"
	kb.Spec.Chunking = agentsv1beta1.ChunkingConfig{Size: 256, Overlap: 32, Splitter: "markdown"}
	kb.Status.Phase = "Ready"
	kb.Status.ChunkCount = 10
	kb.Status.DocumentCount = 2

	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).
		WithStatusSubresource(&agentsv1beta1.KnowledgeBase{}).
		WithObjects(kb).Build()

	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
	})

	code, body := getKB(t, s, "detail-kb", kbNS)
	require.Equal(t, http.StatusOK, code, "expected 200; body: %s", string(body))

	var resp KBDetail
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Equal(t, "detail-kb", resp.Name)
	assert.Equal(t, "My Detail KB", resp.DisplayName)
	assert.Equal(t, "Ready", resp.Phase)
	assert.EqualValues(t, 10, resp.ChunkCount)
	assert.EqualValues(t, 2, resp.DocumentCount)
	assert.Equal(t, "markdown", resp.ChunkSplitter)
	assert.Equal(t, 256, resp.ChunkSize)
	assert.Equal(t, 32, resp.ChunkOverlap)
}

func TestGetKB_AbsentKB_Returns404(t *testing.T) {
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).Build() // no KBs

	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
	})

	code, body := getKB(t, s, "ghost-kb", kbNS)
	assert.Equal(t, http.StatusNotFound, code, "expected 404 for absent KB; body: %s", string(body))
}

func TestSearchKB_ForwardsToTokenServiceAndReturnsResults(t *testing.T) {
	// Start a fake token-service that returns one search hit.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/knowledge/search" || r.Method != http.MethodPost {
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		// Verify the forwarded body includes the embeddingModel from the KB spec.
		var fwdReq struct {
			EmbeddingModel string `json:"embeddingModel"`
			Query          string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&fwdReq); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if fwdReq.EmbeddingModel != "text-embedding-route" {
			http.Error(w, "wrong embeddingModel: "+fwdReq.EmbeddingModel, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"results": []map[string]any{
				{
					"content":     "chunk content",
					"documentRef": "guide.pdf",
					"chunkIndex":  3,
					"score":       0.87,
				},
			},
		})
	}))
	defer ts.Close()

	kb := mockKnowledgeBase("search-kb", kbNS)
	kb.Spec.EmbeddingRoute = "text-embedding-route"

	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()

	s := NewServer(Options{
		CallerClients:   newFakeFactory(fc),
		Scheme:          sc,
		Auth:            AllowAll{},
		Log:             logr.Discard(),
		TokenServiceURL: ts.URL,
	})

	reqBody, _ := json.Marshal(map[string]any{"query": "how to configure", "topK": 5})
	code, body := searchKB(t, s, "search-kb", kbNS, reqBody)
	require.Equal(t, http.StatusOK, code, "expected 200; body: %s", string(body))

	var resp kbSearchResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Len(t, resp.Results, 1, "expected 1 search result")
	assert.Equal(t, "chunk content", resp.Results[0].Content)
	assert.Equal(t, "guide.pdf", resp.Results[0].DocumentRef)
	assert.Equal(t, 3, resp.Results[0].ChunkIndex)
	assert.InDelta(t, 0.87, resp.Results[0].Score, 0.001)
}

func TestSearchKB_Unconfigured_Returns501(t *testing.T) {
	kb := mockKnowledgeBase("search-kb", kbNS)
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()

	// TokenServiceURL deliberately empty — not configured.
	s := NewServer(Options{
		CallerClients: newFakeFactory(fc),
		Scheme:        sc,
		Auth:          AllowAll{},
		Log:           logr.Discard(),
	})

	reqBody, _ := json.Marshal(map[string]any{"query": "test"})
	code, body := searchKB(t, s, "search-kb", kbNS, reqBody)
	assert.Equal(t, http.StatusNotImplemented, code, "expected 501 when token-service is unconfigured; body: %s", string(body))
}

func TestSearchKB_AbsentKB_Returns404(t *testing.T) {
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).Build() // no KBs

	s := NewServer(Options{
		CallerClients:   newFakeFactory(fc),
		Scheme:          sc,
		Auth:            AllowAll{},
		Log:             logr.Discard(),
		TokenServiceURL: "http://token-service:8443",
	})

	reqBody, _ := json.Marshal(map[string]any{"query": "test"})
	code, body := searchKB(t, s, "ghost-kb", kbNS, reqBody)
	assert.Equal(t, http.StatusNotFound, code, "expected 404 for absent KB; body: %s", string(body))
}
