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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/controlplane/auditlog"
	"github.com/ctxmesh/agent-engine/internal/controlplane/sharedrun"
	"github.com/ctxmesh/agent-engine/internal/run"
)

// durableMemRunStore wraps the hot mem run store to report Durable()==true, so the share-mint durability
// gate is satisfied in a handler test without a real Postgres run store. (The mem store itself reports
// false — see TestCreateShare_RefusesNonDurableStore, which uses the bare mem store to prove the refusal.)
type durableMemRunStore struct{ run.Store }

func (durableMemRunStore) Durable() bool { return true }

// captureAuditStore records appended audit entries so a test can assert what was (and was NOT) logged —
// in particular that a share token NEVER appears in any audit row.
type captureAuditStore struct {
	mu      sync.Mutex
	entries []auditlog.Entry
}

func (c *captureAuditStore) Append(_ context.Context, e auditlog.Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
	return nil
}

func (c *captureAuditStore) List(context.Context, auditlog.Query) (auditlog.Page, error) {
	return auditlog.Page{}, nil
}
func (c *captureAuditStore) PruneBefore(context.Context, time.Time) (int64, error) { return 0, nil }

func (c *captureAuditStore) all() []auditlog.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]auditlog.Entry(nil), c.entries...)
}

// shareTestServer builds a Server wired with a share store, an audit store, and a run store seeded with
// one durable run for the given agent/namespace. The caller client answers SelfSubjectReview as `alice`
// and Gets the agent (so authz passes) unless getErr is supplied to force a Forbidden/NotFound.
func shareTestServer(t *testing.T, shareStore sharedrun.Store, audit auditlog.Store, runStore run.Store, getErr error) *Server {
	t.Helper()
	agent := readyAgent("assistant", "team-a", "http://assistant.team-a.svc.cluster.local")
	funcs := ssrInterceptor("alice@example.com", nil)
	if getErr != nil {
		funcs.Get = func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return getErr
		}
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).WithInterceptorFuncs(funcs).Build()
	return NewServer(Options{
		CallerClients:  newFakeFactory(c),
		Scheme:         testScheme(t),
		Auth:           AllowAll{},
		Adapters:       Adapters{Invoke: &fakeInvokeAdapter{}},
		Version:        "test",
		Log:            logr.Discard(),
		RunStore:       runStore,
		SharedRunStore: shareStore,
		AuditStore:     audit,
	})
}

// seededDurableRunStore returns a durable-reporting run store holding one run owned by alice.
func seededDurableRunStore(t *testing.T) run.Store {
	t.Helper()
	base := run.NewMemStore()
	rn := run.New("run-share-1", "team-a", "assistant", json.RawMessage(`{"prompt":"hi"}`), "conv-1", time.Now())
	rn.CallerUsername = "alice@example.com"
	require.NoError(t, base.Create(rn))
	return durableMemRunStore{base}
}

