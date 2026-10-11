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
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
)

// TestOutboundProbesWaitForRealRBAC: against a real API server and the shipped personas, a
// viewer is refused provider connect and MCP registration before the BFF contacts the URL in
// the body, and a developer's registration still probes and lands.
func TestOutboundProbesWaitForRealRBAC(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	const ns = "outbound-authz"
	viewerRole := installPersona(t, admin, "ctxmesh_viewer_role.yaml")
	developerRole := installPersona(t, admin, "ctxmesh_developer_role.yaml")
	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	viewer := personaUser(t, admin, ns, "viewer-"+ns, viewerRole)
	developer := personaUser(t, admin, ns, "developer-"+ns, developerRole)

	mcpServer := func(c client.Client) *Server {
		s, _, _ := newMCPServer(t, c, false)
		s.authorizer = authz.SSARAuthorizer{}
		return s
	}
	registerIn := func(name, url string) []byte {
		b, err := json.Marshal(RegisterMCPServerRequest{Name: name, URL: url, Namespace: ns})
		require.NoError(t, err)
		return b
	}

	t.Run("viewer connect", func(t *testing.T) {
		prov, hits := countingServer(t, anyKeyProvider(t).Config.Handler)
		s, _, _ := newRBACConnectServer(t, viewer)
		body, err := json.Marshal(ConnectProviderRequest{
			Provider: providerCustom, APIKey: "sk-viewer", BaseURL: prov.URL, Namespace: ns,
		})
		require.NoError(t, err)
		rec := postAs(s, "/api/providers", body)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Zero(t, hits.Load(), "the viewer's connect must not reach the base URL")
	})

	t.Run("viewer register", func(t *testing.T) {
		mcp, hits := countingServer(t, fakeMCPServer(t, false).Config.Handler)
		rec := postAs(mcpServer(viewer), "/api/mcpservers", registerIn("viewer-probe", mcp.URL))
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Zero(t, hits.Load(), "the viewer's register must not reach the URL")
	})

	t.Run("developer register", func(t *testing.T) {
		mcp, hits := countingServer(t, fakeMCPServer(t, false).Config.Handler)
		rec := postAs(mcpServer(developer), "/api/mcpservers", registerIn("developer-probe", mcp.URL))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Positive(t, hits.Load(), "an authorized register probes the server")
	})
}
