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

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestUserProxyStore builds an httpUserStore backed by a test HTTP server and a
// temp token file containing "USER-PROXY-TOKEN". It mirrors newTestProxyStore for the
// tenant path, so the two families share the same setup pattern.
func newTestUserProxyStore(t *testing.T, handler http.HandlerFunc) *httpUserStore {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokPath, []byte("USER-PROXY-TOKEN\n"), 0o600))
	return newHTTPUserStore(srv.URL, tokPath)
}

// TestHTTPUserStoreRoundTrips asserts that httpUserStore hits the right endpoint +
// method for each op, sends the userHash in the body / query, and parses the
// response correctly. The store MUST pass the userHash (unlike the tenant store which
// ignores its ID arg — the proxy cannot derive the end-user from the pod token).
func TestHTTPUserStoreRoundTrips(t *testing.T) {
	ctx := context.Background()
	var gotAuth, gotPath, gotMethod, gotBody string
	s := newTestUserProxyStore(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/quota/user-rpm":
			_, _ = w.Write([]byte(`{"count":7}`))
		case r.Method == http.MethodGet && r.URL.Path == "/quota/user-spend":
			_, _ = w.Write([]byte(`{"spentUSD":3.14}`))
		case r.Method == http.MethodPost && r.URL.Path == "/quota/user-spend":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/quota/user-slot":
			_, _ = w.Write([]byte(`{"acquired":true}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/quota/user-slot":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	})

	// IncrRPM → POST /quota/user-rpm, body includes userHash + window.
	n, err := s.IncrRPM(ctx, "u-alice", 999)
	require.NoError(t, err)
	assert.Equal(t, int64(7), n)
	assert.Equal(t, "Bearer USER-PROXY-TOKEN", gotAuth, "pod token sent as Bearer")
	assert.Contains(t, gotPath, "/quota/user-rpm")
	assert.Contains(t, gotBody, `"userHash":"u-alice"`)
	assert.Contains(t, gotBody, `"window":999`)

	// Spend → GET /quota/user-spend?userHash=..., userHash in query.
	spent, err := s.Spend(ctx, "u-alice")
	require.NoError(t, err)
	assert.Equal(t, 3.14, spent)
	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Contains(t, gotPath, "userHash=u-alice")

	// AddSpend → POST /quota/user-spend, body includes userHash + deltaUSD.
	require.NoError(t, s.AddSpend(ctx, "u-alice", 1.5))
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Contains(t, gotBody, `"userHash":"u-alice"`)
	assert.Contains(t, gotBody, `"deltaUSD":1.5`)

	// AcquireSlot → POST /quota/user-slot, body includes userHash + max.
	ok, err := s.AcquireSlot(ctx, "u-alice", 3)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Contains(t, gotBody, `"userHash":"u-alice"`)
	assert.Contains(t, gotBody, `"max":3`)

	// ReleaseSlot → DELETE /quota/user-slot, body includes userHash.
	require.NoError(t, s.ReleaseSlot(ctx, "u-alice"))
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Contains(t, gotBody, `"userHash":"u-alice"`)
}

// TestHTTPUserStore404Permissive: a 404 from the proxy (not configured) maps to the
// PERMISSIVE value per op — the launcher's existing nil-quota "allow" path.
//
//nolint:dupl // structurally mirrors TestHTTPQuota404Permissive (tenant path) — distinct types under test.
func TestHTTPUserStore404Permissive(t *testing.T) {
	ctx := context.Background()
	s := newTestUserProxyStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	n, err := s.IncrRPM(ctx, "u", 1)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "404 → count 0 (allow)")

	spent, err := s.Spend(ctx, "u")
	require.NoError(t, err)
	assert.Equal(t, 0.0, spent, "404 → spent 0 (allow)")

	require.NoError(t, s.AddSpend(ctx, "u", 1), "404 → no-op")

	ok, err := s.AcquireSlot(ctx, "u", 1)
	require.NoError(t, err)
	assert.True(t, ok, "404 → grant (no cap applies)")

	require.NoError(t, s.ReleaseSlot(ctx, "u"), "404 → no-op")
}

// TestHTTPUserStoreErrorsAreErrors: a non-2xx/404 is returned as an error so preCall
// can apply its fail policy (budget CLOSED, rate/concurrency OPEN).
func TestHTTPUserStoreErrorsAreErrors(t *testing.T) {
	ctx := context.Background()
	for _, code := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusBadGateway} {
		s := newTestUserProxyStore(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		_, err := s.IncrRPM(ctx, "u", 1)
		assert.Error(t, err, "rpm status %d", code)
		_, err = s.Spend(ctx, "u")
		assert.Error(t, err, "spend status %d", code)
		require.Error(t, s.AddSpend(ctx, "u", 1), "addspend status %d", code)
		_, err = s.AcquireSlot(ctx, "u", 1)
		assert.Error(t, err, "slot status %d", code)
		require.Error(t, s.ReleaseSlot(ctx, "u"), "release status %d", code)
	}
}

// TestBuildUserQuota_ProxyModeSelectsHTTPUserStore asserts that buildUserQuota selects
// *httpUserStore when StatelayerProxyURL is set (proxy mode), even when QuotaAddr is
// also present — proxy takes precedence, mirroring newAgentSpendAccountant (M107 C20).
func TestBuildUserQuota_ProxyModeSelectsHTTPUserStore(t *testing.T) {
	// A minimal policy carrying a userRateLimit so buildUserQuota doesn't return nil early.
	policy := userLimitPolicy(5, "", 0)

	dir := t.TempDir()
	tokPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokPath, []byte("TOK"), 0o600))

	cfg := gatewayConfig{
		StatelayerProxyURL: "http://proxy.internal:8080",
		PodTokenPath:       tokPath,
		// QuotaAddr is deliberately also set — proxy must take precedence.
		QuotaAddr: "localhost:6379",
	}

	uq, err := buildUserQuota(policy, cfg, noopLog)
	require.NoError(t, err)
	require.NotNil(t, uq, "a userRateLimit + proxy URL must build a per-user quota")
	assert.IsType(t, (*httpUserStore)(nil), uq.store,
		"proxy mode must select *httpUserStore, not *redisUserStore")
}
