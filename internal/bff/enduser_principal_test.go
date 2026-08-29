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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane/enduseragent"
	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
	"github.com/ctxmesh/agent-engine/internal/enduseroidc"
	"github.com/ctxmesh/agent-engine/internal/runcap"
)

type fakeEndUserVerifier struct {
	id  enduseroidc.Identity
	err error
}

func (f fakeEndUserVerifier) Verify(_ context.Context, _, _, _ string) (enduseroidc.Identity, error) {
	return f.id, f.err
}

func TestEndUserPrincipal_ParseRoundTrip(t *testing.T) {
	iss, sub := "https://dex.example.com/realms/x", "user#with#hashes"
	p := endUserPrincipal(iss, sub)
	assert.Equal(t, "oidc:https://dex.example.com/realms/x#user#with#hashes", p)

	gotIss, gotSub, ok := parseEndUserPrincipal(p)
	require.True(t, ok)
	assert.Equal(t, iss, gotIss)
	assert.Equal(t, sub, gotSub, "the FIRST '#' after the prefix separates; the subject may contain '#'")

	// A plain K8s principal is NOT an end-user principal.
	for _, notEU := range []string{"system:serviceaccount:ns:sa", "alice", "oidc:no-separator", ""} {
		_, _, ok := parseEndUserPrincipal(notEU)
		assert.False(t, ok, "%q must not parse as an end-user principal", notEU)
	}
}

func TestPrincipalGrantHash_Dispatch(t *testing.T) {
	// Works regardless of the HMAC key (the prefix is what separates the spaces).
	euHash, isEU := principalGrantHash("oidc:https://i#alice")
	assert.True(t, isEU)
	assert.True(t, strings.HasPrefix(euHash, "eu-"), "end-user principal → eu- hash")

	uHash, isEU2 := principalGrantHash("system:serviceaccount:t:sa")
	assert.False(t, isEU2)
	assert.True(t, strings.HasPrefix(uHash, "u-"), "K8s username → u- hash")
}

