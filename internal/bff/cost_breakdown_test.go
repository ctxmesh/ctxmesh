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

package bff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/costrollup"
)

// --- parseAgentTag round-trip tests ------------------------------------------

// TestParseAgentTagRoundTrips proves parseAgentTag is the exact inverse of
// agentRunTag: round-tripping through agentRunTag then parseAgentTag recovers
// the original (ns, name) for all tag formats the producer emits.
func TestParseAgentTagRoundTrips(t *testing.T) {
	cases := []struct {
		ns   string
		name string
	}{
		{"default", "foo"},
		{"", "bare"},
		{"some-ns", "my-agent"},
		// Name with no slash in a namespace — not a special case, but verify.
		{"prod", "agent-v2"},
	}

	for _, c := range cases {
		tag := agentRunTag(c.ns, c.name)
		gotNs, gotName, ok := parseAgentTag(tag)
		require.True(t, ok, "parseAgentTag(%q) must return ok=true for %q/%q", tag, c.ns, c.name)
		assert.Equal(t, c.ns, gotNs, "namespace round-trip failed for %q/%q", c.ns, c.name)
		assert.Equal(t, c.name, gotName, "name round-trip failed for %q/%q", c.ns, c.name)
	}
}

// TestParseAgentTagRejectsNonAgentTags verifies parseAgentTag returns ok=false
// for strings that are not agent tags.
func TestParseAgentTagRejectsNonAgentTags(t *testing.T) {
	bad := []string{"", "notanagent", "model:gpt-4", "agent:", "env:prod"}
	for _, tag := range bad {
		_, _, ok := parseAgentTag(tag)
		assert.False(t, ok, "parseAgentTag(%q) should return ok=false", tag)
	}
}

// TestParseAgentTagBareNameHasEmptyNs: "agent:<name>" (no slash) → ns="" (the
// bare-name case from agentRunTag when namespace is "").
func TestParseAgentTagBareNameHasEmptyNs(t *testing.T) {
	ns, name, ok := parseAgentTag("agent:bare")
	require.True(t, ok)
	assert.Equal(t, "", ns)
	assert.Equal(t, "bare", name)
}

// --- CostBreakdown adapter tests (via httptest fake) -------------------------

