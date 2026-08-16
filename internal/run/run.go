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

// Package run models a durable, streamable agent RUN — the execution contract of ADR 0034.
// A run is a first-class object with a state machine and an event stream, so the same
// invocation supports streaming, long-running, resumable, and human-in-the-loop execution.
// Synchronous /invoke is sugar over a run (create + wait). Phase 1 (M31) is the object + state
// machine + a HOT store (in-cluster, not durable-across-restart — that is M32).
package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// SpawnRunID derives a DETERMINISTIC sub-run id from the spawn key (parentRunID, step, callID) —
// ADR 0057's idempotency mechanism. A reclaimed supervisor (at-least-once, m32.3) that re-issues the
// SAME delegate_to call computes the SAME id, so the store's `ON CONFLICT (id) DO NOTHING` collapses the
// duplicate to one sub-run instead of double-spawning. `step` + `callID` disambiguate multiple
// delegations within one supervisor step (a fan-out). The `sub-` prefix makes a spawned run recognizable.
func SpawnRunID(parentRunID, step, callID string) string {
	sum := sha256.Sum256([]byte(parentRunID + "\x00" + step + "\x00" + callID))
	return "sub-" + hex.EncodeToString(sum[:16])
}

// HandoffRunID derives a DETERMINISTIC id for the run B that a handoff creates (M67, ADR 0060 §5), so a
// retried handoff (the SDK re-issuing the SAME handoff_to, or a capability replay) resolves the SAME
// run B — the store's ON CONFLICT (id) DO NOTHING collapses the duplicate to one transferred run rather
// than spawning a second B. The key is (sourceRunID=A, targetAgent): one handoff per (A, B) is the
// transfer. The `hand-` prefix (distinct from the `sub-` spawn prefix) makes a transferred run
// recognizable and marks it as NOT a sub-run — a handoff is a NEW ROOT run, never a child.
func HandoffRunID(sourceRunID, targetAgent string) string {
	sum := sha256.Sum256([]byte(sourceRunID + "\x00handoff\x00" + targetAgent))
	return "hand-" + hex.EncodeToString(sum[:16])
}

// Status is the run's lifecycle state. The set + transitions mirror the A2A task states and the
// OpenAI Assistants run statuses (ADR 0034), so external clients and the mesh interoperate.
type Status string

const (
	// StatusQueued — created, not yet picked up for execution.
	StatusQueued Status = "queued"
	// StatusRunning — actively executing (a worker/loop is driving it).
	StatusRunning Status = "running"
	// StatusRequiresAction — paused pending an out-of-band action, then a resume: an OBO
	// consent (the m25.9 consent_required, generalised) or a human-in-the-loop approval (M32).
	// It is the ONE HUMAN-INPUT pause state (ADR 0034 as amended by ADR 0060): a human resolves
	// it — the console banner, A2A input-required, ops alerts. Distinct from StatusWaiting, which
	// is machine-woken.
	StatusRequiresAction Status = "requires_action"
	// StatusWaiting — paused parked on one or more CHILD RUNS, MACHINE-woken (vs requires_action =
	// human-woken), per ADR 0060 §3. A workflow instance run (m67.3) or a suspending supervisor (I1)
	// enters `waiting` after launching child(ren); when the wait condition is met the store flips it
	// back to `queued` (resume) in the SAME transaction as the last child's terminal transition —
	// exactly-once, no notification bus. A `waiting` run holds NO lease and NO worker (it is not
	// claimed while parked; it re-enters execution only via waiting→queued). NON-terminal.
	StatusWaiting Status = "waiting"
	// StatusSucceeded — terminal: produced a final answer.
	StatusSucceeded Status = "succeeded"
	// StatusFailed — terminal: an error the run surfaces (never a swallowed failure).
	StatusFailed Status = "failed"
	// StatusCancelled — terminal: cancelled by the caller.
	StatusCancelled Status = "cancelled"
	// StatusExpired — terminal: exceeded its lifetime bound before completing.
	StatusExpired Status = "expired"
)

