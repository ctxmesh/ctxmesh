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
	"slices"
	"strings"
)

// Route is one egress-sidecar route: the server NAME (the grant-resolution key), its REAL
// upstream URL (kept OUT of the agent's manifest — only the sidecar holds it), and whether
// the server authenticates via OAuth. Deduped by name (many tools may share one server).
// Serialized to the sidecar's EGRESS_ROUTES env by the controller (matches internal/egress
// ServerRoute).
type Route struct {
	Name      string `json:"name"`
	TargetURL string `json:"targetURL"`
	OAuth     bool   `json:"oauth"`
}

// EgressRoutes returns the deduped egress route table for a binding set: one Route per
// remote OBO server (a remote binding with a ServerName), carrying its REAL url + auth type.
// The controller serializes this into the egress sidecar's EGRESS_ROUTES env (the real URLs
// live only in the sidecar, never the agent manifest). Nil when no remote OBO tool exists.
func EgressRoutes(bindings []Binding) []Route {
	routes := make(map[string]Route)
	for _, b := range bindings {
		if b.Mode != ModeRemote || strings.TrimSpace(b.ServerName) == "" {
			continue
		}
		routes[b.ServerName] = Route{Name: b.ServerName, TargetURL: b.URL, OAuth: b.OAuth}
	}
	if len(routes) == 0 {
		return nil
	}
	list := make([]Route, 0, len(routes))
	for _, r := range routes {
		list = append(list, r)
	}
	slices.SortFunc(list, func(a, b Route) int { return strings.Compare(a.Name, b.Name) })
	return list
}

// RewriteRemoteForEgress redirects the manifest's REMOTE tool endpoints through the per-pod
// injecting egress sidecar (ADR 0030 §1): each remote tool's endpoint becomes
// sidecarBaseURL/<serverName>, and the real upstream URL is moved into the returned route
// table (so the agent container never sees it). Sidecar-mode tools are untouched — they
// already run in-pod. A remote binding with no ServerName is left verbatim (not an OBO tool).
//
// It maps each manifest tool back to its binding by ToolName (tool names are unique per
// agent). The rewritten manifest is re-Normalized, so its content-addressed version changes
// — the SDK sees the new endpoints. Returns the manifest unchanged + nil routes when no
// remote OBO tool is present.
func RewriteRemoteForEgress(m Manifest, bindings []Binding, sidecarBaseURL string) (Manifest, []Route) {
	routes := EgressRoutes(bindings)
	if len(routes) == 0 {
		return m, nil
	}

	toServer := make(map[string]string, len(bindings)) // ToolName → ServerName (remote OBO only)
	for _, b := range bindings {
		if b.Mode == ModeRemote && strings.TrimSpace(b.ServerName) != "" {
			toServer[b.ToolName] = b.ServerName
		}
	}

	base := strings.TrimRight(sidecarBaseURL, "/")
	rewritten := make([]Tool, len(m.Tools))
	for i, t := range m.Tools {
		rewritten[i] = t
		if serverName, ok := toServer[t.Name]; ok && t.Mode == ModeRemote {
			rewritten[i].Endpoint = base + "/" + serverName
		}
	}
	return Normalize(Manifest{Tools: rewritten}), routes
}
