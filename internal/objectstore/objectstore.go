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

// Package objectstore provides the durable object-store SPI for the
// Knowledge-Base feature (M68, ADR 0061 Fork 4). It is DISTINCT from the
// launcher's ephemeral blob-offload store (cmd/launcher/objectstore.go): the
// launcher store is content-addressed, GC'd, and carries async A2A payloads;
// this store is durable, never-GC'd, and carries KB source documents.
//
// TODO(m52 Theme M): unify launcher-offload + durable-knowledge ObjectStore SPIs
// once the two use cases are fully understood and the trade-offs are clearer.
//
// The MinIO implementation mirrors the launcher's minioStore client construction
// (same OBJECT_STORE_ADDR env gate, same in-cluster cred env vars) but uses a
// DISTINCT bucket (knowledgeBaseBucket) so the two stores coexist cleanly.
package objectstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	// knowledgeBaseBucket is the single durable bucket all KB documents land in.
	// Distinct from the launcher's "agent-engine-blobs" ephemeral bucket — the two
	// must never share a bucket so their lifecycle policies (GC vs never-GC) cannot
	// bleed across.
	knowledgeBaseBucket = "agent-engine-knowledge"

	// knowledgeKeyPrefix is the top-level key namespace all KB documents share.
	// Full key: knowledge/{namespace}/{kb-name}/{sanitized-document-name}
	knowledgeKeyPrefix = "knowledge"

	// opTimeout bounds a single object-store round-trip (PUT, GET, List). Same
	// discipline as the launcher's objectStoreOpTimeout — a slow/hung MinIO must
	// never wedge the BFF request path.
	opTimeout = 10 * time.Second
)

// ObjectInfo describes one object returned by List.
type ObjectInfo struct {
	Key         string
	Size        int64
	ContentType string
}

// ObjectStore is the durable KB object store SPI. It is satisfied by:
//   - minioStore (production — MinIO/S3 via OBJECT_STORE_ADDR)
//   - memObjectStore (tests — map-backed, no external dependency)
//
// All implementations must be safe for concurrent use.
type ObjectStore interface {
	// Put stores the content of r at key with the given size and content type.
	// Idempotent: re-putting the same key replaces the object. An error means the
	// store is unreachable or refused the write.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error

	// Get returns a streaming reader for the object at key. The caller is
	// responsible for closing the returned ReadCloser. A missing key or
	// unreachable store returns an error.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// List returns metadata for all objects whose key begins with prefix. The
	// slice is in undefined order and may be empty (not nil) when the prefix
	// matches nothing. Used by the ingestion executor (m68.6) to enumerate a
	// corpus's source documents.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)

	// DeletePrefix removes all objects whose key begins with prefix. Used by the
	// KB finalizer (m68.10) to clean up the durable bucket when a KnowledgeBase
	// is deleted. Idempotent: a prefix that matches nothing is not an error.
	DeletePrefix(ctx context.Context, prefix string) error
}

// KnowledgeKey returns the durable object-store key for a named document in an
// ORG-WIDE KB. The document name is sanitized: any path separators and ".."
// components are stripped so a hostile filename cannot traverse outside the KB's
// prefix.
//
// Format: knowledge/{namespace}/{kb}/{sanitized-document-name}
func KnowledgeKey(namespace, kb, documentName string) string {
	return path.Join(knowledgeKeyPrefix, namespace, kb, sanitizeName(documentName))
}

// KnowledgeKeyForSubject returns the durable object-store key for a named document in a
// PER-USER KB, nesting the uploading caller's server-derived subject hash as a path
// segment (m80.4, ADR 0061 Fork 3). This is the transport that carries the subject from
// the caller-scoped upload endpoint (which knows the caller) to the off-request ingestion
// executor (which does not re-derive it): SubjectFromKey recovers it at ingest-create.
//
// The subject-in-key layout (over S3 user-metadata) is deliberate: (1) user metadata is not
// portably returned by List on real S3 (ListObjectsV2 omits it), whereas the key always is;
// (2) a per-user KB where two users upload the same filename must NOT collide on one object
// key (which would let one user overwrite another's document + attribution) — the subject
// segment makes each user's namespace physically distinct. The subject is sanitized as a
// single path segment; a caller CANNOT influence it (it is the un-forgeable userGrantHash,
// never client-supplied). Org-wide keys (KnowledgeKey) are byte-for-byte unchanged.
//
// Format: knowledge/{namespace}/{kb}/{sanitized-subject}/{sanitized-document-name}
func KnowledgeKeyForSubject(namespace, kb, subject, documentName string) string {
	return path.Join(knowledgeKeyPrefix, namespace, kb, sanitizeName(subject), sanitizeName(documentName))
}