// IsWorkflowInstance reports whether this run EXECUTES a declarative Workflow (M67, ADR 0060) — i.e. it
// carries a pinned SpecSnapshot. The run-worker routes such a run to the workflow executor (executeWorkflow)
// instead of the single-agent executeRun. WorkflowRef alone (a name with no resolved snapshot) is NOT a
// workflow instance: the snapshot is the pinned graph the executor drives, so its presence is the gate.
func (r *Run) IsWorkflowInstance() bool {
	return r.SpecSnapshot != ""
}

// IsIngestionJob reports whether this run is a KNOWLEDGE-BASE INGESTION job (M68, ADR 0061 Fork 2) — i.e. it
// carries an IngestionRef (the KnowledgeBase name it ingests). The run-worker routes such a run to the ingestion
// executor (executeIngestion) instead of the single-agent executeRun or the workflow executor. This mirrors the
// M67 typed-marker precedent (IsWorkflowInstance = SpecSnapshot != ""): there is NO generic run `kind` column, so
// the presence of the marker field IS the dispatch gate. A run is EITHER a workflow OR an ingestion, never both —
// each reuses the shared Cursor for its own resume progress (ADR 0061: verification #5's correction).
func (r *Run) IsIngestionJob() bool {
	return r.IngestionRef != ""
}

// IsDatasetExportJob reports whether this run is a DATASET EXPORT job (M69, ADR 0062 Fork 1) — i.e. it carries
// an ExportRef (the dataset name it exports into). The run-worker routes such a run to the dataset-export
// executor (executeDatasetExport) instead of the single-agent executeRun, the workflow executor, or the
// ingestion executor. This mirrors the M67/M68 typed-marker precedent (IsWorkflowInstance = SpecSnapshot != "",
// IsIngestionJob = IngestionRef != ""): there is NO generic run `kind` column, so the presence of the marker
// field IS the dispatch gate. A run is EXACTLY ONE of workflow / ingestion / export — never two — and each
// reuses the shared Cursor for its own resume progress (ADR 0062 Fork 1: export paginates Langfuse, cursor per page).
func (r *Run) IsDatasetExportJob() bool {
	return r.ExportRef != ""
}

// IsSpawned reports whether this run is a SUB-RUN of another (a workflow node or a supervisor delegation) —
// it has a parent. A spawned run's terminal transition goes through CompleteAndWake so a `waiting` parent
// (a suspended workflow run) is woken in the same transaction (ADR 0060 §3).
func (r *Run) IsSpawned() bool {
	return r.ParentRunID != ""
}

// IsTerminal reports whether the status admits no further transition.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired:
		return true
	default:
		return false
	}
}

// transitions is the allowed state machine: from → the set of permitted next states. A
// transition not listed here is rejected (a run can never skip to a terminal state without
// passing through the machine, and terminal states are frozen).
var transitions = map[Status]map[Status]bool{
	StatusQueued: {
		StatusRunning:   true,
		StatusCancelled: true,
		StatusExpired:   true,
	},
	StatusRunning: {
		StatusRequiresAction: true,
		StatusWaiting:        true, // suspend: parked on child run(s), machine-woken (ADR 0060)
		StatusSucceeded:      true,
		StatusFailed:         true,
		StatusCancelled:      true,
		StatusExpired:        true,
	},
	StatusRequiresAction: {
		StatusRunning:   true, // resume
		StatusCancelled: true,
		StatusExpired:   true,
	},
	// waiting is machine-woken: a satisfied wait re-QUEUES the run (waiting→queued) and the existing
	// worker pool re-claims it — the worker pool IS the resume loop (ADR 0060 §3). It may also be
	// cancelled/expired directly (a cancel cascade or a lifetime bound while parked).
	StatusWaiting: {
		StatusQueued:    true, // resume — the wake re-queues; the pool re-claims
		StatusCancelled: true,
		StatusExpired:   true,
	},
}

// CanTransition reports whether from→to is a legal state-machine move.
func CanTransition(from, to Status) bool {
	return transitions[from][to]
}

