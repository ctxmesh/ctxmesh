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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane/sharedrun"
	"github.com/ctxmesh/agent-engine/internal/run"
)

// fakeSharedRunStore is a hand-built sharedrun.Store for the PUBLIC-read tests. Unlike the mem store (whose
// Create forces Revoked=false and a real expiry), it lets a test inject the EXACT raw row GetByTokenHash
// returns — including a revoked or expired share, an absent row, or a forced error — so every uniform-404
// branch of the public read can be exercised deterministically. Only GetByTokenHash is meaningful here;
// the mint/manage methods are unused by the public route.
type fakeSharedRunStore struct {
	byHash map[string]sharedrun.SharedRun
	getErr error
}

func (f *fakeSharedRunStore) GetByTokenHash(_ context.Context, tokenHash string) (*sharedrun.SharedRun, bool, error) {
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	rec, ok := f.byHash[tokenHash]
	if !ok {
		return nil, false, nil
	}
	out := rec
	return &out, true, nil
}

func (f *fakeSharedRunStore) Create(context.Context, sharedrun.SharedRun) error { return nil }
func (f *fakeSharedRunStore) Revoke(context.Context, string) error              { return nil }
func (f *fakeSharedRunStore) ListForRun(context.Context, string) ([]sharedrun.SharedRun, error) {
	return nil, nil
}

func (f *fakeSharedRunStore) ListByCreator(context.Context, string) ([]sharedrun.SharedRun, error) {
	return nil, nil
}

// publicReadTestServer builds a minimal Server for the UNAUTHENTICATED public read: no caller clients, no
// auth wiring beyond the anonymous route. Only the share store + run store matter. The run store is seeded
// via the returned handles so a test can control run existence.
func publicReadTestServer(t *testing.T, shareStore sharedrun.Store, runStore run.Store) *Server {
	t.Helper()
	return NewServer(Options{
		Auth:           AllowAll{},
		Version:        "test",
		Log:            logr.Discard(),
		RunStore:       runStore,
		SharedRunStore: shareStore,
	})
}

