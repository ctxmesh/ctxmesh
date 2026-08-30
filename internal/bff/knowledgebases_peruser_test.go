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

// Per-user KnowledgeBase flows (m80.4, ADR 0061 Fork 3): upload nests the caller's server-derived subject
// hash in the object key; ingest recovers it per-document into the pinned IngestionSpec; the executor stamps
// it on the chunks; retrieval scopes the search subject to the invoking/caller user. The load-bearing
// invariant proven here: the subject stamped at ingest EQUALS the subject the search path scopes by (both
// are userGrantHash of the SAME caller identity), so a user retrieves exactly their own chunks — and the
// !perUser path is byte-for-byte unchanged (subject "", org-wide key, no per-user scoping).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/controlplane/knowledge"
	"github.com/ctxmesh/agentry/internal/objectstore"
	"github.com/ctxmesh/agentry/internal/run"
)

// perUserKB returns an upload-source KnowledgeBase (named "my-kb" in the kbNS namespace) with perUser enabled.
func perUserKB() *agentsv1beta1.KnowledgeBase {
	kb := mockKnowledgeBase("my-kb", kbNS)
	kb.Spec.PerUser = true
	return kb
}

// serverWithIdentity builds a BFF Server whose caller-scoped client resolves the given username via the
// SelfSubjectReview interceptor (so callerUsername → userGrantHash works), with the full ingestion pipeline
// wired for in-process execution.
func serverWithIdentity(t *testing.T, username string, kb *agentsv1beta1.KnowledgeBase) (*Server, *objectstore.MemObjectStore, knowledge.Store) {
	t.Helper()
	sc := testScheme(t)
	builder := fake.NewClientBuilder().WithScheme(sc)
	if kb != nil {
		builder = builder.WithObjects(kb)
	}
	if username != "" {
		builder = builder.WithInterceptorFuncs(ssrInterceptor(username, nil))
	}
	fc := builder.Build()
	docStore := objectstore.NewMemObjectStore()
	ks := knowledge.NewMemStore()
	s := NewServer(Options{
		CallerClients:  newFakeFactory(fc),
		Scheme:         sc,
		Auth:           AllowAll{},
		Log:            logr.Discard(),
		DocStore:       docStore,
		KnowledgeStore: ks,
		Embedder:       newMockEmbedder(),
	})
	return s, docStore, ks
}

// ── Upload: per-user key nesting ────────────────────────────────────────────────────────────────────

// A per-user KB upload nests the caller's userGrantHash as a key path segment (KnowledgeKeyForSubject), so
// the off-request executor can recover the owner — and two users' same-named files never collide.
func TestUploadKBDocument_PerUser_NestsSubjectInKey(t *testing.T) {
	const username = "alice@example.com"
	kb := perUserKB()
	s, docStore, _ := serverWithIdentity(t, username, kb)

	code, body := postUpload(t, s, "my-kb", kbNS, "notes.md", "text/markdown", []byte("alice private note"))
	require.Equal(t, http.StatusCreated, code, "expected 201; body: %s", string(body))

	var resp DocumentUploadResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	wantKey := objectstore.KnowledgeKeyForSubject(kbNS, "my-kb", userGrantHash(username), "notes.md")
	assert.Equal(t, wantKey, resp.Key, "a per-user upload must nest the caller's subject hash in the key")
	assert.Contains(t, resp.Key, userGrantHash(username), "the key must carry the caller's un-forgeable subject hash")

	// The object is actually stored under that per-user key.
	_, err := docStore.Get(context.Background(), wantKey)
	require.NoError(t, err, "the object must be stored under the per-user key")
}

// An ORG-WIDE KB upload is byte-for-byte unchanged: the flat key, no subject segment (proves !perUser).
func TestUploadKBDocument_OrgWide_KeyUnchanged(t *testing.T) {
	kb := mockKnowledgeBase("my-kb", kbNS) // perUser=false
	s, _, _ := serverWithIdentity(t, "alice@example.com", kb)

	code, body := postUpload(t, s, "my-kb", kbNS, "guide.md", "text/markdown", []byte("shared org doc"))
	require.Equal(t, http.StatusCreated, code, "body: %s", string(body))
	var resp DocumentUploadResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Equal(t, objectstore.KnowledgeKey(kbNS, "my-kb", "guide.md"), resp.Key,
		"an org-wide upload must use the flat key (no subject segment) — byte-for-byte unchanged")
}

