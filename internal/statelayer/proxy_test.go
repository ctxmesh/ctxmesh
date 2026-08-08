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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRegistryResolver maps a pod identity ("ns/sa") → registry id (or a fixed error),
// standing in for the SA-label reader so the shared-scope path is testable without a
// cluster (ADR 0052 §C6 RESOLUTION).
type fakeRegistryResolver struct {
	byNsSA map[string]string
	err    error
}

func (f fakeRegistryResolver) Registry(_ context.Context, ns, sa string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.byNsSA[ns+"/"+sa], nil
}

// newTestProxy builds a memory proxy authenticated by POD IDENTITY (ADR 0052 §C6
// RESOLUTION): the returned fakePodAuth's maps are mutable, so podToken registers
// identities into it after construction.
func newTestProxy(t *testing.T, opts ...func(*Options)) (*Server, *miniredis.Miniredis, fakePodAuth) {
	t.Helper()
	mr := miniredis.RunT(t)
	auth := fakePodAuth{byToken: map[string]string{}, saByToken: map[string]string{}}
	o := Options{Store: NewRedisStore(mr.Addr(), "", ""), PodAuthenticator: auth}
	for _, f := range opts {
		f(&o)
	}
	s, err := NewServer(o)
	require.NoError(t, err)
	return s, mr, auth
}

// podToken registers a pod identity in the fake authenticator and returns its bearer
// token. agent is "<ns>/<name>"; the identity SA is "agent-<name>", so the proxy derives
// the SAME mem:{ns}/{name}: key the runcap path used to — no key migration.
func podToken(auth fakePodAuth, agent string) string {
	ns, name, _ := strings.Cut(agent, "/")
	tok := "tok-" + ns + "-" + name
	auth.byToken[tok] = ns
	auth.saByToken[tok] = "agent-" + name
	return tok
}