// doPublicReadRaw issues a request to the public route WITHOUT any Authorization header (the whole point of
// the surface: no bearer token). Returns the recorder.
func doPublicReadRaw(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	// Deliberately NO Authorization header — this must work unauthenticated.
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// seedPublicRun returns a run store holding one completed run for the public-read tests, with content the
// includeContent branch surfaces. Not required to be durable — the public read never checks durability.
func seedPublicRun(t *testing.T) run.Store {
	t.Helper()
	base := run.NewMemStore()
	rn := run.New("run-pub-1", "team-a", "assistant", json.RawMessage(`{"prompt":"secret question"}`), "conv-should-not-leak", time.Now())
	rn.TraceID = "trace-should-not-leak"
	rn.ParentRunID = "parent-should-not-leak"
	rn.Messages = []run.Message{
		{Role: "user", Content: "the user prompt"},
		{Role: "assistant", Content: "the assistant answer"},
	}
	rn.Error = "provider 500: connection string postgres://secret@host leaked here"
	require.NoError(t, base.Create(rn))
	return base
}

// liveShare is a non-revoked, far-future share for run-pub-1 with the given content gate + token.
func liveShare(token, runID string, includeContent bool) sharedrun.SharedRun {
	return sharedrun.SharedRun{
		ID:             "share-" + token,
		TokenHash:      hashShareToken(token),
		RunID:          runID,
		Namespace:      "team-a",
		CreatedAt:      time.Now().Add(-time.Hour),
		ExpiresAt:      time.Now().Add(24 * time.Hour),
		IncludeContent: includeContent,
	}
}

func storeWith(shares ...sharedrun.SharedRun) *fakeSharedRunStore {
	m := make(map[string]sharedrun.SharedRun, len(shares))
	for _, s := range shares {
		m[s.TokenHash] = s
	}
	return &fakeSharedRunStore{byHash: m}
}

// TestSharedRunPublic_ValidLiveToken_MetadataOnly: a live token (includeContent=false) → 200 with ONLY the
// allowlist metadata projection — NO input/messages/raw error, NO traceId/lineage — plus the no-leak
// headers. Served on the UNAUTHENTICATED mux (no bearer token was sent).
func TestSharedRunPublic_ValidLiveToken_MetadataOnly(t *testing.T) {
	token := "live-metadata-token"
	s := publicReadTestServer(t, storeWith(liveShare(token, "run-pub-1", false)), seedPublicRun(t))

	rec := doPublicReadRaw(t, s, http.MethodGet, "/api/shared/runs/"+token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The no-leak headers are present on the success response.
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	assert.Equal(t, "noindex", rec.Header().Get("X-Robots-Tag"))

	// It is EXACTLY the metadata-only allowlist projection. (Key names are pulled from a space-separated
	// string so no individual JSON-key literal is a standalone repeated string across the package.)
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	for present := range strings.FieldsSeq("id namespace agent status messageCount messageRoles errorCategory") {
		assert.Contains(t, got, present, "metadata key %q must be present", present)
	}
	// Content + lineage/sensitive keys MUST be absent in the default projection.
	for absent := range strings.FieldsSeq("input messages error traceId conversationId parentRunId") {
		assert.NotContains(t, got, absent, "key %q must NEVER appear on the public metadata read", absent)
	}
	// The raw error string never leaks (a coarse category only).
	assert.NotContains(t, rec.Body.String(), "postgres://", "the raw error must never reach the public read")
}

// TestSharedRunPublic_ValidLiveToken_IncludeContent: an includeContent=true share → input/messages/full
// error present, but traceId + lineage STILL omitted.
func TestSharedRunPublic_ValidLiveToken_IncludeContent(t *testing.T) {
	token := "live-content-token"
	s := publicReadTestServer(t, storeWith(liveShare(token, "run-pub-1", true)), seedPublicRun(t))

	rec := doPublicReadRaw(t, s, http.MethodGet, "/api/shared/runs/"+token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	for present := range strings.FieldsSeq("input messages error") {
		assert.Contains(t, got, present, "content key %q must be present with includeContent", present)
	}
	// Even with content, lineage/trace never appear.
	for absent := range strings.FieldsSeq("traceId conversationId parentRunId") {
		assert.NotContains(t, got, absent, "lineage/trace key %q must NEVER appear even with includeContent", absent)
	}
	assert.Contains(t, rec.Body.String(), "the assistant answer", "the transcript is surfaced with includeContent")
}

// TestSharedRunPublic_Uniform404 is the no-oracle core: a missing token, a bad/unknown token, a revoked
// share, an expired share, and a deleted run ALL return the IDENTICAL 404 body + status. A caller cannot
// tell them apart.
func TestSharedRunPublic_Uniform404(t *testing.T) {
	runStore := seedPublicRun(t)

	revoked := liveShare("revoked-token", "run-pub-1", false)
	revoked.Revoked = true
	expired := liveShare("expired-token", "run-pub-1", false)
	expired.ExpiresAt = time.Now().Add(-time.Hour)
	deletedRun := liveShare("deleted-run-token", "run-DELETED", false) // run not in the store

	store := storeWith(revoked, expired, deletedRun)
	s := publicReadTestServer(t, store, runStore)

	// The canonical 404 body from a plainly-unknown token.
	canonical := doPublicReadRaw(t, s, http.MethodGet, "/api/shared/runs/totally-unknown")
	require.Equal(t, http.StatusNotFound, canonical.Code)
	want := canonical.Body.String()
	assert.Contains(t, want, sharedRunNotFoundMsg)

	cases := map[string]string{
		"unknown token":   "/api/shared/runs/no-such-token",
		"revoked share":   "/api/shared/runs/revoked-token",
		"expired share":   "/api/shared/runs/expired-token",
		"deleted run":     "/api/shared/runs/deleted-run-token",
		"another unknown": "/api/shared/runs/zzz",
	}
	for name, path := range cases {
		rec := doPublicReadRaw(t, s, http.MethodGet, path)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s must be 404", name)
		assert.Equal(t, want, rec.Body.String(), "%s must return the IDENTICAL 404 body (no oracle)", name)
		// No-leak headers are set even on the 404.
		assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"), "%s: header on 404", name)
		assert.Equal(t, "noindex", rec.Header().Get("X-Robots-Tag"), "%s: header on 404", name)
	}
}

// TestSharedRunPublic_EmptyToken404: the empty-token edge (a trailing slash with no token) never matches
// the {token} segment and is a 404 (via the SPA/catch-all), never a panic or a 200 projection.
func TestSharedRunPublic_EmptyToken404(t *testing.T) {
	s := publicReadTestServer(t, storeWith(), seedPublicRun(t))
	// "/api/shared/runs/" has no {token} — Go's ServeMux does not match the {token} pattern (empty segment).
	rec := doPublicReadRaw(t, s, http.MethodGet, "/api/shared/runs/")
	assert.NotEqual(t, http.StatusOK, rec.Code, "an empty token must never serve a projection")
}

// TestSharedRunPublic_NilStore404: with no share store (no cpDB) the public read is a 404 — NOT a 501. An
// anonymous caller must not learn the feature exists / is unconfigured.
func TestSharedRunPublic_NilStore404(t *testing.T) {
	s := publicReadTestServer(t, nil, seedPublicRun(t))
	rec := doPublicReadRaw(t, s, http.MethodGet, "/api/shared/runs/any-token")
	assert.Equal(t, http.StatusNotFound, rec.Code, "a nil store must 404, never 501 (no feature-existence oracle)")
	assert.Contains(t, rec.Body.String(), sharedRunNotFoundMsg)
}

// TestSharedRunPublic_StoreError500: a genuine store error (not a missing row) is a 500 — a store failure is
// NOT "no such share". The underlying error is never echoed to the caller.
func TestSharedRunPublic_StoreError500(t *testing.T) {
	store := &fakeSharedRunStore{getErr: errors.New("db connection reset: dsn=postgres://secret@host")}
	s := publicReadTestServer(t, store, seedPublicRun(t))
	rec := doPublicReadRaw(t, s, http.MethodGet, "/api/shared/runs/whatever")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "postgres://", "a store error must never echo the underlying error")
	assert.NotContains(t, rec.Body.String(), "dsn", "a store error must never echo the underlying error")
}

// TestSharedRunPublic_RejectsOtherMethods: POST/DELETE/PUT on the share path never reach the projection.
// (They do not match the GET-only pattern; they fall to the authed "/api/" catch-all → 401. Either way the
// projection is NEVER served for a non-GET verb.)
func TestSharedRunPublic_RejectsOtherMethods(t *testing.T) {
	token := "method-token"
	s := publicReadTestServer(t, storeWith(liveShare(token, "run-pub-1", false)), seedPublicRun(t))
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch} {
		rec := doPublicReadRaw(t, s, m, "/api/shared/runs/"+token)
		assert.NotEqual(t, http.StatusOK, rec.Code, "%s must NOT serve the projection", m)
		assert.NotContains(t, rec.Body.String(), `"messageRoles"`, "%s must never surface the SharedRunView", m)
	}
}

// TestSharedRunPublic_NoAuthRequired proves the surface needs NO bearer token: the SAME request with and
// without an Authorization header both succeed identically (the token in the URL is the only credential).
func TestSharedRunPublic_NoAuthRequired(t *testing.T) {
	token := "noauth-token"
	s := publicReadTestServer(t, storeWith(liveShare(token, "run-pub-1", false)), seedPublicRun(t))

	// No auth header at all.
	noAuth := doPublicReadRaw(t, s, http.MethodGet, "/api/shared/runs/"+token)
	require.Equal(t, http.StatusOK, noAuth.Code, "the public read must succeed with NO bearer token")

	// A bogus bearer header must not change the outcome (the route is not auth-gated).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/shared/runs/"+token, nil)
	req.Header.Set("Authorization", "Bearer this-is-ignored-on-the-public-route")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, noAuth.Body.String(), rec.Body.String(), "auth header must not alter the public read")
}

