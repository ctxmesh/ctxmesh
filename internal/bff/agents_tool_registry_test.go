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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// An agent's tools bind to the MCP servers registered in its namespace. Which of those servers a
// create or edit may bind is decided by who is asking: another user's private server is neither
// bindable nor allowed to shadow a server the caller can use.

const (
	aliceUser    = "alice@example.com"
	operatorUser = "operator@example.com"
	// alicePrivateRegistry sorts before teamEchoRegistry, so a by-name pick ignoring
	// visibility lands on Alice's server.
	alicePrivateRegistry = "a-alice-private"
	teamEchoRegistry     = "z-team-echo"
	teamEchoURL          = "https://echo.example/mcp"
	alicePrivateURL      = "https://alice.example/mcp"
)

// identityClient is a fake client whose SelfSubjectReview answers as username, with every
// other create stored as usual.
func identityClient(t *testing.T, username string, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if ssr, ok := obj.(*authnv1.SelfSubjectReview); ok {
					ssr.Status.UserInfo = authnv1.UserInfo{Username: username}
					return nil
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
}

// echoRegistries seeds ns with Alice's private server and a team server, both offering echo.
func echoRegistries(t *testing.T, ns string) toolregistry.Store {
	t.Helper()
	store := toolregistry.NewMemStore()
	for _, r := range []toolregistry.ToolRegistry{
		{
			Namespace: ns, Name: alicePrivateRegistry,
			Tools: []toolregistry.ToolEntry{{Name: "echo", URL: alicePrivateURL}},
			Labels: map[string]string{
				labelManagedBy: managedByMCP, labelMCPScope: scopePersonal, labelMCPOwner: userGrantHash(aliceUser),
				labelMCPVisibility: visibilityPrivate, labelMCPCredentialSource: credSourceByoOAuth,
			},
		},
		{
			Namespace: ns, Name: teamEchoRegistry,
			Tools: []toolregistry.ToolEntry{{Name: "echo", URL: teamEchoURL}},
			Labels: map[string]string{
				labelManagedBy: managedByMCP, labelMCPScope: scopePublic,
				labelMCPVisibility: visibilityTeam, labelMCPCredentialSource: credSourceNone,
			},
		},
	} {
		_, err := store.Upsert(context.Background(), r)
		require.NoError(t, err)
	}
	return store
}

func managedEchoAgent(name string) string {
	return fmt.Sprintf("name: %s\nruntime: managed\nsystemPrompt: be helpful\ntools:\n  - echo\n", name)
}

// createAgentAs creates agentYAML in "prod" through POST /api/agents.
func createAgentAs(t *testing.T, c client.Client, store toolregistry.Store, agentYAML string) *httptest.ResponseRecorder {
	t.Helper()
	s := newConfigServer(t, c)
	s.toolRegistryStore = store
	body, err := json.Marshal(CreateAgentRequest{AgentYAML: agentYAML, Namespace: "prod"})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func echoBinding(t *testing.T, c client.Client, ns, agent string) agentsv1beta1.MCPToolBinding {
	t.Helper()
	var b agentsv1beta1.MCPToolBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: agent + "-echo"}, &b))
	return b
}

// TestCreateAgentBindsTheServerTheCallerCanUse: Alice's private server sorts first and also
// offers echo. The operator's create binds the team server; it used to pick Alice's and refuse
// with 403 "tool echo is on a personal MCP server you do not own". Alice still gets her own.
func TestCreateAgentBindsTheServerTheCallerCanUse(t *testing.T) {
	store := echoRegistries(t, "prod")

	c := identityClient(t, operatorUser)
	rec := createAgentAs(t, c, store, managedEchoAgent("ops-agent"))
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	b := echoBinding(t, c, "prod", "ops-agent")
	assert.Equal(t, teamEchoRegistry, b.Spec.RegistryRef)
	assert.Equal(t, teamEchoURL, b.Spec.Server.URL)

	c = identityClient(t, aliceUser)
	rec = createAgentAs(t, c, store, managedEchoAgent("alice-agent"))
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	b = echoBinding(t, c, "prod", "alice-agent")
	assert.Equal(t, alicePrivateRegistry, b.Spec.RegistryRef, "the owner may bind her own private server")
	assert.Equal(t, alicePrivateURL, b.Spec.Server.URL)
}