// fakeLangfuseBreakdown spins a Langfuse stub that always returns the provided
// traces corpus for /api/public/traces.
func fakeLangfuseBreakdown(t *testing.T, traces []lfTrace) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/traces" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(lfTracesResponse{Data: traces})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCostBreakdownTotalFromDailyMetrics: the Cost page's window TOTAL comes from
// the SAME daily-metrics aggregation the dashboard uses (m24.4 — so the two cost
// surfaces never contradict), while the per-agent breakdown stays trace-derived.
func TestCostBreakdownTotalFromDailyMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/public/traces": // per-agent source: one agent trace at 0.10
			_ = json.NewEncoder(w).Encode(lfTracesResponse{Data: []lfTrace{
				{ID: "t1", Name: "run", TotalCost: 0.10, Tags: []string{"agent:default/echo"}},
			}})
		case "/api/public/metrics/daily": // the window total the dashboard sees: 7.50
			_ = json.NewEncoder(w).Encode(lfDailyMetricsResponse{Data: []lfDailyMetric{
				{
					Date: "2026-07-01", CountObservations: 3, TotalCost: 7.50,
					Usage: []lfDailyModelUsage{{Model: "claude", TotalUsage: 900, TotalCost: 7.50}},
				},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a := newTestLangfuse(t, srv.URL)

	resp, err := a.CostBreakdown(context.Background(), 10, "")
	require.NoError(t, err)
	// TOTAL tracks metrics/daily (7.50), NOT the trace sum (0.10) — reconciled.
	assert.InDelta(t, 7.50, resp.Total.TotalCostUSD, 1e-9)
	assert.Equal(t, int64(900), resp.Total.TotalTokens)
	// Per-agent breakdown is still trace-derived.
	require.Len(t, resp.Agents, 1)
	assert.Equal(t, "echo", resp.Agents[0].AgentName)
	assert.InDelta(t, 0.10, resp.Agents[0].TotalCostUSD, 1e-9)
}

// TestCostBreakdownGroupsMixedCorpus is the core correctness test: 2 named
// agents across namespaces plus 1 untagged trace → correct per-agent
// aggregates, the "(untagged)" bucket, the window total, and cost-desc sort.
func TestCostBreakdownGroupsMixedCorpus(t *testing.T) {
	corpus := []lfTrace{
		// default/alpha: 2 traces, total cost 0.5+0.3=0.8, tokens 100+200=300
		{ID: "a1", Tags: []string{"agent:default/alpha"}, TotalCost: 0.5, Usage: &lfUsage{TotalTokens: 100}},
		{ID: "a2", Tags: []string{"agent:default/alpha"}, TotalCost: 0.3, Usage: &lfUsage{TotalTokens: 200}},
		// other/beta: 1 trace, total cost 1.0, tokens 500
		{ID: "b1", Tags: []string{"agent:other/beta"}, TotalCost: 1.0, Usage: &lfUsage{TotalTokens: 500}},
		// untagged: no agent tag, cost 0.1, tokens 50
		{ID: "u1", TotalCost: 0.1, TotalTokens: 50},
	}
	srv := fakeLangfuseBreakdown(t, corpus)
	a := newTestLangfuse(t, srv.URL)

	resp, err := a.CostBreakdown(context.Background(), 10, "")
	require.NoError(t, err)

	// Non-nil agents slice.
	require.NotNil(t, resp.Agents)

	// 3 buckets: other/beta (1.0), default/alpha (0.8), (untagged) (0.1).
	require.Len(t, resp.Agents, 3)

	// Sorted by cost desc.
	assert.Equal(t, "other", resp.Agents[0].AgentNs)
	assert.Equal(t, "beta", resp.Agents[0].AgentName)
	assert.InDelta(t, 1.0, resp.Agents[0].TotalCostUSD, 1e-9)
	assert.Equal(t, int64(500), resp.Agents[0].TotalTokens)
	assert.Equal(t, 1, resp.Agents[0].RunCount)

	assert.Equal(t, "default", resp.Agents[1].AgentNs)
	assert.Equal(t, "alpha", resp.Agents[1].AgentName)
	assert.InDelta(t, 0.8, resp.Agents[1].TotalCostUSD, 1e-9)
	assert.Equal(t, int64(300), resp.Agents[1].TotalTokens)
	assert.Equal(t, 2, resp.Agents[1].RunCount)

	// Untagged bucket.
	assert.Equal(t, "", resp.Agents[2].AgentNs)
	assert.Equal(t, "(untagged)", resp.Agents[2].AgentName)
	assert.InDelta(t, 0.1, resp.Agents[2].TotalCostUSD, 1e-9)
	assert.Equal(t, int64(50), resp.Agents[2].TotalTokens)
	assert.Equal(t, 1, resp.Agents[2].RunCount)

	// Window total (sum of all 4 traces).
	assert.InDelta(t, 1.9, resp.Total.TotalCostUSD, 1e-9)
	assert.Equal(t, int64(850), resp.Total.TotalTokens)
	assert.Equal(t, int64(4), resp.Total.Observations)

	// No next page (3 agents, limit 10).
	assert.Equal(t, "", resp.NextCursor)
}

// TestCostBreakdownCursorPagination: limit=1 pages through all agents one at a
// time. Round-trips nextCursor → cursor so each page is reached correctly.
func TestCostBreakdownCursorPagination(t *testing.T) {
	corpus := []lfTrace{
		{ID: "a1", Tags: []string{"agent:default/alpha"}, TotalCost: 0.8},
		{ID: "b1", Tags: []string{"agent:other/beta"}, TotalCost: 1.0},
		{ID: "c1", Tags: []string{"agent:ns/gamma"}, TotalCost: 0.5},
	}
	srv := fakeLangfuseBreakdown(t, corpus)
	a := newTestLangfuse(t, srv.URL)

	// Page 1 (limit=1, cursor="").
	p1, err := a.CostBreakdown(context.Background(), 1, "")
	require.NoError(t, err)
	require.Len(t, p1.Agents, 1)
	assert.NotEmpty(t, p1.NextCursor, "page 1 must have a next cursor")
	first := p1.Agents[0].AgentName

	// Page 2.
	p2, err := a.CostBreakdown(context.Background(), 1, p1.NextCursor)
	require.NoError(t, err)
	require.Len(t, p2.Agents, 1)
	assert.NotEmpty(t, p2.NextCursor, "page 2 must have a next cursor")
	assert.NotEqual(t, first, p2.Agents[0].AgentName, "page 2 must be a different agent")

	// Page 3 (last page).
	p3, err := a.CostBreakdown(context.Background(), 1, p2.NextCursor)
	require.NoError(t, err)
	require.Len(t, p3.Agents, 1)
	assert.Equal(t, "", p3.NextCursor, "last page must have no next cursor")

	// All 3 unique agent names are covered across pages.
	names := []string{p1.Agents[0].AgentName, p2.Agents[0].AgentName, p3.Agents[0].AgentName}
	assert.ElementsMatch(t, []string{"alpha", "beta", "gamma"}, names)
}

// TestCostBreakdownBadCursorIs400: a non-integer cursor → ErrBadParam.
func TestCostBreakdownBadCursorIs400(t *testing.T) {
	srv := fakeLangfuseBreakdown(t, []lfTrace{})
	a := newTestLangfuse(t, srv.URL)

	_, err := a.CostBreakdown(context.Background(), 10, "notanint")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadParam)
}

// TestCostBreakdownEmptyIsValid: empty window → {agents:[], total:{...}, nextCursor:""}.
func TestCostBreakdownEmptyIsValid(t *testing.T) {
	srv := fakeLangfuseBreakdown(t, []lfTrace{})
	a := newTestLangfuse(t, srv.URL)

	resp, err := a.CostBreakdown(context.Background(), 10, "")
	require.NoError(t, err)
	assert.NotNil(t, resp.Agents, "agents must be [] not nil on empty window")
	assert.Empty(t, resp.Agents)
	assert.Equal(t, "", resp.NextCursor)
	assert.InDelta(t, 0.0, resp.Total.TotalCostUSD, 1e-9)
}

// TestCostBreakdownUpstreamErrorSurfaces: a non-200 Langfuse response surfaces
// as an error (the handler maps it to 502).
func TestCostBreakdownUpstreamErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	a := newTestLangfuse(t, srv.URL)

	_, err := a.CostBreakdown(context.Background(), 10, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")
}

// --- GET /api/cost/breakdown handler tests -----------------------------------

// costTestTenant is the default tenant used by the cost handler tests. Its
// spec.namespaces owns the namespaces the existing fixtures tag their agents
// with (default, other, ns) so the ADR 0077 breakdown filter keeps them — the
// dedicated isolation tests (below) use their own narrowly-scoped tenants.
const costTestTenant = "acme"

// costTestTenantObject returns the default Tenant CR the cost handler tests seed
// into the fake caller client so the ADR 0077 breakdown filter resolves.
func costTestTenantObject() *agentsv1alpha1.Tenant {
	return &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: costTestTenant},
		Spec:       agentsv1alpha1.TenantSpec{Namespaces: []string{"default", "other", "ns"}},
	}
}

