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

package bff

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge"
	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// TestKnowledgeBaseWritesFollowRealRBAC: against a real API server and the shipped personas, a viewer
// of a KnowledgeBase can neither add a document to it nor start its ingestion, and creates no run,
// while a developer does both.
func TestKnowledgeBaseWritesFollowRealRBAC(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	const ns = "kb-write-rbac"
	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	require.NoError(t, admin.Create(ctx, mockKnowledgeBase("handbook", ns)))
	viewerRole := installPersona(t, admin, "ctxmesh_viewer_role.yaml")
	developerRole := installPersona(t, admin, "ctxmesh_developer_role.yaml")

	serve := func(caller client.Client) (*Server, run.Store) {
		docStore := objectstore.NewMemObjectStore()
		require.NoError(t, docStore.Put(ctx, objectstore.KnowledgeKey(ns, "handbook", "guide.md"),
			bytes.NewReader([]byte("the handbook says hello")), -1, "text/markdown"))
		runs := run.NewMemStore()
		return NewServer(Options{
			CallerClients:  newFakeFactory(caller),
			Scheme:         testScheme(t),
			Auth:           AllowAll{},
			Log:            logr.Discard(),
			DocStore:       docStore,
			KnowledgeStore: knowledge.NewMemStore(),
			Embedder:       newMockEmbedder(),
			RunStore:       runs,
		}), runs
	}
	ingestions := func(t *testing.T, runs run.Store) string {
		t.Helper()
		active, err := runs.ActiveIngestion(ns, "handbook")
		require.NoError(t, err)
		return active
	}

	t.Run("viewer", func(t *testing.T) {
		s, runs := serve(personaUser(t, admin, ns, "viewer-"+ns, viewerRole))
		code, body := postUpload(t, s, "handbook", ns, "notes.md", "text/markdown", []byte("viewer text"))
		assert.Equal(t, http.StatusForbidden, code, "upload: %s", body)
		code, body = postIngest(t, s, "handbook", ns)
		assert.Equal(t, http.StatusForbidden, code, "ingest: %s", body)
		assert.Empty(t, ingestions(t, runs), "the refused ingest created no run")
	})

	t.Run("developer", func(t *testing.T) {
		s, runs := serve(personaUser(t, admin, ns, "developer-"+ns, developerRole))
		code, body := postUpload(t, s, "handbook", ns, "notes.md", "text/markdown", []byte("developer text"))
		assert.Equal(t, http.StatusCreated, code, "upload: %s", body)
		code, body = postIngest(t, s, "handbook", ns)
		require.Equal(t, http.StatusAccepted, code, "ingest: %s", body)
		assert.NotEmpty(t, ingestions(t, runs))
	})
}
