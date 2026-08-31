package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Delegate BY CAPABILITY (m141.4, ADR 0120): a supervisor says what it needs done and the platform
// resolves who can do it — the supervisor never has to have been wired to the agent's name.
func TestDelegate_ByCapabilityResolvesAndSpawnsTheDiscoveredAgent(t *testing.T) {
	fc := &fakeSpawnClient{
		discoverNames: []string{"summarizer", "runner-up"},
		awaitRes:      spawnedRunResult{Status: "succeeded", Answer: "here are the action items"},
	}
	ds := newDelegate(t, fc, openBudget)

	resp := callDelegate(t, ds, "cap-token", delegateRequest{
		Capability: "summarize a long report", Input: json.RawMessage(`"the report"`), Step: "1", CallID: "c1",
	}, nil)

	require.True(t, resp.OK, resp.Error)
	assert.Equal(t, "here are the action items", resp.Answer)
	assert.Equal(t, 1, fc.discovered, "the capability is resolved through the platform, exactly once")
	assert.Equal(t, "summarize a long report", fc.gotCapability)
	assert.Equal(t, "cap-token", fc.gotDiscoverCap,
		"the run capability is relayed — the BFF derives the candidate set, the pod does not nominate it")
	assert.Equal(t, "summarizer", fc.gotBody.SubAgent, "the BEST match is delegated to")
	assert.Equal(t, "http://summarizer.team-ns.svc.cluster.local", fc.gotBody.Endpoint,
		"the discovered agent's endpoint is resolved the same way a named one is")
}

// A name still wins outright: naming a sub-agent must never trigger a discovery call.
func TestDelegate_NamedSubAgentSkipsDiscovery(t *testing.T) {
	fc := &fakeSpawnClient{
		discoverNames: []string{"someone-else"},
		awaitRes:      spawnedRunResult{Status: "succeeded", Answer: "done"},
	}
	ds := newDelegate(t, fc, openBudget)

	resp := callDelegate(t, ds, "cap-token", delegateRequest{
		SubAgent: "researcher", Capability: "ignored when a name is given",
		Input: json.RawMessage(`"go"`), Step: "1", CallID: "c1",
	}, nil)

	require.True(t, resp.OK, resp.Error)
	assert.Zero(t, fc.discovered, "an explicit name is authoritative — no discovery call is made")
	assert.Equal(t, "researcher", fc.gotBody.SubAgent)
}

// Nobody advertising the capability is an HONEST refusal the model can act on — never a silent fallback
// to some arbitrary agent, which would be the dangerous failure here.
func TestDelegate_NoCapabilityMatchRefusesInsteadOfGuessing(t *testing.T) {
	fc := &fakeSpawnClient{discoverNames: nil}
	ds := newDelegate(t, fc, openBudget)

	resp := callDelegate(t, ds, "cap-token", delegateRequest{
		Capability: "fly a spacecraft", Input: json.RawMessage(`"go"`), Step: "1", CallID: "c1",
	}, nil)

	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "fly a spacecraft", "the refusal names what was asked for")
	assert.Zero(t, fc.spawned, "nothing is spawned when nothing matches")
}

// Discovery being unavailable is likewise an honest tool error, not a spawn of whatever was handy.
func TestDelegate_DiscoveryFailureRefuses(t *testing.T) {
	fc := &fakeSpawnClient{discoverErr: errors.New("discovery unavailable")}
	ds := newDelegate(t, fc, openBudget)

	resp := callDelegate(t, ds, "cap-token", delegateRequest{
		Capability: "summarize", Input: json.RawMessage(`"go"`), Step: "1", CallID: "c1",
	}, nil)

	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "could not resolve an agent")
	assert.Zero(t, fc.spawned)
}

// Neither a name nor a capability is a usable request.
func TestDelegate_RequiresANameOrACapability(t *testing.T) {
	fc := &fakeSpawnClient{}
	ds := newDelegate(t, fc, openBudget)

	resp := callDelegate(t, ds, "cap-token", delegateRequest{
		Input: json.RawMessage(`"go"`), Step: "1", CallID: "c1",
	}, nil)

	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "name a subAgent or describe the capability")
	assert.Zero(t, fc.discovered)
	assert.Zero(t, fc.spawned)
}

// The L7 durable-suspend path resolves the capability BEFORE suspending, so the suspend signal carries
// the discovered agent's real endpoint — a placeholder there would strand the child run.
func TestDelegate_ByCapabilityResolvesBeforeSuspending(t *testing.T) {
	fc := &fakeSpawnClient{discoverNames: []string{"summarizer"}}
	ds := newDelegate(t, fc, openBudget)

	resp := callDelegate(t, ds, "cap-token", delegateRequest{
		Capability: "summarize a long report", Input: json.RawMessage(`"the report"`),
		Step: "1", CallID: "c1", Suspend: true,
	}, nil)

	require.True(t, resp.OK, resp.Error)
	assert.True(t, resp.Suspend)
	assert.Equal(t, "http://summarizer.team-ns.svc.cluster.local", resp.Endpoint,
		"the suspend signal carries the DISCOVERED agent's endpoint, resolved before the suspend")
	assert.Equal(t, 1, fc.discovered)
	assert.Zero(t, fc.spawned, "a suspend resolves and signals; it does not spawn")
}
