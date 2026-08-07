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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// fakeTenantUsage is a TenantUsageReader double — no live Valkey in unit tests.
type fakeTenantUsage struct {
	usage TenantUsage
	err   error
	got   string // the tenantID the handler asked for
}

func (f *fakeTenantUsage) Usage(_ context.Context, tenantID string) (TenantUsage, error) {
	f.got = tenantID
	return f.usage, f.err
}

func newUsageServer(t *testing.T, factory CallerClientFactory, reader TenantUsageReader) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients: factory,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Expand: NewExpandAdapter()},
		Version:       "test",
		Log:           logr.Discard(),
		TenantUsage:   reader,
	})
}

func getTenantUsage(t *testing.T, s *Server, name string) (*TenantUsage, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/"+name+"/usage", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var u TenantUsage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &u))
		return &u, rec.Code
	}
	return nil, rec.Code
}

// Happy path: the tenant exists, the reader returns live consumption, keyed by the tenant name.
func TestTenantUsage(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		mockTenant("alpha", []string{"a1"},
			&agentsv1alpha1.TenantModelQuota{BudgetUSD: "100.00", RPM: 600, MaxConcurrent: 20}, true),
	).Build()
	reader := &fakeTenantUsage{usage: TenantUsage{SpendUSD: 12.5, RPM: 42, InFlight: 3}}
	s := newUsageServer(t, &fakeCallerClientFactory{client: c}, reader)

	u, code := getTenantUsage(t, s, "alpha")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, u)
	assert.Equal(t, "alpha", reader.got, "the reader is keyed by the tenant name")
	assert.InDelta(t, 12.5, u.SpendUSD, 0.001)
	assert.Equal(t, int64(42), u.RPM)
	assert.Equal(t, int64(3), u.InFlight)
}

// The batched endpoint returns live usage for every listable tenant in one call
// (m54.5), so the tenants list can render a near-cap indicator without a per-row
// fan-out.
func TestTenantUsageList(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		mockTenant("alpha", []string{"a1"}, &agentsv1alpha1.TenantModelQuota{BudgetUSD: "100.00", RPM: 600}, true),
		mockTenant("beta", []string{"b1"}, &agentsv1alpha1.TenantModelQuota{RPM: 300}, true),
	).Build()
	reader := &fakeTenantUsage{usage: TenantUsage{SpendUSD: 5, RPM: 10, InFlight: 1}}
	s := newUsageServer(t, &fakeCallerClientFactory{client: c}, reader)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/usage", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "the literal /usage route must win over /{name}")

	var resp TenantUsageListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Items, 2)
	names := map[string]bool{resp.Items[0].Name: true, resp.Items[1].Name: true}
	assert.True(t, names["alpha"] && names["beta"], "every listable tenant has a usage row")
	assert.InDelta(t, 5, resp.Items[0].SpendUSD, 0.001)
}

// No usage reader wired ⇒ the batched endpoint is an honest 501, never a 500.
func TestTenantUsageListNoReaderIs501(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newUsageServer(t, &fakeCallerClientFactory{client: c}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/usage", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// A missing tenant → 404, authorized via the caller-scoped Get BEFORE any usage read.
func TestTenantUsageMissingTenant(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	reader := &fakeTenantUsage{}
	s := newUsageServer(t, &fakeCallerClientFactory{client: c}, reader)

	_, code := getTenantUsage(t, s, "nope")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Empty(t, reader.got, "usage must not be read for a tenant the caller can't see")
}

// No state-layer wired → honest 501 (not a 500), even when the tenant exists.
func TestTenantUsageNoReader(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		mockTenant("alpha", []string{"a1"}, nil, true),
	).Build()
	s := newUsageServer(t, &fakeCallerClientFactory{client: c}, nil)

	_, code := getTenantUsage(t, s, "alpha")
	assert.Equal(t, http.StatusNotImplemented, code)
}

// A reader failure → 500 (the tenant exists, the Valkey read failed).
func TestTenantUsageReaderError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		mockTenant("alpha", []string{"a1"}, nil, true),
	).Build()
	reader := &fakeTenantUsage{err: errors.New("valkey down")}
	s := newUsageServer(t, &fakeCallerClientFactory{client: c}, reader)

	_, code := getTenantUsage(t, s, "alpha")
	assert.Equal(t, http.StatusInternalServerError, code)
}
