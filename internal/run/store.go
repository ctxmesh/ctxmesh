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
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when a run id is unknown to the store.
var ErrNotFound = errors.New("run: not found")

// ErrNoQueuedRun is returned by ClaimQueued/ClaimReclaimable when there is no claimable run — the
// worker backs off and polls again (it is not an error condition).
var ErrNoQueuedRun = errors.New("run: no queued run to claim")

// ErrLeaseLost is returned by Heartbeat when the run is no longer leased by the calling worker (it
// was reclaimed after the lease expired, m32.3). The worker stops working the run.
var ErrLeaseLost = errors.New("run: lease lost (reclaimed by another worker)")

// EventKind classifies a run event on the stream (ADR 0034). Phase 1 emits state + message; token
// + step events (live model output) arrive with the launcher event source (m31.4).
type EventKind string

const (
	// EventState — a status transition; Data is the new status.
	EventState EventKind = "state"
	// EventMessage — a completed assistant message; Data is the content.
	EventMessage EventKind = "message"
	// EventToken — a streamed chunk of the assistant's output (m31.4); Data is the chunk.
	EventToken EventKind = "token"
	// EventStep — a loop step / tool-call boundary (m31.4); Data is a short label.
	EventStep EventKind = "step"
	// EventDescendantAction — L1 surfacing (ADR 0075 §4): a DESCENDANT sub-run entered
	// requires_action (a delegated HITL/consent pause). Appended to the ROOT run's stream (never the
	// descendant's own) so a human watching the root sees a nested pause it must resolve; Data is the
	// paused descendant's run id. Derive-don't-denormalize: the authoritative pause lives on the
	// descendant — this is a VISIBILITY breadcrumb, not a second source of truth.
	EventDescendantAction EventKind = "descendant-requires-action"
)

// Event is one item on a run's event stream. Seq is monotonic per run (1-based) so a client can
// resume from a Last-Event-ID cursor after a reconnect.
type Event struct {
	Seq  int       `json:"seq"`
	Kind EventKind `json:"kind"`
	Data string    `json:"data,omitempty"`
	Time time.Time `json:"time"`
}