// TestEditAgentBindsTheServerTheEditorCanUse: an edit re-applies every binding, so it resolves
// from the editor's view too — it used to re-point the operator's binding at Alice's private
// server, with no owner guard at all.
func TestEditAgentBindsTheServerTheEditorCanUse(t *testing.T) {
	src := managedEchoAgent("echo")
	ad := consoleAgent(t, "echo", src, agentsv1beta1.AgentDeploymentSpec{})
	c := identityClient(t, operatorUser, ad)
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	s.toolRegistryStore = echoRegistries(t, detailNS)

	rec := putAgent(t, s, "echo", src+"image: img:2\n")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	b := echoBinding(t, c, detailNS, "echo")
	assert.Equal(t, teamEchoRegistry, b.Spec.RegistryRef)
	assert.Equal(t, teamEchoURL, b.Spec.Server.URL)
}

// alicePrivateBinding is the binding Alice's create left for agent's tool: her private server.
func alicePrivateBinding(agent, tool string) *agentsv1beta1.MCPToolBinding {
	return &agentsv1beta1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: detailNS, Name: agent + "-" + tool},
		Spec: agentsv1beta1.MCPToolBindingSpec{
			AgentRef: agent, RegistryRef: alicePrivateRegistry, ToolName: tool, Mode: "remote",
			Server: agentsv1beta1.ToolServer{URL: alicePrivateURL},
		},
	}
}

// TestEditAgentKeepsTheToolsItAlreadyHasBound: Alice's agent has echo bound to her private
// server. Another user's edit that changes only the image used to re-resolve every binding from
// the editor's view: it re-pointed echo at the team server (200), or, with no team server, was
// refused 400. A tool already bound now keeps its binding, whoever edits.
func TestEditAgentKeepsTheToolsItAlreadyHasBound(t *testing.T) {
	img := "img:2"
	src := managedEchoAgent("echo")
	for _, srv := range []struct {
		name       string
		teamServer bool
	}{
		{"a team server also offers the tool", true},
		{"only the owner's private server offers it", false},
	} {
		for _, edit := range []struct {
			name string
			body UpdateAgentRequest
		}{
			{"field edit", UpdateAgentRequest{Image: &img}},
			{"full spec", UpdateAgentRequest{AgentYAML: src + "image: img:2\n"}},
		} {
			t.Run(srv.name+"/"+edit.name, func(t *testing.T) {
				ad := consoleAgent(t, "echo", src, agentsv1beta1.AgentDeploymentSpec{Image: "img:1"})
				c := identityClient(t, operatorUser, ad, alicePrivateBinding("echo", "echo"))
				s := newCallerServer(t, &fakeCallerClientFactory{client: c})
				store := echoRegistries(t, detailNS)
				if !srv.teamServer {
					require.NoError(t, store.Delete(context.Background(), detailNS, teamEchoRegistry))
				}
				s.toolRegistryStore = store

				body, err := json.Marshal(edit.body)
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPut, "/api/agents/"+detailNS+"/echo", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer caller-token")
				s.Handler().ServeHTTP(rec, req)

				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
				b := echoBinding(t, c, detailNS, "echo")
				assert.Equal(t, alicePrivateRegistry, b.Spec.RegistryRef, "the binding is unchanged")
				assert.Equal(t, alicePrivateURL, b.Spec.Server.URL)
				var live agentsv1beta1.AgentDeployment
				require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: detailNS, Name: "echo"}, &live))
				assert.Equal(t, "img:2", live.Spec.Image, "the edit itself lands")
			})
		}
	}
}

