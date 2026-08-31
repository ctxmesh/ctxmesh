package bff

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentcapability"
)

// withRegistry gives a spawn server a capability registry seeded with the given rows, turning the
// delegate fence on (M142.1, ADR 0122).
func withRegistry(t *testing.T, s *Server, rows ...agentcapability.AgentCapability) {
	t.Helper()
	caps := agentcapability.NewMemStore()
	for _, row := range rows {
		require.NoError(t, caps.Set(context.Background(), row))
	}
	s.agentCapabilities = caps
}

// fenceNS is the namespace every fence case runs in — the fence is about the REGISTRY boundary, and a
// second namespace would only restate what the store's own scoping tests already cover.
const fenceNS = "team-ns"

// member is a membership-only registration — an agent in a registry that advertises no capability. It is
// the common case for a supervisor, and the reason m141.1 writes rows for members with no descriptor.
func member(agent, registry string) agentcapability.AgentCapability {
	return agentcapability.AgentCapability{Namespace: fenceNS, Agent: agent, RegistryID: registry, Ready: true}
}

// The fence permits what the team's declared boundary permits: a peer in the SAME AgentRegistry.
func TestSpawnFence_AllowsAPeerInTheSameRegistry(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("run-fence-1"))
	withRegistry(t, s,
		member("supervisor", "reg-a"),
		member("web-researcher", "reg-a"),
	)

	rec := postSpawn(t, s, mintCap(t, signer, "run-fence-1"), validSpawnBody())
	assert.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// THE FIX: a supervisor can no longer summon an arbitrary namespace peer. Before M142.1 the spawn edge
// bounded how MUCH a supervisor delegated but never WHAT to, so a prompt-injected model could name any
// agent in the namespace and the launcher would build its URL from that name.
func TestSpawnFence_RefusesAnAgentInAnotherRegistry(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("run-fence-2"))
	withRegistry(t, s,
		member("supervisor", "reg-a"),
		member("web-researcher", "reg-b"), // a namespace peer, ANOTHER trust boundary
	)

	rec := postSpawn(t, s, mintCap(t, signer, "run-fence-2"), validSpawnBody())
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "registryRef", "the refusal names the boundary it enforced")
}

// An unknown target is refused with the SAME message as an out-of-registry one: a supervisor probing
// names must not learn which agents exist outside its boundary.
func TestSpawnFence_UnknownTargetIsIndistinguishableFromOutOfRegistry(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("run-fence-3"))
	withRegistry(t, s, member("supervisor", "reg-a"))

	unknown := postSpawn(t, s, mintCap(t, signer, "run-fence-3"), validSpawnBody())
	require.Equal(t, http.StatusForbidden, unknown.Code)

	withRegistry(t, s,
		member("supervisor", "reg-a"),
		member("web-researcher", "reg-b"),
	)
	elsewhere := postSpawn(t, s, mintCap(t, signer, "run-fence-3"), validSpawnBody())
	require.Equal(t, http.StatusForbidden, elsewhere.Code)

	assert.Equal(t, unknown.Body.String(), elsewhere.Body.String(),
		"'does not exist' and 'not yours' must be indistinguishable — otherwise the edge is a name oracle")
}

// Fail-closed on an unverifiable caller. A missing membership row means the controller has not reconciled
// that agent yet; "I cannot verify this" is not a reason to allow it, and the error says how it converges.
func TestSpawnFence_FailsClosedWhenTheCallerHasNoMembership(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("run-fence-4"))
	withRegistry(t, s, member("web-researcher", "reg-a")) // the CALLER has no row

	rec := postSpawn(t, s, mintCap(t, signer, "run-fence-4"), validSpawnBody())
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "next reconcile",
		"an operator must be told the row converges, not left guessing at a bare denial")
}

// An agent in NO registry cannot delegate at all — the boundary is membership, and a non-member has none.
func TestSpawnFence_AnUnscopedCallerCannotDelegate(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("run-fence-5"))
	withRegistry(t, s,
		agentcapability.AgentCapability{Namespace: fenceNS, Agent: "supervisor", RegistryID: ""},
		member("web-researcher", "reg-a"),
	)

	rec := postSpawn(t, s, mintCap(t, signer, "run-fence-5"), validSpawnBody())
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "not a member of any AgentRegistry")
}

// Self-delegation is refused: it is never a legitimate delegation and it is an easy infinite-recursion
// foot-gun for a model, which the spawn budget would only catch after burning the whole budget.
func TestSpawnFence_RefusesSelfDelegation(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("run-fence-6"))
	withRegistry(t, s, member("supervisor", "reg-a"))

	body := validSpawnBody()
	body.SubAgent = "supervisor"
	rec := postSpawn(t, s, mintCap(t, signer, "run-fence-6"), body)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "cannot delegate to itself")
}

// With no capability registry wired at all (no control-plane DB) there is nothing to enforce against, so
// the edge behaves exactly as before rather than failing every delegation on an install that has no mirror.
func TestSpawnFence_NoRegistryWiredLeavesBehaviourUnchanged(t *testing.T) {
	s, signer, _ := newSpawnServer(t, mkParentRun("run-fence-7"))
	s.agentCapabilities = nil

	rec := postSpawn(t, s, mintCap(t, signer, "run-fence-7"), validSpawnBody())
	assert.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// THE SSRF FIX (M142.2, ADR 0122): a caller-supplied endpoint is IGNORED. Before this, the spawn edge
// stored whatever URL the body carried and the trusted run-worker later POSTed the sub-run's input there
// — making the control plane issue attacker-directed requests with platform identity. The address is a
// function of (namespace, name), both authoritative here, so there is nothing to accept.
func TestSpawnFence_CallerSuppliedEndpointIsIgnored(t *testing.T) {
	s, signer, store := newSpawnServer(t, mkParentRun("run-fence-8"))
	withRegistry(t, s, member("supervisor", "reg-a"), member("web-researcher", "reg-a"))

	body := validSpawnBody()
	body.Endpoint = "http://attacker.example.com/collect"

	rec := postSpawn(t, s, mintCap(t, signer, "run-fence-8"), body)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var resp SpawnRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	sub, err := store.Get(resp.ID)
	require.NoError(t, err)

	assert.Equal(t, "http://web-researcher.team-ns.svc.cluster.local", sub.Endpoint,
		"the address is DERIVED from the verified parent's namespace + the fenced agent name")
	assert.NotContains(t, sub.Endpoint, "attacker.example.com",
		"a foreign endpoint must never reach the run store, where the trusted worker would POST to it")
}

// An absent endpoint is now perfectly normal — the field is vestigial, so a launcher that stops sending
// it (or an older one that still does) both work.
func TestSpawnFence_AbsentEndpointIsFine(t *testing.T) {
	s, signer, store := newSpawnServer(t, mkParentRun("run-fence-9"))
	withRegistry(t, s, member("supervisor", "reg-a"), member("web-researcher", "reg-a"))

	body := validSpawnBody()
	body.Endpoint = ""

	rec := postSpawn(t, s, mintCap(t, signer, "run-fence-9"), body)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var resp SpawnRunResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	sub, err := store.Get(resp.ID)
	require.NoError(t, err)
	assert.Equal(t, "http://web-researcher.team-ns.svc.cluster.local", sub.Endpoint)
}