func do(t *testing.T, s *Server, method, path, token, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// The append→get round-trip works, and the Valkey key is the TOKEN-derived prefix.
func TestProxyMemoryRoundTrip(t *testing.T) {
	s, mr, auth := newTestProxy(t)
	tok := podToken(auth, "team-alpha/support-agent")

	rec := do(t, s, "POST", "/memory/c1/append", tok, `{"role":"user","content":"hello"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	rec = do(t, s, "GET", "/memory/c1", tok, "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "hello")
	assert.Equal(t, "1", rec.Header().Get("ETag"))

	// The key is server-derived from the token, not caller input.
	keys := mr.Keys()
	require.Len(t, keys, 1)
	assert.Equal(t, "mem:team-alpha/support-agent:c1", keys[0])
}

// THE HEADLINE GUARANTEE: a different agent/tenant cannot read another's memory —
// its token scopes it to its OWN prefix, so the same conversationId is a different key.
func TestProxyCrossTenantDeny(t *testing.T) {
	s, _, auth := newTestProxy(t)
	alpha := podToken(auth, "team-alpha/support-agent")
	beta := podToken(auth, "team-beta/other-agent")

	// team-alpha writes a secret under conv "shared-id".
	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/memory/shared-id/append", alpha, `{"role":"user","content":"alpha-secret"}`, nil).Code)

	// team-beta reads the SAME conversationId — gets its OWN (empty) space, never alpha's.
	rec := do(t, s, "GET", "/memory/shared-id", beta, "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]", rec.Body.String(), "beta must NOT see alpha's data under the same convId")
	assert.NotContains(t, rec.Body.String(), "alpha-secret")

	// And alpha still sees its own.
	rec = do(t, s, "GET", "/memory/shared-id", alpha, "", nil)
	assert.Contains(t, rec.Body.String(), "alpha-secret")
}

// Attribution is server-authoritative: a forged `agent` field is overwritten with the token's agent.
func TestProxyAttributionOverwrite(t *testing.T) {
	s, _, auth := newTestProxy(t)
	tok := podToken(auth, "team-alpha/support-agent")

	// The caller lies about which agent it is.
	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/memory/c1/append", tok,
			`{"role":"assistant","content":"hi","agent":"impersonated-agent"}`, nil).Code)

	rec := do(t, s, "GET", "/memory/c1", tok, "", nil)
	var entries []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))
	require.Len(t, entries, 1)
	assert.Equal(t, "support-agent", entries[0]["agent"], "the token's agent must overwrite the forged one")
}

// Optimistic concurrency: a stale If-Match PUT is a 412 (ADR 0036 / ADR 0050 §7).
func TestProxyOCCConflict(t *testing.T) {
	s, _, auth := newTestProxy(t)
	tok := podToken(auth, "team-alpha/support-agent")

	do(t, s, "POST", "/memory/c1/append", tok, `{"role":"user","content":"one"}`, nil)
	// ETag is now "1". A concurrent append advances the version to 2.
	do(t, s, "POST", "/memory/c1/append", tok, `{"role":"user","content":"two"}`, nil)

	// A stale replace at version 1 must conflict.
	rec := do(t, s, "PUT", "/memory/c1", tok, `[{"role":"user","content":"clobber"}]`,
		map[string]string{"If-Match": "1"})
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

// Shared scope keys under the registry the CONTROLLER stamped on the agent SA (resolved
// server-side) — a caller can only reach its own registry's scratchpad, and it cannot
// forge the registry (it's derived from the verified identity, not a request header).
func TestProxySharedScope(t *testing.T) {
	resolver := fakeRegistryResolver{byNsSA: map[string]string{
		"team-alpha/agent-support-agent": "reg-1",
	}}
	s, mr, auth := newTestProxy(t, func(o *Options) { o.RegistryResolver = resolver })
	tok := podToken(auth, "team-alpha/support-agent")

	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/memory/c1/append", tok, `{"role":"user","content":"team"}`,
			map[string]string{memoryScopeHeader: "shared"}).Code)
	keys := mr.Keys()
	require.Len(t, keys, 1)
	assert.Equal(t, "mem:shared:reg-1:c1", keys[0], "shared scope keys under the SA-derived registry")
}

// Shared requested but the agent is NOT a registry member (no registry resolves) → the
// proxy falls back to the PRIVATE scope rather than keying under an empty/guessed
// registry (matches the runcap-path fallback).
func TestProxySharedScopeNonMemberFallsBackToPrivate(t *testing.T) {
	resolver := fakeRegistryResolver{byNsSA: map[string]string{}} // no membership
	s, mr, auth := newTestProxy(t, func(o *Options) { o.RegistryResolver = resolver })
	tok := podToken(auth, "team-alpha/support-agent")

	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/memory/c1/append", tok, `{"role":"user","content":"solo"}`,
			map[string]string{memoryScopeHeader: "shared"}).Code)
	require.Equal(t, "mem:team-alpha/support-agent:c1", mr.Keys()[0],
		"a non-member shared request falls back to the private scope")
}

// A registry-resolution INFRA failure fails CLOSED (503) — shared memory is never keyed
// under a missing registry (which would split the scratchpad or cross a boundary).
func TestProxySharedScopeResolverErrorFailsClosed(t *testing.T) {
	resolver := fakeRegistryResolver{err: context.DeadlineExceeded}
	s, _, auth := newTestProxy(t, func(o *Options) { o.RegistryResolver = resolver })
	tok := podToken(auth, "team-alpha/support-agent")

	rec := do(t, s, "POST", "/memory/c1/append", tok, `{"role":"user","content":"x"}`,
		map[string]string{memoryScopeHeader: "shared"})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// The dev bypass scopes unauthenticated requests to a static identity (never enabled in prod).
func TestProxyDevBypass(t *testing.T) {
	s, mr, _ := newTestProxy(t, func(o *Options) { o.DevAgent = "dev-ns/dev-agent" })
	rec := do(t, s, "POST", "/memory/c1/append", "", `{"role":"user","content":"dev"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "mem:dev-ns/dev-agent:c1", mr.Keys()[0])
}

// No token + no dev bypass ⇒ 401; a rejected/absent scope never leaks or 500s.
func TestProxyUnauthenticated(t *testing.T) {
	s, _, _ := newTestProxy(t)
	rec := do(t, s, "GET", "/memory/c1", "", "", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// A pod token the authenticator does not recognize (rejected TokenReview — wrong
// audience, expired, or forged) is a 401. The audience/expiry semantics themselves are
// enforced by the real TokenReview (covered in podauth_test.go); here the memory path
// simply must reject an unauthenticated token, never leak or 500.
func TestProxyRejectedToken(t *testing.T) {
	s, _, _ := newTestProxy(t)
	rec := do(t, s, "GET", "/memory/c1", "unregistered-token", "", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// An agent literally named "shared" keys its PRIVATE memory under mem:{ns}/shared: — the
// "/" separator keeps it disjoint from the shared-scratchpad space mem:shared:{reg}:, so
// the name can never collide across the private/shared key spaces (ADR 0052 §C6 nit N3).
func TestProxyAgentNamedSharedNoCollision(t *testing.T) {
	s, mr, auth := newTestProxy(t)
	tok := podToken(auth, "team-x/shared")
	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/memory/c1/append", tok, `{"role":"user","content":"x"}`, nil).Code)
	require.Equal(t, "mem:team-x/shared:c1", mr.Keys()[0],
		"a private agent named 'shared' keys under mem:{ns}/shared:, disjoint from mem:shared:{reg}:")
}

// A verified pod token that is NOT a per-agent identity SA (e.g. the namespace default
// SA) has no agent scope → 403, never a guessed key.
func TestProxyNonAgentSARejected(t *testing.T) {
	s, _, auth := newTestProxy(t)
	tok := "default-sa-tok"
	auth.byToken[tok] = "team-alpha"
	auth.saByToken[tok] = "default" // not an agent-<name> identity

	rec := do(t, s, "GET", "/memory/c1", tok, "", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
