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

package prompt

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// FixtureResolver satisfies the Resolver interface — the seam the controller
// depends on. A compile-time assertion so a future go-git impl (or a signature
// drift) is caught here.
var _ Resolver = (*FixtureResolver)(nil)

func src(ref string) agentsv1alpha1.GitPromptSource {
	return agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  ref,
		Path: "agents/echo/system.txt",
	}
}

// TestFixtureResolve_Deterministic: the same pointer always resolves to the same
// content and version — no I/O, no clock, no randomness.
func TestFixtureResolve_Deterministic(t *testing.T) {
	r := NewFixtureResolver()
	ctx := context.Background()

	a, err := r.Resolve(ctx, src("v1"))
	require.NoError(t, err)
	b, err := r.Resolve(ctx, src("v1"))
	require.NoError(t, err)

	assert.Equal(t, a.Content, b.Content, "same pointer → same content")
	assert.Equal(t, a.Version, b.Version, "same pointer → same version")
	assert.NotEmpty(t, a.Content)
	assert.NotEmpty(t, a.Version)
}

// TestFixtureResolve_RefSwapChangesPrompt: the property the prompt-only-deploy
// e2e leans on — swapping ONLY the ref yields DIFFERENT content and a DIFFERENT
// version, with zero seeding, offline.
func TestFixtureResolve_RefSwapChangesPrompt(t *testing.T) {
	r := NewFixtureResolver()
	ctx := context.Background()

	v1, err := r.Resolve(ctx, src("v1"))
	require.NoError(t, err)
	v2, err := r.Resolve(ctx, src("v2"))
	require.NoError(t, err)

	assert.NotEqual(t, v1.Content, v2.Content, "a ref swap must change the prompt content")
	assert.NotEqual(t, v1.Version, v2.Version, "a ref swap must change the prompt version")
}

// TestFixtureResolve_Seeded: a seeded pointer returns its pinned content, and its
// version still folds that content (so identical content at different pointers
// differs by pointer).
func TestFixtureResolve_Seeded(t *testing.T) {
	r := NewFixtureResolver().Seed(src("pinned"), "You are a pinned prompt.")
	ctx := context.Background()

	got, err := r.Resolve(ctx, src("pinned"))
	require.NoError(t, err)
	assert.Equal(t, "You are a pinned prompt.", got.Content)
	assert.Equal(t, Version(src("pinned"), "You are a pinned prompt."), got.Version)
}

// TestFixtureResolve_BadPointer: an empty ref/path, or an explicitly-seeded
// not-found pointer, resolves to ErrNotFound (the bad-ref → status-error path)
// rather than fabricated content.
func TestFixtureResolve_BadPointer(t *testing.T) {
	r := NewFixtureResolver().SeedNotFound(src("gone"))
	ctx := context.Background()

	_, err := r.Resolve(ctx, src("gone"))
	assert.ErrorIs(t, err, ErrNotFound, "explicit not-found pointer")

	_, err = r.Resolve(ctx, agentsv1alpha1.GitPromptSource{Repo: "r", Ref: "", Path: "p"})
	assert.ErrorIs(t, err, ErrNotFound, "empty ref never resolves")

	_, err = r.Resolve(ctx, agentsv1alpha1.GitPromptSource{Repo: "r", Ref: "x", Path: ""})
	assert.ErrorIs(t, err, ErrNotFound, "empty path never resolves")
}

// TestVersion_FoldsPointerAndContent: Version changes if ANY of repo/ref/path/
// content changes — so a same-content prompt at a different ref still versions
// distinctly, and a content edit at the same ref versions distinctly.
func TestVersion_FoldsPointerAndContent(t *testing.T) {
	base := Version(src("v1"), "hello")
	assert.NotEqual(t, base, Version(src("v2"), "hello"), "ref change → version change")
	assert.NotEqual(t, base, Version(src("v1"), "world"), "content change → version change")
	assert.Equal(t, base, Version(src("v1"), "hello"), "deterministic")

	// sanity: not the zero value, 12 hex chars
	assert.Len(t, base, 12)
	assert.NotEqual(t, "", base)
	assert.False(t, errors.Is(nil, ErrNotFound)) // guard: ErrNotFound is a real sentinel
}
