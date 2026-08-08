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

	"github.com/ctxmesh/agent-engine/internal/run"
	"github.com/ctxmesh/agent-engine/internal/runcap"
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
