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

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/egress"
)

func writeRoutesFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// TestRoutesReload_HotSwapsAndKeepsLastGood proves J7: an in-place edit of the mounted routes file is
// hot-reloaded into the holder (the new upstream URL takes effect without a restart), while an
// invalid/empty edit KEEPS the last-good table (never blanks the live routes).
func TestRoutesReload_HotSwapsAndKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.json")
	holder := &egress.RouteHolder{}

	// Initial load.
	writeRoutesFile(t, path, `[{"name":"scalekit","targetURL":"https://v1.example/mcp","oauth":true}]`)
	require.NoError(t, loadInitialRoutes(holder, path, logr.Discard()))
	require.Equal(t, "https://v1.example/mcp", holder.Load()["scalekit"].TargetURL)

	// A genuine URL edit → hot-swapped.
	writeRoutesFile(t, path, `[{"name":"scalekit","targetURL":"https://v2.example/mcp","oauth":true}]`)
	reloadRoutes(holder, path, logr.Discard())
	assert.Equal(t, "https://v2.example/mcp", holder.Load()["scalekit"].TargetURL,
		"a routes-file edit must take effect on the running sidecar (no restart)")

	// An INVALID edit → keep last-good (never blank the routes).
	writeRoutesFile(t, path, `not json`)
	reloadRoutes(holder, path, logr.Discard())
	assert.Equal(t, "https://v2.example/mcp", holder.Load()["scalekit"].TargetURL,
		"an invalid routes edit must KEEP the last-good table")

	// An EMPTY edit → keep last-good (a routes table is required).
	writeRoutesFile(t, path, `[]`)
	reloadRoutes(holder, path, logr.Discard())
	assert.Equal(t, "https://v2.example/mcp", holder.Load()["scalekit"].TargetURL,
		"an empty routes edit KEEPS last-good")
}

// TestLoadInitialRoutes_InvalidIsHardError proves a missing/invalid routes file at startup is a hard
// error (a routes-backed sidecar with no routes fronts nothing) — the caller aborts, never fails open.
func TestLoadInitialRoutes_InvalidIsHardError(t *testing.T) {
	holder := &egress.RouteHolder{}
	require.Error(t, loadInitialRoutes(holder, filepath.Join(t.TempDir(), "absent.json"), logr.Discard()),
		"a missing routes file at startup is a hard error")

	path := filepath.Join(t.TempDir(), "bad.json")
	writeRoutesFile(t, path, `{oops`)
	require.Error(t, loadInitialRoutes(holder, path, logr.Discard()), "an unparseable routes file is a hard error")
}
