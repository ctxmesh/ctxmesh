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
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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
		UserQuotaStore:   NewRedisUserQuotaStore(mr.Addr(), "", ""),
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
		UserQuotaStore:   NewRedisUserQuotaStore(mr.Addr(), "", ""),
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
		{"POST", "/quota/agent-spend", `{"deltaUSD":5}`}, // Q8: same non-agent-SA gate.
		{"POST", "/quota/slot", `{"max":1}`},
		{"DELETE", "/quota/slot", ""},
		// M107 C20: per-user ops share the same non-agent-SA gate.
		{"POST", "/quota/user-rpm", `{"userHash":"u1","window":1}`},
		{"GET", "/quota/user-spend?userHash=u1", ""},
		{"POST", "/quota/user-spend", `{"userHash":"u1","deltaUSD":5}`},
		{"POST", "/quota/user-slot", `{"userHash":"u1","max":1}`},
		{"DELETE", "/quota/user-slot", `{"userHash":"u1"}`},
	} {
		rec := do(t, s, tc.method, tc.path, "default-tok", tc.body, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: non-agent SA must be 403", tc.method, tc.path)
	}
	// The spoof wrote nothing: no tenant or user accumulator exists.
	assert.Empty(t, mr.Keys(), "a rejected non-agent SA must never touch the tenant or user ledger")
}

// TestQuotaAgentSpend_BooksPerAgentKey proves Q8: /quota/agent-spend resolves the agent identity
// ({ns}/{name}) from the pod token SERVER-SIDE and accrues onto the per-agent breakdown key
// agent:{ns}/{name}:spend:{window} (the SAME key the cost-rollup worker snapshots), so per-agent
// chargeback works in proxy mode — and it does NOT touch the per-tenant aggregate.
func TestQuotaAgentSpend_BooksPerAgentKey(t *testing.T) {
	byToken := map[string]string{"a-tok": "team-a"} // SA agent-a-tok ⇒ agent name "a-tok".
	s, mr := newQuotaProxy(t, byToken, map[string]string{"team-a": "tenant-x"}, nil)

	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/quota/agent-spend", "a-tok", `{"deltaUSD":2.5}`, nil).Code)
	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/quota/agent-spend", "a-tok", `{"deltaUSD":1.5}`, nil).Code)

	// The per-agent key accrued 2.5 + 1.5 = 4.0 under agent:{ns}/{name}:spend:{window}.
	got, err := mr.Get(agentSpendKey("team-a/a-tok"))
	require.NoError(t, err, "the per-agent spend key must exist")
	v, err := strconv.ParseFloat(got, 64)
	require.NoError(t, err)
	assert.InDelta(t, 4.0, v, 1e-9, "per-agent spend accrued both deltas")

	// The per-TENANT aggregate is untouched — per-agent spend is a separate breakdown, not the cap.
	tenantSpend, _ := mr.Get(quotaSpendKey("tenant-x"))
	assert.Empty(t, tenantSpend, "per-agent spend must not touch the tenant aggregate key")
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

// TestUserQuotaProxy_BooksPerUserKey proves that the /quota/user-spend endpoint accrues
// spend under the per-user key user:{userHash}:spend:{month} (byte-identical to the
// launcher's direct-Valkey key in cmd/launcher/user_quota.go), that two distinct userHashes
// get DISTINCT buckets, and that the per-TENANT aggregate is untouched (per-user spend is a
// separate enforcement bucket, never an alias of the tenant key).
func TestUserQuotaProxy_BooksPerUserKey(t *testing.T) {
	byToken := map[string]string{"ag-tok": "ns-a"}
	s, mr := newQuotaProxy(t, byToken, map[string]string{"ns-a": "tenant-x"}, nil)

	// User alice accumulates $3.
	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/quota/user-spend", "ag-tok", `{"userHash":"alice","deltaUSD":2.0}`, nil).Code)
	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/quota/user-spend", "ag-tok", `{"userHash":"alice","deltaUSD":1.0}`, nil).Code)

	// User bob accumulates $5 — a DIFFERENT bucket.
	require.Equal(t, http.StatusNoContent,
		do(t, s, "POST", "/quota/user-spend", "ag-tok", `{"userHash":"bob","deltaUSD":5.0}`, nil).Code)

	// Read alice's spend back via GET.
	rec := do(t, s, "GET", "/quota/user-spend?userHash=alice", "ag-tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"spentUSD":3}`, rec.Body.String(), "alice's spend must accrue both deltas")

	// Bob has his own isolated bucket.
	rec = do(t, s, "GET", "/quota/user-spend?userHash=bob", "ag-tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"spentUSD":5}`, rec.Body.String(), "bob's spend is independent of alice's")

	// The user-spend keys must carry the user:{hash}:spend:{month} grammar.
	userKeys := make([]string, 0)
	for _, k := range mr.Keys() {
		if strings.Contains(k, "user:") && strings.Contains(k, ":spend:") {
			userKeys = append(userKeys, k)
		}
	}
	require.Len(t, userKeys, 2, "exactly two per-user spend keys (alice + bob)")
	assert.True(t, strings.HasPrefix(userKeys[0], "user:") || strings.HasPrefix(userKeys[1], "user:"),
		"keys must use the user: prefix")

	// The per-TENANT aggregate is untouched — per-user spend is a separate enforcement bucket.
	tenantSpend, _ := mr.Get(quotaSpendKey("tenant-x"))
	assert.Empty(t, tenantSpend, "per-user spend must NOT touch the tenant aggregate key")
}

// TestQuotaAgentAndConvSpend_ReadBack is the F2 statelayer half (M126/ADR 0099): the new GET
// /quota/agent-spend + the /quota/conv-spend pair let the launcher ENFORCE per-agent / per-conversation
// budget caps that are real across replicas + survive restarts (the in-memory Enforcer re-armed on every
// roll). Proves accrue→read-back round-trips and per-conversation isolation.
func TestQuotaAgentAndConvSpend_ReadBack(t *testing.T) {
	s, _ := newQuotaProxy(t, map[string]string{"tok": "prod"}, map[string]string{"prod": "acme"}, nil)

	// Agent spend: accrue (POST, Q8) then READ BACK (GET, F2).
	require.Equal(t, http.StatusNoContent, do(t, s, "POST", "/quota/agent-spend", "tok", `{"deltaUSD":0.25}`, nil).Code)
	rec := do(t, s, "GET", "/quota/agent-spend", "tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got quotaSpendResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.InDelta(t, 0.25, got.SpentUSD, 1e-9, "F2: agent spend reads back")

	// Conversation spend: keyed on the launcher-supplied conversation id.
	require.Equal(t, http.StatusNoContent, do(t, s, "POST", "/quota/conv-spend?conversation=c1", "tok", `{"deltaUSD":0.10}`, nil).Code)
	rec = do(t, s, "GET", "/quota/conv-spend?conversation=c1", "tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.InDelta(t, 0.10, got.SpentUSD, 1e-9, "F2: conv spend reads back")

	// A DIFFERENT conversation is isolated (0).
	rec = do(t, s, "GET", "/quota/conv-spend?conversation=other", "tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Zero(t, got.SpentUSD, "F2: a different conversation has its own bucket")

	// conversation is required.
	assert.Equal(t, http.StatusBadRequest, do(t, s, "GET", "/quota/conv-spend", "tok", "", nil).Code)
}
