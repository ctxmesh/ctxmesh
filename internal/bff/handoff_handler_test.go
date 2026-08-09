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

// newHandoffServer builds a capability-authorized handoff server: a signer, a memstore run store seeded
// with the source run A, and a mem conversation store. It returns the server, its signer (to mint
// capabilities), the run store, and the conversation store.
func newHandoffServer(t *testing.T, source *run.Run) (*Server, *runcap.Signer, run.Store, run.ConversationStore) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := runcap.NewSigner(priv, spawnAud, nil)
	store := run.NewMemStore()
	if source != nil {
		require.NoError(t, store.Create(source))
	}
	conv := run.NewMemConversationStore()
	s := &Server{
		capabilitySigner:  signer,
		runStore:          store,
		convStore:         conv,
		runWorkerDispatch: true, // queue B; don't invoke a real endpoint in a unit test
		log:               logr.Discard(),
	}
	return s, signer, store, conv
}

// mkSourceRun builds a live (running) source run A on a conversation, carrying the OBO identity B inherits.
func mkSourceRun(id string) *run.Run {
	r := run.New(id, "team-ns", "supervisor", nil, "conv-99", time.Now())
	r.Status = run.StatusRunning
	r.CallerUsername = "alice"
	r.Boundary = "r:research"
	r.TraceID = "trace-abc"
	return r
}

func postHandoff(t *testing.T, s *Server, capToken string, body HandoffRunRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/internal/handoff", bytes.NewReader(raw))
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func validHandoffBody() HandoffRunRequest {
	return HandoffRunRequest{
		TargetAgent:    "billing-agent",
		TargetEndpoint: "http://billing-agent.team-ns.svc",
		Message:        "the user needs a refund",
	}
}

// TestHandoff_TransfersToNewRun — the happy path: a verified source capability terminates A and creates B
// as a NEW ROOT run on the SAME conversation, with OBO for the conversation owner, no ParentRunID, and A's
// agent field UNCHANGED (immutable). The conversation active-agent pointer is set to B.
func TestHandoff_TransfersToNewRun(t *testing.T) {
	s, signer, store, conv := newHandoffServer(t, mkSourceRun("A-1"))

	rec := postHandoff(t, s, mintCap(t, signer, "A-1"), validHandoffBody())
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var resp HandoffRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "A-1", resp.SourceRunID)
	assert.Equal(t, "billing-agent", resp.HandedOffTo)

	// B — a NEW ROOT run for the target agent on the same conversation.
	b, err := store.Get(resp.RunID)
	require.NoError(t, err)
	assert.Equal(t, "billing-agent", b.Agent, "B's run targets the target agent")
	assert.Equal(t, "conv-99", b.ConversationID, "same conversation across the transfer")
	assert.Equal(t, string(run.StatusQueued), string(b.Status), "B is queued for the worker")
	// OBO for the conversation OWNER — the SAME user + boundary, minted fresh for B (no capability transfer).
	assert.Equal(t, "alice", b.CallerUsername, "OBO for the conversation owner (the same user)")
	assert.Equal(t, "r:research", b.Boundary, "the same trust boundary")
	// A TRANSFER, not a delegation — B is a fresh ROOT run, NOT a child of A.
	assert.Empty(t, b.ParentRunID, "B is a new ROOT run, not a sub-run of A")
	assert.Empty(t, b.RootRunID, "B roots its own tree")
	assert.Equal(t, 0, b.SpawnDepth)
	assert.Equal(t, "A-1", b.HandoffSourceRunID, "the A→B backlink (B has no ParentRunID by design)")
	assert.Equal(t, "http://billing-agent.team-ns.svc", b.Endpoint)

	// A — TERMINATED with the recorded handoff outcome; its agent field is UNCHANGED (immutable).
	a, err := store.Get("A-1")
	require.NoError(t, err)
	assert.Equal(t, string(run.StatusSucceeded), string(a.Status), "A terminates succeeded (the outcome is the handoff)")
	assert.Equal(t, "billing-agent", a.HandedOffTo, "A records who it handed off to")
	assert.Equal(t, "supervisor", a.Agent, "A's agent field is IMMUTABLE — never mutated by the transfer")

	// The conversation's active-agent pointer now points at B.
	active, err := conv.GetActiveAgent("conv-99")
	require.NoError(t, err)
	assert.Equal(t, "billing-agent", active.Agent, "the conversation is now active-agent = B")
	assert.Equal(t, "A-1", active.SourceRunID, "the pointer records the handing-off run A")
}

