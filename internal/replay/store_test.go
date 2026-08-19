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
	"strings"
	"testing"

	"github.com/ctxmesh/agent-engine/internal/objectstore"
)

// TestFixtureStorePutGetRoundTrip proves a fixture written to the (mem twin of the) durable object
// store reads back byte-identical, through the same ObjectStore SPI the KB feature uses.
func TestFixtureStorePutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	mem := objectstore.NewMemObjectStore()
	fs, err := NewFixtureStore(mem)
	if err != nil {
		t.Fatalf("NewFixtureStore: %v", err)
	}

	f := NewFixture("run-xyz", "team/agent")
	sse := []byte("data: {\"delta\":\"hi\"}\n\ndata: [DONE]\n\n")
	f.AppendModel([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), sse, "text/event-stream", 200)
	f.AppendTool("call_1", "search", []byte(`{"q":"go"}`), []byte(`{"r":1}`), "")

	ref, err := fs.Put(ctx, f)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasPrefix(ref, "fixtures/run-xyz/") || !strings.HasSuffix(ref, ".json") {
		t.Errorf("ref %q not run-keyed under fixtures/ with .json suffix", ref)
	}

	got, err := fs.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RunID != "run-xyz" || got.SchemaVersion != SchemaVersion {
		t.Errorf("round-trip lost provenance: %+v", got)
	}
	if len(got.Model) != 1 || !bytes.Equal(got.Model[0].ResponseBytes, sse) {
		t.Errorf("model channel not verbatim after store round-trip")
	}
	if len(got.Tools) != 1 || got.Tools[0].CallID != "call_1" {
		t.Errorf("tool channel lost after store round-trip: %+v", got.Tools)
	}
}

// TestFixtureStorePutIsContentAddressedIdempotent proves re-Putting the SAME fixture bytes yields the
// SAME key (content-addressed within the run namespace) — a reclaim re-recording does not duplicate.
func TestFixtureStorePutIsContentAddressedIdempotent(t *testing.T) {
	ctx := context.Background()
	fs, _ := NewFixtureStore(objectstore.NewMemObjectStore())
	f := NewFixture("run-1", "a")
	f.AppendModel([]byte(`{"q":1}`), []byte("resp"), "application/json", 200)

	ref1, err := fs.Put(ctx, f)
	if err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	ref2, err := fs.Put(ctx, f)
	if err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	if ref1 != ref2 {
		t.Errorf("content-addressed key not idempotent: %q vs %q", ref1, ref2)
	}
}

// TestFixtureStorePutRefusesCredentialLeak proves Put FAILS CLOSED on a fixture that carries a
// credential — it must never persist a shareable artifact with a leaked token (C4 / the
// non-negotiables). This is the store-boundary belt-and-braces over AssertNoCredentials.
func TestFixtureStorePutRefusesCredentialLeak(t *testing.T) {
	ctx := context.Background()
	mem := objectstore.NewMemObjectStore()
	fs, _ := NewFixtureStore(mem)

	f := NewFixture("run-leak", "a")
	leaked := []byte("POST /v1/chat HTTP/1.1\r\nAuthorization: Bearer sk-secret\r\n\r\n{}")
	f.AppendModel(leaked, []byte("resp"), "application/json", 200)

	_, err := fs.Put(ctx, f)
	if err == nil {
		t.Fatal("Put MUST refuse a fixture carrying a credential")
	}
	if !strings.Contains(err.Error(), "Authorization") {
		t.Errorf("refusal should name the leaked header, got: %v", err)
	}
	// Nothing should have been written to the store.
	objs, _ := mem.List(ctx, "fixtures/")
	if len(objs) != 0 {
		t.Errorf("a refused fixture must not be written; found %d objects", len(objs))
	}
}

// TestNewFixtureStoreRejectsNilStore proves construction fails loud when the object store is
// unconfigured (OBJECT_STORE_ADDR unset ⇒ objectstore.NewMinioStore returns nil).
func TestNewFixtureStoreRejectsNilStore(t *testing.T) {
	if _, err := NewFixtureStore(nil); err == nil {
		t.Fatal("NewFixtureStore(nil) must return an error")
	}
}

