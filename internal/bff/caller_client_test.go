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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// fakeCallerClientFactory is the test double for CallerClientFactory. It records
// the exact bearer token it was handed (so a test can assert the caller's token —
// not the BFF SA — is what reached the client) and hands back a preset fake
// client.Client instead of building a real one, so handler tests prove the TOKEN
// FLOW without a real API server.
//
// By default it is permissive (returns the client regardless of token) so
// handler-focused tests can exercise the CRD routes without a header. The
// dedicated token-gating tests set requireToken=true to prove the production
// contract: an empty token → errUnauthenticated (401) before any K8s call.
type fakeCallerClientFactory struct {
	// client is returned as the caller-scoped client stand-in.
	client client.Client
	// gotToken records the last token ForRequest extracted (assert token flow).
	gotToken string
	// requireToken, when true, makes ForRequest reject an empty token with
	// errUnauthenticated — mirroring the production factory's 401-before-K8s gate.
	requireToken bool
}

func (f *fakeCallerClientFactory) ForRequest(r *http.Request) (client.Client, error) {
	token := bearerToken(r)
	f.gotToken = token
	if f.requireToken && token == "" {
		return nil, errUnauthenticated
	}
	return f.client, nil
}

// newFakeFactory wraps a client.Client as a permissive caller-client factory for
// handler-focused tests (token presence is exercised separately).
func newFakeFactory(c client.Client) *fakeCallerClientFactory {
	return &fakeCallerClientFactory{client: c}
}

// interceptorCreateFlag flips *called when the fake client's Create runs — used
// to prove a K8s create did (or did NOT) happen.
func interceptorCreateFlag(called *bool) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			*called = true
			return c.Create(ctx, obj, opts...)
		},
	}
}

// interceptorListFlag flips *called when the fake client's List runs.
func interceptorListFlag(called *bool) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			*called = true
			return c.List(ctx, list, opts...)
		},
	}
}

// --- bearerToken parsing ----------------------------------------------------

func TestBearerTokenParsing(t *testing.T) {
	cases := []struct {
		name, header, want string
	}{
		{"present", "Bearer abc123", "abc123"},
		{"trimmed", "Bearer   spaced  ", "spaced"},
		{"empty-after-prefix", "Bearer ", ""},
		{"no-prefix", "abc123", ""},
		{"wrong-scheme", "Basic abc123", ""},
		{"absent", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			assert.Equal(t, tc.want, bearerToken(r))
		})
	}
}

// --- rest.Config credential swap (the confused-deputy fix) ------------------

// TestForTokenSwapsCredentialOnly proves the per-request config carries the
// CALLER'S token, CLEARS BearerTokenFile (else client-go re-reads the SA token —
// the silent confused-deputy regression), and never mutates the shared base.
func TestForTokenSwapsCredentialOnly(t *testing.T) {
	base := &rest.Config{
		Host:            "https://api.example:6443",
		BearerToken:     "bff-service-account-token",
		BearerTokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
	}
	base.CAData = []byte("cluster-ca")

	var captured *rest.Config
	f := &restCallerClientFactory{
		base:   base,
		scheme: runtime.NewScheme(),
		newClient: func(cfg *rest.Config, _ client.Options) (client.Client, error) {
			captured = cfg
			return nil, nil
		},
	}

	_, err := f.forToken("caller-viewer-token")
	require.NoError(t, err)
	require.NotNil(t, captured)

	// The caller's token is what flows into the per-request config.
	assert.Equal(t, "caller-viewer-token", captured.BearerToken)
	// BearerTokenFile is cleared so client-go cannot fall back to the SA token.
	assert.Empty(t, captured.BearerTokenFile, "BearerTokenFile must be cleared")
	// Host + CA are inherited from the in-cluster config.
	assert.Equal(t, "https://api.example:6443", captured.Host)
	assert.Equal(t, []byte("cluster-ca"), captured.CAData)

	// The shared base config is untouched (no cross-request leakage).
	assert.Equal(t, "bff-service-account-token", base.BearerToken)
	assert.Equal(t, "/var/run/secrets/kubernetes.io/serviceaccount/token", base.BearerTokenFile)
}

// TestForRequestRejectsMissingToken proves ForRequest returns errUnauthenticated
// (→ 401) BEFORE building any client for an anonymous / empty-token request.
func TestForRequestRejectsMissingToken(t *testing.T) {
	built := false
	f := &restCallerClientFactory{
		base:   &rest.Config{Host: "https://api.example"},
		scheme: runtime.NewScheme(),
		newClient: func(*rest.Config, client.Options) (client.Client, error) {
			built = true
			return nil, nil
		},
	}

	// No Authorization header.
	_, err := f.ForRequest(httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	require.ErrorIs(t, err, errUnauthenticated)

	// Empty bearer.
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer ")
	_, err = f.ForRequest(req)
	require.ErrorIs(t, err, errUnauthenticated)

	assert.False(t, built, "no client must be built for a missing/empty token")
}

// newCallerServer builds a Server whose CRD routes run through the fake caller
// factory (backed by the given fake client). auth is AllowAll so tests drive the
// token flow through the factory, not the edge authenticator.
func newCallerServer(t *testing.T, factory CallerClientFactory) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients: factory,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Expand: NewExpandAdapter()},
		Version:       "test",
		Log:           logr.Discard(),
	})
}

// --- token flow through the handlers (the confused-deputy fix, end to end) ---

// TestCreateRoutesCallerTokenToClient proves the CALLER'S bearer token — not the
// BFF SA — is what the factory is asked to scope the write by, for POST /api/agents.
func TestCreateRoutesCallerTokenToClient(t *testing.T) {
	factory := &fakeCallerClientFactory{
		client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
	}
	s := newCallerServer(t, factory)

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: sampleAgentYAML, Namespace: "prod"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "developer-persona-token", factory.gotToken,
		"the caller's token must be what the factory scopes the create by")
}

// TestListRoutesCallerTokenToClient proves GET /api/agents reads through the
// caller-scoped client (the caller's token reaches the factory).
func TestListRoutesCallerTokenToClient(t *testing.T) {
	factory := &fakeCallerClientFactory{
		client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
	}
	s := newCallerServer(t, factory)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "viewer-persona-token", factory.gotToken)
}

// TestCreateWithoutTokenIs401ThroughFactory proves that when the edge auth is
// permissive but the request carries NO bearer token, the caller-client factory
// rejects it with 401 BEFORE any K8s create — an anonymous request never reaches
// the cluster. It also proves the create client is never invoked (Create is never
// called on the fake).
func TestCreateWithoutTokenIs401ThroughFactory(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).
		Build()
	// AllowAll edge auth so the request reaches the handler; the FACTORY is what
	// enforces the token requirement (no Authorization header → 401).
	factory := &fakeCallerClientFactory{client: c, requireToken: true}
	s := newCallerServer(t, factory)

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: sampleAgentYAML, Namespace: "prod"})
	rec := httptest.NewRecorder()
	// No Authorization header at all.
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, factory.gotToken)
	assert.False(t, createCalled, "no K8s create must run for a token-less request")
}

// TestListWithoutTokenIs401ThroughFactory proves the read path is likewise gated
// by the factory: a token-less GET /api/agents → 401 with no List issued.
func TestListWithoutTokenIs401ThroughFactory(t *testing.T) {
	listCalled := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorListFlag(&listCalled)).
		Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, listCalled, "no K8s list must run for a token-less request")
}