// Message is one turn in the run's conversation (the same {role, content} shape the model
// consumes + the memory plane stores, m29.6).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Run is a durable agent execution. Value fields are non-secret (the invoking user's identity
// is the run capability, not stored here); Input is the raw request forwarded to the agent.
type Run struct {
	// ID is the stable run identifier (also a correlation key alongside TraceID).
	ID string `json:"id"`
	// Namespace + Agent name the deployed agent this run targets.
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	// Input is the raw request body forwarded to the agent's /invoke.
	Input json.RawMessage `json:"input,omitempty"`
	// ConversationID threads this run into a chat (m29.5); empty ⇒ single-shot.
	ConversationID string `json:"conversationId,omitempty"`
	// TraceID is the observability hand-off (minted when execution starts).
	TraceID string `json:"traceId,omitempty"`
	// Status is the current lifecycle state.
	Status Status `json:"status"`
	// Messages is the conversation so far (seeded with the user turn; the final assistant
	// answer is appended on success).
	Messages []Message `json:"messages,omitempty"`
	// RequiresAction, when Status == requires_action, names what the run is waiting on — e.g.
	// the MCP servers needing consent (the m25.9 consent_required set), generalised.
	RequiresAction *Action `json:"requiresAction,omitempty"`
	// Error, when Status == failed, is the honest failure reason.
	Error string `json:"error,omitempty"`
	// CreatedAt / UpdatedAt bound the run's timeline.
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// --- Spawn lineage (M64, ADR 0057): set when this run is a SUB-RUN a supervisor spawned via
	// delegate_to. Non-secret correlation, so exposed on the API (the console renders the spawn tree,
	// the trace nests). A ROOT run leaves ParentRunID empty and RootRunID == ID; SpawnDepth 0.

	// ParentRunID is the supervisor run that spawned this sub-run (empty ⇒ a root run).
	ParentRunID string `json:"parentRunId,omitempty"`
	// RootRunID is the tree root — the aggregate spawn-budget key (spawn:{tenant}:{rootRunId}). For a
	// root run it equals ID; a sub-run inherits the parent's RootRunID.
	RootRunID string `json:"rootRunId,omitempty"`
	// SpawnDepth is this run's depth in the spawn tree (0 = root; parent+1 for a sub-run). Bounded by
	// the team's maxSpawnDepth (the per-path guard, carried in the spawn envelope).
	SpawnDepth int `json:"spawnDepth,omitempty"`

	// --- Execution record (m32.2): the non-secret material a WORKER needs to (re)execute this run
	// off the request path — durably, on any pod. Not part of the public API object (json:"-"); the
	// durable store persists them as their own columns. None is a secret: CallerUsername + Boundary
	// are identifiers, Endpoint is a service URL. They let a worker re-mint a fresh run capability
	// for the original caller (OBO stays intact) without the caller's connection being present.

	// CallerUsername is the invoking user's identity, so a worker re-mints the run capability on
	// their behalf (the user consented by creating the run — an autonomous run acts with their
	// granted scope). Empty ⇒ capability minting was disabled at create time.
	CallerUsername string `json:"-"`
	// Boundary is the ADR 0033 trust boundary credentials resolve within (the agent's registry, or
	// the agent itself when standalone). Captured at create time so the re-mint matches.
	Boundary string `json:"-"`
	// Endpoint is the agent endpoint resolved (and authorized) at create time; the worker reuses it
	// rather than re-resolving, so the create-time RBAC decision is the gate.
	Endpoint string `json:"-"`
	// WorkerID is the worker currently leasing this run (empty ⇒ unclaimed); LeaseExpiresAt bounds
	// the lease so a dead worker's run can be reclaimed and resumed (m32.3).
	WorkerID       string     `json:"-"`
	LeaseExpiresAt *time.Time `json:"-"`
	// OutputSchema is the agent's spec.runtime.outputSchema, pinned at create time (raw JSON Schema
	// text). m65.4 validates the run's terminal answer against it. Empty ⇒ no schema, no validation.
	// Pinned so an operator editing the schema mid-run does not retroactively change validation.
	OutputSchema string `json:"-"`

	// Record, when true, opts THIS run into record mode (M78, ADR 0071 §1/§2): the launcher gateway
	// captures model I/O and the egress sidecar captures tool I/O into a portable fixture
	// (internal/replay). It is a RUN-SCOPED opt-in — you record a specific run, not an agent — set at
	// create time from the run-create request (POST /api/runs {record:true}). This field is the
	// TRIGGER m78.1 defines; the actual capture wiring (the new controller-injected interposition
	// reason that forces both proxies to interpose, fail-closed per ADR 0071 C2, and the RECORD_MODE
	// env it flows to the launcher/sidecar as) is m78.2/m78.3 — they READ this field to decide whether
	// to inject that reason. Non-secret (a boolean opt-in), so it rides the run DTO. Default false ⇒
	// a normal (non-recorded) run, so old rows load unchanged.
	Record bool `json:"record,omitempty"`

	// --- Workflow instance + wait record (M67, ADR 0060): set when this run EXECUTES a declarative
	// Workflow (kind: workflow). A workflow instance is a Run with a WorkflowRef + a pinned SpecSnapshot
	// (resuming against a live-edited CR is a correctness bug — CRD edits affect NEW instances only) +
	// a Cursor tracking per-node progress. When the executor (m67.3) launches child node run(s) it
	// SUSPENDS the run to `waiting` with a wait record; the transactional wake resumes it. None is a
	// secret (a workflow name, a resolved spec, opaque progress JSON), so they COULD ride the DTO — kept
	// json:"-" here (the store persists them as their own columns; the API exposure is m67.4's call).

	// WorkflowRef names the Workflow CR this run instantiates (empty ⇒ not a workflow run).
	WorkflowRef string `json:"-"`
	// SpecSnapshot is the resolved workflow spec pinned at instance-create time (JSON). Empty ⇒ none.
	SpecSnapshot string `json:"-"`
	// NodeEndpoints maps a workflow node's agentRef → its resolved invoke endpoint, pinned at
	// instance-create time through the CALLER-SCOPED client (ADR 0011 — the caller's own RBAC gates the
	// AgentDeployment reads) and exactly as a single run pins its Endpoint at create (m32.2, ADR 0060
	// snapshot-pinning). The workflow executor runs OFF-REQUEST in the run-worker and has no caller token,
	// so it reads these pinned endpoints instead of re-resolving an AgentDeployment — the BFF SA holds NO
	// agent-CRD RBAC (config/bff/role.yaml is `rules: []`). Keyed by agentRef (a node's agent), so several
	// nodes backed by the same agent share one entry. Empty ⇒ not a workflow run (or no nodes).
	NodeEndpoints map[string]string `json:"-"`
	// Cursor is the executor's per-node progress (JSON, opaque to the store — the executor owns its
	// shape: pending / launched(childID) / done(outputRef) per node). Resume advances it, never
	// replays the graph. Empty ⇒ no progress yet.
	Cursor string `json:"-"`
	// WaitOn is the set of CHILD run ids this run is parked on while `waiting`. The transactional
	// wake removes a child as it goes terminal; an empty WaitOn (mode all) or one satisfied child
	// (mode any) means the wait is met → the run is re-queued. Empty when not waiting.
	WaitOn []string `json:"-"`
	// WaitMode is how WaitOn is satisfied: WaitAll (every child terminal) or WaitAny (at least one).
	// Empty when not waiting.
	WaitMode WaitMode `json:"-"`

	// --- Handoff outcome (M67, ADR 0060 §5): set when this run TERMINATED because its agent handed
	// the conversation off to another roster member (`handoff_to`). Handoff is a CONVERSATION primitive,
	// NOT a workflow edge: the run's Agent field stays IMMUTABLE (it is the audit record) — A's run ends
	// here and B's turn is a NEW ROOT run on the same conversation, its own audit identity. HandedOffTo
	// names the agent the conversation was transferred to; the run itself transitions to `succeeded`
	// (the outcome IS the handoff — no new terminal state, per ADR 0060 §5). Non-secret (an agent name),
	// so it rides the API DTO. Empty ⇒ this run did not hand off. Exposed via runToDTO (json:"-" here so
	// the store persists it as its own column).
	HandedOffTo string `json:"-"`

	// HandoffSourceRunID is the BACKLINK on B: the run (A) whose `handoff_to` created THIS run. Handoff
	// is a transfer, so B is a NEW ROOT run with NO ParentRunID (its own audit identity) — this field is
	// the only forward/backward link between A and B, closing the handoff lineage loop (Fable, 2026-08-09).
	// Empty ⇒ this run was not created by a handoff (a normal invoke/create). Non-secret (a run id).
	HandoffSourceRunID string `json:"-"`

	// HandoffSkipHistoryReplay (m83.6) is B's ONE-TURN handoff INPUT FILTER: true ⇒ this run was created
	// by a `handoff_to include_history=false`, so the run-worker stamps X-Ctxmesh-Include-History: false
	// on B's FIRST /invoke and the SDK managed loop skips replaying the prior conversation history on
	// that transfer turn (A handed off with a SUMMARY). It applies to B's TRANSFER TURN ONLY — subsequent
	// user turns to B are ordinary invokes with no header (replay normally); B stays memory-wired on the
	// SAME conversation. Default false ⇒ B replays the full history (ADR 0060 §5 default, unchanged), so
	// old rows + a default handoff load byte-for-byte as today. Non-secret (a boolean), json:"-" (the
	// store persists it as its own column — it is a worker signal, not part of the API DTO).
	HandoffSkipHistoryReplay bool `json:"-"`

	// --- Ingestion job (M68, ADR 0061 Fork 2): set when this run INGESTS a KnowledgeBase corpus. An ingestion
	// run is a Run with an IngestionRef (the KB name) + a pinned IngestionSpec (the resolved source + embedding
	// route + chunking + the snapshotted document object-keys), routed to executeIngestion by the typed marker
	// IsIngestionJob() = IngestionRef != "" (mirroring IsWorkflowInstance). It has NO agent, NO OBO-to-a-model,
	// NO conversation — the run store is a generic durable-job runner (as M67 proved). It reuses the shared
	// Cursor field for per-document progress (a run is either a workflow OR an ingestion, never both), so a
	// worker reclaim resumes at the next un-done document without re-embedding. None is a secret (a KB name, a
	// resolved spec) — kept json:"-" (the store persists them as their own columns; API exposure is m68.10's call).

	// IngestionRef names the KnowledgeBase this run ingests (empty ⇒ not an ingestion run). The dispatch marker.
	IngestionRef string `json:"-"`
	// IngestionSpec is the resolved ingestion parameters pinned at ingest-create time (JSON: namespace, KB,
	// embeddingRoute, chunking, and the document object-keys resolved from the source). Pinned so a live-edited
	// KB or a changed bucket does not retroactively alter an in-flight ingestion (the ADR 0060 snapshot-pinning
	// discipline). Empty ⇒ not an ingestion run.
	IngestionSpec string `json:"-"`

	// --- Dataset export job (M69, ADR 0062 Fork 1): set when this run EXPORTS production traces into the
	// control-plane dataset store, REDACTED (governance #1 / the PII P1). An export run is a Run with an ExportRef
	// (the dataset name) + a pinned ExportSpec (the resolved dataset namespace+name, the agent tag, and the
	// from/to timerange), routed to executeDatasetExport by the typed marker IsDatasetExportJob() = ExportRef != ""
	// (mirroring IsIngestionJob). Like an ingestion run it has NO agent, NO OBO-to-a-model, NO conversation — it is
	// a trusted control-plane job that holds cpDB + Langfuse creds (governance #8). It reuses the shared Cursor for
	// per-page resume progress (a run is either a workflow OR an ingestion OR an export, never two), so a worker
	// reclaim resumes at the next un-exported page. None is a secret (a dataset name, an agent tag, a timerange) —
	// kept json:"-" (the store persists them as their own columns).

	// ExportRef names the dataset this run exports into (empty ⇒ not an export run). The dispatch marker.
	ExportRef string `json:"-"`
	// ExportSpec is the resolved export parameters pinned at export-create time (JSON: dataset namespace+name, the
	// agent tag "<ns>/<name>", from/to timerange). Pinned so a later config change does not retroactively alter an
	// in-flight export (the ADR 0060 snapshot-pinning discipline). Empty ⇒ not an export run.
	ExportSpec string `json:"-"`
	// Outcome is the executor-written terminal outcome record (JSON, opaque to the store). For an ingestion run
	// it carries the document/chunk/size counts + a partial flag + a coded terminal reason — the m68.10 SEAM the
	// KnowledgeBase-status reconcile reads (the off-request run-worker has no KB-status RBAC, so it records the
	// outcome ON THE RUN instead of writing KB.status). It is a MUTABLE column (unlike IngestionRef/IngestionSpec,
	// which are create-only) so the executor persists it on completion. Empty ⇒ no outcome recorded yet.
	Outcome string `json:"-"`
}

