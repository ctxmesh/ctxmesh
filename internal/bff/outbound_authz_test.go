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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
)

// Handlers that contact a URL the caller supplied must authorize the caller first. Otherwise any
// bearer string turns the BFF into an SSRF and a reachability oracle for the network it sits in:
// "could not reach" versus "not an MCP server" answers whether an internal host is up.

// countingServer fronts handler with a server that counts every request it receives — the
// outbound requests the BFF made on the caller's behalf.
func countingServer(t *testing.T, handler http.Handler) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// actionsAuthorizer records every action asked of it and refuses the ones deny matches.
type actionsAuthorizer struct {
	mu      sync.Mutex
	actions []authz.Action
	deny    func(authz.Action) error
}

func (a *actionsAuthorizer) Authorize(_ context.Context, _ client.Client, act authz.Action) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actions = append(a.actions, act)
	if a.deny != nil {
		return a.deny(act)
	}
	return nil
}

func (a *actionsAuthorizer) asked() []authz.Action {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]authz.Action(nil), a.actions...)
}

// rejectedToken is what a SelfSubjectAccessReview returns when the API server does not accept
// the bearer at all — the garbage-token case, as SSARAuthorizer wraps it.
func rejectedToken() error {
	return fmt.Errorf("authz: SelfSubjectAccessReview: %w", apierrors.NewUnauthorized("invalid bearer token"))
}

func postRegister(t *testing.T, s *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestRegisterMCPAuthorizesBeforeContactingTheURL: a caller refused the registration gets
// 403 (or 401 for a token the API server rejects) and the URL in the body is never contacted —
// for an open server, a keyed server, and an OAuth server whose discovery would walk it.
func TestRegisterMCPAuthorizesBeforeContactingTheURL(t *testing.T) {
	inner := fakeMCPServer(t, false)
	disco, _ := oauthDiscoveryStub(t, false, false)

	cases := []struct {
		name string
		body func(url string) []byte
	}{
		{"open server", func(u string) []byte { return registerBody(t, "probe-target", u, "") }},
		{"keyed server", func(u string) []byte { return registerBody(t, "probe-target", u, theMCPKey) }},
		{"oauth autodiscover", func(u string) []byte {
			return oauthRegisterBody(t, "probe-target", u+"/mcp", &MCPAuthRequest{
				Type: "oauth", AutoDiscover: true, RedirectURI: "https://console.example/api/mcp/oauth/callback",
			})
		}},
	}
	refusals := []struct {
		name   string
		err    error
		status int
	}{
		{"forbidden", authz.ErrForbidden, http.StatusForbidden},
		{"token rejected", rejectedToken(), http.StatusUnauthorized},
	}
	for _, tc := range cases {
		for _, ref := range refusals {
			t.Run(tc.name+"/"+ref.name, func(t *testing.T) {
				target := inner.Config.Handler
				if tc.name == "oauth autodiscover" {
					target = disco.Config.Handler
				}
				srv, hits := countingServer(t, target)
				s, _, _ := newMCPServer(t, fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), false)
				auth := &actionsAuthorizer{deny: func(authz.Action) error { return ref.err }}
				store := wireTRStore(t, s, auth)

				rec := postRegister(t, s, tc.body(srv.URL))

				require.Equal(t, ref.status, rec.Code, "body: %s", rec.Body.String())
				assert.Zero(t, hits.Load(), "a refused caller must cause no outbound request to the URL it supplied")
				require.NotEmpty(t, auth.asked())
				assert.Equal(t, authz.Action{
					Verb: authz.VerbCreate, Group: "agents.ctxmesh.ai", Resource: resourceToolRegistries, Namespace: "prod",
				}, auth.asked()[0], "the pre-flight is the registration's own write")
				list, err := store.List(context.Background(), controlplane.ListOptions{Namespace: "prod"})
				require.NoError(t, err)
				assert.Empty(t, list.Items, "nothing is registered")
			})
		}
	}
}

