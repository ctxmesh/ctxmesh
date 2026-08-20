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

// Runtime-reloadable egress routes (J7) — the egress-sidecar side of the M81-K3 mounted-ConfigMap +
// fsnotify pattern (mirrors policy_reload.go / the launcher's guardrail_reload.go). The controller
// delivers the route table as a mounted, read-only ConfigMap file (EGRESS_ROUTES_FILE); this sidecar
// reads it at startup AND watches it, so a remote-tool-URL edit — which the controller applies by
// UPDATING the ConfigMap in place — propagates to the RUNNING sidecar WITHOUT a revision roll. (The
// route URL is excluded from the pod-template digest, so an edit never rolled the revision AND — before
// J7 — never took effect on the running sidecar either, since the static EGRESS_ROUTES env only changes
// with a re-applied pod spec. Now the edit rides the watched ConfigMap and takes effect live.)
//
// KEEP-LAST-GOOD (the sacred invariant, mirrored from policy/guardrail reload): a malformed / empty /
// unreadable NEW route file NEVER drops the active routes or crashes — the watcher logs loudly and
// KEEPS the last-good table. Unlike the tool policy, a route table is REQUIRED (a sidecar with no
// routes fronts nothing), so there is no valid "empty" state: an empty new file keeps last-good.

import (
	"errors"
	"os"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/agent-engine/internal/egress"
)

// loadInitialRoutes reads + parses the mounted routes file at startup into the holder. Returns an
// error when the file is missing / unreadable / invalid so the caller can treat it as a hard startup
// error (a routes-backed sidecar with no routes fronts nothing — the same posture as EGRESS_ROUTES).
func loadInitialRoutes(holder *egress.RouteHolder, path string, log logr.Logger) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	table, err := egress.ParseRouteTable(b)
	if err != nil {
		return err
	}
	holder.Store(table, string(b))
	log.Info("egress: routes loaded from file", "path", path, "routes", len(table))
	return nil
}

// reloadRoutes re-reads the mounted file on a watch event and, on a genuine content change, parses a
// NEW route table and atomically swaps it in. KEEP-LAST-GOOD on any read/parse error and on an empty
// table (a routes table is required, so an empty/invalid file must never blank the live routes).
func reloadRoutes(holder *egress.RouteHolder, path string, log logr.Logger) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return // a transient symlink swap mid-projection; the watch fires again with the file present.
		}
		log.Info("egress: routes reload: read failed (KEEPING last-good routes)", "path", path, "err", err.Error())
		return
	}
	if holder.RawEquals(string(b)) {
		return // unchanged content (or a spurious event) — nothing to swap.
	}
	table, err := egress.ParseRouteTable(b)
	if err != nil {
		log.Info("egress: routes reload: new routes are INVALID (KEEPING last-good routes)", "path", path, "err", err.Error())
		return
	}
	if len(table) == 0 {
		log.Info("egress: routes reload: new routes are EMPTY (KEEPING last-good routes)", "path", path)
		return
	}
	holder.Store(table, string(b))
	log.Info("egress: routes reloaded (no restart)", "path", path, "routes", len(table))
}

// watchRoutes hot-reloads the mounted routes file on change (J7) via the shared watchFileReload loop.
func watchRoutes(holder *egress.RouteHolder, path string, log logr.Logger, stop <-chan struct{}) {
	watchFileReload(path, log, stop, "routes", func() { reloadRoutes(holder, path, log) })
}
