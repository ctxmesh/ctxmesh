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

// agentSAsFor gives every token a distinct per-agent identity SA (agent-<token>), so a
// fake built from a token→namespace map authenticates as a real agent on the m79.2
// quota/dedup paths (which now reject a non-agent SA). Tests that need two tokens to be
// the SAME agent, or a non-agent SA, populate saByToken explicitly instead.
func agentSAsFor(byToken map[string]string) map[string]string {
	sas := make(map[string]string, len(byToken))
	for tok := range byToken {
		sas[tok] = agentSAPrefix + tok
	}
	return sas
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
		// Every token authenticates as a per-agent identity SA (agent-<token>) — the
		// m79.2 quota path now REQUIRES an agent identity (a non-agent SA is 403'd), so
		// the default fake registers one for each token unless a test overrides saByToken.
		auth = fakePodAuth{byToken: byToken, saByToken: agentSAsFor(byToken)}
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

// m79.2 (m52 C7): the quota paths BIND the verified per-agent identity — a verified
// pod token that is NOT a per-agent identity SA (e.g. the namespace default SA) is
// 403'd, exactly like the memory path. Before the fix, ANY pod in a tenant namespace
// could spend the tenant's shared budget.
func TestQuotaNonAgentSARejected(t *testing.T) {
	mr := miniredis.RunT(t)
	cl := fake.NewClientBuilder().WithObjects(
		nsObj("team-alpha-ns", map[string]string{tenantLabel: "team-alpha"})).Build()
	// The token authenticates (namespace resolves) but its SA is the namespace default,
	// NOT an agent-<name> identity — so it has no agent scope on a workload path.
	auth := fakePodAuth{
		byToken:   map[string]string{"default-tok": "team-alpha-ns"},
		saByToken: map[string]string{"default-tok": "default"},
	}
	s, err := NewServer(Options{
		Store:            NewRedisStore(mr.Addr(), "", ""),
		QuotaStore:       NewRedisQuotaStore(mr.Addr(), "", ""),
		PodAuthenticator: auth,
		TenantResolver:   NewLabelTenantResolver(cl),
	})
	require.NoError(t, err)

	for _, tc := range []struct {
		method, path, body string
	}{
		{"POST", "/quota/rpm", ""},
		{"GET", "/quota/spend", ""},
		{"POST", "/quota/spend", `{"deltaUSD":5}`},
		{"POST", "/quota/slot", `{"max":1}`},
		{"DELETE", "/quota/slot", ""},
	} {
		rec := do(t, s, tc.method, tc.path, "default-tok", tc.body, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: non-agent SA must be 403", tc.method, tc.path)
	}
	// The spoof wrote nothing: no tenant accumulator exists.
	assert.Empty(t, mr.Keys(), "a rejected non-agent SA must never touch the tenant ledger")
}

// m79.2: a pod in a tenant namespace whose SA is NOT an agent (the concrete C7 spoof —
// a non-agent workload acting on a sibling agent's quota scope) is rejected, while a
// real agent pod in the SAME tenant still shares the intended per-TENANT budget. This
// pins that binding the identity did NOT re-partition the aggregate.
func TestQuotaAgentBindingPreservesTenantAggregate(t *testing.T) {
	mr := miniredis.RunT(t)
	cl := fake.NewClientBuilder().WithObjects(
		nsObj("team-alpha-ns", map[string]string{tenantLabel: "team-alpha"})).Build()
	// Two DIFFERENT agent pods in the SAME tenant namespace.
	auth := fakePodAuth{
		byToken: map[string]string{"agentA-tok": "team-alpha-ns", "agentB-tok": "team-alpha-ns"},
		saByToken: map[string]string{
			"agentA-tok": "agent-alpha-one",
			"agentB-tok": "agent-alpha-two",
		},
	}
	s, err := NewServer(Options{
		Store:            NewRedisStore(mr.Addr(), "", ""),
		QuotaStore:       NewRedisQuotaStore(mr.Addr(), "", ""),
		PodAuthenticator: auth,
		TenantResolver:   NewLabelTenantResolver(cl),
	})
	require.NoError(t, err)

	// Agent A records $3; agent B records $4 — a SHARED per-tenant budget (ADR 0047), so
	// both land on the SAME accumulator and the tenant total is $7.
	require.Equal(t, http.StatusNoContent, do(t, s, "POST", "/quota/spend", "agentA-tok", `{"deltaUSD":3}`, nil).Code)
	require.Equal(t, http.StatusNoContent, do(t, s, "POST", "/quota/spend", "agentB-tok", `{"deltaUSD":4}`, nil).Code)

	rec := do(t, s, "GET", "/quota/spend", "agentB-tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"spentUSD":7}`, rec.Body.String(), "sibling agents SHARE the per-tenant budget — binding must not re-partition it")

	// Exactly ONE tenant-scoped spend key (not one per agent).
	var spendKeys []string
	for _, k := range mr.Keys() {
		if strings.Contains(k, ":spend:") {
			spendKeys = append(spendKeys, k)
		}
	}
	require.Len(t, spendKeys, 1)
	assert.True(t, strings.HasPrefix(spendKeys[0], "tenant:team-alpha:spend:"), "the key stays per-tenant, not per-agent: got %q", spendKeys[0])
}

// A Valkey backend failure surfaces as 502 (the launcher maps 5xx per op).
func TestQuotaBackendError(t *testing.T) {
	s, mr := newQuotaProxy(t, map[string]string{"t": "team-alpha-ns"},
		map[string]string{"team-alpha-ns": "team-alpha"}, nil)
	mr.Close() // Valkey now unreachable
	assert.Equal(t, http.StatusBadGateway, do(t, s, "GET", "/quota/spend", "t", "", nil).Code)
}
