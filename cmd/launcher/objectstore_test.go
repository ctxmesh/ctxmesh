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

// Unit tests for blob offload / rehydrate (objectstore.go) and its wiring into
// the async publish (publishEnvelope) and consume (asyncConsumer.consume) paths.
//
//	offload:   a >256KiB payload → PUT to the store under a content-addressed key
//	           + the envelope payload REPLACED with {"$ref":...,"$size":n}; a
//	           sub-threshold payload passes through inline unchanged.
//	rehydrate: a $ref payload → the ORIGINAL bytes fetched from the store; a
//	           dangling / failed $ref → an error → the consumer NACKs (502) so the
//	           message DLQs (never a broken payload to the agent).
//	publish:   a store PUT failure → a TYPED error (best-effort; no event emitted).
//
// Run with -race. No real MinIO: the ObjectStore is an in-memory fake, including
// one that ERRORS to exercise the failure posture.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"go.opentelemetry.io/otel/trace/noop"
)

// fakeObjectStore is an in-memory ObjectStore stub. When putErr / getErr are
// set the corresponding op returns it (to exercise the publish-typed-error and
// dangling-$ref→NACK paths); otherwise it stores/returns bytes like MinIO.
// putKeys records the keys written (order-independent) so a test can assert the
// content-addressed key was used.
type fakeObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	putErr  error
	getErr  error
	puts    int
	gets    int
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: map[string][]byte{}}
}

func (f *fakeObjectStore) Put(_ context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.putErr != nil {
		return f.putErr
	}
	// Copy so a later mutation of the caller's slice cannot corrupt the store.
	cp := make([]byte, len(data))
	copy(cp, data)
	f.objects[key] = cp
	return nil
}

func (f *fakeObjectStore) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, errors.New("object not found")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, nil
}

func (f *fakeObjectStore) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func (f *fakeObjectStore) count() (puts, gets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts, f.gets
}

// bigPayload builds a JSON payload whose serialized length exceeds
// offloadThreshold, with recognisable content so a round-trip is verifiable.
func bigPayload(t *testing.T) json.RawMessage {
	t.Helper()
	// A JSON string of (threshold+1024) 'x' bytes is comfortably over the limit.
	body := strings.Repeat("x", offloadThreshold+1024)
	p, err := json.Marshal(map[string]string{"blob": body})
	if err != nil {
		t.Fatalf("marshal big payload: %v", err)
	}
	if len(p) <= offloadThreshold {
		t.Fatalf("bigPayload len %d must exceed threshold %d", len(p), offloadThreshold)
	}
	return p
}

// ── offload (maybeOffload) ─────────────────────────────────────────────────────

func TestMaybeOffload_OversizePayload_PutsAndReturnsRef(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	off := newOffloader(store)
	payload := bigPayload(t)

	ref, offloaded, err := off.maybeOffload(context.Background(), payload)
	if err != nil {
		t.Fatalf("maybeOffload: %v", err)
	}
	if !offloaded {
		t.Fatal("a >256KiB payload must be offloaded")
	}

	// The returned payload is a $ref, not the original bytes.
	parsed, ok := isBlobRef(ref)
	if !ok {
		t.Fatalf("offloaded payload is not a $ref: %s", ref)
	}
	if parsed.Size != len(payload) {
		t.Errorf("$size = %d, want original length %d", parsed.Size, len(payload))
	}

	// The key is the content-addressed sha256, and the object landed in the store.
	wantKey := contentKey(payload)
	if parsed.Ref != objectStoreBucket+"/"+wantKey {
		t.Errorf("$ref = %q, want %q", parsed.Ref, objectStoreBucket+"/"+wantKey)
	}
	if !store.has(wantKey) {
		t.Errorf("payload was not PUT under content-addressed key %q", wantKey)
	}
	if puts, _ := store.count(); puts != 1 {
		t.Errorf("store PUTs = %d, want 1", puts)
	}
}

