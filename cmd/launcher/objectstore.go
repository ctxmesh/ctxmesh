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

// Blob offload / rehydrate for the async A2A path (M7,
// specs/eventing-scaling.md §"Large payloads", §"Blob offload"). The M6
// platform envelope carried as a CloudEvent (cloudevent.go) must stay small: a
// broker/channel that has to buffer multi-hundred-KB events is a memory hazard,
// and the launcher's inbound CloudEvent body is capped at maxAsyncBody (1MiB).
// So when an envelope's serialized payload exceeds offloadThreshold (256KiB),
// the PUBLISHING launcher stores the payload verbatim in the dev object store
// (MinIO, config/objectstore/) under a CONTENT-ADDRESSED key (sha256 of the
// bytes) and replaces the envelope payload with a tiny reference:
//
//	{"$ref":"<bucket>/<key>","$size":<n>}
//
// The CONSUMING launcher rehydrates transparently: before the agent is invoked
// (before the X-A2A-Envelope header is stamped in newProxyInvoker), a payload
// that is a $ref is GET from the object store and the real bytes are put back so
// the agent sees the ORIGINAL payload — exactly as the sync path and a
// sub-threshold async payload would.
//
// Env gate (mirrors memory's MEMORY_BACKEND_ADDR): OBJECT_STORE_ADDR. When it is
// absent no offloader is constructed (nil) — offload is a no-op and payloads
// pass through capped exactly as before (a 256KiB..1MiB payload still fits the
// CloudEvent body; a >1MiB payload without a store is rejected upstream by the
// body cap, unchanged). Content-addressing makes PUT idempotent: the same bytes
// always map to the same key, so a redelivery / a re-publish overwrites rather
// than duplicates, and the seen-set dedupe (async.go) is unaffected.
//
// Failure posture (specs/eventing-scaling.md §"Edge cases: Oversize payload"):
//   - PUBLISH: MinIO down → publishEnvelope returns a TYPED error (best-effort,
//     like memory/tools) — the event is NOT emitted with a half-offloaded body.
//   - CONSUME: a dangling / failed $ref GET → rehydrate returns an error →
//     consume NACKs (502) so the broker retries and eventually DLQs it. A broken
//     payload is NEVER silently delivered to the agent.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/ctxmesh/agent-engine/internal/objectstore"
)

const (
	// offloadThreshold is the serialized-payload size above which the payload is
	// offloaded to the object store rather than carried inline in the CloudEvent.
	// 256KiB per specs/eventing-scaling.md §"Large payloads" (the ">256KB" row).
	// A payload at or below this stays inline (it comfortably fits the 1MiB
	// maxAsyncBody CloudEvent cap even after the envelope framing).
	offloadThreshold = 256 * 1024

	// objectStoreBucket is the single dev bucket every offloaded blob lands in.
	// One bucket keeps the reference format ("<bucket>/<key>") self-describing
	// and the dev MinIO trivially inspectable; content-addressing (sha256 key)
	// gives collision-free coexistence across agents/registries in dev.
	objectStoreBucket = "agent-engine-blobs"

	// objectStoreOpTimeout bounds a single object-store round-trip (PUT on
	// publish, GET on consume, bucket ensure). A slow/hung MinIO must never wedge
	// the publish or consume path beyond this — same discipline as the memory
	// endpoint's per-op bound (memoryOpTimeout).
	objectStoreOpTimeout = 5 * time.Second
)

// blobRef is the reference object that replaces an offloaded payload in the
// envelope. Ref is "<bucket>/<key>" (self-describing: which bucket, which
// content-addressed key); Size is the original payload length in bytes (an
// observability aid + a cheap sanity check on rehydrate). It (de)serializes to
// exactly {"$ref":"...","$size":n} — the "$"-prefixed keys mark this as the
// launcher's envelope-level indirection, distinct from an (opaque) agent payload.
type blobRef struct {
	Ref  string `json:"$ref"`
	Size int    `json:"$size"`
}