// newPermissiveCostServer builds a BFF Server with a permissive (always-allow)
// SSAR authorizer and a caller-client factory, wired with the given adapters.
// Use this for handler tests that need to reach past the costrollups gate into
// the data/error paths (param validation, adapter errors, response shape).
//
// It seeds the default costTestTenant CR into the fake caller client and wires a
// rollup store (ADR 0077 — /api/cost + /api/cost/breakdown are tenant-scoped and
// require the store + a resolvable Tenant), so the standard `?tenant=acme` request
// reaches 200.
func newPermissiveCostServer(t *testing.T, a Adapters) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(costTestTenantObject()).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      a,
		Version:       "test",
		Log:           logr.Discard(),
	})
	s.authorizer = &recordingAuthorizer{} // always allows
	s.rollupStore = costrollup.NewMemStore()
	return s
}

// serveWithToken issues GET <url> with a bearer token so callerClient succeeds.
func serveWithToken(t *testing.T, s *Server, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHandlerCostBreakdownRequiresByAgent: missing or non-"agent" `by` → 400.
// The `by` check is AFTER the ?tenant= check (ADR 0077), so all requests here
// carry ?tenant= to isolate the `by`-param contract.
func TestHandlerCostBreakdownRequiresByAgent(t *testing.T) {
	s := newPermissiveCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{}})

	// Missing ?by entirely → 400.
	w := serveWithToken(t, s, "/api/cost/breakdown?tenant="+costTestTenant)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "by")

	// ?by=model (unsupported) → 400 with the unsupported value in the message.
	w2 := serveWithToken(t, s, "/api/cost/breakdown?by=model&tenant="+costTestTenant)
	assert.Equal(t, http.StatusBadRequest, w2.Code)
	assert.Contains(t, w2.Body.String(), "model")

	// ?by=agent → 200.
	w3 := serveWithToken(t, s, "/api/cost/breakdown?by=agent&tenant="+costTestTenant)
	assert.Equal(t, http.StatusOK, w3.Code)
}

