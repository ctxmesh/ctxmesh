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

func newDedupProxy(t *testing.T, byToken map[string]string, auth PodAuthenticator) (*Server, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	if auth == nil {
		auth = fakePodAuth{byToken: byToken}
	}
	s, err := NewServer(Options{
		Store:            NewRedisStore(mr.Addr(), "", ""),
		DedupStore:       NewRedisDedupStore(mr.Addr(), "", ""),
		PodAuthenticator: auth,
	})
	require.NoError(t, err)
	return s, mr
}

// First sighting of a messageID is firstSeen=true; a repeat within the window is a
// duplicate (false). The Valkey key is namespace-scoped from the token.
func TestDedupFirstSeenThenDuplicate(t *testing.T) {
	s, mr := newDedupProxy(t, map[string]string{"tok": "team-alpha-ns"}, nil)

	rec := do(t, s, "POST", "/dedup", "tok", `{"messageID":"m-1","ttlSeconds":600}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"firstSeen":true}`, rec.Body.String())

	rec = do(t, s, "POST", "/dedup", "tok", `{"messageID":"m-1","ttlSeconds":600}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"firstSeen":false}`, rec.Body.String(), "a repeat within the window is a duplicate")

	require.Contains(t, mr.Keys(), "a2a:seen:team-alpha-ns:m-1", "the key is namespace-scoped from the token")
}

// THE HEADLINE: the seen-set is namespace-scoped, so one namespace cannot poison
// another's dedup by pre-seeding a messageID — the same messageID is a first
// sighting for each namespace.
func TestDedupCrossNamespaceIsolation(t *testing.T) {
	s, _ := newDedupProxy(t, map[string]string{"alpha": "team-alpha-ns", "beta": "team-beta-ns"}, nil)

	// alpha marks m-shared seen.
	require.Equal(t, http.StatusOK, do(t, s, "POST", "/dedup", "alpha", `{"messageID":"m-shared"}`, nil).Code)

	// beta sees m-shared as a FIRST sighting (its own scope), not a duplicate.
	rec := do(t, s, "POST", "/dedup", "beta", `{"messageID":"m-shared"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"firstSeen":true}`, rec.Body.String(), "beta must NOT inherit alpha's dedup")
}

func TestDedupAuthAndValidation(t *testing.T) {
	t.Run("rejected token → 401", func(t *testing.T) {
		s, _ := newDedupProxy(t, map[string]string{"good": "ns"}, nil)
		assert.Equal(t, http.StatusUnauthorized, do(t, s, "POST", "/dedup", "bad", `{"messageID":"m"}`, nil).Code)
	})
	t.Run("auth-infra error → 503", func(t *testing.T) {
		s, _ := newDedupProxy(t, nil, fakePodAuth{err: errors.New("tokenreview down")})
		assert.Equal(t, http.StatusServiceUnavailable, do(t, s, "POST", "/dedup", "x", `{"messageID":"m"}`, nil).Code)
	})
	t.Run("missing messageID → 400", func(t *testing.T) {
		s, _ := newDedupProxy(t, map[string]string{"t": "ns"}, nil)
		assert.Equal(t, http.StatusBadRequest, do(t, s, "POST", "/dedup", "t", `{"ttlSeconds":60}`, nil).Code)
	})
	t.Run("no dedup store configured → 503", func(t *testing.T) {
		mr := miniredis.RunT(t)
		s, err := NewServer(Options{Store: NewRedisStore(mr.Addr(), "", ""), PodAuthenticator: fakePodAuth{byToken: map[string]string{"t": "ns"}}})
		require.NoError(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, do(t, s, "POST", "/dedup", "t", `{"messageID":"m"}`, nil).Code)
	})
}

// A Valkey backend failure surfaces as 502 (the launcher's dedup client fails CLOSED).
func TestDedupBackendError(t *testing.T) {
	s, mr := newDedupProxy(t, map[string]string{"t": "ns"}, nil)
	mr.Close()
	assert.Equal(t, http.StatusBadGateway, do(t, s, "POST", "/dedup", "t", `{"messageID":"m"}`, nil).Code)
}
