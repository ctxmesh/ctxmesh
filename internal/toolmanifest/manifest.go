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

// Package toolmanifest defines the tool manifest document shared by the
// discovery sidecar (live manifest + SSE), the controller (renders it from
// MCPToolBindings and pushes it), and the tools.json ConfigMap (durable
// backing). See agent-brain specs/mcp-tools.md.
package toolmanifest

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Transport is the only MCP transport supported in M4.
const Transport = "streamable-http"

// Tool is one entry in the manifest.
type Tool struct {
	// Name is the tool's catalog name (ToolRegistry key).
	Name string `json:"name"`
	// Mode is "sidecar" or "remote".
	Mode string `json:"mode"`
	// Endpoint is where the agent reaches the MCP server: a localhost URL
	// for sidecar mode, a cluster URL for remote mode.
	Endpoint string `json:"endpoint"`
	// Transport is the MCP transport (always streamable-http in M4).
	Transport string `json:"transport"`
}

// Manifest is the document served at /tools, pushed to /control, and stored
// in tools.json. Version identifies the content: identical tool sets always
// produce identical versions.
type Manifest struct {
	Version string `json:"version"`
	Tools   []Tool `json:"tools"`
}

// Normalize sorts tools by name and recomputes Version. Call before
// serving, pushing, or persisting so equality and hashing are stable.
func Normalize(m Manifest) Manifest {
	tools := make([]Tool, len(m.Tools))
	copy(tools, m.Tools)
	slices.SortFunc(tools, func(a, b Tool) int { return strings.Compare(a.Name, b.Name) })
	m.Tools = tools
	m.Version = computeVersion(tools)
	return m
}

// computeVersion hashes the canonical (sorted) tool list — the version field
// itself is excluded so it is purely content-addressed.
func computeVersion(tools []Tool) string {
	b, err := json.Marshal(tools)
	if err != nil {
		// Tools contain only strings; Marshal cannot fail. Guard anyway.
		return "invalid"
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])[:8]
}