// ObjectStore is the minimal object-store surface the offloader needs. It is an
// interface (not *minio.Client) so unit tests drive publish/consume against an
// in-memory fake — including one that ERRORS, to prove the publish-typed-error
// and dangling-$ref→NACK behaviours — without a real MinIO.
type ObjectStore interface {
	// Put stores data under key (idempotent for content-addressed keys: the same
	// bytes overwrite rather than duplicate). An error means the store is
	// unreachable/refused — the caller surfaces it (publish fails typed).
	Put(ctx context.Context, key string, data []byte) error
	// Get returns the object stored under key. A missing key (dangling ref) or an
	// unreachable store is an error — the caller NACKs so the message DLQs rather
	// than delivering a broken payload.
	Get(ctx context.Context, key string) ([]byte, error)
}

// minioStore is the production ObjectStore backed by the MinIO S3 client. The
// client is constructed lazily (minio.New does not dial), so building it when
// the store is down is cheap and non-fatal — the first op surfaces the error.
// bucketReady guards a one-time lazy bucket creation so the FIRST offload PUT
// creates the dev bucket if an init did not (spec: "an init or the launcher
// lazily creates it").
type minioStore struct {
	client *minio.Client
	bucket string
}

// newMinioStore builds the production ObjectStore from OBJECT_STORE_ADDR and the
// injected dev credentials. addr is a host:port (e.g.
// agent-engine-objectstore.agent-engine-system.svc:9000); dev MinIO is plain
// HTTP in-cluster (Secure:false) — there is no TLS on the dev object store, same
// posture as the dev Valkey. A construction error (bad addr) is returned so the
// caller can log it and run WITHOUT offload rather than crash the launcher.
func newMinioStore(addr, accessKey, secretKey string) (*minioStore, error) {
	client, err := objectstore.NewMinioClient(addr, accessKey, secretKey) // M13: shared client construction.
	if err != nil {
		return nil, err
	}
	return &minioStore{client: client, bucket: objectStoreBucket}, nil
}

// ensureBucket creates the dev bucket if it does not exist. It is idempotent
// (BucketExists → skip) and bounded by the caller's context. Called lazily on
// the first PUT so a fresh dev MinIO with no init still gets the bucket.
func (s *minioStore) ensureBucket(ctx context.Context) error {
	return objectstore.EnsureBucket(ctx, s.client, s.bucket) // M13: shared idempotent-create logic.
}

