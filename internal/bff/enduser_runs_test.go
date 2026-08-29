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
