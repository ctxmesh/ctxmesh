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

// Unit tests for the :2998 memory endpoint. The handler contract is exercised
// end-to-end against miniredis via the REAL redisStore (so the LIST encoding,
// TxPipeline TTL behaviour, and missing-key semantics are covered for real),
// plus an error-injecting fake store for the backend-down 502 path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// newTestMemoryServer spins up a miniredis and returns an httptest server
// wrapping the full memory handler (real redisStore, recording tracer), plus
// the miniredis handle for direct state/TTL assertions.
func newTestMemoryServer(t *testing.T) (*miniredis.Miniredis, *httptest.Server) {
	t.Helper()
	mr := miniredis.RunT(t)

	_, tp := newTestTracer(t)
	ms := newMemoryServer(
		newRedisStore(mr.Addr()),
		memoryConfig{BackendAddr: mr.Addr(), Port: defaultMemoryPort, Namespace: "test-ns", Agent: "test-agent"},
		tp.Tracer(tracerName),
	)
	srv := httptest.NewServer(ms.handler())
	t.Cleanup(srv.Close)
	return mr, srv
}

// doReq issues an HTTP request against the test server and returns the
// response with its body decoded into raw bytes.
func doReq(t *testing.T, method, url string, body string) (*http.Response, []byte) {
	t.Helper()
	var req *http.Request
	var err error
	if body == "" {
		req, err = http.NewRequest(method, url, nil)
	} else {
		req, err = http.NewRequest(method, url, strings.NewReader(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, respBody
}

// ── healthz ───────────────────────────────────────────────────────────────────

func TestMemoryHealthz(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	resp, _ := doReq(t, http.MethodGet, srv.URL+"/healthz", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
}

// ── GET ───────────────────────────────────────────────────────────────────────

func TestMemoryGetEmptyReturnsEmptyArray(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	resp, body := doReq(t, http.MethodGet, srv.URL+"/memory/never-written", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if got := strings.TrimSpace(string(body)); got != "[]" {
		t.Errorf("GET missing conversation = %q, want []", got)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// ── PUT + GET roundtrip ───────────────────────────────────────────────────────

func TestMemoryPutGetRoundtrip(t *testing.T) {
	t.Parallel()
	mr, srv := newTestMemoryServer(t)

	put := `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]`
	resp, body := doReq(t, http.MethodPut, srv.URL+"/memory/conv1", put)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204; body=%s", resp.StatusCode, body)
	}

	resp, body = doReq(t, http.MethodGet, srv.URL+"/memory/conv1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	var got []map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("GET body is not a JSON array of objects: %v; body=%s", err, body)
	}
	if len(got) != 2 || got[0]["content"] != "hi" || got[1]["content"] != "hello" {
		t.Errorf("roundtrip mismatch: %s", body)
	}

	// The key layout must be mem:{ns}/{agent}:{convId}.
	if !mr.Exists("mem:test-ns/test-agent:conv1") {
		t.Errorf("expected key mem:test-ns/test-agent:conv1 in redis; keys=%v", mr.Keys())
	}
}

func TestMemoryPutReplacesExisting(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	doReq(t, http.MethodPut, srv.URL+"/memory/conv1", `["a","b","c"]`)
	doReq(t, http.MethodPut, srv.URL+"/memory/conv1", `["z"]`)

	_, body := doReq(t, http.MethodGet, srv.URL+"/memory/conv1", "")
	if got := strings.TrimSpace(string(body)); got != `["z"]` {
		t.Errorf("PUT did not replace: got %s, want [\"z\"]", got)
	}
}

func TestMemoryPutEmptyArrayClears(t *testing.T) {
	t.Parallel()
	mr, srv := newTestMemoryServer(t)

	doReq(t, http.MethodPut, srv.URL+"/memory/conv1", `["a"]`)
	resp, _ := doReq(t, http.MethodPut, srv.URL+"/memory/conv1", `[]`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT [] status = %d, want 204", resp.StatusCode)
	}
	_, body := doReq(t, http.MethodGet, srv.URL+"/memory/conv1", "")
	if got := strings.TrimSpace(string(body)); got != "[]" {
		t.Errorf("after PUT []: GET = %s, want []", got)
	}
	if mr.Exists("mem:test-ns/test-agent:conv1") {
		t.Error("empty PUT should delete the key entirely")
	}
}

func TestMemoryPutRejectsNonArray(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	for _, bad := range []string{`{"not":"array"}`, `"str"`, `42`, `not json at all`} {
		resp, body := doReq(t, http.MethodPut, srv.URL+"/memory/conv1", bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %q status = %d, want 400", bad, resp.StatusCode)
		}
		assertJSONError(t, body)
	}
}

// ── append ────────────────────────────────────────────────────────────────────

func TestMemoryAppendOrdering(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	for i := range 5 {
		entry := fmt.Sprintf(`{"turn":%d}`, i)
		resp, body := doReq(t, http.MethodPost, srv.URL+"/memory/conv1/append", entry)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("append %d status = %d, want 204; body=%s", i, resp.StatusCode, body)
		}
	}

	_, body := doReq(t, http.MethodGet, srv.URL+"/memory/conv1", "")
	var got []struct{ Turn int }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("GET after appends: %v; body=%s", err, body)
	}
	if len(got) != 5 {
		t.Fatalf("entries = %d, want 5", len(got))
	}
	for i, e := range got {
		if e.Turn != i {
			t.Errorf("entry %d = turn %d, want %d (ordering broken)", i, e.Turn, i)
		}
	}
}

func TestMemoryAppendAttributesMessageEntry(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	resp, body := doReq(t, http.MethodPost, srv.URL+"/memory/c/append", `{"role":"user","content":"hi"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("append status=%d body=%s", resp.StatusCode, body)
	}
	_, got := doReq(t, http.MethodGet, srv.URL+"/memory/c", "")
	var entries []map[string]any
	if err := json.Unmarshal(got, &entries); err != nil {
		t.Fatalf("GET: %v body=%s", err, got)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	e := entries[0]
	if e["role"] != "user" || e["content"] != "hi" {
		t.Errorf("role/content not preserved: %v", e)
	}
	if e["agent"] != "test-agent" {
		t.Errorf("agent = %v, want test-agent (server-authoritative attribution)", e["agent"])
	}
	if _, ok := e["messageId"].(string); !ok {
		t.Errorf("messageId missing/not a string: %v", e["messageId"])
	}
	if _, ok := e["ts"].(float64); !ok {
		t.Errorf("ts missing/not a number: %v", e["ts"])
	}
}

func TestMemoryAppendHonorsClientMessageID(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	req, _ := http.NewRequest(
		http.MethodPost, srv.URL+"/memory/c/append",
		strings.NewReader(`{"role":"assistant","content":"ok"}`),
	)
	req.Header.Set("X-Message-Id", "m-fixed")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	_, got := doReq(t, http.MethodGet, srv.URL+"/memory/c", "")
	var entries []map[string]any
	_ = json.Unmarshal(got, &entries)
	if entries[0]["messageId"] != "m-fixed" {
		t.Errorf("messageId = %v, want m-fixed (per-hop id honored, ADR 0035)", entries[0]["messageId"])
	}
}

func TestMemoryAppendNonMessageStoredVerbatim(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	doReq(t, http.MethodPost, srv.URL+"/memory/c/append", `{"note":"scratch"}`)
	_, got := doReq(t, http.MethodGet, srv.URL+"/memory/c", "")
	var entries []map[string]any
	_ = json.Unmarshal(got, &entries)
	if _, ok := entries[0]["agent"]; ok {
		t.Errorf("a non-message entry must NOT be attributed: %v", entries[0])
	}
	if entries[0]["note"] != "scratch" {
		t.Errorf("note not preserved: %v", entries[0])
	}
}

func TestAttributeEntryIdempotent(t *testing.T) {
	t.Parallel()
	// A message already carrying agent/messageId keeps them (a replay retains its origin); only the
	// absent ts is added.
	in := json.RawMessage(`{"role":"user","content":"hi","agent":"other","messageId":"m-orig"}`)
	out := attributeEntry(in, "me", "m-new", time.UnixMilli(1234))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["agent"] != "other" {
		t.Errorf("agent overwritten: %v", m["agent"])
	}
	if m["messageId"] != "m-orig" {
		t.Errorf("messageId overwritten: %v", m["messageId"])
	}
	if m["ts"] != float64(1234) {
		t.Errorf("ts = %v, want 1234 (added when absent)", m["ts"])
	}
}

func TestMemoryGetReturnsETagVersion(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	doReq(t, http.MethodPost, srv.URL+"/memory/c/append", `{"a":1}`)
	doReq(t, http.MethodPost, srv.URL+"/memory/c/append", `{"b":2}`)
	resp, _ := doReq(t, http.MethodGet, srv.URL+"/memory/c", "")
	if got := resp.Header.Get("ETag"); got != "2" {
		t.Errorf("ETag = %q, want \"2\" (the conversation version = entry count)", got)
	}
}

func putIfMatch(t *testing.T, url, ifMatch, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func TestMemoryConditionalReplaceSucceedsOnMatch(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	doReq(t, http.MethodPost, srv.URL+"/memory/c/append", `{"a":1}`) // version → 1
	resp := putIfMatch(t, srv.URL+"/memory/c", "1", `[{"x":1},{"y":2}]`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("conditional replace status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("ETag"); got != "2" {
		t.Errorf("new ETag = %q, want \"2\"", got)
	}
}

func TestMemoryConditionalReplaceConflictsOnStaleVersion(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	// Read at version 1, then a concurrent APPEND advances it to 2 before our replace lands.
	doReq(t, http.MethodPost, srv.URL+"/memory/c/append", `{"a":1}`) // version 1
	doReq(t, http.MethodPost, srv.URL+"/memory/c/append", `{"b":2}`) // version 2 (the concurrent write)

	// A replace based on the stale version 1 must be REJECTED (412), not clobber the append.
	resp := putIfMatch(t, srv.URL+"/memory/c", "1", `[{"rewrite":true}]`)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale replace status = %d, want 412", resp.StatusCode)
	}
	// The concurrent append survives — no silent clobber.
	_, body := doReq(t, http.MethodGet, srv.URL+"/memory/c", "")
	var entries []map[string]any
	_ = json.Unmarshal(body, &entries)
	if len(entries) != 2 {
		t.Errorf("entries = %d, want 2 (the append was NOT clobbered)", len(entries))
	}
}

func TestMemoryUnconditionalReplaceStillWorks(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)
	doReq(t, http.MethodPost, srv.URL+"/memory/c/append", `{"a":1}`)
	// No If-Match → legacy last-writer-wins replace.
	resp := putIfMatch(t, srv.URL+"/memory/c", "", `[{"x":1}]`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unconditional replace status = %d, want 204", resp.StatusCode)
	}
}

func TestMemoryAppendRejectsInvalidJSON(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	for _, bad := range []string{`{"unclosed":`, `not json`, ``, `{"a":1} {"b":2}`} {
		resp, body := doReq(t, http.MethodPost, srv.URL+"/memory/conv1/append", bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("append %q status = %d, want 400; body=%s", bad, resp.StatusCode, body)
		}
	}

	// Nothing must have been stored.
	_, body := doReq(t, http.MethodGet, srv.URL+"/memory/conv1", "")
	if got := strings.TrimSpace(string(body)); got != "[]" {
		t.Errorf("invalid appends leaked into storage: %s", got)
	}
}

// ── 1MiB cap ─────────────────────────────────────────────────────────────────

func TestMemoryBodyCap(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	// A JSON array slightly over 1MiB.
	big := `["` + strings.Repeat("x", maxMemoryBody) + `"]`

	resp, body := doReq(t, http.MethodPut, srv.URL+"/memory/conv1", big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize PUT status = %d, want 413", resp.StatusCode)
	}
	assertJSONError(t, body)

	resp, body = doReq(t, http.MethodPost, srv.URL+"/memory/conv1/append", big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize append status = %d, want 413", resp.StatusCode)
	}
	assertJSONError(t, body)

	// A body just under the cap must be accepted.
	small := `["` + strings.Repeat("x", 1024) + `"]`
	resp, _ = doReq(t, http.MethodPut, srv.URL+"/memory/conv1", small)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("in-cap PUT status = %d, want 204", resp.StatusCode)
	}
}

// ── conversationId validation ─────────────────────────────────────────────────

func TestValidateConversationID(t *testing.T) {
	t.Parallel()

	valid := []string{"conv1", "a", "user-42_session.7", "UUID-0f8fad5b", strings.Repeat("k", maxConversationID)}
	for _, id := range valid {
		if err := validateConversationID(id); err != nil {
			t.Errorf("validateConversationID(%q) = %v, want nil", id, err)
		}
	}

	invalid := []string{
		"",
		strings.Repeat("k", maxConversationID+1),
		"has space",
		"has/slash",
		"has:colon",
		"tab\there",
		"ctrl\x01char",
	}
	for _, id := range invalid {
		if err := validateConversationID(id); err == nil {
			t.Errorf("validateConversationID(%q) = nil, want error", id)
		}
	}
}

func TestMemoryInvalidConversationIDRejected(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	// Too long — routable as a single path segment but over the 128 cap.
	tooLong := strings.Repeat("k", maxConversationID+1)
	resp, body := doReq(t, http.MethodGet, srv.URL+"/memory/"+tooLong, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("overlong id status = %d, want 400", resp.StatusCode)
	}
	assertJSONError(t, body)

	// Colon is path-routable but forbidden (it would corrupt the key layout).
	resp, body = doReq(t, http.MethodPut, srv.URL+"/memory/bad:id", `[]`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("colon id status = %d, want 400; body=%s", resp.StatusCode, body)
	}
}

// ── search ────────────────────────────────────────────────────────────────────

func TestMemorySearch(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	doReq(t, http.MethodPut, srv.URL+"/memory/conv1",
		`[{"content":"the sky is blue"},{"content":"grass is green"},{"content":"blueberries"}]`)

	t.Run("hit", func(t *testing.T) {
		_, body := doReq(t, http.MethodGet, srv.URL+"/memory/conv1/search?q=blue", "")
		var got []json.RawMessage
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("search body: %v; %s", err, body)
		}
		if len(got) != 2 {
			t.Errorf("search 'blue' matched %d entries, want 2; body=%s", len(got), body)
		}
	})

	t.Run("miss", func(t *testing.T) {
		_, body := doReq(t, http.MethodGet, srv.URL+"/memory/conv1/search?q=purple", "")
		if got := strings.TrimSpace(string(body)); got != "[]" {
			t.Errorf("search miss = %s, want []", got)
		}
	})

	t.Run("empty q returns everything", func(t *testing.T) {
		_, body := doReq(t, http.MethodGet, srv.URL+"/memory/conv1/search", "")
		var got []json.RawMessage
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("search body: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("empty-q search = %d entries, want 3", len(got))
		}
	})
}

// ── TTL ───────────────────────────────────────────────────────────────────────

func TestMemoryTTLSetAndRefreshedOnWrite(t *testing.T) {
	t.Parallel()
	mr, srv := newTestMemoryServer(t)
	key := "mem:test-ns/test-agent:conv1"

	doReq(t, http.MethodPut, srv.URL+"/memory/conv1", `["a"]`)
	if ttl := mr.TTL(key); ttl != memoryTTL {
		t.Errorf("TTL after PUT = %v, want %v", ttl, memoryTTL)
	}

	// Age the key, then append — the TTL must be refreshed to the full 24h.
	mr.FastForward(1 * time.Hour)
	if ttl := mr.TTL(key); ttl != memoryTTL-time.Hour {
		t.Fatalf("TTL after fast-forward = %v, want %v (test setup)", ttl, memoryTTL-time.Hour)
	}
	doReq(t, http.MethodPost, srv.URL+"/memory/conv1/append", `"b"`)
	if ttl := mr.TTL(key); ttl != memoryTTL {
		t.Errorf("TTL after append = %v, want refreshed %v", ttl, memoryTTL)
	}

	// A read must NOT refresh the TTL (TTL is refreshed on write only).
	mr.FastForward(1 * time.Hour)
	doReq(t, http.MethodGet, srv.URL+"/memory/conv1", "")
	if ttl := mr.TTL(key); ttl != memoryTTL-time.Hour {
		t.Errorf("TTL after GET = %v, want unchanged %v", ttl, memoryTTL-time.Hour)
	}
}

// ── backend down → 502 ────────────────────────────────────────────────────────

// failingStore is a MemoryStore whose every op fails — the backend-down case.
type failingStore struct{ err error }

func (f *failingStore) Get(context.Context, string) ([]json.RawMessage, error) { return nil, f.err }
func (f *failingStore) Replace(context.Context, string, []json.RawMessage, time.Duration) error {
	return f.err
}

func (f *failingStore) ReplaceIfVersion(
	context.Context, string, []json.RawMessage, int, time.Duration,
) (int, bool, error) {
	return 0, false, f.err
}

func (f *failingStore) Append(context.Context, string, json.RawMessage, time.Duration) (int, error) {
	return 0, f.err
}

func TestMemoryBackendDownReturns502(t *testing.T) {
	t.Parallel()

	_, tp := newTestTracer(t)
	ms := newMemoryServer(
		&failingStore{err: errors.New("connection refused")},
		memoryConfig{Namespace: "ns", Agent: "ag"},
		tp.Tracer(tracerName),
	)
	srv := httptest.NewServer(ms.handler())
	t.Cleanup(srv.Close)

	cases := []struct{ method, path, body string }{
		{http.MethodGet, "/memory/conv1", ""},
		{http.MethodPut, "/memory/conv1", `["a"]`},
		{http.MethodPost, "/memory/conv1/append", `"a"`},
		{http.MethodGet, "/memory/conv1/search?q=x", ""},
	}
	for _, c := range cases {
		resp, body := doReq(t, c.method, srv.URL+c.path, c.body)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("%s %s status = %d, want 502", c.method, c.path, resp.StatusCode)
		}
		assertJSONError(t, body)
	}

	// healthz must stay 200 even with the backend down (listener-only probe).
	resp, _ := doReq(t, http.MethodGet, srv.URL+"/healthz", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz with backend down = %d, want 200", resp.StatusCode)
	}
}

// TestMemoryBackendUnreachable drives the REAL redisStore at a closed port —
// the lazy-connect path must surface a 502, not hang (2s op timeout).
func TestMemoryBackendUnreachable(t *testing.T) {
	t.Parallel()

	// Grab a port that is guaranteed closed: start and stop a miniredis.
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()

	_, tp := newTestTracer(t)
	ms := newMemoryServer(
		newRedisStore(addr),
		memoryConfig{Namespace: "ns", Agent: "ag"},
		tp.Tracer(tracerName),
	)
	srv := httptest.NewServer(ms.handler())
	t.Cleanup(srv.Close)

	start := time.Now()
	resp, body := doReq(t, http.MethodGet, srv.URL+"/memory/conv1", "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("dead backend GET = %d, want 502; body=%s", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("dead backend GET took %v — per-op timeout not enforced", elapsed)
	}
}

// ── spans ─────────────────────────────────────────────────────────────────────

func TestMemorySpansEmitted(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)
	rec, tp := newTestTracer(t)
	ms := newMemoryServer(
		newRedisStore(mr.Addr()),
		memoryConfig{Namespace: "ns", Agent: "ag"},
		tp.Tracer(tracerName),
	)
	srv := httptest.NewServer(ms.handler())
	t.Cleanup(srv.Close)

	doReq(t, http.MethodPut, srv.URL+"/memory/conv1", `["a","b"]`)
	doReq(t, http.MethodGet, srv.URL+"/memory/conv1", "")
	doReq(t, http.MethodPost, srv.URL+"/memory/conv1/append", `"c"`)
	doReq(t, http.MethodGet, srv.URL+"/memory/conv1/search?q=a", "")
	// An op that errors (bad body) must STILL emit its span.
	doReq(t, http.MethodPut, srv.URL+"/memory/conv1", `not-json`)

	want := map[string]int{"memory.put": 2, "memory.get": 1, "memory.append": 1, "memory.search": 1}
	got := map[string]int{}
	for _, s := range rec.Ended() {
		got[s.Name()]++

		attrs := map[string]any{}
		for _, kv := range s.Attributes() {
			attrs[string(kv.Key)] = kv.Value.AsInterface()
		}
		if attrs["conversation.id"] != "conv1" {
			t.Errorf("span %s conversation.id = %v, want conv1", s.Name(), attrs["conversation.id"])
		}
		if _, ok := attrs["latency_ms"]; !ok {
			t.Errorf("span %s missing latency_ms", s.Name())
		}
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("span %s emitted %d times, want %d (all: %v)", name, got[name], n, got)
		}
	}

	// The successful append span must carry the post-append entry count (3).
	for _, s := range rec.Ended() {
		if s.Name() != "memory.append" {
			continue
		}
		for _, kv := range s.Attributes() {
			if string(kv.Key) == "memory.entries" && kv.Value.AsInt64() != 3 {
				t.Errorf("memory.append memory.entries = %d, want 3", kv.Value.AsInt64())
			}
		}
	}
}

// ── concurrency ───────────────────────────────────────────────────────────────

// TestMemoryConcurrentAppends fires parallel appends at one conversation and
// asserts none are lost (RPUSH atomicity — run with -race).
func TestMemoryConcurrentAppends(t *testing.T) {
	t.Parallel()
	_, srv := newTestMemoryServer(t)

	const writers = 20
	const perWriter = 10

	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				entry := fmt.Sprintf(`{"writer":%d,"seq":%d}`, w, i)
				req, err := http.NewRequest(http.MethodPost, srv.URL+"/memory/shared/append", strings.NewReader(entry))
				if err != nil {
					t.Error(err)
					return
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusNoContent {
					t.Errorf("concurrent append status = %d, want 204", resp.StatusCode)
				}
			}
		})
	}
	wg.Wait()

	_, body := doReq(t, http.MethodGet, srv.URL+"/memory/shared", "")
	var got []struct {
		Writer int `json:"writer"`
		Seq    int `json:"seq"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("GET after concurrent appends: %v", err)
	}
	if len(got) != writers*perWriter {
		t.Fatalf("entries = %d, want %d (appends lost)", len(got), writers*perWriter)
	}

	// Every (writer, seq) pair must be present exactly once, and each writer's
	// own entries must appear in its append order.
	seen := map[[2]int]int{} // (writer,seq) → position in list
	for pos, e := range got {
		k := [2]int{e.Writer, e.Seq}
		if _, dup := seen[k]; dup {
			t.Errorf("duplicate entry writer=%d seq=%d", e.Writer, e.Seq)
		}
		seen[k] = pos
	}
	for w := range writers {
		for i := 1; i < perWriter; i++ {
			prev, ok1 := seen[[2]int{w, i - 1}]
			cur, ok2 := seen[[2]int{w, i}]
			if !ok1 || !ok2 {
				t.Fatalf("missing entry for writer %d", w)
			}
			if prev > cur {
				t.Errorf("writer %d: seq %d at pos %d appears after seq %d at pos %d", w, i-1, prev, i, cur)
			}
		}
	}
}

// ── config gating ─────────────────────────────────────────────────────────────

func TestLoadMemoryConfig(t *testing.T) {
	t.Parallel()

	t.Run("disabled when MEMORY_BACKEND_ADDR unset", func(t *testing.T) {
		t.Parallel()
		cfg, err := loadConfig(envMap(map[string]string{"AGENT_ENTRYPOINT": "/bin/agent"}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MemoryEnabled() {
			t.Error("MemoryEnabled() = true without MEMORY_BACKEND_ADDR")
		}
	})

	t.Run("enabled with defaults", func(t *testing.T) {
		t.Parallel()
		cfg, err := loadConfig(envMap(map[string]string{
			"AGENT_ENTRYPOINT":    "/bin/agent",
			"AGENT_NAME":          "my-agent",
			"MEMORY_BACKEND_ADDR": "valkey:6379",
			"POD_NAMESPACE":       "team-a",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.MemoryEnabled() {
			t.Fatal("MemoryEnabled() = false, want true")
		}
		if cfg.Memory.Port != defaultMemoryPort {
			t.Errorf("Memory.Port = %d, want %d", cfg.Memory.Port, defaultMemoryPort)
		}
		if cfg.Memory.Namespace != "team-a" || cfg.Memory.Agent != "my-agent" {
			t.Errorf("key identity = %s/%s, want team-a/my-agent", cfg.Memory.Namespace, cfg.Memory.Agent)
		}
	})

	t.Run("MEMORY_KEY_NAMESPACE overrides POD_NAMESPACE", func(t *testing.T) {
		t.Parallel()
		cfg, err := loadConfig(envMap(map[string]string{
			"AGENT_ENTRYPOINT":     "/bin/agent",
			"MEMORY_BACKEND_ADDR":  "valkey:6379",
			"MEMORY_KEY_NAMESPACE": "explicit-ns",
			"POD_NAMESPACE":        "pod-ns",
		}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Memory.Namespace != "explicit-ns" {
			t.Errorf("Namespace = %q, want explicit-ns", cfg.Memory.Namespace)
		}
	})

	t.Run("invalid MEMORY_PORT rejected", func(t *testing.T) {
		t.Parallel()
		_, err := loadConfig(envMap(map[string]string{
			"AGENT_ENTRYPOINT":    "/bin/agent",
			"MEMORY_BACKEND_ADDR": "valkey:6379",
			"MEMORY_PORT":         "not-a-port",
		}))
		if err == nil {
			t.Error("expected error for invalid MEMORY_PORT")
		}
	})

	t.Run("invalid MEMORY_PORT ignored when backend unset", func(t *testing.T) {
		t.Parallel()
		_, err := loadConfig(envMap(map[string]string{
			"AGENT_ENTRYPOINT": "/bin/agent",
			"MEMORY_PORT":      "not-a-port",
		}))
		if err != nil {
			t.Errorf("memory envs must be inert when the feature is off: %v", err)
		}
	})
}

// assertJSONError asserts body is a JSON object with a non-empty "error" key —
// the single error shape of the endpoint.
func assertJSONError(t *testing.T, body []byte) {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error == "" {
		t.Errorf("error body is not {\"error\":...}: %s", body)
	}
}
