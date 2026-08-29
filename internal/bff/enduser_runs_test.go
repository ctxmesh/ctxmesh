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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane/enduseragent"
	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/agent-engine/internal/credresolve"
	"github.com/ctxmesh/agent-engine/internal/enduseroidc"
	"github.com/ctxmesh/agent-engine/internal/run"
	"github.com/ctxmesh/agent-engine/internal/runcap"
)

func newEndUserRunServer(t *testing.T, exposedEndpoint string) (*Server, *run.Store) {
	t.Helper()
	ctx := context.Background()
	_, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)

	tenants := namespacetenant.NewMemStore()
	require.NoError(t, tenants.SetMembers(ctx, "t1", []string{"ns1"}))
	require.NoError(t, tenants.SetEndUserIdentity(ctx, "t1", namespacetenant.EndUserIdentity{
		Enabled: true, Issuer: "https://dex-eu.example.com", ClientID: "cid",
	}))
	agents := enduseragent.NewMemStore()
	require.NoError(t, agents.Set(ctx, enduseragent.ExposedAgent{
		Namespace: "ns1", Agent: "chatbot", Endpoint: exposedEndpoint, OutputSchema: `{"type":"object"}`,
	}))
	rs := run.NewMemStore()
	s := &Server{
		runStore:             rs,
		capabilitySigner:     runcap.NewSigner(priv, "test-plane", nil),
		namespaceTenantStore: tenants,
		endUserAgentStore:    agents,
		endUserVerifier:      fakeEndUserVerifier{id: enduseroidc.Identity{Issuer: "https://dex-eu.example.com", Subject: "alice"}},
		runWorkerDispatch:    true, // skip the in-process goroutine dispatch
		log:                  logr.Discard(),
	}
	return s, &rs
}

func endUserCreateReq(t *testing.T, host, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader([]byte(body)))
	req.Header.Set("X-Forwarded-Host", host)
	req.Header.Set("Authorization", "Bearer end-user-oidc-token")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestHandleCreateRun_EndUser_HappyPath(t *testing.T) {
	prev := grantHMACKey.Load()
	t.Cleanup(func() {
		if prev != nil {
			setGrantHMACKey(*prev)
		} else {
			grantHMACKey.Store(nil)
		}
	})
	setGrantHMACKey([]byte("cluster-key"))

	s, rsp := newEndUserRunServer(t, "https://chatbot.ns1.example.com")
	rs := *rsp
	rec := httptest.NewRecorder()

	s.handleCreateRun(rec, endUserCreateReq(t, "chatbot.ns1.example.com", `{"input":"hello","conversationId":"c1"}`))

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp CreateRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.ID)

	rn, err := rs.Get(resp.ID)
	require.NoError(t, err)
	assert.Equal(t, "oidc:https://dex-eu.example.com#alice", rn.CallerUsername, "the run is owned by the end-user principal")
	assert.Equal(t, credresolve.AgentBoundary("ns1", "chatbot"), rn.Boundary)
	assert.Equal(t, "https://chatbot.ns1.example.com", rn.Endpoint, "endpoint from the exposure mirror")
	assert.Equal(t, "ns1", rn.Namespace)
	assert.Equal(t, "chatbot", rn.Agent)
	assert.NotEqual(t, "c1", rn.ConversationID, "the conversation id is principal-scoped (never the raw client value)")
	assert.Contains(t, rn.ConversationID, "eu-conv-")
}