func TestResolveEndUserPrincipal(t *testing.T) {
	ctx := context.Background()
	st := namespacetenant.NewMemStore()
	require.NoError(t, st.SetMembers(ctx, "t1", []string{"ns1"}))
	require.NoError(t, st.SetEndUserIdentity(ctx, "t1", namespacetenant.EndUserIdentity{
		Enabled: true, Issuer: "https://dex-eu.example.com", ClientID: "cid",
	}))

	// A valid end-user token → the principal is returned WITHOUT any K8s client (the Server has no
	// callerClients — if the resolver tried to build one it would panic; passing proves the structural
	// K8s-path separation of ADR 0106 §3).
	s := &Server{
		namespaceTenantStore: st,
		log:                  logr.Discard(),
		endUserVerifier:      fakeEndUserVerifier{id: enduseroidc.Identity{Issuer: "https://dex-eu.example.com", Subject: "alice"}},
	}
	req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
	req.Header.Set("Authorization", "Bearer an-oidc-id-token")

	principal, id, ok, err := s.resolveEndUserPrincipal(ctx, req, "ns1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "oidc:https://dex-eu.example.com#alice", principal)
	assert.Equal(t, "alice", id.Subject)

	// A bearer that fails verification against an ENABLED end-user IdP → ok=false AND
	// errEndUserBearerRejected, so the create path can 401 (the end-user path owns that auth failure)
	// rather than fall through to the console path's "agent is required" 400 (ADR 0107 §3).
	sBad := &Server{namespaceTenantStore: st, log: logr.Discard(), endUserVerifier: fakeEndUserVerifier{err: errors.New("bad token")}}
	_, _, ok, err = sBad.resolveEndUserPrincipal(ctx, req, "ns1")
	assert.False(t, ok, "a rejected end-user bearer is not a verified end-user")
	assert.ErrorIs(t, err, errEndUserBearerRejected, "a presented-but-invalid end-user bearer must signal a 401")

	// A namespace with no end-user IdP → ok=false.
	_, _, ok, err = s.resolveEndUserPrincipal(ctx, req, "orphan-ns")
	require.NoError(t, err)
	assert.False(t, ok)

	// No bearer → ok=false.
	_, _, ok, err = s.resolveEndUserPrincipal(ctx, httptest.NewRequest(http.MethodPost, "/invoke", nil), "ns1")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestMintRunCapability_EndUserRequiresHMACKey(t *testing.T) {
	// Save + restore the process-global grant HMAC key (this test mutates it).
	prev := grantHMACKey.Load()
	t.Cleanup(func() {
		if prev != nil {
			setGrantHMACKey(*prev)
		} else {
			grantHMACKey.Store(nil)
		}
	})

	_, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	s := &Server{capabilitySigner: runcap.NewSigner(priv, "aud", nil), log: logr.Discard()}

	// No HMAC key → minting an END-USER capability is REFUSED (unsalted end-user hash is enumerable).
	grantHMACKey.Store(nil)
	_, ok := s.mintRunCapability("oidc:https://i.example.com#alice", "ns", "agent", "a:ns/agent", "run-1")
	assert.False(t, ok, "end-user capability must be refused without a grant HMAC key")

	// A K8s username still mints (the mandatory-key rule is end-user-only).
	tok, ok := s.mintRunCapability("system:serviceaccount:t:sa", "ns", "agent", "a:ns/agent", "run-2")
	assert.True(t, ok)
	assert.NotEmpty(t, tok)

	// With a key set, the end-user capability mints.
	setGrantHMACKey([]byte("a-cluster-hmac-key"))
	tok, ok = s.mintRunCapability("oidc:https://i.example.com#alice", "ns", "agent", "a:ns/agent", "run-3")
	assert.True(t, ok, "end-user capability mints once a grant HMAC key is set")
	assert.NotEmpty(t, tok)
}

func TestHandleExtAuth_EndUser(t *testing.T) {
	prev := grantHMACKey.Load()
	t.Cleanup(func() {
		if prev != nil {
			setGrantHMACKey(*prev)
		} else {
			grantHMACKey.Store(nil)
		}
	})
	setGrantHMACKey([]byte("cluster-key"))

	pub, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	ctx := context.Background()
	st := namespacetenant.NewMemStore()
	require.NoError(t, st.SetMembers(ctx, "t1", []string{"ns1"}))
	require.NoError(t, st.SetEndUserIdentity(ctx, "t1", namespacetenant.EndUserIdentity{
		Enabled: true, Issuer: "https://dex-eu.example.com", ClientID: "cid",
	}))

	// The agent must be EXPOSED (spec.endUserAccess → a mirror row, ADR 0107 two-key). Expose "chatbot".
	agentStore := enduseragent.NewMemStore()
	require.NoError(t, agentStore.Set(ctx, enduseragent.ExposedAgent{Namespace: "ns1", Agent: "chatbot", Endpoint: "u"}))

	// No callerClients on the Server — an end-user request must NOT build a K8s client (structural
	// separation). If it fell through to the K8s path it would panic; a passing test proves it didn't.
	newServer := func() *Server {
		return &Server{
			capabilitySigner:     runcap.NewSigner(priv, "test-plane", nil),
			namespaceTenantStore: st,
			endUserAgentStore:    agentStore,
			endUserVerifier:      fakeEndUserVerifier{id: enduseroidc.Identity{Issuer: "https://dex-eu.example.com", Subject: "alice"}},
			log:                  logr.Discard(),
		}
	}
	newReq := func(host string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/extauth/invoke", nil)
		req.Header.Set("X-Forwarded-Host", host)
		req.Header.Set("Authorization", "Bearer end-user-oidc-token")
		return req
	}

	// Exposed agent → 200 + a runcap carrying the end-user identity + standalone boundary.
	rec := httptest.NewRecorder()
	newServer().handleExtAuth(rec, newReq("chatbot.ns1.example.com"))
	assert.Equal(t, http.StatusOK, rec.Code)
	tok := rec.Header().Get(runcap.HeaderName)
	require.NotEmpty(t, tok, "an end-user runcap must be minted + injected")
	capb, err := runcap.NewVerifier(pub, "test-plane", nil).Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, credresolve.EndUserHash([]byte("cluster-key"), "https://dex-eu.example.com", "alice"), capb.User,
		"the runcap subject is the end-user's EndUserHash (not a console identity)")
	assert.Equal(t, credresolve.AgentBoundary("ns1", "chatbot"), capb.Boundary,
		"end-user runs use the per-agent standalone boundary")

	// A verified end-user hitting a NON-exposed agent in the same tenant → 403 (two-key: agent opt-in).
	rec2 := httptest.NewRecorder()
	newServer().handleExtAuth(rec2, newReq("internal-ops.ns1.example.com"))
	assert.Equal(t, http.StatusForbidden, rec2.Code, "an un-exposed agent is not end-user-reachable")
	assert.Empty(t, rec2.Header().Get(runcap.HeaderName))
}