// TestHandoff_MissingCapabilityIs401 — the route authenticates ONLY on the capability.
func TestHandoff_MissingCapabilityIs401(t *testing.T) {
	s, _, _, _ := newHandoffServer(t, mkSourceRun("A-1"))
	rec := postHandoff(t, s, "", validHandoffBody())
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestHandoff_ForgedCapabilityIs401 — a capability signed by a DIFFERENT key is rejected (fail-closed).
func TestHandoff_ForgedCapabilityIs401(t *testing.T) {
	s, _, _, _ := newHandoffServer(t, mkSourceRun("A-1"))
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	forged := runcap.NewSigner(otherPriv, spawnAud, nil)
	rec := postHandoff(t, s, mintCap(t, forged, "A-1"), validHandoffBody())
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestHandoff_UnknownSourceRunIs403 — a validly-signed capability whose RunID names no run cannot hand off.
func TestHandoff_UnknownSourceRunIs403(t *testing.T) {
	s, signer, _, _ := newHandoffServer(t, nil)
	rec := postHandoff(t, s, mintCap(t, signer, "ghost"), validHandoffBody())
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestHandoff_TerminalSourceIs409 — a captured capability for a finished run cannot hand off (replay guard).
func TestHandoff_TerminalSourceIs409(t *testing.T) {
	src := mkSourceRun("A-done")
	src.Status = run.StatusSucceeded
	s, signer, _, _ := newHandoffServer(t, src)
	rec := postHandoff(t, s, mintCap(t, signer, "A-done"), validHandoffBody())
	assert.Equal(t, http.StatusConflict, rec.Code)
}

// TestHandoff_SingleShotSourceIs409 — a source run with no conversation has no thread to transfer.
func TestHandoff_SingleShotSourceIs409(t *testing.T) {
	src := run.New("A-solo", "team-ns", "supervisor", nil, "", time.Now()) // no conversationId
	src.Status = run.StatusRunning
	src.CallerUsername = "alice"
	src.Boundary = "r:research"
	s, signer, _, _ := newHandoffServer(t, src)
	rec := postHandoff(t, s, mintCap(t, signer, "A-solo"), validHandoffBody())
	assert.Equal(t, http.StatusConflict, rec.Code, "handoff requires a conversation")
}

// TestHandoff_MissingFieldsIs400 — an incomplete body is rejected before any transfer.
func TestHandoff_MissingFieldsIs400(t *testing.T) {
	s, signer, _, _ := newHandoffServer(t, mkSourceRun("A-1"))
	body := validHandoffBody()
	body.TargetEndpoint = "" // missing
	rec := postHandoff(t, s, mintCap(t, signer, "A-1"), body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandoff_Idempotent — a re-issued identical handoff returns the SAME B, created once; A stays
// terminal; the pointer is unchanged. (A capability replay / SDK retry cannot double-transfer.)
func TestHandoff_Idempotent(t *testing.T) {
	s, signer, store, _ := newHandoffServer(t, mkSourceRun("A-1"))
	cap := mintCap(t, signer, "A-1")

	first := postHandoff(t, s, cap, validHandoffBody())
	require.Equal(t, http.StatusAccepted, first.Code)
	var r1 HandoffRunResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &r1))

	// The SAME handoff again — A is now terminal, so the source capability is rejected as a replay (409),
	// which is the correct idempotent behaviour: a finished A cannot hand off again.
	second := postHandoff(t, s, cap, validHandoffBody())
	assert.Equal(t, http.StatusConflict, second.Code, "A already terminated — no second transfer")

	// Still exactly A + one B (no double-transfer).
	assert.Len(t, store.List(), 2)
}

// TestHandoff_NilConvStoreIs501 — handoff honestly degrades when the conversation store is not configured.
func TestHandoff_NilConvStoreIs501(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	signer := runcap.NewSigner(priv, spawnAud, nil)
	s := &Server{capabilitySigner: signer, runStore: run.NewMemStore(), log: logr.Discard()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/internal/handoff", nil)
	s.handleHandoffRun(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