// Store persists runs + their event streams for the execution contract (ADR 0034). Phase 1 (M31)
// is a HOT store — in-process, NOT durable across a pod restart; M32 replaces the backing behind
// this same seam. Written so a durable implementation slots in without touching callers.
type Store interface {
	// Create stores a new run. It errors if the id already exists.
	Create(r *Run) error
	// Get returns a COPY of the run (callers must not mutate the store's object).
	Get(id string) (*Run, error)
	// GetByTraceID returns a COPY of the run whose TraceID matches traceID, or (nil, nil) if none
	// is found (not an error — a nil run means "not found by trace id"). Callers must not mutate
	// the returned run. This is the fallback resolution path for the share mint, which is keyed by
	// the trace-detail page's traceId rather than the internal run.ID.
	GetByTraceID(traceID string) (*Run, error)
	// Update applies fn to the stored run atomically and returns a copy. A non-nil error from fn
	// (e.g. an illegal Transition) aborts the update, leaving the run unchanged.
	Update(id string, fn func(*Run) error) (*Run, error)
	// List returns copies of all runs (unordered).
	List() []*Run
	// AppendEvent appends an event to the run's stream (assigning Seq) and broadcasts it to live
	// subscribers. Errors if the run is unknown.
	AppendEvent(id string, kind EventKind, data string) error
	// Subscribe returns a channel delivering the run's events with Seq > fromSeq: first the
	// buffered backlog, then live events. The channel is CLOSED when the run reaches a terminal
	// state and its backlog is drained (so an SSE handler ends cleanly). cancel releases the
	// subscription. Errors if the run is unknown.
	Subscribe(id string, fromSeq int) (events <-chan Event, cancel func(), err error)
	// ClaimQueued atomically leases the oldest `queued` run to workerID (transitioning it to
	// `running`, stamping the worker + a lease that expires after `lease`) and returns it, so a
	// KEDA-scaled worker pool drains the queue without two workers grabbing the same run (m32.2).
	// Returns ErrNoQueuedRun when the queue is empty. A status change auto-emits its `state` event,
	// exactly like Update.
	ClaimQueued(workerID string, lease time.Duration) (*Run, error)
	// Heartbeat renews the lease on a run still leased by workerID (and still running), extending it
	// by `lease`, so a healthy long run is not falsely reclaimed. Returns ErrLeaseLost if the run is
	// no longer leased by this worker (already reclaimed) or ErrNotFound if it is gone (m32.3).
	Heartbeat(id, workerID string, lease time.Duration) error
	// ClaimReclaimable atomically re-leases the oldest `running` run whose lease has EXPIRED (its
	// worker died) to workerID and returns it, so a live worker resumes it from its last checkpoint.
	// The run stays `running` (no state change). Returns ErrNoQueuedRun when none is reclaimable —
	// the headline resume-on-pod-loss path (m32.3).
	ClaimReclaimable(workerID string, lease time.Duration) (*Run, error)
	// ReleaseLease expires the lease on a run still leased by workerID (still running) so a peer can
	// reclaim it IMMEDIATELY (ClaimReclaimable takes an expired lease), rather than waiting a full
	// lease-TTL. It is the graceful-drain counterpart of Heartbeat: on SIGTERM a worker that will not
	// finish an in-flight run in the drain window releases it for prompt resume-on-another-worker
	// (D4). No state change (the run stays `running`, resumed from its checkpoint). A no-op (nil) when
	// the run is gone or already leased by someone else — releasing is best-effort and idempotent.
	ReleaseLease(id, workerID string) error
	// ReserveSpawn atomically increments the total-spawn counter for a spawn TREE (keyed by its ROOT
	// run id) and returns whether the reservation is within maxTotal (M64, ADR 0057). This is the
	// AUTHORITATIVE aggregate spawn-budget gate: the BFF keys it on the root it derived from the VERIFIED
	// parent run, so an agent cannot re-key it for a fresh budget (the launcher's Valkey guard reads
	// agent-supplied inputs and is only advisory). Fails CLOSED — a store error returns (false, err) so
	// the spawn is denied. Monotonic within the tree (each accepted spawn permanently consumes budget).
	ReserveSpawn(rootRunID string, maxTotal int) (bool, error)

	// Suspend parks a RUNNING run in the `waiting` state (ADR 0060 §3), waiting on the given child run
	// ids under mode (WaitAll|WaitAny). It clears the run's lease + worker (a waiting run is not
	// claimable and holds no worker), applies the optional fn under the row lock FIRST (so the executor
	// can checkpoint its cursor in the SAME transaction that suspends), and returns a copy. The
	// executor calls this AFTER launching the child(ren). Errors if the run is not `running`, if waitOn
	// is empty, or on an illegal transition. A status change auto-emits its `state` event.
	Suspend(id string, waitOn []string, mode WaitMode, fn func(*Run) error) (*Run, error)

	// SuspendOnDelegate is the L7 delegate suspend transaction (ADR 0091): in ONE OCC-guarded transaction
	// it upserts the delegate CHILD run(s) (idempotent, ON CONFLICT DO NOTHING), checkpoints the parent
	// (fn sets Cursor under the row lock), and — checking each child's CURRENT status under the lock —
	// either suspends the parent to `waiting` on the NON-terminal children (releasing its lease) OR, when
	// EVERY child is already terminal (the WaitAll wait is satisfied NOW — the lost-wakeup race), re-queues
	// the parent directly so the pool re-claims + resumes it. It NEVER parks the parent on an empty /
	// already-satisfied wait set that no future child transition would wake. mode is WaitAll for a
	// delegate fan-out (v1). Returns the parent copy. Idempotent + OCC-safe against a concurrent wake.
	SuspendOnDelegate(parentID string, children []*Run, mode WaitMode, fn func(*Run) error) (*Run, error)

	// CompleteAndWake terminates a CHILD run (via apply, which must transition it to a terminal state)
	// and, in the SAME transaction, wakes a `waiting` PARENT that is waiting on it — flipping the
	// parent waiting→queued when the wait condition is met — so the existing worker pool re-claims it
	// (ADR 0060 §3). This is the TRANSACTIONAL cross-run wake: child terminal + parent re-queue commit
	// together, so the wake is EXACTLY-ONCE by construction with no notification bus. It returns copies
	// of the child and (if woken) the parent (wokeParent is nil when there is no parent, the parent is
	// not waiting, the child is not in the parent's wait set, or the wait is not yet met). Idempotent:
	// re-completing an already-terminal child (a reclaimed completion) is a no-op that does not
	// re-queue the parent or corrupt the wait set.
	CompleteAndWake(childID string, apply func(*Run) error) (child *Run, wokeParent *Run, err error)

	// SweepWaiting is the belt-and-braces reconciler for the crash window between a child's terminal
	// transition and the parent wake (and the sole wake path for the in-mem store across a restart):
	// it finds `waiting` runs whose wait condition is ALREADY met by their children's persisted
	// terminal states and flips them waiting→queued. Idempotent (a run woken by CompleteAndWake is no
	// longer waiting, so the sweep skips it) and bounded in frequency by its caller (~30s). Returns the
	// ids it re-queued.
	SweepWaiting() ([]string, error)

	// DescendantsRequiringAction returns the DESCENDANT sub-runs of rootRunID currently paused in
	// requires_action (L1 surfacing, ADR 0075 §4) — derive-don't-denormalize: a `root_run_id=$1 AND
	// status='requires_action'` read, so a human watching a root run can see (and navigate to) a
	// delegated HITL/consent pause anywhere in its subtree. Descendant rows carry root_run_id = the
	// TRUE root's id, so keying on a mid-tree run returns nothing (only the root the human watches
	// surfaces the subtree's pauses). Read-only; a nil slice + nil error means "none paused".
	DescendantsRequiringAction(rootRunID string) ([]DescendantAction, error)

	// Subtree returns EVERY run in the tree rooted at rootRunID — the root run plus all its descendant
	// sub-runs (delegate children and their nested descendants) — for the orchestration / run-tree view
	// (M124). Derive-don't-denormalize: a `root_run_id=$1` read (a root run carries root_run_id == id,
	// so descendants + the root all match), ordered by created_at so the caller can render the delegation
	// timeline and assemble the parent→child tree from ParentRunID. Read-only; a nil slice means "no tree"
	// (an unknown root, or a run that isn't a supervisor). Keying on a mid-tree run returns nothing — the
	// tree is keyed by the TRUE root's id (same discipline as DescendantsRequiringAction).
	Subtree(rootRunID string) ([]*Run, error)

	// ListWaitingApproval returns the runs in namespace currently paused in requires_action whose Kind is
	// in `kinds` (M75 approvalWaiting passes {plan_approval}; the M113 V5 unified console queue passes the
	// caller-authorized subset of {plan_approval, approval} — consent_required is owner-only, never here),
	// most-recently-updated first. limit>0 bounds the scan (0 = unbounded). The projection carries Kind
	// (the plan-vs-step badge), Namespace, WaitingSince (the pause time), RootRunID (tree context for a
	// paused descendant), and CallerUsername (the BFF's inline-workflow owner filter — never sent to a
	// client). Read-only; a nil/empty `kinds` returns nothing.
	ListWaitingApproval(ctx context.Context, namespace string, kinds []ActionKind, limit int) ([]WaitingApproval, error)

	// ListByEndUser returns the runs OWNED by callerUsername at (namespace, agent), most-recently-updated
	// first — the end-user "my runs" list (M137/EU1c, ADR 0107). The mandatory
	// `WHERE caller_username=$1 AND namespace=$2 AND agent=$3` is the ISOLATION BOUNDARY, not a
	// client-supplied filter: a verified end-user principal (oidc:<iss>#<sub>) can never see another
	// principal's runs, and the (namespace, agent) is HOST-derived so they cannot enumerate their runs on
	// a different agent. A blank callerUsername returns nothing (fail-CLOSED — never a list-all). limit>0
	// bounds the page. Read-only; a query/scan error is returned.
	ListByEndUser(ctx context.Context, callerUsername, namespace, agent string, limit int) ([]EndUserRun, error)
}

