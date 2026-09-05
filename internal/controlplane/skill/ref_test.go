package skill

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seeded(t *testing.T) (Store, Resolver) {
	t.Helper()
	ctx := context.Background()
	s := NewMemStore()
	require.NoError(t, s.UpsertSkill(ctx, Skill{Namespace: "team-a", Name: "summarise"}))
	for _, body := range []string{"v1", "v2"} {
		require.NoError(t, s.AddVersion(ctx, SkillVersion{
			Namespace: "team-a", Skill: "summarise", Digest: Digest([]byte(body)),
			Source: SourceUpload, ObjectKey: "uploads/" + body,
		}))
	}
	require.NoError(t, s.SetAlias(ctx, "team-a", "summarise", "stable", Digest([]byte("v1"))))
	return s, NewResolver(s)
}

// TestABareNameIsRefused. A name with no version would have to mean "latest", and an implicit
// floating reference is the thing this design exists to prevent: the skill could change
// underneath a running agent while the spec that produced it looked untouched. Making the user
// write "@latest" keeps the choice visible in the spec and in review.
func TestABareNameIsRefused(t *testing.T) {
	t.Parallel()

	_, err := ParseRef("summarise")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "never implicit")

	r, err := ParseRef("summarise@latest")
	require.NoError(t, err)
	assert.Equal(t, "summarise", r.Name)
	assert.False(t, r.IsDigest(), "an alias is not a digest, however convenient that would be")
}

// TestResolutionPinsAnAliasToADigest — the property the whole reproducibility story rests on.
func TestResolutionPinsAnAliasToADigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, res := seeded(t)

	pinned, err := ResolveAll(ctx, res, "team-a", []string{"summarise@stable"})
	require.NoError(t, err)
	require.Equal(t, []string{"summarise@" + Digest([]byte("v1"))}, pinned,
		"what gets recorded is the digest, never the alias that produced it")

	// Move the alias. Already-resolved refs are unaffected, which is exactly the point: a
	// running agent cannot be changed by re-pointing an alias.
	require.NoError(t, store.SetAlias(ctx, "team-a", "summarise", "stable", Digest([]byte("v2"))))
	assert.Equal(t, []string{"summarise@" + Digest([]byte("v1"))}, pinned)

	// A NEW resolution picks up the move — the alias is still useful, just not load-bearing
	// at runtime.
	again, err := ResolveAll(ctx, res, "team-a", []string{"summarise@stable"})
	require.NoError(t, err)
	assert.Equal(t, []string{"summarise@" + Digest([]byte("v2"))}, again)
}

// TestADigestThatNamesNothingIsRefused. Recording an unknown digest into the AgentVersion
// snapshot would look like content until the agent started and could not find it — a failure
// moved from apply time, where it is cheap, to start time, where it is not.
func TestADigestThatNamesNothingIsRefused(t *testing.T) {
	t.Parallel()
	_, res := seeded(t)

	_, err := ResolveAll(context.Background(), res, "team-a",
		[]string{"summarise@" + Digest([]byte("never-added"))})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no version")
}

// TestResolutionIsAllOrNothing. A partially resolved list would deploy an agent with some
// skills silently missing, which is worse than refusing the spec.
func TestResolutionIsAllOrNothing(t *testing.T) {
	t.Parallel()
	_, res := seeded(t)

	out, err := ResolveAll(context.Background(), res, "team-a",
		[]string{"summarise@stable", "missing@latest"})
	require.Error(t, err)
	assert.Nil(t, out, "no partial list may escape")
}

// TestTheSameSkillTwiceIsRefused — it would inject the description twice and double its
// always-on context cost for nothing.
func TestTheSameSkillTwiceIsRefused(t *testing.T) {
	t.Parallel()
	_, res := seeded(t)

	_, err := ResolveAll(context.Background(), res, "team-a",
		[]string{"summarise@stable", "summarise@latest"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than once")
}

// TestADigestRefResolvesToItself — no store round-trip beyond the existence check, and the
// value is unchanged.
func TestADigestRefResolvesToItself(t *testing.T) {
	t.Parallel()
	_, res := seeded(t)

	d := Digest([]byte("v2"))
	out, err := ResolveAll(context.Background(), res, "team-a", []string{"summarise@" + d})
	require.NoError(t, err)
	assert.Equal(t, []string{"summarise@" + d}, out)
}