// TestFixtureStoreGetRunMergesPartialBlobs proves GetRun lists a run's fixtures/{runId}/ prefix,
// downloads BOTH partial channel blobs (the gateway's MODEL blob + the sidecar's TOOLS blob, written
// as two distinct content-addressed keys), and merges them into one replayable fixture — the
// object-store analogue of `dev --replay <dir>` that the download-fixture CLI relies on.
func TestFixtureStoreGetRunMergesPartialBlobs(t *testing.T) {
	ctx := context.Background()
	mem := objectstore.NewMemObjectStore()
	fs, _ := NewFixtureStore(mem)

	// Two partial blobs for the SAME run, each Put under its own content-addressed key.
	modelBlob := NewFixture("run-merge", "team/agent")
	modelBlob.AppendModel([]byte(`{"q":1}`), []byte(`{"ok":1}`), "application/json", 200)
	if _, err := fs.Put(ctx, modelBlob); err != nil {
		t.Fatalf("Put model blob: %v", err)
	}
	toolBlob := NewFixture("run-merge", "team/agent")
	toolBlob.AppendTool("call_1", "search", []byte(`{"q":"go"}`), []byte(`{"r":1}`), "text/event-stream")
	if _, err := fs.Put(ctx, toolBlob); err != nil {
		t.Fatalf("Put tool blob: %v", err)
	}

	got, err := fs.GetRun(ctx, "run-merge")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.RunID != "run-merge" || got.Agent != "team/agent" {
		t.Errorf("GetRun lost provenance: %+v", got)
	}
	if len(got.Model) != 1 || len(got.Tools) != 1 {
		t.Fatalf("GetRun did not merge both channels: %d model, %d tool", len(got.Model), len(got.Tools))
	}
	if got.Model[0].Index != 0 {
		t.Errorf("merged model index not re-assigned: %d", got.Model[0].Index)
	}
	if got.Tools[0].ContentType != "text/event-stream" {
		t.Errorf("merged tool lost recorded content-type: %q", got.Tools[0].ContentType)
	}
}

// TestFixtureStoreGetRunNoBlobsIsHonestError proves a run with no recorded fixture is a clear error
// (nothing recorded / wrong run id) — never an empty fixture the CLI would silently write out.
func TestFixtureStoreGetRunNoBlobsIsHonestError(t *testing.T) {
	ctx := context.Background()
	fs, _ := NewFixtureStore(objectstore.NewMemObjectStore())

	_, err := fs.GetRun(ctx, "run-never-recorded")
	if err == nil {
		t.Fatal("GetRun must error for a run with no fixture blobs")
	}
	if !strings.Contains(err.Error(), "run-never-recorded") {
		t.Errorf("error should name the run id, got: %v", err)
	}
}

// TestFixtureStoreGetRunPrefixIsExclusive proves GetRun's run prefix does not over-match a run whose
// id is a prefix of another's (the trailing slash) — "run-1" must not pull "run-12"'s blobs.
func TestFixtureStoreGetRunPrefixIsExclusive(t *testing.T) {
	ctx := context.Background()
	fs, _ := NewFixtureStore(objectstore.NewMemObjectStore())

	f1 := NewFixture("run-1", "a")
	f1.AppendModel([]byte(`{"q":1}`), []byte(`resp1`), "application/json", 200)
	if _, err := fs.Put(ctx, f1); err != nil {
		t.Fatalf("Put run-1: %v", err)
	}
	f12 := NewFixture("run-12", "a")
	f12.AppendModel([]byte(`{"q":2}`), []byte(`resp2`), "application/json", 200)
	f12.AppendTool("c", "t", []byte(`{}`), []byte(`{}`), "")
	if _, err := fs.Put(ctx, f12); err != nil {
		t.Fatalf("Put run-12: %v", err)
	}

	got, err := fs.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun run-1: %v", err)
	}
	if len(got.Model) != 1 || len(got.Tools) != 0 {
		t.Errorf("run-1 over-matched run-12's blobs: %d model, %d tool", len(got.Model), len(got.Tools))
	}
}

// TestFixtureStoreGetRejectsFutureSchema proves the schema-version contract is enforced on the READ
// path too (a stored fixture from a newer binary is rejected, not mis-replayed).
func TestFixtureStoreGetRejectsFutureSchema(t *testing.T) {
	ctx := context.Background()
	mem := objectstore.NewMemObjectStore()
	fs, _ := NewFixtureStore(mem)

	// Write a raw future-version blob directly under a plausible key.
	future := []byte(`{"schemaVersion":999,"runId":"r","agent":"a","model":[],"tools":[]}`)
	key := "fixtures/r/deadbeef.json"
	if err := mem.Put(ctx, key, bytes.NewReader(future), int64(len(future)), fixtureContentType); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	if _, err := fs.Get(ctx, key); err == nil {
		t.Fatal("Get must reject a future-schema fixture")
	}
}
