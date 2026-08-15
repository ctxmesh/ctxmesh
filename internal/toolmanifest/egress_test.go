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

// TestRewriteAllForEgress (M78, ADR 0071 §1/C1): record mode fronts EVERY tool through the sidecar
// — remote OBO (keyed on ServerName, OAuth preserved), plain remote (keyed on ToolName, no auth),
// AND in-pod sidecar tools (keyed on ToolName, routed to their localhost endpoint). The real
// endpoints move into the route table; the agent manifest points only at the sidecar.
func TestRewriteAllForEgress(t *testing.T) {
	bindings := []Binding{
		// Two remote OBO tools on the SAME server → one deduped route keyed on the server name.
		{BindingName: "b-env", ToolName: "list_environments", Mode: ModeRemote, URL: "https://mcp.scalekit.com/mcp", ServerName: "scalekit", OAuth: true},
		{BindingName: "b-org", ToolName: "list_orgs", Mode: ModeRemote, URL: "https://mcp.scalekit.com/mcp", ServerName: "scalekit", OAuth: true},
		// A sidecar-mode tool — now ALSO fronted (routed to its in-pod localhost endpoint).
		{BindingName: "b-echo", ToolName: "echo", Mode: ModeSidecar, Image: "echo:1"},
		// A plain remote (open) tool with no ServerName — fronted under its tool name, no auth.
		{BindingName: "b-open", ToolName: "public_tool", Mode: ModeRemote, URL: "https://open.example/mcp"},
	}
	const base = "http://127.0.0.1:8899"
	manifest, _ := Render(bindings)
	rewritten, routes := RewriteAllForEgress(manifest, bindings, base)

	// Route table: OBO server (OAuth), the sidecar tool → localhost endpoint, the open tool → its URL.
	byName := map[string]Route{}
	for _, r := range routes {
		byName[r.Name] = r
	}
	require.Contains(t, byName, "scalekit")
	assert.Equal(t, Route{Name: "scalekit", TargetURL: "https://mcp.scalekit.com/mcp", OAuth: true}, byName["scalekit"])
	require.Contains(t, byName, "echo")
	assert.Equal(t, Route{Name: "echo", TargetURL: "http://127.0.0.1:3001/mcp", OAuth: false}, byName["echo"], "sidecar tool fronted to its localhost endpoint, no auth")
	require.Contains(t, byName, "public_tool")
	assert.Equal(t, Route{Name: "public_tool", TargetURL: "https://open.example/mcp", OAuth: false}, byName["public_tool"], "open remote fronted under its tool name, no auth")
	assert.Len(t, routes, 3, "one route per server (OBO deduped) + one per non-OBO tool")

	// EVERY tool endpoint now points at the sidecar under its route segment.
	env, _ := toolByName(rewritten, "list_environments")
	assert.Equal(t, base+"/scalekit", env.Endpoint)
	org, _ := toolByName(rewritten, "list_orgs")
	assert.Equal(t, base+"/scalekit", org.Endpoint)
	echo, _ := toolByName(rewritten, "echo")
	assert.Equal(t, base+"/echo", echo.Endpoint, "the in-pod sidecar tool now hops through the egress sidecar")
	open, _ := toolByName(rewritten, "public_tool")
	assert.Equal(t, base+"/public_tool", open.Endpoint)

	// No real upstream URL nor the raw localhost tool port leaks into the agent's manifest.
	for _, tool := range rewritten.Tools {
		assert.True(t, len(tool.Endpoint) > 0 && tool.Endpoint[:len(base)] == base, "%s fronted through the sidecar", tool.Name)
		assert.NotContains(t, tool.Endpoint, "mcp.scalekit.com")
		assert.NotContains(t, tool.Endpoint, "open.example")
	}
	assert.NotEqual(t, manifest.Version, rewritten.Version, "endpoints changed ⇒ new content-addressed version")
}

// TestRewriteAllForEgress_EmptyManifest: no tools ⇒ nothing to front, manifest unchanged, no routes.
func TestRewriteAllForEgress_EmptyManifest(t *testing.T) {
	manifest, _ := Render(nil)
	rewritten, routes := RewriteAllForEgress(manifest, nil, "http://127.0.0.1:8899")
	assert.Nil(t, routes)
	assert.Equal(t, manifest.Version, rewritten.Version)
}

// TestEgressRoutes_RecordVsOBO: the OBO-only table (EgressRoutes) stays byte-for-byte — only remote
// OBO servers — while the record table (via RewriteAllForEgress) is a strict superset that also
// includes the sidecar + open tools. Proves non-record behavior is unchanged.
func TestEgressRoutes_RecordVsOBO(t *testing.T) {
	bindings := []Binding{
		{BindingName: "b-obo", ToolName: "obo_tool", Mode: ModeRemote, URL: "https://obo.example/mcp", ServerName: "obo", OAuth: true},
		{BindingName: "b-side", ToolName: "side_tool", Mode: ModeSidecar, Image: "s:1"},
		{BindingName: "b-open", ToolName: "open_tool", Mode: ModeRemote, URL: "https://open.example/mcp"},
	}
	oboRoutes := EgressRoutes(bindings)
	require.Len(t, oboRoutes, 1, "OBO-only table: just the OBO server")
	assert.Equal(t, "obo", oboRoutes[0].Name)

	manifest, _ := Render(bindings)
	_, recRoutes := RewriteAllForEgress(manifest, bindings, "http://127.0.0.1:8899")
	names := map[string]bool{}
	for _, r := range recRoutes {
		names[r.Name] = true
	}
	assert.True(t, names["obo"] && names["side_tool"] && names["open_tool"], "record table fronts every tool")
	assert.Len(t, recRoutes, 3)
}