func doShareRequest(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestCreateShare_MintReturnsTokenOnce_StoresHashOnly is the core mint contract: the token is returned
// exactly once in the response; the STORE holds only its SHA-256 (never the token); the audit row records
// the create WITHOUT the token.
func TestCreateShare_MintReturnsTokenOnce_StoresHashOnly(t *testing.T) {
	store := sharedrun.NewMemStore()
	audit := &captureAuditStore{}
	s := shareTestServer(t, store, audit, seededDurableRunStore(t), nil)

	rec := doShareRequest(t, s, http.MethodPost, "/api/runs/run-share-1/shares", `{"includeContent":true}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp CreateShareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Token, "the token is returned exactly once at creation")
	require.NotEmpty(t, resp.ID)
	assert.True(t, resp.IncludeContent)
	assert.True(t, resp.ExpiresAt.After(time.Now()), "a live expiry is returned")

	// The store holds ONLY the hash — GetByTokenHash(hash) finds it; the raw token is nowhere in the record.
	got, ok, err := store.GetByTokenHash(context.Background(), hashShareToken(resp.Token))
	require.NoError(t, err)
	require.True(t, ok, "the share is looked up by the hash of the returned token")
	assert.Equal(t, hashShareToken(resp.Token), got.TokenHash)
	assert.NotEqual(t, resp.Token, got.TokenHash, "the token itself is never stored")
	assert.Equal(t, "run-share-1", got.RunID)
	assert.True(t, got.IncludeContent)

	// The audit row recorded the create WITHOUT the token.
	entries := audit.all()
	require.Len(t, entries, 1)
	assert.Equal(t, auditActionShareCreate, entries[0].Action)
	assert.Equal(t, resp.ID, entries[0].ResourceName)
	raw, _ := json.Marshal(entries[0])
	assert.NotContains(t, string(raw), resp.Token, "the token must NEVER appear in an audit row")
}

// TestCreateShare_DefaultTTLAndCap: no ttlHours → 7d default; an over-cap ttlHours → clamped to 90d.
func TestCreateShare_DefaultTTLAndCap(t *testing.T) {
	s := shareTestServer(t, sharedrun.NewMemStore(), &captureAuditStore{}, seededDurableRunStore(t), nil)

	// Default (empty body) → ~7 days.
	rec := doShareRequest(t, s, http.MethodPost, "/api/runs/run-share-1/shares", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var def CreateShareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &def))
	assert.WithinDuration(t, time.Now().Add(defaultShareTTLHours*time.Hour), def.ExpiresAt, time.Minute)

	// Over the cap → clamped to 90 days.
	rec = doShareRequest(t, s, http.MethodPost, "/api/runs/run-share-1/shares", `{"ttlHours":100000}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var capped CreateShareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &capped))
	assert.WithinDuration(t, time.Now().Add(maxShareTTLHours*time.Hour), capped.ExpiresAt, time.Minute)
}