// EndUserRun is the projection for an end-user's "my runs" list (M137/EU1c, ADR 0107): a run the verified
// end-user principal OWNS at a specific (namespace, agent). Just enough to render the list and deep-link
// into a run they already own — deliberately NO Input/Messages (that is the run-detail read, itself
// ownership-gated). Ordered most-recently-updated first.
type EndUserRun struct {
	ID             string    `json:"id"`
	Status         Status    `json:"status"`
	ConversationID string    `json:"conversationId,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// DescendantAction is the L1-surfacing projection of a descendant sub-run paused in requires_action
// (ADR 0075 §4): just enough for the root's watcher to render + link to the nested pause — the run id,
// its agent, the pause kind (consent / approval / plan_approval), and the human-facing message. The
// authoritative Action still lives on the descendant run (this is a visibility breadcrumb).
type DescendantAction struct {
	RunID   string     `json:"runId"`
	Agent   string     `json:"agent"`
	Kind    ActionKind `json:"kind"`
	Message string     `json:"message,omitempty"`
}

// subBuffer bounds a subscriber's live channel; a consumer slower than this is dropped (its
// channel closed) and expected to reconnect with a Last-Event-ID cursor (SSE convention).
const subBuffer = 256

type subscriber struct {
	ch      chan Event
	fromSeq int
}

type entry struct {
	run     *Run
	events  []Event
	subs    map[int]*subscriber
	nextSub int
}

// memStore is the hot in-memory Store. Safe for concurrent use.
type memStore struct {
	mu       sync.Mutex
	entries  map[string]*entry
	spawnCnt map[string]int // rootRunID -> accepted spawns (M64 aggregate budget)
}

// NewMemStore returns a hot in-memory run store.
func NewMemStore() Store {
	return &memStore{entries: map[string]*entry{}, spawnCnt: map[string]int{}}
}

// Durable reports that the hot mem store is NOT durable across a pod restart (M75, m75.1, ADR 0069 §1).
// The share-link mint refuses to create a public link into a non-durable store — a share whose backing
// run vanishes on restart is a broken link. This satisfies the optional DurableStore capability the BFF
// type-asserts (a mem store answers false; the Postgres store answers true) without widening Store.
func (m *memStore) Durable() bool { return false }

// ReserveSpawn increments the tree's total-spawn count and admits when it is within maxTotal.
func (m *memStore) ReserveSpawn(rootRunID string, maxTotal int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.spawnCnt[rootRunID] + 1
	if next > maxTotal {
		return false, nil // over budget — do NOT record the rejected spawn
	}
	m.spawnCnt[rootRunID] = next
	return true, nil
}

// Suspend parks a running run in `waiting` on the given children (the mem twin of pgStore.Suspend).
func (m *memStore) Suspend(id string, waitOn []string, mode WaitMode, fn func(*Run) error) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return nil, ErrNotFound
	}
	oldStatus := e.run.Status
	working := cloneRun(e.run)
	if fn != nil {
		if err := fn(working); err != nil {
			return nil, err
		}
	}
	if err := working.suspendToWaiting(waitOn, mode, time.Now()); err != nil {
		return nil, err
	}
	e.run = working
	if working.Status != oldStatus {
		m.appendLocked(e, EventState, string(working.Status))
	}
	return cloneRun(working), nil
}

// SuspendOnDelegate is the mem twin of the pgStore L7 delegate suspend transaction (ADR 0091). The single
// map lock (m.mu) IS the transaction boundary: child upsert + parent checkpoint + suspend/requeue apply
// together. The lost-wakeup guard (an already-terminal child is never waited on) is identical.
func (m *memStore) SuspendOnDelegate(parentID string, children []*Run, mode WaitMode, fn func(*Run) error) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// (1) Upsert the delegate child run(s) — idempotent (a re-issued delegate leaves the existing row).
	for _, c := range children {
		if _, ok := m.entries[c.ID]; !ok {
			m.entries[c.ID] = &entry{run: cloneRun(c), subs: map[int]*subscriber{}}
		}
	}

	// (2) The wait set is only the NON-terminal children (the lost-wakeup guard — never park on a dead child).
	waitOn := make([]string, 0, len(children))
	for _, c := range children {
		ce, ok := m.entries[c.ID]
		if !ok {
			return nil, ErrNotFound
		}
		if !ce.run.Status.IsTerminal() {
			waitOn = append(waitOn, c.ID)
		}
	}

	// (3) Checkpoint the parent (fn sets Cursor) + suspend-or-requeue.
	pe, ok := m.entries[parentID]
	if !ok {
		return nil, ErrNotFound
	}
	oldStatus := pe.run.Status
	working := cloneRun(pe.run)
	if fn != nil {
		if err := fn(working); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	if len(waitOn) == 0 {
		// All children terminal ⇒ the WaitAll wait is met now: running→waiting→queued (release the lease),
		// never park on an empty wait set (ADR 0091 fork 2).
		if err := working.Transition(StatusWaiting, now); err != nil {
			return nil, err
		}
		working.WorkerID, working.LeaseExpiresAt = "", nil
		if err := working.Transition(StatusQueued, now); err != nil {
			return nil, err
		}
	} else if err := working.suspendToWaiting(waitOn, mode, now); err != nil {
		return nil, err
	}
	pe.run = working
	if working.Status != oldStatus {
		m.appendLocked(pe, EventState, string(working.Status))
	}
	return cloneRun(working), nil
}

// CompleteAndWake terminates a child and wakes a waiting parent in one critical section — the mem
// twin of the pgStore two-row transaction. The single map lock (m.mu) IS the transaction boundary:
// child-terminal + parent-requeue are applied together, so the wake is atomic + exactly-once.
func (m *memStore) CompleteAndWake(childID string, apply func(*Run) error) (*Run, *Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ce, ok := m.entries[childID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	// Idempotency guard: an already-terminal child is a no-op (a reclaimed completion). Do NOT
	// re-run apply (it would fail the state machine anyway) and do NOT re-wake the parent.
	if ce.run.Status.IsTerminal() {
		return cloneRun(ce.run), nil, nil
	}
	childStatus := ce.run.Status
	child := cloneRun(ce.run)
	if err := apply(child); err != nil {
		return nil, nil, err
	}
	if !child.Status.IsTerminal() {
		return nil, nil, fmt.Errorf("run %s: CompleteAndWake apply must reach a terminal state, got %s", childID, child.Status)
	}
	ce.run = child
	if child.Status != childStatus {
		m.appendLocked(ce, EventState, string(child.Status))
	}
	if child.Status.IsTerminal() {
		for sid, sub := range ce.subs {
			close(sub.ch)
			delete(ce.subs, sid)
		}
	}

	// Wake the waiting parent, if any, in the SAME critical section.
	var wokeParent *Run
	if child.ParentRunID != "" {
		if pe, ok := m.entries[child.ParentRunID]; ok && pe.run.Status == StatusWaiting {
			parent := cloneRun(pe.run)
			met, removed := parent.satisfyChild(childID, child.Status)
			if removed {
				pOld := parent.Status
				if met {
					if err := parent.Transition(StatusQueued, time.Now()); err != nil {
						return nil, nil, err
					}
				}
				pe.run = parent
				if parent.Status != pOld {
					m.appendLocked(pe, EventState, string(parent.Status))
					wokeParent = cloneRun(parent)
				}
			}
		}
	}
	return cloneRun(child), wokeParent, nil
}

// SweepWaiting re-queues waiting runs whose children are all-terminal-enough per the mode (the mem
// twin of the pgStore sweeper — the crash-window / cross-restart safety net).
func (m *memStore) SweepWaiting() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var woke []string
	for _, e := range m.entries {
		if e.run.Status != StatusWaiting || len(e.run.WaitOn) == 0 {
			continue
		}
		if !m.waitMetLocked(e.run) {
			continue
		}
		working := cloneRun(e.run)
		if err := working.Transition(StatusQueued, time.Now()); err != nil {
			return woke, err
		}
		e.run = working
		m.appendLocked(e, EventState, string(StatusQueued))
		woke = append(woke, working.ID)
	}
	return woke, nil
}

// waitMetLocked is the mem-store sweep adapter: it gathers the run's WaitOn children's CURRENT persisted
// statuses (a MISSING child row → StatusCancelled, per waitSatisfied's contract — a non-success terminal,
// never a success) and defers the decision to the SINGLE predicate waitSatisfied. No second copy of the
// mode logic lives here. Caller holds m.mu.
func (m *memStore) waitMetLocked(r *Run) bool {
	statuses := make([]Status, len(r.WaitOn))
	for i, cid := range r.WaitOn {
		if ce, ok := m.entries[cid]; ok {
			statuses[i] = ce.run.Status
		} else {
			statuses[i] = StatusCancelled // a missing child ⇒ cancelled-equivalent (never a success)
		}
	}
	return waitSatisfied(r.WaitMode, statuses)
}

func (m *memStore) Create(r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[r.ID]; ok {
		return errors.New("run: id already exists")
	}
	m.entries[r.ID] = &entry{run: cloneRun(r), subs: map[int]*subscriber{}}
	return nil
}

func (m *memStore) Get(id string) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneRun(e.run), nil
}

func (m *memStore) GetByTraceID(traceID string) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.run.TraceID == traceID {
			return cloneRun(e.run), nil
		}
	}
	return nil, nil // not found — not an error
}

func (m *memStore) Update(id string, fn func(*Run) error) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return nil, ErrNotFound
	}
	oldStatus := e.run.Status
	working := cloneRun(e.run)
	if err := fn(working); err != nil {
		return nil, err
	}
	e.run = working
	// A status change automatically emits a `state` event — the stream's state transitions come
	// from the ONE place the state changes, so a caller can't forget to emit one.
	if working.Status != oldStatus {
		m.appendLocked(e, EventState, string(working.Status))
	}
	// A run that has reached a terminal state closes any idle subscribers so their SSE handlers
	// end (the backlog — incl. the terminal state event above — was already delivered).
	if working.Status.IsTerminal() {
		for sid, sub := range e.subs {
			close(sub.ch)
			delete(e.subs, sid)
		}
	}
	return cloneRun(working), nil
}

func (m *memStore) ClaimQueued(workerID string, lease time.Duration) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Oldest queued run first (FIFO by CreatedAt), so the hot store matches the durable store's
	// ORDER BY created_at claim order.
	var pick *entry
	for _, e := range m.entries {
		if e.run.Status != StatusQueued {
			continue
		}
		if pick == nil || e.run.CreatedAt.Before(pick.run.CreatedAt) {
			pick = e
		}
	}
	if pick == nil {
		return nil, ErrNoQueuedRun
	}
	now := time.Now()
	exp := now.Add(lease)
	pick.run.WorkerID = workerID
	pick.run.LeaseExpiresAt = &exp
	if err := pick.run.Transition(StatusRunning, now); err != nil {
		return nil, err
	}
	m.appendLocked(pick, EventState, string(StatusRunning))
	return cloneRun(pick.run), nil
}

func (m *memStore) Heartbeat(id, workerID string, lease time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return ErrNotFound
	}
	if e.run.WorkerID != workerID {
		return ErrLeaseLost
	}
	exp := time.Now().Add(lease)
	e.run.LeaseExpiresAt = &exp
	return nil
}

// ReleaseLease expires this worker's lease so ClaimReclaimable re-leases the run immediately (D4).
// Best-effort + idempotent: a run that is gone, terminal, or held by another worker matches nothing
// and is a no-op (nil), mirroring the pg store.
func (m *memStore) ReleaseLease(id, workerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok || e.run.Status != StatusRunning || e.run.WorkerID != workerID {
		return nil
	}
	expired := time.Now().Add(-time.Second)
	e.run.LeaseExpiresAt = &expired
	return nil
}

func (m *memStore) ClaimReclaimable(workerID string, lease time.Duration) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	// Oldest running run whose lease has expired (its worker died). Stays running — a reclaim is a
	// re-lease, not a state change, so the resumed stream continues rather than restarting.
	var pick *entry
	for _, e := range m.entries {
		if e.run.Status != StatusRunning {
			continue
		}
		if e.run.LeaseExpiresAt == nil || !e.run.LeaseExpiresAt.Before(now) {
			continue
		}
		if pick == nil || e.run.CreatedAt.Before(pick.run.CreatedAt) {
			pick = e
		}
	}
	if pick == nil {
		return nil, ErrNoQueuedRun
	}
	exp := now.Add(lease)
	pick.run.WorkerID = workerID
	pick.run.LeaseExpiresAt = &exp
	pick.run.Attempts++ // F-5: increment on reclaim (parity with the pg store)
	return cloneRun(pick.run), nil
}

func (m *memStore) List() []*Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Run, 0, len(m.entries))
	for _, e := range m.entries {
		r := cloneRun(e.run)
		// L12: a list fill does not carry the (MB-scale, L7 supervisor-checkpoint) cursor — list consumers
		// (the cancel-cascade walk) never read it, and this keeps the observable List contract identical to
		// the durable store, which projects '' for cursor to avoid dragging every checkpoint across the wire.
		r.Cursor = ""
		out = append(out, r)
	}
	return out
}

// DescendantsRequiringAction — see the Store interface (L1 surfacing, ADR 0075 §4).
func (m *memStore) DescendantsRequiringAction(rootRunID string) ([]DescendantAction, error) {
	if rootRunID == "" {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DescendantAction
	for _, e := range m.entries {
		r := e.run
		if r.RootRunID != rootRunID || r.Status != StatusRequiresAction || r.RequiresAction == nil {
			continue
		}
		out = append(out, DescendantAction{
			RunID:   r.ID,
			Agent:   r.Agent,
			Kind:    r.RequiresAction.Kind,
			Message: r.RequiresAction.Message,
		})
	}
	return out, nil
}

// Subtree — see the Store interface (M124 orchestration view). Returns every run whose RootRunID matches
// (a root run also matches by id), cloned + ordered by CreatedAt so the caller can assemble the tree.
func (m *memStore) Subtree(rootRunID string) ([]*Run, error) {
	if rootRunID == "" {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Run
	for _, e := range m.entries {
		if e.run.RootRunID == rootRunID || e.run.ID == rootRunID {
			out = append(out, cloneRun(e.run))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// ListWaitingApproval — see the Store interface (M75 approvalWaiting + M113 V5 unified queue). The mem
// twin mirrors the pgStore: filter to the given kinds, newest-updated first, limit>0 bounds the result.
func (m *memStore) ListWaitingApproval(_ context.Context, namespace string, kinds []ActionKind, limit int) ([]WaitingApproval, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	type row struct {
		wa      WaitingApproval
		updated time.Time
	}
	var rows []row
	for _, e := range m.entries {
		r := e.run
		if r.Namespace != namespace || r.Status != StatusRequiresAction || r.RequiresAction == nil {
			continue
		}
		if !slices.Contains(kinds, r.RequiresAction.Kind) {
			continue
		}
		rows = append(rows, row{
			wa: WaitingApproval{
				ID: r.ID, Agent: r.Agent, Message: r.RequiresAction.Message, Kind: r.RequiresAction.Kind,
				RootRunID: r.RootRunID, CallerUsername: r.CallerUsername,
				Namespace: r.Namespace, WaitingSince: r.UpdatedAt.UTC(),
			},
			updated: r.UpdatedAt,
		})
	}
	slices.SortFunc(rows, func(a, b row) int { return b.updated.Compare(a.updated) }) // newest first
	out := make([]WaitingApproval, 0, len(rows))
	for _, rw := range rows {
		out = append(out, rw.wa)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListByEndUser — see the Store interface (M137/EU1c, ADR 0107). Ownership-scoped my-runs list.
func (m *memStore) ListByEndUser(_ context.Context, callerUsername, namespace, agent string, limit int) ([]EndUserRun, error) {
	if callerUsername == "" || namespace == "" || agent == "" {
		return nil, nil // fail-closed: a blank identity/host never lists runs
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []EndUserRun
	for _, e := range m.entries {
		r := e.run
		if r.CallerUsername != callerUsername || r.Namespace != namespace || r.Agent != agent {
			continue
		}
		out = append(out, EndUserRun{
			ID: r.ID, Status: r.Status, ConversationID: r.ConversationID,
			CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
		})
	}
	slices.SortFunc(out, func(a, b EndUserRun) int { return b.UpdatedAt.Compare(a.UpdatedAt) }) // newest first
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memStore) AppendEvent(id string, kind EventKind, data string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return ErrNotFound
	}
	m.appendLocked(e, kind, data)
	return nil
}

// appendLocked appends an event to the entry's log (assigning Seq) and broadcasts it to live
// subscribers. The caller MUST hold m.mu. A slow subscriber (full buffer) is dropped — it
// reconnects with a Last-Event-ID cursor and replays from the log.
func (m *memStore) appendLocked(e *entry, kind EventKind, data string) {
	ev := Event{Seq: len(e.events) + 1, Kind: kind, Data: data, Time: time.Now()}
	e.events = append(e.events, ev)
	for sid, sub := range e.subs {
		select {
		case sub.ch <- ev:
		default:
			close(sub.ch)
			delete(e.subs, sid)
		}
	}
}

func (m *memStore) Subscribe(id string, fromSeq int) (<-chan Event, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return nil, nil, ErrNotFound
	}
	ch := make(chan Event, subBuffer)
	// Deliver the backlog (events after the cursor) synchronously so no event is missed between
	// a Get and the Subscribe. If that already overflows the buffer the caller is replaying a
	// huge log — fall back to closing (the consumer reconnects with a later cursor).
	overflow := false
	for _, ev := range e.events {
		if ev.Seq <= fromSeq {
			continue
		}
		select {
		case ch <- ev:
		default:
			overflow = true
		}
		if overflow {
			break
		}
	}
	// If the run is already terminal (or the backlog overflowed), close after the backlog — no
	// live events will follow, so the SSE handler ends.
	if overflow || e.run.Status.IsTerminal() {
		close(ch)
		return ch, func() {}, nil
	}
	sid := e.nextSub
	e.nextSub++
	e.subs[sid] = &subscriber{ch: ch, fromSeq: fromSeq}
	cancel := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if sub, ok := e.subs[sid]; ok {
			close(sub.ch)
			delete(e.subs, sid)
		}
	}
	return ch, cancel, nil
}

// cloneRun returns a deep-enough copy so a returned run can be read/mutated by a caller without
// racing the store's copy (slices + the Action pointer are copied, not aliased).
func cloneRun(r *Run) *Run {
	c := *r
	if r.Messages != nil {
		c.Messages = append([]Message(nil), r.Messages...)
	}
	if r.Input != nil {
		c.Input = append([]byte(nil), r.Input...)
	}
	if r.RequiresAction != nil {
		a := *r.RequiresAction
		if r.RequiresAction.Servers != nil {
			a.Servers = append([]string(nil), r.RequiresAction.Servers...)
		}
		c.RequiresAction = &a
	}
	if r.LeaseExpiresAt != nil {
		t := *r.LeaseExpiresAt
		c.LeaseExpiresAt = &t
	}
	if r.WaitOn != nil {
		c.WaitOn = append([]string(nil), r.WaitOn...)
	}
	if r.NodeEndpoints != nil {
		c.NodeEndpoints = make(map[string]string, len(r.NodeEndpoints))
		maps.Copy(c.NodeEndpoints, r.NodeEndpoints)
	}
	return &c
}
