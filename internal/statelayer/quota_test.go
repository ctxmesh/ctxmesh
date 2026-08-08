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

package statelayer

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakePodAuth maps a pod token → namespace (and, optionally, → SA name for the memory
// path) or a fixed error, standing in for the TokenReview authenticator so the handlers
// can be tested without a cluster.
type fakePodAuth struct {
	byToken   map[string]string // token → namespace
	saByToken map[string]string // token → ServiceAccount name (memory-path tests; optional)
	err       error
}

func (f fakePodAuth) Namespace(ctx context.Context, token string) (string, error) {
	id, err := f.Identity(ctx, token)
	if err != nil {
		return "", err
	}
	return id.Namespace, nil
}

func (f fakePodAuth) Identity(_ context.Context, token string) (PodIdentity, error) {
	if f.err != nil {
		return PodIdentity{}, f.err
	}
	ns, ok := f.byToken[token]
	if !ok {
		return PodIdentity{}, ErrTokenRejected
	}
	return PodIdentity{Namespace: ns, ServiceAccount: f.saByToken[token]}, nil
}

func newQuotaProxy(t *testing.T, byToken, nsToTenant map[string]string, auth PodAuthenticator) (*Server, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	objs := make([]client.Object, 0, len(nsToTenant))
	for ns, tenant := range nsToTenant {
		objs = append(objs, nsObj(ns, map[string]string{tenantLabel: tenant}))
	}
	cl := fake.NewClientBuilder().WithObjects(objs...).Build()
	if auth == nil {
		auth = fakePodAuth{byToken: byToken}
	}
	s, err := NewServer(Options{
		Store:            NewRedisStore(mr.Addr(), "", ""),
		QuotaStore:       NewRedisQuotaStore(mr.Addr(), "", ""),
		PodAuthenticator: auth,
		TenantResolver:   NewLabelTenantResolver(cl),
	})
	require.NoError(t, err)
	return s, mr
}

// THE HEADLINE: quota counters are scoped to the proxy-DERIVED tenant, so one
// tenant's spend/rpm/slots are invisible to another — even though the launcher
// never names a tenant.
func TestQuotaCrossTenantIsolation(t *testing.T) {
	s, mr := newQuotaProxy(t,
		map[string]string{"alpha-tok": "team-alpha-ns", "beta-tok": "team-beta-ns"},
		map[string]string{"team-alpha-ns": "team-alpha", "team-beta-ns": "team-beta"}, nil)

	// team-alpha records $5 of spend.
	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/quota/spend", "alpha-tok", `{"deltaUSD":5.0}`, nil).Code)

	// team-beta's spend is a DIFFERENT accumulator — still $0.
	rec := do(t, s, "GET", "/quota/spend", "beta-tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"spentUSD":0}`, rec.Body.String())

	// team-alpha sees its own $5.
	rec = do(t, s, "GET", "/quota/spend", "alpha-tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"spentUSD":5}`, rec.Body.String())

	// The Valkey key is scoped to the DERIVED tenant, never caller input.
	var spendKeys []string
	for _, k := range mr.Keys() {
		if strings.Contains(k, ":spend:") {
			spendKeys = append(spendKeys, k)
		}
	}
	require.Len(t, spendKeys, 1)
	assert.True(t, strings.HasPrefix(spendKeys[0], "tenant:team-alpha:spend:"), "got %q", spendKeys[0])
}

func TestQuotaRPMIncrements(t *testing.T) {
	s, _ := newQuotaProxy(t, map[string]string{"t": "team-alpha-ns"},
		map[string]string{"team-alpha-ns": "team-alpha"}, nil)

	rec := do(t, s, "POST", "/quota/rpm", "t", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"count":1}`, rec.Body.String())

	rec = do(t, s, "POST", "/quota/rpm", "t", "", nil)
	assert.JSONEq(t, `{"count":2}`, rec.Body.String())
}

// A concurrency slot is granted up to max, then denied; releasing frees one.
func TestQuotaSlots(t *testing.T) {
	s, _ := newQuotaProxy(t, map[string]string{"t": "team-alpha-ns"},
		map[string]string{"team-alpha-ns": "team-alpha"}, nil)

	rec := do(t, s, "POST", "/quota/slot", "t", `{"max":1}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"acquired":true}`, rec.Body.String())

	// Second acquire exceeds max=1 → denied (and rolled back).
	rec = do(t, s, "POST", "/quota/slot", "t", `{"max":1}`, nil)
	assert.JSONEq(t, `{"acquired":false}`, rec.Body.String())

	// Release, then re-acquire succeeds.
	require.Equal(t, http.StatusNoContent, do(t, s, "DELETE", "/quota/slot", "t", "", nil).Code)
	rec = do(t, s, "POST", "/quota/slot", "t", `{"max":1}`, nil)
	assert.JSONEq(t, `{"acquired":true}`, rec.Body.String())
}

// The auth boundary: rejected token → 401; auth-infra error → 503; untenanted
// namespace → 404; no quota store configured → 503.
func TestQuotaAuthBoundary(t *testing.T) {
	t.Run("rejected pod token → 401", func(t *testing.T) {
		s, _ := newQuotaProxy(t, map[string]string{"good": "team-alpha-ns"},
			map[string]string{"team-alpha-ns": "team-alpha"}, nil)
		assert.Equal(t, http.StatusUnauthorized, do(t, s, "POST", "/quota/rpm", "bad-token", "", nil).Code)
	})

	t.Run("auth-infra error → 503", func(t *testing.T) {
		s, _ := newQuotaProxy(t, nil,
			map[string]string{"team-alpha-ns": "team-alpha"},
			fakePodAuth{err: errors.New("tokenreview unreachable")})
		assert.Equal(t, http.StatusServiceUnavailable, do(t, s, "POST", "/quota/rpm", "x", "", nil).Code)
	})

	t.Run("untenanted namespace → 404 (launcher allows)", func(t *testing.T) {
		// The token maps to a namespace the resolver has no tenant for.
		s, _ := newQuotaProxy(t, map[string]string{"t": "orphan-ns"},
			map[string]string{"team-alpha-ns": "team-alpha"}, nil)
		assert.Equal(t, http.StatusNotFound, do(t, s, "GET", "/quota/spend", "t", "", nil).Code)
	})

	t.Run("no quota store configured → 503", func(t *testing.T) {
		mr := miniredis.RunT(t)
		s, err := NewServer(Options{Store: NewRedisStore(mr.Addr(), "", "")})
		require.NoError(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, do(t, s, "POST", "/quota/rpm", "t", "", nil).Code)
	})
}

// A Valkey backend failure surfaces as 502 (the launcher maps 5xx per op).
func TestQuotaBackendError(t *testing.T) {
	s, mr := newQuotaProxy(t, map[string]string{"t": "team-alpha-ns"},
		map[string]string{"team-alpha-ns": "team-alpha"}, nil)
	mr.Close() // Valkey now unreachable
	assert.Equal(t, http.StatusBadGateway, do(t, s, "GET", "/quota/spend", "t", "", nil).Code)
}
