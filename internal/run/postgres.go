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
-- Claim the oldest queued run fast (the worker's FOR UPDATE SKIP LOCKED path, m32.2).
CREATE INDEX IF NOT EXISTS runs_queued ON runs (created_at) WHERE status = 'queued';`

// NewPostgresStore opens a durable Postgres-backed run store over an open *sql.DB, applying the
// schema idempotently. The caller owns the DB's lifecycle (open/close), matching credstore.
func NewPostgresStore(ctx context.Context, db *sql.DB) (Store, error) {
	if _, err := db.ExecContext(ctx, runSchemaDDL); err != nil {
		return nil, fmt.Errorf("run: apply schema: %w", err)
	}
	return &pgStore{db: db, pollInterval: 250 * time.Millisecond, now: time.Now}, nil
}

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
	const q = `INSERT INTO runs
		(id, namespace, agent, input, conversation_id, trace_id, status, messages, requires_action, error,
		 caller_username, boundary, endpoint, worker_id, lease_expires_at, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,1,$16,$17)
		ON CONFLICT (id) DO NOTHING`
	res, err := p.db.ExecContext(ctx, q,
		r.ID, r.Namespace, r.Agent, []byte(r.Input), r.ConversationID, r.TraceID,
		string(r.Status), msgs, action, r.Error,
		r.CallerUsername, r.Boundary, r.Endpoint, r.WorkerID, nullableTime(r.LeaseExpiresAt),
		r.CreatedAt.UTC(), r.UpdatedAt.UTC())
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

// getWithVersion loads a run + its OCC version from the given querier (the pool or a tx). A
// missing row is ErrNotFound.
func (p *pgStore) getWithVersion(ctx context.Context, q querier, id string) (*Run, int64, error) {
	const sel = `SELECT namespace, agent, input, conversation_id, trace_id, status, messages, requires_action, error,
		caller_username, boundary, endpoint, worker_id, lease_expires_at, version, created_at, updated_at
		FROM runs WHERE id=$1`
	var (
		r       Run
		input   []byte
		status  string
		msgs    []byte
		action  []byte
		lease   sql.NullTime
		version int64
		created time.Time
		updated time.Time
	)
	err := q.QueryRowContext(ctx, sel, id).Scan(
		&r.Namespace, &r.Agent, &input, &r.ConversationID, &r.TraceID, &status,
		&msgs, &action, &r.Error, &r.CallerUsername, &r.Boundary, &r.Endpoint, &r.WorkerID, &lease,
		&version, &created, &updated)
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
	const upd = `UPDATE runs SET
			trace_id=$2, status=$3, messages=$4, requires_action=$5, error=$6,
			worker_id=$7, lease_expires_at=$8, version=version+1, updated_at=$9
		WHERE id=$1 AND version=$10`
	res, err := tx.ExecContext(ctx, upd,
		id, working.TraceID, string(working.Status), msgs, action, working.Error,
		working.WorkerID, nullableTime(working.LeaseExpiresAt), working.UpdatedAt.UTC(), version)
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
