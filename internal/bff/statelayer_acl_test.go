package bff

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every key the BFF writes to the state layer must be admitted by its ACL; see the matching test in
// internal/statelayer. The bind store and the proof replay set shipped without, and failed NOPERM.
func TestStateLayerACLAdmitsEveryKeyTheBFFWrites(t *testing.T) {
	keys := []string{
		runcapBindKey("jti:x"), proofSpendKey("x"), runControlKey("ns", "r"),
		spendKeyPrefix + "t:spend:2026-10", agentSpendKeyPrefix + "ns/a:spend:2026-10",
	}
	for _, path := range []string{
		"../../config/statelayer/deployment.yaml",
		"../../deploy/helm/ctxmesh/templates/dev-data-plane.yaml",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		matches := regexp.MustCompile(`-\s+"?~([^"*\s]+)\*"?`).FindAllStringSubmatch(string(raw), -1)
		prefixes := make([]string, 0, len(matches))
		for _, m := range matches {
			prefixes = append(prefixes, m[1])
		}
		if len(prefixes) == 0 {
			t.Fatalf("%s: found no ACL key patterns — the test would pass vacuously", path)
		}
		for _, k := range keys {
			admitted := false
			for _, p := range prefixes {
				if strings.HasPrefix(k, p) {
					admitted = true
					break
				}
			}
			if !admitted {
				t.Errorf("%s: the ACL does not admit %q — it would fail NOPERM", path, k)
			}
		}
	}
}