func TestHandleCreateRun_EndUser_Errors(t *testing.T) {
	prev := grantHMACKey.Load()
	t.Cleanup(func() {
		if prev != nil {
			setGrantHMACKey(*prev)
		} else {
			grantHMACKey.Store(nil)
		}
	})
	setGrantHMACKey([]byte("cluster-key"))

	t.Run("unexposed agent → 404", func(t *testing.T) {
		s, _ := newEndUserRunServer(t, "https://chatbot.ns1.example.com")
		rec := httptest.NewRecorder()
		s.handleCreateRun(rec, endUserCreateReq(t, "internal-ops.ns1.example.com", `{"input":"x"}`))
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("exposed but not Ready (empty endpoint) → 409", func(t *testing.T) {
		s, _ := newEndUserRunServer(t, "") // empty endpoint = not Ready
		rec := httptest.NewRecorder()
		s.handleCreateRun(rec, endUserCreateReq(t, "chatbot.ns1.example.com", `{"input":"x"}`))
		assert.Equal(t, http.StatusConflict, rec.Code)
	})

	t.Run("record requested → 400", func(t *testing.T) {
		s, _ := newEndUserRunServer(t, "https://chatbot.ns1.example.com")
		rec := httptest.NewRecorder()
		s.handleCreateRun(rec, endUserCreateReq(t, "chatbot.ns1.example.com", `{"input":"x","record":true}`))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("body namespace/agent mismatch → 400 (host is authoritative)", func(t *testing.T) {
		s, _ := newEndUserRunServer(t, "https://chatbot.ns1.example.com")
		rec := httptest.NewRecorder()
		s.handleCreateRun(rec, endUserCreateReq(t, "chatbot.ns1.example.com", `{"input":"x","namespace":"other-tenant","agent":"chatbot"}`))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("per-principal create rate limit → 429", func(t *testing.T) {
		// A single verified end-user identity is bounded (NAT-proof, keyed on the principal): the first
		// create is admitted (202), the next is over budget → 429 (M137/EU1c).
		s, _ := newEndUserRunServer(t, "https://chatbot.ns1.example.com")
		s.endUserCreateLimiter = newIPRateLimiter(0.0001, 1) // burst 1, negligible refill
		rec1 := httptest.NewRecorder()
		s.handleCreateRun(rec1, endUserCreateReq(t, "chatbot.ns1.example.com", `{"input":"hi"}`))
		require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())
		rec2 := httptest.NewRecorder()
		s.handleCreateRun(rec2, endUserCreateReq(t, "chatbot.ns1.example.com", `{"input":"hi"}`))
		assert.Equal(t, http.StatusTooManyRequests, rec2.Code)
	})

	t.Run("rejected end-user bearer at an agent origin → 401 (owns the auth failure)", func(t *testing.T) {
		// The tenant has end-user login enabled; the bearer fails verification (forged/expired). An
		// end-user chat body carries NO agent — before ADR 0107 §3 this fell through to the console path's
		// "agent is required" 400, leaving the SPA no signal to re-login. Now the end-user path owns it 401.
		s, _ := newEndUserRunServer(t, "https://chatbot.ns1.example.com")
		s.endUserVerifier = fakeEndUserVerifier{err: errors.New("bad token")}
		rec := httptest.NewRecorder()
		s.handleCreateRun(rec, endUserCreateReq(t, "chatbot.ns1.example.com", `{"input":"hi"}`))
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	})
}

func TestHandleEndUserMyRuns(t *testing.T) {
	prev := grantHMACKey.Load()
	t.Cleanup(func() {
		if prev != nil {
			setGrantHMACKey(*prev)
		} else {
			grantHMACKey.Store(nil)
		}
	})
	setGrantHMACKey([]byte("cluster-key"))

	// alice (the fake verifier's identity) owns two runs at chatbot; bob owns one; alice owns one at a
	// different agent. The list must return ONLY alice's two runs at chatbot.
	const alice = "oidc:https://dex-eu.example.com#alice"
	const bob = "oidc:https://dex-eu.example.com#bob"
	seed := func(rs run.Store) {
		mk := func(id, caller, ns, agent string) {
			r := run.New(id, ns, agent, []byte(`{"input":"x"}`), "", time.Now())
			r.CallerUsername = caller
			require.NoError(t, rs.Create(r))
		}
		mk("a1", alice, "ns1", "chatbot")
		mk("a2", alice, "ns1", "chatbot")
		mk("b1", bob, "ns1", "chatbot")
		mk("a3", alice, "ns1", "other-agent")
	}

	myRunsReq := func(host, bearer string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/end-user/runs", nil)
		req.Header.Set("X-Forwarded-Host", host)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		return req
	}

	t.Run("verified end-user sees ONLY their own runs at this agent", func(t *testing.T) {
		s, rsp := newEndUserRunServer(t, "https://chatbot.ns1.example.com")
		seed(*rsp)
		rec := httptest.NewRecorder()
		s.handleEndUserMyRuns(rec, myRunsReq("chatbot.ns1.example.com", "an-oidc-id-token"))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp EndUserRunsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		ids := []string{}
		for _, r := range resp.Runs {
			ids = append(ids, r.ID)
		}
		assert.ElementsMatch(t, []string{"a1", "a2"}, ids, "only alice's chatbot runs (not bob's, not other-agent)")
	})

	t.Run("rejected bearer → 401", func(t *testing.T) {
		s, rsp := newEndUserRunServer(t, "https://chatbot.ns1.example.com")
		seed(*rsp)
		s.endUserVerifier = fakeEndUserVerifier{err: errors.New("bad token")}
		rec := httptest.NewRecorder()
		s.handleEndUserMyRuns(rec, myRunsReq("chatbot.ns1.example.com", "forged"))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("no end-user IdP for the host ns → uniform 404", func(t *testing.T) {
		s, rsp := newEndUserRunServer(t, "https://chatbot.ns1.example.com")
		seed(*rsp)
		rec := httptest.NewRecorder()
		s.handleEndUserMyRuns(rec, myRunsReq("chatbot.orphan-ns.example.com", "an-oidc-id-token"))
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("no bearer → uniform 404 (no oracle)", func(t *testing.T) {
		s, rsp := newEndUserRunServer(t, "https://chatbot.ns1.example.com")
		seed(*rsp)
		rec := httptest.NewRecorder()
		s.handleEndUserMyRuns(rec, myRunsReq("chatbot.ns1.example.com", ""))
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("per-IP pre-auth rate limit → 429", func(t *testing.T) {
		s, rsp := newEndUserRunServer(t, "https://chatbot.ns1.example.com")
		seed(*rsp)
		s.endUserLimiter = newIPRateLimiter(0.0001, 2) // burst 2, negligible refill
		for i := 0; i < 2; i++ {
			rec := httptest.NewRecorder()
			s.handleEndUserMyRuns(rec, myRunsReq("chatbot.ns1.example.com", "an-oidc-id-token"))
			require.Equal(t, http.StatusOK, rec.Code, "burst admits the first requests")
		}
		rec := httptest.NewRecorder()
		s.handleEndUserMyRuns(rec, myRunsReq("chatbot.ns1.example.com", "an-oidc-id-token"))
		assert.Equal(t, http.StatusTooManyRequests, rec.Code, "over budget from one IP → 429")
	})
}

func TestAuthorizeRunAccess_EndUser(t *testing.T) {
	ctx := context.Background()
	rs := run.NewMemStore()
	rn := run.New("run-1", "ns1", "chatbot", nil, "", time.Now())
	rn.CallerUsername = "oidc:https://dex-eu.example.com#alice"
	require.NoError(t, rs.Create(rn))

	st := namespacetenant.NewMemStore()
	require.NoError(t, st.SetMembers(ctx, "t1", []string{"ns1"}))
	require.NoError(t, st.SetEndUserIdentity(ctx, "t1", namespacetenant.EndUserIdentity{
		Enabled: true, Issuer: "https://dex-eu.example.com", ClientID: "cid",
	}))

	server := func(sub string) *Server {
		return &Server{
			runStore:             rs,
			namespaceTenantStore: st,
			endUserVerifier:      fakeEndUserVerifier{id: enduseroidc.Identity{Issuer: "https://dex-eu.example.com", Subject: sub}},
			log:                  logr.Discard(),
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/runs/run-1", nil)
	req.Header.Set("Authorization", "Bearer end-user-token")

	// The owner (alice) reads her own run → authorized, WITHOUT any K8s client (caller is nil — if the
	// K8s path were reached it would panic; passing proves the structural separation).
	rec := httptest.NewRecorder()
	got, ok := server("alice").authorizeRunAccess(rec, req, nil, "run-1", true)
	require.True(t, ok)
	assert.Equal(t, "run-1", got.ID)

	// A DIFFERENT end-user (bob) reading alice's run → uniform 404 (no oracle), never leaked.
	rec2 := httptest.NewRecorder()
	_, ok = server("bob").authorizeRunAccess(rec2, req, nil, "run-1", true)
	assert.False(t, ok)
	assert.Equal(t, http.StatusNotFound, rec2.Code)
}
