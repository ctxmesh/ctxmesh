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

// Package dataset is the control-plane store for eval datasets (ADR 0062 Fork 1, M69 — the improvement loop's
// dataset half). A dataset is thousands of versioned cases mutated by labeling: instance-heavy records-of-record
// that do NOT belong in etcd (the ADR 0044 precedent, mirroring internal/controlplane/promptversion). So
// EvalSuite.datasetRef stays the GitOps object (`name@version` immutable pin, or bare `name` = latest pinned)
// while the cases + labels + pinned versions live here in Postgres.
//
// Load-bearing invariant enforced by the store shape, not by callers (ADR 0062 Fork 1):
//
//   - Cases append into a mutable DRAFT HEAD (ListCases). Labels are APPEND-ONLY audit rows (AppendLabel never
//     updates/deletes); the latest label row per case is that case's current label state.
//   - PinVersion FREEZES a version = the draft head's case set PLUS a snapshot of each case's label state at pin
//     time. A pinned version resolves IDENTICALLY every time (ResolveVersion): appending cases or labels AFTER a
//     pin does NOT change what that version resolves to. The eval-gate records the resolved version so
//     "0.82 on rev A vs 0.79 on rev B" is only compared when the dataset didn't move between them.
//
// Two implementations — Postgres (pgstore) + an in-memory twin (memstore) — both pass one conformance suite,
// the internal/controlplane/promptversion + internal/controlplane/knowledge pattern.
//
// The caller seams this task leaves clean (built later): the export worker (m69.2) calls EnsureDataset +
// AppendCase to copy M66-redacted, traceId-lineaged cases out of Langfuse; the labeling API (m69.3) calls
// AppendLabel + ListCases; the eval-gate calls ResolveRef to resolve datasetRef → a frozen case set + a version
// number to record. This task builds the store only; no HTTP/worker/credentials are wired here.
package dataset

import (
	"context"
	"time"
)

// Dataset is the top-level record, identified by (Namespace, Name) — the EvalSuite.datasetRef target. ID is the
// store-assigned handle the case/label/version methods key on.
type Dataset struct {
	ID          string
	Namespace   string
	Name        string
	DisplayName string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Case is one dataset case in the draft head. ID is assigned by the store on AppendCase. Input/Expected are the
// eval inputs/expected output; SourceTraceID is the Langfuse trace lineage (PII-deletion cascade + the labeling
// link); MimeType + Tags carry provenance/filtering metadata.
type Case struct {
	ID            string
	DatasetID     string
	Input         string
	Expected      string
	SourceTraceID string
	MimeType      string
	Tags          map[string]string
	CreatedAt     time.Time
}

// Label is one APPEND-ONLY human judgment on a case (ADR 0062 Fork 1 — an audit-grade history). Value is the
// verdict (e.g. pass/fail/a score); Correction is an optional fixed expected output; Note is free text; Author
// records who. ID + CreatedAt are store-assigned. Labels are never updated or deleted — a re-judgment is a NEW
// row, and the latest row per case is that case's current label state.
type Label struct {
	ID         string
	CaseID     string
	Value      string
	Correction string
	Note       string
	Author     string
	CreatedAt  time.Time
}

// ResolvedCase is a case as an immutable pinned version resolves it: the case input/expected FROZEN at pin time,
// plus the frozen label state at pin time (HasLabel=false when the case had no label when it was pinned). This is
// exactly what the eval-gate scores against — stable across later case/label appends.
type ResolvedCase struct {
	CaseID          string
	Input           string
	Expected        string
	SourceTraceID   string
	MimeType        string
	Tags            map[string]string
	HasLabel        bool
	LabelValue      string
	LabelCorrection string
	LabelNote       string
	LabelAuthor     string
}

// Store is the control-plane repository for eval datasets. Entity-specific (not a generic Store[T]), matching the
// per-entity control-plane stores. Errors wrap the controlplane sentinels (ErrNotFound/ErrInvalid) so the BFF can
// map them to HTTP status codes.
type Store interface {
	// EnsureDataset idempotently creates a dataset by (namespace, name), returning the existing row if one
	// already exists (the export worker + the "add to dataset" flag both call this before appending). Namespace
	// and name are required; a blank one is controlplane.ErrInvalid.
	EnsureDataset(ctx context.Context, namespace, name string) (*Dataset, error)

	// AppendCase adds a case to the dataset's DRAFT HEAD, returning the assigned case ID. datasetID must refer to
	// an existing dataset (else controlplane.ErrNotFound); Input is required (controlplane.ErrInvalid otherwise).
	// The case is NOT part of any pinned version until a later PinVersion snapshots it.
	AppendCase(ctx context.Context, datasetID string, c Case) (caseID string, err error)

	// AppendLabel appends an APPEND-ONLY label row to a case (never updates/deletes an existing one). caseID must
	// exist (controlplane.ErrNotFound otherwise). The latest label per case becomes that case's current state,
	// which a later PinVersion freezes.
	AppendLabel(ctx context.Context, caseID string, l Label) error

	// PinVersion freezes the CURRENT draft head — every case + each case's latest label state — into a new
	// immutable version v = maxVersion+1, returning v. Appending cases/labels after the pin does NOT change what
	// v resolves to. datasetID must exist (controlplane.ErrNotFound); a dataset with no cases is
	// controlplane.ErrInvalid (an empty pinned version can't gate anything).
	PinVersion(ctx context.Context, datasetID string) (version int, err error)

	// ResolveVersion returns the immutable resolution of (namespace, name)@version: each frozen case's
	// input/expected + its frozen label state at pin time. controlplane.ErrNotFound if the dataset or the
	// version does not exist.
	ResolveVersion(ctx context.Context, namespace, name string, version int) ([]ResolvedCase, error)

	// ResolveRef parses a datasetRef and resolves it to a frozen case set + the resolved version number (so the
	// eval-gate can record which version it scored against). ref is either "name@version" (an immutable pin) or a
	// bare "name" (→ the latest PINNED version). A bare name with NO pinned version yet is controlplane.ErrInvalid
	// — an unpinned dataset can't gate reproducibly. controlplane.ErrNotFound if the dataset/version is missing.
	ResolveRef(ctx context.Context, namespace, ref string) (cases []ResolvedCase, version int, err error)

	// ListCases returns the dataset's DRAFT HEAD cases (for the labeling UI, m69.3), ordered oldest-first.
	// datasetID must exist (controlplane.ErrNotFound otherwise).
	ListCases(ctx context.Context, datasetID string) ([]Case, error)
}