// A per-user upload with NO resolvable caller identity is refused (fail-closed) — never stamped under a
// wrong/empty subject.
func TestUploadKBDocument_PerUser_NoIdentityRefused(t *testing.T) {
	kb := perUserKB()
	// No SSR interceptor ⇒ callerUsername returns empty username → error.
	s, _, _ := serverWithIdentity(t, "", kb)
	code, body := postUpload(t, s, "my-kb", kbNS, "notes.md", "text/markdown", []byte("x"))
	assert.Equal(t, http.StatusForbidden, code,
		"a per-user upload with no resolvable identity must be refused; body: %s", string(body))
}

// ── Ingest + executor: per-user subject threading & stamping ────────────────────────────────────────

// The end-to-end per-user proof: alice and bob each upload to a perUser KB; a full ingest run stamps each
// user's chunks with THEIR OWN subject (userGrantHash), and a subject-scoped search returns only that user's
// chunk. This is the load-bearing invariant — ingested subject == retrievable subject (same function).
func TestIngestKB_PerUser_StampsAndScopesBySubject(t *testing.T) {
	kb := perUserKB()
	kb.Spec.EmbeddingRoute = "embed-v1"

	// Two distinct users upload. We reuse ONE server per user so each upload derives that user's subject.
	aliceHash := userGrantHash("alice@example.com")
	bobHash := userGrantHash("bob@example.com")
	require.NotEqual(t, aliceHash, bobHash, "distinct users must hash to distinct subjects")

	sAlice, docStore, ks := serverWithIdentity(t, "alice@example.com", kb)
	code, body := postUpload(t, sAlice, "my-kb", kbNS, "shared-name.md", "text/markdown",
		[]byte("alice's confidential quarterly numbers"))
	require.Equal(t, http.StatusCreated, code, "body: %s", string(body))

	// bob uploads a same-named file — a per-user KB must NOT collide (distinct key subtrees).
	sBob := serverWithSharedStores(t, "bob@example.com", kb, docStore, ks)
	code, body = postUpload(t, sBob, "my-kb", kbNS, "shared-name.md", "text/markdown",
		[]byte("bob's confidential quarterly numbers"))
	require.Equal(t, http.StatusCreated, code, "body: %s", string(body))

	// Both objects coexist (no overwrite) under their per-user subtrees.
	infos, err := docStore.List(context.Background(), objectstore.KnowledgePrefix(kbNS, "my-kb"))
	require.NoError(t, err)
	assert.Len(t, infos, 2, "two users' same-named files must not collide in a per-user KB")

	// Ingest the whole corpus (in-process). Use alice's server (any caller can trigger; per-doc subjects come
	// from the keys, not the trigger's identity).
	code, body = postIngest(t, sAlice, "my-kb", kbNS)
	require.Equal(t, http.StatusAccepted, code, "body: %s", string(body))
	var resp IngestResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	waitForRun(t, sAlice, resp.RunID)

	// The pinned IngestionSpec carries a per-document subject recovered from each key.
	rn, err := sAlice.runStore.Get(resp.RunID)
	require.NoError(t, err)
	var spec IngestionSpec
	require.NoError(t, json.Unmarshal([]byte(rn.IngestionSpec), &spec))
	require.Len(t, spec.Documents, 2)
	subjectsSeen := map[string]bool{}
	for _, d := range spec.Documents {
		require.NotEmpty(t, d.Subject, "a per-user document must carry its owner's subject in the spec")
		subjectsSeen[d.Subject] = true
	}
	assert.True(t, subjectsSeen[aliceHash], "alice's document subject must be pinned")
	assert.True(t, subjectsSeen[bobHash], "bob's document subject must be pinned")

	// The store now holds each user's chunks stamped with THEIR subject. A search scoped to alice's subject
	// returns only alice's chunk; a search scoped to bob's returns only bob's.
	ctx := context.Background()
	vec := queryVec(t, sAlice, "embed-v1", "quarterly numbers")

	aliceHits, err := ks.Search(ctx, knowledge.SearchQuery{
		Namespace: kbNS, KnowledgeBase: "my-kb", Subject: aliceHash, EmbeddingModel: "embed-v1", Vector: vec, TopK: 10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, aliceHits, "alice must retrieve her own chunk")
	for _, h := range aliceHits {
		assert.Equal(t, aliceHash, h.Chunk.Subject, "a subject-scoped search must return only that subject's chunks")
		assert.Contains(t, h.Chunk.Content, "alice", "alice's search must not surface bob's content")
	}

	// The org-wide subject "" must retrieve NOTHING in a per-user corpus (every chunk is stamped u-...).
	orgHits, err := ks.Search(ctx, knowledge.SearchQuery{
		Namespace: kbNS, KnowledgeBase: "my-kb", Subject: "", EmbeddingModel: "embed-v1", Vector: vec, TopK: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, orgHits, "an org-wide (subject \"\") search must find nothing in a per-user corpus")
}

// An ORG-WIDE ingest is unchanged: chunks are stamped Subject "" (proves !perUser byte-for-byte).
func TestIngestKB_OrgWide_StampsEmptySubject(t *testing.T) {
	kb := mockKnowledgeBase("my-kb", kbNS) // perUser=false
	kb.Spec.EmbeddingRoute = "embed-v1"
	s, docStore, ks := serverWithIdentity(t, "alice@example.com", kb)

	key := objectstore.KnowledgeKey(kbNS, "my-kb", "guide.md")
	require.NoError(t, docStore.Put(context.Background(), key,
		bytes.NewReader([]byte("shared org knowledge base content")), -1, "text/markdown"))

	code, body := postIngest(t, s, "my-kb", kbNS)
	require.Equal(t, http.StatusAccepted, code, "body: %s", string(body))
	var resp IngestResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	waitForRun(t, s, resp.RunID)

	// The pinned spec doc carries no subject; the chunks are stamped Subject "".
	rn, err := s.runStore.Get(resp.RunID)
	require.NoError(t, err)
	var spec IngestionSpec
	require.NoError(t, json.Unmarshal([]byte(rn.IngestionSpec), &spec))
	require.Len(t, spec.Documents, 1)
	assert.Equal(t, "", spec.Documents[0].Subject, "an org-wide document must pin an empty subject")

	vec := queryVec(t, s, "embed-v1", "org knowledge")
	hits, err := ks.Search(context.Background(), knowledge.SearchQuery{
		Namespace: kbNS, KnowledgeBase: "my-kb", Subject: "", EmbeddingModel: "embed-v1", Vector: vec, TopK: 10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, hits, "an org-wide corpus is retrieved under subject \"\"")
	for _, h := range hits {
		assert.Equal(t, "", h.Chunk.Subject, "an org-wide corpus must stamp Subject \"\" (unchanged)")
	}
}

// ── Console test-query: per-user scoping ────────────────────────────────────────────────────────────

// The console test-query on a per-user KB scopes to the CALLER'S subject: the BFF forwards the caller's
// userGrantHash as the subject to the token-service.
func TestSearchKB_PerUser_ScopesToCallerSubject(t *testing.T) {
	const username = "alice@example.com"
	kb := perUserKB()
	kb.Spec.EmbeddingRoute = "embed-v1"

	// A fake token-service that records the forwarded subject.
	gotSubject := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		subj, _ := body["subject"].(string)
		gotSubject <- subj
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(ts.Close)

	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).
		WithInterceptorFuncs(ssrInterceptor(username, nil)).Build()
	s := NewServer(Options{
		CallerClients:   newFakeFactory(fc),
		Scheme:          sc,
		Auth:            AllowAll{},
		Log:             logr.Discard(),
		TokenServiceURL: ts.URL,
	})

	code, body := searchKB(t, s, "my-kb", kbNS, []byte(`{"query":"my notes"}`))
	require.Equal(t, http.StatusOK, code, "body: %s", string(body))
	select {
	case subj := <-gotSubject:
		assert.Equal(t, userGrantHash(username), subj,
			"the console test-query on a per-user KB must scope to the caller's own subject hash")
	default:
		t.Fatal("token-service was not called")
	}
}

// The console test-query on an ORG-WIDE KB forwards subject "" (unchanged / no identity dependency).
func TestSearchKB_OrgWide_SubjectEmpty(t *testing.T) {
	kb := mockKnowledgeBase("my-kb", kbNS) // perUser=false
	kb.Spec.EmbeddingRoute = "embed-v1"

	gotSubject := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		subj, _ := body["subject"].(string)
		gotSubject <- subj
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(ts.Close)

	sc := testScheme(t)
	// No SSR interceptor needed — an org-wide search never resolves a caller identity.
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).Build()
	s := NewServer(Options{
		CallerClients:   newFakeFactory(fc),
		Scheme:          sc,
		Auth:            AllowAll{},
		Log:             logr.Discard(),
		TokenServiceURL: ts.URL,
	})

	code, body := searchKB(t, s, "my-kb", kbNS, []byte(`{"query":"anything"}`))
	require.Equal(t, http.StatusOK, code, "body: %s", string(body))
	select {
	case subj := <-gotSubject:
		assert.Equal(t, "", subj, "an org-wide KB test-query must forward subject \"\" (unchanged)")
	default:
		t.Fatal("token-service was not called")
	}
}

// ── SubjectFromKey unit coverage ────────────────────────────────────────────────────────────────────

// SubjectFromKey recovers the per-user subject and is the exact inverse the ingest-create path relies on.
func TestSubjectFromKey_RoundTrip(t *testing.T) {
	subject := userGrantHash("carol@example.com")
	perUserKey := objectstore.KnowledgeKeyForSubject("ns", "kb", subject, "doc.md")
	assert.Equal(t, subject, objectstore.SubjectFromKey("ns", "kb", perUserKey),
		"SubjectFromKey must recover the subject a per-user key nests")

	orgKey := objectstore.KnowledgeKey("ns", "kb", "doc.md")
	assert.Equal(t, "", objectstore.SubjectFromKey("ns", "kb", orgKey),
		"an org-wide key carries no subject")

	assert.Equal(t, "", objectstore.SubjectFromKey("ns", "kb", "unrelated/key"),
		"a key outside the KB prefix recovers no subject")
}

// ── helpers ─────────────────────────────────────────────────────────────────────────────────────────

// serverWithSharedStores builds a second BFF Server for a DIFFERENT user that SHARES the given doc/knowledge
// stores (so two users' uploads land in one store the way they would in production).
func serverWithSharedStores(t *testing.T, username string, kb *agentsv1beta1.KnowledgeBase,
	docStore *objectstore.MemObjectStore, ks knowledge.Store,
) *Server {
	t.Helper()
	sc := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sc).WithObjects(kb).
		WithInterceptorFuncs(ssrInterceptor(username, nil)).Build()
	return NewServer(Options{
		CallerClients:  newFakeFactory(fc),
		Scheme:         sc,
		Auth:           AllowAll{},
		Log:            logr.Discard(),
		DocStore:       docStore,
		KnowledgeStore: ks,
		Embedder:       newMockEmbedder(),
	})
}

// waitForRun blocks until the in-process ingestion run reaches a terminal state (the dev path runs the
// executor in a goroutine). It fails the test on timeout.
func waitForRun(t *testing.T, s *Server, runID string) {
	t.Helper()
	for range 200 {
		rn, err := s.runStore.Get(runID)
		require.NoError(t, err)
		if rn.Status.IsTerminal() {
			require.Equalf(t, run.StatusSucceeded, rn.Status, "ingestion run must succeed (err=%q)", rn.Error)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ingestion run %s did not reach a terminal state", runID)
}

// queryVec embeds a query string with the mock embedder wired into the server (so the test's query vector
// matches the direction the mock produced for the stored chunks).
func queryVec(t *testing.T, s *Server, model, query string) []float32 {
	t.Helper()
	vec, _, err := s.embedder.Embed(context.Background(), model, query)
	require.NoError(t, err)
	return vec
}
