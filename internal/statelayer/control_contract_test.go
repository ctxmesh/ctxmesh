package statelayer

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ctxmesh/agentry/internal/controlplane/killscope"
)

// The cross-PACKAGE key contract (M146, ADR 0126).
//
// killscope.Scope.MarkerKey WRITES the accelerator marker; statelayer.scopeKeys READS it. They live in
// different packages and each spells the key out literally — deliberately, so both ends are greppable —
// which means nothing but a test stops them drifting apart. And the drift would be SILENT in the worst
// way: a kill would be recorded, reported active by the API, honoured by the fail-closed layers, and
// simply never interrupt an in-flight call. The stop would look like it worked.
//
// The same reasoning already guards run:{ns}:{id}:control (control.go's comment says the two must never
// drift); this extends it to every scope the hierarchy added.
func TestKeyContract_TheWriterAndReaderAgreeOnEveryScope(t *testing.T) {
	sc := ControlScope{Namespace: "team-a", Agent: "support-bot", Tenant: "acme", RunID: "run-1"}
	read := scopeKeys(sc)

	for _, tc := range []struct {
		name  string
		scope killscope.Scope
	}{
		{"agent", killscope.Scope{Level: killscope.LevelAgent, Namespace: "team-a", Agent: "support-bot"}},
		{"namespace", killscope.Scope{Level: killscope.LevelNamespace, Namespace: "team-a"}},
		{"tenant", killscope.Scope{Level: killscope.LevelTenant, Tenant: "acme"}},
		{"fleet", killscope.Scope{Level: killscope.LevelFleet}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Contains(t, read, tc.scope.MarkerKey(),
				"the %s marker the control plane WRITES is not a key the proxy READS — a kill at this "+
					"scope would be recorded, reported active, and never interrupt anything", tc.name)
		})
	}

	// And the spec-sourced projection must land on the SAME marker as the operator's stop: the two are
	// different intents but the same halt, so a suspended agent's in-flight call must be interrupted too.
	spec := killscope.Scope{Level: killscope.LevelAgent, Namespace: "team-a", Agent: "support-bot", Source: killscope.SourceSpec}
	assert.Contains(t, read, spec.MarkerKey(),
		"a spec.suspend projection must interrupt in-flight calls exactly like an operator's stop")
}
