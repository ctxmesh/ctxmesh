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

func newTestProxyStore(t *testing.T, handler http.HandlerFunc) *httpTenantStore {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokPath, []byte("POD-TOKEN\n"), 0o600))
	return newHTTPTenantStore(srv.URL, tokPath)
}

// The store sends the pod token as Bearer, hits the right path/method, and parses
// each op's response — sending NO tenant id (the proxy derives it).
func TestHTTPQuotaRoundTrips(t *testing.T) {
	ctx := context.Background()
	var gotAuth, gotPath, gotMethod, gotBody string
	s := newTestProxyStore(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/quota/rpm":
			_, _ = w.Write([]byte(`{"count":3}`))
		case r.Method == http.MethodGet && r.URL.Path == "/quota/spend":
			_, _ = w.Write([]byte(`{"spentUSD":2.5}`))
		case r.Method == http.MethodPost && r.URL.Path == "/quota/spend":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/quota/slot":
			_, _ = w.Write([]byte(`{"acquired":true}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/quota/slot":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	})

	n, err := s.IncrRPM(ctx, "ignored-tenant", 42)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
	assert.Equal(t, "Bearer POD-TOKEN", gotAuth, "the projected pod token is sent as Bearer")
	assert.Equal(t, "/quota/rpm", gotPath)

	spent, err := s.Spend(ctx, "ignored")
	require.NoError(t, err)
	assert.Equal(t, 2.5, spent)

	require.NoError(t, s.AddSpend(ctx, "ignored", 1.25))
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.JSONEq(t, `{"deltaUSD":1.25}`, gotBody)

	ok, err := s.AcquireSlot(ctx, "ignored", 5)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.JSONEq(t, `{"max":5}`, gotBody)

	require.NoError(t, s.ReleaseSlot(ctx, "ignored"))
	assert.Equal(t, http.MethodDelete, gotMethod)
}

// A 404 (the proxy has no tenant for this namespace) maps to the PERMISSIVE value
// per op — the launcher's existing nil-quota "allow" path.
func TestHTTPQuota404Permissive(t *testing.T) {
	ctx := context.Background()
	s := newTestProxyStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	n, err := s.IncrRPM(ctx, "t", 1)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "404 → count 0 (allow)")

	spent, err := s.Spend(ctx, "t")
	require.NoError(t, err)
	assert.Equal(t, 0.0, spent, "404 → spent 0 (allow)")

	require.NoError(t, s.AddSpend(ctx, "t", 1), "404 → no-op")

	ok, err := s.AcquireSlot(ctx, "t", 1)
	require.NoError(t, err)
	assert.True(t, ok, "404 → grant (no cap applies)")

	require.NoError(t, s.ReleaseSlot(ctx, "t"), "404 → no-op")
}

// A non-2xx/404 (401 rejected, 503 auth-infra, 502 backend) is an error so preCall
// applies its fail policy (budget CLOSED, rate/concurrency OPEN).
func TestHTTPQuotaErrorsAreErrors(t *testing.T) {
	ctx := context.Background()
	for _, code := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusBadGateway} {
		s := newTestProxyStore(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		_, err := s.IncrRPM(ctx, "t", 1)
		assert.Error(t, err, "rpm status %d", code)
		_, err = s.Spend(ctx, "t")
		assert.Error(t, err, "spend status %d", code)
		require.Error(t, s.AddSpend(ctx, "t", 1), "addspend status %d", code)
		_, err = s.AcquireSlot(ctx, "t", 1)
		assert.Error(t, err, "slot status %d", code)
		require.Error(t, s.ReleaseSlot(ctx, "t"), "release status %d", code)
	}
}

// A proxy 401 maps to ErrQuotaProxyRejected (a DEFINITIVE rejection) — distinct
// from a transient 5xx — so preCall fails CLOSED (Amд 3).
func TestHTTPQuota401IsRejection(t *testing.T) {
	ctx := context.Background()
	s := newTestProxyStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := s.IncrRPM(ctx, "t", 1)
	assert.ErrorIs(t, err, ErrQuotaProxyRejected)
	_, err = s.Spend(ctx, "t")
	assert.ErrorIs(t, err, ErrQuotaProxyRejected)
	require.ErrorIs(t, s.AddSpend(ctx, "t", 1), ErrQuotaProxyRejected)
	_, err = s.AcquireSlot(ctx, "t", 1)
	assert.ErrorIs(t, err, ErrQuotaProxyRejected)
	require.ErrorIs(t, s.ReleaseSlot(ctx, "t"), ErrQuotaProxyRejected)

	// A transient 503 is NOT a rejection.
	s503 := newTestProxyStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err = s503.IncrRPM(ctx, "t", 1)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrQuotaProxyRejected, "a 503 must be transient, not a rejection")
}

// An empty/whitespace token file is a hard error (never a silent empty Bearer).
func TestHTTPQuotaEmptyTokenFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"count":1}`))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokPath, []byte("   \n"), 0o600))
	s := newHTTPTenantStore(srv.URL, tokPath)
	_, err := s.IncrRPM(context.Background(), "t", 1)
	assert.Error(t, err)
}

// A missing token file is an error (→ preCall fail policy), never a silent allow.
func TestHTTPQuotaMissingToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"count":1}`))
	}))
	t.Cleanup(srv.Close)
	s := newHTTPTenantStore(srv.URL, filepath.Join(t.TempDir(), "does-not-exist"))
	_, err := s.IncrRPM(context.Background(), "t", 1)
	assert.Error(t, err)
}
