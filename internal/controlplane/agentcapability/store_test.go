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

package agentcapability_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentcapability"
)

// eachStore runs a contract against the in-memory twin AND the Postgres store (when
// CONTROLPLANE_TEST_DSN points at a throwaway migrated DB).
func eachStore(t *testing.T, fn func(t *testing.T, s agentcapability.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, agentcapability.NewMemStore()) })

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres run (the twin still ran)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE agent_capabilities`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, agentcapability.NewPostgresStore(db)) })
}

// Registration round-trip: a described agent becomes discoverable within its registry, an updated
// descriptor replaces the old one, and a prune removes it from the candidate set.
func TestStore_RegistrationRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, s agentcapability.Store) {
		ctx := context.Background()

		got, err := s.List(ctx, "ns1", "team-a")
		require.NoError(t, err)
		assert.Empty(t, got, "an empty registry has no discoverable agents")

		want := agentcapability.AgentCapability{
			Namespace: "ns1", Agent: "summarizer", RegistryID: "team-a", Role: "worker",
			Endpoint:    "http://summarizer.ns1.svc.cluster.local",
			Description: "Summarizes long documents and extracts action items.",
			Tags:        []string{"summarization", "pdf"}, Ready: true,
		}
		require.NoError(t, s.Set(ctx, want))
		got, err = s.List(ctx, "ns1", "team-a")
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, want, got[0], "the registration round-trips verbatim (descriptor + tags + readiness)")

		// Re-registering the same agent REPLACES its descriptor, role and address (a spec edit, not a
		// duplicate row) — an execution-model change moves where it receives deliveries.
		want.Role = "reviewer"
		want.Endpoint = "http://summarizer-eventing.ns1.svc.cluster.local"
		want.Description = "Answers questions about SQL schemas."
		want.Tags = nil
		want.Ready = false
		require.NoError(t, s.Set(ctx, want))
		got, err = s.List(ctx, "ns1", "team-a")
		require.NoError(t, err)
		require.Len(t, got, 1, "re-registration upserts; it never duplicates")
		assert.Equal(t, want, got[0])

		// Get resolves ONE agent's own registration — how the discovery path learns the CALLER's scope
		// from the control plane rather than from anything the calling pod claims.
		self, ok, err := s.Get(ctx, "ns1", "summarizer")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "team-a", self.RegistryID, "the caller's own row supplies its discovery scope")
		assert.Equal(t, "reviewer", self.Role, "and the role the BFF attests for it")
		assert.Equal(t, "http://summarizer-eventing.ns1.svc.cluster.local", self.Endpoint,
			"and the address the async dispatcher delivers to")
		_, ok, err = s.Get(ctx, "ns1", "nobody")
		require.NoError(t, err)
		assert.False(t, ok, "an unregistered agent has no scope (fail-closed)")

		// Prune (the reconciler clearing spec.capabilities, or the agent being deleted).
		require.NoError(t, s.Delete(ctx, "ns1", "summarizer"))
		got, err = s.List(ctx, "ns1", "team-a")
		require.NoError(t, err)
		assert.Empty(t, got, "a pruned agent is no longer discoverable")
		require.NoError(t, s.Delete(ctx, "ns1", "summarizer"), "deleting an absent registration is a no-op")
	})
}

// Discovery is scoped: a query never reaches across a registry, across a namespace, or into the
// unscoped (no-registry) set.
func TestStore_ListIsScopedToRegistryAndNamespace(t *testing.T) {
	eachStore(t, func(t *testing.T, s agentcapability.Store) {
		ctx := context.Background()
		mk := func(ns, agent, registry string) agentcapability.AgentCapability {
			return agentcapability.AgentCapability{
				Namespace: ns, Agent: agent, RegistryID: registry, Description: agent + " does things.", Ready: true,
			}
		}
		require.NoError(t, s.Set(ctx, mk("ns1", "writer", "team-a")))
		require.NoError(t, s.Set(ctx, mk("ns1", "auditor", "team-a")))
		require.NoError(t, s.Set(ctx, mk("ns1", "outsider", "team-b")))
		require.NoError(t, s.Set(ctx, mk("ns2", "writer", "team-a")))
		require.NoError(t, s.Set(ctx, mk("ns1", "loner", "")))

		got, err := s.List(ctx, "ns1", "team-a")
		require.NoError(t, err)
		names := make([]string, len(got))
		for i, a := range got {
			names[i] = a.Agent
		}
		assert.Equal(t, []string{"auditor", "writer"}, names,
			"only ns1/team-a members, name-ordered so ranking built on this is deterministic")

		// An agent in NO registry is never a candidate — discovery is fail-closed on scope.
		got, err = s.List(ctx, "ns1", "")
		require.NoError(t, err)
		assert.Empty(t, got, "an empty registry scope yields no candidates (never 'every agent')")
	})
}

// A membership-only registration (no description) carries a discovery SCOPE but advertises nothing:
// Get resolves it — that is how a supervisor that advertises no capability of its own still discovers —
// while List never offers it as a candidate.
func TestStore_MembershipOnlyRowScopesButIsNotACandidate(t *testing.T) {
	eachStore(t, func(t *testing.T, s agentcapability.Store) {
		ctx := context.Background()
		require.NoError(t, s.Set(ctx, agentcapability.AgentCapability{
			Namespace: "ns1", Agent: "supervisor", RegistryID: "team-a", Role: "orchestrator", Ready: true, // no description
		}))
		require.NoError(t, s.Set(ctx, agentcapability.AgentCapability{
			Namespace: "ns1", Agent: "worker", RegistryID: "team-a",
			Description: "Extracts tables from invoices.", Ready: true,
		}))

		self, ok, err := s.Get(ctx, "ns1", "supervisor")
		require.NoError(t, err)
		require.True(t, ok, "a member registers even with nothing advertised — it still needs a scope")
		assert.Equal(t, "team-a", self.RegistryID)
		assert.Equal(t, "orchestrator", self.Role, "a member's role is recorded with nothing advertised")
		assert.Empty(t, self.Description)

		got, err := s.List(ctx, "ns1", "team-a")
		require.NoError(t, err)
		require.Len(t, got, 1, "only the DESCRIBED agent is a candidate")
		assert.Equal(t, "worker", got[0].Agent)
	})
}

// ListMembers is every agent in a registry, across namespaces and whether or not it advertises anything:
// the agents a hop can be sent to, each of which the async dispatcher keeps a queue for. An agent in no
// registry is not one.
func TestStore_ListMembersSpansNamespacesAndSkipsNonMembers(t *testing.T) {
	eachStore(t, func(t *testing.T, s agentcapability.Store) {
		ctx := context.Background()
		for _, a := range []agentcapability.AgentCapability{
			{Namespace: "ns2", Agent: "writer", RegistryID: "team-b", Endpoint: "http://writer-eventing.ns2.svc.cluster.local"},
			{Namespace: "ns1", Agent: "worker", RegistryID: "team-a", Description: "Extracts tables."},
			{Namespace: "ns1", Agent: "batch", RegistryID: "team-a"}, // a job: a member with no address
			{Namespace: "ns1", Agent: "loner", Description: "Belongs to no registry."},
		} {
			require.NoError(t, s.Set(ctx, a))
		}

		got, err := s.ListMembers(ctx)
		require.NoError(t, err)
		names := make([]string, 0, len(got))
		for _, a := range got {
			names = append(names, a.Namespace+"/"+a.Agent)
		}
		assert.Equal(t, []string{"ns1/batch", "ns1/worker", "ns2/writer"}, names,
			"every member, ordered by namespace and name; the agent in no registry is left out")
		assert.Equal(t, "http://writer-eventing.ns2.svc.cluster.local", got[2].Endpoint, "rows round-trip")
	})
}
