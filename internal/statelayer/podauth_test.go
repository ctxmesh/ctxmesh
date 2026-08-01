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
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// fakeReviewer is an injectable tokenReviewer; it counts calls so cache behaviour
// is observable.
type fakeReviewer struct {
	calls         int
	authenticated bool
	username      string
	err           error
}

func (f *fakeReviewer) review(_ context.Context, _ string, _ []string) (bool, string, error) {
	f.calls++
	return f.authenticated, f.username, f.err
}

func jwtWithExp(exp int64) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, exp))
	return hdr + "." + payload + ".sig"
}

func TestPodAuthNamespace(t *testing.T) {
	ctx := context.Background()

	t.Run("authenticated SA token → namespace", func(t *testing.T) {
		r := &fakeReviewer{authenticated: true, username: "system:serviceaccount:team-alpha:launcher"}
		a := newTokenReviewAuthenticator(r, "statelayer-proxy", nil)
		ns, err := a.Namespace(ctx, jwtWithExp(time.Now().Add(time.Hour).Unix()))
		require.NoError(t, err)
		assert.Equal(t, "team-alpha", ns)
	})

	t.Run("not authenticated → rejected (401)", func(t *testing.T) {
		r := &fakeReviewer{authenticated: false}
		a := newTokenReviewAuthenticator(r, "statelayer-proxy", nil)
		_, err := a.Namespace(ctx, jwtWithExp(time.Now().Add(time.Hour).Unix()))
		assert.ErrorIs(t, err, ErrTokenRejected)
	})

	t.Run("reviewer infra error → NOT a rejection (503, not 401)", func(t *testing.T) {
		r := &fakeReviewer{err: errors.New("apiserver unreachable")}
		a := newTokenReviewAuthenticator(r, "statelayer-proxy", nil)
		_, err := a.Namespace(ctx, jwtWithExp(time.Now().Add(time.Hour).Unix()))
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrTokenRejected, "an infra error must map to 503, never 401")
	})

	t.Run("authenticated but not a ServiceAccount → rejected", func(t *testing.T) {
		r := &fakeReviewer{authenticated: true, username: "alice@example.com"}
		a := newTokenReviewAuthenticator(r, "statelayer-proxy", nil)
		_, err := a.Namespace(ctx, jwtWithExp(time.Now().Add(time.Hour).Unix()))
		assert.ErrorIs(t, err, ErrTokenRejected)
	})

	t.Run("empty token → rejected without calling the reviewer", func(t *testing.T) {
		r := &fakeReviewer{authenticated: true, username: "system:serviceaccount:x:y"}
		a := newTokenReviewAuthenticator(r, "statelayer-proxy", nil)
		_, err := a.Namespace(ctx, "  ")
		assert.ErrorIs(t, err, ErrTokenRejected)
		assert.Zero(t, r.calls, "no reviewer call for an empty token")
	})
}

// A verified token is cached on its raw string until its exp; a repeat call is a
// cache hit (no second review), and a call past exp re-reviews.
func TestPodAuthCache(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	clock := func() time.Time { return now }
	r := &fakeReviewer{authenticated: true, username: "system:serviceaccount:team-alpha:launcher"}
	a := newTokenReviewAuthenticator(r, "statelayer-proxy", clock)

	tok := jwtWithExp(now.Add(10 * time.Minute).Unix())

	ns, err := a.Namespace(ctx, tok)
	require.NoError(t, err)
	assert.Equal(t, "team-alpha", ns)
	require.Equal(t, 1, r.calls)

	// Repeat within the token's life → cache hit, no new review.
	_, err = a.Namespace(ctx, tok)
	require.NoError(t, err)
	assert.Equal(t, 1, r.calls, "second call within TTL must hit the cache")

	// Advance past exp → the entry is stale → re-review.
	now = now.Add(11 * time.Minute)
	_, err = a.Namespace(ctx, tok)
	require.NoError(t, err)
	assert.Equal(t, 2, r.calls, "a call past exp must re-review")
}

// authWithTokenReviewStatus builds a real clientTokenReviewer over a fake clientset
// whose TokenReview always returns the given status — so the Status.Error /
// Status.Audiences handling (the 401-vs-503 boundary + audience-awareness) is
// exercised, which the fakeReviewer above bypasses.
func authWithTokenReviewStatus(status authv1.TokenReviewStatus) PodAuthenticator {
	cs := k8sfake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		tr := action.(clienttesting.CreateAction).GetObject().(*authv1.TokenReview)
		tr.Status = status
		return true, tr, nil
	})
	return NewTokenReviewAuthenticator(cs, "statelayer-proxy", nil)
}

