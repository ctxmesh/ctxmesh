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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const (
	// ModeSidecar runs the tool server inside the agent pod, reached over
	// localhost.
	ModeSidecar = "sidecar"
	// ModeRemote reaches a standalone tool server over the cluster network.
	ModeRemote = "remote"

	// SidecarBasePort is the first localhost port assigned to a sidecar-mode
	// tool container. Subsequent sidecar tools take 3002, 3003, … in
	// binding-name order (specs/mcp-tools.md).
	SidecarBasePort = 3001

	// SidecarLoopbackHost is the address a sidecar-mode tool server is reached
	// at from the agent container (same pod netns).
	SidecarLoopbackHost = "127.0.0.1"

	// MCPPath is the streamable-http MCP path FastMCP servers serve at. Sidecar
	// endpoints append it; remote endpoints carry it verbatim in the binding
	// URL (specs/mcp-tools.md — "Endpoint path").
	MCPPath = "/mcp"
)

// Binding is the render-time view of a validated MCPToolBinding: only the
// fields that shape the manifest and the pod template. The controller resolves
// each binding to one of these (dropping invalid ones) before rendering.
//
// It deliberately does NOT depend on the api/v1alpha1 types so the render logic
// stays a pure, dependency-light unit under test.
type Binding struct {
	// BindingName is the MCPToolBinding object name. It orders deterministic
	// sidecar port assignment and container naming.
	BindingName string
	// ToolName is the catalog tool name that becomes the manifest entry name.
	ToolName string
	// Mode is ModeSidecar or ModeRemote.
	Mode string
	// Image is the sidecar-mode tool-server container image (empty for remote).
	Image string
	// URL is the remote-mode server URL, carried verbatim into the manifest
	// endpoint (must already include the /mcp path). Empty for sidecar.
	URL string
}

// SidecarTool describes an assigned sidecar-mode tool container: which image to
// run and the localhost port it listens on. The AgentDeployment reconciler
// turns these into tool containers; the manifest reaches them at
// http://127.0.0.1:<Port>/mcp.
type SidecarTool struct {
	BindingName string
	ToolName    string
	Image       string
	Port        int32
}

// Render turns a set of validated bindings into the manifest served to the
// agent AND the list of sidecar-mode tool containers the pod must run.
//
//   - remote binding → manifest endpoint = binding URL verbatim (already carries
//     /mcp).
//   - sidecar binding → assigned localhost port (3001+ in binding-name order),
//     manifest endpoint = http://127.0.0.1:<port>/mcp.
//
// Bindings are sorted by BindingName first so port assignment and the manifest
// are deterministic regardless of input order. The returned Manifest is
// Normalize'd (sorted by tool name + content-addressed version).
func Render(bindings []Binding) (Manifest, []SidecarTool) {
	sorted := make([]Binding, len(bindings))
	copy(sorted, bindings)
	slices.SortFunc(sorted, func(a, b Binding) int {
		return strings.Compare(a.BindingName, b.BindingName)
	})

	tools := make([]Tool, 0, len(sorted))
	var sidecars []SidecarTool

	nextPort := int32(SidecarBasePort)
	for _, b := range sorted {
		switch b.Mode {
		case ModeSidecar:
			port := nextPort
			nextPort++
			tools = append(tools, Tool{
				Name:      b.ToolName,
				Mode:      ModeSidecar,
				Endpoint:  fmt.Sprintf("http://%s:%d%s", SidecarLoopbackHost, port, MCPPath),
				Transport: Transport,
			})
			sidecars = append(sidecars, SidecarTool{
				BindingName: b.BindingName,
				ToolName:    b.ToolName,
				Image:       b.Image,
				Port:        port,
			})
		default: // ModeRemote
			tools = append(tools, Tool{
				Name:      b.ToolName,
				Mode:      ModeRemote,
				Endpoint:  b.URL, // verbatim — binding URLs carry /mcp
				Transport: Transport,
			})
		}
	}

	return Normalize(Manifest{Tools: tools}), sidecars
}

// StructuralDigest is the load-bearing hash for the Knative revision name: it
// captures ONLY the pod-template shape a binding set induces — which sidecar
// tool containers exist (by name + image + assigned port) and whether the
// discovery sidecar/volume are present at all. It deliberately EXCLUDES
// remote-mode endpoints and the manifest version, because a remote URL change
// is a manifest-only (hot-path) update that must NOT roll a new revision
// (specs/mcp-tools.md — "Hot path vs cold path"). Remote pod-template state is
// identical regardless of the URL: the discovery sidecar + CM volume are the
// same containers.
//
// Contract, unit-tested in both directions:
//   - add/remove a binding, or change a sidecar image → digest changes → new
//     revision name → pod rolls (deploy-time semantics).
//   - change only a remote URL → digest UNCHANGED → same revision name → no
//     roll → restart-free hot path (push + SSE propagate the manifest).
//
// The empty binding set returns "" so callers can special-case "no bindings"
// (revision name stays the bare spec-hash, unchanged from pre-M4 behaviour).
func StructuralDigest(sidecars []SidecarTool, hasBindings bool) string {
	if !hasBindings {
		return ""
	}

	// Canonical, order-independent shape. Sidecar containers by
	// name/image/port; a marker that the discovery sidecar + volume are present
	// (true whenever hasBindings). Remote URLs are intentionally absent.
	sorted := make([]SidecarTool, len(sidecars))
	copy(sorted, sidecars)
	slices.SortFunc(sorted, func(a, b SidecarTool) int {
		return strings.Compare(a.BindingName, b.BindingName)
	})

	type shape struct {
		DiscoverySidecar bool          `json:"discovery"`
		Sidecars         []SidecarTool `json:"sidecars"`
	}
	b, err := json.Marshal(shape{DiscoverySidecar: true, Sidecars: sorted})
	if err != nil {
		return "invalid"
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])[:8]
}
