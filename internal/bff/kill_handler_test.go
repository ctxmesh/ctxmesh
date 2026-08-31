package bff

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/agentry/internal/controlplane/auditlog"
	"github.com/ctxmesh/agentry/internal/controlplane/authz"
	"github.com/ctxmesh/agentry/internal/controlplane/killscope"
	"github.com/ctxmesh/agentry/internal/run"
)

// The kill-switch control surface (M146.5, ADR 0126 §5): its own verb, its own audit trail, and an
// un-kill that is a distinct authorized act rather than a silent expiry.

func killAPIServer(t *testing.T, auth authz.Authorizer) (*Server, killscope.Store, auditlog.Store) {
	t.Helper()
	ks, audit := killscope.NewMemStore(), auditlog.NewMemStore()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c), Scheme: testScheme(t), Auth: AllowAll{},
		Version: "test", Log: logr.Discard(), RunStore: run.NewMemStore(),
		KillScopes: ks, AuditStore: audit,
	})
	s.authorizer = auth // the field is private; tests inject a fake to drive allow/deny deterministically
	return s, ks, audit
}

func postKill(t *testing.T, s *Server, path string, body killRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// THE BAR: a caller WITHOUT the kill verb cannot stop anything — the control does not ride an existing
// persona.
func TestKillAPI_WithoutTheKillVerbIsForbidden(t *testing.T) {
	s, ks, audit := killAPIServer(t, denyAuthorizer{})

	rec := postKill(t, s, "/api/kill", killRequest{Level: "fleet", Reason: "incident"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "the kill verb is not implied by any persona")

	active, err := ks.Active(context.Background())
	require.NoError(t, err)
	assert.Empty(t, active, "a denied kill must not record a stop")

	// A DENIED attempt is audited too — an attempted fleet stop by someone without the verb is exactly
	// what a security review wants to see.
	page, err := audit.List(context.Background(), auditlog.Query{PageSize: 10})
	require.NoError(t, err)
	require.NotEmpty(t, page.Items, "a denied kill must still be audited")
	assert.Equal(t, "killswitch.kill", page.Items[0].Action)
	assert.Equal(t, "denied", page.Items[0].Outcome)
}

// With the verb, a kill records the stop AND audits it with principal + scope.
func TestKillAPI_KillRecordsAndAudits(t *testing.T) {
	s, ks, audit := killAPIServer(t, allowAuthorizer{})

	rec := postKill(t, s, "/api/kill", killRequest{
		Level: "namespace", Namespace: "team-a", Reason: "prompt-injection incident",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	active, err := ks.Active(context.Background())
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, "namespace:team-a", active[0].Scope.Key())
	assert.Equal(t, "prompt-injection incident", active[0].Reason)

	page, err := audit.List(context.Background(), auditlog.Query{PageSize: 10})
	require.NoError(t, err)
	require.NotEmpty(t, page.Items)
	assert.Equal(t, "killswitch.kill", page.Items[0].Action)
	assert.Equal(t, "success", page.Items[0].Outcome)
	assert.Equal(t, "namespace:team-a", page.Items[0].ResourceName)
}

// An un-kill is a DISTINCT audited act, and lifting a scope that was not killed is an honest no-op
// rather than a success that did nothing.
func TestKillAPI_UnkillIsSeparatelyAuditedAndHonest(t *testing.T) {
	s, ks, audit := killAPIServer(t, allowAuthorizer{})
	require.Equal(t, http.StatusOK,
		postKill(t, s, "/api/kill", killRequest{Level: "fleet", Reason: "r"}).Code)

	rec := postKill(t, s, "/api/kill/lift", killRequest{Level: "fleet"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"applied":true`)

	active, err := ks.Active(context.Background())
	require.NoError(t, err)
	assert.Empty(t, active)

	// Lifting again is applied:false — an honest no-op.
	rec = postKill(t, s, "/api/kill/lift", killRequest{Level: "fleet"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"applied":false`,
		"lifting a scope that was not killed must not report success")

	page, err := audit.List(context.Background(), auditlog.Query{PageSize: 10})
	require.NoError(t, err)
	var unkills int
	for _, e := range page.Items {
		if e.Action == "killswitch.unkill" {
			unkills++
		}
	}
	assert.Equal(t, 2, unkills, "every un-kill attempt is its own audit row — never a silent expiry")
}

// A reason is REQUIRED: an unexplained stop found at 3am is nearly as bad as no stop at all.
func TestKillAPI_AKillWithoutAReasonIsRejected(t *testing.T) {
	s, _, _ := killAPIServer(t, allowAuthorizer{})
	rec := postKill(t, s, "/api/kill", killRequest{Level: "fleet"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "reason is required")
}

// A scope whose identifiers disagree with its level is REJECTED, not normalised — a safety control must
// mean exactly what the operator wrote.
func TestKillAPI_AMalformedScopeIsRejectedNotNormalised(t *testing.T) {
	s, _, _ := killAPIServer(t, allowAuthorizer{})
	for _, tc := range []struct {
		name string
		req  killRequest
	}{
		{"agent level with no agent", killRequest{Level: "agent", Namespace: "team-a", Reason: "r"}},
		{"fleet level naming a namespace", killRequest{Level: "fleet", Namespace: "team-a", Reason: "r"}},
		{"tenant level naming an agent", killRequest{Level: "tenant", Tenant: "acme", Agent: "bot", Reason: "r"}},
		{"unknown level", killRequest{Level: "everything", Reason: "r"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, http.StatusBadRequest, postKill(t, s, "/api/kill", tc.req).Code)
		})
	}
}

type denyAuthorizer struct{}

func (denyAuthorizer) Authorize(context.Context, client.Client, authz.Action) error {
	return authz.ErrForbidden
}

type allowAuthorizer struct{}

func (allowAuthorizer) Authorize(context.Context, client.Client, authz.Action) error { return nil }

// ─── m146.6: declarative spec.suspend ─────────────────────────────────────────────────────────────

// The create edge reads spec.suspend STRAIGHT off the resolved deployment, so a suspend takes effect
// immediately rather than after the controller's next reconcile projects it.
func TestSuspend_ASuspendedAgentRefusesNewRuns(t *testing.T) {
	agent := readyAgent("bot", "team-a", "http://bot.team-a.svc.cluster.local")
	agent.Spec.Suspend = true
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	store := run.NewMemStore()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c), Scheme: testScheme(t), Auth: AllowAll{},
		Adapters: Adapters{Invoke: &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"x"}`)}},
		Version:  "test", Log: logr.Discard(), RunStore: store,
	})

	rec := postRun(t, s, InvokeRequest{Agent: "bot", Namespace: "team-a", Input: json.RawMessage(`{}`)})
	assert.Equal(t, http.StatusLocked, rec.Code)
	assert.Contains(t, rec.Body.String(), "suspended")
	assert.Empty(t, store.List(), "a suspended agent must not accumulate a queued backlog")
}

// An operator's imperative stop and a spec-projected suspend can name the SAME agent without clobbering
// each other — they are different intents. Lifting one must not lift the other, or an incident stop
// would silently un-suspend an agent someone deliberately turned off.
func TestSuspend_OperatorAndSpecStopsAreIndependent(t *testing.T) {
	ks := killscope.NewMemStore()
	ctx := context.Background()
	operator := killscope.Scope{Level: killscope.LevelAgent, Namespace: "team-a", Agent: "bot"}
	spec := killscope.Scope{Level: killscope.LevelAgent, Namespace: "team-a", Agent: "bot", Source: killscope.SourceSpec}

	require.NoError(t, ks.Kill(ctx, killscope.Kill{Scope: operator, Reason: "incident", Principal: "alice"}))
	require.NoError(t, ks.Kill(ctx, killscope.Kill{Scope: spec, Reason: "spec.suspend", Principal: "controller"}))

	active, err := ks.Active(ctx)
	require.NoError(t, err)
	assert.Len(t, active, 2, "the two intents coexist as distinct rows")

	lifted, err := ks.Unkill(ctx, operator)
	require.NoError(t, err)
	assert.True(t, lifted)

	active, err = ks.Active(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1, "lifting the operator stop must leave the spec suspend standing")
	assert.Equal(t, killscope.SourceSpec, active[0].Scope.Source)

	// ...and both still halt the same agent, because the MARKER key ignores the source.
	assert.Equal(t, operator.MarkerKey(), spec.MarkerKey(),
		"the accelerator is the same halt whichever intent recorded it")
}
