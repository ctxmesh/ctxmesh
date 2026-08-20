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

// Runtime-reloadable tool policy (M82, ADR 0074 §1) — the egress-sidecar side of the M81-K3
// mounted-ConfigMap + fsnotify pattern (mirrors the launcher's guardrail_reload.go). The controller
// delivers the resolved spec.runtime.toolPolicy as a mounted, read-only ConfigMap file
// (TOOL_POLICY_FILE); this sidecar reads it at startup AND watches it, so a policy edit — which
// updates the ConfigMap in place via the controller's reconcile — propagates to the RUNNING sidecar
// WITHOUT a revision roll.
//
// The policy is read, parsed, and held behind the PolicyHolder's RWMutex (atomic swap on reload). The
// proxy ENFORCES it (M82, ADR 0074): deny → 403, require-approval → the voucher protocol, plus the
// fan-out ceiling — so an invalid policy is NOT harmless.
//
// STARTUP is FAIL-CLOSED (C16, ADR 0087): a MALFORMED initial policy is a HARD startup error
// (loadInitialToolPolicy returns it → the sidecar exits) — since the sidecar enforces, starting
// permissive on a broken policy would silently disable the governance the operator configured. A
// legitimately ABSENT/EMPTY policy is permissive (a nil policy — the agent set no toolPolicy).
//
// RELOAD is FAIL-CLOSED, KEEP-LAST-GOOD (the sacred invariant, mirrored from the guardrail reload): a
// malformed/unparseable NEW policy — or a transient read error — NEVER drops the active policy or
// crashes. The watcher logs loudly and KEEPS the last-good policy. The only content-driven transition
// to nil is an explicitly EMPTY file (the operator cleared the policy).

import (
	"errors"
	"fmt"
	"os"

	"github.com/go-logr/logr"

	"github.com/ctxmesh/agent-engine/internal/egress"
)

// readToolPolicyFile reads the mounted policy file. A missing file OR an empty/whitespace file
// yields "" (no active policy — permissive, byte-compatible with an agent that set no toolPolicy),
// which ParseToolPolicy turns into a nil policy. Any OTHER read error (a transient I/O failure) is
// returned so the RELOAD path can KEEP the last-good policy rather than drop it.
func readToolPolicyFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil // no file mounted ⇒ no policy (permissive)
		}
		return "", err
	}
	return string(b), nil
}

// loadInitialToolPolicy reads + parses the mounted policy at startup and stores it in the holder.
// FAIL-CLOSED (C16, ADR 0087): the sidecar ENFORCES the policy (M82), so a MALFORMED initial policy is
// a HARD startup error — starting permissive on a broken policy would silently disable the governance
// the operator configured. A legitimately ABSENT/EMPTY policy is NOT an error (readToolPolicyFile maps
// a missing/empty file → ""; ParseToolPolicy("") → a nil policy = permissive, the un-governed agent). A
// transient READ error (mount not ready) is ALSO a hard error — we cannot confirm the intended policy,
// so we refuse to start un-governed. Mirrors loadInitialRoutes (J7) + the guardrail engine's load.
func loadInitialToolPolicy(holder *egress.PolicyHolder, path string, log logr.Logger) error {
	raw, err := readToolPolicyFile(path)
	if err != nil {
		return fmt.Errorf("reading tool policy %q: %w", path, err)
	}
	policy, err := egress.ParseToolPolicy(raw)
	if err != nil {
		return fmt.Errorf("parsing tool policy %q (a malformed policy must not start permissive — the "+
			"sidecar enforces it): %w", path, err)
	}
	holder.Store(policy, raw)
	log.Info("egress: tool policy loaded", "path", path, "present", policy != nil)
	return nil
}

// reloadToolPolicy re-reads the mounted file on a watch event and, on a genuine content change,
// parses a NEW policy and atomically swaps it in.
//
// FAIL-CLOSED, KEEP-LAST-GOOD:
//   - a transient read error → KEEP last-good (log loudly), never drop the policy;
//   - a malformed new policy → KEEP last-good (log loudly), never crash;
//   - byte-identical content → no-op (skip the reparse);
//   - an explicitly empty file → a nil policy (the operator cleared it — the only content-driven
//     path to nil, permissive).
func reloadToolPolicy(holder *egress.PolicyHolder, path string, log logr.Logger) {
	raw, err := readToolPolicyFile(path)
	if err != nil {
		log.Info("egress: tool policy reload: read failed (KEEPING last-good policy)", "path", path, "err", err.Error())
		return
	}
	if holder.RawEquals(raw) {
		return // unchanged content (or a spurious event) — nothing to swap
	}
	policy, err := egress.ParseToolPolicy(raw)
	if err != nil {
		log.Info("egress: tool policy reload: new policy is INVALID (KEEPING last-good policy)",
			"path", path, "err", err.Error())
		return
	}
	holder.Store(policy, raw)
	log.Info("egress: tool policy reloaded (no restart)", "path", path, "present", policy != nil)
}

// watchToolPolicy runs an fsnotify watch on the mounted policy file's DIRECTORY and triggers a
// reload on any change. It watches the directory, not the file, because a ConfigMap-projected mount
// updates the file via an atomic symlink swap of the "..data" directory — a file-level watch would
// go deaf after the first update. It blocks until stop is closed. A watcher-construction/add error
// is logged and the function returns — the initial policy stays active, just not reloadable (a
// visible degradation, never a crash).
func watchToolPolicy(holder *egress.PolicyHolder, path string, log logr.Logger, stop <-chan struct{}) {
	watchFileReload(path, log, stop, "tool policy", func() { reloadToolPolicy(holder, path, log) })
}
