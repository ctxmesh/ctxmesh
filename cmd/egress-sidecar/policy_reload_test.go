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

// Tool-policy read + reload (M82, ADR 0074 §1) egress-sidecar tests, mirroring the launcher's
// guardrail_reload_test.go. They prove the DELIVERY plumbing: the sidecar reads + parses the mounted
// policy at startup, a rewritten file swaps the held policy live, a malformed reload KEEPS last-good
// (never drops the policy), and an explicitly emptied file clears it. Behavior is PERMISSIVE — these
// tests assert the HELD policy, not enforcement (there is none yet).

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/egress"
)

// denyPolicy is a small tool-policy JSON (default=deny, one allow override) — the SAME shape the
// controller marshals from spec.runtime.toolPolicy into the mounted file.
func denyPolicy(defaultRule, overrideName, overrideRule string) string {
	return `{"default":"` + defaultRule + `","overrides":[{"name":"` + overrideName + `","rule":"` + overrideRule + `"}]}`
}

// writePolicyFile writes the mounted policy file (simulating a ConfigMap projection/update).
func writePolicyFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "policy.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// TestToolPolicy_InitialLoad_Present proves the sidecar reads + parses the mounted policy at startup
// and holds it (permissive — held, not enforced). RuleFor exercises the parsed rules.
func TestToolPolicy_InitialLoad_Present(t *testing.T) {
	dir := t.TempDir()
	path := writePolicyFile(t, dir, denyPolicy("deny", "web_search", "allow"))

	holder := &egress.PolicyHolder{}
	loadInitialToolPolicy(holder, path, logr.Discard())

	p := holder.Load()
	require.NotNil(t, p, "a mounted policy must be held after the initial load")
	assert.Equal(t, "allow", p.RuleFor("web_search"), "the override rule is parsed")
	assert.Equal(t, "deny", p.RuleFor("shell_exec"), "the default rule applies to un-overridden tools")
}

// TestToolPolicy_AbsentFile_Permissive proves an absent/empty file ⇒ no policy (nil holder), the
// byte-compatible pre-M82 permissive state.
func TestToolPolicy_AbsentFile_Permissive(t *testing.T) {
	holder := &egress.PolicyHolder{}
	// A path that does not exist (no ConfigMap mounted) ⇒ nil policy, no error.
	loadInitialToolPolicy(holder, filepath.Join(t.TempDir(), "missing.json"), logr.Discard())
	assert.Nil(t, holder.Load(), "an absent policy file ⇒ no policy (permissive)")

	// An explicitly EMPTY file ⇒ nil policy too.
	dir := t.TempDir()
	path := writePolicyFile(t, dir, "")
	loadInitialToolPolicy(holder, path, logr.Discard())
	assert.Nil(t, holder.Load(), "an empty policy file ⇒ no policy (permissive)")
}

// TestToolPolicy_Reload_NewPolicyActive proves the K3 core: rewriting the mounted file and reloading
// swaps the held policy live — WITHOUT a restart.
func TestToolPolicy_Reload_NewPolicyActive(t *testing.T) {
	dir := t.TempDir()
	path := writePolicyFile(t, dir, denyPolicy("allow", "shell_exec", "deny"))

	holder := &egress.PolicyHolder{}
	loadInitialToolPolicy(holder, path, logr.Discard())
	require.Equal(t, "deny", holder.Load().RuleFor("shell_exec"), "original policy denies shell_exec")

	// Rewrite to a DIFFERENT policy and reload (as the fsnotify watch would).
	require.NoError(t, os.WriteFile(path, []byte(denyPolicy("deny", "shell_exec", "allow")), 0o600))
	reloadToolPolicy(holder, path, logr.Discard())

	assert.Equal(t, "allow", holder.Load().RuleFor("shell_exec"),
		"after reload shell_exec is allowed (new policy active, no restart)")
	assert.Equal(t, "deny", holder.Load().RuleFor("other"), "after reload the new default (deny) applies")
}

