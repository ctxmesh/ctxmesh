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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// checkNS is the namespace used in check-requirements tests.
const checkNS = "default"

// newCheckReqServer builds a Server with a fake caller client and a
// ToolRegistry memstore (ToolRegistry is retired as a CRD — ADR 0044).
// ModelRoutes are seeded via client objects (they are still CRDs).
func newCheckReqServer(t *testing.T, routes []*agentsv1alpha1.ModelRoute, regs []*agentsv1alpha1.ToolRegistry) *Server {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(testScheme(t))
	for _, r := range routes {
		builder = builder.WithObjects(r)
	}
	c := builder.Build()

	s := newTestServer(t, c)
	// Wire the ToolRegistry store (retired CRD path; nil authorizer → permissive).
	if len(regs) > 0 {
		wireTRStore(t, s, nil, regs...)
	} else {
		// Still wire an empty store so requirementsToolCheck can read.
		wireTRStore(t, s, nil)
	}
	return s
}

// postCheckRequirements sends a POST to /api/agents/check-requirements with the
// given agentYAML and returns the parsed response (namespace always = checkNS).
func postCheckRequirements(t *testing.T, s *Server, agentYAML string) CheckRequirementsResponse {
	t.Helper()
	reqBody, err := json.Marshal(CheckRequirementsRequest{AgentYAML: agentYAML, Namespace: checkNS})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/check-requirements", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "response body: %s", rec.Body.String())

	var resp CheckRequirementsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// anthropicRoute builds a connect-managed anthropic ModelRoute in checkNS.
func anthropicRoute() *agentsv1alpha1.ModelRoute {
	return &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "anthropic",
			Namespace: checkNS,
			Labels:    map[string]string{labelManagedBy: managedByConnect, labelProvider: "anthropic"},
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 1}},
		},
	}
}

// namedRoute builds a connect-managed ModelRoute with a custom name in checkNS.
func namedRoute(name string) *agentsv1alpha1.ModelRoute {
	return &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: checkNS,
			Labels:    map[string]string{labelManagedBy: managedByConnect, labelProvider: "anthropic"},
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 1}},
		},
	}
}

// TestCheckRequirements_AllReady verifies that a spec with a connected model and a
// ready (approved) tool returns all-green.
func TestCheckRequirements_AllReady(t *testing.T) {
	registry := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tools", Namespace: checkNS},
		Spec: agentsv1alpha1.ToolRegistrySpec{
			Tools: []agentsv1alpha1.ToolEntry{
				{Name: "get_order", ApprovalStatus: agentsv1alpha1.ApprovalApproved},
			},
		},
	}

	// Use namedRoute to verify that a spec with an exact route name is matched correctly.
	s := newCheckReqServer(t, []*agentsv1alpha1.ModelRoute{namedRoute("anthropic")}, []*agentsv1alpha1.ToolRegistry{registry})

	yaml := "name: my-agent\nruntime: managed\nmodel:\n  route: anthropic\ntools:\n  - get_order\n"
	resp := postCheckRequirements(t, s, yaml)

	// Model: the named route exists.
	assert.True(t, resp.Model.Required, "a named route makes model required")
	assert.True(t, resp.Model.Connected, "anthropic route is present and connected")
	assert.Equal(t, "anthropic", resp.Model.Route)

	// Tools: get_order is approved.
	require.Len(t, resp.Tools, 1)
	assert.Equal(t, "get_order", resp.Tools[0].Name)
	assert.Equal(t, toolStatusReady, resp.Tools[0].Status)
}

// TestCheckRequirements_UnconnectedModel verifies that a spec with a model.route
// that does not match any connect-managed route is flagged as not-connected.
func TestCheckRequirements_UnconnectedModel(t *testing.T) {
	// No ModelRoutes at all.
	s := newCheckReqServer(t, nil, nil)

	yaml := "name: my-agent\nruntime: managed\nmodel:\n  route: missing-route\n"
	resp := postCheckRequirements(t, s, yaml)

	assert.True(t, resp.Model.Required)
	assert.False(t, resp.Model.Connected, "no connected routes → not connected")
	assert.Equal(t, "missing-route", resp.Model.Route)
	assert.Empty(t, resp.Tools)
}