// TestHandlerCostBreakdownLangfuseAbsent501: Langfuse not wired → 501.
func TestHandlerCostBreakdownLangfuseAbsent501(t *testing.T) {
	s := serverWithAdapters(t, Adapters{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/cost/breakdown?by=agent", nil))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

// TestHandlerCostBreakdownUpstream502: adapter error → 502.
func TestHandlerCostBreakdownUpstream502(t *testing.T) {
	s := newPermissiveCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{breakdownErr: assert.AnError}})
	w := serveWithToken(t, s, "/api/cost/breakdown?by=agent&tenant="+costTestTenant)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

// TestHandlerCostBreakdownDegradesCalmly: ErrUpstreamUnavailable (the trace store
// is slow/circuit-broken) → a CALM 200 with empty agents + a notice, NOT a red 502
// (m23.6 — the whole reason wiring Langfuse must not flash red errors).
func TestHandlerCostBreakdownDegradesCalmly(t *testing.T) {
	s := newPermissiveCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{breakdownErr: ErrUpstreamUnavailable}})
	w := serveWithToken(t, s, "/api/cost/breakdown?by=agent&tenant="+costTestTenant)
	require.Equal(t, http.StatusOK, w.Code, "a transient upstream stall must degrade calmly, not 502")

	var resp CostBreakdownResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotNil(t, resp.Agents)
	assert.Empty(t, resp.Agents)
	assert.NotEmpty(t, resp.Notice, "a degrade notice must be present for the SPA to render")
}

// TestHandlerRunsDegradesCalmly: ErrUpstreamUnavailable on the runs list → 200 with
// empty runs + notice, not 502.
func TestHandlerRunsDegradesCalmly(t *testing.T) {
	s := serverWithAdapters(t, Adapters{Langfuse: fakeLangfuseAdapter{filteredErr: ErrUpstreamUnavailable}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var resp RunListResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotNil(t, resp.Runs)
	assert.Empty(t, resp.Runs)
	assert.NotEmpty(t, resp.Notice)
}

// TestHandlerCostEmptyStore200: the summary now reads the durable tenant rollup
// (ADR 0077), not Langfuse — so a Langfuse stall no longer affects it. With an
// empty rollup store for the tenant, /api/cost returns 200 with a zeroed summary
// and a non-nil (empty) ByModel — an honest empty view, never a 502. This is the
// tenant-scoped successor to the old Langfuse "degrade calmly" cost path.
func TestHandlerCostEmptyStore200(t *testing.T) {
	s := newPermissiveCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{err: ErrUpstreamUnavailable}})
	w := serveWithToken(t, s, "/api/cost?tenant="+costTestTenant)
	require.Equal(t, http.StatusOK, w.Code)

	var resp CostResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotNil(t, resp.Summary.ByModel, "ByModel must be [] not null (ADR 0077: intentionally empty)")
	assert.Empty(t, resp.Summary.ByModel, "the durable rollup carries no per-model detail (ADR 0077)")
	assert.Equal(t, 0.0, resp.Summary.TotalCostUSD, "no rollup rows ⇒ zero MTD spend")
	assert.Equal(t, int64(0), resp.Summary.TotalTokens)
}