func (s *minioStore) Put(ctx context.Context, key string, data []byte) error {
	if err := s.ensureBucket(ctx); err != nil {
		return err
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

func (s *minioStore) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	defer func() { _ = obj.Close() }()
	// ReadAll surfaces a dangling key here: MinIO GetObject is lazy, so a missing
	// object errors on the first Read, not on GetObject. Cap the read at
	// maxAsyncBody+1 so a corrupt/oversize object cannot exhaust memory (a
	// well-formed offloaded payload is always < maxAsyncBody by construction).
	data, err := io.ReadAll(io.LimitReader(obj, maxAsyncBody+1))
	if err != nil {
		return nil, fmt.Errorf("read object %q: %w", key, err)
	}
	return data, nil
}

// offloader carries the object store used by the publish/consume paths. It is
// nil when OBJECT_STORE_ADDR is unset (offload disabled — payloads pass through
// capped). Every field is read-only after construction and the ObjectStore is
// concurrency-safe, so it is safe to share across the ksvc goroutines.
type offloader struct {
	store  ObjectStore
	bucket string
}

// newOffloader builds an offloader around store, using the single dev bucket
// (objectStoreBucket) in the "<bucket>/<key>" reference form.
func newOffloader(store ObjectStore) *offloader {
	return &offloader{store: store, bucket: objectStoreBucket}
}

// contentKey is the content-addressed object key for data: the hex sha256 of the
// bytes. Identical payloads map to the identical key, making PUT idempotent (a
// re-publish / redelivery overwrites the same object) and the store naturally
// deduplicated across agents.
func contentKey(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

// maybeOffload returns the payload to place in the envelope. When the payload
// serialized length is at or below offloadThreshold, it is returned UNCHANGED
// (inline). When it exceeds the threshold, the payload is PUT to the object
// store under its content-addressed key and a {"$ref":...,"$size":n} object is
// returned to carry in the envelope instead.
//
// A store PUT failure is returned as an error — the caller (publishEnvelope)
// surfaces it typed and does NOT emit a half-offloaded event.
func (o *offloader) maybeOffload(ctx context.Context, payload json.RawMessage) (json.RawMessage, bool, error) {
	if len(payload) <= offloadThreshold {
		return payload, false, nil
	}
	key := contentKey(payload)

	opCtx, cancel := context.WithTimeout(ctx, objectStoreOpTimeout)
	defer cancel()
	if err := o.store.Put(opCtx, key, payload); err != nil {
		return nil, false, fmt.Errorf("offload payload to object store: %w", err)
	}

	ref := blobRef{Ref: o.bucket + "/" + key, Size: len(payload)}
	refJSON, err := json.Marshal(ref)
	if err != nil {
		// Marshalling a fixed two-field struct cannot realistically fail; surface
		// rather than swallow.
		return nil, false, fmt.Errorf("marshal blob reference: %w", err)
	}
	return refJSON, true, nil
}

// isBlobRef reports whether payload is a blob reference (a JSON object carrying a
// non-empty "$ref" string) rather than agent data. It is deliberately strict:
// only a top-level object with a "$ref" key is a reference; any other JSON
// (including an agent object that happens to nest "$ref" deeper) is treated as
// inline data and passed through untouched.
func isBlobRef(payload json.RawMessage) (blobRef, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return blobRef{}, false
	}
	var ref blobRef
	if err := json.Unmarshal(trimmed, &ref); err != nil {
		return blobRef{}, false
	}
	if ref.Ref == "" {
		return blobRef{}, false
	}
	return ref, true
}

// refKey extracts the object key from a "<bucket>/<key>" reference, verifying it
// names THIS offloader's bucket. A reference to a foreign bucket (or a malformed
// ref) is an error — we never GET from an unexpected bucket.
func (o *offloader) refKey(ref blobRef) (string, error) {
	prefix := o.bucket + "/"
	if len(ref.Ref) <= len(prefix) || ref.Ref[:len(prefix)] != prefix {
		return "", fmt.Errorf("blob reference %q does not name bucket %q", ref.Ref, o.bucket)
	}
	key := ref.Ref[len(prefix):]
	if key == "" {
		return "", fmt.Errorf("blob reference %q has an empty key", ref.Ref)
	}
	return key, nil
}

// rehydrate replaces a $ref payload with the original bytes fetched from the
// object store. A payload that is NOT a $ref is returned unchanged (a
// sub-threshold async payload, or a payload from a producer that never
// offloaded). A dangling / failed GET is returned as an error so the consumer
// NACKs (message DLQs) rather than delivering a broken payload — the agent never
// sees a $ref or a truncated object.
func (o *offloader) rehydrate(ctx context.Context, payload json.RawMessage) (json.RawMessage, bool, error) {
	ref, ok := isBlobRef(payload)
	if !ok {
		return payload, false, nil
	}
	key, err := o.refKey(ref)
	if err != nil {
		return nil, false, err
	}

	opCtx, cancel := context.WithTimeout(ctx, objectStoreOpTimeout)
	defer cancel()
	data, err := o.store.Get(opCtx, key)
	if err != nil {
		return nil, false, fmt.Errorf("rehydrate payload from object store: %w", err)
	}
	// Guard against a corrupt/oversize object exceeding the async body cap (the
	// LimitReader in Get reads one extra byte to detect this).
	if len(data) > maxAsyncBody {
		return nil, false, fmt.Errorf("rehydrated object %q exceeds %d-byte cap", key, maxAsyncBody)
	}
	return json.RawMessage(data), true, nil
}
