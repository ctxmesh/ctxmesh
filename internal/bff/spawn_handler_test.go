package bff

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/run"
	"github.com/ctxmesh/agentry/internal/runcap"
)

const spawnAud = "credential-plane"

// newSpawnServer builds a minimal capability-authorized spawn server: a signer, a memstore run store
// seeded with the parent run, and worker-dispatch ON (so a spawned sub-run just queues — no real
// endpoint is invoked). It returns the server, its signer (to mint capabilities), and the store.
func newSpawnServer(t *testing.T, parent *run.Run) (*Server, *runcap.Signer, run.Store) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := runcap.NewSigner(priv, spawnAud, nil)
	store := run.NewMemStore()
	if parent != nil {
		require.NoError(t, store.Create(parent))
	}
	s := &Server{
		capabilitySigner:  signer,
		runStore:          store,
		runWorkerDispatch: true, // queue the sub-run; don't invoke a real endpoint in a unit test
		log:               logr.Discard(),
	}
	return s, signer, store
}

// mkParentRun builds a live (running) parent run carrying the OBO identity a sub-run inherits.
func mkParentRun(id string) *run.Run {
	r := run.New(id, "team-ns", "supervisor", nil, "conv-42", time.Now())
	r.Status = run.StatusRunning
	r.CallerUsername = "alice"
	r.Boundary = "r:research"
	r.TraceID = "trace-xyz"
	return r
}

func mintCap(t *testing.T, signer *runcap.Signer, runID string) string {
	t.Helper()
	tok, err := signer.Mint(runcap.MintRequest{
		User: "uhash-alice", Agent: "supervisor", Boundary: "r:research", RunID: runID, TTL: time.Hour,
	})
	require.NoError(t, err)
	return tok
}

func postSpawn(t *testing.T, s *Server, capToken string, body SpawnRunRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/internal/spawn", bytes.NewReader(raw))
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func validSpawnBody() SpawnRunRequest {
	return SpawnRunRequest{
		SubAgent: "web-researcher", Endpoint: "http://web-researcher.team-ns.svc",
		Input: json.RawMessage(`"find the docs"`), Step: "step-1", CallID: "call-a",
	}
}

// TestSpawn_ValidCapabilityCreatesSubRunAsSameUser — the happy path: a verified parent capability spawns
// a sub-run that inherits the SAME invoking user + boundary + conversation + trace, with spawn lineage.
func TestSpawn_ValidCapabilityCreatesSubRunAsSameUser(t *testing.T) {
	s, signer, store := newSpawnServer(t, mkParentRun("parent-1"))

	rec := postSpawn(t, s, mintCap(t, signer, "parent-1"), validSpawnBody())
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var resp SpawnRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	sub, err := store.Get(resp.ID)
	require.NoError(t, err)

	// OBO inheritance — the sub-run acts as the SAME user in the SAME boundary (no re-consent, no escalation).
	assert.Equal(t, "alice", sub.CallerUsername)
	assert.Equal(t, "r:research", sub.Boundary)
	assert.Equal(t, "conv-42", sub.ConversationID, "one conversation across the tree")
	assert.Equal(t, "trace-xyz", sub.TraceID, "one trace across the tree")
	// Spawn lineage.
	assert.Equal(t, "parent-1", sub.ParentRunID)
	assert.Equal(t, "parent-1", sub.RootRunID, "a root parent roots the tree at itself")
	assert.Equal(t, 1, sub.SpawnDepth)
	assert.Equal(t, "web-researcher", sub.Agent)
	assert.Equal(t, "http://web-researcher.team-ns.svc", sub.Endpoint)
	assert.Equal(t, "team-ns", sub.Namespace, "the sub-run runs in the supervisor's namespace")
}

// TestSpawn_MissingCapabilityIs401 — no bearer-token fallback: the route authenticates ONLY on the capability.
func TestSpawn_MissingCapabilityIs401(t *testing.T) {
	s, _, _ := newSpawnServer(t, mkParentRun("parent-1"))
	rec := postSpawn(t, s, "", validSpawnBody())
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestSpawn_ForgedCapabilityIs401 — a capability signed by a DIFFERENT key is rejected (fail-closed).
func TestSpawn_ForgedCapabilityIs401(t *testing.T) {
	s, _, _ := newSpawnServer(t, mkParentRun("parent-1"))
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	forged := runcap.NewSigner(otherPriv, spawnAud, nil)
	rec := postSpawn(t, s, mintCap(t, forged, "parent-1"), validSpawnBody())
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "a capability signed by an unknown key is refused")
}

// TestSpawn_UnknownParentRunIs403 — a validly-signed capability whose RunID names no live run cannot spawn.
func TestSpawn_UnknownParentRunIs403(t *testing.T) {
	s, signer, _ := newSpawnServer(t, nil) // no parent seeded
	rec := postSpawn(t, s, mintCap(t, signer, "ghost-run"), validSpawnBody())
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestSpawn_TerminalParentIs409 — a captured capability for a finished run cannot spawn (replay guard).
func TestSpawn_TerminalParentIs409(t *testing.T) {
	parent := mkParentRun("parent-done")
	parent.Status = run.StatusSucceeded
	s, signer, _ := newSpawnServer(t, parent)
	rec := postSpawn(t, s, mintCap(t, signer, "parent-done"), validSpawnBody())
	assert.Equal(t, http.StatusConflict, rec.Code)
}

// TestSpawn_Idempotent — the SAME (parent, step, callId) returns the SAME sub-run, created once.
func TestSpawn_Idempotent(t *testing.T) {
	s, signer, store := newSpawnServer(t, mkParentRun("parent-1"))
	cap := mintCap(t, signer, "parent-1")

	first := postSpawn(t, s, cap, validSpawnBody())
	require.Equal(t, http.StatusAccepted, first.Code)
	var r1 SpawnRunResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &r1))

	second := postSpawn(t, s, cap, validSpawnBody())
	require.Equal(t, http.StatusOK, second.Code, "a re-issued identical spawn returns the existing sub-run")
	var r2 SpawnRunResponse
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &r2))

	assert.Equal(t, r1.ID, r2.ID, "same deterministic sub-run id")
	assert.Len(t, store.List(), 2, "still exactly parent + one sub-run (no double-spawn)")
}