// WaitMode is how a `waiting` run's WaitOn set is satisfied (ADR 0060 §3, extended in ADR 0075).
//
// ONE-WAY DOOR (ADR 0075 §1 / Consequences): the persisted string of each mode is pinned FOREVER, and
// so are the two semantic invariants baked into waitSatisfied below — (1) `cancelled` is a NON-SUCCESS
// terminal (it counts toward exhaustion / triggers fail-fast, never toward success), and (2) a MISSING
// child row is treated as `StatusCancelled` (a non-success terminal, never a success). Changing the
// meaning of an already-persisted mode value later is the trap; these are fixed now.
type WaitMode string

const (
	// WaitAll — the wait is met when EVERY child in WaitOn has gone terminal (a join / all-of).
	WaitAll WaitMode = "all"
	// WaitAny — the wait is met when AT LEAST ONE child in WaitOn has gone terminal (an any-of).
	WaitAny WaitMode = "any"
	// WaitAllFailFast — outcome-aware "all" (ADR 0075 §1, L5 map fail-fast): the wait is met the moment
	// ANY child ends NON-SUCCEEDED (failed/cancelled/expired — a doomed fan-out we can cut short) OR every
	// child has gone terminal (the all-success join). Persisted; do NOT change its meaning.
	WaitAllFailFast WaitMode = "all-fail-fast"
	// WaitAnySuccess — outcome-aware "any" (ADR 0075 §1, L4 any-of): the wait is met the moment ANY child
	// SUCCEEDS (the first winner) OR every child has gone terminal (exhaustion — all failed/cancelled).
	// Persisted; do NOT change its meaning.
	WaitAnySuccess WaitMode = "any-success"
)

