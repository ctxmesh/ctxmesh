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

package bff

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/auditlog"
	"github.com/ctxmesh/agent-engine/internal/run"
	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// newGuardrailEventServer builds a minimal BFF server for guardrail-event tests:
// a valid capability signer, a memstore audit store, and worker-dispatch ON (so
// the spawn route doesn't try to invoke a real endpoint when accidentally hit).
func newGuardrailEventServer(t *testing.T) (*Server, *runcap.Signer, auditlog.Store) {
	t.Helper()
	_, signer, _ := newSpawnServer(t, nil)
	store := auditlog.NewMemStore()
	s := &Server{
		capabilitySigner:  signer,
		auditStore:        store,
		runStore:          run.NewMemStore(),
		runWorkerDispatch: true,
		log:               logr.Discard(),
	}
	return s, signer, store
}

// mintGuardrailCap mints a valid run capability with a non-empty User field (the actor
// the guardrail-event handler extracts).
func mintGuardrailCap(t *testing.T, signer *runcap.Signer) string {
	t.Helper()
	tok, err := signer.Mint(runcap.MintRequest{
		// An AGENT boundary ("a:<ns>/<agent>") — a guardrail block is on a deployed agent, so the audit
		// row is namespace-scoped from the verified capability (m52.G11c, M139).
		User: "uhash-alice", Agent: "guarded-agent", Boundary: "a:prod/guarded-agent", RunID: "run-g1", TTL: time.Hour,
	})
	require.NoError(t, err)
	return tok
}

