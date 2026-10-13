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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envFrom(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// Without a token service the BFF writes grants itself, and needs no token.
func TestTokenServiceWiring_NoTokenServiceNeedsNoToken(t *testing.T) {
	client, store, err := tokenServiceWiring(logr.Discard(), envFrom(nil))
	require.NoError(t, err)
	assert.Nil(t, client)
	assert.Nil(t, store)
}

// Pointed at the token service, the BFF refuses to start without a readable token, naming the fix
// (ADR 0154): every grant write would be refused.
func TestTokenServiceWiring_RefusesToStartWithoutItsToken(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.WriteFile(empty, []byte(" \n"), 0o600))
	for name, path := range map[string]string{
		"unset":   "",
		"missing": filepath.Join(t.TempDir(), "absent"),
		"empty":   empty,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := tokenServiceWiring(logr.Discard(), envFrom(map[string]string{
				"TOKEN_SERVICE_URL": "http://token-service", "TOKEN_SERVICE_TOKEN_PATH": path,
			}))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "TOKEN_SERVICE_TOKEN_PATH")
			assert.Contains(t, err.Error(), "ctxmesh-token-service", "the error names the audience to mount")
			if path == "" {
				assert.Contains(t, err.Error(), "or unset TOKEN_SERVICE_URL", "and the other way out")
			}
		})
	}
}

// With its token the BFF sends it on every call to the token service.
func TestTokenServiceWiring_SendsItsToken(t *testing.T) {
	var (
		mu  sync.Mutex
		got string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(srv.Close)
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("bff-token\n"), 0o600))

	client, store, err := tokenServiceWiring(logr.Discard(), envFrom(map[string]string{
		"TOKEN_SERVICE_URL": srv.URL, "TOKEN_SERVICE_TOKEN_PATH": token,
	}))
	require.NoError(t, err)
	require.NotNil(t, store)
	resp, err := client.Post(srv.URL+"/v1/knowledge/search", "application/json", nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Bearer bff-token", got)
}
