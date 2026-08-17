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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSpawnStore(t *testing.T, handler http.HandlerFunc) *httpSpawnStore {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	tokPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokPath, []byte("POD-TOKEN\n"), 0o600))
	return newHTTPSpawnStore(srv.URL, tokPath)
}

// The store authenticates with the pod token, hits /spawn/acquire with the right body, and decodes acquired.
func TestHTTPSpawnStore_AcquireRoundTrip(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	s := newTestSpawnStore(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]bool{"acquired": true})
	})

	ok, err := s.AcquireInflight(context.Background(), "reg", "root-1", 4)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "/spawn/acquire", gotPath)
	assert.Equal(t, "Bearer POD-TOKEN", gotAuth, "the pod token is presented")
	assert.JSONEq(t, `{"scope":"reg","rootRunId":"root-1","counter":"inflight","max":4}`, gotBody)

	// AcquireTotal maps to counter:"count".
	_, _ = s.AcquireTotal(context.Background(), "reg", "root-1", 20)
	assert.JSONEq(t, `{"scope":"reg","rootRunId":"root-1","counter":"count","max":20}`, gotBody)
}

func TestHTTPSpawnStore_Release(t *testing.T) {
	var gotPath, gotBody string
	s := newTestSpawnStore(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	})
	require.NoError(t, s.ReleaseInflight(context.Background(), "reg", "root-1"))
	assert.Equal(t, "/spawn/release", gotPath)
	assert.JSONEq(t, `{"scope":"reg","rootRunId":"root-1","counter":"inflight"}`, gotBody)
}

// A proxy error → (false, err): the guard maps a store error to SpawnDeniedError, so a spawn is DENIED when
// the proxy is unreachable/erroring (fail-closed, never silently admitted).
func TestHTTPSpawnStore_FailsClosedOnProxyError(t *testing.T) {
	s := newTestSpawnStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ok, err := s.AcquireInflight(context.Background(), "reg", "root-1", 4)
	require.Error(t, err, "a proxy error must surface so the guard fails closed")
	assert.False(t, ok)
}

// The delegate selects the proxy spawn store when STATELAYER_PROXY_URL is set, else the direct-Valkey store.
func TestDelegateSpawnStoreSelection(t *testing.T) {
	viaProxy := delegateRuntime{Enabled: true, ProxyURL: "http://statelayer:8080", TokenPath: "/t"}
	_, isHTTP := viaProxy.spawnStore().(*httpSpawnStore)
	assert.True(t, isHTTP, "STATELAYER_PROXY_URL set → the HTTP proxy spawn store")

	direct := delegateRuntime{Enabled: true, QuotaAddr: "valkey:6379"}
	_, isRedis := direct.spawnStore().(*redisSpawnStore)
	assert.True(t, isRedis, "no proxy URL → the direct-Valkey spawn store")
}
