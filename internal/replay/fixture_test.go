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
	"encoding/json"
	"strings"
	"testing"
)

// TestFixtureRoundTrip proves a fixture marshals → unmarshals losslessly, preserving the schema
// version, provenance, and both channels' bytes verbatim (incl. SSE-framed response bytes).
func TestFixtureRoundTrip(t *testing.T) {
	f := NewFixture("run-123", "team-a/planner")
	sseBytes := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	idx := f.AppendModel([]byte(`{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`), sseBytes, "text/event-stream", 200)
	if idx != 0 {
		t.Fatalf("first AppendModel index = %d, want 0", idx)
	}
	idx2 := f.AppendModel([]byte(`{"model":"gpt","messages":[{"role":"assistant","content":"hi"}]}`), []byte(`{"ok":true}`), "application/json", 200)
	if idx2 != 1 {
		t.Fatalf("second AppendModel index = %d, want 1", idx2)
	}
	f.AppendTool("call_abc", "search", []byte(`{"q":"go"}`), []byte(`{"results":["a","b"]}`))

	data, err := f.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	got, err := UnmarshalFixture(data)
	if err != nil {
		t.Fatalf("UnmarshalFixture: %v", err)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
	if got.RunID != "run-123" || got.Agent != "team-a/planner" {
		t.Errorf("provenance lost: runId=%q agent=%q", got.RunID, got.Agent)
	}
	if len(got.Model) != 2 || len(got.Tools) != 1 {
		t.Fatalf("channels lost: model=%d tools=%d", len(got.Model), len(got.Tools))
	}
	// SSE bytes must survive VERBATIM (the load-bearing replay property).
	if !bytes.Equal(got.Model[0].ResponseBytes, sseBytes) {
		t.Errorf("model[0].ResponseBytes not verbatim: got %q", got.Model[0].ResponseBytes)
	}
	if got.Model[0].ContentType != "text/event-stream" {
		t.Errorf("model[0].ContentType = %q", got.Model[0].ContentType)
	}
	if got.Model[0].Index != 0 || got.Model[1].Index != 1 {
		t.Errorf("model indices not preserved: %d, %d", got.Model[0].Index, got.Model[1].Index)
	}
	if got.Tools[0].CallID != "call_abc" || got.Tools[0].ToolName != "search" {
		t.Errorf("tool interaction lost: %+v", got.Tools[0])
	}
	if !bytes.Equal(got.Tools[0].ResponseBytes, []byte(`{"results":["a","b"]}`)) {
		t.Errorf("tool[0].ResponseBytes not verbatim: %q", got.Tools[0].ResponseBytes)
	}
}

// TestMarshalPinsSchemaVersion proves MarshalJSON always writes the current version even if a caller
// zeroed the field, so a fixture is never persisted with a stale/zero version.
func TestMarshalPinsSchemaVersion(t *testing.T) {
	f := NewFixture("r", "a")
	f.SchemaVersion = 0 // caller tampering
	data, err := f.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var probe struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if probe.SchemaVersion != SchemaVersion {
		t.Errorf("marshaled schemaVersion = %d, want %d", probe.SchemaVersion, SchemaVersion)
	}
}

// TestUnmarshalRejectsFutureVersion proves an unknown FUTURE major version is rejected (an older
// binary must never silently mis-replay a newer format).
func TestUnmarshalRejectsFutureVersion(t *testing.T) {
	future := SchemaVersion + 1
	data := []byte(`{"schemaVersion":` + itoa(future) + `,"runId":"r","agent":"a","model":[],"tools":[]}`)
	_, err := UnmarshalFixture(data)
	if err == nil {
		t.Fatal("expected an error for a future schema version, got nil")
	}
	if !strings.Contains(err.Error(), "newer than supported") {
		t.Errorf("error = %v, want a 'newer than supported' message", err)
	}
}

// TestUnmarshalRejectsMissingVersion proves a version-less (malformed / pre-versioning) fixture is
// rejected rather than defaulted.
func TestUnmarshalRejectsMissingVersion(t *testing.T) {
	data := []byte(`{"runId":"r","agent":"a","model":[],"tools":[]}`)
	_, err := UnmarshalFixture(data)
	if err == nil {
		t.Fatal("expected an error for a fixture with no schemaVersion, got nil")
	}
	if !strings.Contains(err.Error(), "no schemaVersion") {
		t.Errorf("error = %v, want a 'no schemaVersion' message", err)
	}
}

// TestUnmarshalAcceptsCurrentVersion sanity: the current version loads.
func TestUnmarshalAcceptsCurrentVersion(t *testing.T) {
	data := []byte(`{"schemaVersion":` + itoa(SchemaVersion) + `,"runId":"r","agent":"a","model":[],"tools":[]}`)
	if _, err := UnmarshalFixture(data); err != nil {
		t.Fatalf("current version should load: %v", err)
	}
}

// TestMatchModelByIndex proves the model channel is matched by request INDEX and the RequestHash
// divergence check is advisory (lenient on bytes: a diverged request still returns the recorded
// response, ADR 0071 §3).
func TestMatchModelByIndex(t *testing.T) {
	f := NewFixture("r", "a")
	f.AppendModel([]byte(`{"q":"one"}`), []byte("resp-0"), "application/json", 200)
	f.AppendModel([]byte(`{"q":"two"}`), []byte("resp-1"), "application/json", 200)

	// Exact match on index 1, same request bytes → not diverged.
	mi, diverged, ok := f.MatchModel(1, []byte(`{"q":"two"}`))
	if !ok {
		t.Fatal("index 1 should match")
	}
	if diverged {
		t.Error("identical request should not be flagged diverged")
	}
	if !bytes.Equal(mi.ResponseBytes, []byte("resp-1")) {
		t.Errorf("wrong response for index 1: %q", mi.ResponseBytes)
	}

	// Same index, DIFFERENT request bytes → diverged=true but STILL serves the recorded response.
	mi, diverged, ok = f.MatchModel(1, []byte(`{"q":"changed"}`))
	if !ok {
		t.Fatal("index 1 should still match on divergence")
	}
	if !diverged {
		t.Error("changed request should be flagged diverged")
	}
	if !bytes.Equal(mi.ResponseBytes, []byte("resp-1")) {
		t.Errorf("divergence must still serve the recorded response, got %q", mi.ResponseBytes)
	}

	// An index beyond the recording → structural miss (the caller's hard-fail).
	if _, _, ok := f.MatchModel(2, []byte(`{}`)); ok {
		t.Error("index past the recording should NOT match (structural divergence)")
	}
}

// TestMatchModelCanonicalHash proves the divergence hash is stable across key order + whitespace (a
// re-serialized-but-equal request does not falsely diverge).
func TestMatchModelCanonicalHash(t *testing.T) {
	f := NewFixture("r", "a")
	f.AppendModel([]byte(`{"a":1,"b":2}`), []byte("resp"), "application/json", 200)
	// Same content, different key order + whitespace.
	_, diverged, ok := f.MatchModel(0, []byte("{ \"b\": 2, \"a\": 1 }"))
	if !ok {
		t.Fatal("index 0 should match")
	}
	if diverged {
		t.Error("semantically identical JSON must not be flagged diverged")
	}
}

// TestMatchToolByCallID proves the tool channel matches primarily by call id, and each recording is
// consumed at most once (two identical calls consume two distinct recordings in order).
func TestMatchToolByCallID(t *testing.T) {
	f := NewFixture("r", "a")
	f.AppendTool("c1", "search", []byte(`{"q":"x"}`), []byte("r1"))
	f.AppendTool("c2", "search", []byte(`{"q":"x"}`), []byte("r2"))

	used := map[int]bool{}
	ti, ok := f.MatchTool("c2", "search", []byte(`{"q":"x"}`), used)
	if !ok || !bytes.Equal(ti.ResponseBytes, []byte("r2")) {
		t.Fatalf("call id c2 should match r2, got ok=%v resp=%q", ok, ti.ResponseBytes)
	}
	ti, ok = f.MatchTool("c1", "search", []byte(`{"q":"x"}`), used)
	if !ok || !bytes.Equal(ti.ResponseBytes, []byte("r1")) {
		t.Fatalf("call id c1 should match r1, got ok=%v resp=%q", ok, ti.ResponseBytes)
	}
	// Both consumed; a third call by an unknown id misses.
	if _, ok := f.MatchTool("c3", "search", []byte(`{"q":"x"}`), used); ok {
		t.Error("unknown call id should not match (structural divergence)")
	}
}

// TestMatchToolByNameArgsHashFallback proves a call with NO id falls back to name+args-hash, and the
// hash is canonical (key order / whitespace insensitive).
func TestMatchToolByNameArgsHashFallback(t *testing.T) {
	f := NewFixture("r", "a")
	f.AppendTool("", "weather", []byte(`{"city":"NYC","unit":"c"}`), []byte("cold"))

	used := map[int]bool{}
	// Match with no call id, different key order + whitespace.
	ti, ok := f.MatchTool("", "weather", []byte("{ \"unit\":\"c\", \"city\":\"NYC\" }"), used)
	if !ok || !bytes.Equal(ti.ResponseBytes, []byte("cold")) {
		t.Fatalf("name+args fallback should match, got ok=%v resp=%q", ok, ti.ResponseBytes)
	}
	// Wrong tool name misses.
	if _, ok := f.MatchTool("", "traffic", []byte(`{"city":"NYC","unit":"c"}`), map[int]bool{}); ok {
		t.Error("wrong tool name should not match")
	}
	// Wrong args miss.
	if _, ok := f.MatchTool("", "weather", []byte(`{"city":"LA"}`), map[int]bool{}); ok {
		t.Error("wrong args should not match")
	}
}

// TestAssertNoCredentials_Clean proves a normal fixture (prompts + tool results, no headers) passes.
func TestAssertNoCredentials_Clean(t *testing.T) {
	f := NewFixture("r", "a")
	// A prompt that MENTIONS the word "Authorization" in prose must NOT trip the invariant — only a
	// real header line does.
	f.AppendModel([]byte(`{"messages":[{"role":"user","content":"explain the Authorization header"}]}`), []byte("ok"), "application/json", 200)
	f.AppendTool("c1", "search", []byte(`{"q":"authorization: bearer as a search term is fine mid-value"}`), []byte("ok"))
	if err := f.AssertNoCredentials(); err != nil {
		t.Errorf("clean fixture should pass the no-credential invariant, got: %v", err)
	}
}

// TestAssertNoCredentials_AuthorizationHeaderFails proves a fixture carrying an Authorization HEADER
// LINE fails the no-token invariant (the C4 enforcement + the required test).
func TestAssertNoCredentials_AuthorizationHeaderFails(t *testing.T) {
	f := NewFixture("r", "a")
	// A raw request that leaked an Authorization header line (a capture-strip miss).
	leaked := []byte("POST /v1/chat/completions HTTP/1.1\r\nAuthorization: Bearer sk-secret-token\r\nContent-Type: application/json\r\n\r\n{}")
	f.AppendModel(leaked, []byte("resp"), "application/json", 200)

	err := f.AssertNoCredentials()
	if err == nil {
		t.Fatal("a fixture with an Authorization header MUST fail the no-credential invariant")
	}
	if !strings.Contains(err.Error(), "Authorization") {
		t.Errorf("error should name the Authorization header, got: %v", err)
	}
	// Put must refuse it too (belt-and-braces at the store boundary, covered in store_test.go).
}

// TestAssertNoCredentials_ToolBearerFails proves a leaked OBO bearer on the TOOL channel (an
// injection-point capture miss) fails.
func TestAssertNoCredentials_ToolBearerFails(t *testing.T) {
	f := NewFixture("r", "a")
	leaked := []byte("GET /calendar HTTP/1.1\nAuthorization: Bearer obo-user-token\n\n")
	f.AppendTool("c1", "gcal", leaked, []byte("ok"))
	if err := f.AssertNoCredentials(); err == nil {
		t.Fatal("a leaked OBO bearer on the tool channel MUST fail the invariant")
	}
}

// TestAssertNoCredentials_CookieAndAPIKey proves other credential header names are caught too.
func TestAssertNoCredentials_CookieAndAPIKey(t *testing.T) {
	for _, hdr := range []string{"Cookie: session=abc", "X-Api-Key: key-123", "Anthropic-Api-Key: sk-ant-xxx"} {
		f := NewFixture("r", "a")
		f.AppendModel([]byte("HDR\n"+hdr+"\n\nbody"), []byte("resp"), "application/json", 200)
		if err := f.AssertNoCredentials(); err == nil {
			t.Errorf("credential header %q should fail the invariant", hdr)
		}
	}
}

// itoa is a tiny int→string helper so the tests avoid importing strconv just for literals.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
