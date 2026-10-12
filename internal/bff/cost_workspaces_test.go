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
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
)

// costReaderIn answers the costrollups gate: allowed cluster-wide when clusterWide, otherwise only
// in the listed namespaces. Everything else is allowed. Safe for the concurrent probes.
type costReaderIn struct {
	clusterWide bool
	namespaces  map[string]bool
}

func (a costReaderIn) Authorize(_ context.Context, _ client.Client, act authz.Action) error {
	if act.Resource != resourceCostRollups {
		return nil
	}
	if act.Namespace == "" && a.clusterWide || act.Namespace != "" && (a.clusterWide || a.namespaces[act.Namespace]) {
		return nil
	}
	return authz.ErrForbidden
}

var errEtcdDown = errors.New("etcdserver: request timed out")

// workspaceCostServer is a caller bound to "mine" and "other" (they may list agents there) who
// may not list namespaces, and whose Tenant list answers tenantsErr.
func workspaceCostServer(t *testing.T, tenantsErr error, auth costReaderIn) *Server {
	t.Helper()
	return workspaceCostServerWith(t, tenantsErr, auth, &CostBreakdownResponse{
		Agents: []AgentCostItem{
			{AgentNs: "mine", AgentName: "a", TotalCostUSD: costPtr(1), TotalTokens: 10, RunCount: 1},
			{AgentNs: "theirs", AgentName: "b", TotalCostUSD: costPtr(2), TotalTokens: 20, RunCount: 1},
			{AgentNs: "other", AgentName: "c", TotalCostUSD: costPtr(4), TotalTokens: 40, RunCount: 1},
		},
		Total: CostSummary{TotalCostUSD: 7, TotalTokens: 70, Observations: 3, ByModel: []MetricPoint{}},
	})
}

// workspaceCostServerWith is workspaceCostServer over the cluster-wide breakdown br, with more
// Tenants.
func workspaceCostServerWith(t *testing.T, tenantsErr error, auth costReaderIn, br *CostBreakdownResponse, tenants ...client.Object) *Server {
	t.Helper()
	works := map[string]bool{"mine": true, "other": true}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(append([]client.Object{tenantCR("acme", "mine", "theirs")}, tenants...)...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				switch list.(type) {
				case *agentsv1beta1.TenantList:
					if tenantsErr != nil {
						return tenantsErr
					}
				case *corev1.NamespaceList:
					return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", errors.New("bound to a namespace"))
				}
				return c.List(ctx, list, opts...)
			},
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				ssar, ok := obj.(*authzv1.SelfSubjectAccessReview)
				if !ok {
					return errors.New("unexpected create")
				}
				ra := ssar.Spec.ResourceAttributes
				ssar.Status.Allowed = ra.Resource == resAgentDeployments && ra.Verb == verbList && works[ra.Namespace]
				return nil
			},
		}).Build()
	mirror := namespacetenant.NewMemStore()
	for _, ns := range []string{"mine", "other", "theirs"} {
		require.NoError(t, mirror.RecordNamespace(context.Background(), ns))
	}
	s := NewServer(Options{
		CallerClients:        newFakeFactory(c),
		Scheme:               testScheme(t),
		Auth:                 AllowAll{},
		Adapters:             Adapters{Langfuse: fakeLangfuseAdapter{breakdown: br}},
		NamespaceTenantStore: mirror,
		Version:              "test",
		Log:                  logr.Discard(),
	})
	s.authorizer = auth
	return s
}

var forbiddenTenants = apierrors.NewForbidden(schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "tenants"}, "",
	errors.New("tenants are cluster-scoped"))

// A person who cannot list Tenants is not a tenant admin: with no tenant, the breakdown covers
// the workspaces where they may read cost, and its total is the sum of those rows. A Tenant they
// cannot see owning another namespace does not widen it.
func TestCostBreakdownWithoutTenantCoversTheCallersWorkspaces(t *testing.T) {
	s := workspaceCostServer(t, forbiddenTenants, costReaderIn{namespaces: map[string]bool{"mine": true}})

	rec := serveWithToken(t, s, "/api/cost/breakdown?by=agent")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp CostBreakdownResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Agents, 1, "only the workspace where they may read cost; not other, not theirs")
	assert.Equal(t, "mine", resp.Agents[0].AgentNs)
	assert.Equal(t, []string{"mine"}, resp.Namespaces)
	assert.InDelta(t, 1.0, resp.Total.TotalCostUSD, 1e-9, "the total sums the kept rows, never the cluster")
	assert.Equal(t, int64(10), resp.Total.TotalTokens)
}

func TestCostBreakdownWithoutTenantRefusals(t *testing.T) {
	for _, c := range []struct {
		name       string
		tenantsErr error
		auth       costReaderIn
		want       int
	}{
		{"may read cost in none of their workspaces", forbiddenTenants, costReaderIn{}, http.StatusForbidden},
		// They may read cost cluster-wide but cannot see the tenant boundaries, so they name one.
		{"cluster-wide cost reader who cannot list Tenants", forbiddenTenants, costReaderIn{clusterWide: true}, http.StatusBadRequest},
		{"a tenant admin who sees a tenant names it", nil, costReaderIn{clusterWide: true}, http.StatusBadRequest},
		{"a rejected token", apierrors.NewUnauthorized("token expired"), costReaderIn{clusterWide: true}, http.StatusUnauthorized},
		{"the Tenant list failed", errEtcdDown, costReaderIn{clusterWide: true}, http.StatusInternalServerError},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := serveWithToken(t, workspaceCostServer(t, c.tenantsErr, c.auth), "/api/cost/breakdown?by=agent")
			assert.Equal(t, c.want, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), `"agents"`, "a refusal carries no rows")
		})
	}
}

