package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAnAgentWithoutSkillsKeepsItsRevisionName. The feature must not re-roll every existing
// agent on upgrade: a pod-template change that moves the revision name restarts the workload,
// and doing that fleet-wide for a capability an agent does not use is pure churn.
func TestAnAgentWithoutSkillsKeepsItsRevisionName(t *testing.T) {
	t.Parallel()
	assert.Empty(t, skillDigest(nil), "no skills ⇒ no digest component ⇒ byte-identical revision name")
	assert.Empty(t, skillDigest([]string{}))
}

// TestAnAliasMoveRollsARevision is the reason the digest is over RESOLVED refs rather than the
// spec's own strings. Two specs both saying "summarise@stable" are identical as text; if the
// alias moved between them they are different agents, and a revision that did not roll would
// leave the old content serving with no signal that anything changed.
func TestAnAliasMoveRollsARevision(t *testing.T) {
	t.Parallel()

	before := skillDigest([]string{"summarise@sha256:aaa"})
	after := skillDigest([]string{"summarise@sha256:bbb"})
	assert.NotEqual(t, before, after, "a different resolved digest must produce a different revision")

	// Re-resolving to the SAME digests must NOT roll: reconciliation runs constantly, and a
	// digest that changed on every pass would restart the agent forever.
	assert.Equal(t, before, skillDigest([]string{"summarise@sha256:aaa"}))
}

// TestSkillOrderIsPartOfTheIdentity. The list is `listType=atomic` and ordered, and the order
// reaches the agent — so reordering is a real change and must roll, not be silently equal.
func TestSkillOrderIsPartOfTheIdentity(t *testing.T) {
	t.Parallel()

	a := skillDigest([]string{"one@sha256:aaa", "two@sha256:bbb"})
	b := skillDigest([]string{"two@sha256:bbb", "one@sha256:aaa"})
	assert.NotEqual(t, a, b)
}
