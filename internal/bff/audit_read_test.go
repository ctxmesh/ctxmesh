package bff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/controlplane/auditlog"
	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
)

// newAuditServer builds a caller-scoped BFF server with an audit memstore wired as the source and
// the given authorizer as the persona gate (nil ⇒ permissive). It mirrors wireTRStore: the store
// holds the data, the SSAR authorizer holds the RBAC, and callerClient authenticates the request.
func newAuditServer(t *testing.T, auth authz.Authorizer, seed ...auditlog.Entry) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	store := auditlog.NewMemStore()
	s.auditStore = store
	if auth != nil {
		s.authorizer = auth
	} else {
		s.authorizer = &recordingAuthorizer{}
	}
	for _, e := range seed {
		require.NoError(t, store.Append(context.Background(), e))
	}
	return s
}

// getAudit issues GET /api/audit?<rawQuery> with a caller token and decodes the body on 200.
func getAudit(t *testing.T, s *Server, rawQuery string) (AuditListResponse, int) {
	t.Helper()
	url := "/api/audit"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body AuditListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

// TestListAudit_NilStoreIs501 proves an un-provisioned audit surface degrades to 501 (not enabled),
// never a fake empty page — the CONTROLPLANE_DSN-absent deployment tells the console the truth.
func TestListAudit_NilStoreIs501(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	s.auditStore = nil // audit not enabled
	s.authorizer = &recordingAuthorizer{}

	_, code := getAudit(t, s, "")
	assert.Equal(t, http.StatusNotImplemented, code, "no store ⇒ 501, the audit surface is not enabled")
}

// TestListAudit_PersonaDeniedIs403 proves a caller without the `list auditlogs` grant gets 403 —
// never an empty list. The audit trail is operator-only; a developer/viewer persona is refused.
func TestListAudit_PersonaDeniedIs403(t *testing.T) {
	s := newAuditServer(t, &recordingAuthorizer{err: authz.ErrForbidden},
		auditlog.Entry{Actor: "alice", Action: "connect", Namespace: "ns1"})

	_, code := getAudit(t, s, "")
	assert.Equal(t, http.StatusForbidden, code, "no auditlogs persona ⇒ 403, never a leaked/empty page")
}

// TestListAudit_EmptyIsEmptyArray proves an authorized read of an empty store is a 200 with an empty
// JSON array (`[]`, not `null`) and no cursor — a clean "nothing yet", distinct from 403/501.
func TestListAudit_EmptyIsEmptyArray(t *testing.T) {
	s := newAuditServer(t, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"items":[]`, "empty page serializes items as [] not null")
	var body AuditListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Empty(t, body.NextCursor, "an empty (or last) page carries no cursor")
}

// TestListAudit_GatesOnListAuditlogs proves the persona SSAR is exactly `list auditlogs`, scoped to
// the requested namespace — the resource name MUST match the ClusterRole grant or the gate is a no-op.
func TestListAudit_GatesOnListAuditlogs(t *testing.T) {
	rec := &recordingAuthorizer{}
	s := newAuditServer(t, rec)

	_, code := getAudit(t, s, "namespace=team-a")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, authz.VerbList, rec.last.Verb)
	assert.Equal(t, resourceAuditLogs, rec.last.Resource, "the SSAR resource is the auditlogs virtual resource")
	assert.Equal(t, "team-a", rec.last.Namespace, "the SSAR is scoped to the requested namespace")
	assert.Equal(t, 1, rec.count, "exactly one persona gate, never per-row")
}

// TestListAudit_KeysetCursorRoundTrips proves the keyset page + cursor: 3 rows at limit=2 yield a
// first page of 2 (newest first) with a nextCursor, and following that cursor returns the last row
// with no further cursor — the append-only high-churn table paginates without offset drift.
func TestListAudit_KeysetCursorRoundTrips(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	s := newAuditServer(t, nil,
		auditlog.Entry{Actor: "a1", Action: "connect", Namespace: "ns1", OccurredAt: base},
		auditlog.Entry{Actor: "a2", Action: "connect", Namespace: "ns1", OccurredAt: base.Add(time.Second)},
		auditlog.Entry{Actor: "a3", Action: "connect", Namespace: "ns1", OccurredAt: base.Add(2 * time.Second)},
	)

	first, code := getAudit(t, s, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, first.Items, 2, "first page is full")
	require.NotEmpty(t, first.NextCursor, "a full page with more rows carries a cursor")
	assert.Equal(t, "a3", first.Items[0].Actor, "newest first (DESC by occurredAt)")
	assert.Equal(t, "a2", first.Items[1].Actor)

	second, code := getAudit(t, s, "limit=2&cursor="+first.NextCursor)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, second.Items, 1, "the cursor resumes exactly after the first page — no dupes, no gaps")
	assert.Equal(t, "a1", second.Items[0].Actor, "the oldest row is last")
	assert.Empty(t, second.NextCursor, "the final page carries no cursor")
}

// TestListAudit_FiltersByActorAndAction proves the query filters reach the store (actor + action),
// so the console's filter bar narrows server-side, not client-side over a giant page.
func TestListAudit_FiltersByActorAndAction(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	s := newAuditServer(t, nil,
		auditlog.Entry{Actor: "alice", Action: "connect", Namespace: "ns1", OccurredAt: base},
		auditlog.Entry{Actor: "bob", Action: "grant.revoke", Namespace: "ns1", OccurredAt: base.Add(time.Second)},
		auditlog.Entry{Actor: "alice", Action: "grant.create", Namespace: "ns1", OccurredAt: base.Add(2 * time.Second)},
	)

	byActor, code := getAudit(t, s, "actor=alice")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, byActor.Items, 2, "actor filter selects only alice's rows")
	for _, it := range byActor.Items {
		assert.Equal(t, "alice", it.Actor)
	}

	byAction, code := getAudit(t, s, "action=grant.revoke")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, byAction.Items, 1, "action filter selects only the revoke")
	assert.Equal(t, "bob", byAction.Items[0].Actor)
}
