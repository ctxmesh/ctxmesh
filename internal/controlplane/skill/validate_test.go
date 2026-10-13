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
	mixed.ObjectKey = "skills/team-a/abc"
	assert.Error(t, ValidateVersion(mixed), "a version must not carry both a git pin and an upload")

	upload := SkillVersion{
		Namespace: "team-a", Skill: "summarise", Digest: Digest([]byte("b")),
		Source: SourceUpload, ObjectKey: "skills/team-a/abc",
	}
	require.NoError(t, ValidateVersion(upload))

	upload.ObjectKey = ""
	assert.Error(t, ValidateVersion(upload), "an upload with no object key resolves to nothing")

	incomplete := gitVersion()
	incomplete.Path = ""
	assert.Error(t, ValidateVersion(incomplete))
}

// TestAnUploadKeyStaysInItsNamespace. The controller reads uploaded bodies with the platform's
// store credential, and the store is shared by every namespace, so a key is accepted only under
// its own namespace's prefix — otherwise a skill version could name another namespace's object
// and have the controller mount it into this namespace's agent.
func TestAnUploadKeyStaysInItsNamespace(t *testing.T) {
	t.Parallel()

	v := SkillVersion{
		Namespace: "team-a", Skill: "summarise", Digest: Digest([]byte("b")),
		Source: SourceUpload, ObjectKey: "skills/team-a/summarise/SKILL.md",
	}
	require.NoError(t, ValidateVersion(v))

	for _, foreign := range []string{
		"skills/team-b/summarise/SKILL.md",
		"knowledge/team-b/kb/doc.pdf",
		"skills/team-a/../team-b/SKILL.md",
		"skills/team-a/./SKILL.md",
		"skills/team-a//SKILL.md",
		"skills/team-a/",
		"skills/team-ab/SKILL.md",
		"/skills/team-a/SKILL.md",
		"skills/team-a/a b.md",
		"skills/team-a/%2e%2e/team-b/x",
		"skills/team-a/x?y",
		"skills/team-a/" + strings.Repeat("k", MaxObjectKeyLength),
	} {
		v.ObjectKey = foreign
		err := ValidateVersion(v)
		require.Errorf(t, err, "key %q must be refused for namespace team-a", foreign)
		assert.Contains(t, err.Error(), "skills/team-a/")
	}
}

// TestAVersionsPointersAreBoundedAndClean. Every pointer is echoed into the SkillBodies condition of
// each agent attaching the version, and the ref and path become part of the URL the controller
// fetches: a long one could stop status writes, and a '..', '?' or '#' could fetch another file.
func TestAVersionsPointersAreBoundedAndClean(t *testing.T) {
	t.Parallel()

	v := gitVersion()
	require.NoError(t, ValidateVersion(v))

	for name, mutate := range map[string]func(*SkillVersion){
		"short digest":      func(v *SkillVersion) { v.Digest = "sha256:abc" },
		"upper-case digest": func(v *SkillVersion) { v.Digest = "sha256:" + strings.Repeat("A", 64) },
		"other algorithm":   func(v *SkillVersion) { v.Digest = "sha512:" + strings.Repeat("a", 64) },
		"long repo":         func(v *SkillVersion) { v.Repo = "https://github.com/" + strings.Repeat("r", MaxRepoLength) },
		"long path":         func(v *SkillVersion) { v.Path = strings.Repeat("p", MaxPathLength+1) },
		"long tag":          func(v *SkillVersion) { v.Ref = "refs/tags/" + strings.Repeat("t", MaxRefLength) },
		"dot-dot path":      func(v *SkillVersion) { v.Path = "skills/../../other/SKILL.md" },
		"absolute path":     func(v *SkillVersion) { v.Path = "/etc/SKILL.md" },
		"query in path":     func(v *SkillVersion) { v.Path = "SKILL.md?raw=1" },
		"fragment in path":  func(v *SkillVersion) { v.Path = "SKILL.md#x" },
		"escape in path":    func(v *SkillVersion) { v.Path = "%2e%2e/SKILL.md" },
		"newline in path":   func(v *SkillVersion) { v.Path = "SKILL.md\nx" },
		"dot-dot tag":       func(v *SkillVersion) { v.Ref = "refs/tags/../../heads/main" },
	} {
		bad := gitVersion()
		mutate(&bad)
		assert.Errorf(t, ValidateVersion(bad), "%s must be refused", name)
	}
	assert.True(t, ValidDigest(Digest([]byte("x"))), "Digest's own output is canonical")
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
