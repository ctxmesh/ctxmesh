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

// Package prompt resolves a git-pointer PromptVersion (repo, ref, path) into the
// prompt content the launcher serves, behind a swappable interface.
//
// The interface (Resolver) is the same mock⇄real seam the M9 scorer uses: the
// controller depends only on Resolver, so the deploy path is identical whether a
// deterministic fixture backs the resolve (dev / envtest / e2e — no live git
// remote) or a real go-git fetch does (production). v1 ships the fixture-backed
// Resolver so the whole prompt-only-deploy path — resolve, materialise, digest,
// revision roll — is exercised OFFLINE and reproducibly in CI. A real go-git
// Resolver is a drop-in future impl of the same interface (see the phase note on
// FixtureResolver); nothing else in the controller changes when it lands.
package prompt

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// ErrNotFound is returned by a Resolver when the (repo, ref, path) pointer does
// not resolve to any content — a bad git ref or a missing path. The controller
// maps it to a user-facing status error (Ready=False) and keeps the old revision
// serving; it is NOT a transient/infra failure to requeue on.
var ErrNotFound = errors.New("prompt: git pointer does not resolve to any content")

// Resolved is the outcome of resolving a git-pointer prompt: the prompt Content
// the launcher serves, plus a Version — a deterministic, display-only identifier
// for the resolved prompt (surfaced as the prompt.version trace attribute). Git
// stays the source of truth; Version is a stable label, not a store key.
type Resolved struct {
	// Content is the resolved prompt text.
	Content string
	// Version is a deterministic identifier for this resolved prompt. It folds the
	// full pointer (repo, ref, path) AND the content so that a ref swap OR a
	// content change both produce a different Version — the value the launcher
	// surfaces as prompt.version and the controller folds into the revision digest.
	Version string
}

// Resolver turns a git-pointer PromptVersion spec into resolved prompt content.
// It is the single seam between the controller's prompt-only-deploy logic and the
// mechanism that actually fetches a prompt, mirroring the M9 scorer's mock⇄real
// interface: swapping the impl (fixture ⇄ go-git) changes nothing in the deploy
// path.
type Resolver interface {
	// Resolve returns the prompt content and a deterministic version for the given
	// git pointer. It returns ErrNotFound when the pointer does not resolve (bad
	// ref / missing path) so the caller can surface a user error without
	// requeueing, and any other error for a transient/infra failure.
	Resolve(ctx context.Context, src agentsv1alpha1.GitPromptSource) (Resolved, error)
}

// Version computes the deterministic display version for a resolved prompt: the
// first 12 hex chars of sha256 over the full pointer and content. Exposed so any
// Resolver impl (fixture or real) derives the identical prompt.version for the
// same inputs, and so the controller can recompute it for the digest component.
func Version(src agentsv1alpha1.GitPromptSource, content string) string {
	payload := fmt.Sprintf("repo=%s;ref=%s;path=%s;content=%s",
		src.Repo, src.Ref, src.Path, content)
	h := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", h[:])[:12]
}
