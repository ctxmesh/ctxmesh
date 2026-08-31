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
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newControlProxy builds a proxy with a control store + pod authenticator. Returns the server + the
// backing miniredis so a test can seed the `run:{ns}:{id}:control` marker the BFF would write. Since
// M142.3 the read is scoped to the AUTHENTICATED pod's namespace, so the seeded key must carry it.
func newControlProxy(t *testing.T, byToken map[string]string, auth PodAuthenticator) (*Server, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	if auth == nil {
		// m52.C12: /control now requires an AGENT identity (authenticateAgentNamespace), so every default
		// token authenticates as a per-agent SA (agent-<token>) — mirroring the m79.2 quota/dedup tests.
		sa := make(map[string]string, len(byToken))
		for tok := range byToken {
			sa[tok] = "agent-" + tok
		}
		auth = fakePodAuth{byToken: byToken, saByToken: sa}
	}
	s, err := NewServer(Options{
		Store:            NewRedisStore(mr.Addr(), "", ""),
		ControlStore:     NewRedisControlStore(mr.Addr(), "", ""),
		PodAuthenticator: auth,
	})
	require.NoError(t, err)
	return s, mr
}

// A pod-authed GET /control/{runID} returns the verb the BFF wrote to `run:{id}:control`.
func TestControlGet_PodAuthedReturnsVerb(t *testing.T) {
	s, mr := newControlProxy(t, map[string]string{"pod-tok": "team-alpha-ns"}, nil)
	// The BFF writes this exact key (internal/bff/run_control.go runControlKey) — namespace included.
	require.NoError(t, mr.Set("run:team-alpha-ns:run-123:control", "cancel"))

	rec := do(t, s, "GET", "/control/run-123", "pod-tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"control":"cancel"}`, rec.Body.String())
}

// An ABSENT marker → 200 with an empty verb (the common, no-cancel case) — never a 404/error.
func TestControlGet_AbsentKeyIsEmptyVerb(t *testing.T) {
	s, _ := newControlProxy(t, map[string]string{"pod-tok": "team-alpha-ns"}, nil)

	rec := do(t, s, "GET", "/control/never-cancelled", "pod-tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"control":""}`, rec.Body.String())
}

// The auth boundary: an UNauthenticated OR non-agent caller is REJECTED before any read — the same
// AGENT-auth the quota/dedup endpoints use (m79.2/C12), not weakened. A rejected token → 401; a
// verified-but-non-agent SA → 403; auth-infra down → 503; no control store configured → 503.
func TestControlGet_AuthBoundary(t *testing.T) {
	t.Run("rejected pod token → 401", func(t *testing.T) {
		s, _ := newControlProxy(t, map[string]string{"good": "team-alpha-ns"}, nil)
		assert.Equal(t, http.StatusUnauthorized, do(t, s, "GET", "/control/r", "bad-token", "", nil).Code)
	})

	// m52.C12: a token that authenticates but whose SA is NOT an agent identity (e.g. the namespace
	// `default` SA) is 403'd — closing the gap where any pod in a tenant namespace could read /control.
	t.Run("verified non-agent SA → 403", func(t *testing.T) {
		auth := fakePodAuth{
			byToken:   map[string]string{"nonagent-tok": "team-alpha-ns"},
			saByToken: map[string]string{"nonagent-tok": "default"}, // NOT agent-<name>
		}
		s, _ := newControlProxy(t, nil, auth)
		assert.Equal(t, http.StatusForbidden, do(t, s, "GET", "/control/r", "nonagent-tok", "", nil).Code)
	})

	t.Run("missing token → 401", func(t *testing.T) {
		s, _ := newControlProxy(t, map[string]string{"good": "team-alpha-ns"}, nil)
		assert.Equal(t, http.StatusUnauthorized, do(t, s, "GET", "/control/r", "", "", nil).Code)
	})

	t.Run("auth-infra error → 503", func(t *testing.T) {
		s, _ := newControlProxy(t, nil, fakePodAuth{err: errors.New("tokenreview unreachable")})
		assert.Equal(t, http.StatusServiceUnavailable, do(t, s, "GET", "/control/r", "x", "", nil).Code)
	})

	t.Run("no control store configured → 503", func(t *testing.T) {
		mr := miniredis.RunT(t)
		s, err := NewServer(Options{
			Store:            NewRedisStore(mr.Addr(), "", ""),
			PodAuthenticator: fakePodAuth{byToken: map[string]string{"t": "ns"}},
		})
		require.NoError(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, do(t, s, "GET", "/control/r", "t", "", nil).Code)
	})
}

// An UNauthenticated caller must NOT be able to read a marker even when one exists — the auth gate runs
// BEFORE the Valkey read (a rejected token gets 401, never the verb).
func TestControlGet_UnauthenticatedCannotReadExistingMarker(t *testing.T) {
	s, mr := newControlProxy(t, map[string]string{"pod-tok": "team-alpha-ns"}, nil)
	require.NoError(t, mr.Set("run:secret-run:control", "cancel"))

	rec := do(t, s, "GET", "/control/secret-run", "bad-token", "", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotContains(t, rec.Body.String(), "cancel", "an unauthenticated caller must never see the verb")
}

// A Valkey backend failure surfaces as 502 (the launcher's control client fails OPEN — no verb ⇒ no cancel).
func TestControlGet_BackendError(t *testing.T) {
	s, mr := newControlProxy(t, map[string]string{"pod-tok": "team-alpha-ns"}, nil)
	mr.Close() // Valkey now unreachable
	assert.Equal(t, http.StatusBadGateway, do(t, s, "GET", "/control/r", "pod-tok", "", nil).Code)
}

// THE C15 FIX (M142.3): the read is scoped to the CALLER's own namespace. Before it, /control was
// agent-authenticated but not run-scoped — any verified agent could name any run id and learn whether it
// was being cancelled, a cross-tenant read in a shared state layer. The namespace comes from the pod's
// verified identity and is what the key is built from, so this is structural, not a check that could be
// forgotten: an agent in another namespace simply looks somewhere else.
func TestControlGet_CannotReadAnotherNamespacesRun(t *testing.T) {
	s, mr := newControlProxy(t, map[string]string{
		"alpha-tok": "team-alpha-ns",
		"beta-tok":  "team-beta-ns",
	}, nil)
	// A run being cancelled in team-alpha-ns.
	require.NoError(t, mr.Set("run:team-alpha-ns:run-123:control", "cancel"))

	// Its owner sees the verb.
	owner := do(t, s, "GET", "/control/run-123", "alpha-tok", "", nil)
	require.Equal(t, http.StatusOK, owner.Code)
	assert.JSONEq(t, `{"control":"cancel"}`, owner.Body.String())

	// An agent in ANOTHER namespace, naming the same run id, learns nothing. It is answered 200 with an
	// empty verb — the same response as "no such run" — so the endpoint is not an existence oracle either.
	stranger := do(t, s, "GET", "/control/run-123", "beta-tok", "", nil)
	require.Equal(t, http.StatusOK, stranger.Code)
	assert.JSONEq(t, `{"control":""}`, stranger.Body.String(),
		"a cross-namespace read must return nothing — and be indistinguishable from an absent marker")
}

// ─── M146 scope hierarchy (ADR 0126) ──────────────────────────────────────────────────────────────
// Before M146 the platform could stop ONE run and nothing larger. The hierarchy is walked SERVER-SIDE
// from the verified pod identity, so `GET /control/{runID}` keeps its exact wire contract and an
// already-deployed launcher gains scope kills with no image rebuild.

// fakeTenantResolver maps namespace → tenant without a cluster. errNS forces the infrastructure-error
// path so a test can prove an unresolvable tenant SKIPS that scope rather than failing the whole read.
type fakeTenantResolver struct {
	byNS  map[string]string
	errNS map[string]bool
}

func (f fakeTenantResolver) TenantID(_ context.Context, namespace string) (string, error) {
	if f.errNS[namespace] {
		return "", errors.New("tenant resolver unreachable")
	}
	return f.byNS[namespace], nil
}

// newScopedProxy builds a control proxy whose pod token authenticates as agent `agent-<name>` in ns, and
// whose namespace resolves to the given tenant — the identity the hierarchy is derived from.
func newScopedProxy(t *testing.T, tok, ns, agent, tenant string) (*Server, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	s, err := NewServer(Options{
		Store:            NewRedisStore(mr.Addr(), "", ""),
		ControlStore:     NewRedisControlStore(mr.Addr(), "", ""),
		PodAuthenticator: fakePodAuth{byToken: map[string]string{tok: ns}, saByToken: map[string]string{tok: "agent-" + agent}},
		TenantResolver:   fakeTenantResolver{byNS: map[string]string{ns: tenant}},
	})
	require.NoError(t, err)
	return s, mr
}

// THE BAR: a kill written at ANY scope covering the run is seen by that run — one write, whole blast
// radius. Each scope is asserted on its own so a single over-broad match cannot pass for all five.
func TestControlGet_AKillAtAnyScopeCoversTheRun(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{"run", "run:team-a:run-1:control"},
		{"agent", "agent:team-a:support-bot:control"},
		{"namespace", "ns:team-a:control"},
		{"tenant", "tenant:acme:control"},
		{"fleet", "fleet:control"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mr := newScopedProxy(t, "pod-tok", "team-a", "support-bot", "acme")
			require.NoError(t, mr.Set(tc.key, "cancel"))

			rec := do(t, s, "GET", "/control/run-1", "pod-tok", "", nil)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), "cancel",
				"a kill at the %s scope must reach every run beneath it", tc.name)
		})
	}
}

