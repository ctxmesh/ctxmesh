package statelayer

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// aclKeyPrefixes reads the key patterns the state-layer Valkey's ACL admits (the `~prefix*` args).
func aclKeyPrefixes(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	matches := regexp.MustCompile(`-\s+"?~([^"*\s]+)\*"?`).FindAllStringSubmatch(string(raw), -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("%s: found no ACL key patterns — the test would pass vacuously", path)
	}
	return out
}

// Every key this package writes must be admitted by the ACL. A key space missing from the ACL fails
// NOPERM on a real cluster while every unit test (on miniredis or a map) passes: spawn (M119), agent
// (M122), and run/ns/fleet/user/conv (m184.9) each shipped that way.
func TestStateLayerACLAdmitsEveryKeyThisPackageWrites(t *testing.T) {
	keys := append(scopeKeys(ControlScope{Namespace: "ns", Agent: "a", Tenant: "t", RunID: "r"}),
		dedupKey("ns", "m"),
		quotaRPMKey("t", 1), quotaSpendKey("t"), quotaInflightKey("t"),
		agentSpendKey("ns/a"), convSpendKey("c"),
		userRPMKey("u", 1), userSpendKey("u"), userInflightKey("u"),
		spawnKey("ns", "s", "root", "inflight"),
		"mem:ns/a:k",
	)
	for _, path := range []string{
		"../../config/statelayer/deployment.yaml",
		"../../deploy/helm/ctxmesh/templates/dev-data-plane.yaml",
	} {
		prefixes := aclKeyPrefixes(t, path)
		for _, k := range keys {
			admitted := false
			for _, p := range prefixes {
				if strings.HasPrefix(k, p) {
					admitted = true
					break
				}
			}
			if !admitted {
				t.Errorf("%s: the ACL does not admit %q (patterns %v) — it would fail NOPERM", path, k, prefixes)
			}
		}
	}
}
