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
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// MergeFixtures unions the per-channel interactions of several PARTIAL fixture blobs into one
// logical fixture (ADR 0071 §3a). Capture writes two blobs per run — the launcher gateway writes
// the MODEL channel, the egress sidecar writes the TOOLS channel — so a run's fixture is
// assembled by merging them. This is the load-side counterpart of the object-store assembly
// (store.go): `dev --replay` may point at either a single already-merged fixture OR a directory
// of partial blobs it reads + merges here.
//
// Merge rules:
//   - Model interactions are concatenated in blob order and their Index is RE-ASSIGNED to the
//     merged position (0..n-1), so the index-matcher stays correct regardless of how the model
//     channel was split across blobs. (A single blob normally carries the whole model channel;
//     re-indexing is defensive + makes the merge total-order-independent.)
//   - Tool interactions are concatenated (order is not load-bearing — MatchTool is by
//     id/name+args, ADR 0071 §2).
//   - Provenance (RunID/Agent/RecordedAt) is taken from the first blob that carries it.
//
// It returns a fresh Fixture at the current SchemaVersion; inputs are not mutated. Passing no
// fixtures returns an empty (but valid) fixture.
func MergeFixtures(fixtures ...*Fixture) *Fixture {
	out := &Fixture{
		SchemaVersion: SchemaVersion,
		Model:         []ModelInteraction{},
		Tools:         []ToolInteraction{},
	}
	for _, f := range fixtures {
		if f == nil {
			continue
		}
		if out.RunID == "" {
			out.RunID = f.RunID
		}
		if out.Agent == "" {
			out.Agent = f.Agent
		}
		if out.RecordedAt.IsZero() {
			out.RecordedAt = f.RecordedAt
		}
		out.Model = append(out.Model, f.Model...)
		out.Tools = append(out.Tools, f.Tools...)
	}
	// Re-assign contiguous model indices so the primary matcher is correct on the merged channel.
	for i := range out.Model {
		out.Model[i].Index = i
	}
	return out
}

// LoadFixturePath loads a fixture from a filesystem path: a single *.json file is decoded (and
// validated) directly; a DIRECTORY is read as a set of *.json partial blobs, each decoded +
// validated, then MergeFixtures'd into one. This is what `dev --replay <path>` and
// `replay-serve <path>` call. Object-store listing/download is out of scope (m78.6) — this works
// from local file(s) only.
//
// Every blob is run through UnmarshalFixture (schema-version gate) AND AssertNoCredentials (the
// C4 no-token invariant) so a fixture that leaked a credential fails LOUDLY at load, never
// silently replays one.
func LoadFixturePath(path string) (*Fixture, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("replay: stat fixture path %q: %w", path, err)
	}
	if !info.IsDir() {
		return loadOneFixture(path)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("replay: read fixture dir %q: %w", path, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) == ".json" {
			files = append(files, filepath.Join(path, e.Name()))
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("replay: no *.json fixture blobs in directory %q", path)
	}
	// Deterministic order so re-runs are byte-stable (the model channel is re-indexed on merge).
	slices.Sort(files)

	var blobs []*Fixture
	for _, f := range files {
		fx, err := loadOneFixture(f)
		if err != nil {
			return nil, err
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

// loadOneFixture reads + decodes + validates a single fixture blob file.
func loadOneFixture(file string) (*Fixture, error) {
	data, err := os.ReadFile(file) //nolint:gosec // operator-supplied local fixture path (a CLI arg)
	if err != nil {
		return nil, fmt.Errorf("replay: read fixture %q: %w", file, err)
	}
	fx, err := UnmarshalFixture(data)
	if err != nil {
		return nil, fmt.Errorf("replay: %q: %w", file, err)
	}
	if err := fx.AssertNoCredentials(); err != nil {
		return nil, fmt.Errorf("replay: %q: %w", file, err)
	}
	return fx, nil
}
