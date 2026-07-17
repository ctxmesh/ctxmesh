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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// scopedRegistry builds a register-managed ToolRegistry with a scope + owner label.
func scopedRegistry(name, scope, owner string) *agentsv1alpha1.ToolRegistry {
	labels := map[string]string{labelManagedBy: managedByMCP}
	if scope != "" {
		labels[labelMCPScope] = scope
	}
	if owner != "" {
		labels[labelMCPOwner] = owner
	}
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod", Labels: labels},
		Spec:       agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{Name: name + "-tool"}}},
	}
}

func TestMCPScopeVisibleTo(t *testing.T) {
	aliceHash := userGrantHash("alice@example.com")
	bobHash := userGrantHash("bob@example.com")

	// public / org / absent (grandfathered) are visible to everyone.
	for _, scope := range []string{scopePublic, scopeOrg, ""} {
		assert.True(t, mcpScopeVisibleTo(scopedRegistry("s", scope, ""), aliceHash), "scope %q visible to all", scope)
		assert.True(t, mcpScopeVisibleTo(scopedRegistry("s", scope, ""), ""), "scope %q visible even with no caller", scope)
	}

	// personal: only the owner sees it.
	personal := scopedRegistry("alices", scopePersonal, aliceHash)
	assert.True(t, mcpScopeVisibleTo(personal, aliceHash), "owner sees their personal server")
	assert.False(t, mcpScopeVisibleTo(personal, bobHash), "a non-owner never sees another's personal server")
	assert.False(t, mcpScopeVisibleTo(personal, ""), "an unresolved caller never sees a personal server (fail-closed)")
}

// listServersAs issues GET /api/mcpservers as the given username (via the SSR interceptor)
// and returns the server names in the response.
func listServersAs(t *testing.T, objs []*agentsv1alpha1.ToolRegistry, username string) []string {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(ssrInterceptor(username, nil))
	for _, o := range objs {
		builder = builder.WithObjects(o)
	}
	s, _, _ := newMCPServer(t, builder.Build(), false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcpservers?namespace=prod", nil)
	req.Header.Set("Authorization", "Bearer tok")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp MCPServerListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	names := make([]string, 0, len(resp.Servers))
	for _, srv := range resp.Servers {
		names = append(names, srv.Name)
	}
	return names
}

func TestCheckBindingOwnership(t *testing.T) {
	aliceHash := userGrantHash("alice@example.com")
	bobHash := userGrantHash("bob@example.com")
	idx := map[string]toolLoc{
		"alice_tool":  {registry: "alice-mcp", scope: scopePersonal, owner: aliceHash},
		"public_tool": {registry: "open-mcp", scope: scopePublic},
		"org_tool":    {registry: "shared-mcp", scope: scopeOrg},
	}
	bind := func(tool string) []decodedObject {
		return []decodedObject{{obj: &agentsv1alpha1.MCPToolBinding{Spec: agentsv1alpha1.MCPToolBindingSpec{ToolName: tool}}}}
	}

	// The owner may bind their personal server's tool.
	assert.Nil(t, checkBindingOwnership(bind("alice_tool"), idx, aliceHash))
	// A non-owner (or unresolved caller) is refused — an honest 403.
	for _, owner := range []string{bobHash, ""} {
		err := checkBindingOwnership(bind("alice_tool"), idx, owner)
		require.NotNil(t, err)
		assert.Equal(t, 403, err.status)
	}
	// Public / org tools + tools absent from the index are unrestricted.
	assert.Nil(t, checkBindingOwnership(bind("public_tool"), idx, bobHash))
	assert.Nil(t, checkBindingOwnership(bind("org_tool"), idx, bobHash))
	assert.Nil(t, checkBindingOwnership(bind("unknown_tool"), idx, ""))
}

func TestListMCPServersOwnerFiltered(t *testing.T) {
	aliceHash := userGrantHash("alice@example.com")
	objs := []*agentsv1alpha1.ToolRegistry{
		scopedRegistry("alice-personal", scopePersonal, aliceHash),
		scopedRegistry("open-public", scopePublic, ""),
		scopedRegistry("shared-org", scopeOrg, ""),
		scopedRegistry("legacy-grandfathered", "", ""), // no scope label ⇒ org
	}

	// Alice (the owner) sees her personal server + all shared/public/legacy.
	assert.ElementsMatch(t,
		[]string{"alice-personal", "open-public", "shared-org", "legacy-grandfathered"},
		listServersAs(t, objs, "alice@example.com"))

	// Bob sees everything EXCEPT alice's personal server.
	assert.ElementsMatch(t,
		[]string{"open-public", "shared-org", "legacy-grandfathered"},
		listServersAs(t, objs, "bob@example.com"))
}
