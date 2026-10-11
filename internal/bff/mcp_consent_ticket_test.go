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
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/oauthpending"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/pgtest"
	"github.com/ctxmesh/ctxmesh/internal/credresolve"
)

const (
	ticketConsole     = "https://console.agents.example.com"
	ticketAgent       = "helper"
	ticketAgentOrigin = "https://helper." + grantNS + ".agents.example.com"
	ticketServer      = "ticket-mcp"
	ticketUserToken   = "alice-token"
	ticketMinter      = "alice@example.com"
)

// ticketReplica is one BFF replica: its own Server over the shared cluster (fake client), grant
// writer and pending-flow backend.
type ticketReplica struct {
	s    *Server
	logs *logBuffer
}

type ticketEnv struct {
	oauth   *fakeOAuthServer
	c       client.WithWatch
	grants  *fakeGrantWriter
	backend *countingStore
	// now is the default in-memory backend's clock.
	now func() time.Time
	// extra layers interceptors onto every caller's client.
	extra interceptor.Funcs
}

// countingStore counts the records put, so a refused mint can be shown to store nothing.
type countingStore struct {
	oauthpending.Store
	puts int
}

func (c *countingStore) Put(ctx context.Context, kind oauthpending.Kind, handle string, payload []byte, ttl time.Duration) error {
	c.puts++
	return c.Store.Put(ctx, kind, handle, payload, ttl)
}

func newTicketEnv(t *testing.T, backend oauthpending.Store) *ticketEnv {
	t.Helper()
	oauth := newFakeOAuthServer(t)
	oauth.expectChallenge = ""
	agent := &agentsv1beta1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: ticketAgent, Namespace: grantNS}}
	e := &ticketEnv{
		oauth:  oauth,
		c:      fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build(),
		grants: &fakeGrantWriter{},
		now:    time.Now,
	}
	if backend == nil {
		backend = oauthpending.NewMemStoreWithClock(func() time.Time { return e.now() })
	}
	e.backend = &countingStore{Store: backend}
	return e
}

func (e *ticketEnv) replica(t *testing.T) ticketReplica {
	t.Helper()
	s, lb := newGrantServer(t, e.c)
	s.callerClients = &identityCallerFactory{backing: e.c, extra: e.extra, username: func(token string) string {
		if token == ticketUserToken {
			return ticketMinter
		}
		return "user:" + token
	}}
	wireTRStore(t, s, nil, oauthToolRegistryWithConfig(ticketServer, "http://ticket/mcp", e.oauth))
	s.consoleURL = ticketConsole
	s.grantStore = e.grants
	s.oauthFlows = newPendingOAuthStore(e.backend, logr.Discard())
	return ticketReplica{s: s, logs: lb}
}

// mintTicket posts the chatbox's mint at the agent hostname, as the edge delivers it.
func mintTicket(t *testing.T, s *Server, agent string, mutate func(*http.Request)) (*httptest.ResponseRecorder, MCPConsentTicketResponse) {
	t.Helper()
	body, err := json.Marshal(MCPConsentTicketRequest{Server: ticketServer, Namespace: grantNS, Agent: agent})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, ticketAgentOrigin+"/api/mcp/oauth/ticket", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ticketUserToken)
	req.Header.Set("Origin", ticketAgentOrigin)
	req.Header.Set(agentChatboxHeader, "1")
	if mutate != nil {
		mutate(req)
	}
	s.Handler().ServeHTTP(rec, req)
	var out MCPConsentTicketResponse
	if rec.Code == http.StatusCreated {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return rec, out
}

// redeemTicket posts the console /connect page's redeem, as a browser at the console sends it.
func redeemTicket(t *testing.T, s *Server, ticket, opener string, mutate func(*http.Request)) (*httptest.ResponseRecorder, MCPConsentRedeemResponse) {
	t.Helper()
	body, err := json.Marshal(MCPConsentRedeemRequest{Ticket: ticket, OpenerOrigin: opener})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, ticketConsole+"/api/mcp/oauth/grant/redeem", bytes.NewReader(body))
	req.Header.Set("Origin", ticketConsole)
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	s.Handler().ServeHTTP(rec, req)
	var out MCPConsentRedeemResponse
	if rec.Code == http.StatusAccepted {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return rec, out
}