// A tenant admin's breakdown is unchanged: a named tenant keeps its namespaces and says nothing
// about workspaces.
func TestCostBreakdownForANamedTenantIsUnchanged(t *testing.T) {
	s := workspaceCostServer(t, nil, costReaderIn{clusterWide: true})
	rec := serveWithToken(t, s, "/api/cost/breakdown?by=agent&tenant=acme")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp CostBreakdownResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	got := make([]string, 0, len(resp.Agents))
	for _, a := range resp.Agents {
		got = append(got, a.AgentNs)
	}
	assert.ElementsMatch(t, []string{"mine", "theirs"}, got)
	assert.Nil(t, resp.Namespaces)
}

// rankedBelow returns a cost-sorted breakdown of n agents in "theirs", each costlier than the
// caller's own agents in "mine", which follow them.
func rankedBelow(n int, mine ...string) *CostBreakdownResponse {
	br := &CostBreakdownResponse{Total: CostSummary{ByModel: []MetricPoint{}}}
	for i := range n {
		br.Agents = append(br.Agents, AgentCostItem{
			AgentNs: "theirs", AgentName: fmt.Sprintf("big-%02d", i),
			TotalCostUSD: costPtr(float64(1000 - i)), TotalTokens: 1000, RunCount: 1,
		})
		br.Total.TotalCostUSD += float64(1000 - i)
		br.Total.TotalTokens += 1000
	}
	for i, name := range mine {
		br.Agents = append(br.Agents, AgentCostItem{
			AgentNs: "mine", AgentName: name, TotalCostUSD: costPtr(float64(2 - i)), TotalTokens: 7, RunCount: 1,
		})
		br.Total.TotalCostUSD += float64(2 - i)
		br.Total.TotalTokens += 7
	}
	return br
}

// pagesOf walks a breakdown with limit=1 from the first page and returns every row it was shown.
func pagesOf(t *testing.T, s *Server, path string) []AgentCostItem {
	t.Helper()
	var rows []AgentCostItem
	cursor := ""
	for range 200 {
		rec := serveWithToken(t, s, path+"&limit=1&cursor="+cursor)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp CostBreakdownResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Agents, 1, "a page with a cursor before or after it is never empty")
		rows = append(rows, resp.Agents...)
		if resp.NextCursor == "" {
			return rows
		}
		cursor = resp.NextCursor
	}
	t.Fatal("the cursor never ended")
	return nil
}

// A caller whose one agent ranks 60th in the cluster sees it on the first page, with its own
// total and no cursor: pages are cut from the caller's rows, never from the cluster's.
func TestCostBreakdownPagesOnlyTheCallersWorkspaces(t *testing.T) {
	s := workspaceCostServerWith(t, forbiddenTenants, costReaderIn{namespaces: map[string]bool{"mine": true}},
		rankedBelow(59, "small"))

	rec := serveWithToken(t, s, "/api/cost/breakdown?by=agent&limit=50")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp CostBreakdownResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Agents, 1, "their agent is on page 1, not behind 59 rows they may not see")
	assert.Equal(t, "small", resp.Agents[0].AgentName)
	assert.Empty(t, resp.NextCursor, "no cursor points past their own rows")
	assert.InDelta(t, 2.0, resp.Total.TotalCostUSD, 1e-9)
	assert.Equal(t, int64(7), resp.Total.TotalTokens)

	// limit=1 pages count the caller's rows, never the cluster's.
	rows := pagesOf(t, s, "/api/cost/breakdown?by=agent")
	require.Len(t, rows, 1)
	assert.Equal(t, "mine", rows[0].AgentNs)
}

// The same holds on the tenant path: a tenant's pages, cursor and total are its own namespaces'.
func TestCostBreakdownPagesOnlyTheTenantsNamespaces(t *testing.T) {
	s := workspaceCostServerWith(t, nil, costReaderIn{clusterWide: true}, rankedBelow(59, "small", "smaller"),
		tenantCR("only-mine", "mine"))

	rec := serveWithToken(t, s, "/api/cost/breakdown?by=agent&tenant=only-mine&limit=50")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp CostBreakdownResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Agents, 2)
	assert.Equal(t, []string{"small", "smaller"}, []string{resp.Agents[0].AgentName, resp.Agents[1].AgentName})
	assert.Empty(t, resp.NextCursor)
	assert.InDelta(t, 3.0, resp.Total.TotalCostUSD, 1e-9, "the sum of the tenant's rows on every page")

	rows := pagesOf(t, s, "/api/cost/breakdown?by=agent&tenant=only-mine")
	require.Len(t, rows, 2, "limit=1 reveals the tenant's row count, not the cluster's")

	// The total is the tenant's whole set even when the page holds one row.
	rec = serveWithToken(t, s, "/api/cost/breakdown?by=agent&tenant=only-mine&limit=1")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.InDelta(t, 3.0, resp.Total.TotalCostUSD, 1e-9)
}
