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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

func mockTenant(name string, namespaces []string, model *agentsv1alpha1.TenantModelQuota, ready bool) *agentsv1alpha1.Tenant {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	return &agentsv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       agentsv1alpha1.TenantSpec{Namespaces: namespaces, Model: model},
		Status: agentsv1alpha1.TenantStatus{
			MemberNamespaces: int32(len(namespaces)),
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: status, Reason: "Reconciled", LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

func getTenants(t *testing.T, s *Server) (TenantListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body TenantListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

func getTenant(t *testing.T, s *Server, name string) (*TenantDetail, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var d TenantDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &d))
		return &d, rec.Code
	}
	return nil, rec.Code
}

func TestListTenants(t *testing.T) {
	objs := []client.Object{
		mockTenant("alpha", []string{"a1", "a2"}, &agentsv1alpha1.TenantModelQuota{BudgetUSD: "100.00", RPM: 600}, true),
		mockTenant("beta", []string{"b1"}, nil, false),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getTenants(t, s)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	byName := map[string]TenantSummary{}
	for _, it := range body.Items {
		byName[it.Name] = it
	}
	assert.Equal(t, int32(2), byName["alpha"].MemberNamespaces)
	assert.True(t, byName["alpha"].Ready)
	assert.False(t, byName["beta"].Ready)
}

func TestGetTenantDetail(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		mockTenant("alpha", []string{"a1", "a2"},
			&agentsv1alpha1.TenantModelQuota{BudgetUSD: "100.00", RPM: 600, MaxConcurrent: 20}, true),
	).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	d, code := getTenant(t, s, "alpha")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, d)
	assert.Equal(t, []string{"a1", "a2"}, d.Namespaces)
	require.NotNil(t, d.Model)
	assert.Equal(t, "100.00", d.Model.BudgetUSD)
	assert.Equal(t, int32(600), d.Model.RPM)
	assert.Equal(t, int32(20), d.Model.MaxConcurrent)
	assert.True(t, d.Ready)

	// A missing tenant → 404.
	_, code404 := getTenant(t, s, "nope")
	assert.Equal(t, http.StatusNotFound, code404)
}

// The list is a non-nil [] for zero tenants (never null).
func TestListTenantsEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	body, code := getTenants(t, s)
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items)
	assert.Empty(t, body.Items)
}
