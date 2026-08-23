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

package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// pgStore is the DURABLE run store (ADR 0034 §durability, m32.1). It persists run state, the
// message history, the pending action, and the full event log to Postgres, so a run survives a
// pod restart/reschedule and a reconnecting client replays from the durable log via a
// Last-Event-ID cursor. It aligns with the credential state layer (Postgres via database/sql +
// the pgx driver, idempotent DDL at open, optimistic-concurrency via a version column) so an
// operator runs one datastore, not two.
//
// The store slots behind the same Store seam as the hot memStore: the BFF/worker construct it
// when a run-store DSN is configured and are otherwise untouched.
type pgStore struct {
	db *sql.DB
	// pollInterval is how often a live Subscribe tails run_events for new rows. Postgres has no
	// in-process broadcast, and events may be appended by ANOTHER pod (the worker, m32.2), so
	// live delivery is a short poll of the durable log — correct across processes. LISTEN/NOTIFY
	// is a latency optimisation left for later.
	pollInterval time.Duration
	// now is injectable for deterministic tests; defaults to time.Now.
	now func() time.Time
}

// errRunConflict is the optimistic-concurrency loser signal (another writer advanced the
// version between our read and write). Update re-reads and retries.
var errRunConflict = errors.New("run: optimistic-concurrency conflict")

// runStoreMaxRetries bounds the Update read-modify-write retry loop under contention.
const runStoreMaxRetries = 8

// runSchemaDDL creates the two run tables. Applied idempotently at open (matches the credstore
// pattern). runs holds one row per run (the checkpoint: status + messages + pending action);
// run_events is the append-only event log the SSE stream replays.
const runSchemaDDL = `
CREATE TABLE IF NOT EXISTS runs (
    id               text PRIMARY KEY,
    namespace        text NOT NULL DEFAULT '',
    agent            text NOT NULL DEFAULT '',
    input            bytea,
    conversation_id  text NOT NULL DEFAULT '',
    trace_id         text NOT NULL DEFAULT '',
    status           text NOT NULL,
    messages         bytea,
    requires_action  bytea,
    error            text NOT NULL DEFAULT '',
    caller_username  text NOT NULL DEFAULT '',
    boundary         text NOT NULL DEFAULT '',
    endpoint         text NOT NULL DEFAULT '',
    worker_id        text NOT NULL DEFAULT '',
    lease_expires_at timestamptz,
    workflow_ref     text NOT NULL DEFAULT '',
    spec_snapshot    text NOT NULL DEFAULT '',
    cursor           text NOT NULL DEFAULT '',
    wait_on          text NOT NULL DEFAULT '',
    wait_mode        text NOT NULL DEFAULT '',
    handed_off_to    text NOT NULL DEFAULT '',
    handoff_source_run_id text NOT NULL DEFAULT '',
    handoff_skip_history_replay boolean NOT NULL DEFAULT false,
    ingestion_ref    text NOT NULL DEFAULT '',
    ingestion_spec   text NOT NULL DEFAULT '',
    outcome          text NOT NULL DEFAULT '',
    version          bigint NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS run_events (
    run_id  text    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq     integer NOT NULL,
    kind    text    NOT NULL,
    data    text    NOT NULL DEFAULT '',
    at      timestamptz NOT NULL,
    PRIMARY KEY (run_id, seq)
);
-- Idempotent migration for a runs table created before the worker execution record (m32.2):
-- add the columns (defaults = an unclaimed run with no OBO re-mint material) so old rows load.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS caller_username  text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS boundary         text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS endpoint         text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS worker_id        text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz;
-- Spawn lineage (M64, ADR 0057): a sub-run records its parent + tree-root + depth. Defaults ('' / 0)
-- describe a ROOT run (no parent), so old rows load unchanged.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS parent_run_id text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS root_run_id   text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS spawn_depth   integer NOT NULL DEFAULT 0;
-- Output schema (M65, ADR 0058): the agent's spec.runtime.outputSchema pinned at create time.
-- NULL / absent ⇒ no schema (backward-compat: old rows load as "").
ALTER TABLE runs ADD COLUMN IF NOT EXISTS output_schema text;
-- Record mode (M78, ADR 0071 §1/§2): a run-scoped opt-in into record/replay capture. Default false
-- ⇒ a normal (non-recorded) run, so old rows load unchanged.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS record boolean NOT NULL DEFAULT false;
-- Workflow instance + wait record (M67, ADR 0060): a workflow instance run pins its WorkflowRef +
-- resolved SpecSnapshot + per-node Cursor; a waiting run parks on wait_on children under wait_mode.
-- wait_on is a JSON array of child ids (stored as text/JSON, matching the messages/requires_action
-- convention — no array-driver dependency). Defaults ('' / empty) describe a non-workflow,
-- non-waiting run, so old rows load unchanged.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS workflow_ref  text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS spec_snapshot text NOT NULL DEFAULT '';
-- Pinned node endpoints (m67.13, ADR 0011/0060): a workflow instance run resolves each node's agentRef →
-- endpoint at CREATE time through the CALLER-SCOPED client, then pins them here so the off-request executor
-- launches nodes WITHOUT any BFF-SA agent-CRD RBAC (the BFF Role is empty, rules: []). JSON object text
-- ('' ⇒ none), matching the wait_on JSON-in-text convention. Pinned at create + never mutated, so old rows
-- load unchanged.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS node_endpoints text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS cursor        text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS wait_on       text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS wait_mode     text NOT NULL DEFAULT '';
-- Handoff outcome (M67, ADR 0060 §5): the roster member the conversation was handed off to when this
-- run terminated via handoff_to. Default '' ⇒ this run did not hand off, so old rows load unchanged.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS handed_off_to text NOT NULL DEFAULT '';
-- Handoff backlink (M67, ADR 0060 §5): B's run records the run (A) whose handoff_to created it, since
-- a transferred run is a NEW ROOT with no parent_run_id. Default '' ⇒ not created by a handoff.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS handoff_source_run_id text NOT NULL DEFAULT '';
-- Handoff input filter (m83.6): a target run B created by a handoff_to with include_history=false
-- carries this flag so the run-worker stamps X-Ctxmesh-Include-History: false on B's transfer-turn
-- /invoke and the SDK skips the full-history replay (B starts from A's summary). Default false ⇒
-- replay the full history (ADR 0060 §5 default), so old rows + a default handoff load unchanged.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS handoff_skip_history_replay boolean NOT NULL DEFAULT false;
-- Ingestion job (M68, ADR 0061 Fork 2): an ingestion run pins its IngestionRef (the KB name) + resolved
-- IngestionSpec (source/embeddingRoute/chunking/doc-keys), routed to executeIngestion by IsIngestionJob().
-- outcome carries the executor-written terminal outcome (counts + partial flag + coded reason) — the m68.10
-- seam the KB-status reconcile reads. Defaults ('') describe a non-ingestion run, so old rows load unchanged.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS ingestion_ref  text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS ingestion_spec text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS outcome        text NOT NULL DEFAULT '';
-- Dataset export job (M69, ADR 0062 Fork 1): an export run pins its ExportRef (the dataset name) + resolved
-- ExportSpec (dataset namespace+name, agent tag, from/to timerange), routed to executeDatasetExport by
-- IsDatasetExportJob(). The shared outcome column carries the executor-written terminal outcome (documents
-- exported + cases appended + a coded reason). Defaults ('') describe a non-export run, so old rows load unchanged.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS export_ref  text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS export_spec text NOT NULL DEFAULT '';
-- Claim the oldest queued run fast (the worker's FOR UPDATE SKIP LOCKED path, m32.2).
CREATE INDEX IF NOT EXISTS runs_queued ON runs (created_at) WHERE status = 'queued';
-- Sweep waiting runs (the belt-and-braces reconciler, ADR 0060 §3) — a small partial index.
CREATE INDEX IF NOT EXISTS runs_waiting ON runs (id) WHERE status = 'waiting';
-- List runs paused for human approval (M75, ADR 0069 §3): the AlertPolicy approvalWaiting condition
-- reads runs in requires_action per namespace to fire the HITL notification. A small partial index.
CREATE INDEX IF NOT EXISTS runs_requires_action ON runs (namespace) WHERE status = 'requires_action';
-- Walk a spawn tree (audit / the console's parent→sub-run view) by its root.
CREATE INDEX IF NOT EXISTS runs_root ON runs (root_run_id) WHERE root_run_id <> '';
-- Resolve a run by its trace_id (GetByTraceID — the share-mint path, m75.4 / V6). trace_id defaults
-- to '' for runs not yet traced, so a PARTIAL index on the non-empty values keeps it small (most
-- rows early in a run's life have no trace yet) — mirroring the runs_root partial-index pattern.
CREATE INDEX IF NOT EXISTS runs_trace_id ON runs (trace_id) WHERE trace_id <> '';
-- The AUTHORITATIVE aggregate spawn-budget counter (M64, ADR 0057): one row per spawn TREE (keyed by
-- root run id), incremented atomically as the BFF admits each sub-run. The BFF keys it on the root it
-- derived from the VERIFIED parent, so it cannot be re-keyed by an agent for a fresh budget.
CREATE TABLE IF NOT EXISTS spawn_counters (
    root_run_id text PRIMARY KEY,
    spawns      bigint NOT NULL DEFAULT 0
);`