// waitSatisfied is THE satisfaction predicate — the SINGLE source of truth for whether a `waiting`
// run's wait is met, evaluated over its WaitOn children's persisted statuses (ADR 0075 §1). Both the
// event-driven hot path (satisfyChild) and the sweep reconcilers (waitMet / waitMetLocked) funnel
// through it so there is exactly one copy of the logic.
//
// statuses is one entry per WaitOn child = that child's persisted Status. A MISSING child row MUST be
// passed as StatusCancelled by the caller — a non-success terminal, NEVER a success (a vanished child
// can never wake us; under any-success it counts as exhaustion, never a win). This is a pinned contract
// (ADR 0075 §3 / the one-way door on WaitMode above).
//
//	all            : ∀ terminal                                          (existing — join)
//	any            : ∃ terminal                                          (existing — any-of)
//	all-fail-fast  : (∃ terminal ∧ status ≠ succeeded) ∨ ∀ terminal      (L5 — map fail-fast)
//	any-success    : (∃ succeeded) ∨ ∀ terminal                          (L4 — first success, else exhausted)
//
// Every rule is a MONOTONE predicate over ABSORBING (terminal) states (ADR 0075's load-bearing
// invariant): terminal never un-happens, so once met, always met — early wake, duplicate completion,
// and sweep re-evaluation collapse to "at worst a late/spurious KICK, never a wrong one".
//
// An UNKNOWN mode DELIBERATELY degrades to `all`-semantics (the default branch) — NOT a panic. This is
// the intentional mixed-version / rollback story (ADR 0075 §1): an OLD binary that sees a NEW persisted
// mode string falls back to a late-but-never-wrong all-terminal wake; the resume path recomputes the
// real outcome from the children.
func waitSatisfied(mode WaitMode, statuses []Status) bool {
	allTerminal := true
	anyTerminal := false
	anySucceeded := false
	anyNonSuccessTerminal := false
	for _, st := range statuses {
		if st.IsTerminal() {
			anyTerminal = true
			if st == StatusSucceeded {
				anySucceeded = true
			} else {
				anyNonSuccessTerminal = true // failed / cancelled / expired (incl. a missing child ⇒ cancelled)
			}
		} else {
			allTerminal = false
		}
	}
	switch mode {
	case WaitAny:
		return anyTerminal
	case WaitAllFailFast:
		return anyNonSuccessTerminal || allTerminal
	case WaitAnySuccess:
		return anySucceeded || allTerminal
	default: // WaitAll — AND the intentional unknown-mode degradation (ADR 0075 §1, mixed-version safe).
		return allTerminal
	}
}

