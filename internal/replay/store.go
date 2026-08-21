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

package replay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/ctxmesh/agent-engine/internal/objectstore"
)

// ErrNoFixture is returned (wrapped) by GetRun when a run has NO recorded fixture blobs — nothing was
// recorded for it, or the run id is wrong. A caller (the BFF stepper endpoint) uses errors.Is to tell
// this "not recorded" case (an honest empty result) apart from a real object-store I/O failure.
var ErrNoFixture = errors.New("replay: no fixture recorded for this run")

// FixtureStore assembles a recorded Fixture into the DURABLE object store and reads it back (ADR
// 0071 §2). It reuses the durable KB object-store SPI (internal/objectstore.ObjectStore — the
// never-GC'd durable store, NOT the launcher's ephemeral/GC'd A2A blob store): a fixture is a
// durable, shareable artifact, not an in-flight payload, so the durable store is the right sink and
// mirrors the objectstore precedent (same OBJECT_STORE_ADDR gate via objectstore.NewMinioStore).
//
// Keys live under a fixtures/ prefix, run-keyed for a stable, human-findable location plus a content
// digest so a re-Put of the same bytes is idempotent (identical content ⇒ identical key). A fixture
// is SENSITIVE-BY-DEFAULT (full prompts + tool results, ADR 0071 C4); read access is CALLER-SCOPED
// (ADR 0011) — the caller passes the object store obtained through their own scope, and the BFF that
// serves a fixture gates the caller's RBAC before handing them a ref. This store performs the blob
// I/O only; it never mints or holds a credential.
type FixtureStore struct {
	store objectstore.ObjectStore
}

// fixturesKeyPrefix is the durable-store key namespace all fixtures share. Distinct from the KB
// documents' "knowledge/" prefix so fixtures and KB corpora coexist in the same bucket without a
// prefix collision. Full key: fixtures/{runId}/{contentDigest}.json
const fixturesKeyPrefix = "fixtures"

// fixtureContentType is the object Content-Type stored with a fixture blob.
const fixtureContentType = "application/json"

// NewFixtureStore wraps a durable ObjectStore. store must be non-nil — a caller that has no
// configured object store (OBJECT_STORE_ADDR unset ⇒ objectstore.NewMinioStore returns nil) must
// NOT construct a FixtureStore and instead surface an honest "fixtures unavailable" (mirrors the KB
// upload path's 501-on-unconfigured posture). Returns an error on a nil store so the misuse is loud.
func NewFixtureStore(store objectstore.ObjectStore) (*FixtureStore, error) {
	if store == nil {
		return nil, fmt.Errorf("replay: fixture store requires a configured object store (OBJECT_STORE_ADDR unset?)")
	}
	return &FixtureStore{store: store}, nil
}

// Put marshals the fixture, enforces the no-credential invariant (C4 — a fixture that somehow
// carries a credential is REFUSED, never persisted), writes it to the durable object store under a
// run-keyed, content-addressed key, and returns the object-store ref ("fixtures/{runId}/{digest}.json").
// The ref is the handle the run's `step` metadata event / the BFF hand back; Get resolves it.
//
// Content-addressing makes the write idempotent: recording the SAME run twice (a reclaim re-running
// capture) writes the same bytes to the same key rather than duplicating. Fails typed on a marshal
// error, a credential leak, or a store write error — never a half-written fixture.
func (s *FixtureStore) Put(ctx context.Context, f *Fixture) (string, error) {
	if f == nil {
		return "", fmt.Errorf("replay: put nil fixture")
	}
	if err := f.AssertNoCredentials(); err != nil {
		// Refuse to persist a fixture that failed the no-token invariant. A leaked credential in a
		// shareable artifact is an incident (the non-negotiables) — fail closed, do not write.
		return "", fmt.Errorf("replay: refusing to store fixture: %w", err)
	}
	data, err := f.MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("replay: marshal fixture for run %q: %w", f.RunID, err)
	}
	key := fixtureKey(f.RunID, data)
	if err := s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), fixtureContentType); err != nil {
		return "", fmt.Errorf("replay: store fixture for run %q: %w", f.RunID, err)
	}
	return key, nil
}

