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
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// This file exercises the list contract on GET /api/agents (ui-foundation §4):
// ?limit (default 50, cap 200) + K8s native limit/continue, ?cursor pass-through
// with a nextCursor round-trip, ?q windowed substring filtering, ?namespace
// scoping, and the []-not-null shape. It complements server_test.go's
// projection/empty tests (which stay unchanged — the fields added here are
// additive).

// capturedListOpts records the ListOptions the caller-scoped List was invoked
// with, so a test can assert exactly how the contract params mapped onto the K8s
// native limit/continue/namespace.
type capturedListOpts struct {
	limit     int64
	continueT string
	namespace string
	called    bool
}

// captureListInterceptor records the resolved ListOptions on every List, then
// delegates to the fake store so real namespace filtering still happens.
//
// NOTE: the controller-runtime fake client does NOT implement server-side
// limit/continue paging — it returns the whole list and never sets a continue
// token. So tests that assert paging semantics use pagingListInterceptor (below),
// which stands in for the API server's pagination the way ssarInterceptor stands
// in for its authz decisions. captureListInterceptor is only for asserting HOW
// the contract params mapped onto the K8s options (not the paging result).
func captureListInterceptor(rec *capturedListOpts) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var lo client.ListOptions
			lo.ApplyOptions(opts)
			rec.limit = lo.Limit
			rec.continueT = lo.Continue
			rec.namespace = lo.Namespace
			rec.called = true
			return c.List(ctx, list, opts...)
		},
	}
}

// pagingListInterceptor stands in for the API server's limit/continue pagination
// over a fixed, name-sorted AgentDeployment dataset. It honors the caller's
// Limit + Continue exactly as the API server would: the continue token is the
// offset of the next page as a decimal string, and the returned list's Continue
// is set to the next offset (empty when the page reaches the end). Namespace
// scoping is applied first. This is the only faithful way to exercise the
// handler's paging plumbing, since the fake store ignores Limit/Continue.
func pagingListInterceptor(all []*agentsv1alpha1.AgentDeployment) interceptor.Funcs {
	// Sort by (namespace, name) for a stable, API-server-like order.
	sorted := make([]*agentsv1alpha1.AgentDeployment, len(all))
	copy(sorted, all)
	slices.SortFunc(sorted, func(a, b *agentsv1alpha1.AgentDeployment) int {
		if a.Namespace != b.Namespace {
			return strings.Compare(a.Namespace, b.Namespace)
		}
		return strings.Compare(a.Name, b.Name)
	})

	return interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var lo client.ListOptions
			lo.ApplyOptions(opts)

			// Namespace scope first (empty = all namespaces).
			scoped := make([]*agentsv1alpha1.AgentDeployment, 0, len(sorted))
			for _, ad := range sorted {
				if lo.Namespace == "" || ad.Namespace == lo.Namespace {
					scoped = append(scoped, ad)
				}
			}

			// Resolve the start offset from the continue token.
			start := 0
			if lo.Continue != "" {
				n, err := strconv.Atoi(lo.Continue)
				if err != nil {
					return fmt.Errorf("invalid continue token %q", lo.Continue)
				}
				start = n
			}
			if start > len(scoped) {
				start = len(scoped)
			}

			end := len(scoped)
			if lo.Limit > 0 && start+int(lo.Limit) < end {
				end = start + int(lo.Limit)
			}

			page := scoped[start:end]
			out := make([]agentsv1alpha1.AgentDeployment, 0, len(page))
			for _, ad := range page {
				out = append(out, *ad)
			}
			adList, ok := list.(*agentsv1alpha1.AgentDeploymentList)
			if !ok {
				return fmt.Errorf("pagingListInterceptor: unexpected list type %T", list)
			}
			adList.Items = out
			// The next-page continue token: the end offset, or "" when exhausted.
			if end < len(scoped) {
				adList.Continue = strconv.Itoa(end)
			} else {
				adList.Continue = ""
			}
			return nil
		},
	}
}

// pagingServer builds a caller-scoped Server whose AgentDeployment List honors
// limit/continue paging over the given fixtures (via pagingListInterceptor).
func pagingServer(t *testing.T, all ...*agentsv1alpha1.AgentDeployment) *Server {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(pagingListInterceptor(all)).
		Build()
	return newCallerServer(t, &fakeCallerClientFactory{client: c})
}

// agent is a terse AgentDeployment fixture constructor for the paging tests.
func agent(namespace, name string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
}

// makeAgentPtrs builds n AgentDeployment pointers named agent-000..agent-(n-1).
func makeAgentPtrs(namespace string, n int) []*agentsv1alpha1.AgentDeployment {
	out := make([]*agentsv1alpha1.AgentDeployment, 0, n)
	for i := range n {
		out = append(out, agent(namespace, fmt.Sprintf("agent-%03d", i)))
	}
	return out
}