// SubjectFromKey recovers the per-user subject a KnowledgeKeyForSubject key nests under, or
// "" for an org-wide (KnowledgeKey) key. It parses the key against the KB's prefix: a key with
// EXACTLY one extra path segment before the filename is org-wide (subject ""); a key with TWO
// extra segments carries the subject as the first of them. Any deeper nesting is rejected (""),
// so a malformed key can never be misattributed. The caller (ingest-create) is responsible for
// fail-closed handling when it expected a subject and got "".
//
// This is the inverse of the two key builders and never trusts caller input beyond the
// structural shape; the subject it returns is only ever a value the server itself stamped.
func SubjectFromKey(namespace, kb, key string) string {
	prefix := KnowledgePrefix(namespace, kb)
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return "" // not under this KB's prefix — nothing to recover.
	}
	segs := strings.Split(rest, "/")
	// segs == [filename]            → org-wide (no subject segment).
	// segs == [subject, filename]   → per-user (subject is the leading segment).
	if len(segs) == 2 && segs[0] != "" && segs[1] != "" {
		return segs[0]
	}
	return ""
}

// KnowledgePrefix returns the object-store key prefix for ALL documents in a
// KB (org-wide AND every per-user subtree — the subject segment nests UNDER this
// prefix, so a single prefix List/DeletePrefix covers both layouts; the finalizer
// GC and source resolution need no per-layout branch). The prefix is suitable as
// an argument to List or DeletePrefix.
//
// Format: knowledge/{namespace}/{kb}/
func KnowledgePrefix(namespace, kb string) string {
	// path.Join strips the trailing slash; add it back so the prefix is exclusive
	// to this KB's subtree and does not accidentally match a sibling KB whose
	// name is a prefix of this one.
	return path.Join(knowledgeKeyPrefix, namespace, kb) + "/"
}

// sanitizeName cleans a document filename so it cannot path-traverse outside
// the KB prefix. It keeps only the base component of any path the caller
// supplies (stripping directories), collapses "..", and rejects empty names
// (falling back to "_unnamed").
func sanitizeName(name string) string {
	// Strip any directory component the caller might have embedded.
	clean := path.Base(path.Clean(name))
	// path.Base returns "." when name is empty or "."; use a fallback.
	if clean == "." || clean == ".." || clean == "" {
		return "_unnamed"
	}
	// Disallow any residual slash (should not happen after Base, but belt-and-suspenders).
	clean = strings.ReplaceAll(clean, "/", "_")
	return clean
}

// -------------------------------------------------------------------------
// MinIO/S3 implementation
// -------------------------------------------------------------------------

// minioKBStore is the production ObjectStore backed by a MinIO/S3 client.
// Bucket creation is lazy (first PUT) and idempotent — a fresh MinIO with no
// init creates the KB bucket on the first upload rather than failing the call.
type minioKBStore struct {
	client *minio.Client
	bucket string
}

// NewMinioStore builds a durable ObjectStore from OBJECT_STORE_ADDR and the
// injected credentials. Returns (nil, nil) when OBJECT_STORE_ADDR is unset —
// the caller registers an unconfigured state and serves honest 501 rather than
// panicking on a missing env var.
//
// The credential resolution mirrors the launcher's newMinioStore:
//   - OBJECT_STORE_ADDR → host:port (e.g. agent-engine-objectstore.agent-engine-system.svc:9000)
//   - OBJECT_STORE_ACCESS_KEY / OBJECT_STORE_SECRET_KEY → MinIO root credentials
//   - Secure:false — dev in-cluster MinIO uses plain HTTP, same posture as the launcher
//
// A construction error (bad addr) is returned so the caller can log it and
// surface 501 rather than crash. No credentials are hardcoded.
func NewMinioStore() (*minioKBStore, error) {
	addr := os.Getenv("OBJECT_STORE_ADDR")
	if addr == "" {
		return nil, nil //nolint:nilnil // Explicit: unconfigured state, not an error.
	}
	accessKey := os.Getenv("OBJECT_STORE_ACCESS_KEY")
	secretKey := os.Getenv("OBJECT_STORE_SECRET_KEY")
	client, err := minio.New(addr, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false, // dev in-cluster: plain HTTP, no TLS (mirrors launcher posture).
	})
	if err != nil {
		return nil, fmt.Errorf("build durable KB object-store client for %q: %w", addr, err)
	}
	return &minioKBStore{client: client, bucket: knowledgeBaseBucket}, nil
}

// ensureBucket creates the KB bucket if it does not exist. Idempotent and
// bounded by the caller's context. A concurrent creator race is handled by
// re-checking BucketExists after a MakeBucket failure.
func (s *minioKBStore) ensureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("check KB bucket %q: %w", s.bucket, err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
		// Handle the BucketExists→MakeBucket race — another caller won the race.
		if exists2, exErr := s.client.BucketExists(ctx, s.bucket); exErr == nil && exists2 {
			return nil
		}
		return fmt.Errorf("create KB bucket %q: %w", s.bucket, err)
	}
	return nil
}