// TestCreateShare_ForbiddenAgentIs403 proves the authz crux: a caller who cannot GET the run's agent
// (a Forbidden on the caller-scoped read) is DENIED a mint — a user cannot mint a public link for a run
// they cannot access. The share is never created.
func TestCreateShare_ForbiddenAgentIs403(t *testing.T) {
	store := sharedrun.NewMemStore()
	forbidden := errors.NewForbidden(
		schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"}, "assistant", assert.AnError)
	s := shareTestServer(t, store, &captureAuditStore{}, seededDurableRunStore(t), forbidden)

	rec := doShareRequest(t, s, http.MethodPost, "/api/runs/run-share-1/shares", `{}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, "a caller without agent access must NOT mint a public link")

	list, err := store.ListForRun(context.Background(), "run-share-1")
	require.NoError(t, err)
	assert.Empty(t, list, "no share is created on a denied mint")
}

// TestCreateShare_UnknownRunIs404: minting for a non-existent run is a 404 (before any authz).
func TestCreateShare_UnknownRunIs404(t *testing.T) {
	s := shareTestServer(t, sharedrun.NewMemStore(), &captureAuditStore{}, seededDurableRunStore(t), nil)
	rec := doShareRequest(t, s, http.MethodPost, "/api/runs/does-not-exist/shares", `{}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestCreateShare_RefusesNonDurableStore proves a mint against the HOT (non-durable) run store is refused
// with a 409 — a share into the mem store dies on restart (ADR 0069 §1). Here the run store is the bare
// mem store (Durable()==false).
func TestCreateShare_RefusesNonDurableStore(t *testing.T) {
	base := run.NewMemStore()
	rn := run.New("run-hot-1", "team-a", "assistant", json.RawMessage(`{}`), "", time.Now())
	require.NoError(t, base.Create(rn))
	store := sharedrun.NewMemStore()
	s := shareTestServer(t, store, &captureAuditStore{}, base, nil) // bare mem store: not durable

	rec := doShareRequest(t, s, http.MethodPost, "/api/runs/run-hot-1/shares", `{}`)
	assert.Equal(t, http.StatusConflict, rec.Code, "a share into the hot store must be refused")

	list, err := store.ListForRun(context.Background(), "run-hot-1")
	require.NoError(t, err)
	assert.Empty(t, list, "no share is created against a non-durable store")
}

// TestListShares_HidesToken proves the manage list returns the token-FREE DTO (no token, no hash) and
// excludes revoked shares.
func TestListShares_HidesToken(t *testing.T) {
	store := sharedrun.NewMemStore()
	s := shareTestServer(t, store, &captureAuditStore{}, seededDurableRunStore(t), nil)

	// Mint two shares.
	r1 := doShareRequest(t, s, http.MethodPost, "/api/runs/run-share-1/shares", `{}`)
	require.Equal(t, http.StatusCreated, r1.Code)
	var s1 CreateShareResponse
	require.NoError(t, json.Unmarshal(r1.Body.Bytes(), &s1))
	require.Equal(t, http.StatusCreated, doShareRequest(t, s, http.MethodPost, "/api/runs/run-share-1/shares", `{}`).Code)

	rec := doShareRequest(t, s, http.MethodGet, "/api/runs/run-share-1/shares", "")
	require.Equal(t, http.StatusOK, rec.Code)
	// The raw JSON must not contain any token or a "token"/"tokenHash" key.
	assert.NotContains(t, rec.Body.String(), s1.Token, "the manage list must NEVER expose the token")
	assert.NotContains(t, strings.ToLower(rec.Body.String()), "tokenhash")
	assert.NotContains(t, strings.ToLower(rec.Body.String()), "\"token\"")

	var list []ShareSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 2)
	for _, item := range list {
		assert.NotEmpty(t, item.ID)
		assert.False(t, item.Revoked)
	}
}

// TestRevokeShare_Idempotent proves DELETE revokes (204), the share drops from the list, and a repeat
// DELETE (and an unknown shareId) are still 204 — idempotent, no oracle on share existence.
func TestRevokeShare_Idempotent(t *testing.T) {
	store := sharedrun.NewMemStore()
	audit := &captureAuditStore{}
	s := shareTestServer(t, store, audit, seededDurableRunStore(t), nil)

	r1 := doShareRequest(t, s, http.MethodPost, "/api/runs/run-share-1/shares", `{}`)
	require.Equal(t, http.StatusCreated, r1.Code)
	var s1 CreateShareResponse
	require.NoError(t, json.Unmarshal(r1.Body.Bytes(), &s1))

	// Revoke → 204.
	rec := doShareRequest(t, s, http.MethodDelete, "/api/runs/run-share-1/shares/"+s1.ID, "")
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// The share is gone from the manage list.
	listRec := doShareRequest(t, s, http.MethodGet, "/api/runs/run-share-1/shares", "")
	var list []ShareSummary
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &list))
	assert.Empty(t, list, "a revoked share is excluded from the manage list")

	// A repeat revoke + an unknown id are both idempotent 204s.
	assert.Equal(t, http.StatusNoContent, doShareRequest(t, s, http.MethodDelete, "/api/runs/run-share-1/shares/"+s1.ID, "").Code)
	assert.Equal(t, http.StatusNoContent, doShareRequest(t, s, http.MethodDelete, "/api/runs/run-share-1/shares/nope", "").Code)

	// An audit row was recorded for the revoke.
	var sawRevoke bool
	for _, e := range audit.all() {
		if e.Action == auditActionShareRevoke {
			sawRevoke = true
		}
	}
	assert.True(t, sawRevoke, "a revoke is audit-logged")
}