// NewPostgresStore opens a durable Postgres-backed run store over an open *sql.DB, applying the
// schema idempotently. The caller owns the DB's lifecycle (open/close), matching credstore.
func NewPostgresStore(ctx context.Context, db *sql.DB) (Store, error) {
	if _, err := db.ExecContext(ctx, runSchemaDDL); err != nil {
		return nil, fmt.Errorf("run: apply schema: %w", err)
	}
	return &pgStore{db: db, pollInterval: 250 * time.Millisecond, now: time.Now}, nil
}

// Durable reports that the Postgres store IS durable across a pod restart (M75, m75.1, ADR 0069 §1) — so
// the share-link mint (which refuses a non-durable backing store) is permitted against it. This is the
// Postgres side of the optional DurableStore capability the BFF type-asserts.
func (p *pgStore) Durable() bool { return true }

func (p *pgStore) Create(r *Run) error {
	inserted, err := insertRunTx(context.Background(), p.db, r)
	if err != nil {
		return err
	}
	if !inserted {
		return errors.New("run: id already exists")
	}
	return nil
}

// execer is the write half of *sql.DB / *sql.Tx (the pool or a transaction), so insertRunTx can run on
// either — a bare Create on the pool, or a child upsert inside the L7 suspend transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertRunTx inserts a run row (ON CONFLICT (id) DO NOTHING) via any execer and reports whether a row
// was actually inserted (false ⇒ the id already existed — the idempotent path SuspendOnDelegate relies on
// for a re-issued delegate child). The 36-column insert lives HERE, shared by Create + the L7 delegate
// suspend TX, so the two can never drift.
func insertRunTx(ctx context.Context, ex execer, r *Run) (bool, error) {
	msgs, err := json.Marshal(r.Messages)
	if err != nil {
		return false, fmt.Errorf("run: marshal messages: %w", err)
	}
	action, err := marshalAction(r.RequiresAction)
	if err != nil {
		return false, err
	}
	waitOn, err := marshalWaitOn(r.WaitOn)
	if err != nil {
		return false, err
	}
	nodeEndpoints, err := marshalNodeEndpoints(r.NodeEndpoints)
	if err != nil {
		return false, err
	}
	const q = `INSERT INTO runs
		(id, namespace, agent, input, conversation_id, trace_id, status, messages, requires_action, error,
		 caller_username, boundary, endpoint, worker_id, lease_expires_at,
		 parent_run_id, root_run_id, spawn_depth, output_schema, record,
		 workflow_ref, spec_snapshot, cursor, wait_on, wait_mode, handed_off_to, handoff_source_run_id,
		 node_endpoints, ingestion_ref, ingestion_spec, outcome, export_ref, export_spec,
		 handoff_skip_history_replay, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,1,$35,$36)
		ON CONFLICT (id) DO NOTHING`
	res, err := ex.ExecContext(ctx, q,
		r.ID, r.Namespace, r.Agent, []byte(r.Input), r.ConversationID, r.TraceID,
		string(r.Status), msgs, action, r.Error,
		r.CallerUsername, r.Boundary, r.Endpoint, r.WorkerID, nullableTime(r.LeaseExpiresAt),
		r.ParentRunID, r.RootRunID, r.SpawnDepth, nullableString(r.OutputSchema), r.Record,
		r.WorkflowRef, r.SpecSnapshot, r.Cursor, waitOn, string(r.WaitMode), r.HandedOffTo, r.HandoffSourceRunID,
		nodeEndpoints, r.IngestionRef, r.IngestionSpec, r.Outcome, r.ExportRef, r.ExportSpec,
		r.HandoffSkipHistoryReplay, r.CreatedAt.UTC(), r.UpdatedAt.UTC())
	if err != nil {
		return false, fmt.Errorf("run: insert: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (p *pgStore) Get(id string) (*Run, error) {
	r, _, err := p.getWithVersion(context.Background(), p.db, id)
	return r, err
}

// GetByTraceID looks up a run by its trace_id column. Returns (nil, nil) when no row matches —
// not an error, just "not found by trace id". The share mint calls this as a FALLBACK when
// runStore.Get(id) returns ErrNotFound (the UI's trace-detail page keys by traceId, not run.ID).
// Note: adding an index on trace_id would speed this up at scale — deferred as a follow-up since
// share minting is a rare, user-initiated action and an unindexed scan over the runs table is
// acceptable for now.
func (p *pgStore) GetByTraceID(traceID string) (*Run, error) {
	const sel = `SELECT id FROM runs WHERE trace_id = $1 LIMIT 1`
	var id string
	err := p.db.QueryRowContext(context.Background(), sel, traceID).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil // not found — not an error
	case err != nil:
		return nil, fmt.Errorf("run: get by trace id: %w", err)
	}
	r, _, err := p.getWithVersion(context.Background(), p.db, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil // row disappeared between the two selects — treat as not found
	}
	return r, err
}

// ReserveSpawn atomically increments the tree's total-spawn counter and admits when within maxTotal.
// The INSERT ... ON CONFLICT ... WHERE is one atomic statement: a first spawn inserts 1; a subsequent
// spawn increments ONLY while under budget (the WHERE gates the DO UPDATE), so an over-budget attempt
// updates nothing and returns no row → denied WITHOUT recording the rejected spawn. Fails CLOSED.
func (p *pgStore) ReserveSpawn(rootRunID string, maxTotal int) (bool, error) {
	const q = `INSERT INTO spawn_counters (root_run_id, spawns) VALUES ($1, 1)
		ON CONFLICT (root_run_id) DO UPDATE SET spawns = spawn_counters.spawns + 1
		WHERE spawn_counters.spawns < $2
		RETURNING spawns`
	var spawns int64
	err := p.db.QueryRowContext(context.Background(), q, rootRunID, maxTotal).Scan(&spawns)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil // the DO UPDATE's WHERE was false → over budget
	case err != nil:
		return false, fmt.Errorf("run: reserve spawn: %w", err) // fail closed
	}
	return true, nil
}