// ActionKind classifies what a requires_action run is waiting on.
type ActionKind string

const (
	// ActionConsentRequired — the invoking user must connect their account for one or more MCP
	// servers before the run can proceed (the OBO consent, ADR 0031 generalised).
	ActionConsentRequired ActionKind = "consent_required"
	// ActionApproval — a human must approve a step before it runs (human-in-the-loop, M32).
	ActionApproval ActionKind = "approval"
	// ActionPlanApproval — a human must approve a workflow run's PLAN before the graph executes
	// (planning mode, M67, ADR 0060 §6). It is the ONE legitimate use of requires_action on a workflow
	// run: the executor pauses here BEFORE launching node 1; resume-approve runs the graph, resume-deny
	// terminates the run ("plan rejected"). Distinct from ActionApproval (a mid-run step gate) — this
	// gates the whole plan up front and is resolved by the workflow executor, not by re-invoking an agent.
	ActionPlanApproval ActionKind = "plan_approval"
)

// Action describes why a run is paused in requires_action + what resolves it.
type Action struct {
	Kind ActionKind `json:"kind"`
	// Servers names the MCP servers needing consent (for ActionConsentRequired).
	Servers []string `json:"servers,omitempty"`
	// Key is the stable approval key the resumed run must carry back (for ActionApproval, m32.4) so
	// the agent's pause_for_approval(key) proceeds instead of pausing again.
	Key string `json:"key,omitempty"`
	// Message is a human-readable description of the required action (for approval: the summary the
	// approver sees).
	Message string `json:"message,omitempty"`
}

