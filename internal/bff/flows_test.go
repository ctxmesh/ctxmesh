package bff

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// caps is a terse builder for a probed capability map: resource → verb → allowed.
func caps(pairs map[string][]string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for res, verbs := range pairs {
		out[res] = map[string]bool{}
		for _, v := range verbs {
			out[res][v] = true
		}
	}
	return out
}

// TestConnectProviderFlowNeedsEveryObjectItWrites is the regression test for the defect this
// registry exists to prevent. createProviderObjects upserts THREE objects, and the console
// asked about two. A caller holding secrets + secretbindings but not modelroutes saw an
// enabled button; because the Secret is written FIRST and the sequence is not transactional,
// the denial landed after a live credential was already in the cluster.
func TestConnectProviderFlowNeedsEveryObjectItWrites(t *testing.T) {
	t.Parallel()

	full := caps(map[string][]string{
		resSecrets:        {verbCreate},
		resSecretBindings: {verbCreate},
		resModelRoutes:    {verbCreate},
	})
	assert.True(t, evaluateFlows(full)["connectProvider"], "all three writes allowed ⇒ completable")

	// The exact shape that used to pass the UI's two-resource conjunction.
	missingRoute := caps(map[string][]string{
		resSecrets:        {verbCreate},
		resSecretBindings: {verbCreate},
	})
	assert.False(t, evaluateFlows(missingRoute)["connectProvider"],
		"no modelroutes.create ⇒ NOT completable, even though the old two-resource check passed")

	missingSecret := caps(map[string][]string{
		resSecretBindings: {verbCreate},
		resModelRoutes:    {verbCreate},
	})
	assert.False(t, evaluateFlows(missingSecret)["connectProvider"])
}

// TestRotateFlowAsksAboutTheSecretNotTheBinding: the console gated rotation on
// `secretbindings.update` while the write that actually matters is the core Secret — asking
// about the wrong object entirely.
func TestRotateFlowAsksAboutTheSecretNotTheBinding(t *testing.T) {
	t.Parallel()

	bindingOnly := caps(map[string][]string{
		resSecretBindings: {verbUpdate},
		resModelRoutes:    {verbUpdate},
	})
	assert.False(t, evaluateFlows(bindingOnly)["rotateProviderKey"],
		"secretbindings.update alone must not authorise a rotate — the key lives in the Secret")

	all := caps(map[string][]string{
		resSecrets:        {verbUpdate},
		resSecretBindings: {verbUpdate},
		resModelRoutes:    {verbUpdate},
	})
	assert.True(t, evaluateFlows(all)["rotateProviderKey"])
}

// TestUnprobedCellIsNotAllowed inverts the UI's optimistic default on purpose. `can()` returns
// true for a cell nobody probed, which is right for hiding chrome and wrong for a write: an
// unprobed cell means "we did not ask", and offering an unverified flow is how a partial write
// happens. The drift is not hypothetical — goldenResources omits workflows and alertpolicies
// while nav gated on them, so those gates never gated anything.
func TestUnprobedCellIsNotAllowed(t *testing.T) {
	t.Parallel()

	assert.False(t, evaluateFlows(map[string]map[string]bool{})["connectProvider"],
		"an empty map means nothing was probed — that is not permission")
}

// TestEveryFlowNeedIsProbed is the gate Fable's review asked for: a role can satisfy
// hack/flow-completability.sh and still leave the UI dark, because that script checks what the
// shipped ROLES grant and says nothing about what the capability probe ASKS.
//
// A flow need the probe never issues evaluates against a missing cell. evaluateFlows treats
// that as denied — safe, but invisible: the control disappears and no amount of granting
// permission brings it back, which is indistinguishable from the M155 defect this milestone
// exists to fix. So every need must be covered by a probe that actually runs.
func TestEveryFlowNeedIsProbed(t *testing.T) {
	t.Parallel()

	// What the agents-group cross-product covers.
	agentsResources := map[string]bool{}
	for _, r := range goldenResources {
		agentsResources[r] = true
	}
	agentsVerbs := map[string]bool{}
	for _, v := range goldenVerbs {
		agentsVerbs[v] = true
	}
	// What the derived core-group Secret probes cover.
	coreSecretVerbs := map[string]bool{}
	for _, v := range flowNeedsCoreSecretVerbs() {
		coreSecretVerbs[v] = true
	}

	require.NotEmpty(t, consoleFlows, "an empty registry would make every canFlow() false silently")

	for _, f := range consoleFlows {
		require.NotEmptyf(t, f.needs, "flow %q declares no writes — it would report completable to everyone", f.name)
		for _, n := range f.needs {
			require.NotEmptyf(t, n.verbs, "flow %q names %q with no verb", f.name, n.resource)
			for _, v := range n.verbs {
				switch {
				case n.group == "" && n.resource == resSecrets:
					require.Truef(t, coreSecretVerbs[v],
						"flow %q needs core secrets %q but the probe never asks for it", f.name, v)
				case n.group == agentsAPIGroup:
					require.Truef(t, agentsResources[n.resource],
						"flow %q needs %q, which goldenResources does not probe — the cell would read as denied forever",
						f.name, n.resource)
					require.Truef(t, agentsVerbs[v],
						"flow %q needs %q on %q, which goldenVerbs does not probe", f.name, v, n.resource)
				default:
					t.Fatalf("flow %q names group %q, which no probe covers", f.name, n.group)
				}
			}
		}
	}
}
