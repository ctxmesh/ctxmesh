package skill

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gitVersion() SkillVersion {
	return SkillVersion{
		Namespace: "team-a", Skill: "summarise", Digest: Digest([]byte("body")),
		Source: SourceGit,
		Repo:   "https://github.com/x/skills.git",
		Ref:    "0123456789abcdef0123456789abcdef01234567",
		Path:   "skills/summarise/SKILL.md",
	}
}

// TestABranchIsNotAVersion is the rule the whole design rests on. A ref that can move means the
// content behind a "version" can change underneath a deployment — and this product's replay
// fixtures and eval deploy gate both assume a pinned artifact, so a moved skill would make a
// green fixture a lie rather than merely a stale one.
func TestABranchIsNotAVersion(t *testing.T) {
	t.Parallel()

	v := gitVersion()
	require.NoError(t, ValidateVersion(v), "a full SHA is an immutable pin")

	v.Ref = "refs/tags/v1.2.0"
	require.NoError(t, ValidateVersion(v), "an explicit tag ref is pinned")

	for _, branchy := range []string{"main", "master", "release", "refs/heads/main", "v1.2.0"} {
		v.Ref = branchy
		err := ValidateVersion(v)
		require.Errorf(t, err, "ref %q must be refused", branchy)
		assert.Contains(t, err.Error(), "not an immutable pin")
	}
}

// TestAVersionNamesExactlyOneSource. A half-specified source is a row that resolves to nothing
// and fails at the worst moment — when an agent rolls and needs the content.
func TestAVersionNamesExactlyOneSource(t *testing.T) {
	t.Parallel()

	mixed := gitVersion()
	mixed.ObjectKey = "uploads/abc"
	assert.Error(t, ValidateVersion(mixed), "a version must not carry both a git pin and an upload")

	upload := SkillVersion{
		Namespace: "team-a", Skill: "summarise", Digest: Digest([]byte("b")),
		Source: SourceUpload, ObjectKey: "uploads/abc",
	}
	require.NoError(t, ValidateVersion(upload))

	upload.ObjectKey = ""
	assert.Error(t, ValidateVersion(upload), "an upload with no object key resolves to nothing")

	incomplete := gitVersion()
	incomplete.Path = ""
	assert.Error(t, ValidateVersion(incomplete))
}

// TestDigestIsContentIdentity. Same bytes ⇒ same version, whichever source produced them. That
// is what lets a re-add be an idempotent no-op rather than forking history, and what stops two
// users uploading identical content from creating two "versions" of the same thing.
func TestDigestIsContentIdentity(t *testing.T) {
	t.Parallel()

	assert.Equal(t, Digest([]byte("same")), Digest([]byte("same")))
	assert.NotEqual(t, Digest([]byte("same")), Digest([]byte("different")))
	assert.True(t, strings.HasPrefix(Digest([]byte("x")), "sha256:"),
		"the algorithm is part of the identity, so it must be readable in the digest")
}

// TestDescriptionIsCapped. The description is ALWAYS in context — progressive disclosure exists
// because context is scarce, so an unbounded one silently taxes every run of every agent that
// attaches the skill.
func TestDescriptionIsCapped(t *testing.T) {
	t.Parallel()

	s := Skill{Namespace: "team-a", Name: "summarise", Description: strings.Repeat("x", MaxDescriptionBytes)}
	require.NoError(t, ValidateSkill(s))

	s.Description = strings.Repeat("x", MaxDescriptionBytes+1)
	err := ValidateSkill(s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "every agent's context")
}

func TestSkillNameShape(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateSkill(Skill{Namespace: "n", Name: "pdf-summarise"}))
	for _, bad := range []string{"", "Upper", "trailing-", "has_underscore", strings.Repeat("a", 64)} {
		assert.Errorf(t, ValidateSkill(Skill{Namespace: "n", Name: bad}), "name %q must be refused", bad)
	}
}
