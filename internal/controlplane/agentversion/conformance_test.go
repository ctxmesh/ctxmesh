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

package agentversion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/pgtest"
)

const (
	ns    = "team-a"
	agent = "echo"
	uid   = "uid-1"
)

func ver(name, image string) Version {
	return Version{
		Namespace: ns, Agent: agent, AgentUID: uid, Name: name,
		Snapshot: agentsv1beta1.AgentDeploymentSpec{Image: image},
	}
}

func names(t *testing.T, s Store, namespace, agentUID string) []string {
	t.Helper()
	entries, err := s.List(context.Background(), namespace, agentUID)
	require.NoError(t, err)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func runConformance(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()

	t.Run("a returning version keeps its first place", func(t *testing.T) {
		s := newStore(t)
		for _, v := range []string{"echo-a", "echo-b"} {
			inserted, err := s.Record(ctx, ver(v, "img:"+v))
			require.NoError(t, err)
			assert.True(t, inserted, v)
		}
		// A, B, then A again: A is not re-inserted and stays older than B.
		inserted, err := s.Record(ctx, ver("echo-a", "img:changed"))
		require.NoError(t, err)
		assert.False(t, inserted, "a recorded version is not recorded again")
		assert.Equal(t, []string{"echo-b", "echo-a"}, names(t, s, ns, uid))

		got, ok, err := s.Get(ctx, ns, uid, "echo-a")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "img:echo-a", got.Snapshot.Image, "a recorded snapshot is never modified")

		prior, ok, err := s.Prior(ctx, ns, uid, "echo-b")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "echo-a", prior)
	})

	t.Run("versions recorded in the same second keep insertion order", func(t *testing.T) {
		s := newStore(t)
		at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
		// Names sort opposite to insertion, and every timestamp ties.
		for _, v := range []string{"echo-z", "echo-m", "echo-a"} {
			r := ver(v, "img")
			r.CreatedAt = at
			_, err := s.Record(ctx, r)
			require.NoError(t, err)
		}
		assert.Equal(t, []string{"echo-a", "echo-m", "echo-z"}, names(t, s, ns, uid))
		prior, ok, err := s.Prior(ctx, ns, uid, "echo-a")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "echo-m", prior)
	})

	t.Run("a snapshot round-trips", func(t *testing.T) {
		s := newStore(t)
		v := ver("echo-a", "img:1")
		v.Snapshot.PromptRef = "support-v1"
		v.Snapshot.Runtime = &agentsv1beta1.RuntimeSpec{ToolPolicy: &agentsv1beta1.ToolPolicySpec{
			Default:   "allow",
			Overrides: []agentsv1beta1.ToolPolicyOverride{{Name: "db.delete", Rule: "require-approval"}},
		}}
		created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		v.CreatedAt = created
		_, err := s.Record(ctx, v)
		require.NoError(t, err)

		got, ok, err := s.Get(ctx, ns, uid, "echo-a")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, v.Snapshot, got.Snapshot)
		assert.Equal(t, agent, got.Agent)
		assert.Equal(t, SourceController, got.Source)
		assert.True(t, created.Equal(got.CreatedAt), "an explicit creation time is kept")
		assert.Positive(t, got.Seq)

		_, ok, err = s.Get(ctx, ns, "uid-other", "echo-a")
		require.NoError(t, err)
		assert.False(t, ok, "a version belongs to one incarnation")
	})

	t.Run("prior is absent for the oldest and for an unknown name", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Record(ctx, ver("echo-a", "img"))
		require.NoError(t, err)
		_, ok, err := s.Prior(ctx, ns, uid, "echo-a")
		require.NoError(t, err)
		assert.False(t, ok)
		_, ok, err = s.Prior(ctx, ns, uid, "echo-missing")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("history is per incarnation and per namespace", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Record(ctx, ver("echo-a", "img"))
		require.NoError(t, err)
		next := ver("echo-a", "img")
		next.AgentUID = "uid-2"
		_, err = s.Record(ctx, next)
		require.NoError(t, err)
		elsewhere := ver("echo-b", "img")
		elsewhere.Namespace = "team-b"
		_, err = s.Record(ctx, elsewhere)
		require.NoError(t, err)

		assert.Equal(t, []string{"echo-a"}, names(t, s, ns, uid), "the other namespace's version is not listed")
		assert.Equal(t, []string{"echo-a"}, names(t, s, ns, "uid-2"))
		assert.Equal(t, []string{"echo-b"}, names(t, s, "team-b", uid))
	})

	t.Run("owners name every incarnation holding a version name", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Record(ctx, ver("shared-v1", "img"))
		require.NoError(t, err)
		other := ver("shared-v1", "img")
		other.Agent, other.AgentUID = "other", "uid-9"
		_, err = s.Record(ctx, other)
		require.NoError(t, err)

		owners, err := s.Owners(ctx, ns, "shared-v1")
		require.NoError(t, err)
		assert.Equal(t, []Owner{{Agent: agent, AgentUID: uid}, {Agent: "other", AgentUID: "uid-9"}}, owners)
		owners, err = s.Owners(ctx, ns, "nobody")
		require.NoError(t, err)
		assert.Empty(t, owners)
	})

	t.Run("prune keeps the newest and the pinned, and drops earlier incarnations", func(t *testing.T) {
		s := newStore(t)
		for _, v := range []string{"v1", "v2", "v3", "v4", "v5"} {
			_, err := s.Record(ctx, ver(v, "img"))
			require.NoError(t, err)
		}
		old := ver("v0", "img")
		old.AgentUID = "uid-0"
		_, err := s.Record(ctx, old)
		require.NoError(t, err)
		other := ver("v1", "img")
		other.Agent, other.AgentUID = "other", "uid-9"
		_, err = s.Record(ctx, other)
		require.NoError(t, err)

		removed, err := s.Prune(ctx, ns, agent, uid, 2, []string{"v2", "not-recorded"})
		require.NoError(t, err)
		assert.Equal(t, 3, removed, "v1, v3 and the earlier incarnation's v0")
		assert.Equal(t, []string{"v5", "v4", "v2"}, names(t, s, ns, uid))
		assert.Empty(t, names(t, s, ns, "uid-0"))
		assert.Equal(t, []string{"v1"}, names(t, s, ns, "uid-9"), "another agent is untouched")

		removed, err = s.Prune(ctx, ns, agent, uid, 2, []string{"v2"})
		require.NoError(t, err)
		assert.Zero(t, removed, "pruning again removes nothing")
	})

	t.Run("deleting an agent removes every incarnation and nothing else", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Record(ctx, ver("v1", "img"))
		require.NoError(t, err)
		old := ver("v0", "img")
		old.AgentUID = "uid-0"
		_, err = s.Record(ctx, old)
		require.NoError(t, err)
		other := ver("v1", "img")
		other.Agent, other.AgentUID = "other", "uid-9"
		_, err = s.Record(ctx, other)
		require.NoError(t, err)

		require.NoError(t, s.DeleteAgent(ctx, ns, agent))
		assert.Empty(t, names(t, s, ns, uid))
		assert.Empty(t, names(t, s, ns, "uid-0"))
		assert.Equal(t, []string{"v1"}, names(t, s, ns, "uid-9"))
	})

	t.Run("an incomplete identity is refused", func(t *testing.T) {
		s := newStore(t)
		for _, mut := range []func(*Version){
			func(v *Version) { v.Namespace = "" },
			func(v *Version) { v.Agent = "" },
			func(v *Version) { v.AgentUID = "" },
			func(v *Version) { v.Name = "" },
		} {
			v := ver("v1", "img")
			mut(&v)
			_, err := s.Record(ctx, v)
			assert.ErrorIs(t, err, controlplane.ErrInvalid)
		}
	})

	t.Run("the import runs once, ordered by creation, and records its source", func(t *testing.T) {
		s := newStore(t)
		calls := 0
		at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		load := func(_ context.Context, emit func(Version) error) (int, error) {
			calls++
			// Emitted newest first; a tie in time falls back to the namespace, then the name.
			late, b, a := ver("imp-late", "img"), ver("imp-b", "img:b"), ver("imp-a", "img:a")
			late.CreatedAt = at.Add(time.Hour)
			b.CreatedAt, a.CreatedAt = at, at
			for _, v := range []Version{late, b, a} {
				if err := emit(v); err != nil {
					return 0, err
				}
			}
			return 1, nil
		}
		res, err := s.ImportOnce(ctx, load)
		require.NoError(t, err)
		assert.Equal(t, ImportResult{Ran: true, Imported: 3, Skipped: 1}, res)
		assert.Equal(t, []string{"imp-late", "imp-b", "imp-a"}, names(t, s, ns, uid))
		got, ok, err := s.Get(ctx, ns, uid, "imp-a")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, SourceImport, got.Source)
		assert.Equal(t, "img:a", got.Snapshot.Image)
		assert.True(t, got.CreatedAt.Equal(at))

		res, err = s.ImportOnce(ctx, load)
		require.NoError(t, err)
		assert.False(t, res.Ran, "a completed import does not run again")
		assert.Equal(t, 1, calls)
	})

	t.Run("a failed import writes nothing and runs again", func(t *testing.T) {
		s := newStore(t)
		boom := errors.New("api server unavailable")
		_, err := s.ImportOnce(ctx, func(_ context.Context, emit func(Version) error) (int, error) {
			if err := emit(ver("imp-a", "img")); err != nil {
				return 0, err
			}
			return 0, boom
		})
		require.ErrorIs(t, err, boom)
		assert.Empty(t, names(t, s, ns, uid), "versions emitted before the failure are not kept")

		_, err = s.ImportOnce(ctx, func(_ context.Context, emit func(Version) error) (int, error) {
			for _, v := range []Version{ver("imp-a", "img"), ver("", "img")} {
				if err := emit(v); err != nil {
					return 0, err
				}
			}
			return 0, nil
		})
		require.ErrorIs(t, err, controlplane.ErrInvalid)
		assert.Empty(t, names(t, s, ns, uid), "a partial import leaves no rows")

		res, err := s.ImportOnce(ctx, func(_ context.Context, emit func(Version) error) (int, error) {
			return 0, emit(ver("imp-a", "img"))
		})
		require.NoError(t, err)
		assert.True(t, res.Ran, "the import is retried after a failure")
		assert.Equal(t, []string{"imp-a"}, names(t, s, ns, uid))
	})

	t.Run("an import larger than one batch keeps every version in order", func(t *testing.T) {
		s := newStore(t)
		const n = 3*importBatch + 7
		at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		res, err := s.ImportOnce(ctx, func(_ context.Context, emit func(Version) error) (int, error) {
			// Emitted newest first, so storing in emission order would reverse the history.
			for i := n - 1; i >= 0; i-- {
				v := ver(fmt.Sprintf("v%04d", i), "img")
				v.CreatedAt = at.Add(time.Duration(i) * time.Second)
				if err := emit(v); err != nil {
					return 0, err
				}
			}
			return 0, nil
		})
		require.NoError(t, err)
		assert.Equal(t, n, res.Imported)
		got := names(t, s, ns, uid)
		require.Len(t, got, n)
		for i, name := range got {
			require.Equal(t, fmt.Sprintf("v%04d", n-1-i), name, "position %d", i)
		}
	})

	t.Run("two replicas starting together import once", func(t *testing.T) {
		s := newStore(t)
		var calls atomic.Int32
		var wg sync.WaitGroup
		results := make([]ImportResult, 2)
		errs := make([]error, 2)
		for i := range 2 {
			wg.Go(func() {
				results[i], errs[i] = s.ImportOnce(ctx, func(_ context.Context, emit func(Version) error) (int, error) {
					calls.Add(1)
					time.Sleep(50 * time.Millisecond) // hold the lock while the other replica arrives
					return 0, emit(ver("imp-a", "img"))
				})
			})
		}
		wg.Wait()
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])
		assert.Equal(t, int32(1), calls.Load(), "only one replica loads")
		assert.True(t, results[0].Ran != results[1].Ran, "exactly one replica ran the import: %+v", results)
	})

	t.Run("an import that finds nothing still completes", func(t *testing.T) {
		s := newStore(t)
		res, err := s.ImportOnce(ctx, func(context.Context, func(Version) error) (int, error) { return 0, nil })
		require.NoError(t, err)
		assert.Equal(t, ImportResult{Ran: true}, res)
		res, err = s.ImportOnce(ctx, func(context.Context, func(Version) error) (int, error) {
			t.Fatal("a completed import must not load again")
			return 0, nil
		})
		require.NoError(t, err)
		assert.False(t, res.Ran)
	})
}

// The twin always runs; Postgres runs when CONTROLPLANE_TEST_DSN points at a throwaway database.
func TestStoreConformance(t *testing.T) {
	t.Run("mem", func(t *testing.T) {
		runConformance(t, func(*testing.T) Store { return NewMemStore() })
	})

	if os.Getenv("CONTROLPLANE_TEST_DSN") == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset: skipping the Postgres conformance run (the twin still ran)")
		return
	}
	t.Run("postgres", func(t *testing.T) {
		db := pgtest.Open(t, "agentversion_conformance")
		for _, table := range []string{"agent_versions", "agent_version_import"} {
			_, err := db.Exec(`CREATE TABLE IF NOT EXISTS ` + table + ` (LIKE public.` + table + ` INCLUDING ALL)`)
			require.NoError(t, err)
		}
		runConformance(t, func(t *testing.T) Store {
			_, err := db.Exec(`TRUNCATE agent_versions, agent_version_import`)
			require.NoError(t, err)
			return NewPostgresStore(db)
		})
	})
}
