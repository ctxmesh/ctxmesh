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

// Package auditlog is the control-plane store for the audit surface (ADR 0056, M63 / PROD-2): the
// queryable projection of the audit trail that `internal/audit` used to emit only to structured logs. It
// serves BOTH the controller's CRD-mutation trail AND the BFF's security events (grant/connect/denials)
// as one discriminated Entry. Append-only (no UPDATE); the retention pruner is the only bounded delete.
//
// Two impls — Postgres (pgstore) + an in-memory twin (memstore) — pass one conformance suite, the
// per-entity pattern (internal/controlplane/toolregistry). UNLIKE the other stores it uses a KEYSET
// cursor (on occurred_at,id), NOT controlplane.ListOptions' offset: audit_log is the highest-churn
// append-only table, where OFFSET + count(*) degrades and the count drifts under concurrent inserts
// (ADR 0056 §1).
package auditlog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Entry is one audit record. A controller CRD-mutation and a BFF security event share this shape (the
// BFF-only / controller-only fields are simply zero on the other source).
type Entry struct {
	ID              int64          // assigned by the store on Append (0 on input)
	OccurredAt      time.Time      // event time; the store defaults it to now() (UTC) when zero
	Source          string         // "controller" | "bff"
	Actor           string         // the real principal (managedFields field-manager, or the ADR-0011 caller)
	ActorKind       string         // "user" | "controller" | "system"
	Action          string         // "create"|"update"|"delete" | "grant.create"|"grant.revoke"|"connect"…
	ResourceKind    string         // CRD Kind or BFF resource type
	ResourceName    string         //
	Namespace       string         // "" = cluster-scoped / non-namespaced BFF event
	Outcome         string         // "success" | "denied" | "error" (defaults "success")
	TraceID         string         // links the row to a run/trace
	Detail          map[string]any // arbitrary structured context → JSONB
	ResourceVersion string         // controller rows: the mutated object's resourceVersion
	// DedupKey collapses cross-replica duplicate observations (ADR 0056 §3): a controller row sets a
	// DETERMINISTIC key (ControllerDedupKey) so N replica observations become one row; a BFF row leaves it
	// empty and the store mints a UUID (single-writer → never dedupes). Insert is ON CONFLICT DO NOTHING.
	DedupKey string
}

// Query is the keyset list query for GET /api/audit. Filters are AND-ed; empty means "no filter".
type Query struct {
	Namespace    string    // "" = all (requires cluster-wide audit-read; see the read handler)
	Actor        string    // exact match
	Action       string    // exact match
	ResourceKind string    // exact match
	From         time.Time // occurred_at >= From (zero = unbounded)
	To           time.Time // occurred_at <= To (zero = unbounded)
	PageSize     int       // <=0 → DefaultPageSize; capped at MaxPageSize
	Cursor       string    // opaque keyset cursor ("" = first page)
}

// Page is a page of newest-first entries + the opaque cursor for the next (older) page ("" = last page).
type Page struct {
	Items      []Entry
	NextCursor string
}

const (
	// DefaultPageSize / MaxPageSize bound a list (own constants — the audit surface is its own contract).
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Store is the append-only audit repository. Append MUST be idempotent on DedupKey. List returns
// newest-first, keyset-paged.
type Store interface {
	Append(ctx context.Context, e Entry) error
	List(ctx context.Context, q Query) (Page, error)
	// PruneBefore deletes rows older than cutoff (the retention pruner, ADR 0056 §5) and returns the count.
	PruneBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// ControllerDedupKey builds the deterministic idempotency key for a controller CRD-mutation row, so the
// same mutation observed on N manager replicas collapses to one stored row. resourceVersion is stable per
// mutation and survives deletes (the informer tombstone carries it).
func ControllerDedupKey(source, resourceKind, namespace, resourceName, resourceVersion, action string) string {
	raw := strings.Join([]string{source, resourceKind, namespace, resourceName, resourceVersion, action}, "\x00")
	sum := sha256.Sum256([]byte(raw))
	return "c:" + base64.RawURLEncoding.EncodeToString(sum[:16])
}

// normalize fills the server-owned defaults so both stores agree: UTC timestamp (now when zero),
// non-empty Outcome/ActorKind, a non-nil Detail, and a minted dedup key for a BFF/single-writer row
// that supplied none (a fresh random key never dedupes — correct for a single writer).
func normalize(e Entry) Entry {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	} else {
		e.OccurredAt = e.OccurredAt.UTC()
	}
	if e.Outcome == "" {
		e.Outcome = "success"
	}
	if e.ActorKind == "" {
		e.ActorKind = "system"
	}
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	if e.DedupKey == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		e.DedupKey = "b:" + base64.RawURLEncoding.EncodeToString(b[:])
	}
	return e
}

// clampPageSize normalises a caller-supplied page size.
func clampPageSize(n int) int {
	if n <= 0 {
		return DefaultPageSize
	}
	if n > MaxPageSize {
		return MaxPageSize
	}
	return n
}

// cursor encodes the keyset position (the last row's occurred_at + id) as an opaque token. The next page
// is everything strictly older: (occurred_at, id) < (ts, id) under the ORDER BY occurred_at DESC, id DESC.
type cursor struct {
	TS time.Time
	ID int64
}

func encodeCursor(c cursor) string {
	// Nanosecond-precision RFC3339 + id; base64 so it's opaque + URL-safe.
	raw := c.TS.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(c.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(token string) (cursor, error) {
	if token == "" {
		return cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursor{}, fmt.Errorf("auditlog: malformed cursor")
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return cursor{}, fmt.Errorf("auditlog: malformed cursor")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return cursor{}, fmt.Errorf("auditlog: malformed cursor timestamp")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return cursor{}, fmt.Errorf("auditlog: malformed cursor id")
	}
	return cursor{TS: ts.UTC(), ID: id}, nil
}

// detailJSON marshals Detail to a JSONB-ready []byte, always a valid object ("{}" when nil/empty).
func detailJSON(d map[string]any) ([]byte, error) {
	if len(d) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(d)
}