// makeAgents is makeAgentPtrs as []client.Object, for tests that seed the real
// fake store (which honors namespace/label filters, unlike the paging simulator).
func makeAgents(namespace string, n int) []client.Object {
	ptrs := makeAgentPtrs(namespace, n)
	objs := make([]client.Object, 0, len(ptrs))
	for _, p := range ptrs {
		objs = append(objs, p)
	}
	return objs
}

// getAgents drives GET /api/agents with the given raw query and returns the
// decoded response + the ListOptions the caller-scoped read resolved to.
func getAgents(t *testing.T, s *Server, rawQuery string) (AgentListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents?"+rawQuery, nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	var body AgentListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

// --- limit: default + capping ------------------------------------------------

func TestListLimitDefaultAndCap(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantLimit int64
	}{
		{"absent-defaults-50", "", defaultListLimit},
		{"explicit-25", "limit=25", 25},
		{"over-cap-clamped-200", "limit=5000", maxListLimit},
		{"zero-defaults", "limit=0", defaultListLimit},
		{"negative-defaults", "limit=-4", defaultListLimit},
		{"garbage-defaults", "limit=abc", defaultListLimit},
		{"exact-cap", "limit=200", maxListLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec capturedListOpts
			c := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithInterceptorFuncs(captureListInterceptor(&rec)).
				Build()
			s := newCallerServer(t, &fakeCallerClientFactory{client: c})

			_, code := getAgents(t, s, tc.query)
			require.Equal(t, http.StatusOK, code)
			assert.True(t, rec.called)
			assert.Equal(t, tc.wantLimit, rec.limit, "resolved K8s limit")
		})
	}
}

// --- cursor pass-through + nextCursor round-trip -----------------------------

// TestCursorPassThroughAndNextCursor proves ?cursor is forwarded verbatim as the
// K8s continue token, and the API server's continue token for the page comes back
// as nextCursor — so a client can round-trip page 1's nextCursor into page 2's
// cursor. Exhaustion yields an empty nextCursor.
func TestCursorPassThroughAndNextCursor(t *testing.T) {
	// 5 agents, page size 2 → page 1 returns 2 items + a non-empty continue token.
	s := pagingServer(t, makeAgentPtrs("prod", 5)...)

	// Page 1: no cursor.
	page1, code := getAgents(t, s, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 2, "page is bounded by limit")
	require.NotEmpty(t, page1.NextCursor, "a non-exhausted list must expose a nextCursor")

	// Page 2: feed page 1's nextCursor back as ?cursor — the round-trip.
	page2, code := getAgents(t, s, "limit=2&cursor="+page1.NextCursor)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page2.Items, 2)
	assert.NotEqual(t, page1.Items[0].Name, page2.Items[0].Name,
		"page 2 must be a different window than page 1")

	// Drain the rest until exhaustion; nextCursor must eventually be empty.
	seen := len(page1.Items) + len(page2.Items)
	cursor := page2.NextCursor
	for cursor != "" {
		next, code := getAgents(t, s, "limit=2&cursor="+cursor)
		require.Equal(t, http.StatusOK, code)
		seen += len(next.Items)
		cursor = next.NextCursor
	}
	assert.Equal(t, 5, seen, "paging must eventually visit every agent exactly once")
}

// TestCursorForwardedVerbatim proves the raw cursor string is handed to the K8s
// client as the continue token unchanged (opaque pass-through — the BFF only
// validates emptiness, never re-encodes it).
func TestCursorForwardedVerbatim(t *testing.T) {
	var rec capturedListOpts
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(captureListInterceptor(&rec)).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// An opaque token value; the handler must not touch it (emptiness aside).
	_, code := getAgents(t, s, "limit=10&cursor=opaque-continue-token-xyz")
	// The fake store may reject an unknown continue token; either way we only
	// assert what was forwarded to the client (verbatim pass-through).
	_ = code
	assert.Equal(t, "opaque-continue-token-xyz", rec.continueT,
		"the cursor must be forwarded verbatim as the K8s continue token")
}

// TestEmptyCursorNotForwarded proves an absent/empty cursor is NOT forwarded as a
// continue token (an empty continue is invalid to the API server).
func TestEmptyCursorNotForwarded(t *testing.T) {
	var rec capturedListOpts
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(captureListInterceptor(&rec)).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code := getAgents(t, s, "limit=10")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, rec.continueT, "an empty cursor must not be forwarded as a continue token")
}

// --- q: windowed, case-insensitive substring filter --------------------------