func TestMaybeOffload_SubThreshold_PassesThroughInline(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	off := newOffloader(store)
	payload := json.RawMessage(`{"task":"summarize","doc":"small"}`)

	out, offloaded, err := off.maybeOffload(context.Background(), payload)
	if err != nil {
		t.Fatalf("maybeOffload: %v", err)
	}
	if offloaded {
		t.Error("a sub-threshold payload must NOT be offloaded")
	}
	if string(out) != string(payload) {
		t.Errorf("inline payload changed: got %s, want %s", out, payload)
	}
	if puts, _ := store.count(); puts != 0 {
		t.Errorf("store PUTs = %d, want 0 (no offload)", puts)
	}
}

func TestMaybeOffload_StoreError_ReturnsTypedError(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	store.putErr = errors.New("minio down")
	off := newOffloader(store)

	_, _, err := off.maybeOffload(context.Background(), bigPayload(t))
	if err == nil {
		t.Fatal("expected a typed error when the store PUT fails")
	}
	if !strings.Contains(err.Error(), "object store") {
		t.Errorf("error %q should mention the object store", err)
	}
}

// ── rehydrate ──────────────────────────────────────────────────────────────────

func TestRehydrate_Ref_RestoresOriginal(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	off := newOffloader(store)
	original := bigPayload(t)

	// Offload it first so the store holds the object and we have the $ref.
	ref, _, err := off.maybeOffload(context.Background(), original)
	if err != nil {
		t.Fatalf("maybeOffload: %v", err)
	}

	got, wasRef, err := off.rehydrate(context.Background(), ref)
	if err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if !wasRef {
		t.Fatal("a $ref payload must be recognised as a reference")
	}
	if string(got) != string(original) {
		t.Errorf("rehydrated payload != original\n got %d bytes\nwant %d bytes", len(got), len(original))
	}
}

func TestRehydrate_NonRef_PassesThrough(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	off := newOffloader(store)
	payload := json.RawMessage(`{"task":"inline","doc":"x"}`)

	got, wasRef, err := off.rehydrate(context.Background(), payload)
	if err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if wasRef {
		t.Error("a non-$ref payload must NOT be treated as a reference")
	}
	if string(got) != string(payload) {
		t.Errorf("payload changed: got %s, want %s", got, payload)
	}
	if _, gets := store.count(); gets != 0 {
		t.Errorf("store GETs = %d, want 0 (nothing to rehydrate)", gets)
	}
}

func TestRehydrate_DanglingRef_Errors(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore() // empty: the referenced key does not exist.
	off := newOffloader(store)
	// A well-formed $ref to a key that was never PUT (a dangling reference).
	dangling, _ := json.Marshal(blobRef{Ref: objectStoreBucket + "/deadbeef", Size: 999})

	_, _, err := off.rehydrate(context.Background(), dangling)
	if err == nil {
		t.Fatal("a dangling $ref must return an error (so the consumer NACKs → DLQ)")
	}
}

func TestRehydrate_ForeignBucket_Errors(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	off := newOffloader(store)
	// A $ref naming a bucket that is NOT ours — must be rejected, not GET blindly.
	foreign, _ := json.Marshal(blobRef{Ref: "some-other-bucket/key", Size: 10})

	if _, _, err := off.rehydrate(context.Background(), foreign); err == nil {
		t.Fatal("a $ref to a foreign bucket must error")
	}
}

// ── isBlobRef recognition ──────────────────────────────────────────────────────

func TestIsBlobRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload string
		wantRef bool
	}{
		{"valid ref", `{"$ref":"ctxmesh-blobs/abc","$size":42}`, true},
		{"ref with whitespace", `  {"$ref":"ctxmesh-blobs/abc"}  `, true},
		{"agent object without ref", `{"task":"x","doc":"y"}`, false},
		{"empty ref string", `{"$ref":"","$size":0}`, false},
		{"json array", `[1,2,3]`, false},
		{"json string", `"a plain string"`, false},
		{"json null", `null`, false},
		{"empty", ``, false},
		// An agent object nesting $ref deeper is data, not a reference.
		{"nested ref key", `{"data":{"$ref":"x/y"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, got := isBlobRef(json.RawMessage(tc.payload))
			if got != tc.wantRef {
				t.Errorf("isBlobRef(%s) = %v, want %v", tc.payload, got, tc.wantRef)
			}
		})
	}
}

// ── publish integration (offload before encode) ────────────────────────────────

// TestPublishEnvelope_Offloads proves publishEnvelope offloads a >256KiB payload
// to the store and emits a CloudEvent whose envelope carries a tiny $ref (never
// the original bytes) — the whole point of offload: the broker sees a small event.
func TestPublishEnvelope_Offloads(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	off := newOffloader(store)

	env := sampleEnvelope("msg-offload", "worker-agent")
	env.Payload = bigPayload(t)
	originalLen := len(env.Payload)

	// Capture the event the "broker" receives.
	var gotEvent *cloudevents.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, err := cehttp.NewEventFromHTTPRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotEvent = e
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	if err := publishEnvelope(context.Background(), srv.Client(), srv.URL, "", env, off); err != nil {
		t.Fatalf("publishEnvelope: %v", err)
	}
	if gotEvent == nil {
		t.Fatal("broker did not receive a CloudEvent")
	}
	// The delivered event's envelope payload is a $ref, and it is far smaller than
	// the original payload (the event body stayed small).
	delivered, err := cloudEventToEnvelope(*gotEvent)
	if err != nil {
		t.Fatalf("decode delivered envelope: %v", err)
	}
	ref, ok := isBlobRef(delivered.Payload)
	if !ok {
		t.Fatalf("delivered payload is not a $ref: %s", delivered.Payload)
	}
	if ref.Size != originalLen {
		t.Errorf("$size = %d, want %d", ref.Size, originalLen)
	}
	if len(delivered.Payload) >= originalLen {
		t.Errorf("delivered payload (%d bytes) not smaller than original (%d)", len(delivered.Payload), originalLen)
	}
	if puts, _ := store.count(); puts != 1 {
		t.Errorf("store PUTs = %d, want 1", puts)
	}
}

// TestPublishEnvelope_StoreDown_TypedError proves a store failure on publish is a
// typed error and NO event is emitted to the broker (best-effort posture).
func TestPublishEnvelope_StoreDown_TypedError(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	store.putErr = errors.New("minio unreachable")
	off := newOffloader(store)

	env := sampleEnvelope("msg-offload-fail", "worker-agent")
	env.Payload = bigPayload(t)

	var delivered bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	err := publishEnvelope(context.Background(), srv.Client(), srv.URL, "", env, off)
	if err == nil {
		t.Fatal("expected a typed error when the store is down on publish")
	}
	if delivered {
		t.Error("no event must be emitted when offload fails (best-effort, no half-offloaded event)")
	}
}

// TestPublishEnvelope_NoOffloader_InlinePassthrough proves that with a nil
// offloader (OBJECT_STORE_ADDR absent) an oversize-but-sub-1MiB payload is
// carried inline unchanged — offload is a no-op, payloads pass through capped.
func TestPublishEnvelope_NoOffloader_InlinePassthrough(t *testing.T) {
	t.Parallel()
	env := sampleEnvelope("msg-noffload", "worker-agent")
	env.Payload = bigPayload(t) // > 256KiB but well under the 1MiB body cap.

	var gotEvent *cloudevents.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, err := cehttp.NewEventFromHTTPRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotEvent = e
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	if err := publishEnvelope(context.Background(), srv.Client(), srv.URL, "", env, nil); err != nil {
		t.Fatalf("publishEnvelope: %v", err)
	}
	delivered, err := cloudEventToEnvelope(*gotEvent)
	if err != nil {
		t.Fatalf("decode delivered envelope: %v", err)
	}
	if _, ok := isBlobRef(delivered.Payload); ok {
		t.Error("with no offloader the payload must stay inline, not become a $ref")
	}
	if string(delivered.Payload) != string(env.Payload) {
		t.Error("inline payload must be delivered unchanged")
	}
}

// ── consume integration (rehydrate before invoke) ──────────────────────────────

// newTestConsumerWithOffload builds an asyncConsumer with an offloader and an
// invoker that CAPTURES the payload the agent would see, so a test can assert the
// agent received the rehydrated original rather than a $ref.
func newTestConsumerWithOffload(off *offloader, invokeStatus int, invokeErr error) (*asyncConsumer, *[]byte) {
	var mu sync.Mutex
	var lastPayload []byte
	c := &asyncConsumer{
		cfg:     asyncConfig{SelfName: "worker-agent"},
		seen:    newFakeSeenSet(),
		tracer:  noop.NewTracerProvider().Tracer("test"),
		offload: off,
		invoke: func(_ context.Context, env envelope) (int, error) {
			mu.Lock()
			lastPayload = append([]byte(nil), env.Payload...)
			mu.Unlock()
			return invokeStatus, invokeErr
		},
	}
	return c, &lastPayload
}

// TestConsume_RehydratesRefBeforeInvoke proves the agent sees the ORIGINAL
// payload: a published $ref is GET from the store and put back before invoke.
func TestConsume_RehydratesRefBeforeInvoke(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	off := newOffloader(store)

	original := bigPayload(t)
	// Publish (offload) the payload so the store holds the object; capture the
	// resulting $ref envelope as the consumer would receive it.
	env := sampleEnvelope("msg-rehydrate", "worker-agent")
	env.Payload = original
	refPayload, offloaded, err := off.maybeOffload(context.Background(), env.Payload)
	if err != nil || !offloaded {
		t.Fatalf("setup offload: offloaded=%v err=%v", offloaded, err)
	}
	env.Payload = refPayload

	c, lastPayload := newTestConsumerWithOffload(off, http.StatusOK, nil)
	rr := httptest.NewRecorder()
	c.consume(rr, cloudEventRequest(t, env))

	if rr.Code != http.StatusNoContent {
		t.Fatalf("consume status = %d, want 204", rr.Code)
	}
	if string(*lastPayload) != string(original) {
		t.Errorf("agent saw payload of %d bytes, want the rehydrated original (%d bytes)",
			len(*lastPayload), len(original))
	}
	if _, ok := isBlobRef(*lastPayload); ok {
		t.Error("the agent must NEVER see a $ref — rehydrate must have replaced it")
	}
}

// TestConsume_DanglingRef_Nacks proves a $ref that cannot be rehydrated (store
// error / missing object) NACKs (502) so the broker DLQs it — the agent is never
// invoked with a broken payload.
func TestConsume_DanglingRef_Nacks(t *testing.T) {
	t.Parallel()
	store := newFakeObjectStore()
	store.getErr = errors.New("object not found") // every GET fails.
	off := newOffloader(store)

	env := sampleEnvelope("msg-dangling", "worker-agent")
	danglingRef, _ := json.Marshal(blobRef{Ref: objectStoreBucket + "/deadbeef", Size: 500 * 1024})
	env.Payload = danglingRef

	c, lastPayload := newTestConsumerWithOffload(off, http.StatusOK, nil)
	rr := httptest.NewRecorder()
	c.consume(rr, cloudEventRequest(t, env))

	if rr.Code != http.StatusBadGateway {
		t.Errorf("dangling $ref: status = %d, want 502 (NACK → DLQ)", rr.Code)
	}
	if len(*lastPayload) != 0 {
		t.Error("the agent must NOT be invoked when rehydrate fails")
	}
}

// TestConsume_NoOffloader_InlinePayloadUntouched proves that with offload
// disabled a normal inline payload is delivered to the agent unchanged (no
// rehydrate attempted).
func TestConsume_NoOffloader_InlinePayloadUntouched(t *testing.T) {
	t.Parallel()
	c, lastPayload := newTestConsumerWithOffload(nil, http.StatusOK, nil)
	env := sampleEnvelope("msg-inline", "worker-agent")

	rr := httptest.NewRecorder()
	c.consume(rr, cloudEventRequest(t, env))

	if rr.Code != http.StatusNoContent {
		t.Fatalf("consume status = %d, want 204", rr.Code)
	}
	if string(*lastPayload) != string(env.Payload) {
		t.Errorf("inline payload changed: got %s, want %s", *lastPayload, env.Payload)
	}
}
