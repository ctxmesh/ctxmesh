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
	"fmt"
	"io"
	"path"

	"github.com/ctxmesh/agent-engine/internal/objectstore"
)

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
	safeRun := path.Base(path.Clean("/" + runID))
	if safeRun == "." || safeRun == "/" || safeRun == "" {
		safeRun = "_unkeyed"
	}
	return path.Join(fixturesKeyPrefix, safeRun, digest) + ".json"
}
