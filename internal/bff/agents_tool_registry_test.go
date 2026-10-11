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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
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

func createAgentAs(t *testing.T, c client.Client, store toolregistry.Store, agentYAML, ns string) *httptest.ResponseRecorder {
	t.Helper()
	s := newConfigServer(t, c)
	s.toolRegistryStore = store
	body, err := json.Marshal(CreateAgentRequest{AgentYAML: agentYAML, Namespace: ns})
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
	rec := createAgentAs(t, c, store, managedEchoAgent("ops-agent"), "prod")
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	b := echoBinding(t, c, "prod", "ops-agent")
	assert.Equal(t, teamEchoRegistry, b.Spec.RegistryRef)
	assert.Equal(t, teamEchoURL, b.Spec.Server.URL)

	c = identityClient(t, aliceUser)
	rec = createAgentAs(t, c, store, managedEchoAgent("alice-agent"), "prod")
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
