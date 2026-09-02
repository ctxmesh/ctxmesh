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

// Package ingestion holds the ONE source of truth for creating a KnowledgeBase ingestion run: the pinned
// IngestionSpec, the source resolution, and the Creator. Both the BFF /ingest handler and the KB controller's
// scheduled re-ingest (M140.4) call the SAME Creator, so the two paths cannot drift. It is a neutral package
// (no BFF, no controller import) so the controller can create runs without importing the object store or the
// run package — the discipline the controller's read-only run seam already preserves.
package ingestion

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// KnowledgeBase source types (ADR 0061).
const (
	SourceTypeUpload            = "upload"
	SourceTypeObjectStorePrefix = "objectStorePrefix"
)

// IngestionDoc is one resolved source document pinned onto an ingestion run.
type IngestionDoc struct {
	Key         string `json:"key"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	// Subject is the per-user corpus owner's server-derived subject hash for a PER-USER KB (recovered from
	// the document's object key at ingest-create, ADR 0061 Fork 3), or "" for an org-wide corpus. The executor
	// stamps it onto every chunk this document produces so a user retrieves only their own chunks. Pinned here
	// at create so a live-edited KB / re-derivation cannot retroactively re-attribute an in-flight ingest.
	Subject string `json:"subject,omitempty"`
}

// IngestionSpec is the resolved ingestion parameters pinned onto the run (run.IngestionSpec JSON) at
// ingest-create. It snapshots everything the off-request executor needs — namespace, KB, embedding route,
// chunking, and the resolved document list — so a live-edited KnowledgeBase or a changed bucket cannot
// retroactively alter an in-flight ingestion (the ADR 0060 snapshot-pinning discipline).
type IngestionSpec struct {
	Namespace      string                       `json:"namespace"`
	KnowledgeBase  string                       `json:"knowledgeBase"`
	EmbeddingRoute string                       `json:"embeddingRoute"`
	Chunking       agentsv1beta1.ChunkingConfig `json:"chunking"`
	Documents      []IngestionDoc               `json:"documents"`
}

// ResolveKBSources enumerates the source documents of a KnowledgeBase from the durable object store. An
// "upload" KB reads the KB's own bucket prefix; an "objectStorePrefix" KB reads the configured prefix.
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
	case SourceTypeUpload:
		return store.List(ctx, objectstore.KnowledgePrefix(ns, kb.Name))
	case SourceTypeObjectStorePrefix:
		prefix := kb.Spec.Source.ObjectStorePrefix
		if prefix == "" {
			return nil, fmt.Errorf("KnowledgeBase %q has source.type=objectStorePrefix but source.objectStorePrefix is empty", kb.Name)
		}
		return store.List(ctx, prefix)
	default:
		return nil, fmt.Errorf("KnowledgeBase %q has unsupported source.type %q (supported: upload, objectStorePrefix)", kb.Name, kb.Spec.Source.Type)
	}
}

// RunStore is the narrow seam the Creator needs: create a run, and answer whether one is
// already in flight for a knowledge base.
//
// ActiveIngestion exists because SweepOrphans makes concurrent ingests MUTUALLY
// DESTRUCTIVE, not merely wasteful (M152 m152.3). It deletes a document's chunks whose
// ingestion_run_id differs from the current run — so with runs A and B on one KB: A upserts
// as A, B upserts as B, A sweeps everything ≠ A (taking B's rows), B sweeps everything ≠ B
// (taking A's). Both runs report success and the corpus is left holding whichever fragment
// lost the race last. Nothing errors, so nothing surfaces it.
type RunStore interface {
	Create(rn *run.Run) error
	// ActiveIngestion returns the id of a queued-or-running ingestion run for (namespace,
	// knowledgeBase), or "" when none is in flight. An implementation that cannot answer
	// returns an error — the caller FAILS CLOSED rather than admitting a second run, because
	// the damage is silent corpus loss.
	ActiveIngestion(namespace, knowledgeBase string) (string, error)
}

// Creator builds + creates ingestion runs — the ONE source of truth used by the BFF /ingest handler AND the
// KB controller's scheduled re-ingest (M140.4). It resolves the source, pins an IngestionSpec, and creates a
// queued Run; it NEVER touches KB.status (the caller owns that — the BFF via the caller's client, the
// controller via its own).
type Creator struct {
	DocStore objectstore.ObjectStore
	RunStore RunStore
}

// CreateIngestionRun resolves the KB's source, pins the IngestionSpec, and creates a queued ingestion Run,
// returning its id. For a per-user corpus each document's owner subject is recovered from its object key and
// pinned; a per-user document with no recoverable owner is fail-closed skipped (never misattributed).
// ErrIngestionInFlight reports that a KB already has a queued-or-running ingestion. The BFF
// translates it to 409; the controller's scheduled re-ingest treats it as "skip this tick",
// which is the correct behaviour for a timer that fires while the previous run is still going.
var ErrIngestionInFlight = errors.New("an ingestion is already in flight for this knowledge base")

// ErrEmptyCorpus reports that the resolved source contains no documents. Ingesting nothing is
// never what a caller meant, and a run that succeeds having read nothing makes the console
// show an ingested KB whose retrieval is silently empty (M148 m148.11, moved here by M152 so
// the SCHEDULED path is guarded too).
var ErrEmptyCorpus = errors.New("the knowledge base has no documents to ingest")

func (c *Creator) CreateIngestionRun(ctx context.Context, kb *agentsv1beta1.KnowledgeBase) (string, error) {
	ns := kb.Namespace
	infos, err := ResolveKBSources(ctx, c.DocStore, ns, kb)
	if err != nil {
		return "", fmt.Errorf("resolve KB sources: %w", err)
	}
	// One ingestion at a time per KB (M152 m152.3). Checked HERE rather than in the BFF
	// handler because this Creator is the ONE source of truth for both entry points — the
	// handler and the controller's scheduled re-ingest. M148 put the empty-corpus guard in
	// the handler alone, and M149's re-audit found the scheduled path still creating
	// zero-document runs that succeeded having read nothing (m52 M148-ingest-guard-layer).
	// The same mistake made twice would be a pattern, not an oversight.
	if active, aErr := c.RunStore.ActiveIngestion(ns, kb.Name); aErr != nil {
		return "", fmt.Errorf("could not determine whether an ingestion is already running for %q (failing closed — a concurrent ingest silently destroys the corpus): %w", kb.Name, aErr)
	} else if active != "" {
		return "", fmt.Errorf("%w: ingestion run %s is already in flight for KnowledgeBase %q", ErrIngestionInFlight, active, kb.Name)
	}

	docs := make([]IngestionDoc, 0, len(infos))
	for _, info := range infos {
		subject := ""
		if kb.Spec.PerUser {
			subject = objectstore.SubjectFromKey(ns, kb.Name, info.Key)
			if subject == "" {
				continue // per-user corpus, no recoverable owner → don't misattribute; skip
			}
		}
		docs = append(docs, IngestionDoc{
			Key:         info.Key,
			Filename:    info.Key,
			ContentType: info.ContentType,
			Subject:     subject,
		})
	}
	// Ingesting nothing is never what a caller meant, and a run that succeeds having read
	// nothing leaves the console showing an ingested KB whose retrieval is silently empty.
	// M148 put this in the BFF handler; M149's re-audit found the controller's SCHEDULED
	// re-ingest still creating zero-document runs on a timer, because it does not go through
	// that handler. It belongs here, where both entry points meet.
	if len(docs) == 0 {
		return "", ErrEmptyCorpus
	}

	spec := IngestionSpec{
		Namespace:      ns,
		KnowledgeBase:  kb.Name,
		EmbeddingRoute: kb.Spec.EmbeddingRoute,
		Chunking:       kb.Spec.Chunking,
		Documents:      docs,
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshal ingestion spec: %w", err)
	}
	runID, err := randToken(16)
	if err != nil {
		return "", fmt.Errorf("mint run id: %w", err)
	}
	rn := run.New(runID, ns, kb.Name, nil, "", time.Now())
	rn.IngestionRef = kb.Name
	rn.IngestionSpec = string(specJSON)
	if err := c.RunStore.Create(rn); err != nil {
		return "", fmt.Errorf("create ingestion run: %w", err)
	}
	return runID, nil
}

func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