// Get resolves a fixture ref (an object-store key returned by Put) back into a Fixture, enforcing
// the schema-version contract on load (UnmarshalFixture rejects a newer-than-supported version) and
// the no-credential invariant on read (a stored fixture that carries a credential is a bug — surface
// it rather than hand a leaked token to a caller). The caller MUST have already gated its own RBAC
// (caller-scoped read, ADR 0011) — this method does the blob read only.
func (s *FixtureStore) Get(ctx context.Context, ref string) (*Fixture, error) {
	rc, err := s.store.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("replay: fetch fixture %q: %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("replay: read fixture %q: %w", ref, err)
	}
	f, err := UnmarshalFixture(data)
	if err != nil {
		return nil, err
	}
	if credErr := f.AssertNoCredentials(); credErr != nil {
		return nil, fmt.Errorf("replay: stored fixture %q failed the no-credential invariant on read: %w", ref, credErr)
	}
	return f, nil
}

// fixtureKey builds the durable-store key for a fixture: fixtures/{runId}/{contentDigest}.json. The
// content digest (hex sha256 of the marshaled bytes) makes the key content-addressed WITHIN the run
// namespace, so a re-Put of identical bytes is idempotent while distinct recordings of the same run
// (should not normally happen) do not clobber each other. runId is path-sanitized defensively so a
// malformed id cannot traverse outside the fixtures/ prefix.
func fixtureKey(runID string, data []byte) string {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	return path.Join(fixturesKeyPrefix, SanitizeRunID(runID), digest) + ".json"
}

// SanitizeRunID reduces a run id to a single safe path segment (defensive against traversal),
// matching what fixtureKey stamps into the store key. GetRun lists a run's prefix with the SAME
// sanitization so a LIST finds exactly what Put wrote, and the download-fixture CLI reuses it to
// derive a filesystem-safe default output filename. A run id that sanitizes to nothing maps to the
// same "_unkeyed" bucket the key builder uses.
func SanitizeRunID(runID string) string {
	safe := path.Base(path.Clean("/" + runID))
	if safe == "." || safe == "/" || safe == "" {
		safe = "_unkeyed"
	}
	return safe
}

// fixtureRunPrefix is the durable-store key prefix under which ALL of a run's partial fixture blobs
// live: fixtures/{runId}/. Listing it enumerates every channel blob (the launcher gateway's MODEL
// blob + the egress sidecar's TOOLS blob, ADR 0071 §3a) a run recorded — what GetRun merges into
// one replayable fixture. The trailing slash makes the prefix exclusive to this run's subtree so a
// run id that is a prefix of another's does not over-match.
func fixtureRunPrefix(runID string) string {
	return path.Join(fixturesKeyPrefix, SanitizeRunID(runID)) + "/"
}

// GetRun downloads and merges ALL partial fixture blobs a run recorded into one replayable fixture
// (the load-side of ADR 0071 §3a assembly, off the durable object store instead of a local dir).
// It lists the run's fixtures/{runId}/ prefix, Gets + validates each *.json blob (Get enforces the
// schema-version gate and the C4 no-credential invariant), and MergeFixtures them in a deterministic
// key order so a re-download is byte-stable. A run with no fixture blobs is an honest error (nothing
// was recorded, or the run id is wrong) — never an empty fixture. The caller MUST have gated its own
// RBAC before calling (caller-scoped read, ADR 0011); this does the blob I/O only.
func (s *FixtureStore) GetRun(ctx context.Context, runID string) (*Fixture, error) {
	prefix := fixtureRunPrefix(runID)
	infos, err := s.store.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("replay: list fixtures for run %q: %w", runID, err)
	}
	keys := make([]string, 0, len(infos))
	for _, in := range infos {
		if strings.HasSuffix(in.Key, ".json") {
			keys = append(keys, in.Key)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf(
			"replay: no fixture blobs for run %q (object-store prefix %q): %w", runID, prefix, ErrNoFixture)
	}
	// Deterministic order so a re-download is byte-stable (the model channel is re-indexed on merge).
	slices.Sort(keys)

	blobs := make([]*Fixture, 0, len(keys))
	for _, k := range keys {
		fx, gerr := s.Get(ctx, k)
		if gerr != nil {
			return nil, gerr
		}
		blobs = append(blobs, fx)
	}
	merged := MergeFixtures(blobs...)
	// Re-assert the invariant on the merged whole (belt-and-braces; each blob was already checked).
	if err := merged.AssertNoCredentials(); err != nil {
		return nil, err
	}
	return merged, nil
}
