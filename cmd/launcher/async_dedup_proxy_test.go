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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSeenProxy(t *testing.T, handler http.HandlerFunc) *httpSeenSet {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokPath, []byte("POD-TOKEN\n"), 0o600))
	return newHTTPSeenSet(srv.URL, tokPath)
}

// The dedup client presents the pod token, POSTs {messageID, ttlSeconds} to /dedup,
// and parses firstSeen. It sends NO namespace (the proxy scopes server-side).
func TestHTTPSeenSetRoundTrip(t *testing.T) {
	ctx := context.Background()
	var gotAuth, gotPath, gotBody string
	firstSeen := true
	s := newTestSeenProxy(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if firstSeen {
			_, _ = w.Write([]byte(`{"firstSeen":true}`))
		} else {
			_, _ = w.Write([]byte(`{"firstSeen":false}`))
		}
	})

	ok, err := s.MarkSeen(ctx, "msg-1", 10*time.Minute)
	require.NoError(t, err)
	assert.True(t, ok, "first sighting")
	assert.Equal(t, "Bearer POD-TOKEN", gotAuth)
	assert.Equal(t, "/dedup", gotPath)
	assert.JSONEq(t, `{"messageID":"msg-1","ttlSeconds":600}`, gotBody)

	firstSeen = false
	ok, err = s.MarkSeen(ctx, "msg-1", 10*time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "a duplicate is not first-seen")
}

// Any non-200 (auth rejected, proxy down, backend error) is an ERROR so the async
// consumer fails CLOSED (NACK — never double-process; M11).
func TestHTTPSeenSetErrorsFailClosed(t *testing.T) {
	ctx := context.Background()
	for _, code := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusBadGateway} {
		s := newTestSeenProxy(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		_, err := s.MarkSeen(ctx, "m", time.Minute)
		assert.Error(t, err, "status %d must be an error (fail closed)", code)
	}
}

// A missing token file is an error (fail closed), never a silent first-seen.
func TestHTTPSeenSetMissingToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"firstSeen":true}`))
	}))
	t.Cleanup(srv.Close)
	s := newHTTPSeenSet(srv.URL, filepath.Join(t.TempDir(), "nope"))
	_, err := s.MarkSeen(context.Background(), "m", time.Minute)
	assert.Error(t, err)
}