// runRowScanner is satisfied by both *sql.Row and *sql.Rows, so getWithVersion (one row) and List (many
// rows) share ONE scan + column list — the column ORDER must match runRowColumns exactly.
type runRowScanner interface{ Scan(dest ...any) error }

// runRowColumns is the ordered SELECT list for a full run row, shared by getWithVersion and List so the
// two can never drift. cursorExpr is the SQL at the cursor position: `cursor` HYDRATES the checkpoint
// (Get / worker claim / Update need it); `”` SKIPS the MB-scale L7 supervisor checkpoint on a list fill
// (L12) — Postgres neither reads nor transfers it, and the column order is unchanged so ONE scan serves
// both. It leads with `id` so a bulk List can carry the id per row (getWithVersion already knows it).
func runRowColumns(cursorExpr string) string {
	return `id, namespace, agent, input, conversation_id, trace_id, status, messages, requires_action, error,
		caller_username, boundary, endpoint, worker_id, lease_expires_at,
		parent_run_id, root_run_id, spawn_depth, output_schema, record,
		workflow_ref, spec_snapshot, ` + cursorExpr + ` AS cursor, wait_on, wait_mode, handed_off_to, handoff_source_run_id,
		node_endpoints, ingestion_ref, ingestion_spec, outcome, export_ref, export_spec,
		handoff_skip_history_replay, version, created_at, updated_at`
}

// scanRunRow scans one full run row (columns in runRowColumns order) into a Run + its OCC version. A list
// fill projects ” for cursor (L12), so r.Cursor is empty on those rows — the resume path reads the real
// checkpoint only via getWithVersion (worker claim / Get). sql.ErrNoRows is returned BARE so a single-row
// caller can map it to ErrNotFound; other scan failures are wrapped.
func scanRunRow(sc runRowScanner) (*Run, int64, error) {
	var (
		r             Run
		input         []byte
		status        string
		msgs          []byte
		action        []byte
		lease         sql.NullTime
		outputSchema  sql.NullString
		waitOn        string
		waitMode      string
		nodeEndpoints string
		version       int64
		created       time.Time
		updated       time.Time
	)
	err := sc.Scan(
		&r.ID, &r.Namespace, &r.Agent, &input, &r.ConversationID, &r.TraceID, &status,
		&msgs, &action, &r.Error, &r.CallerUsername, &r.Boundary, &r.Endpoint, &r.WorkerID, &lease,
		&r.ParentRunID, &r.RootRunID, &r.SpawnDepth, &outputSchema, &r.Record,
		&r.WorkflowRef, &r.SpecSnapshot, &r.Cursor, &waitOn, &waitMode, &r.HandedOffTo, &r.HandoffSourceRunID,
		&nodeEndpoints, &r.IngestionRef, &r.IngestionSpec, &r.Outcome, &r.ExportRef, &r.ExportSpec,
		&r.HandoffSkipHistoryReplay, &version, &created, &updated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, 0, err // bare — the single-row caller maps this to ErrNotFound
	case err != nil:
		return nil, 0, fmt.Errorf("run: scan run row: %w", err)
	}
	r.Status = Status(status)
	r.CreatedAt = created.UTC()
	r.UpdatedAt = updated.UTC()
	if lease.Valid {
		t := lease.Time.UTC()
		r.LeaseExpiresAt = &t
	}
	if outputSchema.Valid {
		r.OutputSchema = outputSchema.String
	}
	r.WaitMode = WaitMode(waitMode)
	wo, err := unmarshalWaitOn(waitOn)
	if err != nil {
		return nil, 0, err
	}
	r.WaitOn = wo
	ne, err := unmarshalNodeEndpoints(nodeEndpoints)
	if err != nil {
		return nil, 0, err
	}
	r.NodeEndpoints = ne
	if len(input) > 0 {
		r.Input = append(json.RawMessage(nil), input...)
	}
	if len(msgs) > 0 {
		if err := json.Unmarshal(msgs, &r.Messages); err != nil {
			return nil, 0, fmt.Errorf("run: unmarshal messages: %w", err)
		}
	}
	if len(action) > 0 {
		var a Action
		if err := json.Unmarshal(action, &a); err != nil {
			return nil, 0, fmt.Errorf("run: unmarshal action: %w", err)
		}
		r.RequiresAction = &a
	}
	return &r, version, nil
}