// TestRegisterMCPAuthorizedCallerStillProbes: the gate moved, the flow did not — an allowed
// caller's register still contacts the server and lands the catalog.
func TestRegisterMCPAuthorizedCallerStillProbes(t *testing.T) {
	inner := fakeMCPServer(t, false)
	srv, hits := countingServer(t, inner.Config.Handler)
	s, _, _ := newMCPServer(t, fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), false)
	auth := &actionsAuthorizer{}
	wireTRStore(t, s, auth)

	rec := postRegister(t, s, registerBody(t, "allowed-mcp", srv.URL, ""))

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	assert.Positive(t, hits.Load(), "an authorized register probes the server")
	var resp RegisterMCPServerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Server.ToolCount)
}

// TestRegisterMCPWithoutStoreIs501BeforeProbing: with no control-plane store the register can
// never complete, so it must not probe first and fail afterwards.
func TestRegisterMCPWithoutStoreIs501BeforeProbing(t *testing.T) {
	inner := fakeMCPServer(t, false)
	srv, hits := countingServer(t, inner.Config.Handler)
	s, _, _ := newMCPServer(t, fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), false)
	s.toolRegistryStore = nil

	rec := postRegister(t, s, registerBody(t, "no-store", srv.URL, ""))

	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Zero(t, hits.Load())
}

func postConnect(t *testing.T, s *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestConnectProviderAuthorizesBeforeContactingTheBaseURL: the connect probe goes to the base
// URL in the body. A caller refused ANY of the flow's writes — here only the last, the
// ModelRoute — is answered before that request is made.
func TestConnectProviderAuthorizesBeforeContactingTheBaseURL(t *testing.T) {
	inner := fakeProvider(t, "m-1")
	refusals := []struct {
		name   string
		err    error
		status int
	}{
		{"forbidden", authz.ErrForbidden, http.StatusForbidden},
		{"token rejected", rejectedToken(), http.StatusUnauthorized},
	}
	for _, ref := range refusals {
		t.Run(ref.name, func(t *testing.T) {
			srv, hits := countingServer(t, inner.Config.Handler)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
			s, _, _ := newConnectServer(t, c)
			auth := &actionsAuthorizer{deny: func(a authz.Action) error {
				if a.Resource == resModelRoutes {
					return ref.err
				}
				return nil
			}}
			s.authorizer = auth

			rec := postConnect(t, s, connectBody(t, providerCustom, theTestKey, srv.URL, "prod"))

			require.Equal(t, ref.status, rec.Code, "body: %s", rec.Body.String())
			assert.Zero(t, hits.Load(), "a refused caller must cause no outbound request to the base URL it supplied")
			var secrets corev1.SecretList
			require.NoError(t, c.List(context.Background(), &secrets))
			assert.Empty(t, secrets.Items, "nothing is written")
			assert.NotContains(t, rec.Body.String(), theTestKey)
		})
	}
}

// TestConnectProviderPreflightAsksForTheWholeWriteSet: the pre-flight is the connectProvider
// flow — every object connect writes — and an allowed caller still probes and connects.
func TestConnectProviderPreflightAsksForTheWholeWriteSet(t *testing.T) {
	inner := fakeProvider(t, "m-1")
	srv, hits := countingServer(t, inner.Config.Handler)
	s, _, _ := newConnectServer(t, fake.NewClientBuilder().WithScheme(testScheme(t)).Build())
	auth := &actionsAuthorizer{}
	s.authorizer = auth

	rec := postConnect(t, s, connectBody(t, providerCustom, theTestKey, srv.URL, "prod"))

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	assert.Positive(t, hits.Load(), "an authorized connect probes the provider")
	assert.ElementsMatch(t, []authz.Action{
		{Verb: verbCreate, Group: "", Resource: resSecrets, Namespace: "prod"},
		{Verb: verbCreate, Group: agentsAPIGroup, Resource: resSecretBindings, Namespace: "prod"},
		{Verb: verbCreate, Group: agentsAPIGroup, Resource: resModelRoutes, Namespace: "prod"},
	}, auth.asked())
}
