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

package bff

// recipes_overlay.go — the OPERATOR recipe overlay (S1, ADR 0066 D4 follow-up). A cluster operator adds
// starter recipes via a ConfigMap mounted as a read-only volume (NOT a k8s-client read → no RBAC change,
// ADR 0011; mirrors the K3 guardrail_reload.go mounted-volume + fsnotify hot-reload pattern). The overlay
// is merged OVER the Go-embedded defaults in GET /api/recipes.
//
// Design (Fable-gated, M114 — see ADR 0092... no: recorded on the m114 board + acceptance report):
//   - Hot-reload via fsnotify on the mount DIRECTORY (a ConfigMap projection swaps the "..data" symlink,
//     so a file-level watch goes deaf — watch the dir). The marginal cost is near zero (the K3 shape).
//   - Merge: an operator recipe OVERRIDES an embedded default with the same `name`, IN PLACE (stable gallery
//     position); net-new operator recipes are appended. Standard drop-in (conf.d / helm-values) semantics.
//   - FAIL-CLOSED to the EMBEDDED defaults — and DELIBERATELY NOT K3's keep-last-good: the safe state for
//     recipes is the embedded gallery, not stale operator data. Each reload REBUILDS from what is valid on
//     disk NOW: a valid file serves, an invalid file (bad YAML / missing name+title / spec fails
//     expand.Expand) is SKIPPED with a loud per-file log, an absent/empty dir means pure embedded. (A
//     transient read error mid-swap keeps the current overlay — the only keep-last-good we retain.)

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"

	"github.com/ctxmesh/agentry/internal/expand"
)

// recipeOverlayHolder guards the current operator overlay behind an RWMutex so the fsnotify watcher can
// atomically swap it while request goroutines read it (the guardrailHolder shape). The zero value is a
// valid empty holder (nil overlay ⇒ embedded-only).
type recipeOverlayHolder struct {
	mu      sync.RWMutex
	current []recipeFile
}

func (h *recipeOverlayHolder) store(r []recipeFile) {
	h.mu.Lock()
	h.current = r
	h.mu.Unlock()
}

func (h *recipeOverlayHolder) load() []recipeFile {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.current
}

