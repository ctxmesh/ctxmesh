//go:build integration

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

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
)

// TestCostAndTenantsForAPersonaBoundToOneNamespace: against a real API server, an operator bound by
// RoleBinding the way the chart binds one cannot list Tenants (they are cluster-scoped), is told so
// by /api/capabilities, and reads a cost breakdown of only their namespace, with or without a
// Tenant they cannot see. An operator bound cluster-wide keeps tenant scoping as it was, and a
// developer bound to the namespace, who holds no cost grant, is refused.
func TestCostAndTenantsForAPersonaBoundToOneNamespace(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	const mine, theirs = "cost-mine", "cost-theirs"
	mirror := namespacetenant.NewMemStore()
	for _, ns := range []string{mine, theirs} {
		require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
		require.NoError(t, mirror.RecordNamespace(ctx, ns))
	}
	operatorRole := installPersona(t, admin, "ctxmesh_operator_role.yaml")
	developerRole := installPersona(t, admin, "ctxmesh_developer_role.yaml")
	operator := personaUser(t, admin, mine, "cost-operator", operatorRole)
	developer := personaUser(t, admin, mine, "cost-developer", developerRole)
	require.NoError(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-everywhere"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: operatorRole},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: "cost-everywhere", APIGroup: rbacv1.GroupName}},
	}))
	everywhereUser, err := rbacEnv.AddUser(envtest.User{Name: "cost-everywhere", Groups: []string{"system:authenticated"}}, nil)
	require.NoError(t, err)
	everywhere, err := client.New(everywhereUser.Config(), client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	breakdown := &CostBreakdownResponse{
		Agents: []AgentCostItem{
			{AgentNs: mine, AgentName: "a", TotalCostUSD: costPtr(1), TotalTokens: 10, RunCount: 1},
			{AgentNs: theirs, AgentName: "b", TotalCostUSD: costPtr(2), TotalTokens: 20, RunCount: 1},
		},
		Total: CostSummary{TotalCostUSD: 3, TotalTokens: 30, Observations: 2, ByModel: []MetricPoint{}},
	}
	get := func(c client.Client, path string) *httptest.ResponseRecorder {
		s := NewServer(Options{
			CallerClients:        newFakeFactory(c),
			Scheme:               testScheme(t),
			Auth:                 AllowAll{},
			Adapters:             Adapters{Langfuse: fakeLangfuseAdapter{breakdown: breakdown}},
			NamespaceTenantStore: mirror,
			Version:              "test",
			Log:                  logr.Discard(),
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer persona-token")
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	namespacesOf := func(t *testing.T, rec *httptest.ResponseRecorder) ([]string, CostBreakdownResponse) {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp CostBreakdownResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		var got []string
		for _, a := range resp.Agents {
			got = append(got, a.AgentNs)
		}
		return got, resp
	}
	canListTenants := func(t *testing.T, c client.Client) bool {
		t.Helper()
		rec := get(c, "/api/capabilities?namespace="+mine)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var caps CapabilitiesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &caps))
		return caps.Allowed[resourceTenants][verbList]
	}
	operatorSeesOnlyTheirNamespace := func(t *testing.T) {
		t.Helper()
		got, resp := namespacesOf(t, get(operator, "/api/cost/breakdown?by=agent"))
		assert.Equal(t, []string{mine}, got)
		assert.Equal(t, []string{mine}, resp.Namespaces)
		assert.InDelta(t, 1.0, resp.Total.TotalCostUSD, 1e-9, "the total is their namespace's, not the cluster's")
	}

	t.Run("bound to one namespace: not a tenant admin, cost for their namespace", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, get(operator, "/api/tenants").Code, "Tenants are cluster-scoped")
		assert.False(t, canListTenants(t, operator), "the console is told, so it hides tenant surfaces")
		operatorSeesOnlyTheirNamespace(t)
	})

	t.Run("a developer holds no cost grant and is refused", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, get(developer, "/api/cost/breakdown?by=agent").Code)
	})

	t.Run("a cluster-wide operator on a cluster with no tenants reads the whole breakdown", func(t *testing.T) {
		assert.True(t, canListTenants(t, everywhere))
		got, resp := namespacesOf(t, get(everywhere, "/api/cost/breakdown?by=agent"))
		assert.ElementsMatch(t, []string{mine, theirs}, got)
		assert.Nil(t, resp.Namespaces)
	})

	tenant := &agentsv1beta1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-tenant"},
		Spec:       agentsv1beta1.TenantSpec{Namespaces: []string{theirs}},
	}
	require.NoError(t, admin.Create(ctx, tenant))
	t.Cleanup(func() { _ = admin.Delete(context.Background(), tenant) })

	t.Run("with a tenant, a cluster-wide operator still names it", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, get(everywhere, "/api/cost/breakdown?by=agent").Code)
		got, _ := namespacesOf(t, get(everywhere, "/api/cost/breakdown?by=agent&tenant=cost-tenant"))
		assert.Equal(t, []string{theirs}, got)
	})

	t.Run("a tenant they cannot see does not widen a namespace-bound operator", func(t *testing.T) {
		operatorSeesOnlyTheirNamespace(t)
		assert.Equal(t, http.StatusForbidden, get(operator, "/api/cost/breakdown?by=agent&tenant=cost-tenant").Code,
			"naming a tenant needs the cluster-wide cost grant, as before")
	})
}
