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

package ingestion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

type fakeRunStore struct {
	created []*run.Run
	// active simulates a queued-or-running ingestion for the KB (M152 m152.3); activeErr
	// simulates a store that cannot answer, which the Creator must treat as fail-closed.
	active    string
	activeErr error
}

func (f *fakeRunStore) Create(rn *run.Run) error {
	f.created = append(f.created, rn)
	return nil
}

func (f *fakeRunStore) ActiveIngestion(_, _ string) (string, error) {
	return f.active, f.activeErr
}

// newTestCreator builds a Creator over a mem object store holding one document, plus the KB
// that names it — the minimum a CreateIngestionRun call needs to get past source resolution
// and reach the guards under test.
func newTestCreator(t *testing.T, rs *fakeRunStore) (*Creator, *agentsv1beta1.KnowledgeBase) {
	t.Helper()
	ns, kbName := "team-a", "docs-kb"
	store := objectstore.NewMemObjectStore()
	prefix := objectstore.KnowledgePrefix(ns, kbName)
	require.NoError(t, store.Put(context.Background(), prefix+"a.md",
		bytes.NewReader([]byte("aaa")), 3, "text/markdown"))
	return &Creator{DocStore: store, RunStore: rs}, &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: kbName, Namespace: ns},
		Spec: agentsv1beta1.KnowledgeBaseSpec{
			Source:         agentsv1beta1.KnowledgeBaseSource{Type: SourceTypeUpload},
			EmbeddingRoute: "demo-embed",
			Chunking:       agentsv1beta1.ChunkingConfig{Size: 512, Overlap: 64, Splitter: "recursive"},
		},
	}
}

func TestCreator_CreateIngestionRun_PinsSpecAndCreatesRun(t *testing.T) {
	ctx := context.Background()
	ns, kbName := "team-a", "docs-kb"

	store := objectstore.NewMemObjectStore()
	prefix := objectstore.KnowledgePrefix(ns, kbName)
	require.NoError(t, store.Put(ctx, prefix+"a.md", bytes.NewReader([]byte("aaa")), 3, "text/markdown"))
	require.NoError(t, store.Put(ctx, prefix+"b.md", bytes.NewReader([]byte("bbb")), 3, "text/markdown"))

	rs := &fakeRunStore{}
	c := &Creator{DocStore: store, RunStore: rs}

	kb := &agentsv1beta1.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: kbName, Namespace: ns},
		Spec: agentsv1beta1.KnowledgeBaseSpec{
			Source:         agentsv1beta1.KnowledgeBaseSource{Type: SourceTypeUpload},
			EmbeddingRoute: "demo-embed",
			Chunking:       agentsv1beta1.ChunkingConfig{Size: 512, Overlap: 64, Splitter: "recursive"},
		},
	}

	runID, err := c.CreateIngestionRun(ctx, kb)
	require.NoError(t, err)
	require.NotEmpty(t, runID)
	require.Len(t, rs.created, 1, "exactly one ingestion run created")

	rn := rs.created[0]
	require.Equal(t, kbName, rn.IngestionRef)
	require.True(t, rn.IsIngestionJob(), "the run is dispatched as an ingestion job")

	var spec IngestionSpec
	require.NoError(t, json.Unmarshal([]byte(rn.IngestionSpec), &spec))
	require.Equal(t, ns, spec.Namespace)
	require.Equal(t, kbName, spec.KnowledgeBase)
	require.Equal(t, "demo-embed", spec.EmbeddingRoute)
	require.Len(t, spec.Documents, 2, "both source documents are pinned into the spec")
}

// M152 m152.3: the shared Creator refuses a second ingest, so the controller's SCHEDULED
// re-ingest is guarded too — not only the BFF handler. M148 put the empty-corpus guard in
// the handler alone and M149's re-audit found the scheduled path still creating
// zero-document runs on a timer; the same mistake twice would be a pattern.
func TestCreateIngestionRun_RefusesWhenOneIsAlreadyInFlight(t *testing.T) {
	rs := &fakeRunStore{active: "run-already-going"}
	c, kb := newTestCreator(t, rs)

	_, err := c.CreateIngestionRun(context.Background(), kb)
	if !errors.Is(err, ErrIngestionInFlight) {
		t.Fatalf("expected ErrIngestionInFlight, got %v", err)
	}
	if len(rs.created) != 0 {
		t.Fatalf("a refused ingest must create no run; got %d", len(rs.created))
	}
}

// A store that cannot answer must FAIL CLOSED. Admitting a second run on an unknown state is
// the worst option available: concurrent ingests destroy the corpus silently.
func TestCreateIngestionRun_FailsClosedWhenTheStoreCannotAnswer(t *testing.T) {
	rs := &fakeRunStore{activeErr: errors.New("store down")}
	c, kb := newTestCreator(t, rs)

	if _, err := c.CreateIngestionRun(context.Background(), kb); err == nil {
		t.Fatal("an unanswerable in-flight check must refuse, not admit")
	}
	if len(rs.created) != 0 {
		t.Fatalf("nothing may be created when the guard cannot run; got %d", len(rs.created))
	}
}
