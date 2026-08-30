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
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/objectstore"
	"github.com/ctxmesh/agentry/internal/run"
)

type fakeRunStore struct {
	created []*run.Run
}

func (f *fakeRunStore) Create(rn *run.Run) error {
	f.created = append(f.created, rn)
	return nil
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
