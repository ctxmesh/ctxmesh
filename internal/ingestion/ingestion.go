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

// RunStore is the narrow write seam the Creator needs (the run store's Create).
type RunStore interface {
	Create(rn *run.Run) error
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
func (c *Creator) CreateIngestionRun(ctx context.Context, kb *agentsv1beta1.KnowledgeBase) (string, error) {
	ns := kb.Namespace
	infos, err := ResolveKBSources(ctx, c.DocStore, ns, kb)
	if err != nil {
		return "", fmt.Errorf("resolve KB sources: %w", err)
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