// TestCheckRequirements_ToolNotFound verifies that a tool missing from the catalog
// is returned with status "not-found".
func TestCheckRequirements_ToolNotFound(t *testing.T) {
	// A connected route so the model check is green; an empty registry (tool absent).
	s := newCheckReqServer(t, []*agentsv1alpha1.ModelRoute{anthropicRoute()}, nil)

	yaml := "name: my-agent\nruntime: managed\ntools:\n  - unknown_tool\n"
	resp := postCheckRequirements(t, s, yaml)

	assert.False(t, resp.Model.Required, "no explicit route → not required")
	assert.True(t, resp.Model.Connected, "a provider is connected")

	require.Len(t, resp.Tools, 1)
	assert.Equal(t, "unknown_tool", resp.Tools[0].Name)
	assert.Equal(t, toolStatusNotFound, resp.Tools[0].Status)
}

// TestCheckRequirements_PendingTool verifies that a tool with pending approval
// is returned with status "needs-approval".
func TestCheckRequirements_PendingTool(t *testing.T) {
	registry := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "byo-tools", Namespace: checkNS},
		Spec: agentsv1alpha1.ToolRegistrySpec{
			Tools: []agentsv1alpha1.ToolEntry{
				{Name: "risky_tool", ApprovalStatus: agentsv1alpha1.ApprovalPending},
			},
		},
	}

	s := newCheckReqServer(t, []*agentsv1alpha1.ModelRoute{anthropicRoute()}, []*agentsv1alpha1.ToolRegistry{registry})

	yaml := "name: my-agent\nruntime: managed\ntools:\n  - risky_tool\n"
	resp := postCheckRequirements(t, s, yaml)

	require.Len(t, resp.Tools, 1)
	assert.Equal(t, toolStatusNeedsApproval, resp.Tools[0].Status)
}

// TestCheckRequirements_OAuthTool verifies that a tool on a server with an OAuth
// endpoint annotation is returned with status "needs-consent".
func TestCheckRequirements_OAuthTool(t *testing.T) {
	// A registry whose server requires OAuth (has the authorization endpoint annotation).
	registry := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oauth-server",
			Namespace: checkNS,
			Annotations: map[string]string{
				annMCPOAuthAuthEndpoint: "https://example.com/oauth/authorize",
			},
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{
			Tools: []agentsv1alpha1.ToolEntry{
				{Name: "oauth_tool", ApprovalStatus: agentsv1alpha1.ApprovalApproved},
			},
		},
	}

	s := newCheckReqServer(t, []*agentsv1alpha1.ModelRoute{anthropicRoute()}, []*agentsv1alpha1.ToolRegistry{registry})

	yaml := "name: my-agent\nruntime: managed\ntools:\n  - oauth_tool\n"
	resp := postCheckRequirements(t, s, yaml)

	require.Len(t, resp.Tools, 1)
	assert.Equal(t, toolStatusNeedsConsent, resp.Tools[0].Status)
}

// TestCheckRequirements_EmptySpec verifies that an agent with no model.route and
// no tools, but a connected provider, returns model.connected=true with empty tools.
func TestCheckRequirements_EmptySpec(t *testing.T) {
	s := newCheckReqServer(t, []*agentsv1alpha1.ModelRoute{anthropicRoute()}, nil)

	yaml := "name: my-agent\nruntime: managed\n"
	resp := postCheckRequirements(t, s, yaml)

	assert.False(t, resp.Model.Required)
	assert.True(t, resp.Model.Connected)
	assert.Empty(t, resp.Tools)
}