// TestSpawn_MissingFieldsIs400 — an incomplete body is rejected before any create.
func TestSpawn_MissingFieldsIs400(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("parent-1"))
	body := validSpawnBody()
	body.Endpoint = "" // missing
	rec := postSpawn(t, s, mintCap(t, signer, "parent-1"), body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestSpawn_NilSignerIs501 — spawn honestly degrades when capability minting is not configured.
func TestSpawn_NilSignerIs501(t *testing.T) {
	// A server with a store but NO signer never registers the route → the mux 404s. To exercise the
	// handler's own 501 path we call it directly.
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/internal/spawn", nil)
	s.handleSpawnRun(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func getSpawnedRun(t *testing.T, s *Server, capToken, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/internal/runs/"+id, nil)
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestReadSpawnedRun_DirectChildOK — a supervisor reads a sub-run it directly spawned + gets the answer.
func TestReadSpawnedRun_DirectChildOK(t *testing.T) {
	s, signer, store := newSpawnServer(t, mkParentRun("parent-1"))
	sub := run.New("sub-1", "team-ns", "worker", nil, "conv-42", time.Now())
	sub.ParentRunID = "parent-1"
	sub.Status = run.StatusSucceeded
	sub.Messages = []run.Message{{Role: "user", Content: "go"}, {Role: "assistant", Content: "done: 42"}}
	require.NoError(t, store.Create(sub))

	rec := getSpawnedRun(t, s, mintCap(t, signer, "parent-1"), "sub-1")
	require.Equal(t, http.StatusOK, rec.Code)
	var got SpawnedRunStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "succeeded", got.Status)
	assert.Equal(t, "done: 42", got.Answer, "the final assistant message is the answer")
}

// TestReadSpawnedRun_NotMyChildIs403 — a capability cannot read a run it did not spawn (no sibling leak).
func TestReadSpawnedRun_NotMyChildIs403(t *testing.T) {
	s, signer, store := newSpawnServer(t, mkParentRun("parent-1"))
	other := run.New("sub-other", "team-ns", "worker", nil, "", time.Now())
	other.ParentRunID = "someone-else" // spawned by a different supervisor
	require.NoError(t, store.Create(other))

	rec := getSpawnedRun(t, s, mintCap(t, signer, "parent-1"), "sub-other")
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestReadSpawnedRun_MissingCapIs401 — the await endpoint authenticates only on the capability.
func TestReadSpawnedRun_MissingCapIs401(t *testing.T) {
	s, _, _ := newSpawnServer(t, mkParentRun("parent-1"))
	rec := getSpawnedRun(t, s, "", "sub-1")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// budgetedBody adds the team spawn budget the launcher relays (trusted env) to a spawn request.
func budgetedBody(maxDepth, maxTotal int) SpawnRunRequest {
	b := validSpawnBody()
	b.MaxSpawnDepth = maxDepth
	b.MaxTotalSpawns = maxTotal
	return b
}

// TestSpawn_DepthExceeded_Authoritative — the BFF enforces maxSpawnDepth against the VERIFIED parent's
// depth (not an agent-supplied header), fail-closed with 429 (the security-review P1-A fix).
func TestSpawn_DepthExceeded_Authoritative(t *testing.T) {
	parent := mkParentRun("parent-deep")
	parent.SpawnDepth = 3 // a sub-run 3 levels down
	s, signer, store := newSpawnServer(t, parent)

	rec := postSpawn(t, s, mintCap(t, signer, "parent-deep"), budgetedBody(3, 20)) // child depth 4 > 3
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Len(t, store.List(), 1, "no sub-run created past the depth bound")
}

// TestSpawn_TotalBudgetExhausted — the authoritative per-root counter denies past maxTotalSpawns.
func TestSpawn_TotalBudgetExhausted(t *testing.T) {
	s, signer, store := newSpawnServer(t, mkParentRun("parent-1"))
	cap := mintCap(t, signer, "parent-1")

	first := postSpawn(t, s, cap, budgetedBody(9, 1)) // maxTotal=1
	require.Equal(t, http.StatusAccepted, first.Code)

	// A DIFFERENT delegation (distinct callId → distinct sub-run) exceeds the tree's total budget.
	body2 := budgetedBody(9, 1)
	body2.CallID = "call-b"
	second := postSpawn(t, s, cap, body2)
	assert.Equal(t, http.StatusTooManyRequests, second.Code, "the tree's total spawn budget is exhausted")
	assert.Len(t, store.List(), 2, "still only parent + the one admitted sub-run")
}

// TestSpawn_IdempotentDoesNotConsumeBudget — a re-issued identical spawn returns the existing sub-run
// WITHOUT consuming a second unit of the total budget (idempotency precedes the reservation).
func TestSpawn_IdempotentDoesNotConsumeBudget(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("parent-1"))
	cap := mintCap(t, signer, "parent-1")

	require.Equal(t, http.StatusAccepted, postSpawn(t, s, cap, budgetedBody(9, 2)).Code)
	// The SAME delegation again (same callId) → idempotent 200, no budget consumed.
	require.Equal(t, http.StatusOK, postSpawn(t, s, cap, budgetedBody(9, 2)).Code)

	// A THIRD DISTINCT delegation still fits (budget 2, only 1 consumed so far).
	body3 := budgetedBody(9, 2)
	body3.CallID = "call-c"
	assert.Equal(t, http.StatusAccepted, postSpawn(t, s, cap, body3).Code, "the idempotent retry did not consume budget")
}

// TestClampSpawnBudget covers the C19 (ADR 0088) authoritative-gate ceiling: an inflated relayed budget
// is clamped per dimension (depth <= 32, total <= 1024); legit and 0 (unbudgeted) values pass through.
func TestClampSpawnBudget(t *testing.T) {
	cases := []struct {
		inDepth, inTotal     int
		wantDepth, wantTotal int
	}{
		{3, 20, 3, 20},               // legit passes unchanged
		{32, 1024, 32, 1024},         // exactly the ceilings
		{1 << 40, 1 << 40, 32, 1024}, // the abuse -> ceilings
		{100, 5000, 32, 1024},        // both over
		{0, 0, 0, 0},                 // unbudgeted preserved
		{5, 0, 5, 0},                 // total unbudgeted, depth legit
	}
	for _, tc := range cases {
		d, tot := clampSpawnBudget(tc.inDepth, tc.inTotal)
		assert.Equal(t, tc.wantDepth, d, "depth in=%d", tc.inDepth)
		assert.Equal(t, tc.wantTotal, tot, "total in=%d", tc.inTotal)
	}
}

// TestLastAssistantMessage_StripsSpotlightDelimiters guards that a leaked K1 spotlight delimiter
// (ADR 0059 — ⟦tool-output:TOKEN⟧) never surfaces in a sub-run's answer (the orchestration tree, a
// delegate result). A model can echo one into its output after copying a wrapped tool result into a
// delegated task; the surfacing helper must strip them.
func TestLastAssistantMessage_StripsSpotlightDelimiters(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"clean answer unchanged", "Final polished copy.", "Final polished copy."},
		{"leading open delimiter + newline stripped", "⟦tool-output:9f905f4914d05d1547b20643fd4e9ac1⟧\nFinal copy.", "Final copy."},
		{"wrapping delimiters stripped", "⟦tool-output:abc123⟧inner text⟦/tool-output:abc123⟧", "inner text"},
		{"no assistant turn yields empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []run.Message{{Role: "user", Content: "x"}}
			if tc.name != "no assistant turn yields empty" {
				msgs = append(msgs, run.Message{Role: "assistant", Content: tc.content})
			}
			assert.Equal(t, tc.want, lastAssistantMessage(&run.Run{Messages: msgs}))
		})
	}
}