// getWithVersion loads a run + its OCC version from the given querier (the pool or a tx). It hydrates the
// full row INCLUDING the cursor (the resume path — worker claim / Get / Update round-trip — needs it). A
// missing row is ErrNotFound.
func (p *pgStore) getWithVersion(ctx context.Context, q querier, id string) (*Run, int64, error) {
	sel := `SELECT ` + runRowColumns("cursor") + ` FROM runs WHERE id=$1`
	r, version, err := scanRunRow(q.QueryRowContext(ctx, sel, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, 0, ErrNotFound
	case err != nil:
		return nil, 0, err
	}
	return r, version, nil
}

func (p *pgStore) Update(id string, fn func(*Run) error) (*Run, error) {
	ctx := context.Background()
	for range runStoreMaxRetries {
		result, err := p.tryUpdate(ctx, id, fn)
		if errors.Is(err, errRunConflict) {
			continue // another writer won; re-read and retry
		}
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, errRunConflict
}

// tryUpdate performs one read-modify-write attempt in a transaction. It re-reads the run under a
// row lock, applies fn, writes the row guarded by the version, and — mirroring memStore — emits a
// `state` event from the ONE place status changes, all atomically.
func (p *pgStore) tryUpdate(ctx context.Context, id string, fn func(*Run) error) (*Run, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("run: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the run row so a concurrent AppendEvent/Update on the same run serialises (keeps the
	// event seq monotonic and the version check race-free).
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE`, id); err != nil {
		return nil, fmt.Errorf("run: lock: %w", err)
	}
	current, version, err := p.getWithVersion(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	oldStatus := current.Status
	working := cloneRun(current)
	if err := fn(working); err != nil {
		return nil, err // fn's error (e.g. an illegal Transition) aborts, run unchanged
	}

	msgs, err := json.Marshal(working.Messages)
	if err != nil {
		return nil, fmt.Errorf("run: marshal messages: %w", err)
	}
	action, err := marshalAction(working.RequiresAction)
	if err != nil {
		return nil, err
	}
	waitOn, err := marshalWaitOn(working.WaitOn)
	if err != nil {
		return nil, err
	}
	const upd = `UPDATE runs SET
			trace_id=$2, status=$3, messages=$4, requires_action=$5, error=$6,
			worker_id=$7, lease_expires_at=$8, cursor=$9, wait_on=$10, wait_mode=$11, handed_off_to=$12,
			outcome=$13, version=version+1, updated_at=$14
		WHERE id=$1 AND version=$15`
	res, err := tx.ExecContext(ctx, upd,
		id, working.TraceID, string(working.Status), msgs, action, working.Error,
		working.WorkerID, nullableTime(working.LeaseExpiresAt),
		working.Cursor, waitOn, string(working.WaitMode), working.HandedOffTo,
		working.Outcome, working.UpdatedAt.UTC(), version)
	if err != nil {
		return nil, fmt.Errorf("run: update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, errRunConflict
	}

	// A status change automatically appends a `state` event — same invariant as memStore.
	if working.Status != oldStatus {
		if err := appendEventTx(ctx, tx, id, EventState, string(working.Status), p.clock()); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("run: commit: %w", err)
	}
	return cloneRun(working), nil
}

// writeRunTx persists a run row inside a transaction, guarded by its OCC version (version=$N),
// bumping the version. It returns errRunConflict if the row moved under us (the standard OCC loser
// signal, retried by the caller's retry loop). The caller MUST already hold the row lock. It writes
// the same mutable column set as tryUpdate's UPDATE (incl. handed_off_to, m67.6, and outcome, m68.6), so the
// child/parent writes in CompleteAndWake persist cursor + wait record + lease + handoff outcome + ingestion
// outcome exactly like an ordinary Update — the two mutable-column sets must never diverge. handoff_source_run_id,
// ingestion_ref and ingestion_spec are NOT here: they are create-only (set once at create, never mutated), like
// parent_run_id.
func (p *pgStore) writeRunTx(ctx context.Context, tx *sql.Tx, r *Run, version int64) error {
	msgs, err := json.Marshal(r.Messages)
	if err != nil {
		return fmt.Errorf("run: marshal messages: %w", err)
	}
	action, err := marshalAction(r.RequiresAction)
	if err != nil {
		return err
	}
	waitOn, err := marshalWaitOn(r.WaitOn)
	if err != nil {
		return err
	}
	const upd = `UPDATE runs SET
			trace_id=$2, status=$3, messages=$4, requires_action=$5, error=$6,
			worker_id=$7, lease_expires_at=$8, cursor=$9, wait_on=$10, wait_mode=$11, handed_off_to=$12,
			outcome=$13, version=version+1, updated_at=$14
		WHERE id=$1 AND version=$15`
	res, err := tx.ExecContext(ctx, upd,
		r.ID, r.TraceID, string(r.Status), msgs, action, r.Error,
		r.WorkerID, nullableTime(r.LeaseExpiresAt), r.Cursor, waitOn, string(r.WaitMode), r.HandedOffTo,
		r.Outcome, r.UpdatedAt.UTC(), version)
	if err != nil {
		return fmt.Errorf("run: update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errRunConflict
	}
	return nil
}

// Suspend parks a running run in `waiting` (ADR 0060 §3). It is a single-row read-modify-write, so
// it rides the existing Update path (row lock + OCC retry): fn checkpoints (e.g. the cursor) first,
// then suspendToWaiting flips running→waiting, sets the wait record, and releases the lease/worker.
// The `state` event is auto-emitted by Update from the one place status changes.
func (p *pgStore) Suspend(id string, waitOn []string, mode WaitMode, fn func(*Run) error) (*Run, error) {
	return p.Update(id, func(r *Run) error {
		if fn != nil {
			if err := fn(r); err != nil {
				return err
			}
		}
		return r.suspendToWaiting(waitOn, mode, p.clock())
	})
}

// SuspendOnDelegate — see the Store interface. The L7 delegate suspend transaction (ADR 0091): child
// upsert + parent checkpoint + suspend-or-requeue in ONE OCC-guarded, lock-ordered transaction, with the
// lost-wakeup guard (an already-terminal child is never waited on). OCC-retried like CompleteAndWake.
func (p *pgStore) SuspendOnDelegate(parentID string, children []*Run, mode WaitMode, fn func(*Run) error) (*Run, error) {
	ctx := context.Background()
	for range runStoreMaxRetries {
		parent, err := p.trySuspendOnDelegate(ctx, parentID, children, mode, fn)
		if errors.Is(err, errRunConflict) {
			continue // an OCC loser on the parent version (a concurrent wake) — re-read and retry
		}
		if err != nil {
			return nil, err
		}
		return parent, nil
	}
	return nil, errRunConflict
}

func (p *pgStore) trySuspendOnDelegate(
	ctx context.Context, parentID string, children []*Run, mode WaitMode, fn func(*Run) error,
) (*Run, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("run: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// (1) Upsert the delegate child run(s) inside the TX — idempotent (a re-issued delegate collapses to
	// the same row via ON CONFLICT DO NOTHING). Whether a row was freshly inserted is irrelevant; step (3)
	// re-reads the authoritative status under the lock either way.
	for _, c := range children {
		if _, err := insertRunTx(ctx, tx, c); err != nil {
			return nil, err
		}
	}

	// (2) Lock the parent + every child by ASCENDING id — the deadlock-free discipline CompleteAndWake uses,
	// so a concurrent suspend/wake touching the same rows can never hold-and-wait in a cycle.
	lockIDs := make([]string, 0, len(children)+1)
	lockIDs = append(lockIDs, parentID)
	for _, c := range children {
		lockIDs = append(lockIDs, c.ID)
	}
	if err := lockRunsOrdered(ctx, tx, lockIDs); err != nil {
		return nil, err
	}

	// (3) Re-read each child's CURRENT status under the lock; the wait set is only the NON-terminal children.
	// An already-terminal child is NEVER added to WaitOn (the lost-wakeup race, ADR 0091 fork 2 — the #1
	// footgun): a dead child fires no future CompleteAndWake, so parking on it would strand the parent forever.
	waitOn := make([]string, 0, len(children))
	for _, c := range children {
		cur, _, err := p.getWithVersion(ctx, tx, c.ID)
		if err != nil {
			return nil, err
		}
		if !cur.Status.IsTerminal() {
			waitOn = append(waitOn, c.ID)
		}
	}

	// (4) Re-read the parent under the lock, checkpoint it (fn sets Cursor), then suspend-or-requeue.
	parent, pv, err := p.getWithVersion(ctx, tx, parentID)
	if err != nil {
		return nil, err
	}
	parentOld := parent.Status
	if fn != nil {
		if err := fn(parent); err != nil {
			return nil, err
		}
	}
	now := p.clock()
	if len(waitOn) == 0 {
		// Every child is ALREADY terminal ⇒ the WaitAll wait is satisfied NOW. Collapse suspend+wake into
		// this TX: running→waiting (release the lease) → queued (the immediately-met wait re-queues) — EXACTLY
		// what a suspend followed by an instant CompleteAndWake would produce. Never park on an empty wait set.
		if err := parent.Transition(StatusWaiting, now); err != nil {
			return nil, err
		}
		parent.WorkerID, parent.LeaseExpiresAt = "", nil
		if err := parent.Transition(StatusQueued, now); err != nil {
			return nil, err
		}
	} else if err := parent.suspendToWaiting(waitOn, mode, now); err != nil {
		return nil, err
	}
	if err := p.writeRunTx(ctx, tx, parent, pv); err != nil {
		return nil, err
	}
	if parent.Status != parentOld {
		if err := appendEventTx(ctx, tx, parentID, EventState, string(parent.Status), now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("run: commit: %w", err)
	}
	return cloneRun(parent), nil
}

// CompleteAndWake is the transactional cross-run wake (ADR 0060 §3) — the load-bearing two-row
// transaction. In ONE BeginTx it terminates the child and, if a `waiting` parent is parked on it,
// re-queues that parent when the wait is satisfied.
//
// Lock order (deadlock-avoidance): we lock the child + its parent by ASCENDING id in a single
// `SELECT ... FOR UPDATE ORDER BY id`. Any two transactions that touch the same {child, parent} pair
// acquire the two rows in the SAME order, so they can never hold-and-wait in a cycle. (A child has
// exactly one parent and a parent's id is fixed, so the pair is well-defined; ordering by id is a
// total order over the two rows.) We resolve the parent id with a prior unlocked read only to know
// WHICH two rows to lock; the authoritative state is re-read AFTER the locks are held.
//
// Exactly-once / no-double-wake: the child terminal + the parent re-queue commit atomically, so no
// separate notification can be lost or duplicated. Re-invoking on an already-terminal child is a
// no-op (guarded before apply) — a reclaimed/duplicated completion neither re-queues the parent nor
// corrupts the wait set. The parent re-queue only fires when the child is STILL in the parent's
// persisted wait set (satisfyChild is a no-op otherwise), so two children completing concurrently
// each remove themselves under the parent row lock and only the one meeting the condition re-queues.
func (p *pgStore) CompleteAndWake(childID string, apply func(*Run) error) (*Run, *Run, error) {
	ctx := context.Background()
	for range runStoreMaxRetries {
		child, parent, err := p.tryCompleteAndWake(ctx, childID, apply)
		if errors.Is(err, errRunConflict) {
			continue // an OCC loser on the child or parent version; re-read and retry
		}
		if err != nil {
			return nil, nil, err
		}
		return child, parent, nil
	}
	return nil, nil, errRunConflict
}

func (p *pgStore) tryCompleteAndWake(ctx context.Context, childID string, apply func(*Run) error) (*Run, *Run, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("run: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Resolve the parent id with an unlocked read so we know which rows to lock (and in what order).
	// The child's ParentRunID never changes, so this is safe to read before taking locks.
	childPeek, _, err := p.getWithVersion(ctx, tx, childID)
	if err != nil {
		return nil, nil, err
	}
	parentID := childPeek.ParentRunID

	// Lock the child (and its parent, if any) in ASCENDING id order — a consistent lock order that
	// makes a deadlock between two concurrent completions impossible. FOR UPDATE (not SKIP LOCKED):
	// we MUST wait for the row, not skip it, because we are the sole terminal writer for the child.
	lockIDs := []string{childID}
	if parentID != "" {
		lockIDs = append(lockIDs, parentID)
	}
	if err := lockRunsOrdered(ctx, tx, lockIDs); err != nil {
		return nil, nil, err
	}

	// Re-read the child UNDER the lock — the authoritative state.
	child, childVersion, err := p.getWithVersion(ctx, tx, childID)
	if err != nil {
		return nil, nil, err
	}
	// Idempotency: an already-terminal child is a no-op. A reclaimed/duplicated completion must not
	// re-run apply (the state machine would reject terminal→terminal anyway) or re-wake the parent.
	if child.Status.IsTerminal() {
		if err := tx.Commit(); err != nil {
			return nil, nil, fmt.Errorf("run: commit: %w", err)
		}
		return cloneRun(child), nil, nil
	}

	childOld := child.Status
	if err := apply(child); err != nil {
		return nil, nil, err // apply's error (e.g. an illegal transition) aborts, nothing written
	}
	if !child.Status.IsTerminal() {
		return nil, nil, fmt.Errorf("run %s: CompleteAndWake apply must reach a terminal state, got %s", childID, child.Status)
	}
	if err := p.writeRunTx(ctx, tx, child, childVersion); err != nil {
		return nil, nil, err
	}
	if child.Status != childOld {
		if err := appendEventTx(ctx, tx, childID, EventState, string(child.Status), p.clock()); err != nil {
			return nil, nil, err
		}
	}

	// Wake the waiting parent, in the SAME transaction, if it is parked on this child.
	var wokeParent *Run
	if parentID != "" {
		parent, parentVersion, gErr := p.getWithVersion(ctx, tx, parentID)
		switch {
		case errors.Is(gErr, ErrNotFound):
			// Parent gone — nothing to wake (lineage without a live parent). Not an error.
		case gErr != nil:
			return nil, nil, gErr
		case parent.Status == StatusWaiting:
			met, removed := parent.satisfyChild(childID, child.Status)
			if removed {
				pOld := parent.Status
				if met {
					if err := parent.Transition(StatusQueued, p.clock()); err != nil {
						return nil, nil, err
					}
				}
				if err := p.writeRunTx(ctx, tx, parent, parentVersion); err != nil {
					return nil, nil, err
				}
				if parent.Status != pOld {
					if err := appendEventTx(ctx, tx, parentID, EventState, string(parent.Status), p.clock()); err != nil {
						return nil, nil, err
					}
					wokeParent = cloneRun(parent)
				}
			}
		default:
			// The parent is NOT `waiting` on this child — e.g. it was CANCELLED by a subtree cascade
			// (L9, ADR 0091 fork 6) or already woken/terminal. The child still terminates (committed
			// above); waking is a clean no-op, NEVER an error — a canceled parent is simply not re-queued.
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("run: commit: %w", err)
	}
	return cloneRun(child), wokeParent, nil
}

// SweepWaiting re-queues waiting runs whose wait is ALREADY satisfied by their children's persisted
// terminal states — the belt-and-braces reconciler for the crash window between a child's terminal
// transition and CompleteAndWake's parent re-queue (ADR 0060 §3). Each candidate is re-queued in its
// OWN row-locked transaction via the standard Update path, so it is idempotent (a run already woken
// by CompleteAndWake is no longer `waiting` and is skipped) and OCC-safe against a concurrent wake.
func (p *pgStore) SweepWaiting() ([]string, error) {
	ctx := context.Background()
	// Candidate ids: waiting runs with a non-empty wait set. We evaluate the wait condition below,
	// per candidate, against the children's current statuses (a small set — the partial index helps).
	rows, err := p.db.QueryContext(ctx, `SELECT id FROM runs WHERE status='waiting' AND wait_on <> ''`)
	if err != nil {
		return nil, fmt.Errorf("run: sweep query: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("run: sweep scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("run: sweep iterate: %w", err)
	}
	_ = rows.Close()

	var woke []string
	for _, id := range ids {
		requeued, err := p.sweepOne(ctx, id)
		if err != nil {
			return woke, err
		}
		if requeued {
			woke = append(woke, id)
		}
	}
	return woke, nil
}

// sweepOne re-queues one waiting run IF its wait is met by its children's persisted terminal states.
// The wait is re-checked UNDER the run's row lock so a concurrent CompleteAndWake wake is serialised.
func (p *pgStore) sweepOne(ctx context.Context, id string) (bool, error) {
	requeued := false
	_, err := p.Update(id, func(r *Run) error {
		requeued = false
		if r.Status != StatusWaiting || len(r.WaitOn) == 0 {
			return nil // already woken / no longer waiting — idempotent no-op
		}
		met, err := p.waitMet(ctx, r)
		if err != nil {
			return err
		}
		if !met {
			return nil
		}
		requeued = true
		return r.Transition(StatusQueued, p.clock())
	})
	if err != nil {
		return false, err
	}
	return requeued, nil
}

// waitMet is the Postgres sweep adapter: it reads the run's WaitOn children's persisted statuses (a
// MISSING child row → StatusCancelled, per waitSatisfied's contract — a non-success terminal, never a
// success) and defers the decision to the SINGLE predicate waitSatisfied. There is NO second copy of the
// mode logic here — the hot path (satisfyChild) and the sweep must agree, and a property test pins it.
// Read on the pool (advisory); the caller re-checks under the row lock.
func (p *pgStore) waitMet(ctx context.Context, r *Run) (bool, error) {
	statuses := make([]Status, len(r.WaitOn))
	for i, cid := range r.WaitOn {
		var status string
		err := p.db.QueryRowContext(ctx, `SELECT status FROM runs WHERE id=$1`, cid).Scan(&status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			statuses[i] = StatusCancelled // a missing child ⇒ cancelled-equivalent (never a success)
		case err != nil:
			return false, fmt.Errorf("run: sweep child status: %w", err)
		default:
			statuses[i] = Status(status)
		}
	}
	return waitSatisfied(r.WaitMode, statuses), nil
}

// lockRunsOrdered acquires FOR UPDATE row locks on the given run ids in a single statement, ordered
// by id, so all callers touching an overlapping set take the locks in the SAME order (deadlock-free).
// It errors ErrNotFound if any id is missing (all rows must exist to lock the pair coherently).
func lockRunsOrdered(ctx context.Context, tx *sql.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	// Build ($1,$2,...) and lock in ascending id order (ORDER BY id) — the consistent lock order.
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "$" + strconv.Itoa(i+1)
		args[i] = id
	}
	q := "SELECT id FROM runs WHERE id IN (" + strings.Join(placeholders, ",") + ") ORDER BY id FOR UPDATE"
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("run: lock ordered: %w", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("run: lock scan: %w", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("run: lock iterate: %w", err)
	}
	if n != len(ids) {
		return ErrNotFound // a row we needed to lock is gone
	}
	return nil
}

func (p *pgStore) ClaimQueued(workerID string, lease time.Duration) (*Run, error) {
	ctx := context.Background()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("run: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Claim the oldest queued run with FOR UPDATE SKIP LOCKED so N workers never grab the same run
	// and a locked row doesn't block a peer — it just picks the next one. Stamp the worker + lease
	// and flip to running in the one statement.
	now := p.clock()
	exp := now.Add(lease)
	const claim = `UPDATE runs SET status=$1, worker_id=$2, lease_expires_at=$3, version=version+1, updated_at=$4
		WHERE id = (
			SELECT id FROM runs WHERE status=$5 ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1
		)
		RETURNING id`
	var id string
	err = tx.QueryRowContext(ctx, claim,
		string(StatusRunning), workerID, exp.UTC(), now.UTC(), string(StatusQueued)).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNoQueuedRun
	case err != nil:
		return nil, fmt.Errorf("run: claim queued: %w", err)
	}

	// queued→running auto-emits its state event, same invariant as Update.
	if err := appendEventTx(ctx, tx, id, EventState, string(StatusRunning), now); err != nil {
		return nil, err
	}
	claimed, _, err := p.getWithVersion(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("run: commit: %w", err)
	}
	return claimed, nil
}

func (p *pgStore) Heartbeat(id, workerID string, lease time.Duration) error {
	ctx := context.Background()
	now := p.clock()
	exp := now.Add(lease)
	// Renew only if THIS worker still holds the lease on a still-running run. No version bump — a
	// lease renewal is not a logical state change (a concurrent terminal Update is unaffected).
	const q = `UPDATE runs SET lease_expires_at=$1, updated_at=$2
		WHERE id=$3 AND worker_id=$4 AND status=$5`
	res, err := p.db.ExecContext(ctx, q, exp.UTC(), now.UTC(), id, workerID, string(StatusRunning))
	if err != nil {
		return fmt.Errorf("run: heartbeat: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Nothing updated: the run is gone, terminal, or leased by someone else (reclaimed).
		if _, _, gErr := p.getWithVersion(ctx, p.db, id); errors.Is(gErr, ErrNotFound) {
			return ErrNotFound
		}
		return ErrLeaseLost
	}
	return nil
}

// ReleaseLease expires the lease on a run still leased by workerID (D4). It sets lease_expires_at to
// NOW so ClaimReclaimable (which takes lease_expires_at < now) can re-lease it immediately, without
// waiting a full lease-TTL. Scoped to worker_id + status=running: a run already reclaimed by a peer,
// terminal, or gone matches nothing and is a no-op (nil) — releasing is best-effort + idempotent, so
// a draining worker never fails on it and never disturbs a run it no longer holds. No version bump (a
// lease release is not a logical state change, exactly like Heartbeat).
func (p *pgStore) ReleaseLease(id, workerID string) error {
	ctx := context.Background()
	now := p.clock()
	// Set the lease to a moment strictly in the PAST so ClaimReclaimable (lease_expires_at < now) can
	// re-lease it immediately — robust even under a fixed test clock where release-now == reclaim-now.
	expired := now.Add(-time.Second)
	const q = `UPDATE runs SET lease_expires_at=$1, updated_at=$2
		WHERE id=$3 AND worker_id=$4 AND status=$5`
	if _, err := p.db.ExecContext(ctx, q, expired.UTC(), now.UTC(), id, workerID, string(StatusRunning)); err != nil {
		return fmt.Errorf("run: release lease: %w", err)
	}
	return nil
}

func (p *pgStore) ClaimReclaimable(workerID string, lease time.Duration) (*Run, error) {
	ctx := context.Background()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("run: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := p.clock()
	exp := now.Add(lease)
	// Re-lease the oldest running run whose lease has expired (its worker is presumed dead). FOR
	// UPDATE SKIP LOCKED so two live workers never reclaim the same run. It stays `running` (no
	// state change, no version bump) — the worker resumes it from its last checkpoint.
	const claim = `UPDATE runs SET worker_id=$1, lease_expires_at=$2, updated_at=$3
		WHERE id = (
			SELECT id FROM runs
			WHERE status=$4 AND lease_expires_at IS NOT NULL AND lease_expires_at < $5
			ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1
		)
		RETURNING id`
	var id string
	err = tx.QueryRowContext(ctx, claim,
		workerID, exp.UTC(), now.UTC(), string(StatusRunning), now.UTC()).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNoQueuedRun
	case err != nil:
		return nil, fmt.Errorf("run: claim reclaimable: %w", err)
	}
	reclaimed, _, err := p.getWithVersion(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("run: commit: %w", err)
	}
	return reclaimed, nil
}

func (p *pgStore) List() []*Run {
	ctx := context.Background()
	// L12: a list fill never needs the (MB-scale, L7 supervisor-checkpoint) cursor — project '' for it so
	// the bulk scan neither reads nor transfers every run's checkpoint (a console poll / a cancel-cascade
	// walk would otherwise drag every suspended supervisor's ~MiB checkpoint into the BFF). The resume
	// path (worker claim → getWithVersion) still hydrates the real cursor. One scan also replaces the old
	// N+1 (SELECT id, then a per-id Get).
	rows, err := p.db.QueryContext(ctx, `SELECT `+runRowColumns("''")+` FROM runs`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	out := make([]*Run, 0)
	for rows.Next() {
		r, _, err := scanRunRow(rows)
		if err != nil {
			return nil
		}
		out = append(out, r)
	}
	return out
}

// WaitingApproval is the projection of a plan_approval-paused run — the AlertPolicy approvalWaiting
// notification (M75, ADR 0069 §3) AND the V5 console approval queue (M112). RootRunID gives a paused
// DESCENDANT sub-run its tree context; CallerUsername is the run's creator, used ONLY by the BFF's
// inline-workflow owner filter (V5) — it must never be sent to a client.
type WaitingApproval struct {
	ID             string
	Agent          string
	Message        string
	Kind           ActionKind
	RootRunID      string
	CallerUsername string
	Namespace      string
	// WaitingSince is when the run entered requires_action (its pause transition = the row's
	// updated_at) — the "waiting since" triage signal the V5 console queue renders (M113). For a
	// currently-paused run the last transition IS the pause, so updated_at is the honest wait-start
	// (more accurate than created_at, which is run-start).
	WaitingSince time.Time
}

// ListWaitingApproval returns the runs in the given namespace currently paused in requires_action whose
// RequiresAction.Kind is in `kinds` (M75 approvalWaiting passes {plan_approval}; the M112/M113 V5 console
// queue passes the caller-authorized subset of {plan_approval, approval} — never consent_required, which is
// owner-only). It reads the partial-indexed requires_action rows for the namespace and filters kind in Go
// (the action is JSON). Read-only; a query/scan error is returned so a caller can log + skip.
func (p *pgStore) ListWaitingApproval(ctx context.Context, namespace string, kinds []ActionKind, limit int) ([]WaitingApproval, error) {
	if len(kinds) == 0 {
		return nil, nil // no authorized kinds ⇒ nothing to read (the BFF 403s before this)
	}
	// Most-recently-updated first. The kind filter is applied in Go (the action is JSON), so the SQL
	// LIMIT must NOT be applied here — a pre-filter LIMIT would let consent/denied-kind rows consume the
	// window and return a silently-SHORT page while more of the caller's entitled rows exist (a partial
	// list masquerading as complete — the ADR-0011 trap). The scan is over one namespace's requires_action
	// rows (the runs_requires_action partial index; a small paused-run set), and the display limit is
	// applied AFTER the kind filter, below.
	q := `SELECT id, agent, root_run_id, caller_username, updated_at, requires_action
		FROM runs WHERE namespace=$1 AND status=$2 ORDER BY updated_at DESC`
	args := []any{namespace, string(StatusRequiresAction)}
	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("run: list requires_action: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WaitingApproval
	for rows.Next() {
		var (
			id             string
			agent          string
			rootRunID      string
			callerUsername string
			updated        time.Time
			action         []byte
		)
		if err := rows.Scan(&id, &agent, &rootRunID, &callerUsername, &updated, &action); err != nil {
			return nil, fmt.Errorf("run: list requires_action scan: %w", err)
		}
		if len(action) == 0 {
			continue // requires_action with no action record — nothing to key on, skip
		}
		var a Action
		if err := json.Unmarshal(action, &a); err != nil {
			return nil, fmt.Errorf("run: list requires_action unmarshal: %w", err)
		}
		if !slices.Contains(kinds, a.Kind) {
			continue // a pause kind the caller did not ask for / is not authorized for
		}
		out = append(out, WaitingApproval{
			ID: id, Agent: agent, Message: a.Message, Kind: a.Kind, RootRunID: rootRunID,
			CallerUsername: callerUsername, Namespace: namespace, WaitingSince: updated.UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run: list requires_action rows: %w", err)
	}
	// Apply the display limit AFTER the kind filter so the page is accurate (never silently short).
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DescendantsRequiringAction — see the Store interface (L1 surfacing, ADR 0075 §4). Reads the
// descendant rows (root_run_id = the true root) currently in requires_action and projects the pause,
// unmarshaling the JSON action in Go (as the store already does elsewhere). Read-only. The
// runs_root + runs_requires_action partial indexes cover the predicate.
func (p *pgStore) DescendantsRequiringAction(rootRunID string) ([]DescendantAction, error) {
	if rootRunID == "" {
		return nil, nil
	}
	rows, err := p.db.QueryContext(context.Background(),
		`SELECT id, agent, requires_action FROM runs WHERE root_run_id=$1 AND status=$2`,
		rootRunID, string(StatusRequiresAction))
	if err != nil {
		return nil, fmt.Errorf("run: list descendants requiring action: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DescendantAction
	for rows.Next() {
		var (
			id     string
			agent  string
			action []byte
		)
		if err := rows.Scan(&id, &agent, &action); err != nil {
			return nil, fmt.Errorf("run: descendants requiring action scan: %w", err)
		}
		if len(action) == 0 {
			continue // requires_action with no action record — nothing to surface
		}
		var a Action
		if err := json.Unmarshal(action, &a); err != nil {
			return nil, fmt.Errorf("run: descendants requiring action unmarshal: %w", err)
		}
		out = append(out, DescendantAction{RunID: id, Agent: agent, Kind: a.Kind, Message: a.Message})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run: descendants requiring action rows: %w", err)
	}
	return out, nil
}

// Subtree — see the Store interface (M124 orchestration view). Reads every run in the tree
// (root_run_id = rootRunID; a root run has root_run_id == id) ordered by created_at, reusing the same
// column projection + scanRunRow as Get. Read-only.
func (p *pgStore) Subtree(rootRunID string) ([]*Run, error) {
	if rootRunID == "" {
		return nil, nil
	}
	sel := `SELECT ` + runRowColumns("cursor") + ` FROM runs WHERE root_run_id=$1 ORDER BY created_at ASC, id ASC`
	rows, err := p.db.QueryContext(context.Background(), sel, rootRunID)
	if err != nil {
		return nil, fmt.Errorf("run: subtree: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Run
	for rows.Next() {
		r, _, err := scanRunRow(rows)
		if err != nil {
			return nil, fmt.Errorf("run: subtree scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run: subtree rows: %w", err)
	}
	return out, nil
}

// CountRunOutcomes returns (failed, total) run counts for one (namespace, agent) over runs CREATED at or
// after `since` (M84, AlertPolicy runFailureRate condition, ADR 0063 D2). It is the cpDB-native data
// source the reconciler evaluates the runFailureRate SLO from: rate = failed/total over the condition's
// window. `failed` counts terminal-FAILED runs (status='failed'); `total` counts every run created in the
// window (all statuses — the denominator is "runs attempted in the window", so an in-flight or succeeded
// run is part of the base rate). Read-only; it never mutates a run. A query/scan error is returned so the
// caller (the reconciler) can log + abstain — a bad read must never wedge the reconcile or fabricate a rate.
func (p *pgStore) CountRunOutcomes(ctx context.Context, namespace, agent string, since time.Time) (failed, total int, err error) {
	const q = `SELECT
			count(*) FILTER (WHERE status = $4),
			count(*)
		FROM runs
		WHERE namespace = $1 AND agent = $2 AND created_at >= $3`
	if scanErr := p.db.QueryRowContext(ctx, q, namespace, agent, since.UTC(), string(StatusFailed)).
		Scan(&failed, &total); scanErr != nil {
		return 0, 0, fmt.Errorf("run: count run outcomes: %w", scanErr)
	}
	return failed, total, nil
}

func (p *pgStore) AppendEvent(id string, kind EventKind, data string) error {
	ctx := context.Background()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("run: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the run row: proves the run exists (ErrNotFound) AND serialises seq assignment.
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE`, id).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("run: lock: %w", err)
	}
	if err := appendEventTx(ctx, tx, id, kind, data, p.clock()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("run: commit: %w", err)
	}
	return nil
}

// appendEventTx inserts one event, assigning the next per-run seq from the durable log. The
// caller MUST hold the run row lock (FOR UPDATE) so concurrent appends can't collide on seq.
func appendEventTx(ctx context.Context, tx *sql.Tx, id string, kind EventKind, data string, at time.Time) error {
	const ins = `INSERT INTO run_events (run_id, seq, kind, data, at)
		SELECT $1, COALESCE(MAX(seq),0)+1, $2, $3, $4 FROM run_events WHERE run_id=$1`
	if _, err := tx.ExecContext(ctx, ins, id, string(kind), data, at.UTC()); err != nil {
		return fmt.Errorf("run: append event: %w", err)
	}
	return nil
}

func (p *pgStore) Subscribe(id string, fromSeq int) (<-chan Event, func(), error) {
	// Confirm the run exists up front (ErrNotFound before we spawn the tailer).
	r, err := p.Get(id)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan Event, subBuffer)
	ctx, cancel := context.WithCancel(context.Background())

	// If already terminal, deliver the backlog once and close — no live events will follow.
	if r.Status.IsTerminal() {
		go func() {
			defer close(ch)
			_ = p.drain(ctx, id, fromSeq, ch)
		}()
		return ch, cancel, nil
	}

	go p.tail(ctx, id, fromSeq, ch)
	return ch, cancel, nil
}

