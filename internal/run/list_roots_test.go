package run

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listRootsContract is the ListRoots behaviour both stores must share: the agent allow-set is the
// isolation boundary, child runs never appear, and keyset paging neither skips nor repeats a run.
func listRootsContract(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }
	mk := func(id, ns, agent string, created time.Time, parent string) {
		r := New(id, ns, agent, nil, "", created)
		r.ParentRunID = parent
		if parent != "" {
			r.RootRunID = parent
		}
		r.TraceID = "trace-" + id
		require.NoError(t, s.Create(r))
	}
	mk("a1", "team", "worker", at(1), "")
	mk("a2", "team", "worker", at(2), "")
	mk("a3", "team", "worker", at(3), "")
	mk("b1", "team", "other", at(4), "")
	mk("c1", "rival", "worker", at(5), "") // same agent name, another namespace
	mk("a3-child", "team", "worker", at(6), "a3")
	mk("a4", "team", "worker", at(3), "") // ties a3 on created_at; id breaks the tie

	ids := func(rows []RootRun) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	worker := []AgentKey{{Namespace: "team", Name: "worker"}}

	none, err := s.ListRoots(ctx, RootListFilter{})
	require.NoError(t, err)
	assert.Empty(t, none, "no agents means no runs, never every run")

	all, err := s.ListRoots(ctx, RootListFilter{Agents: worker})
	require.NoError(t, err)
	assert.Equal(t, []string{"a4", "a3", "a2", "a1"}, ids(all),
		"newest first, ties by id; another namespace's agent of the same name and child runs are excluded")
	assert.Equal(t, "trace-a4", all[0].TraceID)
	assert.Equal(t, "team", all[0].Namespace)
	assert.Equal(t, StatusQueued, all[0].Status)

	both, err := s.ListRoots(ctx, RootListFilter{Agents: append(worker, AgentKey{Namespace: "team", Name: "other"})})
	require.NoError(t, err)
	assert.Equal(t, []string{"b1", "a4", "a3", "a2", "a1"}, ids(both))

	// Pairs, not a cross product: allowing team/worker and rival/other must not admit team/other (b1)
	// or rival/worker (c1), which "namespace IN (...) AND agent IN (...)" would.
	pairs, err := s.ListRoots(ctx, RootListFilter{Agents: append(worker, AgentKey{Namespace: "rival", Name: "other"})})
	require.NoError(t, err)
	assert.Equal(t, []string{"a4", "a3", "a2", "a1"}, ids(pairs))

	bounded, err := s.ListRoots(ctx, RootListFilter{Agents: worker, From: at(2), To: at(3)})
	require.NoError(t, err)
	assert.Equal(t, []string{"a4", "a3", "a2"}, ids(bounded), "From and To are inclusive")

	var paged []string
	var before *RootCursor
	for range 5 {
		rows, err := s.ListRoots(ctx, RootListFilter{Agents: worker, Limit: 3, Before: before})
		require.NoError(t, err)
		if len(rows) == 0 {
			break
		}
		paged = append(paged, ids(rows)...)
		last := rows[len(rows)-1]
		before = &RootCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	assert.Equal(t, ids(all), paged, "paging walks the same order with no gap and no repeat")
}

func TestMemStore_ListRoots(t *testing.T) {
	listRootsContract(t, NewMemStore())
}

func TestPostgresStore_ListRoots(t *testing.T) {
	listRootsContract(t, openPGStore(t))
}
