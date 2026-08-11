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
-- Walk a spawn tree (audit / the console's parent→sub-run view) by its root.
CREATE INDEX IF NOT EXISTS runs_root ON runs (root_run_id) WHERE root_run_id <> '';
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
	ctx := context.Background()
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
	nodeEndpoints, err := marshalNodeEndpoints(r.NodeEndpoints)
	if err != nil {
		return err
	}
	const q = `INSERT INTO runs
		(id, namespace, agent, input, conversation_id, trace_id, status, messages, requires_action, error,
		 caller_username, boundary, endpoint, worker_id, lease_expires_at,
		 parent_run_id, root_run_id, spawn_depth, output_schema,
		 workflow_ref, spec_snapshot, cursor, wait_on, wait_mode, handed_off_to, handoff_source_run_id,
		 node_endpoints, ingestion_ref, ingestion_spec, outcome, export_ref, export_spec, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,1,$33,$34)
		ON CONFLICT (id) DO NOTHING`
	res, err := p.db.ExecContext(ctx, q,
		r.ID, r.Namespace, r.Agent, []byte(r.Input), r.ConversationID, r.TraceID,
		string(r.Status), msgs, action, r.Error,
		r.CallerUsername, r.Boundary, r.Endpoint, r.WorkerID, nullableTime(r.LeaseExpiresAt),
		r.ParentRunID, r.RootRunID, r.SpawnDepth, nullableString(r.OutputSchema),
		r.WorkflowRef, r.SpecSnapshot, r.Cursor, waitOn, string(r.WaitMode), r.HandedOffTo, r.HandoffSourceRunID,
		nodeEndpoints, r.IngestionRef, r.IngestionSpec, r.Outcome, r.ExportRef, r.ExportSpec, r.CreatedAt.UTC(), r.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("run: insert: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("run: id already exists")
	}
	return nil
}

func (p *pgStore) Get(id string) (*Run, error) {
	r, _, err := p.getWithVersion(context.Background(), p.db, id)
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

// getWithVersion loads a run + its OCC version from the given querier (the pool or a tx). A
// missing row is ErrNotFound.
func (p *pgStore) getWithVersion(ctx context.Context, q querier, id string) (*Run, int64, error) {
	const sel = `SELECT namespace, agent, input, conversation_id, trace_id, status, messages, requires_action, error,
		caller_username, boundary, endpoint, worker_id, lease_expires_at,
		parent_run_id, root_run_id, spawn_depth, output_schema,
		workflow_ref, spec_snapshot, cursor, wait_on, wait_mode, handed_off_to, handoff_source_run_id,
		node_endpoints, ingestion_ref, ingestion_spec, outcome, export_ref, export_spec, version, created_at, updated_at
		FROM runs WHERE id=$1`
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
	err := q.QueryRowContext(ctx, sel, id).Scan(
		&r.Namespace, &r.Agent, &input, &r.ConversationID, &r.TraceID, &status,
		&msgs, &action, &r.Error, &r.CallerUsername, &r.Boundary, &r.Endpoint, &r.WorkerID, &lease,
		&r.ParentRunID, &r.RootRunID, &r.SpawnDepth, &outputSchema,
		&r.WorkflowRef, &r.SpecSnapshot, &r.Cursor, &waitOn, &waitMode, &r.HandedOffTo, &r.HandoffSourceRunID,
		&nodeEndpoints, &r.IngestionRef, &r.IngestionSpec, &r.Outcome, &r.ExportRef, &r.ExportSpec, &version, &created, &updated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, 0, ErrNotFound
	case err != nil:
		return nil, 0, fmt.Errorf("run: select: %w", err)
	}
	r.ID = id
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
	if _, err := lockRunsOrdered(ctx, tx, lockIDs); err != nil {
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
			met, removed := parent.satisfyChild(childID)
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

// waitMet reports whether a waiting run's condition is met by its children's persisted statuses.
// all → every WaitOn child is terminal (a missing child counts as satisfied — it can never wake us);
// any → at least one is terminal. Read on the pool (advisory); the caller re-checks under the lock.
func (p *pgStore) waitMet(ctx context.Context, r *Run) (bool, error) {
	terminal := 0
	for _, cid := range r.WaitOn {
		var status string
		err := p.db.QueryRowContext(ctx, `SELECT status FROM runs WHERE id=$1`, cid).Scan(&status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			terminal++ // missing child → satisfied
		case err != nil:
			return false, fmt.Errorf("run: sweep child status: %w", err)
		default:
			if Status(status).IsTerminal() {
				terminal++
			}
		}
	}
	if r.WaitMode == WaitAny {
		return terminal > 0, nil
	}
	return terminal == len(r.WaitOn), nil
}

// lockRunsOrdered acquires FOR UPDATE row locks on the given run ids in a single statement, ordered
// by id, so all callers touching an overlapping set take the locks in the SAME order (deadlock-free).
// It errors ErrNotFound if any id is missing (all rows must exist to lock the pair coherently).
func lockRunsOrdered(ctx context.Context, tx *sql.Tx, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
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
		return 0, fmt.Errorf("run: lock ordered: %w", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("run: lock scan: %w", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("run: lock iterate: %w", err)
	}
	if n != len(ids) {
		return n, ErrNotFound // a row we needed to lock is gone
	}
	return n, nil
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
	rows, err := p.db.QueryContext(ctx, `SELECT id FROM runs`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil
		}
		ids = append(ids, id)
	}
	out := make([]*Run, 0, len(ids))
	for _, id := range ids {
		if r, err := p.Get(id); err == nil {
			out = append(out, r)
		}
	}
	return out
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