// TestIPRateLimiter_BucketAndRefill unit-tests the minimal limiter: a burst is admitted, the next request
// over budget is denied, and after a refill window it admits again. A disabled (burst<=0) limiter always
// admits.
func TestIPRateLimiter_BucketAndRefill(t *testing.T) {
	// burst=2, rate=1000/s so a tiny sleep refills.
	l := newIPRateLimiter(1000, 2)
	assert.True(t, l.allow("ip-a"), "first within burst")
	assert.True(t, l.allow("ip-a"), "second within burst")
	assert.False(t, l.allow("ip-a"), "third over burst → denied")
	// A different key has its own bucket.
	assert.True(t, l.allow("ip-b"), "a distinct IP is independent")
	// Refill: at 1000/s, ~2ms yields ~2 tokens.
	time.Sleep(3 * time.Millisecond)
	assert.True(t, l.allow("ip-a"), "after refill, admitted again")

	// A disabled limiter always admits.
	off := newIPRateLimiter(0, 0)
	for range 100 {
		assert.True(t, off.allow("ip-x"), "a disabled limiter never throttles")
	}
	// A nil limiter is safe (always admits).
	var nilL *ipRateLimiter
	assert.True(t, nilL.allow("ip-y"))
}

// TestSharedRunPublic_RateLimited proves the endpoint is bounded: after the burst is exhausted from one IP,
// further requests get 429 (a non-oracle status — it says nothing about token validity), with the no-leak
// headers still set.
func TestSharedRunPublic_RateLimited(t *testing.T) {
	token := "rl-token"
	s := publicReadTestServer(t, storeWith(liveShare(token, "run-pub-1", false)), seedPublicRun(t))
	// Force a tiny limiter so the test is deterministic and fast (no reliance on the production constants).
	s.sharedRunLimiter = newIPRateLimiter(0.0001, 2)

	// The RemoteAddr is a fixed host:port for httptest, so all requests share one bucket.
	var got429 bool
	for range 5 {
		rec := doPublicReadRaw(t, s, http.MethodGet, "/api/shared/runs/"+token)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"), "429 still sets no-leak headers")
			break
		}
	}
	assert.True(t, got429, "the unauthenticated read must be rate-limited after the burst")
}