// TestHandlerCostBreakdownEmpty200: empty breakdown → {agents:[], ...} 200.
func TestHandlerCostBreakdownEmpty200(t *testing.T) {
	s := newPermissiveCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{}})
	w := serveWithToken(t, s, "/api/cost/breakdown?by=agent&tenant="+costTestTenant)
	require.Equal(t, http.StatusOK, w.Code)

	var resp CostBreakdownResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotNil(t, resp.Agents, "agents must be [] not null")
	assert.Empty(t, resp.Agents)
	assert.Equal(t, "", resp.NextCursor)
}

// TestHandlerCostBreakdownBadCursor400: a malformed cursor → 400.
func TestHandlerCostBreakdownBadCursor400(t *testing.T) {
	// Use a real Langfuse stub that returns an empty corpus so the cursor parse
	// is reached (the fake adapter ignores the cursor param).
	srv := fakeLangfuseBreakdown(t, []lfTrace{})
	a, err := NewLangfuseAdapter(LangfuseConfig{
		BaseURL:   srv.URL,
		PublicKey: "pk",
		SecretKey: "sk",
	})
	require.NoError(t, err)

	s := newPermissiveCostServer(t, Adapters{Langfuse: a})
	w := serveWithToken(t, s, "/api/cost/breakdown?by=agent&cursor=notanint&tenant="+costTestTenant)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestHandlerCostBreakdownData200: a seeded breakdown response is returned 200.
func TestHandlerCostBreakdownData200(t *testing.T) {
	br := &CostBreakdownResponse{
		Agents: []AgentCostItem{
			{AgentNs: "default", AgentName: "foo", TotalCostUSD: 1.5, TotalTokens: 300, RunCount: 2},
		},
		Total:      CostSummary{TotalCostUSD: 1.5, TotalTokens: 300, Observations: 2, ByModel: []MetricPoint{}},
		NextCursor: "",
	}
	s := newPermissiveCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{breakdown: br}})
	// The seeded agent is in namespace "default", which costTestTenant owns — the
	// ADR 0077 filter keeps it.
	w := serveWithToken(t, s, "/api/cost/breakdown?by=agent&tenant="+costTestTenant)
	require.Equal(t, http.StatusOK, w.Code)

	var resp CostBreakdownResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.Len(t, resp.Agents, 1)
	assert.Equal(t, "default", resp.Agents[0].AgentNs)
	assert.Equal(t, "foo", resp.Agents[0].AgentName)
	assert.InDelta(t, 1.5, resp.Agents[0].TotalCostUSD, 1e-9)
	assert.Equal(t, int64(300), resp.Agents[0].TotalTokens)
	assert.Equal(t, 2, resp.Agents[0].RunCount)
}

// --- ADR 0077 tenant-isolation tests (the m86.1 security proof) --------------
//
// These prove the cross-tenant leak (m52.Q4) is closed: /api/cost + /api/cost/
// breakdown require ?tenant= and return ONLY the requested tenant's spend —
// never cluster-wide, never another tenant's.

// tenantCR is a small Tenant-fixture builder for the isolation tests.
func tenantCR(name string, namespaces ...string) *agentsv1alpha1.Tenant {
	return &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       agentsv1alpha1.TenantSpec{Namespaces: namespaces},
	}
}

// newIsolationCostServer builds a permissive (always-allow SSAR) cost server
// seeded with the given Tenant CRs and rollup store, wired with the given
// adapters — the fixture backbone for the tenant-isolation proofs.
func newIsolationCostServer(t *testing.T, a Adapters, store costrollup.Store, tenants ...*agentsv1alpha1.Tenant) *Server {
	t.Helper()
	objs := make([]client.Object, 0, len(tenants))
	for _, tnt := range tenants {
		objs = append(objs, tnt)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      a,
		Version:       "test",
		Log:           logr.Discard(),
	})
	s.authorizer = &recordingAuthorizer{} // always allows — isolation is by tenant, not SSAR
	s.rollupStore = store
	return s
}

// TestHandlerCostRequiresTenant: /api/cost without ?tenant= → 400 (ADR 0077).
func TestHandlerCostRequiresTenant(t *testing.T) {
	s := newIsolationCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{}}, costrollup.NewMemStore())
	w := serveWithToken(t, s, "/api/cost")
	assert.Equal(t, http.StatusBadRequest, w.Code, "missing ?tenant= ⇒ 400")
	assert.Contains(t, w.Body.String(), "tenant")
}

