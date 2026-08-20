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

package statelayer

import (
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSpawnProxy(t *testing.T, byToken map[string]string, auth PodAuthenticator) (*Server, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	if auth == nil {
		auth = fakePodAuth{byToken: byToken, saByToken: agentSAsFor(byToken)}
	}
	s, err := NewServer(Options{
		Store:            NewRedisStore(mr.Addr(), "", ""),
		SpawnStore:       NewRedisSpawnStore(mr.Addr(), "", ""),
		PodAuthenticator: auth,
	})
	require.NoError(t, err)
	return s, mr
}

// Acquire returns acquired:true under the cap and acquired:false (rolling back) over it; the key is
// namespace-scoped from the pod token.
func TestSpawnAcquireEnforcesMaxAndRollsBack(t *testing.T) {
	s, mr := newSpawnProxy(t, map[string]string{"tok": "team-a"}, nil)

	rec := do(t, s, "POST", "/spawn/acquire", "tok", `{"scope":"reg1","rootRunId":"r1","counter":"inflight","max":1}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"acquired":true}`, rec.Body.String())

	// Second acquire would exceed max=1 → denied + rolled back (the counter stays at 1).
	rec = do(t, s, "POST", "/spawn/acquire", "tok", `{"scope":"reg1","rootRunId":"r1","counter":"inflight","max":1}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"acquired":false}`, rec.Body.String(), "over the cap → denied fail-closed")

	require.Contains(t, mr.Keys(), "spawn:team-a:reg1:r1:inflight", "the key is namespace-scoped from the pod token")
	v, _ := mr.Get("spawn:team-a:reg1:r1:inflight")
	assert.Equal(t, "1", v, "the denied acquire rolled back — the counter is still 1")
}

// Release decrements, freeing a slot.
func TestSpawnReleaseDecrements(t *testing.T) {
	s, _ := newSpawnProxy(t, map[string]string{"tok": "team-a"}, nil)
	body := `{"scope":"reg1","rootRunId":"r1","counter":"inflight","max":1}`
	require.Equal(t, http.StatusOK, do(t, s, "POST", "/spawn/acquire", "tok", body, nil).Code)
	require.Equal(t, http.StatusOK, do(t, s, "POST", "/spawn/release", "tok", `{"scope":"reg1","rootRunId":"r1","counter":"inflight"}`, nil).Code)
	// After release, a new acquire under max=1 succeeds again.
	rec := do(t, s, "POST", "/spawn/acquire", "tok", body, nil)
	assert.JSONEq(t, `{"acquired":true}`, rec.Body.String(), "the released slot is free again")
}

// Two namespaces sharing the SAME scope + rootRunID get INDEPENDENT counters — a pod cannot touch another
// namespace's spawn tree (the key is prefixed by the pod's authenticated namespace).
func TestSpawnNamespaceIsolation(t *testing.T) {
	s, mr := newSpawnProxy(t, map[string]string{"tokA": "ns-a", "tokB": "ns-b"}, nil)
	body := `{"scope":"reg","rootRunId":"shared","counter":"count","max":1}`

	assert.JSONEq(t, `{"acquired":true}`, do(t, s, "POST", "/spawn/acquire", "tokA", body, nil).Body.String())
	// ns-b acquiring the same scope/rootRunId is a DIFFERENT key → also succeeds at its own count 1.
	assert.JSONEq(t, `{"acquired":true}`, do(t, s, "POST", "/spawn/acquire", "tokB", body, nil).Body.String(),
		"ns-b's counter is independent of ns-a's (namespace-scoped keys)")

	require.Contains(t, mr.Keys(), "spawn:ns-a:reg:shared:count")
	require.Contains(t, mr.Keys(), "spawn:ns-b:reg:shared:count")
}

// Release-spam must NOT drive a counter negative (audit P2-3) — else a later Acquire would admit far past
// the budget. The counter floors at 0 and the budget stays enforced.
func TestSpawnReleaseFloorsAtZero(t *testing.T) {
	s, mr := newSpawnProxy(t, map[string]string{"tok": "ns"}, nil)
	rel := `{"scope":"s","rootRunId":"r","counter":"inflight"}`
	for range 3 { // spam release on a fresh/at-zero tree
		require.Equal(t, http.StatusOK, do(t, s, "POST", "/spawn/release", "tok", rel, nil).Code)
	}
	v, _ := mr.Get("spawn:ns:s:r:inflight")
	assert.Equal(t, "0", v, "release-spam must floor at 0, never negative (audit P2-3)")

	// The budget is still enforced: with max=1 the first acquire admits, the second is denied.
	acq := `{"scope":"s","rootRunId":"r","counter":"inflight","max":1}`
	assert.JSONEq(t, `{"acquired":true}`, do(t, s, "POST", "/spawn/acquire", "tok", acq, nil).Body.String())
	assert.JSONEq(t, `{"acquired":false}`, do(t, s, "POST", "/spawn/acquire", "tok", acq, nil).Body.String(),
		"the budget is still enforced after release-spam (no bypass)")
}

// A non-agent SA (or a rejected token) cannot touch the spawn endpoints.
func TestSpawnRejectsNonAgentAndBadInput(t *testing.T) {
	// A verified-but-NON-agent SA (saByToken not agent-prefixed) → 403.
	s, _ := newSpawnProxy(t, nil, fakePodAuth{byToken: map[string]string{"tok": "ns"}, saByToken: map[string]string{"tok": "default"}})
	assert.Equal(t, http.StatusForbidden, do(t, s, "POST", "/spawn/acquire", "tok", `{"scope":"s","rootRunId":"r","counter":"inflight","max":1}`, nil).Code)

	// A good agent token but an unknown counter → 400.
	s2, _ := newSpawnProxy(t, map[string]string{"tok": "ns"}, nil)
	assert.Equal(t, http.StatusBadRequest, do(t, s2, "POST", "/spawn/acquire", "tok", `{"scope":"s","rootRunId":"r","counter":"bogus","max":1}`, nil).Code)
	// A rootRunId with a disallowed ':' (key-injection attempt) → 400.
	assert.Equal(t, http.StatusBadRequest, do(t, s2, "POST", "/spawn/acquire", "tok", `{"scope":"s","rootRunId":"r:evil","counter":"inflight","max":1}`, nil).Code)
}

// TestClampSpawnMax covers the C19 (ADR 0088) server-side ceiling: an inflated client max is clamped to
// the platform ceiling (fan-out for "inflight", total for "count"); a legit or 0 value passes through.
// Literal ceilings (128/1024) are a deliberate guard — bumping api/v1beta1.Max*Ceiling must update this.
func TestClampSpawnMax(t *testing.T) {
	cases := []struct {
		counter  string
		in, want int
	}{
		{spawnCounterInflight, 4, 4},         // a legit fan-out passes unchanged
		{spawnCounterInflight, 128, 128},     // exactly the fan-out ceiling
		{spawnCounterInflight, 1 << 40, 128}, // the max=1<<40 abuse -> fan-out ceiling
		{spawnCounterCount, 20, 20},          // a legit total passes unchanged
		{spawnCounterCount, 1024, 1024},      // exactly the total ceiling
		{spawnCounterCount, 1 << 40, 1024},   // the abuse -> total ceiling
		{spawnCounterCount, 0, 0},            // unbudgeted (0) preserved
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, clampSpawnMax(tc.counter, tc.in), "%s in=%d", tc.counter, tc.in)
	}
}