// TestToolPolicy_Reload_MalformedKeepsLastGood proves the SACRED fail-closed invariant: a malformed
// reload KEEPS the last-good policy (never drops it, never crashes).
func TestToolPolicy_Reload_MalformedKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	path := writePolicyFile(t, dir, denyPolicy("deny", "web_search", "allow"))

	holder := &egress.PolicyHolder{}
	loadInitialToolPolicy(holder, path, logr.Discard())
	require.NotNil(t, holder.Load())

	// Rewrite to GARBAGE and reload — the parse must fail and the swap must NOT happen.
	require.NoError(t, os.WriteFile(path, []byte(`{ this is not valid json`), 0o600))
	reloadToolPolicy(holder, path, logr.Discard())

	p := holder.Load()
	require.NotNil(t, p, "a malformed reload must NOT drop the policy")
	assert.Equal(t, "allow", p.RuleFor("web_search"), "the last-good policy is still held after a malformed reload")
}

// TestToolPolicy_Reload_EmptyClears proves the ONE content-driven path to nil: an explicitly emptied
// file (the operator cleared the policy) turns the held policy off — permissive.
func TestToolPolicy_Reload_EmptyClears(t *testing.T) {
	dir := t.TempDir()
	path := writePolicyFile(t, dir, denyPolicy("deny", "web_search", "allow"))

	holder := &egress.PolicyHolder{}
	loadInitialToolPolicy(holder, path, logr.Discard())
	require.NotNil(t, holder.Load(), "policy active at startup")

	require.NoError(t, os.WriteFile(path, []byte(""), 0o600)) // operator cleared it
	reloadToolPolicy(holder, path, logr.Discard())
	assert.Nil(t, holder.Load(), "an explicitly emptied file clears the held policy (permissive)")
}

// TestToolPolicy_Reload_TransientReadErrorKeepsLastGood proves a transient read error (reading a
// directory as a file) does NOT drop the active policy — keep-last-good.
func TestToolPolicy_Reload_TransientReadErrorKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	path := writePolicyFile(t, dir, denyPolicy("deny", "web_search", "allow"))

	holder := &egress.PolicyHolder{}
	loadInitialToolPolicy(holder, path, logr.Discard())
	require.NotNil(t, holder.Load())

	// Reading a DIRECTORY as a file yields a non-NotExist read error → keep last-good.
	reloadToolPolicy(holder, dir, logr.Discard())
	require.NotNil(t, holder.Load(), "a transient read error must NOT drop the policy")
	assert.Equal(t, "allow", holder.Load().RuleFor("web_search"))
}

// TestToolPolicy_Watch_LiveReload proves the fsnotify watch actually fires a live reload when the
// mounted file changes (the end-to-end delivery path, not just the manual reload). It updates the
// file via an ATOMIC RENAME — the exact mechanism a projected-ConfigMap mount uses (it swaps the
// "..data" symlink, firing a Create/Rename in the watched dir), which is why watchToolPolicy keys on
// Create|Write|Rename|Remove (an in-place O_TRUNC overwrite surfaces as CHMOD on some platforms and
// is NOT how kubelet projects a ConfigMap update).
func TestToolPolicy_Watch_LiveReload(t *testing.T) {
	dir := t.TempDir()
	path := writePolicyFile(t, dir, denyPolicy("allow", "shell_exec", "deny"))

	holder := &egress.PolicyHolder{}
	loadInitialToolPolicy(holder, path, logr.Discard())
	require.Equal(t, "deny", holder.Load().RuleFor("shell_exec"))

	stop := make(chan struct{})
	go watchToolPolicy(holder, path, logr.Discard(), stop)
	t.Cleanup(func() { close(stop) })
	// Let the watcher establish its fsnotify Add on the dir before the update — otherwise the rename
	// can fire before the watch is registered and the event is missed (a test-only startup race; the
	// running sidecar establishes the watch once at boot, long before any policy edit).
	time.Sleep(100 * time.Millisecond)

	// Atomically replace the file (write a temp sibling, then rename over) — the ConfigMap-update
	// mechanism the directory watch is designed to catch.
	tmp := filepath.Join(dir, "policy.json.new")
	require.NoError(t, os.WriteFile(tmp, []byte(denyPolicy("deny", "shell_exec", "allow")), 0o600))
	require.NoError(t, os.Rename(tmp, path))

	require.Eventually(t, func() bool {
		return holder.Load() != nil && holder.Load().RuleFor("shell_exec") == "allow"
	}, 3*time.Second, 10*time.Millisecond, "the fsnotify watch must live-reload the rewritten policy")
}
