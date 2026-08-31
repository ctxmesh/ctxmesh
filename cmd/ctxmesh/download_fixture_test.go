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

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/replay"
)

// seedRunFixture writes a run's two partial channel blobs (model + tools) into a mem object store
// through a FixtureStore, returning the store the download path reads back from.
func seedRunFixture(t *testing.T, runID string) *replay.FixtureStore {
	t.Helper()
	fs, err := replay.NewFixtureStore(objectstore.NewMemObjectStore())
	if err != nil {
		t.Fatalf("NewFixtureStore: %v", err)
	}
	ctx := context.Background()

	modelBlob := replay.NewFixture(runID, "team/agent")
	modelBlob.AppendModel([]byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		[]byte(`{"choices":[{"message":{"content":"ok"}}]}`), "application/json", 200)
	if _, err := fs.Put(ctx, modelBlob); err != nil {
		t.Fatalf("Put model blob: %v", err)
	}
	toolBlob := replay.NewFixture(runID, "team/agent")
	toolBlob.AppendTool("call_1", "search", []byte(`{"q":"go"}`), []byte(`{"r":1}`), "")
	if _, err := fs.Put(ctx, toolBlob); err != nil {
		t.Fatalf("Put tool blob: %v", err)
	}
	return fs
}

// TestDownloadFixtureToFile_MergesAndIsReplayable proves the download path merges a run's partial
// blobs into a single local fixture file that `dev --replay` can load — the whole point of O8.
func TestDownloadFixtureToFile_MergesAndIsReplayable(t *testing.T) {
	fs := seedRunFixture(t, "run-abc123")
	out := filepath.Join(t.TempDir(), "run.fixture.json")

	var stdout bytes.Buffer
	if err := downloadFixtureToFile(context.Background(), fs, "run-abc123", out, &stdout); err != nil {
		t.Fatalf("downloadFixtureToFile: %v", err)
	}

	// The written file is a valid, merged fixture that the replay loader accepts.
	fx, err := replay.LoadFixturePath(out)
	if err != nil {
		t.Fatalf("written fixture not replayable: %v", err)
	}
	if fx.RunID != "run-abc123" || len(fx.Model) != 1 || len(fx.Tools) != 1 {
		t.Errorf("merged fixture wrong: run=%q %d model %d tool", fx.RunID, len(fx.Model), len(fx.Tools))
	}

	// The file is written 0600 (sensitive-by-default, C4).
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat out: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("fixture file perms = %o, want 0600", perm)
	}

	// The operator gets a copy-pasteable replay hint.
	if !strings.Contains(stdout.String(), "dev --replay "+out) {
		t.Errorf("missing replay hint in output:\n%s", stdout.String())
	}
}

// TestDownloadFixtureToFile_DefaultName proves the default output filename is derived from the run
// id (<run-id>.fixture.json) when -o is not given, written into the current directory.
func TestDownloadFixtureToFile_DefaultName(t *testing.T) {
	fs := seedRunFixture(t, "run-xyz")
	t.Chdir(t.TempDir())

	if err := downloadFixtureToFile(context.Background(), fs, "run-xyz", "", new(bytes.Buffer)); err != nil {
		t.Fatalf("downloadFixtureToFile: %v", err)
	}
	if _, err := os.Stat("run-xyz.fixture.json"); err != nil {
		t.Errorf("default-named fixture not written: %v", err)
	}
}

// TestDownloadFixtureToFile_NoRecordingIsError proves a run with no recorded fixture is a clear,
// non-silent error (and does NOT write an empty file).
func TestDownloadFixtureToFile_NoRecordingIsError(t *testing.T) {
	fs, _ := replay.NewFixtureStore(objectstore.NewMemObjectStore())
	out := filepath.Join(t.TempDir(), "nope.json")

	err := downloadFixtureToFile(context.Background(), fs, "run-missing", out, new(bytes.Buffer))
	if err == nil {
		t.Fatal("expected an error for a run with no recording")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("no file should be written when the run has no recording")
	}
}

// TestRunDownloadFixture_UnconfiguredStore proves an unset OBJECT_STORE_ADDR is a clear validation
// error (exit 1) pointing at the env var, not a panic or an empty fixture.
func TestRunDownloadFixture_UnconfiguredStore(t *testing.T) {
	t.Setenv("OBJECT_STORE_ADDR", "")

	err := runDownloadFixture(context.Background(), "run-abc", "", new(bytes.Buffer))
	if err == nil {
		t.Fatal("expected a validation error when OBJECT_STORE_ADDR is unset")
	}
	var xe *expandError
	if !isExpandError(err, &xe) || xe.code != 1 {
		t.Errorf("want a validation expandError (code 1), got %#v", err)
	}
	if !strings.Contains(err.Error(), "OBJECT_STORE_ADDR") {
		t.Errorf("error should name OBJECT_STORE_ADDR, got: %v", err)
	}
}

// TestRunDownloadFixture_EmptyRunID proves a blank run id is rejected up front.
func TestRunDownloadFixture_EmptyRunID(t *testing.T) {
	if err := runDownloadFixture(context.Background(), "   ", "", new(bytes.Buffer)); err == nil {
		t.Fatal("expected an error for an empty run id")
	}
}
