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

package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge"
)

// fakeIngestionRunCreator records CreateIngestionRun calls for the scheduled-re-ingest test (M140.4).
type fakeIngestionRunCreator struct {
	mu    sync.Mutex
	Calls int
	RunID string
	Err   error
}

func (f *fakeIngestionRunCreator) CreateIngestionRun(_ context.Context, _ *agentsv1beta1.KnowledgeBase) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	return f.RunID, f.Err
}

// TestKnowledgeBase_ScheduledReingest_CreatesRunWhenDue drives the full Reconcile (envtest) with a fake creator
// + fake corpus store: a due KB creates exactly one run + goes Ingesting, and no second run is created while in
// flight. Per Fable, the test boundary is CREATION (not execution — a controller-created run needs the worker).
func TestKnowledgeBase_ScheduledReingest_CreatesRunWhenDue(t *testing.T) {
	ns := "default"
	spec := validKBSpec()
	spec.RefreshInterval = &metav1.Duration{Duration: time.Minute}
	mkKnowledgeBase(t, "kb-sched", ns, spec)

	creator := &fakeIngestionRunCreator{RunID: "run-sched-1"}
	past := time.Now().Add(-2 * time.Minute)
	corpus := &fakeCorpusStore{
		GetStatusFound: true,
		GetStatusResult: knowledge.CorpusStatus{
			Phase: "Ready", LastIngestedAt: &past, DocumentCount: 1, ChunkCount: 3,
		},
	}
	r := &KnowledgeBaseReconciler{Client: k8sClient, Knowledge: corpus, IngestionRunCreator: creator}

	// Drive reconciles until the scheduled refresh fires (validation + finalizer may take a pass or two). The
	// loop stops the instant a run is created, so no double-fire while the corpus still reports Ready.
	for i := 0; i < 5 && creator.Calls == 0; i++ {
		reconcileKB(t, r, "kb-sched", ns)
	}
	require.Equal(t, 1, creator.Calls, "a due refresh must create exactly one ingestion run")

	var got agentsv1beta1.KnowledgeBase
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "kb-sched", Namespace: ns}, &got))
	require.Equal(t, kbPhaseIngesting, got.Status.Phase, "the KB is stamped Ingesting after the scheduled create")
	require.Equal(t, "run-sched-1", got.Status.IngestionRunRef)
	require.NotNil(t, got.Status.LastScheduledIngestAt, "the attempt timestamp is stamped for backoff")

	// The run is now in flight — the executor hasn't written a terminal corpus-status row yet (found=false).
	// The reconcile must take the in-flight poll path and NOT create a second run.
	corpus.GetStatusFound = false
	reconcileKB(t, r, "kb-sched", ns)
	require.Equal(t, 1, creator.Calls, "no second ingestion run while one is in flight")
}
