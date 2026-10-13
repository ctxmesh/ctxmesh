//go:build integration

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

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// TestToolCatalogForAPersonaBoundToOneNamespace: against a real API server, a developer bound by a
// RoleBinding in one namespace (the chart's default shape, which never grants a cluster-wide list)
// reads the tool catalog of that namespace, reads it again as "All workspaces" and sees only that
// namespace's tools, is refused another namespace's, and binds a catalog tool to an agent.
func TestToolCatalogForAPersonaBoundToOneNamespace(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	const mine, theirs = "catalog-mine", "catalog-theirs"
	store := toolregistry.NewMemStore()
	mirror := namespacetenant.NewMemStore()
	for ns, tool := range map[string]string{mine: "lookup", theirs: "their_lookup"} {
		require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
		require.NoError(t, mirror.RecordNamespace(ctx, ns))
		_, err := store.Upsert(ctx, toolregistry.ToolRegistry{
			Namespace: ns, Name: ns + "-tools",
			Tools:  []toolregistry.ToolEntry{{Name: tool, URL: "http://" + ns + "-tools.svc/mcp"}},
			Labels: map[string]string{labelManagedBy: managedByMCP, labelMCPVisibility: visibilityTeam},
		})
		require.NoError(t, err)
	}
	developer := personaUser(t, admin, mine, "developer-"+mine, installPersona(t, admin, "ctxmesh_developer_role.yaml"))

	s := NewServer(Options{
		CallerClients:        newFakeFactory(developer),
		Scheme:               testScheme(t),
		Auth:                 AllowAll{},
		MCPEnabled:           true,
		ProviderHTTP:         &http.Client{},
		NamespaceTenantStore: mirror,
		Version:              "test",
		Log:                  logr.Discard(),
	})
	s.toolRegistryStore = store
	send := func(method, path string, body []byte) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer developer-token")
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	toolNames := func(rec *httptest.ResponseRecorder) []string {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ToolCatalogResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		names := make([]string, 0, len(resp.Tools))
		for _, tool := range resp.Tools {
			names = append(names, tool.Namespace+"/"+tool.Name)
		}
		return names
	}

	assert.Equal(t, []string{mine + "/lookup"}, toolNames(send(http.MethodGet, "/api/tools?namespace="+mine, nil)))
	assert.Equal(t, []string{mine + "/lookup"}, toolNames(send(http.MethodGet, "/api/tools", nil)),
		"All workspaces is every namespace the caller may list")
	assert.Equal(t, http.StatusForbidden, send(http.MethodGet, "/api/tools?namespace="+theirs, nil).Code)

	body, err := json.Marshal(MCPToolBindingCreateRequest{
		Name: "aha-lookup", Namespace: mine, AgentRef: "aha", RegistryRef: mine + "-tools", ToolName: "lookup",
	})
	require.NoError(t, err)
	rec := send(http.MethodPost, "/api/mcptoolbindings", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var got agentsv1beta1.MCPToolBinding
	require.NoError(t, admin.Get(ctx, client.ObjectKey{Namespace: mine, Name: "aha-lookup"}, &got))
	assert.Equal(t, "http://"+mine+"-tools.svc/mcp", got.Spec.Server.URL)
}