// New builds a queued run for the given agent + input. The id must be supplied by the caller
// (minted with crypto/rand at the API boundary) so this package holds no ambient randomness.
func New(id, namespace, agent string, input json.RawMessage, conversationID string, now time.Time) *Run {
	return &Run{
		ID:             id,
		Namespace:      namespace,
		Agent:          agent,
		Input:          input,
		ConversationID: conversationID,
		Status:         StatusQueued,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// Transition moves the run to a new status, validating the state machine. It returns an error
// (leaving the run unchanged) on an illegal move — the caller must never force a run past the
// machine. On success UpdatedAt advances.
func (r *Run) Transition(to Status, now time.Time) error {
	if r.Status == to {
		return nil // idempotent no-op
	}
	if !CanTransition(r.Status, to) {
		return fmt.Errorf("run %s: illegal transition %s → %s", r.ID, r.Status, to)
	}
	r.Status = to
	r.UpdatedAt = now
	// Leaving requires_action clears the pending action.
	if to != StatusRequiresAction {
		r.RequiresAction = nil
	}
	// Leaving waiting clears the wait record — a resumed (or cancelled/expired) run is no longer
	// parked on children. The wake path clears WaitOn as it satisfies it; this is the belt-and-braces
	// for any other exit (cancel/expire) so a re-queued run never carries a stale wait set.
	if to != StatusWaiting {
		r.WaitOn = nil
		r.WaitMode = ""
	}
	return nil
}

// suspendToWaiting parks a RUNNING run on the given child run ids, releasing its lease so the
// worker pool does not treat it as claimed (a waiting run holds NO worker/lease, ADR 0060 §3). It
// is the store-internal primitive the exported Suspend wraps. mode must be WaitAll or WaitAny and
// waitOn must be non-empty — an empty wait set can never be woken, so it is rejected.
func (r *Run) suspendToWaiting(waitOn []string, mode WaitMode, now time.Time) error {
	if len(waitOn) == 0 {
		return fmt.Errorf("run %s: suspend requires at least one child to wait on", r.ID)
	}
	switch mode {
	case WaitAll, WaitAny, WaitAllFailFast, WaitAnySuccess:
		// ok — a known, outcome-aware or outcome-agnostic mode.
	default:
		return fmt.Errorf("run %s: invalid wait mode %q", r.ID, mode)
	}
	if err := r.Transition(StatusWaiting, now); err != nil {
		return err
	}
	r.WaitOn = append([]string(nil), waitOn...)
	r.WaitMode = mode
	// A waiting run holds no lease and no worker — it is not claimable and not reclaimable while
	// parked; it re-enters execution only via waiting→queued.
	r.WorkerID = ""
	r.LeaseExpiresAt = nil
	return nil
}

// satisfyChild removes childID from the wait set and reports whether the wait is NOW met — the O(1)
// incremental hot-path counterpart of waitSatisfied (ADR 0075 §1). childStatus is the child's FULL
// terminal Status (the caller passes the just-committed terminal state): `cancelled` matters, so this
// takes the whole Status, not a succeeded/failed bool. It returns removed=false when childID was not in
// the set (an already-satisfied / duplicate completion) so the wake is idempotent — a reclaimed child
// completion cannot re-fire a wake or corrupt the set.
//
// The `met` it returns for each mode is EXACTLY what waitSatisfied would return over the children's
// final statuses — a property test pins the two equivalent. Because every rule is monotone over
// absorbing terminal states, an early-fire here (fail-fast / first-success) directly WITNESSES the
// predicate: the completing child's own outcome is the reason, so the fire can never be premature.
func (r *Run) satisfyChild(childID string, childStatus Status) (met, removed bool) {
	idx := -1
	for i, id := range r.WaitOn {
		if id == childID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, false // not waiting on this child (or already satisfied) — no-op
	}
	r.WaitOn = append(r.WaitOn[:idx], r.WaitOn[idx+1:]...)
	switch r.WaitMode {
	case WaitAny:
		return true, true // one terminal child meets an any-wait (outcome-agnostic)
	case WaitAllFailFast:
		// A non-succeeded terminal child is a doomed fan-out → wake NOW (its outcome witnesses the ∃
		// clause). Otherwise met iff every child is now terminal (the all-success join drained the set).
		return childStatus != StatusSucceeded || len(r.WaitOn) == 0, true
	case WaitAnySuccess:
		// A succeeded child is the first winner → wake NOW. A failed/cancelled child only advances
		// toward exhaustion: met iff it was the LAST child (the set is now empty ⇒ ∀ terminal, all-failed).
		return childStatus == StatusSucceeded || len(r.WaitOn) == 0, true
	default: // WaitAll — AND the intentional unknown-mode degradation (mixed-version safe, ADR 0075 §1).
		return len(r.WaitOn) == 0, true
	}
}