// The discriminating half: a kill on a SIBLING scope must NOT stop this run. A switch that stops
// everything is indistinguishable from a broken platform, so the test above proves nothing without this.
func TestControlGet_ASiblingScopeKillIsNotSeen(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{"another run", "run:team-a:run-OTHER:control"},
		{"another agent in the same namespace", "agent:team-a:billing-bot:control"},
		{"another namespace", "ns:team-b:control"},
		{"another tenant", "tenant:globex:control"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mr := newScopedProxy(t, "pod-tok", "team-a", "support-bot", "acme")
			require.NoError(t, mr.Set(tc.key, "cancel"))

			rec := do(t, s, "GET", "/control/run-1", "pod-tok", "", nil)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.NotContains(t, rec.Body.String(), "cancel",
				"a kill scoped to %s must not stop an unrelated run", tc.name)
		})
	}
}

// An untenanted namespace (or no resolver) SKIPS the tenant scope rather than guessing one — and every
// other scope still works. Guessing a tenant here would let one namespace's kill leak into another's.
func TestControlGet_NoTenantSkipsThatScopeAndStillWorks(t *testing.T) {
	mr := miniredis.RunT(t)
	s, err := NewServer(Options{
		Store:            NewRedisStore(mr.Addr(), "", ""),
		ControlStore:     NewRedisControlStore(mr.Addr(), "", ""),
		PodAuthenticator: fakePodAuth{byToken: map[string]string{"pod-tok": "team-a"}, saByToken: map[string]string{"pod-tok": "agent-support-bot"}},
	})
	require.NoError(t, err)

	require.NoError(t, mr.Set("tenant::control", "cancel")) // the key a naive empty-tenant build would read
	rec := do(t, s, "GET", "/control/run-1", "pod-tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "cancel", "an empty tenant must not synthesise a scope key")

	require.NoError(t, mr.Set("ns:team-a:control", "cancel"))
	rec = do(t, s, "GET", "/control/run-1", "pod-tok", "", nil)
	assert.Contains(t, rec.Body.String(), "cancel", "the remaining scopes must still be honoured")
}

// Precedence is cheapest-first, so a per-run cancel keeps the ordering it always had even when a wider
// scope is also set — the hierarchy must not change what a single-run cancel means.
func TestControlGet_ScopesAreWalkedCheapestFirst(t *testing.T) {
	s, mr := newScopedProxy(t, "pod-tok", "team-a", "support-bot", "acme")
	require.NoError(t, mr.Set("run:team-a:run-1:control", "cancel"))
	require.NoError(t, mr.Set("fleet:control", "some-future-verb"))

	rec := do(t, s, "GET", "/control/run-1", "pod-tok", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "cancel")
	assert.NotContains(t, rec.Body.String(), "some-future-verb",
		"the most specific scope wins — a per-run cancel is not overridden by a wider marker")
}

// Scope keys are built ONLY from the verified identity, never from the request, so a caller cannot widen
// its own blast radius by naming someone else's namespace/agent/tenant in the run id.
func TestScopeKeys_BuiltOnlyFromVerifiedIdentity(t *testing.T) {
	keys := scopeKeys(ControlScope{Namespace: "team-a", Agent: "support-bot", Tenant: "acme", RunID: "run-1"})
	assert.Equal(t, []string{
		"run:team-a:run-1:control",
		"agent:team-a:support-bot:control",
		"ns:team-a:control",
		"tenant:acme:control",
		"fleet:control",
	}, keys, "the exact cross-package key contract — the BFF writer must match these byte for byte")

	bare := scopeKeys(ControlScope{Namespace: "team-a", RunID: "run-1"})
	assert.NotContains(t, bare, "agent:team-a::control", "an empty agent must not synthesise a key")
	assert.NotContains(t, bare, "tenant::control", "an empty tenant must not synthesise a key")
}
