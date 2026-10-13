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

	"github.com/ctxmesh/ctxmesh/internal/deliveryclaim"
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

// The dedup client presents the pod token and POSTs each claim step to /dedup. It sends NO
// namespace (the proxy scopes server-side).
func TestHTTPSeenSetRoundTrip(t *testing.T) {
	ctx := context.Background()
	var gotAuth, gotPath string
	var bodies []string
	answer := `{"firstSeen":true,"state":"claimed"}`
	s := newTestSeenProxy(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		_, _ = w.Write([]byte(answer))
	})

	st, err := s.Claim(ctx, "msg-1", "tok-1", 2*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, deliveryclaim.Claimed, st)
	assert.Equal(t, "Bearer POD-TOKEN", gotAuth)
	assert.Equal(t, "/dedup", gotPath)

	for state, want := range map[string]deliveryclaim.State{
		"inflight": deliveryclaim.InFlight,
		"done":     deliveryclaim.Done,
	} {
		answer = `{"firstSeen":false,"state":"` + state + `"}`
		st, err = s.Claim(ctx, "msg-1", "tok-2", 2*time.Minute)
		require.NoError(t, err)
		assert.Equal(t, want, st)
	}

	answer = `{"firstSeen":false}`
	require.NoError(t, s.Complete(ctx, "msg-1", 10*time.Minute))
	require.NoError(t, s.Release(ctx, "msg-1", "tok-1"))

	assert.JSONEq(t, `{"messageID":"msg-1","ttlSeconds":120,"op":"claim","token":"tok-1"}`, bodies[0])
	assert.JSONEq(t, `{"messageID":"msg-1","ttlSeconds":600,"op":"complete"}`, bodies[3])
	assert.JSONEq(t, `{"messageID":"msg-1","op":"release","token":"tok-1"}`, bodies[4])
}

// A proxy that predates claims answers a claim as a MarkSeen; a first sighting is processed
// and anything else is a duplicate, as before. A state the client does not know is an error.
func TestHTTPSeenSetAgainstAnOlderProxy(t *testing.T) {
	ctx := context.Background()
	answer := `{"firstSeen":true}`
	s := newTestSeenProxy(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(answer)) })

	st, err := s.Claim(ctx, "m", "t", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, deliveryclaim.Claimed, st)

	answer = `{"firstSeen":false}`
	st, err = s.Claim(ctx, "m", "t", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, deliveryclaim.Done, st)

	answer = `{"firstSeen":false,"state":"pending"}`
	_, err = s.Claim(ctx, "m", "t", time.Minute)
	assert.Error(t, err, "an unknown state fails closed")
}

// Any non-200 (auth rejected, proxy down, backend error) is an ERROR so the async
// consumer fails CLOSED (NACK — never double-process; M11).
func TestHTTPSeenSetErrorsFailClosed(t *testing.T) {
	ctx := context.Background()
	for _, code := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusBadGateway} {
		s := newTestSeenProxy(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		_, err := s.Claim(ctx, "m", "t", time.Minute)
		assert.Error(t, err, "status %d must be an error (fail closed)", code)
		assert.Error(t, s.Release(ctx, "m", "t"), "status %d", code)
		assert.Error(t, s.Complete(ctx, "m", time.Minute), "status %d", code)
	}
}

// A missing token file is an error (fail closed), never a silent first sighting.
func TestHTTPSeenSetMissingToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"firstSeen":true,"state":"claimed"}`))
	}))
	t.Cleanup(srv.Close)
	s := newHTTPSeenSet(srv.URL, filepath.Join(t.TempDir(), "nope"))
	_, err := s.Claim(context.Background(), "m", "t", time.Minute)
	assert.Error(t, err)
}
