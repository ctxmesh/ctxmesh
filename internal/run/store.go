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
	"errors"
	"fmt"
	"maps"
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
			met, removed := parent.satisfyChild(childID)
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

// waitMetLocked reports whether a waiting run's condition is met by its children's CURRENT statuses.
// Caller holds m.mu. all → every WaitOn child is terminal (or gone); any → at least one is.
func (m *memStore) waitMetLocked(r *Run) bool {
	terminalCount := 0
	for _, cid := range r.WaitOn {
		ce, ok := m.entries[cid]
		// A missing child cannot be waited on further — treat it as satisfied (it will never wake us).
		if !ok || ce.run.Status.IsTerminal() {
			terminalCount++
		}
	}
	if r.WaitMode == WaitAny {
		return terminalCount > 0
	}
	return terminalCount == len(r.WaitOn) // WaitAll
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
	return cloneRun(pick.run), nil
}

func (m *memStore) List() []*Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Run, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, cloneRun(e.run))
	}
	return out
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
