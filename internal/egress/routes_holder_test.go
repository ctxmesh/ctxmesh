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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCurrentRoutes_PrefersHolder proves J7's read path: a wired, non-empty RouteHolder supersedes the
// static Routes (so a hot-reloaded URL takes effect), an empty holder falls back to static (a routes
// table is always required), and a nil holder uses static (legacy / tests unchanged).
func TestCurrentRoutes_PrefersHolder(t *testing.T) {
	static, err := ParseRouteTable([]byte(`[{"name":"s","targetURL":"https://static.example","oauth":false}]`))
	require.NoError(t, err)

	// No holder wired → static (byte-for-byte legacy behaviour).
	p := &Proxy{cfg: ProxyConfig{Routes: static}}
	assert.Equal(t, "https://static.example", p.currentRoutes()["s"].TargetURL)

	// Empty holder → falls back to static (never serve an empty table).
	holder := &RouteHolder{}
	p = &Proxy{cfg: ProxyConfig{Routes: static, RoutesHolder: holder}}
	assert.Equal(t, "https://static.example", p.currentRoutes()["s"].TargetURL)

	// Holder with routes → supersedes static (a hot-reloaded edit takes effect).
	hot, err := ParseRouteTable([]byte(`[{"name":"s","targetURL":"https://hot.example","oauth":false}]`))
	require.NoError(t, err)
	holder.Store(hot, "raw")
	assert.Equal(t, "https://hot.example", p.currentRoutes()["s"].TargetURL,
		"a non-empty holder supersedes the static route table")
}
