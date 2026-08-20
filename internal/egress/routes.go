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

package egress

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

// ServerRoute is one remote MCP server the egress sidecar fronts: the grant server NAME
// (the credential-resolution key — matches the ToolRegistry / grant label), the real
// upstream URL the sidecar forwards to, and whether the server uses OAuth (so an absent
// grant is consent-required rather than an open server). The agent never sees TargetURL —
// its manifest points the tool at the sidecar; only the sidecar holds the real URL.
type ServerRoute struct {
	Name      string `json:"name"`
	TargetURL string `json:"targetURL"`
	OAuth     bool   `json:"oauth"`

	// target is the parsed TargetURL, validated at load time.
	target *url.URL
}

// Target returns the parsed, validated upstream URL for this route.
func (r ServerRoute) Target() *url.URL { return r.target }

// RouteTable maps a server name (the first path segment the manifest points a tool's
// endpoint at — http://127.0.0.1:PORT/<name>) to its ServerRoute. The controller renders
// it from the agent's MCPToolBindings (m25.8); the sidecar loads it at start-up.
type RouteTable map[string]ServerRoute

// RouteHolder guards the current route table behind an RWMutex so the sidecar's fsnotify watcher can
// atomically swap it while request goroutines read it (J7 — mirrors PolicyHolder / the launcher's
// guardrail holder). Delivering the routes as a WATCHED, mounted ConfigMap (vs a static pod-spec env)
// is what lets a remote-tool-URL edit take effect on the RUNNING sidecar WITHOUT a revision roll: the
// edit updates the ConfigMap content the sidecar reloads, not the pod template. The zero value is an
// empty holder; Load on a nil receiver returns nil so a caller can always read. It remembers the raw
// JSON so a byte-identical (spurious) fsnotify event skips the reparse.
type RouteHolder struct {
	mu      sync.RWMutex
	current RouteTable
	rawJSON string
}

// Store swaps in a new route table under the write lock (the reload path). raw is the JSON it was
// built from (for the unchanged-content skip).
func (h *RouteHolder) Store(t RouteTable, raw string) {
	h.mu.Lock()
	h.current = t
	h.rawJSON = raw
	h.mu.Unlock()
}

// Load returns the current route table under the read lock. Safe on a nil receiver (an unwired
// holder ⇒ nil), so the proxy can always read.
func (h *RouteHolder) Load() RouteTable {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	t := h.current
	h.mu.RUnlock()
	return t
}

// RawEquals reports whether the held table was built from byte-identical raw JSON (the skip-reparse
// fast path on a spurious fsnotify event).
func (h *RouteHolder) RawEquals(raw string) bool {
	if h == nil {
		return false
	}
	h.mu.RLock()
	eq := h.rawJSON == raw
	h.mu.RUnlock()
	return eq
}

// ParseRouteTable parses the JSON route config (a list of ServerRoute) into a keyed table,
// validating each entry has a name and an absolute upstream URL. A route with a relative
// or unparseable URL is a hard config error (never a silently-broken forward).
func ParseRouteTable(raw []byte) (RouteTable, error) {
	var list []ServerRoute
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("egress: parse route table: %w", err)
	}
	table := make(RouteTable, len(list))
	for i := range list {
		route := list[i]
		name := strings.TrimSpace(route.Name)
		if name == "" {
			return nil, fmt.Errorf("egress: route %d has no name", i)
		}
		u, err := url.Parse(strings.TrimSpace(route.TargetURL))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("egress: route %q targetURL must be an absolute URL, got %q", name, route.TargetURL)
		}
		route.Name = name
		route.target = u
		table[name] = route
	}
	return table, nil
}

// routeForPath returns the ServerRoute selected by the first path segment, the remainder of
// the path after that segment (forwarded to the upstream), and whether a route matched.
// e.g. "/scalekit-mcp-server/foo" → (route, "/foo", true); "/scalekit-mcp-server" → (route, "", true).
func (t RouteTable) routeForPath(path string) (ServerRoute, string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	name := trimmed
	remainder := ""
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		name = trimmed[:i]
		remainder = trimmed[i:] // keeps the leading slash
	}
	route, ok := t[name]
	return route, remainder, ok
}
