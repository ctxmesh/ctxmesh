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
	"fmt"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
)

// FixtureResolver is the deterministic, OFFLINE Resolver used in dev / envtest /
// e2e. It never touches the network — the dev/CI environment has no live git
// remote — yet it exercises the full prompt-only-deploy path: a swap of the
// pointer's ref yields a DIFFERENT prompt (so a new revision rolls) while the
// image is untouched (so the image digest is unchanged), exactly what the e2e
// asserts.
//
// Resolution is two-tier:
//
//  1. Seed table: an exact (repo, ref, path) key mapped to fixed content, for a
//     named fixture whose text the test wants to pin.
//  2. Deterministic fallback: any pointer NOT in the table resolves to content
//     derived FROM the pointer itself. This means the e2e can swap only the ref
//     and reliably get different content with zero seeding — the property the
//     prompt-swap assertion needs — while resolution stays reproducible (the same
//     pointer always yields the same content).
//
// The empty ref/path sentinel is treated as "does not resolve" (ErrNotFound) so a
// bad user pointer surfaces as a status error rather than silently serving junk.
//
// PHASE NOTE (real resolve): a production go-git Resolver — clone/fetch the repo
// at ref, read path — is a drop-in future impl of the same Resolver interface. It
// is deliberately NOT built in v1: it needs a reachable remote (or an in-cluster
// git mirror), which the offline dev/CI posture (ADR 0004, mock-first) does not
// provide. When it lands it replaces FixtureResolver at the one construction site
// in cmd/main.go; the controller, digest, and launcher wiring are unchanged.
type FixtureResolver struct {
	// seed maps a fully-qualified pointer key to fixed content. Optional; an
	// unseeded pointer uses the deterministic fallback.
	seed map[string]string
	// notFound, when non-empty, is a set of pointer keys that explicitly resolve to
	// ErrNotFound (to exercise the bad-ref failure path deterministically).
	notFound map[string]struct{}
}

// NewFixtureResolver returns a FixtureResolver with an empty seed table (every
// pointer uses the deterministic fallback). This is the default construction the
// controller uses in dev/CI.
func NewFixtureResolver() *FixtureResolver {
	return &FixtureResolver{
		seed:     map[string]string{},
		notFound: map[string]struct{}{},
	}
}

// Seed pins fixed content for an exact (repo, ref, path) pointer. Chainable.
// Used by tests that assert on specific prompt text.
func (f *FixtureResolver) Seed(src agentsv1alpha1.GitPromptSource, content string) *FixtureResolver {
	f.seed[key(src)] = content
	return f
}

// SeedNotFound marks an exact pointer as unresolvable (Resolve returns
// ErrNotFound) so the bad-ref → status-error path can be exercised offline.
// Chainable.
func (f *FixtureResolver) SeedNotFound(src agentsv1alpha1.GitPromptSource) *FixtureResolver {
	f.notFound[key(src)] = struct{}{}
	return f
}

// Resolve implements Resolver. It is pure and deterministic: no I/O, no clock, no
// randomness — the same pointer always yields the same Resolved.
func (f *FixtureResolver) Resolve(_ context.Context, src agentsv1alpha1.GitPromptSource) (Resolved, error) {
	// A pointer with no ref or no path can never address a concrete artifact →
	// treat as unresolved rather than fabricating content. (The CRD requires both
	// non-empty; this guards a hand-built spec.)
	if src.Ref == "" || src.Path == "" {
		return Resolved{}, fmt.Errorf("%w: repo=%q ref=%q path=%q", ErrNotFound, src.Repo, src.Ref, src.Path)
	}
	if _, bad := f.notFound[key(src)]; bad {
		return Resolved{}, fmt.Errorf("%w: repo=%q ref=%q path=%q", ErrNotFound, src.Repo, src.Ref, src.Path)
	}

	content, ok := f.seed[key(src)]
	if !ok {
		// Deterministic fallback: derive content from the pointer so a ref swap
		// changes the content (and thus the version + revision digest) without any
		// seeding. Human-readable so the e2e can grep it out of the served prompt.
		content = fmt.Sprintf(
			"PROMPT[repo=%s ref=%s path=%s]\nYou are the agent configured by git-pointer prompt %s@%s.\n",
			src.Repo, src.Ref, src.Path, src.Path, src.Ref)
	}

	return Resolved{
		Content: content,
		Version: Version(src, content),
	}, nil
}

// key is the fully-qualified pointer identity used for the seed / not-found maps.
func key(src agentsv1alpha1.GitPromptSource) string {
	return src.Repo + "\x00" + src.Ref + "\x00" + src.Path
}