// TestCreateShare_StoreNotConfiguredIs501: absent the share store, the endpoint is an honest 501.
func TestCreateShare_StoreNotConfiguredIs501(t *testing.T) {
	s := shareTestServer(t, nil, &captureAuditStore{}, seededDurableRunStore(t), nil)
	rec := doShareRequest(t, s, http.MethodPost, "/api/runs/run-share-1/shares", `{}`)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// seededDurableRunStoreWithTraceID returns a durable-reporting run store holding a run whose
// run.ID and run.TraceID are DISTINCT — exactly the production case where the trace-detail page
// (/traces/:id) passes the traceId to the Share button rather than the internal run.ID.
func seededDurableRunStoreWithTraceID(t *testing.T) (runStore run.Store, runID, traceID string) {
	t.Helper()
	base := run.NewMemStore()
	runID = "run-internal-id-99"
	traceID = "trace-distinct-id-99" // deliberately different from runID
	rn := run.New(runID, "team-a", "assistant", json.RawMessage(`{"prompt":"hi"}`), "conv-trace", time.Now())
	rn.TraceID = traceID
	rn.CallerUsername = "alice@example.com"
	require.NoError(t, base.Create(rn))
	return durableMemRunStore{base}, runID, traceID
}

// TestCreateShare_MintByTraceID is the core regression fix (m75.5): the Share button on the
// trace-detail page (/traces/:id) passes the run's traceId — which is DISTINCT from run.ID.
// The mint must resolve the run by traceId when the direct run.ID lookup fails, and the stored
// shared_runs.run_id must be the real run.ID (not the traceId) so the public read resolves it.
func TestCreateShare_MintByTraceID(t *testing.T) {
	store := sharedrun.NewMemStore()
	audit := &captureAuditStore{}
	runStore, runID, traceID := seededDurableRunStoreWithTraceID(t)
	s := shareTestServer(t, store, audit, runStore, nil)

	// Mint using the traceId (the path the trace-detail page follows).
	rec := doShareRequest(t, s, http.MethodPost, "/api/runs/"+traceID+"/shares", `{"includeContent":false}`)
	require.Equal(t, http.StatusCreated, rec.Code, "minting by traceId must succeed: "+rec.Body.String())

	var resp CreateShareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Token)

	// Critical: the stored run_id must be the real run.ID, NOT the traceId.
	got, ok, err := store.GetByTokenHash(context.Background(), hashShareToken(resp.Token))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, runID, got.RunID,
		"stored run_id must be the real run.ID — not the traceId — so the public read resolves it")
	assert.NotEqual(t, traceID, got.RunID,
		"the traceId must NEVER be stored as run_id in shared_runs")
}

// TestCreateShare_MintByRunIDStillWorks proves the existing by-run.ID path is unaffected by the
// traceId fallback — a caller who already has the internal run.ID can still mint normally.
func TestCreateShare_MintByRunIDStillWorks(t *testing.T) {
	store := sharedrun.NewMemStore()
	runStore, runID, _ := seededDurableRunStoreWithTraceID(t)
	s := shareTestServer(t, store, &captureAuditStore{}, runStore, nil)

	rec := doShareRequest(t, s, http.MethodPost, "/api/runs/"+runID+"/shares", `{}`)
	require.Equal(t, http.StatusCreated, rec.Code, "minting by run.ID must still work: "+rec.Body.String())

	var resp CreateShareResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	got, ok, err := store.GetByTokenHash(context.Background(), hashShareToken(resp.Token))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, runID, got.RunID, "stored run_id must be the real run.ID when resolved by run.ID")
}

// TestCreateShare_BadIDOrTraceID404 proves that an id that is neither a known run.ID nor a known
// traceId returns 404 — the uniform "run not found" semantics are preserved.
func TestCreateShare_BadIDOrTraceID404(t *testing.T) {
	store := sharedrun.NewMemStore()
	runStore, _, _ := seededDurableRunStoreWithTraceID(t)
	s := shareTestServer(t, store, &captureAuditStore{}, runStore, nil)

	rec := doShareRequest(t, s, http.MethodPost, "/api/runs/totally-unknown-id/shares", `{}`)
	assert.Equal(t, http.StatusNotFound, rec.Code, "an unknown id/traceId must return 404")
}