func (s *minioKBStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	opCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	if err := s.ensureBucket(opCtx); err != nil {
		return err
	}
	opts := minio.PutObjectOptions{}
	if contentType != "" {
		opts.ContentType = contentType
	}
	_, err := s.client.PutObject(opCtx, s.bucket, key, r, size, opts)
	if err != nil {
		return fmt.Errorf("put KB object %q: %w", key, err)
	}
	return nil
}

func (s *minioKBStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	// MinIO GetObject returns a LAZY reader: the HTTP body is fetched on the caller's first Read,
	// which happens AFTER this function returns. So the op-timeout context must stay alive until the
	// returned reader is CLOSED — a `defer cancel()` here would cancel that context on return and the
	// caller's io.ReadAll would fail with "context canceled" (the bug the m68.14 live tier caught;
	// the mem-store twin never exercises this lazy-reader path). Cancel on Close instead.
	opCtx, cancel := context.WithTimeout(ctx, opTimeout)

	obj, err := s.client.GetObject(opCtx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("get KB object %q: %w", key, err)
	}
	// Trigger a stat to surface a missing key eagerly (MinIO GetObject is lazy
	// — a missing key errors only on first Read).
	if _, statErr := obj.Stat(); statErr != nil {
		_ = obj.Close()
		cancel()
		return nil, fmt.Errorf("get KB object %q: %w", key, statErr)
	}
	return &cancelOnCloseReader{ReadCloser: obj, cancel: cancel}, nil
}

// cancelOnCloseReader ties a context's cancel func to a reader's Close, so a lazily-read MinIO
// object's op-timeout context outlives the Get call that created it (until the caller finishes
// reading + closes). Without this, a `defer cancel()` in Get cancels the read (see Get above).
type cancelOnCloseReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnCloseReader) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func (s *minioKBStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	opCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	var out []ObjectInfo
	for obj := range s.client.ListObjects(opCtx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list KB objects under %q: %w", prefix, obj.Err)
		}
		out = append(out, ObjectInfo{
			Key:         obj.Key,
			Size:        obj.Size,
			ContentType: obj.ContentType,
		})
	}
	if out == nil {
		out = []ObjectInfo{}
	}
	return out, nil
}

func (s *minioKBStore) DeletePrefix(ctx context.Context, prefix string) error {
	opCtx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	// Collect all keys under the prefix then batch-remove them. MinIO's
	// RemoveObjects takes a channel; we close it after sending all keys.
	objectsCh := make(chan minio.ObjectInfo)
	go func() {
		defer close(objectsCh)
		for obj := range s.client.ListObjects(opCtx, s.bucket, minio.ListObjectsOptions{
			Prefix:    prefix,
			Recursive: true,
		}) {
			if obj.Err != nil {
				return // errors surfaces via RemoveObjects
			}
			objectsCh <- obj
		}
	}()

	for removeErr := range s.client.RemoveObjects(opCtx, s.bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		if removeErr.Err != nil {
			return fmt.Errorf("delete KB objects under prefix %q: %w", prefix, removeErr.Err)
		}
	}
	return nil
}

// -------------------------------------------------------------------------
// In-memory implementation (for tests)
// -------------------------------------------------------------------------

// MemObjectStore is a map-backed ObjectStore for tests. It is exported so the
// BFF upload tests can build a test Server with it wired into Options.
// All operations are safe for concurrent use.
type MemObjectStore struct {
	entries map[string]memEntry
}

type memEntry struct {
	data        []byte
	contentType string
}

// NewMemObjectStore returns a ready-to-use in-memory ObjectStore.
func NewMemObjectStore() *MemObjectStore {
	return &MemObjectStore{entries: make(map[string]memEntry)}
}

func (m *MemObjectStore) Put(_ context.Context, key string, r io.Reader, _ int64, contentType string) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read upload body for key %q: %w", key, err)
	}
	m.entries[key] = memEntry{data: data, contentType: contentType}
	return nil
}

func (m *MemObjectStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	e, ok := m.entries[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	return io.NopCloser(strings.NewReader(string(e.data))), nil
}

func (m *MemObjectStore) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	out := []ObjectInfo{}
	for k, e := range m.entries {
		if strings.HasPrefix(k, prefix) {
			out = append(out, ObjectInfo{Key: k, Size: int64(len(e.data)), ContentType: e.contentType})
		}
	}
	return out, nil
}

func (m *MemObjectStore) DeletePrefix(_ context.Context, prefix string) error {
	for k := range m.entries {
		if strings.HasPrefix(k, prefix) {
			delete(m.entries, k)
		}
	}
	return nil
}
