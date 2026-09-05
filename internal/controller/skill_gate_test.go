package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// TestEditingSkillRefsReachesTheDeployGate. specHash covers the whole spec, so attaching or
// changing a skill produces a new hash → a new AgentVersion → the eval gate evaluates it. A
// skill change alters agent behaviour, so a path that bypassed the gate would make the gate
// decorative.
func TestEditingSkillRefsReachesTheDeployGate(t *testing.T) {
	t.Parallel()

	base := agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"}
	withSkill := base
	withSkill.SkillRefs = []string{"summarise@sha256:aaa"}
	changed := base
	changed.SkillRefs = []string{"summarise@sha256:bbb"}

	h0, err := specHash(base)
	require.NoError(t, err)
	h1, err := specHash(withSkill)
	require.NoError(t, err)
	h2, err := specHash(changed)
	require.NoError(t, err)

	assert.NotEqual(t, h0, h1, "attaching a skill must produce a new version for the gate to see")
	assert.NotEqual(t, h1, h2, "pinning a different digest is a different agent")
}

// TestAnAliasMoveDoesNotChangeTheSpecHash records a REAL and deliberate asymmetry, so nobody
// discovers it by surprise.
//
// Moving an alias changes what the agent runs — the revision digest is taken over the RESOLVED
// refs, so the workload rolls. It does NOT change the spec, so specHash is unchanged and the
// eval gate does not re-evaluate.
//
// That is the correct trade for now: the gate keys on the user's declared intent, and an alias
// move is a change made by whoever owns the skill rather than by the agent's owner. But it does
// mean a moved alias reaches production without a fresh gate run, which is exactly why
// "@latest" has to be written explicitly rather than being the default (ParseRef refuses a bare
// name). Carded as m52.M161-alias-gate.
func TestAnAliasMoveDoesNotChangeTheSpecHash(t *testing.T) {
	t.Parallel()

	spec := agentsv1alpha1.AgentDeploymentSpec{
		Image:     "img:1",
		SkillRefs: []string{"summarise@stable"},
	}
	before, err := specHash(spec)
	require.NoError(t, err)

	// The alias moved in the store; the SPEC is byte-identical.
	after, err := specHash(spec)
	require.NoError(t, err)
	assert.Equal(t, before, after,
		"the spec did not change, so the gate does not re-run — the revision digest is what rolls")

	// And the revision digest DOES move, so the workload is not left serving stale content.
	assert.NotEqual(t,
		skillDigest([]string{"summarise@sha256:aaa"}),
		skillDigest([]string{"summarise@sha256:bbb"}),
		"a resolved-digest change must roll the revision even when the spec is unchanged")
}