// postGuardrailEvent sends a POST /api/internal/guardrail-event to the server with the given
// capability token and body, returning the recorder.
func postGuardrailEvent(t *testing.T, s *Server, capToken string, body guardrailEventRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/internal/guardrail-event", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// validGuardrailBody returns a PII-safe block event body (no raw content).
func validGuardrailBody() guardrailEventRequest {
	return guardrailEventRequest{
		Detector:     "credit-card",
		ScanPoint:    "input",
		ContentHash:  "abc123def456abc123def456abc123def456abc123def456abc123def456abc1", // sha256-like hex
		Agent:        "guarded-agent",
		PolicyAction: "block",
	}
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestGuardrailEvent_ValidCapabilityWritesAuditRow is the golden path: a valid capability
// + PII-safe body produces an audit_log row with action="guardrail.block", actor=userHash,
// the correct detail fields, and NO raw content anywhere.
func TestGuardrailEvent_ValidCapabilityWritesAuditRow(t *testing.T) {
	s, signer, store := newGuardrailEventServer(t)
	tok := mintGuardrailCap(t, signer)

	rec := postGuardrailEvent(t, s, tok, validGuardrailBody())
	assert.Equal(t, http.StatusNoContent, rec.Code)

	page, err := store.List(context.Background(), auditlog.Query{})
	require.NoError(t, err)
	require.Len(t, page.Items, 1, "exactly one audit row must be written")

	row := page.Items[0]
	assert.Equal(t, auditActionGuardrailBlock, row.Action, "action must be guardrail.block")
	assert.Equal(t, "uhash-alice", row.Actor, "actor must be the capability's User (already-hashed)")
	assert.Equal(t, "user", row.ActorKind)
	assert.Equal(t, "bff", row.Source, "source is stamped by appendAudit")
	assert.Equal(t, "prod", row.Namespace, "the agent's namespace is stamped (G11c) so a namespaced audit-reader sees the block")
	assert.Equal(t, "denied", row.Outcome, "a block is a denial")
	assert.Equal(t, "GuardrailPolicy", row.ResourceKind)
	assert.Equal(t, "guarded-agent", row.ResourceName)

	// Detail must contain the PII-safe fields.
	assert.Equal(t, "credit-card", row.Detail["detector"])
	assert.Equal(t, "input", row.Detail["scan_point"])
	assert.Equal(t, "abc123def456abc123def456abc123def456abc123def456abc123def456abc1", row.Detail["content_hash"])
	assert.Equal(t, "guarded-agent", row.Detail["agent"])
	assert.Equal(t, "block", row.Detail["policy_action"])

	// PII-safe invariant: no raw "content" field (only "content_hash" is present and that's a hash).
	_, hasRawContent := row.Detail["content"]
	assert.False(t, hasRawContent, "raw 'content' key must never appear in the audit detail (PII-safe invariant)")

	// Verify the content_hash field holds a non-empty value (not the raw match).
	hashVal, ok := row.Detail["content_hash"].(string)
	require.True(t, ok)
	assert.NotEmpty(t, hashVal)
}

// TestGuardrailEvent_MissingCapabilityRejects verifies that a missing capability returns 401
// and no audit row is written.
func TestGuardrailEvent_MissingCapabilityRejects(t *testing.T) {
	s, _, store := newGuardrailEventServer(t)

	rec := postGuardrailEvent(t, s, "", validGuardrailBody())
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	page, err := store.List(context.Background(), auditlog.Query{})
	require.NoError(t, err)
	assert.Empty(t, page.Items, "no row must be written for a missing capability")
}

// TestGuardrailEvent_ForgedCapabilityRejects verifies that a forged/invalid capability
// returns 401 and no audit row is written.
func TestGuardrailEvent_ForgedCapabilityRejects(t *testing.T) {
	s, _, store := newGuardrailEventServer(t)

	rec := postGuardrailEvent(t, s, "this.is.a.forged.token", validGuardrailBody())
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	page, err := store.List(context.Background(), auditlog.Query{})
	require.NoError(t, err)
	assert.Empty(t, page.Items, "no row must be written for a forged capability")
}

// TestGuardrailEvent_CapabilityFromDifferentSignerRejects verifies that a capability signed
// by a different key (a rogue launcher) is rejected and no row is written.
func TestGuardrailEvent_CapabilityFromDifferentSignerRejects(t *testing.T) {
	s, _, store := newGuardrailEventServer(t)

	// Mint a capability with a DIFFERENT signer (different key pair).
	_, rogueSigner, _ := newSpawnServer(t, nil)
	rogueTok := mintGuardrailCap(t, rogueSigner)

	rec := postGuardrailEvent(t, s, rogueTok, validGuardrailBody())
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	page, err := store.List(context.Background(), auditlog.Query{})
	require.NoError(t, err)
	assert.Empty(t, page.Items, "a capability from a different signer must be rejected")
}

// TestGuardrailEvent_MissingDetectorRejects verifies that a body without a detector is rejected.
func TestGuardrailEvent_MissingDetectorRejects(t *testing.T) {
	s, signer, store := newGuardrailEventServer(t)
	tok := mintGuardrailCap(t, signer)

	body := validGuardrailBody()
	body.Detector = ""
	rec := postGuardrailEvent(t, s, tok, body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	page, _ := store.List(context.Background(), auditlog.Query{})
	assert.Empty(t, page.Items, "missing detector must not produce a row")
}

// TestGuardrailEvent_MissingContentHashRejects verifies that a body without a content_hash is rejected.
func TestGuardrailEvent_MissingContentHashRejects(t *testing.T) {
	s, signer, store := newGuardrailEventServer(t)
	tok := mintGuardrailCap(t, signer)

	body := validGuardrailBody()
	body.ContentHash = ""
	rec := postGuardrailEvent(t, s, tok, body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	page, _ := store.List(context.Background(), auditlog.Query{})
	assert.Empty(t, page.Items, "missing content_hash must not produce a row")
}

// TestGuardrailEvent_NonBlockPolicyActionRejects verifies that only policy_action="block" is accepted
// (this endpoint is block-only by design — auditOnly events stay span-only).
func TestGuardrailEvent_NonBlockPolicyActionRejects(t *testing.T) {
	s, signer, store := newGuardrailEventServer(t)
	tok := mintGuardrailCap(t, signer)

	body := validGuardrailBody()
	body.PolicyAction = "auditOnly"
	rec := postGuardrailEvent(t, s, tok, body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	page, _ := store.List(context.Background(), auditlog.Query{})
	assert.Empty(t, page.Items, "non-block policy_action must not produce a row")
}

// TestGuardrailEvent_NilAuditStoreIsNoop verifies that a nil audit store (audit not wired)
// returns 204 without panicking — the best-effort contract (observability, never a gate).
func TestGuardrailEvent_NilAuditStoreIsNoop(t *testing.T) {
	s, signer, _ := newGuardrailEventServer(t)
	s.auditStore = nil // unwire the store
	tok := mintGuardrailCap(t, signer)

	rec := postGuardrailEvent(t, s, tok, validGuardrailBody())
	// The endpoint still succeeds (the block already happened in the launcher).
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// TestGuardrailEvent_RawContentFieldRejectedBySchema is a paranoid PII-safety check: even
// if a "content" field were injected into the body, DisallowUnknownFields rejects it,
// so no raw match ever reaches the audit_log.
func TestGuardrailEvent_RawContentFieldRejectedBySchema(t *testing.T) {
	s, signer, _ := newGuardrailEventServer(t)
	tok := mintGuardrailCap(t, signer)

	// Manually craft a body with an extra "content" field not in the struct.
	raw := []byte(`{
		"detector":"ssn","scan_point":"input",
		"content_hash":"abc123","agent":"ag","policy_action":"block",
		"content":"123-45-6789"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/internal/guardrail-event", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(runcap.HeaderName, tok)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	// DisallowUnknownFields rejects the body — raw content never reaches the store.
	assert.Equal(t, http.StatusBadRequest, rec.Code, "unknown field 'content' must be rejected (PII-safe invariant)")
}

// TestGuardrailEvent_RealPostgresRoundTrip tests that a guardrail.block row persists and reads
// back via the M63 audit store. It always exercises the in-memory twin; the Postgres path is
// gated on CONTROLPLANE_TEST_DSN (same gate as the auditlog conformance suite).
//
// When the Postgres path is skipped (CONTROLPLANE_TEST_DSN unset), the test logs a notice
// so the caller (orchestrator) knows to run it against a real Postgres separately.
func TestGuardrailEvent_RealPostgresRoundTrip(t *testing.T) {
	type storeEntry struct {
		name  string
		store auditlog.Store
	}
	stores := []storeEntry{{"mem", auditlog.NewMemStore()}}

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — SKIP Postgres guardrail.block round-trip (in-memory twin ran)")
	} else {
		db, err := controlplane.OpenDB(context.Background(), dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		_, err = db.Exec(`TRUNCATE audit_log`)
		require.NoError(t, err)
		stores = append(stores, storeEntry{"postgres", auditlog.NewPostgresStore(db)})
	}

	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			_, signer, _ := newSpawnServer(t, nil)
			srv := &Server{
				capabilitySigner:  signer,
				auditStore:        tc.store,
				runStore:          run.NewMemStore(),
				runWorkerDispatch: true,
				log:               logr.Discard(),
			}
			tok := mintGuardrailCap(t, signer)

			rec := postGuardrailEvent(t, srv, tok, validGuardrailBody())
			require.Equal(t, http.StatusNoContent, rec.Code)

			page, err := tc.store.List(context.Background(), auditlog.Query{Action: auditActionGuardrailBlock})
			require.NoError(t, err)
			require.NotEmpty(t, page.Items, "guardrail.block row must persist in %s store", tc.name)
			row := page.Items[0]
			assert.Equal(t, auditActionGuardrailBlock, row.Action)
			assert.Equal(t, "uhash-alice", row.Actor)
			assert.Equal(t, "credit-card", row.Detail["detector"])
			assert.Equal(t, "abc123def456abc123def456abc123def456abc123def456abc123def456abc1", row.Detail["content_hash"])
			// PII invariant: raw "content" must never appear in the stored row.
			_, hasRawContent := row.Detail["content"]
			assert.False(t, hasRawContent, "raw content must never be stored in %s; PII-safe invariant", tc.name)
		})
	}
}