// TestQWindowedSubstringFilter proves ?q is a case-insensitive substring filter
// on the name, applied to the FETCHED WINDOW only (it never widens the K8s page).
func TestQWindowedSubstringFilter(t *testing.T) {
	objs := []client.Object{
		&agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "echo-prod", Namespace: "prod"}},
		&agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "ECHO-staging", Namespace: "prod"}},
		&agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "summarizer", Namespace: "prod"}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Case-insensitive "echo" matches both echo-prod and ECHO-staging.
	body, code := getAgents(t, s, "q=echo")
	require.Equal(t, http.StatusOK, code)
	names := map[string]bool{}
	for _, a := range body.Items {
		names[a.Name] = true
	}
	assert.Len(t, body.Items, 2)
	assert.True(t, names["echo-prod"])
	assert.True(t, names["ECHO-staging"])
	assert.False(t, names["summarizer"], "q must exclude non-matches")

	// A q that matches nothing in the window yields [] (not null) — a valid state.
	body, code = getAgents(t, s, "q=zzz-nomatch")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, body.Items)
	assert.NotNil(t, body.Items, "a fully-filtered window must be [] not null")
}

// TestQIsWindowedNotClusterWide proves q filters the fetched page only and never
// loops to fetch more from the cluster: with limit=2 over 5 agents, at most 2
// items can be returned even when more would match a global search. This is the
// honesty guarantee (a 10k-agent cluster must not OOM the BFF).
func TestQIsWindowedNotClusterWide(t *testing.T) {
	// 5 agents all named agent-00N → every one matches q=agent.
	s := pagingServer(t, makeAgentPtrs("prod", 5)...)

	body, code := getAgents(t, s, "limit=2&q=agent")
	require.Equal(t, http.StatusOK, code)
	assert.LessOrEqual(t, len(body.Items), 2,
		"q filters only the fetched window; it must not fetch beyond the page limit")
	// The page still has a continue token even though q hid nothing here, proving
	// paging is independent of the (windowed) filter (2 shown of 5 → more pages).
	assert.NotEmpty(t, body.NextCursor, "nextCursor tracks the raw page, independent of q")
}

// TestQAndCursorInterplay proves q applies to the window a cursor lands on — the
// filter follows the page, not the whole cluster.
func TestQAndCursorInterplay(t *testing.T) {
	// Two agents match "keep", spread so they land on different pages by name.
	s := pagingServer(t,
		agent("prod", "aaa-keep"),
		agent("prod", "bbb-drop"),
		agent("prod", "ccc-drop"),
		agent("prod", "ddd-keep"),
	)

	// Page 1 (limit 2, sorted by name): aaa-keep, bbb-drop → q=keep yields only
	// aaa-keep from THIS window; the ddd-keep match lives on a later page.
	page1, code := getAgents(t, s, "limit=2&q=keep")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 1, "only the in-window match survives q")
	assert.Equal(t, "aaa-keep", page1.Items[0].Name)
	require.NotEmpty(t, page1.NextCursor)

	// Page 2 via the cursor: ccc-drop, ddd-keep → q=keep yields ddd-keep.
	page2, code := getAgents(t, s, "limit=2&q=keep&cursor="+page1.NextCursor)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page2.Items, 1)
	assert.Equal(t, "ddd-keep", page2.Items[0].Name, "q follows the cursor's window")
}

// --- namespace scoping -------------------------------------------------------

// TestNamespaceScoping proves ?namespace scopes the list to one namespace, and an
// absent namespace lists across all the caller can see (empty InNamespace).
func TestNamespaceScoping(t *testing.T) {
	objs := append(makeAgents("prod", 2), makeAgents("dev", 3)...)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Scoped to prod → only the 2 prod agents.
	scoped, code := getAgents(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, scoped.Items, 2)
	for _, a := range scoped.Items {
		assert.Equal(t, "prod", a.Namespace)
	}

	// No namespace → all 5 across namespaces.
	all, code := getAgents(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, all.Items, 5)
}

// TestNamespaceForwardedToClient proves ?namespace maps onto the K8s
// InNamespace list option, and its absence leaves the list cluster-wide.
func TestNamespaceForwardedToClient(t *testing.T) {
	var rec capturedListOpts
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(captureListInterceptor(&rec)).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code := getAgents(t, s, "namespace=team-a")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "team-a", rec.namespace)

	rec = capturedListOpts{}
	_, code = getAgents(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, rec.namespace, "an absent namespace lists cluster-wide")
}

// --- shape: agents/items mirror, []-not-null ---------------------------------

// TestListItemsMirrorAgents proves the additive `items` field mirrors `agents`
// exactly (same flat summaries) so both the M12 surfaces and the v2 DataTable
// read the same data, and both are [] not null when empty.
func TestListItemsMirrorAgents(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(makeAgents("prod", 3)...).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getAgents(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, body.Agents, body.Items, "items must mirror agents exactly")
	assert.Len(t, body.Items, 3)

	// Empty case: both [] not null.
	empty := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	es := newCallerServer(t, &fakeCallerClientFactory{client: empty})
	body, code = getAgents(t, es, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Agents)
	assert.NotNil(t, body.Items)
	assert.Empty(t, body.NextCursor)
}