// TestEditAgentBindsAnAddedToolFromTheEditorsView: a tool the edit adds is resolved as a create
// resolves it, among the servers the editor may use and under the owner guard, while the tools
// already bound stay where they are.
func TestEditAgentBindsAnAddedToolFromTheEditorsView(t *testing.T) {
	const searchTeamURL = "https://search.example/mcp"
	src := managedEchoAgent("echo")
	seed := func(t *testing.T, teamOffersSearch bool) toolregistry.Store {
		t.Helper()
		store := echoRegistries(t, detailNS)
		alice, err := store.Get(context.Background(), detailNS, alicePrivateRegistry)
		require.NoError(t, err)
		alice.Tools = append(alice.Tools, toolregistry.ToolEntry{Name: "search", URL: alicePrivateURL})
		_, err = store.Upsert(context.Background(), *alice)
		require.NoError(t, err)
		if teamOffersSearch {
			team, gErr := store.Get(context.Background(), detailNS, teamEchoRegistry)
			require.NoError(t, gErr)
			team.Tools = append(team.Tools, toolregistry.ToolEntry{Name: "search", URL: searchTeamURL})
			_, err = store.Upsert(context.Background(), *team)
			require.NoError(t, err)
		}
		return store
	}
	setup := func(t *testing.T, teamOffersSearch bool) (*Server, client.Client) {
		ad := consoleAgent(t, "echo", src, agentsv1beta1.AgentDeploymentSpec{Image: "img:1"})
		c := identityClient(t, operatorUser, ad, alicePrivateBinding("echo", "echo"))
		s := newCallerServer(t, &fakeCallerClientFactory{client: c})
		s.toolRegistryStore = seed(t, teamOffersSearch)
		return s, c
	}

	t.Run("a server the editor may use", func(t *testing.T) {
		s, c := setup(t, true)
		rec := putAgent(t, s, "echo", src+"  - search\n")
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		var search agentsv1beta1.MCPToolBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: detailNS, Name: "echo-search"}, &search))
		assert.Equal(t, teamEchoRegistry, search.Spec.RegistryRef, "the added tool binds the server the editor can use")
		assert.Equal(t, searchTeamURL, search.Spec.Server.URL)
		assert.Equal(t, alicePrivateRegistry, echoBinding(t, c, detailNS, "echo").Spec.RegistryRef, "the bound tool stays")
	})

	t.Run("only another user's private server", func(t *testing.T) {
		s, c := setup(t, false)
		rec := putAgent(t, s, "echo", src+"  - search\nimage: img:2\n")
		require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), `tool \"search\"`)
		var live agentsv1beta1.AgentDeployment
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: detailNS, Name: "echo"}, &live))
		assert.Equal(t, "img:1", live.Spec.Image, "a refused edit changes nothing")
	})
}

// TestEditAgentDoesNotAdoptAnotherAgentsBinding: a live binding is kept only when it is this
// agent's binding of the same tool. One of the regenerated name that binds the tool for another
// agent (binding names are "<agent>-<tool>", and a binding applied outside the console can have
// any name) is not adopted: the tool is resolved as new.
func TestEditAgentDoesNotAdoptAnotherAgentsBinding(t *testing.T) {
	src := "name: a\nruntime: managed\nsystemPrompt: hi\ntools:\n  - b-echo\n"
	ad := consoleAgent(t, "a", src, agentsv1beta1.AgentDeploymentSpec{Image: "img:1"})
	other := alicePrivateBinding("a", "b-echo")
	other.Spec.AgentRef = "other-agent"
	c := identityClient(t, operatorUser, ad, other)
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	store := toolregistry.NewMemStore()
	_, err := store.Upsert(context.Background(), toolregistry.ToolRegistry{
		Namespace: detailNS, Name: teamEchoRegistry,
		Tools: []toolregistry.ToolEntry{{Name: "b-echo", URL: teamEchoURL}},
		Labels: map[string]string{
			labelManagedBy: managedByMCP, labelMCPScope: scopePublic,
			labelMCPVisibility: visibilityTeam, labelMCPCredentialSource: credSourceNone,
		},
	})
	require.NoError(t, err)
	s.toolRegistryStore = store

	rec := putAgent(t, s, "a", src+"image: img:2\n")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var b agentsv1beta1.MCPToolBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: detailNS, Name: "a-b-echo"}, &b))
	assert.Equal(t, teamEchoRegistry, b.Spec.RegistryRef, "resolved from the editor's view, not taken from the other agent's binding")
	assert.Equal(t, teamEchoURL, b.Spec.Server.URL)
}

