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

package egress

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/credresolve"
	"github.com/ctxmesh/agent-engine/internal/runcap"
)

const (
	testAudience = "ctxmesh-credential-plane"
	testAgent    = "team-alpha/support-agent"
	testNS       = "team-alpha"
	testServer   = "weather"
)

// mockResolver is a credresolve.CredentialResolver test double capturing its inputs.
type mockResolver struct {
	cred        credresolve.Credential
	err         error
	calls       int
	gotNS       string
	gotServer   string
	gotUser     string
	gotBoundary string
}

func (m *mockResolver) Resolve(_ context.Context, ns, boundary, server, userHash string) (credresolve.Credential, error) {
	m.calls++
	m.gotNS, m.gotServer, m.gotUser, m.gotBoundary = ns, server, userHash, boundary
	return m.cred, m.err
}

func (m *mockResolver) Revoke(context.Context, string, string, string, string) error { return nil }

// upstream captures what the sidecar forwarded.
type upstream struct {
	server  *httptest.Server
	gotAuth string
	gotCap  string
	hits    int
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits++
		u.gotAuth = r.Header.Get("Authorization")
		u.gotCap = r.Header.Get(runcap.HeaderName)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	}))
	t.Cleanup(u.server.Close)
	return u
}

// harness wires a Proxy over a mock upstream + mock resolver with a real capability signer.
type harness struct {
	proxy    *Proxy
	signer   *runcap.Signer
	resolver *mockResolver
	up       *upstream
}

func newHarness(t *testing.T, resolver *mockResolver) *harness {
	t.Helper()
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	up := newUpstream(t)
	routes, err := ParseRouteTable(fmt.Appendf(nil, `[{"name":%q,"targetURL":%q,"oauth":true}]`, testServer, up.server.URL))
	require.NoError(t, err)

	proxy := NewProxy(ProxyConfig{
		Verifier:      runcap.NewVerifier(pub, testAudience, nil),
		Resolver:      resolver,
		Namespace:     testNS,
		ExpectedAgent: testAgent,
		Routes:        routes,
		Log:           logr.Discard(),
	})
	return &harness{
		proxy:    proxy,
		signer:   runcap.NewSigner(priv, testAudience, nil),
		resolver: resolver,
		up:       up,
	}
}

func (h *harness) mint(t *testing.T, user, agent string) string {
	t.Helper()
	tok, err := h.signer.Mint(runcap.MintRequest{User: user, Agent: agent, RunID: "run-1", TTL: 5 * time.Minute})
	require.NoError(t, err)
	return tok
}

// call drives one tool-call request through the sidecar to /<server>, optionally with a
// capability header.
func (h *harness) call(t *testing.T, path, capToken string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","method":"tools/call"}`))
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	rec := httptest.NewRecorder()
	h.proxy.ServeHTTP(rec, req)
	return rec
}

func TestEgressInjectsResolvedCredential(t *testing.T) {
	h := newHarness(t, &mockResolver{cred: credresolve.Credential{Kind: credresolve.KindBearer, Value: "FRESH-USER-TOKEN"}})
	rec := h.call(t, "/"+testServer, h.mint(t, "u-alicehash", testAgent))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "ok")
	// The upstream received the invoking user's fresh token — and NOT the capability.
	assert.Equal(t, 1, h.up.hits)
	assert.Equal(t, "Bearer FRESH-USER-TOKEN", h.up.gotAuth)
	assert.Empty(t, h.up.gotCap, "the capability must never leak to the upstream MCP server")
	// Resolution keyed on the source namespace + server + the capability's hashed user.
	assert.Equal(t, testNS, h.resolver.gotNS)
	assert.Equal(t, testServer, h.resolver.gotServer)
	assert.Equal(t, "u-alicehash", h.resolver.gotUser)
}

