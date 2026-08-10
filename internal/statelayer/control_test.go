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
	"errors"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newControlProxy builds a proxy with a control store + pod authenticator (no tenant resolver — the
// /control endpoint is pod-authed but NOT tenant-scoped). Returns the server + the backing miniredis so a
// test can seed the `run:{id}:control` marker the BFF would write.
func newControlProxy(t *testing.T, byToken map[string]string, auth PodAuthenticator) (*Server, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	if auth == nil {
		auth = fakePodAuth{byToken: byToken}
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
	// The BFF writes this exact key (internal/bff/run_control.go runControlKey).
	require.NoError(t, mr.Set("run:run-123:control", "cancel"))

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

// The auth boundary: an UNauthenticated caller is REJECTED before any read — the same pod-auth the
// quota/dedup endpoints use, not weakened. A rejected token → 401; auth-infra down → 503; no control
// store configured → 503.
func TestControlGet_AuthBoundary(t *testing.T) {
	t.Run("rejected pod token → 401", func(t *testing.T) {
		s, _ := newControlProxy(t, map[string]string{"good": "team-alpha-ns"}, nil)
		assert.Equal(t, http.StatusUnauthorized, do(t, s, "GET", "/control/r", "bad-token", "", nil).Code)
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
