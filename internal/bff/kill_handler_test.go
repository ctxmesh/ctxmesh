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

	"github.com/ctxmesh/ctxmesh/internal/controlplane/auditlog"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/killscope"
	"github.com/ctxmesh/ctxmesh/internal/run"
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

// ─── M151 hardening A1: GET /api/kills is caller-scoped ───────────────────────────────────────────
//
// The list used to run NO authorization at all. Proved live against the dev cluster with a
// zero-RBAC ServiceAccount token: /api/agents answered 403 and /api/kills answered 200, same
// token, same second — handing every authenticated caller the namespace, agent, tenant, the
// operator's free-text reason and the principal for every stop in the cluster. M151 is what made
// it reach: the frame polls this endpoint on every page and the Stops page lists it cluster-wide.
//
// The rule the tests below pin down is deliberately NOT "hide what you cannot read". A caller must
// never be told "nothing is stopped" when something that halts THEIR work is in force. So a
// tenant- or fleet-wide stop is always listed — with the reason and the principal stripped, since
// those are the parts that needed cluster-wide authority to see.

// nsAuthorizer allows `list agentdeployments` only in the named namespaces, and never cluster-wide
// (an empty Namespace is the cluster-scoped probe). Anything else — the kill verb included — is
// allowed, so these tests isolate the READ scoping rather than re-testing the kill gate.
type nsAuthorizer struct{ allowed map[string]bool }

func (a nsAuthorizer) Authorize(_ context.Context, _ client.Client, act authz.Action) error {
	if act.Resource == resourceAgentDeployments && act.Verb == "list" && act.Subresource == "" {
		if act.Namespace == "" || !a.allowed[act.Namespace] {
			return authz.ErrForbidden
		}
	}
	return nil
}

func listKills(t *testing.T, s *Server) (*httptest.ResponseRecorder, []activeKill) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/kills", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	var out []activeKill
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	}
	return rec, out
}

// seedStops records one stop at each level, through the store directly, so the list tests do not
// depend on the kill gate.
func seedStops(t *testing.T, ks killscope.Store) {
	t.Helper()
	ctx := context.Background()
	for _, sc := range []killscope.Scope{
		{Level: killscope.LevelNamespace, Namespace: "team-a"},
		{Level: killscope.LevelNamespace, Namespace: "team-b"},
		{Level: killscope.LevelAgent, Namespace: "team-b", Agent: "billing"},
		{Level: killscope.LevelFleet},
	} {
		require.NoError(t, ks.Kill(ctx, killscope.Kill{
			Scope: sc, Reason: "the incident narrative", Principal: "alice@example.com",
		}))
	}
}

// THE BAR: a caller who cannot read a namespace does not learn what is stopped inside it.
func TestListKills_NamespaceStopsAreScopedToWhatTheCallerCanRead(t *testing.T) {
	s, ks, _ := killAPIServer(t, nsAuthorizer{allowed: map[string]bool{"team-a": true}})
	seedStops(t, ks)

	rec, out := listKills(t, s)
	require.Equal(t, http.StatusOK, rec.Code)

	got := map[string]activeKill{}
	for _, k := range out {
		got[k.Scope] = k
	}
	assert.Contains(t, got, "namespace:team-a", "a namespace the caller CAN read stays visible")
	assert.NotContains(t, got, "namespace:team-b", "a namespace the caller cannot read must not leak")
	assert.NotContains(t, got, "agent:team-b/billing", "nor an agent stop inside that namespace")

	// The visible one keeps its full detail: the caller is entitled to it.
	assert.Equal(t, "the incident narrative", got["namespace:team-a"].Reason)
	assert.Equal(t, "alice@example.com", got["namespace:team-a"].Principal)
}

// A stop that halts the caller's own work is ALWAYS disclosed — silence there would be a worse
// failure than the leak. What is withheld is the free-text reason and who pressed it.
func TestListKills_WiderStopsAreDisclosedButRedacted(t *testing.T) {
	s, ks, _ := killAPIServer(t, nsAuthorizer{allowed: map[string]bool{"team-a": true}})
	seedStops(t, ks)
	require.NoError(t, ks.Kill(context.Background(), killscope.Kill{
		Scope:     killscope.Scope{Level: killscope.LevelTenant, Tenant: "acme"},
		Reason:    "acme is over budget",
		Principal: "bob@example.com",
	}))

	_, out := listKills(t, s)
	var fleet, tenant *activeKill
	for i := range out {
		switch out[i].Scope {
		case "fleet":
			fleet = &out[i]
		case "tenant:acme":
			tenant = &out[i]
		}
	}
	require.NotNil(t, fleet, "a fleet stop halts everyone's work and must never be hidden")
	require.NotNil(t, tenant, "nor a tenant stop")

	for _, k := range []*activeKill{fleet, tenant} {
		assert.Empty(t, k.Reason, "the operator's words needed cluster-wide authority to read")
		assert.Empty(t, k.Principal, "and so did the name of who pressed it")
		assert.NotEmpty(t, k.Level, "but the caller is still told THAT their work is halted")
	}
	assert.Equal(t, "acme", tenant.Tenant, "and how far the stop reaches")
}

// A cluster-wide reader — the operator this page was designed for — still sees everything, in full.
func TestListKills_ClusterWideReaderSeesEverythingInFull(t *testing.T) {
	s, ks, _ := killAPIServer(t, allowAuthorizer{})
	seedStops(t, ks)

	_, out := listKills(t, s)
	assert.Len(t, out, 4, "nothing is filtered from a caller who can read the whole cluster")
	for _, k := range out {
		assert.Equal(t, "the incident narrative", k.Reason)
		assert.Equal(t, "alice@example.com", k.Principal)
	}
}

// A caller who can read NOTHING learns only that a fleet-wide stop exists — never a namespace one,
// and never a word of why.
func TestListKills_ZeroRBACCallerLearnsNothingBeyondTheFleetHalt(t *testing.T) {
	s, ks, _ := killAPIServer(t, nsAuthorizer{allowed: map[string]bool{}})
	seedStops(t, ks)

	_, out := listKills(t, s)
	require.Len(t, out, 1, "only the fleet stop, which halts this caller too")
	assert.Equal(t, "fleet", out[0].Scope)
	assert.Empty(t, out[0].Reason)
	assert.Empty(t, out[0].Principal)
}
