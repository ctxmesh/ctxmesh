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

// Runtime-reloadable GuardrailPolicy (K3, ADR 0059 Fork-2). M66 delivered the resolved
// policy as the GUARDRAIL_POLICY env folded into the structural digest, so a policy edit
// ROLLED a new Knative revision (a full restart). K3 delivers the policy as a mounted,
// read-only ConfigMap the controller updates in place; the launcher reads it at startup
// AND watches it (fsnotify), so a GuardrailPolicy edit propagates to the running agent
// WITHOUT a revision roll.
//
// The three POLICY-DERIVED pieces — the deterministic guardrail engine, the fenced
// LLM-judge, and the per-user quota — are bundled behind an RWMutex (guardrailHolder).
// The request path reads the current bundle under the read lock; the watcher builds a NEW
// bundle from the rewritten file and swaps it atomically under the write lock. A concurrent
// request always sees one consistent bundle (never a half-swapped mix), and the swap is a
// single pointer store, so it is fast.
//
// FAIL-CLOSED, KEEP-LAST-GOOD (the sacred invariant): a malformed/unparseable new policy —
// or a transient read error — NEVER disables guardrails, drops the active policy, or crashes.
// The watcher logs loudly and KEEPS the last-good bundle. The only content-driven transition
// to a nil engine is an explicitly EMPTY file (mirroring the "empty ⇒ nil engine" load
// semantics: the operator cleared the policy), which is the reverse of the M4 landmine.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ctxmesh/ctxmesh/internal/guardrail"
	"github.com/fsnotify/fsnotify"
)

// guardrailBundle is the immutable set of POLICY-DERIVED enforcement pieces, all built from
// the SAME policy JSON so they always move together. Any field may be nil (no policy / no
// judge / no user rate limit); a nil bundle field is treated exactly as an all-nil bundle.
type guardrailBundle struct {
	// policyJSON is the raw JSON the bundle was built from — kept so the watcher can skip a
	// rebuild when the file content is byte-identical (a spurious fsnotify event, or a kubelet
	// re-write of unchanged data).
	policyJSON string
	engine     *guardrailEngine
	judge      *semanticJudge
	user       *userQuota
	// streamEligible is true iff this policy opted into streaming (spec.streaming.mode=Enabled)
	// AND it is provably stream-safe: every OUTPUT detector is bounded/non-empty/within the window
	// cap AND there is no semanticJudge (K2, ADR 0086). false ⇒ a guarded agent stays buffered-only
	// (stream:true is refused). streamWindow is the rune hold-window W the scanner uses; valid only
	// when streamEligible.
	streamEligible bool
	streamWindow   int
}

// guardrailHolder guards the current guardrailBundle behind an RWMutex so the fsnotify
// watcher can atomically swap it while request goroutines read it. The zero value is a
// valid empty holder (nil bundle ⇒ no policy).
type guardrailHolder struct {
	mu      sync.RWMutex
	current *guardrailBundle
}

// store swaps in a new bundle under the write lock (the reload path). A nil bundle means
// "no active policy".
func (h *guardrailHolder) store(b *guardrailBundle) {
	h.mu.Lock()
	h.current = b
	h.mu.Unlock()
}

// load returns the current bundle under the read lock. It never returns nil — an empty
// holder yields a zero bundle so callers can dereference the fields (all nil) without a
// nil-map/nil-pointer check on the bundle itself.
func (h *guardrailHolder) load() *guardrailBundle {
	h.mu.RLock()
	b := h.current
	h.mu.RUnlock()
	if b == nil {
		return &guardrailBundle{}
	}
	return b
}

// guardrailEngine returns the currently-active deterministic engine (nil ⇒ no policy).
// Read on the request hot path via the holder's read lock.
func (gp *gatewayProxy) guardrailEngine() *guardrailEngine { return gp.policy.load().engine }

// semanticJudge returns the currently-active fenced LLM-judge (nil ⇒ disabled/absent).
func (gp *gatewayProxy) semanticJudge() *semanticJudge { return gp.policy.load().judge }

// userQuota returns the currently-active per-user (OBO) quota (nil ⇒ no userRateLimit).
func (gp *gatewayProxy) userQuota() *userQuota { return gp.policy.load().user }

// buildGuardrailBundle constructs the three policy-derived pieces from the raw policy JSON,
// using the EXACT SAME fail-closed builders the startup path uses (newGuardrailEngine /
// newSemanticJudge / buildUserQuota). An empty policyJSON yields an all-nil bundle (no
// policy). A malformed policy — or a pattern that does not compile — returns an error, which
// the caller (startup or reload) handles per its own fail-closed posture.
func buildGuardrailBundle(policyJSON string, cfg gatewayConfig, logf func(string, ...any)) (*guardrailBundle, error) {
	engine, err := newGuardrailEngine(policyJSON)
	if err != nil {
		return nil, fmt.Errorf("guardrail engine: %w", err)
	}
	judge, err := newSemanticJudge(policyJSON, logf)
	if err != nil {
		return nil, fmt.Errorf("semantic judge: %w", err)
	}
	user, err := buildUserQuota(policyJSON, cfg, logf)
	if err != nil {
		return nil, fmt.Errorf("user quota: %w", err)
	}
	eligible, window := evalStreamEligibility(policyJSON, engine, judge, logf)
	return &guardrailBundle{
		policyJSON: policyJSON, engine: engine, judge: judge, user: user,
		streamEligible: eligible, streamWindow: window,
	}, nil
}

