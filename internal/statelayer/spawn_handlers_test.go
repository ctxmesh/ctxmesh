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
