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

package toolmanifest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toolByName(m Manifest, name string) (Tool, bool) {
	for _, t := range m.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func TestRewriteRemoteForEgress(t *testing.T) {
	bindings := []Binding{
		// Two remote OBO tools on the SAME server (must dedupe to one route).
		{BindingName: "b-env", ToolName: "list_environments", Mode: ModeRemote, URL: "https://mcp.scalekit.com/mcp", ServerName: "scalekit", OAuth: true},
		{BindingName: "b-org", ToolName: "list_orgs", Mode: ModeRemote, URL: "https://mcp.scalekit.com/mcp", ServerName: "scalekit", OAuth: true},
		// A sidecar-mode tool — untouched (already in-pod).
		{BindingName: "b-echo", ToolName: "echo", Mode: ModeSidecar, Image: "echo:1"},
		// A remote tool with NO server name — left verbatim (not an OBO tool).
		{BindingName: "b-open", ToolName: "public_tool", Mode: ModeRemote, URL: "https://open.example/mcp"},
	}

	manifest, _ := Render(bindings)
	rewritten, routes := RewriteRemoteForEgress(manifest, bindings, "http://127.0.0.1:8081")

	// Routes: one deduped entry for the shared server, carrying the REAL url + oauth.
	require.Len(t, routes, 1)
	assert.Equal(t, Route{Name: "scalekit", TargetURL: "https://mcp.scalekit.com/mcp", OAuth: true}, routes[0])

	// Both OBO tools now point at the sidecar keyed by the server name — never the real URL.
	for _, name := range []string{"list_environments", "list_orgs"} {
		tool, ok := toolByName(rewritten, name)
		require.True(t, ok)
		assert.Equal(t, "http://127.0.0.1:8081/scalekit", tool.Endpoint, "%s → sidecar", name)
	}

	// The sidecar-mode tool keeps its localhost port; the non-OBO remote keeps its URL.
	echo, _ := toolByName(rewritten, "echo")
	assert.Equal(t, "http://127.0.0.1:3001/mcp", echo.Endpoint)
	open, _ := toolByName(rewritten, "public_tool")
	assert.Equal(t, "https://open.example/mcp", open.Endpoint, "a non-OBO remote tool is left verbatim")

	// The real Scalekit URL never appears in the agent's manifest.
	for _, tool := range rewritten.Tools {
		assert.NotContains(t, tool.Endpoint, "mcp.scalekit.com", "the real MCP URL must not reach the agent manifest")
	}
	// The endpoints changed ⇒ the content-addressed version changed (SDK sees the new manifest).
	assert.NotEqual(t, manifest.Version, rewritten.Version)
}

func TestRewriteRemoteForEgressNoOBOTools(t *testing.T) {
	// No remote binding carries a ServerName ⇒ nothing to rewrite.
	bindings := []Binding{
		{BindingName: "b-echo", ToolName: "echo", Mode: ModeSidecar, Image: "echo:1"},
		{BindingName: "b-open", ToolName: "public_tool", Mode: ModeRemote, URL: "https://open.example/mcp"},
	}
	manifest, _ := Render(bindings)
	rewritten, routes := RewriteRemoteForEgress(manifest, bindings, "http://127.0.0.1:8081")
	assert.Nil(t, routes)
	assert.Equal(t, manifest.Version, rewritten.Version, "no OBO tools ⇒ manifest unchanged")
}