// loadRecipeOverlay reads + validates the operator recipes currently on disk in dir, rebuilding the whole
// overlay from scratch (NOT keep-last-good — the safe fallback is the embedded gallery). An absent/empty/
// unreadable dir yields nil (embedded-only). Each *.yaml is parsed, requires a non-empty name+title, and
// must pass expand.Expand (the same gate the embedded recipes get at test time) — a failure SKIPS just
// that file with a log, never the whole overlay. Duplicate names within the overlay resolve last-wins in
// lexical filename order (fs.ReadDir is sorted). It also logs any overlay name that shadows an embedded
// default (diagnosability). Returns the overlay slice in stable (lexical) order.
func (s *Server) loadRecipeOverlay(dir string) []recipeFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log.Info("recipe overlay: directory unreadable — serving embedded recipes only", "dir", dir, "err", err.Error())
		}
		return nil
	}
	byName := make(map[string]recipeFile)
	order := make([]string, 0, len(entries)) // first-seen (lexical) name order for a stable gallery
	for _, e := range entries {
		// glob *.yaml so the ConfigMap projection's "..data"/"..2026_*" symlink machinery is skipped.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			s.log.Info("recipe overlay: skipping unreadable file", "file", e.Name(), "err", rerr.Error())
			continue
		}
		var rf recipeFile
		if uerr := yaml.Unmarshal(data, &rf); uerr != nil {
			s.log.Info("recipe overlay: skipping malformed YAML", "file", e.Name(), "err", uerr.Error())
			continue
		}
		if strings.TrimSpace(rf.Name) == "" || strings.TrimSpace(rf.Title) == "" {
			s.log.Info("recipe overlay: skipping (missing name or title)", "file", e.Name())
			continue
		}
		if _, xerr := expand.Expand([]byte(rf.Spec)); xerr != nil {
			s.log.Info("recipe overlay: skipping (spec fails validation)", "file", e.Name(), "recipe", rf.Name, "err", xerr.Error())
			continue
		}
		if _, dup := byName[rf.Name]; dup {
			s.log.Info("recipe overlay: duplicate recipe name within the overlay — later file wins", "name", rf.Name, "file", e.Name())
		} else {
			order = append(order, rf.Name)
		}
		byName[rf.Name] = rf // last-wins
	}
	if len(order) == 0 {
		return nil
	}
	// Log shadows of embedded defaults (best-effort; loadRecipes is sync.Once-cached).
	if embedded, eerr := loadRecipes(); eerr == nil {
		embeddedNames := make(map[string]struct{}, len(embedded))
		for _, r := range embedded {
			embeddedNames[r.Name] = struct{}{}
		}
		for _, n := range order {
			if _, ok := embeddedNames[n]; ok {
				s.log.Info("recipe overlay: operator recipe shadows an embedded default", "name", n)
			}
		}
	}
	out := make([]recipeFile, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

// mergeRecipes overlays operator recipes over the embedded defaults: an operator recipe with the same
// `name` REPLACES the embedded one IN PLACE (a customized default keeps its gallery position); net-new
// operator recipes are appended in overlay order. A pure function (no logging — shadows are logged at
// load time, once per reload, not per request).
func mergeRecipes(embedded, overlay []recipeFile) []recipeFile {
	ov := make(map[string]recipeFile, len(overlay))
	for _, o := range overlay {
		ov[o.Name] = o
	}
	out := make([]recipeFile, 0, len(embedded)+len(overlay))
	used := make(map[string]bool, len(overlay))
	for _, e := range embedded {
		if o, ok := ov[e.Name]; ok {
			out = append(out, o) // operator overrides in place
			used[e.Name] = true
		} else {
			out = append(out, e)
		}
	}
	for _, o := range overlay {
		if !used[o.Name] {
			out = append(out, o) // net-new operator recipe
		}
	}
	return out
}

// StartRecipeOverlayWatcher loads the operator overlay from dir and hot-reloads it on any change, until
// stop is closed (the BFF lifecycle owns it). It does the initial load synchronously (so the first
// request already sees the overlay), then watches the mount DIRECTORY. A watcher-init/add failure logs
// and returns — the overlay stays at its initial value, just not reloadable (a visible degradation, never
// a crash; the worst case is exactly read-once). Call it in a goroutine when RECIPES_OVERLAY_DIR is set.
func (s *Server) StartRecipeOverlayWatcher(dir string, stop <-chan struct{}) {
	if s.recipeOverlay == nil {
		s.recipeOverlay = &recipeOverlayHolder{}
	}
	s.recipeOverlay.store(s.loadRecipeOverlay(dir))

	w, err := fsnotify.NewWatcher()
	if err != nil {
		s.log.Info("recipe overlay watch disabled (fsnotify init failed) — overlay fixed at startup", "err", err.Error())
		return
	}
	defer func() { _ = w.Close() }()
	if err := w.Add(dir); err != nil {
		s.log.Info("recipe overlay watch disabled (cannot watch dir) — overlay fixed at startup", "dir", dir, "err", err.Error())
		return
	}
	s.log.Info("recipe overlay: watching for operator recipe changes (hot-reload)", "dir", dir)
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			// A ConfigMap update rewrites the "..data" symlink (Create/Rename on the dir). Any relevant
			// event re-reads; loadRecipeOverlay rebuilds from current disk (a spurious event is cheap).
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				s.recipeOverlay.store(s.loadRecipeOverlay(dir))
			}
		case werr, ok := <-w.Errors:
			if !ok {
				return
			}
			s.log.Error(werr, "recipe overlay watch error")
		}
	}
}