// TestEditAgentRefusesWhenItCannotReadTheBindings: keeping a bound tool's server needs the live
// binding. A read that fails is not taken as "not bound", which would re-resolve it; the edit is
// refused and nothing is applied.
func TestEditAgentRefusesWhenItCannotReadTheBindings(t *testing.T) {
	src := managedEchoAgent("echo")
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "mcptoolbindings"}, "echo-echo", errors.New("denied")), http.StatusForbidden},
		{"unavailable", errors.New("etcdserver: request timed out"), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ad := consoleAgent(t, "echo", src, agentsv1beta1.AgentDeploymentSpec{Image: "img:1"})
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithObjects(ad, alicePrivateBinding("echo", "echo")).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*agentsv1beta1.MCPToolBinding); ok {
							return tc.err
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).Build()
			s := newCallerServer(t, &fakeCallerClientFactory{client: c})
			s.toolRegistryStore = echoRegistries(t, detailNS)

			rec := putAgent(t, s, "echo", src+"image: img:2\n")

			require.Equal(t, tc.status, rec.Code, "body: %s", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "etcdserver")
			var live agentsv1beta1.AgentDeployment
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: detailNS, Name: "echo"}, &live))
			assert.Equal(t, "img:1", live.Spec.Image, "a refused edit changes nothing")
		})
	}
}

// failingListStore is a ToolRegistry store whose List always fails.
type failingListStore struct{ toolregistry.Store }

func (failingListStore) List(context.Context, controlplane.ListOptions) (controlplane.Page[toolregistry.ToolRegistry], error) {
	return controlplane.Page[toolregistry.ToolRegistry]{}, errors.New("connection refused")
}

// TestCreateAgentRefusesAToolNoServerProvides: a managed agent's tool that no server the
// caller may use provides used to be bound to expand's default registry, which nothing
// provisions, and the agent ran without it. The create is now refused with 400 naming the
// tool, and nothing is created — including when the only server offering it is another
// user's private one.
func TestCreateAgentRefusesAToolNoServerProvides(t *testing.T) {
	cases := []struct {
		name, agent, missing string
	}{
		{"unregistered tool", "name: a1\nruntime: managed\nsystemPrompt: hi\ntools:\n  - echo\n  - nope\n", "nope"},
		{"only on another user's private server", "name: a2\nruntime: managed\nsystemPrompt: hi\ntools:\n  - echo\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := echoRegistries(t, "prod")
			missing := tc.missing
			if missing == "" {
				// Only Alice's private server offers echo here.
				require.NoError(t, store.Delete(context.Background(), "prod", teamEchoRegistry))
				missing = "echo"
			}
			c := identityClient(t, operatorUser)
			rec := createAgentAs(t, c, store, tc.agent)

			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), fmt.Sprintf(`tool \"%s\"`, missing))
			assert.Contains(t, rec.Body.String(), "add the MCP server that provides it first")
			assertNothingCreated(t, c, "prod")
		})
	}
}