// TestHandlerCostNilStoreIs501: /api/cost requires the durable rollup store
// (ADR 0077) — nil store ⇒ 501 even with ?tenant= present.
func TestHandlerCostNilStoreIs501(t *testing.T) {
	s := newIsolationCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{}}, nil)
	w := serveWithToken(t, s, "/api/cost?tenant=x")
	assert.Equal(t, http.StatusNotImplemented, w.Code, "no rollup store ⇒ 501")
}

// TestHandlerCostTenantIsolatesSummary is the core summary-isolation proof: two
// tenants (X, Y) each have their own MTD rollup; /api/cost?tenant=X returns X's
// total (not the cluster-wide sum, not Y's), and ?tenant=Y returns Y's.
func TestHandlerCostTenantIsolatesSummary(t *testing.T) {
	store := costrollup.NewMemStore()
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	// Seed each tenant's MTD-cumulative rollup for the current month. The handler
	// reads the LAST (most recent) row as the MTD total.
	seedRollup(t, store, "tenant-x", monthStart, 12.50, 1250)
	seedRollup(t, store, "tenant-y", monthStart, 99.00, 9900)

	s := newIsolationCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{}}, store,
		tenantCR("tenant-x", "ns-x"), tenantCR("tenant-y", "ns-y"))

	// Tenant X sees ONLY X's spend.
	wx := serveWithToken(t, s, "/api/cost?tenant=tenant-x")
	require.Equal(t, http.StatusOK, wx.Code)
	var rx CostResponse
	require.NoError(t, json.NewDecoder(wx.Body).Decode(&rx))
	assert.InDelta(t, 12.50, rx.Summary.TotalCostUSD, 1e-9, "tenant X sees X's total, not the cluster sum")
	assert.Equal(t, int64(1250), rx.Summary.TotalTokens)
	assert.NotNil(t, rx.Summary.ByModel)
	assert.Empty(t, rx.Summary.ByModel, "ADR 0077: the durable rollup carries no per-model detail")

	// Tenant Y sees ONLY Y's spend — cross-tenant isolation.
	wy := serveWithToken(t, s, "/api/cost?tenant=tenant-y")
	require.Equal(t, http.StatusOK, wy.Code)
	var ry CostResponse
	require.NoError(t, json.NewDecoder(wy.Body).Decode(&ry))
	assert.InDelta(t, 99.00, ry.Summary.TotalCostUSD, 1e-9, "tenant Y sees Y's total, not X's")
	assert.Equal(t, int64(9900), ry.Summary.TotalTokens)
}

// TestHandlerCostBreakdownRequiresTenant: /api/cost/breakdown without ?tenant= →
// 400 (ADR 0077) WHEN tenants exist (a cluster-wide breakdown would leak across them).
func TestHandlerCostBreakdownRequiresTenant(t *testing.T) {
	// A tenant exists → a missing ?tenant= must still be a 400 (never cluster-wide).
	s := newIsolationCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{}}, costrollup.NewMemStore(),
		tenantCR("acme", "default"))
	w := serveWithToken(t, s, "/api/cost/breakdown?by=agent")
	assert.Equal(t, http.StatusBadRequest, w.Code, "missing ?tenant= with tenants present ⇒ 400")
	assert.Contains(t, w.Body.String(), "tenant")
}

// TestHandlerCostBreakdownZeroTenantsFallback (M99 B1): on a cluster with NO tenants there is no
// boundary to leak across, so a missing ?tenant= serves the cluster-wide per-agent breakdown instead
// of 400 — the "per-agent fallback" so the Cost page isn't empty on a tenant-less cluster.
func TestHandlerCostBreakdownZeroTenantsFallback(t *testing.T) {
	br := &CostBreakdownResponse{
		Agents:     []AgentCostItem{{AgentNs: "default", AgentName: "a1", TotalCostUSD: 1.5, TotalTokens: 100, RunCount: 3}},
		Total:      CostSummary{TotalCostUSD: 1.5, TotalTokens: 100, Observations: 3, ByModel: []MetricPoint{}},
		NextCursor: "",
	}
	// No tenant CRs seeded → the fallback engages.
	s := newIsolationCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{breakdown: br}}, costrollup.NewMemStore())
	w := serveWithToken(t, s, "/api/cost/breakdown?by=agent")
	require.Equal(t, http.StatusOK, w.Code, "zero tenants + missing ?tenant= ⇒ cluster-wide breakdown, not 400")
	var resp CostBreakdownResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.Len(t, resp.Agents, 1)
	assert.Equal(t, "a1", resp.Agents[0].AgentName)
}

