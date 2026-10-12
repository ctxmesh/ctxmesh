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

// Tool discovery (:2999): the agent's bound MCP tools, served from the <agent>-tools ConfigMap the
// controller mounts into this container and names in TOOLS_JSON_PATH.
//
//	GET /tools   the current manifest (toolmanifest.Normalize'd JSON)
//	GET /events  SSE, one "data: <version>" event per manifest change
//	GET /healthz 200 ok
//
// The file is watched, so a binding change reaches a running agent when the kubelet projects the
// updated ConfigMap (its sync period, about a minute by default) without a restart. A file that cannot
// be read or parsed never replaces the manifest being served: an agent keeps its last good tool list.
// An empty tool list is valid, because the agent's last binding can be removed.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/ctxmesh/ctxmesh/internal/toolmanifest"
)

const (
	// discoveryPort is the fixed tool-discovery port every SDK reads (no env names it).
	discoveryPort = 2999
	// envToolsJSONPath is set by the controller when the agent has a tool binding.
	envToolsJSONPath = "TOOLS_JSON_PATH"
	// discoverySubscriberBuffer bounds what a slow /events reader can hold back. A reader whose buffer
	// is full misses that event and gets the next; GET /tools always has the current manifest.
	discoverySubscriberBuffer = 8
)

// toolDiscovery holds the manifest being served and the /events subscribers.
type toolDiscovery struct {
	path string
	logf func(format string, args ...any)

	mu          sync.RWMutex
	manifest    toolmanifest.Manifest
	subscribers map[chan string]struct{}

	done     chan struct{}
	doneOnce sync.Once
}

func newToolDiscovery(path string, logf func(string, ...any)) *toolDiscovery {
	return &toolDiscovery{
		path:        path,
		logf:        logf,
		manifest:    toolmanifest.Normalize(toolmanifest.Manifest{}),
		subscribers: make(map[chan string]struct{}),
		done:        make(chan struct{}),
	}
}

// reload reads the manifest file and, when its content changed, serves it and tells /events readers.
// A missing, unreadable or malformed file keeps the manifest being served.
func (d *toolDiscovery) reload() {
	data, err := os.ReadFile(d.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			d.logf("launcher: discovery: reading %q failed, keeping the current tools: %v", d.path, err)
		}
		return
	}
	var m toolmanifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		d.logf("launcher: discovery: %q is not a tool manifest, keeping the current tools: %v", d.path, err)
		return
	}
	next := toolmanifest.Normalize(m)

	d.mu.Lock()
	if next.Version == d.manifest.Version {
		d.mu.Unlock()
		return
	}
	d.manifest = next
	subs := make([]chan string, 0, len(d.subscribers))
	for ch := range d.subscribers {
		subs = append(subs, ch)
	}
	d.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- next.Version:
		default:
		}
	}
	d.logf("launcher: discovery: serving tool manifest %s (%d tools)", next.Version, len(next.Tools))
}

func (d *toolDiscovery) current() toolmanifest.Manifest {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.manifest
}

func (d *toolDiscovery) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(d.current()); err != nil {
			d.logf("launcher: discovery: writing /tools: %v", err)
		}
	})
	mux.HandleFunc("GET /events", d.handleEvents)
	return mux
}

// handleEvents streams one SSE event per manifest change until the reader leaves or the launcher
// stops. Subscriber channels are never closed, so a reload that took a snapshot of them cannot send
// on a closed channel.
func (d *toolDiscovery) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	ch := make(chan string, discoverySubscriberBuffer)
	d.mu.Lock()
	d.subscribers[ch] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.subscribers, ch)
		d.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-d.done:
			return
		case version := <-ch:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", version); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// stop ends every /events stream so a graceful shutdown does not wait on them. Idempotent.
func (d *toolDiscovery) stop() { d.doneOnce.Do(func() { close(d.done) }) }

// watch re-reads the manifest on any change in its directory until stop is closed. It watches the
// directory, not the file: a ConfigMap volume updates by swapping its ..data symlink, after which a
// watch on the old file would see nothing. Without a watcher the manifest stays as loaded at start.
func (d *toolDiscovery) watch(stop <-chan struct{}) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		d.logf("launcher: discovery: watch disabled, tools are fixed at startup: %v", err)
		return
	}
	defer func() { _ = w.Close() }()
	dir := filepath.Dir(d.path)
	if err := w.Add(dir); err != nil {
		d.logf("launcher: discovery: watch of %q disabled, tools are fixed at startup: %v", dir, err)
		return
	}
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				d.reload()
			}
		case werr, ok := <-w.Errors:
			if !ok {
				return
			}
			d.logf("launcher: discovery: watch error, keeping the current tools: %v", werr)
		}
	}
}

// buildDiscoveryServer returns the :2999 listener and its manifest holder when the agent has tools
// (TOOLS_JSON_PATH set), else nils. It binds loopback: only the agent in this pod reads it.
func buildDiscoveryServer(lookup func(string) string, logf func(string, ...any)) (*http.Server, *toolDiscovery) {
	path := lookup(envToolsJSONPath)
	if path == "" {
		return nil, nil
	}
	d := newToolDiscovery(path, logf)
	d.reload()
	srv := &http.Server{
		Addr:              loopbackAddr(discoveryPort),
		Handler:           d.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	srv.RegisterOnShutdown(d.stop)
	return srv, d
}
