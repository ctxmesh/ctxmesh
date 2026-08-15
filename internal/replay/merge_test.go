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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestMergeFixtures_UnionsChannels proves MergeFixtures unions the Model + Tools channels from a
// run's separate partial blobs (the gateway wrote Model, the sidecar wrote Tools) into one logical
// fixture, re-indexing the model channel and preserving provenance from the first blob.
func TestMergeFixtures_UnionsChannels(t *testing.T) {
	modelBlob := NewFixture("run-7", "team/planner")
	modelBlob.AppendModel([]byte(`{"m":1}`), []byte(`{"ok":1}`), "application/json", 200)
	modelBlob.AppendModel([]byte(`{"m":2}`), []byte(`{"ok":2}`), "application/json", 200)

	toolBlob := NewFixture("run-7", "team/planner")
	toolBlob.AppendTool("c1", "search", []byte(`{"q":"a"}`), []byte(`{"r":1}`))

	merged := MergeFixtures(modelBlob, toolBlob)

	if len(merged.Model) != 2 {
		t.Fatalf("merged model len = %d, want 2", len(merged.Model))
	}
	if len(merged.Tools) != 1 {
		t.Fatalf("merged tools len = %d, want 1", len(merged.Tools))
	}
	if merged.RunID != "run-7" || merged.Agent != "team/planner" {
		t.Errorf("provenance not carried: runId=%q agent=%q", merged.RunID, merged.Agent)
	}
	// Model indices are re-assigned contiguously so the index matcher stays correct.
	for i, m := range merged.Model {
		if m.Index != i {
			t.Errorf("merged model[%d].Index = %d, want %d", i, m.Index, i)
		}
	}
}

// TestMergeFixtures_ReindexesAcrossSplitModelBlobs proves the model channel is re-indexed 0..n-1
// even when it was split across two blobs (total-order-independent merge).
func TestMergeFixtures_ReindexesAcrossSplitModelBlobs(t *testing.T) {
	a := NewFixture("run-1", "a")
	a.AppendModel([]byte(`{"m":1}`), []byte(`{"ok":1}`), "application/json", 200)
	b := NewFixture("run-1", "a")
	b.AppendModel([]byte(`{"m":2}`), []byte(`{"ok":2}`), "application/json", 200)

	merged := MergeFixtures(a, b)
	if len(merged.Model) != 2 || merged.Model[0].Index != 0 || merged.Model[1].Index != 1 {
		t.Fatalf("split model blobs not re-indexed contiguously: %+v", merged.Model)
	}
}

// TestMergeFixtures_Empty returns a valid empty fixture for no inputs.
func TestMergeFixtures_Empty(t *testing.T) {
	merged := MergeFixtures()
	if merged.SchemaVersion != SchemaVersion || merged.Model == nil || merged.Tools == nil {
		t.Fatalf("empty merge should return a valid empty fixture: %+v", merged)
	}
}

// TestLoadFixturePath_SingleFile loads + validates a single merged fixture JSON file.
func TestLoadFixturePath_SingleFile(t *testing.T) {
	dir := t.TempDir()
	f := NewFixture("run-1", "a")
	f.AppendModel([]byte(`{"m":1}`), []byte(`{"ok":1}`), "application/json", 200)
	path := filepath.Join(dir, "fixture.json")
	writeFixture(t, path, f)

	got, err := LoadFixturePath(path)
	if err != nil {
		t.Fatalf("LoadFixturePath(file): %v", err)
	}
	if len(got.Model) != 1 {
		t.Errorf("loaded model len = %d, want 1", len(got.Model))
	}
}

// TestLoadFixturePath_DirectoryMerges reads a directory of partial *.json blobs and merges them.
func TestLoadFixturePath_DirectoryMerges(t *testing.T) {
	dir := t.TempDir()

	modelBlob := NewFixture("run-9", "a")
	modelBlob.AppendModel([]byte(`{"m":1}`), []byte(`{"ok":1}`), "application/json", 200)
	writeFixture(t, filepath.Join(dir, "model.json"), modelBlob)

	toolBlob := NewFixture("run-9", "a")
	toolBlob.AppendTool("c1", "search", []byte(`{"q":"x"}`), []byte(`{"r":1}`))
	writeFixture(t, filepath.Join(dir, "tools.json"), toolBlob)

	got, err := LoadFixturePath(dir)
	if err != nil {
		t.Fatalf("LoadFixturePath(dir): %v", err)
	}
	if len(got.Model) != 1 || len(got.Tools) != 1 {
		t.Errorf("merged dir fixture = %d model, %d tools; want 1,1", len(got.Model), len(got.Tools))
	}
}

// TestLoadFixturePath_RejectsCredentialLeak proves the C4 no-token invariant is enforced at load:
// a fixture that leaked an Authorization header fails LOUDLY rather than silently replaying it.
func TestLoadFixturePath_RejectsCredentialLeak(t *testing.T) {
	dir := t.TempDir()
	f := NewFixture("run-1", "a")
	// A response body that leads with an Authorization header line trips AssertNoCredentials.
	f.AppendModel([]byte(`{"m":1}`), []byte("Authorization: Bearer sk-leak\n\n{}"), "application/json", 200)
	path := filepath.Join(dir, "leaky.json")
	writeFixture(t, path, f)

	if _, err := LoadFixturePath(path); err == nil {
		t.Fatalf("LoadFixturePath must reject a fixture carrying a credential header")
	}
}

// TestLoadFixturePath_EmptyDir errors clearly when a directory has no *.json blobs.
func TestLoadFixturePath_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadFixturePath(dir); err == nil {
		t.Fatalf("LoadFixturePath must error on an empty fixture directory")
	}
}

func writeFixture(t *testing.T, path string, f *Fixture) {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
