package skill

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The conformance suite. Both implementations run it, so a behaviour that holds only in the
// in-memory twin is a test failure rather than a surprise in production — the pattern
// promptversion and credpostgres already use.
func runConformance(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()

	seed := func(t *testing.T) Store {
		s := newStore(t)
		require.NoError(t, s.UpsertSkill(ctx, Skill{
			Namespace: "team-a", Name: "summarise",
			Description: "Summarises long documents and extracts action items.",
		}))
		return s
	}

	t.Run("a version is immutable and identified by its digest", func(t *testing.T) {
		s := seed(t)
		v := SkillVersion{
			Namespace: "team-a", Skill: "summarise", Digest: Digest([]byte("v1")),
			Source: SourceUpload, ObjectKey: "uploads/v1",
		}
		require.NoError(t, s.AddVersion(ctx, v))

		// Re-adding the SAME digest is a no-op, not an error and not a duplicate. Same bytes
		// are the same version, so a retry must succeed and two callers uploading identical
		// content must not fork one thing into two.
		require.NoError(t, s.AddVersion(ctx, v))
		got, err := s.ListVersions(ctx, "team-a", "summarise")
		require.NoError(t, err)
		assert.Len(t, got, 1, "re-adding a digest must not create a second version")
	})

	t.Run("editing a skill does not rewrite its history", func(t *testing.T) {
		s := seed(t)
		require.NoError(t, s.AddVersion(ctx, SkillVersion{
			Namespace: "team-a", Skill: "summarise", Digest: Digest([]byte("v1")),
			Source: SourceUpload, ObjectKey: "uploads/v1",
		}))
		require.NoError(t, s.UpsertSkill(ctx, Skill{
			Namespace: "team-a", Name: "summarise", Description: "A new description.",
		}))

		got, err := s.ListVersions(ctx, "team-a", "summarise")
		require.NoError(t, err)
		require.Len(t, got, 1, "metadata edits must not touch the append-only version history")

		sk, ok, err := s.GetSkill(ctx, "team-a", "summarise")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "A new description.", sk.Description)
	})

	t.Run("latest resolves to the newest version and is not settable", func(t *testing.T) {
		s := seed(t)
		for _, body := range []string{"v1", "v2"} {
			require.NoError(t, s.AddVersion(ctx, SkillVersion{
				Namespace: "team-a", Skill: "summarise", Digest: Digest([]byte(body)),
				Source: SourceUpload, ObjectKey: "uploads/" + body,
			}))
			time.Sleep(2 * time.Millisecond)
		}
		d, ok, err := s.ResolveAlias(ctx, "team-a", "summarise", "latest")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, Digest([]byte("v2")), d)

		// `latest` is DERIVED. Letting it be pinned would create two different meanings for one
		// name — the pinned one and the newest one — which is exactly the ambiguity the
		// resolve-at-deploy-time rule exists to remove.
		assert.Error(t, s.SetAlias(ctx, "team-a", "summarise", "latest", d))
	})

	t.Run("an alias cannot point at a digest that does not exist", func(t *testing.T) {
		s := seed(t)
		err := s.SetAlias(ctx, "team-a", "summarise", "stable", Digest([]byte("never-added")))
		require.Error(t, err, "a dangling alias would resolve to nothing at deploy time")
	})

	t.Run("an alias moves, and resolving it yields the new digest", func(t *testing.T) {
		s := seed(t)
		for _, body := range []string{"v1", "v2"} {
			require.NoError(t, s.AddVersion(ctx, SkillVersion{
				Namespace: "team-a", Skill: "summarise", Digest: Digest([]byte(body)),
				Source: SourceUpload, ObjectKey: "uploads/" + body,
			}))
		}
		require.NoError(t, s.SetAlias(ctx, "team-a", "summarise", "stable", Digest([]byte("v1"))))
		d, ok, err := s.ResolveAlias(ctx, "team-a", "summarise", "stable")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, Digest([]byte("v1")), d)

		require.NoError(t, s.SetAlias(ctx, "team-a", "summarise", "stable", Digest([]byte("v2"))))
		d, _, err = s.ResolveAlias(ctx, "team-a", "summarise", "stable")
		require.NoError(t, err)
		assert.Equal(t, Digest([]byte("v2")), d,
			"an alias is the one mutable part; the deployment records the digest it resolved to")
	})

	t.Run("a version cannot be added to a skill that does not exist", func(t *testing.T) {
		s := newStore(t)
		err := s.AddVersion(ctx, SkillVersion{
			Namespace: "team-a", Skill: "ghost", Digest: Digest([]byte("v1")),
			Source: SourceUpload, ObjectKey: "uploads/v1",
		})
		require.Error(t, err)
	})

	t.Run("deleting a skill takes its versions with it", func(t *testing.T) {
		s := seed(t)
		require.NoError(t, s.AddVersion(ctx, SkillVersion{
			Namespace: "team-a", Skill: "summarise", Digest: Digest([]byte("v1")),
			Source: SourceUpload, ObjectKey: "uploads/v1",
		}))
		require.NoError(t, s.DeleteSkill(ctx, "team-a", "summarise"))

		vs, err := s.ListVersions(ctx, "team-a", "summarise")
		require.NoError(t, err)
		assert.Empty(t, vs, "orphaned versions would resolve for a skill nobody can see")
	})

	t.Run("namespaces do not leak into each other", func(t *testing.T) {
		s := seed(t)
		require.NoError(t, s.UpsertSkill(ctx, Skill{Namespace: "team-b", Name: "other"}))
		got, err := s.ListSkills(ctx, "team-a")
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "summarise", got[0].Name)
	})
}

// Both implementations run the same contract. The twin always runs; Postgres runs only when
// CONTROLPLANE_TEST_DSN points at a throwaway database — the pattern agentmemory and
// promptversion already use. A behaviour that holds only in the twin is a test failure here
// rather than a surprise in production.
func TestStoreConformance(t *testing.T) {
	t.Run("mem", func(t *testing.T) {
		t.Parallel()
		runConformance(t, func(*testing.T) Store { return NewMemStore() })
	})

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres conformance run (the twin still ran)")
		return
	}
	t.Run("postgres", func(t *testing.T) {
		db, err := controlplane.OpenDB(context.Background(), dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		_, err = db.Exec(`TRUNCATE skills CASCADE`)
		require.NoError(t, err)
		runConformance(t, func(*testing.T) Store { return NewPostgresStore(db) })
	})
}
