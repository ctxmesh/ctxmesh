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

package bff

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
)

func TestResolveEndUserIdentity(t *testing.T) {
	ctx := context.Background()
	newServer := func(store namespacetenant.Store, consoleIssuer string) *Server {
		return &Server{namespaceTenantStore: store, oidcIssuer: consoleIssuer, log: logr.Discard()}
	}

	t.Run("enabled+complete config resolves", func(t *testing.T) {
		st := namespacetenant.NewMemStore()
		require.NoError(t, st.SetMembers(ctx, "t1", []string{"ns1"}))
		require.NoError(t, st.SetEndUserIdentity(ctx, "t1", namespacetenant.EndUserIdentity{
			Enabled: true, Issuer: "https://dex-eu.example.com", ClientID: "cid",
		}))
		got, err := newServer(st, "https://console-dex.example.com").resolveEndUserIdentity(ctx, "ns1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "https://dex-eu.example.com", got.Issuer)
		assert.Equal(t, "cid", got.ClientID)
	})

	t.Run("disabled config → nil", func(t *testing.T) {
		st := namespacetenant.NewMemStore()
		require.NoError(t, st.SetMembers(ctx, "t1", []string{"ns1"}))
		require.NoError(t, st.SetEndUserIdentity(ctx, "t1", namespacetenant.EndUserIdentity{
			Enabled: false, Issuer: "https://x", ClientID: "c",
		}))
		got, err := newServer(st, "").resolveEndUserIdentity(ctx, "ns1")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("no tenant for ns → nil (fail-closed)", func(t *testing.T) {
		got, err := newServer(namespacetenant.NewMemStore(), "").resolveEndUserIdentity(ctx, "orphan-ns")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("incomplete config (no clientId) → nil", func(t *testing.T) {
		st := namespacetenant.NewMemStore()
		require.NoError(t, st.SetMembers(ctx, "t1", []string{"ns1"}))
		require.NoError(t, st.SetEndUserIdentity(ctx, "t1", namespacetenant.EndUserIdentity{
			Enabled: true, Issuer: "https://x",
		}))
		got, err := newServer(st, "").resolveEndUserIdentity(ctx, "ns1")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("issuer == console issuer → REFUSED (structural K8s-trust guard)", func(t *testing.T) {
		st := namespacetenant.NewMemStore()
		require.NoError(t, st.SetMembers(ctx, "t1", []string{"ns1"}))
		// Trailing slash + case variance must still be caught.
		require.NoError(t, st.SetEndUserIdentity(ctx, "t1", namespacetenant.EndUserIdentity{
			Enabled: true, Issuer: "https://Console-Dex.example.com/", ClientID: "cid",
		}))
		got, err := newServer(st, "https://console-dex.example.com").resolveEndUserIdentity(ctx, "ns1")
		require.NoError(t, err)
		assert.Nil(t, got, "an end-user issuer equal to the console issuer must be refused")
	})

	t.Run("nil store → nil, no error", func(t *testing.T) {
		got, err := (&Server{log: logr.Discard()}).resolveEndUserIdentity(ctx, "ns1")
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

func TestHandleEndUserAuthConfig(t *testing.T) {
	ctx := context.Background()
	st := namespacetenant.NewMemStore()
	require.NoError(t, st.SetMembers(ctx, "t1", []string{"ns1"}))
	require.NoError(t, st.SetEndUserIdentity(ctx, "t1", namespacetenant.EndUserIdentity{
		Enabled: true, Issuer: "https://dex-eu.example.com", ClientID: "cid", Scopes: []string{"email"},
	}))
	s := &Server{namespaceTenantStore: st, log: logr.Discard()}

	// Host-derived ns with an enabled IdP → 200 + issuer/clientId (no secret).
	req := httptest.NewRequest(http.MethodGet, "/api/end-user-auth-config", nil)
	req.Header.Set("X-Forwarded-Host", "chatbot.ns1.example.com")
	rec := httptest.NewRecorder()
	s.handleEndUserAuthConfig(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "https://dex-eu.example.com")
	assert.Contains(t, rec.Body.String(), "cid")

	// A ns with no end-user IdP → uniform 404 (no oracle).
	req2 := httptest.NewRequest(http.MethodGet, "/api/end-user-auth-config", nil)
	req2.Header.Set("X-Forwarded-Host", "x.other-ns.example.com")
	rec2 := httptest.NewRecorder()
	s.handleEndUserAuthConfig(rec2, req2)
	assert.Equal(t, http.StatusNotFound, rec2.Code)
}