// TestHandlerCostBreakdownFiltersByTenantNamespaces is the core breakdown-
// isolation proof: Langfuse returns agents across THREE namespaces, but tenant X
// owns only a SUBSET — the response keeps ONLY the agents in X's namespaces, and
// resp.Total is recomputed over the kept rows (not the cluster-wide total).
func TestHandlerCostBreakdownFiltersByTenantNamespaces(t *testing.T) {
	// Langfuse (cluster-wide) returns three agents in three distinct namespaces.
	br := &CostBreakdownResponse{
		Agents: []AgentCostItem{
			{AgentNs: "ns-a", AgentName: "alpha", TotalCostUSD: 1.00, TotalTokens: 100, RunCount: 1},
			{AgentNs: "ns-b", AgentName: "beta", TotalCostUSD: 2.00, TotalTokens: 200, RunCount: 2},
			{AgentNs: "ns-c", AgentName: "gamma", TotalCostUSD: 4.00, TotalTokens: 400, RunCount: 4},
		},
		// The pre-filter cluster-wide total (must NOT leak into resp.Total).
		Total:      CostSummary{TotalCostUSD: 7.00, TotalTokens: 700, Observations: 7, ByModel: []MetricPoint{}},
		NextCursor: "",
	}
	// Tenant X owns ns-a and ns-c only (NOT ns-b).
	s := newIsolationCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{breakdown: br}}, costrollup.NewMemStore(),
		tenantCR("tenant-x", "ns-a", "ns-c"))

	w := serveWithToken(t, s, "/api/cost/breakdown?by=agent&tenant=tenant-x")
	require.Equal(t, http.StatusOK, w.Code)

	var resp CostBreakdownResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))

	// Only alpha (ns-a) + gamma (ns-c) survive; beta (ns-b) is excluded even though
	// Langfuse returned it — the tenant-isolation filter.
	require.Len(t, resp.Agents, 2, "beta (ns-b ∉ tenant X) must be filtered out")
	gotNs := []string{resp.Agents[0].AgentNs, resp.Agents[1].AgentNs}
	assert.ElementsMatch(t, []string{"ns-a", "ns-c"}, gotNs)
	assert.NotContains(t, gotNs, "ns-b", "an agent outside the tenant's namespaces must never appear")

	// resp.Total is recomputed over the KEPT rows (1.00 + 4.00 = 5.00), NOT the
	// cluster-wide 7.00 — the leak is closed.
	assert.InDelta(t, 5.00, resp.Total.TotalCostUSD, 1e-9, "total must sum only the kept agents, not the cluster total")
	assert.Equal(t, int64(500), resp.Total.TotalTokens, "tokens must sum only the kept agents (100+400)")
}

// TestHandlerCostBreakdownUnknownTenantIs404: ?tenant=X where no Tenant CR X
// exists → 404 (the Get fails) — a non-existent tenant leaks nothing.
func TestHandlerCostBreakdownUnknownTenantIs404(t *testing.T) {
	br := &CostBreakdownResponse{
		Agents:     []AgentCostItem{{AgentNs: "ns-a", AgentName: "alpha", TotalCostUSD: 1.0, TotalTokens: 100, RunCount: 1}},
		Total:      CostSummary{TotalCostUSD: 1.0, TotalTokens: 100, ByModel: []MetricPoint{}},
		NextCursor: "",
	}
	// No Tenant CRs seeded.
	s := newIsolationCostServer(t, Adapters{Langfuse: fakeLangfuseAdapter{breakdown: br}}, costrollup.NewMemStore())
	w := serveWithToken(t, s, "/api/cost/breakdown?by=agent&tenant=ghost")
	assert.Equal(t, http.StatusNotFound, w.Code, "an unknown tenant ⇒ 404, never a cluster-wide leak")
}
