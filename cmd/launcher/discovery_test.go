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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/toolmanifest"
)

// configMapDir lays out a directory the way the kubelet projects a ConfigMap volume: the key is a
// symlink through ..data, which points at a timestamped directory. update follows the kubelet's atomic
// writer: write a new timestamped directory, point ..data_tmp at it, rename that over ..data, then
// remove the old directory. The key file's inode changes under any watcher.
type configMapDir struct {
	t    *testing.T
	dir  string
	gen  int
	prev string
}

func newConfigMapDir(t *testing.T, content string) *configMapDir {
	t.Helper()
	c := &configMapDir{t: t, dir: t.TempDir()}
	c.update(content)
	require.NoError(t, os.Symlink(filepath.Join("..data", "tools.json"), filepath.Join(c.dir, "tools.json")))
	return c
}

func (c *configMapDir) path() string { return filepath.Join(c.dir, "tools.json") }

func (c *configMapDir) update(content string) {
	c.t.Helper()
	c.gen++
	ts := fmt.Sprintf("..2026_10_13_%03d", c.gen)
	require.NoError(c.t, os.Mkdir(filepath.Join(c.dir, ts), 0o755))
	require.NoError(c.t, os.WriteFile(filepath.Join(c.dir, ts, "tools.json"), []byte(content), 0o644))
	tmp := filepath.Join(c.dir, "..data_tmp")
	require.NoError(c.t, os.Symlink(ts, tmp))
	require.NoError(c.t, os.Rename(tmp, filepath.Join(c.dir, "..data")))
	if c.prev != "" {
		require.NoError(c.t, os.RemoveAll(filepath.Join(c.dir, c.prev)))
	}
	c.prev = ts
}

func manifestJSON(t *testing.T, names ...string) string {
	t.Helper()
	m := toolmanifest.Manifest{Tools: []toolmanifest.Tool{}}
	for _, n := range names {
		m.Tools = append(m.Tools, toolmanifest.Tool{
			Name: n, Mode: "remote", Endpoint: "http://127.0.0.1:8899/" + n, Transport: "streamable-http",
		})
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return string(b)
}

func discard(string, ...any) {}

// serveDiscovery builds the launcher's discovery listener for the manifest at path and serves it on an
// ephemeral loopback port instead of :2999, returning its base URL.
func serveDiscovery(t *testing.T, path string) (string, *http.Server, *toolDiscovery) {
	t.Helper()
	srv, d := buildDiscoveryServer(func(k string) string {
		if k == envToolsJSONPath {
			return path
		}
		return ""
	}, discard)
	require.NotNil(t, srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String(), srv, d
}

func getTools(t *testing.T, base string) toolmanifest.Manifest {
	t.Helper()
	resp, err := http.Get(base + "/tools")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var m toolmanifest.Manifest
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&m))
	return m
}

func toolNames(m toolmanifest.Manifest) []string {
	out := make([]string, 0, len(m.Tools))
	for _, tl := range m.Tools {
		out = append(out, tl.Name)
	}
	return out
}

// openEvents subscribes to /events and returns a channel of the versions it announces.
func openEvents(t *testing.T, base string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	out := make(chan string, 8)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				out <- v
			}
		}
	}()
	return out
}

func TestDiscoveryIsOffWithoutTools(t *testing.T) {
	srv, d := buildDiscoveryServer(func(string) string { return "" }, discard)
	assert.Nil(t, srv, "an agent with no tool binding gets no :2999 listener")
	assert.Nil(t, d)
}

func TestDiscoveryServesTheMountedManifestOnLoopback(t *testing.T) {
	cm := newConfigMapDir(t, manifestJSON(t, "weather", "search"))
	base, srv, _ := serveDiscovery(t, cm.path())
	assert.Equal(t, "127.0.0.1:2999", srv.Addr, "only the agent in this pod reads its tools")

	m := getTools(t, base)
	assert.ElementsMatch(t, []string{"weather", "search"}, toolNames(m))
	want := toolmanifest.Normalize(m)
	assert.Equal(t, want.Version, m.Version, "the version is computed from the tools, not taken from the file")

	resp, err := http.Get(base + "/healthz")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// A binding change rewrites the <agent>-tools ConfigMap; the kubelet projects it into the running pod by
// swapping ..data. The launcher must pick that up and tell /events readers, with no restart and no push.
func TestDiscoveryFollowsAConfigMapUpdate(t *testing.T) {
	cm := newConfigMapDir(t, manifestJSON(t, "weather"))
	base, _, d := serveDiscovery(t, cm.path())
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go d.watch(stop)
	events := openEvents(t, base)
	before := getTools(t, base).Version

	// The watch is registered asynchronously; keep updating until an event arrives, as the kubelet would
	// on its next sync.
	var got string
	deadline := time.After(10 * time.Second)
	for got == "" {
		cm.update(manifestJSON(t, "weather", "calendar"))
		select {
		case got = <-events:
		case <-time.After(200 * time.Millisecond):
		case <-deadline:
			t.Fatal("no /events announcement after the ConfigMap update")
		}
	}
	m := getTools(t, base)
	assert.NotEqual(t, before, m.Version)
	assert.Equal(t, m.Version, got, "the event names the version /tools now serves")
	assert.ElementsMatch(t, []string{"weather", "calendar"}, toolNames(m))

	// Every later update is seen too: a watch that followed the first file it was given would go deaf
	// once the kubelet removed that file's directory.
	for _, names := range [][]string{{"calendar"}, {"calendar", "search"}, {}} {
		cm.update(manifestJSON(t, names...))
		require.Eventually(t, func() bool {
			return assert.ObjectsAreEqual(names, sortedNames(getTools(t, base)))
		}, 10*time.Second, 50*time.Millisecond, "update to %v not served", names)
	}
}

func sortedNames(m toolmanifest.Manifest) []string {
	names := toolNames(m)
	slices.Sort(names)
	return names
}

func TestDiscoveryKeepsTheLastGoodManifest(t *testing.T) {
	cm := newConfigMapDir(t, manifestJSON(t, "weather"))
	d := newToolDiscovery(cm.path(), discard)
	d.reload()
	good := d.current()

	cm.update("{not json")
	d.reload()
	assert.Equal(t, good, d.current(), "a malformed file never replaces the tools being served")

	require.NoError(t, os.Remove(filepath.Join(cm.dir, "..data")))
	d.reload()
	assert.Equal(t, good, d.current(), "a file missing mid-swap never empties the tools")

	cold := newToolDiscovery(filepath.Join(t.TempDir(), "absent", "tools.json"), discard)
	cold.reload()
	assert.Empty(t, cold.current().Tools, "before the ConfigMap exists the agent sees no tools, not an error")
	assert.Equal(t, toolmanifest.Normalize(toolmanifest.Manifest{}).Version, cold.current().Version)
}

// A graceful shutdown must not wait out a reader holding /events open.
func TestDiscoveryShutdownEndsEventStreams(t *testing.T) {
	cm := newConfigMapDir(t, manifestJSON(t, "weather"))
	base, srv, _ := serveDiscovery(t, cm.path())
	_ = openEvents(t, base)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := srv.Shutdown(ctx)
	assert.False(t, errors.Is(err, context.DeadlineExceeded), "shutdown waited on an open /events stream")
}