// The real clientTokenReviewer must map an audience-UNAWARE apiserver (empty
// status.audiences on authenticated=true) to a 503 (fail closed), a
// wrong-audience/invalid token to a 401, and a matching audience to success.
//
// IMPORTANT (verified against the real apiserver in the envtest): a rejection —
// invalid token, wrong audience, expired — comes back as authenticated=FALSE with
// the reason in Status.Error. So Status.Error is NOT an infra signal; an
// authenticated=false response is always a 401 rejection regardless of Status.Error.
// The only infra (503) signal is the TokenReview CALL failing (err != nil).
func TestClientTokenReviewerStatusHandling(t *testing.T) {
	ctx := context.Background()
	goodTok := jwtWithExp(time.Now().Add(time.Hour).Unix())
	saUser := authv1.UserInfo{Username: "system:serviceaccount:team-x:launcher"}

	t.Run("authenticated=false WITH Status.Error (the apiserver's rejection reason) → 401", func(t *testing.T) {
		a := authWithTokenReviewStatus(authv1.TokenReviewStatus{Authenticated: false, Error: "invalid bearer token"})
		_, err := a.Namespace(ctx, goodTok)
		assert.ErrorIs(t, err, ErrTokenRejected, "a rejection reason in Status.Error is still a 401, not 503")
	})

	t.Run("the TokenReview CALL fails → infra 503, not rejection", func(t *testing.T) {
		cs := k8sfake.NewSimpleClientset()
		cs.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver unreachable")
		})
		a := NewTokenReviewAuthenticator(cs, "statelayer-proxy", nil)
		_, err := a.Namespace(ctx, goodTok)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrTokenRejected, "a failed review CALL is the genuine 503 signal")
	})

	t.Run("audience-unaware apiserver (empty status.audiences) → fail closed (503)", func(t *testing.T) {
		a := authWithTokenReviewStatus(authv1.TokenReviewStatus{Authenticated: true, User: saUser, Audiences: nil})
		_, err := a.Namespace(ctx, goodTok)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrTokenRejected, "empty audiences must fail closed as infra, not accept")
	})

	t.Run("wrong audience → rejected (401)", func(t *testing.T) {
		a := authWithTokenReviewStatus(authv1.TokenReviewStatus{
			Authenticated: true, User: saUser, Audiences: []string{"some-other-audience"},
		})
		_, err := a.Namespace(ctx, goodTok)
		assert.ErrorIs(t, err, ErrTokenRejected)
	})

	t.Run("matching audience → namespace", func(t *testing.T) {
		a := authWithTokenReviewStatus(authv1.TokenReviewStatus{
			Authenticated: true, User: saUser, Audiences: []string{"statelayer-proxy"},
		})
		ns, err := a.Namespace(ctx, goodTok)
		require.NoError(t, err)
		assert.Equal(t, "team-x", ns)
	})

	t.Run("clean unauthenticated (no error) → rejected (401)", func(t *testing.T) {
		a := authWithTokenReviewStatus(authv1.TokenReviewStatus{Authenticated: false})
		_, err := a.Namespace(ctx, goodTok)
		assert.ErrorIs(t, err, ErrTokenRejected)
	})
}

func TestNamespaceFromSAUsername(t *testing.T) {
	cases := []struct {
		username string
		wantNS   string
		wantOK   bool
	}{
		{"system:serviceaccount:team-alpha:launcher", "team-alpha", true},
		{"system:serviceaccount:kube-system:default", "kube-system", true},
		{"alice@example.com", "", false},
		{"system:serviceaccount:", "", false},
		{"system:serviceaccount::sa", "", false}, // empty namespace (double colon) → rejected
		{"system:serviceaccount:only-ns", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		ns, ok := namespaceFromSAUsername(c.username)
		assert.Equal(t, c.wantOK, ok, c.username)
		assert.Equal(t, c.wantNS, ns, c.username)
	}
}

func TestTokenExpiry(t *testing.T) {
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	got, ok := tokenExpiry(jwtWithExp(exp.Unix()))
	require.True(t, ok)
	assert.True(t, got.Equal(exp))

	_, ok = tokenExpiry("not-a-jwt")
	assert.False(t, ok)
	_, ok = tokenExpiry("a.b") // wrong part count
	assert.False(t, ok)
}

// Server.authenticatePod threads the authenticator; nil ⇒ errPodAuthUnavailable
// (distinct from a rejection).
func TestServerAuthenticatePod(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	store := NewRedisStore(mr.Addr(), "", "")

	r := &fakeReviewer{authenticated: true, username: "system:serviceaccount:team-beta:launcher"}
	withAuth, err := NewServer(Options{Store: store, PodAuthenticator: newTokenReviewAuthenticator(r, "statelayer-proxy", nil)})
	require.NoError(t, err)
	ns, err := withAuth.authenticatePod(ctx, jwtWithExp(time.Now().Add(time.Hour).Unix()))
	require.NoError(t, err)
	assert.Equal(t, "team-beta", ns)

	noAuth, err := NewServer(Options{Store: store})
	require.NoError(t, err)
	_, err = noAuth.authenticatePod(ctx, "tok")
	assert.ErrorIs(t, err, errPodAuthUnavailable)
}