// TestCreateAgentWithToolsNeedsTheStore: without the control-plane store no server can be
// registered or resolved, so a managed agent with tools is refused (501) rather than created
// with bindings that can never resolve; a store read failure is a 500, never a guess.
func TestCreateAgentWithToolsNeedsTheStore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		store  toolregistry.Store
		status int
	}{
		{"no store", nil, http.StatusNotImplemented},
		{"store read fails", failingListStore{}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := identityClient(t, operatorUser)
			rec := createAgentAs(t, c, tc.store, managedEchoAgent("no-store"))
			require.Equal(t, tc.status, rec.Code, "body: %s", rec.Body.String())
			assertNothingCreated(t, c, "prod")
		})
	}

	// An agent with no tools never needs the store.
	c := identityClient(t, operatorUser)
	rec := createAgentAs(t, c, nil, "name: plain\nruntime: managed\nsystemPrompt: hi\n")
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

// TestToolStoreFailureIsLoggedNotSent: a failed read of the MCP servers is a server fault. The
// client gets a generic 500 naming what failed; the store's own error text, which can carry
// database detail, goes only to the log. Create and edit share the path.
func TestToolStoreFailureIsLoggedNotSent(t *testing.T) {
	const cause = "connection refused"
	captureLog := func(s *Server) *logBuffer {
		lb := &logBuffer{}
		s.log = funcr.New(func(prefix, args string) { lb.write(prefix, args) }, funcr.Options{})
		return lb
	}

	t.Run("create", func(t *testing.T) {
		c := identityClient(t, operatorUser)
		s := newConfigServer(t, c)
		s.toolRegistryStore = failingListStore{}
		lb := captureLog(s)
		body, err := json.Marshal(CreateAgentRequest{AgentYAML: managedEchoAgent("store-down"), Namespace: "prod"})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer caller-token")
		s.Handler().ServeHTTP(rec, req)

		require.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), `failed to read the MCP servers in namespace \"prod\"`)
		assert.NotContains(t, rec.Body.String(), cause)
		assert.Contains(t, lb.String(), cause, "the cause is logged")
	})

	t.Run("edit", func(t *testing.T) {
		src := managedEchoAgent("echo")
		ad := consoleAgent(t, "echo", src, agentsv1beta1.AgentDeploymentSpec{})
		c := identityClient(t, operatorUser, ad)
		s := newCallerServer(t, &fakeCallerClientFactory{client: c})
		s.toolRegistryStore = failingListStore{}
		lb := captureLog(s)

		rec := putAgent(t, s, "echo", src+"image: img:2\n")

		require.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())
		assert.NotContains(t, rec.Body.String(), cause)
		assert.Contains(t, lb.String(), cause, "the cause is logged")
	})
}

// TestEditAgentRefusesAToolNoServerProvides: the edit path shares the create's binding, so an
// edit adding an unprovided tool is refused and the live agent is left as it was.
func TestEditAgentRefusesAToolNoServerProvides(t *testing.T) {
	src := managedEchoAgent("echo")
	ad := consoleAgent(t, "echo", src, agentsv1beta1.AgentDeploymentSpec{Image: "img:1"})
	c := identityClient(t, operatorUser, ad)
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	s.toolRegistryStore = echoRegistries(t, detailNS)

	rec := putAgent(t, s, "echo", src+"  - nope\nimage: img:2\n")

	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `tool \"nope\"`)
	assert.Contains(t, rec.Body.String(), "add the MCP server that provides it first")
	var live agentsv1beta1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: detailNS, Name: "echo"}, &live))
	assert.Equal(t, "img:1", live.Spec.Image, "a refused edit changes nothing")
	var bindings agentsv1beta1.MCPToolBindingList
	require.NoError(t, c.List(context.Background(), &bindings, client.InNamespace(detailNS)))
	assert.Empty(t, bindings.Items)
}

func assertNothingCreated(t *testing.T, c client.Client, ns string) {
	t.Helper()
	var agents agentsv1beta1.AgentDeploymentList
	require.NoError(t, c.List(context.Background(), &agents, client.InNamespace(ns)))
	assert.Empty(t, agents.Items, "no agent is created")
	var bindings agentsv1beta1.MCPToolBindingList
	require.NoError(t, c.List(context.Background(), &bindings, client.InNamespace(ns)))
	assert.Empty(t, bindings.Items, "no binding is created")
}