// tail polls the durable event log for events after the cursor, delivering them live, until the
// run reaches a terminal state and the log is fully drained (then it closes the channel so the
// SSE handler ends cleanly). Correct across processes: the appender may be another pod.
func (p *pgStore) tail(ctx context.Context, id string, fromSeq int, ch chan<- Event) {
	defer close(ch)
	cursor := fromSeq
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()
	for {
		next, err := p.deliverAfter(ctx, id, cursor, ch)
		if err != nil {
			return // ctx cancelled or a DB error — end the stream
		}
		cursor = next

		// Terminal check AFTER draining: if the run is done, do one final drain (to catch the
		// state event that lands in the same tx as the terminal transition) then close.
		terminal, err := p.isTerminal(ctx, id)
		if err != nil {
			return
		}
		if terminal {
			_, _ = p.deliverAfter(ctx, id, cursor, ch)
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// drain delivers the backlog after the cursor once (used for an already-terminal run).
func (p *pgStore) drain(ctx context.Context, id string, fromSeq int, ch chan<- Event) error {
	_, err := p.deliverAfter(ctx, id, fromSeq, ch)
	return err
}

// deliverAfter fetches events with seq > cursor and sends them on ch, returning the new cursor.
func (p *pgStore) deliverAfter(ctx context.Context, id string, cursor int, ch chan<- Event) (int, error) {
	const q = `SELECT seq, kind, data, at FROM run_events WHERE run_id=$1 AND seq>$2 ORDER BY seq`
	rows, err := p.db.QueryContext(ctx, q, id, cursor)
	if err != nil {
		return cursor, fmt.Errorf("run: query events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			ev   Event
			kind string
			at   time.Time
		)
		if err := rows.Scan(&ev.Seq, &kind, &ev.Data, &at); err != nil {
			return cursor, fmt.Errorf("run: scan event: %w", err)
		}
		ev.Kind = EventKind(kind)
		ev.Time = at.UTC()
		select {
		case ch <- ev:
			cursor = ev.Seq
		case <-ctx.Done():
			return cursor, ctx.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return cursor, fmt.Errorf("run: iterate events: %w", err)
	}
	return cursor, nil
}

// isTerminal reports whether the run's persisted status is terminal.
func (p *pgStore) isTerminal(ctx context.Context, id string) (bool, error) {
	var status string
	err := p.db.QueryRowContext(ctx, `SELECT status FROM runs WHERE id=$1`, id).Scan(&status)
	if err != nil {
		return false, err
	}
	return Status(status).IsTerminal(), nil
}

func (p *pgStore) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// nullableTime maps an optional time to a driver value (nil → SQL NULL).
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// nullableString maps an empty string to SQL NULL (preserving the convention that "" means absent).
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// marshalWaitOn serialises a wait-set to JSON text (” for an empty set, so a non-waiting run's
// column stays the DEFAULT ” — the sweeper's "wait_on <> ”" filter then cheaply skips it).
func marshalWaitOn(ids []string) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("run: marshal wait_on: %w", err)
	}
	return string(b), nil
}

// unmarshalWaitOn parses the wait_on JSON text back to a slice (” → nil, the empty set).
func unmarshalWaitOn(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(s), &ids); err != nil {
		return nil, fmt.Errorf("run: unmarshal wait_on: %w", err)
	}
	return ids, nil
}

// marshalNodeEndpoints serialises the pinned node-endpoint map to JSON text (”  for an empty/absent map,
// so a non-workflow run's column stays the DEFAULT ” — the same JSON-in-text convention as wait_on).
func marshalNodeEndpoints(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("run: marshal node_endpoints: %w", err)
	}
	return string(b), nil
}

// unmarshalNodeEndpoints parses the node_endpoints JSON text back to a map (” → nil, the empty map).
func unmarshalNodeEndpoints(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("run: unmarshal node_endpoints: %w", err)
	}
	return m, nil
}

// marshalAction serialises the optional pending action (nil → NULL column).
func marshalAction(a *Action) ([]byte, error) {
	if a == nil {
		return nil, nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("run: marshal action: %w", err)
	}
	return b, nil
}

// querier is the read surface shared by *sql.DB and *sql.Tx, so getWithVersion serves both the
// pool (Get) and a locked transaction (Update).
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