// evalStreamEligibility decides whether this policy may serve STREAMING responses (K2, ADR 0086)
// and the hold-window W the scanner needs. Eligible requires ALL of: the operator opted in
// (spec.streaming.mode=Enabled); an active engine to guard; NO semanticJudge (it needs the whole
// completion); and a stream-SAFE output rule set (analyzeOutputStreamability). Any miss ⇒ not
// eligible (buffered-only) — a fail-safe default, never a silent weakening. A malformed policy
// (which the engine builder already rejected) ⇒ not eligible. Non-eligibility is logged once at
// build time (a visible, one-line operator signal), not on the request hot path.
func evalStreamEligibility(
	policyJSON string, engine *guardrailEngine, judge *semanticJudge, logf func(string, ...any),
) (bool, int) {
	if engine == nil {
		return false, 0 // no policy to guard; the unguarded path handles stream:true itself
	}
	mode, err := parseStreamingMode(policyJSON)
	optedIn := err == nil && strings.EqualFold(mode, streamModeEnabled)

	// The DECISION is the shared one (internal/guardrail) — the SAME code the GuardrailPolicyReconciler
	// reports status.streaming with, so enforcement and the reported status never disagree (M139/K10).
	res := guardrail.DecideStreaming(guardrail.StreamingInput{
		OptedIn:      optedIn,
		JudgePresent: judge != nil,
		OutputRules:  outputRulesOf(engine.output),
	})
	if res.EffectiveMode != guardrail.EffectiveStreaming {
		// Only log the downgrade when the operator ASKED for streaming (opted in) — otherwise it is the
		// silent buffered-only default. The reason names the cause (judge / a non-streamable detector).
		if optedIn {
			logf("launcher: gateway: streaming requested but %s → buffered-only (ADR 0086)", res.Reason)
		}
		return false, 0
	}
	logf("launcher: gateway: streaming ENABLED (span-suppression, hold-window W=%d runes; ADR 0086)", res.Window)
	return true, res.Window
}

// readGuardrailPolicyFile reads the mounted policy file. A missing file OR an empty/whitespace
// file yields "" (no active policy — the operator cleared the ref / no policy mounted), which
// the load path turns into an all-nil bundle. Any OTHER read error (a transient I/O failure) is
// returned so the RELOAD path can KEEP the last-good bundle rather than drop the policy.
func readGuardrailPolicyFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil // no file mounted ⇒ no policy (byte-compatible with the empty-env path)
		}
		return "", err
	}
	return string(b), nil
}

// reloadGuardrailPolicy is invoked by the watcher on a file-change event. It re-reads the
// mounted file and, on a genuine content change, builds a NEW bundle and swaps it in.
//
// FAIL-CLOSED, KEEP-LAST-GOOD:
//   - a transient read error → KEEP last-good (log loudly), never drop the policy;
//   - a malformed / uncompilable new policy → KEEP last-good (log loudly), never disable guardrails,
//     never crash;
//   - byte-identical content → no-op (skip the rebuild);
//   - an explicitly empty file → an all-nil bundle (the operator cleared the policy — the only
//     content-driven path to nil, the reverse of the M4 silent-loss landmine).
func (gp *gatewayProxy) reloadGuardrailPolicy(path string) {
	newJSON, err := readGuardrailPolicyFile(path)
	if err != nil {
		gp.logf("launcher: gateway: guardrail reload: read %q failed (KEEPING last-good policy): %v", path, err)
		return
	}
	if newJSON == gp.policy.load().policyJSON {
		return // unchanged content (or a spurious event) — nothing to swap
	}
	bundle, err := buildGuardrailBundle(newJSON, gp.cfg, gp.logf)
	if err != nil {
		gp.logf("launcher: gateway: guardrail reload: new policy is INVALID (KEEPING last-good policy, "+
			"guardrails still enforce the previous policy): %v", err)
		return
	}
	gp.policy.store(bundle)
	gp.logf("launcher: gateway: guardrail policy reloaded from %q (no restart)", path)
}

// watchGuardrailPolicy runs an fsnotify watch on the mounted policy file's DIRECTORY and
// triggers a reload on any change (K3). It watches the directory, not the file, because a
// ConfigMap-projected mount updates the file via an atomic symlink swap of the "..data"
// directory — the file's own inode is replaced, so a file-level watch would go deaf after the
// first update. A directory watch catches the symlink swap every time (the standard
// mounted-ConfigMap reload pattern).
//
// It blocks until stop is closed (the launcher lifecycle owns it), then closes the watcher.
// A watcher-construction error is logged and the function returns — the initial policy stays
// active, just not reloadable (a visible degradation, never a crash). fsnotify is the
// standard Go file-watch library (already in go.mod as an indirect dep of controller-runtime).
func (gp *gatewayProxy) watchGuardrailPolicy(path string, stop <-chan struct{}) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		gp.logf("launcher: gateway: guardrail watch disabled (fsnotify init: %v) — policy is fixed at startup", err)
		return
	}
	defer func() { _ = w.Close() }()

	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		gp.logf("launcher: gateway: guardrail watch disabled (watch %q: %v) — policy is fixed at startup", dir, err)
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
			// A ConfigMap update rewrites the "..data" symlink (a Create/Rename on the dir).
			// Any event in the dir is a cheap trigger to re-read; reloadGuardrailPolicy skips the
			// rebuild when the content is unchanged, so a spurious event is nearly free.
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				gp.reloadGuardrailPolicy(path)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			gp.logf("launcher: gateway: guardrail watch error (KEEPING last-good policy): %v", err)
		}
	}
}
