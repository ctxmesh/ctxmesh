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

	"github.com/ctxmesh/agentry/internal/controlplane/alertstore"
	"github.com/ctxmesh/agentry/internal/controlplane/authz"
)

// newAlertsServer builds a caller-scoped BFF server with a fake alertstore.Store wired
// as the source and the given authorizer as the persona gate (nil ⇒ permissive). Mirrors
// newAuditServer: the store holds the data, the SSAR authorizer holds the RBAC.
func newAlertsServer(t *testing.T, auth authz.Authorizer, seed ...alertstore.Alert) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	store := alertstore.NewMemStore()
	s.alertStore = store
	if auth != nil {
		s.authorizer = auth
	} else {
		s.authorizer = &recordingAuthorizer{}
	}
	for i := range seed {
		a := seed[i]
		_, err := store.Append(context.Background(), a)
		require.NoError(t, err)
	}
	return s
}

// getAlerts issues GET /api/alerts?<rawQuery> with a caller token and decodes the body on 200.
func getAlerts(t *testing.T, s *Server, rawQuery string) (AlertListResponse, int) {
	t.Helper()
	url := "/api/alerts"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body AlertListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

// TestListAlerts_NilStoreIs501 proves an un-provisioned alerts surface degrades to 501 (not
// enabled), never a fake empty page — the CONTROLPLANE_DSN-absent deployment tells the console
// the truth.
func TestListAlerts_NilStoreIs501(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	s.alertStore = nil // alert store not enabled
	s.authorizer = &recordingAuthorizer{}

	_, code := getAlerts(t, s, "")
	assert.Equal(t, http.StatusNotImplemented, code, "no store ⇒ 501, the alerts feed is not enabled")
}

// TestListAlerts_PersonaDeniedIs403 proves a caller without the `list alertpolicies` grant
// gets 403 — never an empty list.
func TestListAlerts_PersonaDeniedIs403(t *testing.T) {
	s := newAlertsServer(t, &recordingAuthorizer{err: authz.ErrForbidden},
		alertstore.Alert{Namespace: "ns1", PolicyName: "p1", Condition: "c1", CondType: "budgetSoft"})

	_, code := getAlerts(t, s, "")
	assert.Equal(t, http.StatusForbidden, code, "no alertpolicies persona ⇒ 403, never a leaked/empty page")
}

// TestListAlerts_EmptyIsEmptyArray proves an authorized read of an empty store is a 200
// with an empty JSON array (`[]`, not `null`) — a clean "nothing yet", distinct from 403/501.
func TestListAlerts_EmptyIsEmptyArray(t *testing.T) {
	s := newAlertsServer(t, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/alerts", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"items":[]`, "empty feed serializes items as [] not null")
}

// TestListAlerts_GatesOnListAlertpolicies proves the persona SSAR is exactly `list
// alertpolicies`, scoped to the requested namespace.
func TestListAlerts_GatesOnListAlertpolicies(t *testing.T) {
	rec := &recordingAuthorizer{}
	s := newAlertsServer(t, rec)

	_, code := getAlerts(t, s, "namespace=team-a")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, authz.VerbList, rec.last.Verb)
	assert.Equal(t, resourceAlerts, rec.last.Resource, "the SSAR resource is alertpolicies")
	assert.Equal(t, "team-a", rec.last.Namespace, "the SSAR is scoped to the requested namespace")
	assert.Equal(t, 1, rec.count, "exactly one persona gate, never per-row")
}

// TestListAlerts_DTOMapping proves the AlertSummary wire DTO maps all alertstore.Alert
// fields correctly: policy/condition/type/value/message/firedAt/resolvedAt/firing.
func TestListAlerts_DTOMapping(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	resolved := base.Add(5 * time.Minute)
	s := newAlertsServer(t, nil,
		alertstore.Alert{
			Namespace:  "ns1",
			PolicyName: "pol-1",
			Condition:  "cond-a",
			Agent:      "ns1/agent-x",
			CondType:   "budgetSoft",
			Value:      "42.50",
			Message:    "cost exceeded threshold",
			FiredAt:    base,
			ResolvedAt: &resolved,
		},
		alertstore.Alert{
			Namespace:  "ns1",
			PolicyName: "pol-2",
			Condition:  "cond-b",
			CondType:   "regressionDetected",
			FiredAt:    base.Add(time.Minute),
			ResolvedAt: nil, // still firing
		},
	)

	body, code := getAlerts(t, s, "namespace=ns1")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2, "both seed rows are returned")

	// The store is newest-first; the second seed (later FiredAt) should be first.
	firing := body.Items[0]
	assert.Equal(t, "pol-2", firing.Policy)
	assert.Equal(t, "cond-b", firing.Condition)
	assert.Equal(t, "regressionDetected", firing.Type)
	assert.True(t, firing.Firing, "no resolvedAt ⇒ Firing=true")
	assert.Nil(t, firing.ResolvedAt)

	resolved_ := body.Items[1]
	assert.Equal(t, "pol-1", resolved_.Policy)
	assert.Equal(t, "cond-a", resolved_.Condition)
	assert.Equal(t, "budgetSoft", resolved_.Type)
	assert.Equal(t, "42.50", resolved_.Value)
	assert.Equal(t, "cost exceeded threshold", resolved_.Message)
	assert.Equal(t, "ns1/agent-x", resolved_.Agent)
	assert.False(t, resolved_.Firing, "resolvedAt set ⇒ Firing=false")
	require.NotNil(t, resolved_.ResolvedAt)
	assert.Equal(t, resolved.UTC().Format(time.RFC3339), *resolved_.ResolvedAt)
}

// TestListAlerts_NamespaceFilterIsForwarded proves the ?namespace= query param reaches
// the store (the store's List filters by namespace, the gate is namespace-scoped too).
func TestListAlerts_NamespaceFilterIsForwarded(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	s := newAlertsServer(t, nil,
		alertstore.Alert{Namespace: "ns1", PolicyName: "pol-a", Condition: "c1", CondType: "budgetSoft", FiredAt: base},
		alertstore.Alert{Namespace: "ns2", PolicyName: "pol-b", Condition: "c2", CondType: "budgetSoft", FiredAt: base.Add(time.Second)},
	)

	body, code := getAlerts(t, s, "namespace=ns1")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1, "namespace filter limits to ns1")
	assert.Equal(t, "pol-a", body.Items[0].Policy)
}
