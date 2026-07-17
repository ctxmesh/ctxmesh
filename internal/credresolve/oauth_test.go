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

package credresolve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNeedsRefresh(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	rfc := func(tm time.Time) string { return tm.UTC().Format(time.RFC3339) }

	assert.False(t, NeedsRefresh("", now), "no expiry ⇒ never refresh")
	assert.False(t, NeedsRefresh("garbage", now), "unparseable expiry ⇒ never refresh")
	assert.False(t, NeedsRefresh(rfc(now.Add(10*time.Minute)), now), "far from expiry ⇒ no refresh")
	assert.True(t, NeedsRefresh(rfc(now.Add(30*time.Second)), now), "within skew ⇒ refresh")
	assert.True(t, NeedsRefresh(rfc(now.Add(-time.Minute)), now), "already expired ⇒ refresh")
}

func TestSecretDataRoundTrip(t *testing.T) {
	exp := time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC)
	data := SecretData(
		OAuthConfig{TokenEndpoint: "https://as.example/token", ClientID: "cid", RevocationEndpoint: "https://as.example/revoke"},
		Tokens{AccessToken: "AT", RefreshToken: "RT", ExpiresAt: exp},
	)
	assert.Equal(t, "AT", string(data[KeyAccessToken]))
	assert.Equal(t, "RT", string(data[KeyRefreshToken]))
	assert.Equal(t, "https://as.example/token", string(data[KeyTokenEndpoint]))
	assert.Equal(t, "cid", string(data[KeyClientID]))
	assert.Equal(t, "https://as.example/revoke", string(data[KeyRevocationEndpoint]))
	assert.Equal(t, exp.Format(time.RFC3339), string(data[KeyExpiry]))
	// NeedsRefresh reads back exactly what SecretData wrote.
	assert.True(t, NeedsRefresh(string(data[KeyExpiry]), exp))

	t.Run("omits absent optional fields", func(t *testing.T) {
		d := SecretData(OAuthConfig{TokenEndpoint: "e", ClientID: "c"}, Tokens{AccessToken: "AT"})
		_, hasRefresh := d[KeyRefreshToken]
		_, hasExpiry := d[KeyExpiry]
		_, hasRevoke := d[KeyRevocationEndpoint]
		assert.False(t, hasRefresh)
		assert.False(t, hasExpiry)
		assert.False(t, hasRevoke)
	})
}

// tokenServer stands in for an OAuth token endpoint; handler decides the response.
func tokenServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPTokenExchangerRefresh(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	t.Run("success rotates tokens and computes expiry", func(t *testing.T) {
		var gotGrant, gotRefresh, gotClient string
		srv := tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, r.ParseForm())
			gotGrant = r.Form.Get("grant_type")
			gotRefresh = r.Form.Get("refresh_token")
			gotClient = r.Form.Get("client_id")
			_, _ = w.Write([]byte(`{"access_token":"NEW-AT","refresh_token":"NEW-RT","expires_in":3600}`))
		})
		x := &HTTPTokenExchanger{Now: func() time.Time { return now }}
		toks, err := x.Refresh(ctx, srv.URL, "cid", "old-RT")
		require.NoError(t, err)
		assert.Equal(t, "refresh_token", gotGrant)
		assert.Equal(t, "old-RT", gotRefresh)
		assert.Equal(t, "cid", gotClient)
		assert.Equal(t, "NEW-AT", toks.AccessToken)
		assert.Equal(t, "NEW-RT", toks.RefreshToken)
		assert.Equal(t, now.Add(time.Hour), toks.ExpiresAt)
	})

	t.Run("oauth error surfaces as a sanitized code, never the description", func(t *testing.T) {
		srv := tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"secret leaked here"}`))
		})
		x := &HTTPTokenExchanger{}
		_, err := x.Refresh(ctx, srv.URL, "cid", "old-RT")
		require.Error(t, err)
		var te *TokenError
		require.ErrorAs(t, err, &te)
		assert.Equal(t, TokenErrOAuth, te.Kind)
		assert.Equal(t, "invalid_grant", te.Code)
		assert.NotContains(t, err.Error(), "secret leaked here")
	})

	t.Run("no access token is a bad-response fault", func(t *testing.T) {
		srv := tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"token_type":"bearer"}`))
		})
		x := &HTTPTokenExchanger{}
		_, err := x.Refresh(ctx, srv.URL, "cid", "old-RT")
		var te *TokenError
		require.ErrorAs(t, err, &te)
		assert.Equal(t, TokenErrBadResponse, te.Kind)
	})

	t.Run("transport failure is a transport fault", func(t *testing.T) {
		x := &HTTPTokenExchanger{Client: &http.Client{Timeout: 50 * time.Millisecond}}
		// A port that nothing listens on → dial failure.
		_, err := x.Refresh(ctx, "http://127.0.0.1:1/token", "cid", "old-RT")
		var te *TokenError
		require.ErrorAs(t, err, &te)
		assert.Equal(t, TokenErrTransport, te.Kind)
	})
}

func TestHTTPTokenExchangerRevoke(t *testing.T) {
	ctx := context.Background()

	t.Run("posts token per RFC 7009 and treats 200 as success", func(t *testing.T) {
		var gotToken string
		srv := tokenServer(t, func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, r.ParseForm())
			gotToken = r.Form.Get("token")
			w.WriteHeader(http.StatusOK)
		})
		x := &HTTPTokenExchanger{}
		require.NoError(t, x.Revoke(ctx, srv.URL, "the-refresh-token"))
		assert.Equal(t, "the-refresh-token", gotToken)
	})

	t.Run("empty endpoint or token is a no-op", func(t *testing.T) {
		x := &HTTPTokenExchanger{}
		assert.NoError(t, x.Revoke(ctx, "", "tok"))
		assert.NoError(t, x.Revoke(ctx, "https://as/revoke", ""))
	})
}

func TestPostTokenEndpointBadURL(t *testing.T) {
	_, te := PostTokenEndpoint(context.Background(), http.DefaultClient, "://not a url", url.Values{}, nil)
	require.NotNil(t, te)
	assert.Equal(t, TokenErrBadRequest, te.Kind)
}