func TestEgressRejectsMissingCapability(t *testing.T) {
	h := newHarness(t, &mockResolver{cred: credresolve.Credential{Value: "T"}})
	rec := h.call(t, "/"+testServer, "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "no_capability")
	assert.Equal(t, 0, h.up.hits, "no upstream call without a verified identity")
	assert.Equal(t, 0, h.resolver.calls, "no resolution without a verified identity")
}

func TestEgressRejectsInvalidCapability(t *testing.T) {
	h := newHarness(t, &mockResolver{cred: credresolve.Credential{Value: "T"}})
	// A capability signed by a DIFFERENT platform key.
	otherPub, otherPriv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	_ = otherPub
	forged, err := runcap.NewSigner(otherPriv, testAudience, nil).
		Mint(runcap.MintRequest{User: "u-attacker", Agent: testAgent, RunID: "r", TTL: time.Minute})
	require.NoError(t, err)

	rec := h.call(t, "/"+testServer, forged)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid_capability")
	assert.Equal(t, 0, h.up.hits)
}

func TestEgressRejectsUnknownRoute(t *testing.T) {
	h := newHarness(t, &mockResolver{})
	rec := h.call(t, "/not-a-server", h.mint(t, "u-alice", testAgent))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "no_route")
}

func TestEgressRejectsAgentMismatch(t *testing.T) {
	h := newHarness(t, &mockResolver{cred: credresolve.Credential{Value: "T"}})
	// A capability minted for a DIFFERENT agent must not be redeemable here.
	rec := h.call(t, "/"+testServer, h.mint(t, "u-alice", "team-alpha/other-agent"))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "agent_mismatch")
	assert.Equal(t, 0, h.up.hits)
	assert.Equal(t, 0, h.resolver.calls)
}

// TestEgressBoundaryMatch: the ADR 0033 / m30.3 scoping gate. When the sidecar serves a boundary
// (a registry), a capability whose `bnd` matches is redeemable even from a DIFFERENT agent (a
// teammate's capability relayed across A2A — team-OBO), while a capability scoped to a different
// boundary is rejected. The boundary gate supersedes the exact-agent gate.
func TestEgressBoundaryMatch(t *testing.T) {
	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	up := newUpstream(t)
	routes, err := ParseRouteTable(fmt.Appendf(nil, `[{"name":%q,"targetURL":%q,"oauth":true}]`, testServer, up.server.URL))
	require.NoError(t, err)
	signer := runcap.NewSigner(priv, testAudience, nil)

	proxy := NewProxy(ProxyConfig{
		Verifier:         runcap.NewVerifier(pub, testAudience, nil),
		Resolver:         &mockResolver{cred: credresolve.Credential{Kind: credresolve.KindBearer, Value: "TOK"}},
		Namespace:        testNS,
		ExpectedAgent:    "team-alpha/support", // present but SUPERSEDED by the boundary gate
		ExpectedBoundary: "r:squad-a",
		Routes:           routes,
		Log:              logr.Discard(),
	})
	call := func(agent, boundary string) *httptest.ResponseRecorder {
		tok, mErr := signer.Mint(runcap.MintRequest{User: "u-alice", Agent: agent, Boundary: boundary, RunID: "r", TTL: time.Minute})
		require.NoError(t, mErr)
		req := httptest.NewRequest(http.MethodPost, "/"+testServer, strings.NewReader(`{}`))
		req.Header.Set(runcap.HeaderName, tok)
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		return rec
	}

	// A teammate agent (different act) with the SAME registry boundary is accepted (team-OBO).
	rec := call("team-alpha/other-agent", "r:squad-a")
	assert.Equal(t, http.StatusOK, rec.Code)

	// A capability scoped to a DIFFERENT registry is rejected — isolation across registries.
	rec = call("team-alpha/support", "r:squad-b")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "boundary_mismatch")
}

func TestEgressConsentRequired(t *testing.T) {
	h := newHarness(t, &mockResolver{err: credresolve.ErrConsentRequired})
	rec := h.call(t, "/"+testServer, h.mint(t, "u-alice", testAgent))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "consent_required")
	assert.Contains(t, rec.Body.String(), testServer, "the consent error names the server to connect")
	assert.Equal(t, 0, h.up.hits, "no upstream call when the user must consent")
}

func TestEgressOpenServerForwardsWithoutAuth(t *testing.T) {
	h := newHarness(t, &mockResolver{err: credresolve.ErrNoCredential})
	rec := h.call(t, "/"+testServer, h.mint(t, "u-alice", testAgent))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, h.up.hits)
	assert.Empty(t, h.up.gotAuth, "an open server is forwarded with no injected credential")
}

func TestEgressResolveErrorIsBadGateway(t *testing.T) {
	h := newHarness(t, &mockResolver{err: fmt.Errorf("etcd unreachable")})
	rec := h.call(t, "/"+testServer, h.mint(t, "u-alice", testAgent))
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "resolve_failed")
}