// consoleCallback is the authorization server's redirect back to the console callback, in a
// browser holding cookies.
func consoleCallback(s *Server, code, authorizationURL string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	state := ""
	if u, err := url.Parse(authorizationURL); err == nil {
		state = u.Query().Get("state")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, ticketConsole+mcpOAuthCallbackPath+"?"+
		url.Values{"code": {code}, "state": {state}}.Encode(), nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func redirectURIOf(t *testing.T, authorizationURL string) string {
	t.Helper()
	u, err := url.Parse(authorizationURL)
	require.NoError(t, err)
	return u.Query().Get("redirect_uri")
}

// TestConsentTicketCompletesOnTheConsole: a chatbox on an agent hostname mints a ticket, the
// console's /connect page redeems it, and the binding cookie lands on the console response, so
// the console callback completes in that browser. The grant is the minter's, under the agent's
// boundary, and the landing relays to the agent hostname.
func TestConsentTicketCompletesOnTheConsole(t *testing.T) {
	env := newTicketEnv(t, nil)
	r := env.replica(t)

	mrec, minted := mintTicket(t, r.s, ticketAgent, nil)
	require.Equal(t, http.StatusCreated, mrec.Code, "body: %s", mrec.Body.String())
	require.NotEmpty(t, minted.Ticket)
	assert.Equal(t, ticketConsole+"/connect", minted.ConnectURL)
	assert.Equal(t, 60, minted.ExpiresIn)
	assert.Empty(t, mrec.Result().Cookies(), "minting sets no cookie")
	assert.Equal(t, "no-store", mrec.Header().Get("Cache-Control"))
	assert.NotContains(t, mrec.Body.String(), ticketUserToken, "the caller's token stays server-side")

	rrec, redeemed := redeemTicket(t, r.s, minted.Ticket, ticketAgentOrigin, nil)
	require.Equal(t, http.StatusAccepted, rrec.Code, "body: %s", rrec.Body.String())
	assert.Equal(t, ticketConsole+mcpOAuthCallbackPath, redirectURIOf(t, redeemed.AuthorizationURL))
	assert.Equal(t, ticketMinter, redeemed.Minter, "/connect is told whose consent this is")
	assert.Equal(t, ticketServer, redeemed.Server)
	assert.Equal(t, ticketAgent, redeemed.Agent)
	assert.Equal(t, grantNS, redeemed.Namespace)
	cookie := bindingCookie(t, rrec)
	assert.True(t, strings.HasPrefix(cookie.Name, "__Host-"), "the console is HTTPS: %s", cookie.Name)
	assert.NotContains(t, rrec.Body.String(), ticketUserToken)
	assertNoOAuthSecretsInBody(t, rrec.Body.String())

	q := assertCallbackRedirect(t, consoleCallback(r.s, env.oauth.validCode, redeemed.AuthorizationURL, rrec.Result().Cookies()...), "/tools/catalog")
	assert.Equal(t, ticketServer, q.Get("mcp_connected"))
	assert.Equal(t, ticketAgentOrigin, q.Get("opener_origin"))
	assert.Equal(t, "authorization_code", env.oauth.gotGrant)
	require.Equal(t, 1, env.grants.calls)
	assert.Equal(t, userGrantHash(ticketMinter), env.grants.userHash, "the grant is the minter's")
	assert.Equal(t, credresolve.AgentBoundary(grantNS, ticketAgent), env.grants.boundary)
	assert.Equal(t, ticketServer, env.grants.server)

	assert.NotContains(t, r.logs.String(), minted.Ticket)
	assert.NotContains(t, r.logs.String(), ticketUserToken)
}

// TestConsentTicketRefusals: a redeem that fails any check starts no flow and sets no cookie, and
// the ticket it presented is spent.
func TestConsentTicketRefusals(t *testing.T) {
	t.Run("an opener that is not the ticket's agent hostname", func(t *testing.T) {
		for _, opener := range []string{
			ticketConsole,
			"https://other." + grantNS + ".agents.example.com",
			"https://helper.dev.agents.example.com",
			"https://helper." + grantNS + ".attacker.example",
			"https://helper." + grantNS + ".x.agents.example.com",
			"https://helper." + grantNS + ".example.com",
			"http://helper." + grantNS + ".agents.example.com",
			ticketAgentOrigin + "/chat",
			"null",
			"",
		} {
			t.Run(opener, func(t *testing.T) {
				env := newTicketEnv(t, nil)
				r := env.replica(t)
				_, minted := mintTicket(t, r.s, ticketAgent, nil)
				require.NotEmpty(t, minted.Ticket)

				rec, out := redeemTicket(t, r.s, minted.Ticket, opener, nil)
				assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
				assert.Empty(t, out.AuthorizationURL)
				assert.Empty(t, rec.Result().Cookies())

				again, _ := redeemTicket(t, r.s, minted.Ticket, ticketAgentOrigin, nil)
				assert.Equal(t, http.StatusBadRequest, again.Code, "the refused redeem spent the ticket")
			})
		}
	})

	t.Run("a reused ticket", func(t *testing.T) {
		env := newTicketEnv(t, nil)
		r := env.replica(t)
		_, minted := mintTicket(t, r.s, ticketAgent, nil)
		first, _ := redeemTicket(t, r.s, minted.Ticket, ticketAgentOrigin, nil)
		require.Equal(t, http.StatusAccepted, first.Code)

		second, out := redeemTicket(t, r.s, minted.Ticket, ticketAgentOrigin, nil)
		assert.Equal(t, http.StatusBadRequest, second.Code)
		assert.Empty(t, out.AuthorizationURL)
		assert.Empty(t, second.Result().Cookies())
	})

	t.Run("an expired ticket", func(t *testing.T) {
		env := newTicketEnv(t, nil)
		r := env.replica(t)
		base := time.Now()
		env.now = func() time.Time { return base }
		_, minted := mintTicket(t, r.s, ticketAgent, nil)
		env.now = func() time.Time { return base.Add(consentTicketTTL) }

		rec, _ := redeemTicket(t, r.s, minted.Ticket, ticketAgentOrigin, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Empty(t, rec.Result().Cookies())
	})

	t.Run("an unknown ticket", func(t *testing.T) {
		r := newTicketEnv(t, nil).replica(t)
		for _, ticket := range []string{"made-up", strings.Repeat("x", 43)} {
			rec, _ := redeemTicket(t, r.s, ticket, ticketAgentOrigin, nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Empty(t, rec.Result().Cookies())
		}
	})

	t.Run("a redeem not sent by the console's page", func(t *testing.T) {
		env := newTicketEnv(t, nil)
		r := env.replica(t)
		_, minted := mintTicket(t, r.s, ticketAgent, nil)
		for _, origin := range []string{"", ticketAgentOrigin, "https://attacker.example", "http://console.agents.example.com"} {
			rec, _ := redeemTicket(t, r.s, minted.Ticket, ticketAgentOrigin, func(req *http.Request) {
				if origin == "" {
					req.Header.Del("Origin")
				} else {
					req.Header.Set("Origin", origin)
				}
			})
			assert.Equal(t, http.StatusForbidden, rec.Code, "origin %q", origin)
			assert.Empty(t, rec.Result().Cookies())
		}
	})
}

// TestConsentTicketOnALocalhostBaseDomain: with the edge's baseDomain localhost, the console is
// console.localhost and agents are <agent>.<ns>.localhost; a ticket from such an agent completes.
func TestConsentTicketOnALocalhostBaseDomain(t *testing.T) {
	const console = "http://console.localhost:8888"
	env := newTicketEnv(t, nil)
	r := env.replica(t)
	r.s.consoleURL = console

	_, minted := mintTicket(t, r.s, ticketAgent, nil)
	require.NotEmpty(t, minted.Ticket)
	rec, redeemed := redeemTicket(t, r.s, minted.Ticket, "http://helper."+grantNS+".localhost:8888", func(req *http.Request) {
		req.Header.Set("Origin", console)
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, console+mcpOAuthCallbackPath, redirectURIOf(t, redeemed.AuthorizationURL))
}

// TestConsentTicketForgedHeaders: Origin and X-Forwarded-Host are the client's to choose, so on
// neither the mint nor the redeem do they move the redirect_uri or the relay target.
func TestConsentTicketForgedHeaders(t *testing.T) {
	env := newTicketEnv(t, nil)
	r := env.replica(t)
	forge := func(req *http.Request) {
		req.Header.Set("X-Forwarded-Host", "helper."+grantNS+".attacker.example")
		req.Header.Set("X-Forwarded-Proto", "https")
	}

	_, minted := mintTicket(t, r.s, ticketAgent, func(req *http.Request) {
		forge(req)
		req.Header.Set("Origin", "https://helper."+grantNS+".attacker.example")
	})
	require.NotEmpty(t, minted.Ticket)
	rrec, redeemed := redeemTicket(t, r.s, minted.Ticket, ticketAgentOrigin, forge)
	require.Equal(t, http.StatusAccepted, rrec.Code, "body: %s", rrec.Body.String())
	assert.Equal(t, ticketConsole+mcpOAuthCallbackPath, redirectURIOf(t, redeemed.AuthorizationURL))

	q := assertCallbackRedirect(t, consoleCallback(r.s, env.oauth.validCode, redeemed.AuthorizationURL, rrec.Result().Cookies()...), "/tools/catalog")
	assert.Equal(t, ticketAgentOrigin, q.Get("opener_origin"))

	// A forged origin as the opener the page reports is refused like any other.
	_, again := mintTicket(t, r.s, ticketAgent, forge)
	rec, _ := redeemTicket(t, r.s, again.Ticket, "https://helper."+grantNS+".attacker.example", forge)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestConsentTicketBindsTheRedeemingBrowser: the flow a redeem starts completes only in the
// browser that redeemed it. Whoever begins the authorization elsewhere cannot finish it without
// that browser's cookie, so a ticket delivered to someone's /connect binds nothing to anyone else.
func TestConsentTicketBindsTheRedeemingBrowser(t *testing.T) {
	env := newTicketEnv(t, nil)
	r := env.replica(t)
	redeem := func(t *testing.T) (*httptest.ResponseRecorder, MCPConsentRedeemResponse) {
		_, minted := mintTicket(t, r.s, ticketAgent, nil)
		rec, out := redeemTicket(t, r.s, minted.Ticket, ticketAgentOrigin, nil)
		require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
		return rec, out
	}

	t.Run("no cookie", func(t *testing.T) {
		_, redeemed := redeem(t)
		q := assertCallbackRedirect(t, consoleCallback(r.s, env.oauth.validCode, redeemed.AuthorizationURL), "/tools/add-mcp")
		assert.Contains(t, q.Get("mcp_error"), "not started in this browser")
	})
	t.Run("another redeem's cookie", func(t *testing.T) {
		_, redeemed := redeem(t)
		other, _ := redeem(t)
		q := assertCallbackRedirect(t, consoleCallback(r.s, env.oauth.validCode, redeemed.AuthorizationURL, other.Result().Cookies()...), "/tools/add-mcp")
		assert.Contains(t, q.Get("mcp_error"), "not started in this browser")
	})
	assert.Empty(t, env.oauth.gotGrant, "the code is never exchanged")
	assert.Zero(t, env.grants.calls, "no grant is stored")
}

// TestConsentTicketMint covers what the mint requires: a caller, an agent, an OAuth server they
// can read, and a console origin to send them to. It is reachable at an agent hostname; the redeem
// is not.
func TestConsentTicketMint(t *testing.T) {
	t.Run("without a caller", func(t *testing.T) {
		r := newTicketEnv(t, nil).replica(t)
		rec, _ := mintTicket(t, r.s, ticketAgent, func(req *http.Request) { req.Header.Del("Authorization") })
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
	t.Run("without an agent", func(t *testing.T) {
		r := newTicketEnv(t, nil).replica(t)
		rec, _ := mintTicket(t, r.s, " ", nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("for a server that is not registered", func(t *testing.T) {
		r := newTicketEnv(t, nil).replica(t)
		wireTRStore(t, r.s, nil)
		rec, _ := mintTicket(t, r.s, ticketAgent, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
	t.Run("for an agent that does not exist", func(t *testing.T) {
		env := newTicketEnv(t, nil)
		r := env.replica(t)
		rec, minted := mintTicket(t, r.s, "nobody", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
		assert.Empty(t, minted.Ticket)
		assert.Zero(t, env.backend.puts, "nothing is stored")
	})
	t.Run("for an agent the caller cannot read", func(t *testing.T) {
		env := newTicketEnv(t, nil)
		env.extra = interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*agentsv1beta1.AgentDeployment); ok {
				return apierrors.NewForbidden(schema.GroupResource{Group: "agents.ctxmesh.io", Resource: "agentdeployments"}, key.Name, errors.New("denied"))
			}
			return c.Get(ctx, key, obj, opts...)
		}}
		r := env.replica(t)
		rec, minted := mintTicket(t, r.s, ticketAgent, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
		assert.Empty(t, minted.Ticket)
		assert.Zero(t, env.backend.puts, "nothing is stored")
	})
	t.Run("without a console origin", func(t *testing.T) {
		r := newTicketEnv(t, nil).replica(t)
		r.s.consoleURL = ""
		rec, _ := mintTicket(t, r.s, ticketAgent, nil)
		assert.Equal(t, http.StatusNotImplemented, rec.Code)
		rrec, _ := redeemTicket(t, r.s, strings.Repeat("x", 43), ticketAgentOrigin, nil)
		assert.Equal(t, http.StatusNotImplemented, rrec.Code)
	})
	t.Run("the redeem is not served at an agent hostname", func(t *testing.T) {
		env := newTicketEnv(t, nil)
		r := env.replica(t)
		_, minted := mintTicket(t, r.s, ticketAgent, nil)
		rec, _ := redeemTicket(t, r.s, minted.Ticket, ticketAgentOrigin, func(req *http.Request) {
			req.Header.Set(agentChatboxHeader, "1")
		})
		assert.Equal(t, http.StatusNotFound, rec.Code)
		ok, _ := redeemTicket(t, r.s, minted.Ticket, ticketAgentOrigin, nil)
		assert.Equal(t, http.StatusAccepted, ok.Code, "the refused request never reached the ticket")
	})
}

// TestConsentCompletesAcrossReplicas: the pending flow and the ticket live in the shared store, so
// each leg may reach a different BFF replica. Run on the in-memory twin always, and on Postgres
// when CONTROLPLANE_TEST_DSN is set.
func TestConsentCompletesAcrossReplicas(t *testing.T) {
	backends := map[string]func(t *testing.T) oauthpending.Store{
		"mem": func(*testing.T) oauthpending.Store { return oauthpending.NewMemStore() },
		"postgres": func(t *testing.T) oauthpending.Store {
			db := pgtest.Open(t, "bff_oauthpending")
			_, err := db.Exec(`CREATE TABLE IF NOT EXISTS oauth_pending (LIKE public.oauth_pending INCLUDING ALL)`)
			require.NoError(t, err)
			_, err = db.Exec(`TRUNCATE oauth_pending`)
			require.NoError(t, err)
			return oauthpending.NewPostgresStore(db)
		},
	}
	for name, newBackend := range backends {
		t.Run(name, func(t *testing.T) {
			t.Run("a ticket minted on one replica, redeemed on another, called back on a third", func(t *testing.T) {
				env := newTicketEnv(t, newBackend(t))
				a, b, c := env.replica(t), env.replica(t), env.replica(t)

				_, minted := mintTicket(t, a.s, ticketAgent, nil)
				require.NotEmpty(t, minted.Ticket)
				rrec, redeemed := redeemTicket(t, b.s, minted.Ticket, ticketAgentOrigin, nil)
				require.Equal(t, http.StatusAccepted, rrec.Code, "body: %s", rrec.Body.String())

				q := assertCallbackRedirect(t, consoleCallback(c.s, env.oauth.validCode, redeemed.AuthorizationURL, rrec.Result().Cookies()...), "/tools/catalog")
				assert.Equal(t, ticketAgentOrigin, q.Get("opener_origin"))
				assert.Equal(t, 1, env.grants.calls)

				again, _ := redeemTicket(t, a.s, minted.Ticket, ticketAgentOrigin, nil)
				assert.Equal(t, http.StatusBadRequest, again.Code, "single use holds across replicas")
			})
			t.Run("a replica whose clock runs ahead does not expire another replica's ticket", func(t *testing.T) {
				env := newTicketEnv(t, newBackend(t))
				a, b := env.replica(t), env.replica(t)
				b.s.oauthFlows.now = func() time.Time { return time.Now().Add(2 * consentTicketTTL) }

				_, minted := mintTicket(t, a.s, ticketAgent, nil)
				require.NotEmpty(t, minted.Ticket)
				_, other := mintTicket(t, b.s, ticketAgent, nil) // b's first put sweeps
				require.NotEmpty(t, other.Ticket)

				rec, _ := redeemTicket(t, a.s, minted.Ticket, ticketAgentOrigin, nil)
				assert.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
			})
			t.Run("a consent begun on one replica completes on another", func(t *testing.T) {
				env := newTicketEnv(t, newBackend(t))
				a, b := env.replica(t), env.replica(t)
				a.s.consoleURL, b.s.consoleURL = "", ""

				rec, pending := beginGrant(t, a.s, ticketServer, ticketUserToken, nil)
				require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
				assertCallbackRedirect(t, callback(t, b.s, env.oauth.validCode, pending.State, rec.Result().Cookies()...), "/tools/catalog")
				assert.Equal(t, 1, env.grants.calls)

				replay := callback(t, a.s, env.oauth.validCode, pending.State, rec.Result().Cookies()...)
				assertCallbackRedirect(t, replay, "/tools/add-mcp")
				assert.Equal(t, 1, env.grants.calls, "a replayed callback on the first replica finds no flow")
			})
		})
	}
}

// TestConsentOpenerAllowed pins which openers each console shape accepts for agent helper in prod.
func TestConsentOpenerAllowed(t *testing.T) {
	for _, tc := range []struct {
		console string
		allow   []string
		refuse  []string
	}{
		{
			console: "https://console.agents.example.com",
			allow:   []string{"https://helper.prod.agents.example.com", "https://HELPER.Prod.agents.example.com", "https://helper.prod.agents.example.com:8443"},
			refuse: []string{
				"https://console.agents.example.com", "https://other.prod.agents.example.com",
				"https://helper.prod.example.com", "https://helper.prod.x.agents.example.com",
				"https://helper.prod.agents.example.com.attacker.example", "http://helper.prod.agents.example.com",
				"https://user@helper.prod.agents.example.com", "https://helper.prod.agents.example.com/?x=1",
			},
		},
		{
			console: "https://example.com",
			allow:   []string{"https://helper.prod.example.com"},
			refuse:  []string{"https://helper.prod.com", "https://helper.prod.evil.com"},
		},
		{
			console: "https://example.co.uk",
			allow:   []string{"https://helper.prod.example.co.uk"},
			refuse:  []string{"https://helper.prod.co.uk"},
		},
		{
			console: "http://console.127.0.0.1.sslip.io",
			allow:   []string{"http://helper.prod.127.0.0.1.sslip.io"},
			refuse:  []string{"http://helper.prod.6.6.6.6.sslip.io", "http://helper.prod.sslip.io"},
		},
		{
			console: "http://127.0.0.1:9090",
			allow: []string{
				"http://helper.prod.127.0.0.1.sslip.io", "http://helper.prod.127.0.0.1.nip.io",
				"http://helper.prod.localhost:5173",
			},
			refuse: []string{
				"http://helper.prod.6.6.6.6.sslip.io", "http://helper.prod.evil.com",
				"https://helper.prod.127.0.0.1.sslip.io", "http://helper.prod.0.0.1",
			},
		},
		{
			console: "http://localhost:9090",
			allow:   []string{"http://helper.prod.127.0.0.1.sslip.io", "http://helper.prod.localhost"},
			refuse:  []string{"http://helper.prod.evil.com"},
		},
		{
			console: "https://10.0.0.5",
			refuse:  []string{"https://helper.prod.0.0.5", "https://helper.prod.10.0.0.5.sslip.io"},
		},
		{
			console: "http://[::1]:9090",
			allow:   []string{"http://helper.prod.localhost", "http://helper.prod.127.0.0.1.sslip.io"},
			refuse:  []string{"http://helper.prod.evil.com", "http://helper.prod.1"},
		},
		// A single-label base domain off the ICANN list is private: the public-suffix list's
		// default rule returns it, but agents live under it, not under the console.
		{
			console: "http://console.localhost:9090",
			allow:   []string{"http://helper.prod.localhost:9090", "http://helper.prod.localhost"},
			refuse: []string{
				"http://helper.prod.console.localhost", "http://helper.prod.evil.localhost",
				"http://helper.prod.localhost.evil.com", "https://helper.prod.localhost",
			},
		},
		{
			console: "https://console.internal",
			allow:   []string{"https://helper.prod.internal"},
			refuse:  []string{"https://helper.prod.console.internal", "https://helper.prod.evil.internal"},
		},
		{
			console: "https://console.corp",
			allow:   []string{"https://helper.prod.corp"},
			refuse:  []string{"https://helper.prod.console.corp"},
		},
		{
			console: "https://console.example.com",
			allow:   []string{"https://helper.prod.example.com"},
			refuse:  []string{"https://helper.prod.console.example.com", "https://helper.prod.com"},
		},
		{
			console: "https://console.example.co.uk",
			allow:   []string{"https://helper.prod.example.co.uk"},
			refuse:  []string{"https://helper.prod.co.uk", "https://helper.prod.console.example.co.uk"},
		},
		// A private-section public suffix (github.io) is still a public suffix: other tenants own its
		// other subdomains, so a console at x.github.io has its agents under itself.
		{
			console: "https://x.github.io",
			allow:   []string{"https://helper.prod.x.github.io"},
			refuse:  []string{"https://helper.prod.github.io", "https://helper.prod.y.github.io"},
		},
	} {
		t.Run(tc.console, func(t *testing.T) {
			s := &Server{consoleURL: tc.console}
			for _, o := range tc.allow {
				got, ok := s.consentOpenerAllowed(o, "helper", "prod")
				assert.True(t, ok, "allow %s", o)
				assert.Equal(t, strings.ToLower(o), got)
			}
			for _, o := range tc.refuse {
				_, ok := s.consentOpenerAllowed(o, "helper", "prod")
				assert.False(t, ok, "refuse %s", o)
			}
		})
	}
}
