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
//
// This is the OBO-only route table (ADR 0030): only remote OBO servers are fronted. For the
// M78 record-mode variant — front EVERY tool (remote OBO, plain remote, AND in-pod sidecar
// tools) so all tool I/O passes through the capture seam — use RewriteAllForEgress, which
// derives its route table from the rendered manifest's per-tool endpoints.
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
	return sortedRoutes(routes)
}

// sortedRoutes flattens a name→Route map into a name-sorted slice (deterministic EGRESS_ROUTES
// + egress digest regardless of binding input order).
func sortedRoutes(routes map[string]Route) []Route {
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

// RewriteAllForEgress is the RECORD-mode manifest rewrite (M78, ADR 0071 §1/C1): it redirects
// EVERY tool's endpoint through the per-pod egress sidecar so all tool I/O flows through the
// capture seam — not just remote OBO tools (RewriteRemoteForEgress). Each tool's real endpoint
// (the localhost URL of an in-pod sidecar tool, or the verbatim remote URL) is moved into the
// returned route table and its manifest endpoint becomes sidecarBaseURL/<routeSegment>:
//
//   - a remote OBO tool → segment = ServerName, OAuth preserved so the sidecar still injects
//     the invoking user's OBO credential (deduped: many tools may share one OBO server);
//   - any other tool     → segment = ToolName, OAuth=false so the sidecar forwards with NO
//     credential injection (the ErrNoCredential open-server path, egress/proxy.go) — the call
//     now hops agent → sidecar → real endpoint, so its I/O is captured.
//
// Routes are derived from the RENDERED manifest so each tool's real endpoint (including the
// port-assigned localhost URL of a sidecar tool) is taken verbatim from Render's output — no
// re-derivation of ports. The rewritten manifest is re-Normalized so its content-addressed
// version reflects the new endpoints. Returns the manifest unchanged + nil routes when the
// manifest has no tools.
func RewriteAllForEgress(m Manifest, bindings []Binding, sidecarBaseURL string) (Manifest, []Route) {
	if len(m.Tools) == 0 {
		return m, nil
	}

	// ToolName → the remote OBO binding it belongs to (ServerName + OAuth), for the tools that
	// keep OBO credential injection. A tool absent from this map is a non-OBO tool.
	oboByTool := make(map[string]Binding, len(bindings))
	for _, b := range bindings {
		if b.Mode == ModeRemote && strings.TrimSpace(b.ServerName) != "" {
			oboByTool[b.ToolName] = b
		}
	}

	base := strings.TrimRight(sidecarBaseURL, "/")
	routes := make(map[string]Route)
	rewritten := make([]Tool, len(m.Tools))
	for i, t := range m.Tools {
		rewritten[i] = t
		realEndpoint := strings.TrimSpace(t.Endpoint)
		if realEndpoint == "" {
			// A tool with no endpoint cannot be fronted — leave it verbatim (defensive; Render
			// always assigns an endpoint).
			continue
		}
		if ob, ok := oboByTool[t.Name]; ok {
			// Remote OBO tool: route under the ServerName (shared grant key), preserve OAuth.
			routes[ob.ServerName] = Route{Name: ob.ServerName, TargetURL: ob.URL, OAuth: ob.OAuth}
			rewritten[i].Endpoint = base + "/" + ob.ServerName
			continue
		}
		// Any other tool (a plain remote tool or an in-pod sidecar tool): route under the unique
		// ToolName to its real endpoint, OAuth=false ⇒ forwarded with no credential injection.
		routes[t.Name] = Route{Name: t.Name, TargetURL: realEndpoint, OAuth: false}
		rewritten[i].Endpoint = base + "/" + t.Name
	}

	if len(routes) == 0 {
		return m, nil
	}
	return Normalize(Manifest{Tools: rewritten}), sortedRoutes(routes)
}
