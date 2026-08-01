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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/runcap"
)

const proxyAudience = "statelayer-proxy"

func newTestProxy(t *testing.T, opts ...func(*Options)) (*Server, *miniredis.Miniredis, *runcap.Signer) {
	t.Helper()
	mr := miniredis.RunT(t)
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	signer := runcap.NewSigner(priv, proxyAudience, nil)
	o := Options{Store: NewRedisStore(mr.Addr(), "", ""), Verifier: runcap.NewVerifier(pub, proxyAudience, nil)}
	for _, f := range opts {
		f(&o)
	}
	s, err := NewServer(o)
	require.NoError(t, err)
	return s, mr, signer
}

func mint(t *testing.T, signer *runcap.Signer, agent, boundary string) string {
	t.Helper()
	tok, err := signer.Mint(runcap.MintRequest{
		User: "u-hash", Agent: agent, RunID: "run-1", Boundary: boundary, TTL: 5 * time.Minute,
	})
	require.NoError(t, err)
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
	s, mr, signer := newTestProxy(t)
	tok := mint(t, signer, "team-alpha/support-agent", "")

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
	s, _, signer := newTestProxy(t)
	alpha := mint(t, signer, "team-alpha/support-agent", "")
	beta := mint(t, signer, "team-beta/other-agent", "")

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
	s, _, signer := newTestProxy(t)
	tok := mint(t, signer, "team-alpha/support-agent", "")

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
	s, _, signer := newTestProxy(t)
	tok := mint(t, signer, "team-alpha/support-agent", "")

	do(t, s, "POST", "/memory/c1/append", tok, `{"role":"user","content":"one"}`, nil)
	// ETag is now "1". A concurrent append advances the version to 2.
	do(t, s, "POST", "/memory/c1/append", tok, `{"role":"user","content":"two"}`, nil)

	// A stale replace at version 1 must conflict.
	rec := do(t, s, "PUT", "/memory/c1", tok, `[{"role":"user","content":"clobber"}]`,
		map[string]string{"If-Match": "1"})
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

// Shared scope keys under the TOKEN's registry boundary — a caller can only reach its own registry's scratchpad.
func TestProxySharedScope(t *testing.T) {
	s, mr, signer := newTestProxy(t)
	tok := mint(t, signer, "team-alpha/support-agent", "r:reg-1")

	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/memory/c1/append", tok, `{"role":"user","content":"team"}`,
			map[string]string{memoryScopeHeader: "shared"}).Code)
	keys := mr.Keys()
	require.Len(t, keys, 1)
	assert.Equal(t, "mem:shared:reg-1:c1", keys[0], "shared scope keys under the token's registry")
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

// A token minted for a DIFFERENT audience is rejected (replay protection, ADR 0050 §2).
func TestProxyWrongAudience(t *testing.T) {
	s, _, _ := newTestProxy(t)
	_, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	otherSigner := runcap.NewSigner(priv, "credential-plane", nil) // different aud AND key
	tok := mint(t, otherSigner, "team-alpha/support-agent", "")

	rec := do(t, s, "GET", "/memory/c1", tok, "", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
