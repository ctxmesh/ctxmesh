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

// Package controlplane is the shared foundation for the CRD→Postgres migration (ADR 0042): the common
// list/page shapes, error sentinels, DB open + goose migration runner. Each moved entity gets its own
// per-entity store package (e.g. controlplane/promptversion) with a Postgres impl + an in-memory twin
// that both pass one conformance suite — mirroring the internal/run + internal/credpostgres patterns.
package controlplane

import (
	"encoding/base64"
	"errors"
	"strconv"
)

// ErrNotFound is returned when a lookup finds no row. ErrConflict signals an optimistic-concurrency
// version mismatch (a concurrent write moved the row on since the caller read it).
var (
	ErrNotFound = errors.New("controlplane: not found")
	ErrConflict = errors.New("controlplane: version conflict")
)

// DefaultPageSize bounds an unpaginated list; MaxPageSize caps a caller-supplied one.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ListOptions is the shared query shape for the console's list endpoints — the rich filtering/paging the
// K8s API's label-selector lists could not give us (a motivation for the migration, ADR 0042). Offset
// pagination (not keyset) is deliberate: the catalog tables are low-churn and the console needs a total
// count + "page N of M", which keyset makes awkward.
type ListOptions struct {
	Namespace string            // "" = all namespaces (admin/cluster scope)
	Labels    map[string]string // label-equality filter, AND-ed (like a k8s label selector)
	Search    string            // case-insensitive substring match on the name (optional)
	PageSize  int               // <=0 → DefaultPageSize; capped at MaxPageSize
	PageToken string            // opaque; "" = first page (see EncodePageToken)
	SortBy    string            // entity-defined column; "" = the entity default
	SortDesc  bool
}

// Page is a page of results plus the total matching count and an opaque token for the next page.
type Page[T any] struct {
	Items    []T
	Total    int64
	NextPage string // "" = last page
}

// Limit resolves the effective page size (default + cap applied).
func (o ListOptions) Limit() int {
	switch {
	case o.PageSize <= 0:
		return DefaultPageSize
	case o.PageSize > MaxPageSize:
		return MaxPageSize
	default:
		return o.PageSize
	}
}

// Offset decodes the opaque PageToken to a row offset (0 on empty/invalid — a bad token is a fresh page,
// never an error, so a stale token can't wedge a list).
func (o ListOptions) Offset() int {
	if o.PageToken == "" {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(o.PageToken)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// EncodePageToken makes the opaque next-page token for a given absolute offset. Kept opaque so the
// pagination impl (offset today, keyset later) can change without a public API break.
func EncodePageToken(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// NextToken returns the token for the page after [offset, offset+limit) given total, or "" when the page
// is the last one.
func NextToken(offset, limit int, total int64) string {
	if int64(offset+limit) >= total {
		return ""
	}
	return EncodePageToken(offset + limit)
}
