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
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// theTestKey is the secret the connect tests paste. It is DELIBERATELY a
// recognizable literal so a leak scan (response body + log buffer) can prove it
// appears NOWHERE it must not (the ADR 0015 crux).
const theTestKey = "sk-super-secret-KEY-do-not-leak-1234567890"

// fakeProvider is an httptest server standing in for anthropic/openai. It returns
// a model list on GET /v1/models when the presented key matches theTestKey, else
// a 401 — so the connect tests exercise both the good-key and bad-key paths
// without a real provider. It reads the auth header the probe sent (proving the
// key flowed on the wire server-side only).
func fakeProvider(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		// Accept either the OpenAI Bearer scheme or the Anthropic x-api-key scheme.
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("x-api-key")
		}
		if got != theTestKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		out := struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}{}
		for _, m := range models {
			out.Data = append(out.Data, struct {
				ID string `json:"id"`
			}{ID: m})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// logBuffer is a concurrency-safe buffer a funcr logger writes into, so a test
// can scan EVERY log line the handler emitted for a leaked key.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) write(prefix, args string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.WriteString(prefix)
	b.buf.WriteString(" ")
	b.buf.WriteString(args)
	b.buf.WriteString("\n")
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newConnectServer builds a Server with the connect flow ENABLED, the fake caller
// factory, and a real HTTP client (the probe URL is built from the baseURL each
// test passes, pointed at the fake provider). It returns the server, the
// token-recording factory, and the log buffer (to scan for a leaked key).
func newConnectServer(t *testing.T, c client.Client) (*Server, *fakeCallerClientFactory, *logBuffer) {
	t.Helper()
	factory := &fakeCallerClientFactory{client: c}
	lb := &logBuffer{}
	log := funcr.New(func(prefix, args string) { lb.write(prefix, args) }, funcr.Options{})
	s := NewServer(Options{
		CallerClients:   factory,
		Scheme:          testScheme(t),
		Auth:            AllowAll{},
		ProviderConnect: true,
		ProviderHTTP:    &http.Client{},
		Version:         "test",
		Log:             log,
	})
	return s, factory, lb
}

// connectBody marshals a connect request pointed at the fake provider base URL.
func connectBody(t *testing.T, provider, key, baseURL, ns string) []byte {
	t.Helper()
	b, err := json.Marshal(ConnectProviderRequest{
		Provider:    provider,
		DisplayName: strings.Title(provider), //nolint:staticcheck // test label only
		APIKey:      key,
		BaseURL:     baseURL,
		Namespace:   ns,
	})
	require.NoError(t, err)
	return b
}

// --- POST /api/providers: the connect flow -----------------------------------

// TestConnectCreatesAllThreeObjects proves the happy path: a valid key is
// validated against the (fake) provider, then a Secret + SecretBinding +
// ModelRoute are created with the CALLER'S client — and the key is present ONLY
// in the Secret, never in the response DTO or any log line.
func TestConnectCreatesAllThreeObjects(t *testing.T) {
	prov := fakeProvider(t, "claude-sonnet-4-6", "claude-opus-4-1")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, factory, lb := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers",
		bytes.NewReader(connectBody(t, "anthropic", theTestKey, prov.URL, "prod")))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	// The caller's token — not a BFF SA — scoped the create.
	assert.Equal(t, "developer-persona-token", factory.gotToken)

	var resp ConnectProviderResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "anthropic", resp.Provider.Provider)
	assert.Equal(t, "anthropic", resp.Provider.Name)
	assert.Equal(t, "anthropic", resp.Provider.SecretName)
	// The initial model list comes back from the connect probe (no 2nd round-trip).
	assert.Equal(t, []string{"claude-opus-4-1", "claude-sonnet-4-6"}, resp.Provider.Models)

	// Exactly three objects were created, in dependency order.
	require.Len(t, resp.Created, 3)
	assert.Equal(t, "Secret", resp.Created[0].Kind)
	assert.Equal(t, "SecretBinding", resp.Created[1].Kind)
	assert.Equal(t, "ModelRoute", resp.Created[2].Kind)

	// The Secret holds the key under the api-key data key.
	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "anthropic", Namespace: "prod"}, &secret))
	assert.Equal(t, theTestKey, string(secret.Data[secretKeyAPIKey]))
	assert.Equal(t, managedByConnect, secret.Labels[labelManagedBy])

	// The SecretBinding points at the Secret/key (backend kubernetes).
	var binding agentsv1alpha1.SecretBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "anthropic", Namespace: "prod"}, &binding))
	assert.Equal(t, secretBackendKubernetes, binding.Spec.Backend)
	assert.Equal(t, "anthropic", binding.Spec.SecretRef.Name)
	assert.Equal(t, secretKeyAPIKey, binding.Spec.SecretRef.Key)

	// The ModelRoute references the binding + carries the provider/model.
	var route agentsv1alpha1.ModelRoute
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "anthropic", Namespace: "prod"}, &route))
	require.Len(t, route.Spec.Providers, 1)
	assert.Equal(t, "anthropic", route.Spec.Providers[0].Provider)
	assert.Equal(t, "anthropic", route.Spec.Providers[0].SecretBindingRef)
	assert.Equal(t, "claude-opus-4-1", route.Spec.Providers[0].Model)

	// THE CRUX (ADR 0015): the key appears in the Secret ONLY — never in the
	// response DTO and never in any log line.
	assert.NotContains(t, rec.Body.String(), theTestKey, "the key must NEVER appear in the response DTO")
	assert.NotContains(t, lb.String(), theTestKey, "the key must NEVER be logged")
}

// TestConnectBadKeyIs401 proves a key the provider rejects (fake returns 401)
// surfaces as a clean 401 — NOT a 500 — and NO objects are created.
func TestConnectBadKeyIs401(t *testing.T) {
	prov := fakeProvider(t, "claude-sonnet-4-6")
	createCalled := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).
		Build()
	s, _, lb := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers",
		bytes.NewReader(connectBody(t, "anthropic", "wrong-key", prov.URL, "prod")))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code, "a bad key is a clean 401, not a 500")
	assert.False(t, createCalled, "a bad key must create NO objects")
	// Even the (wrong) key must not leak into the response or logs.
	assert.NotContains(t, rec.Body.String(), "wrong-key")
	assert.NotContains(t, lb.String(), "wrong-key")
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.NotEmpty(t, errBody.Error)
}

// TestConnectOpenAIShape proves the openai provider shape is supported too (the
// Bearer auth scheme + the shared {"data":[{"id":...}]} model list).
func TestConnectOpenAIShape(t *testing.T) {
	prov := fakeProvider(t, "gpt-4o", "gpt-4o-mini")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers",
		bytes.NewReader(connectBody(t, "openai", theTestKey, prov.URL, "")))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp ConnectProviderResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "openai", resp.Provider.Provider)
	assert.Equal(t, []string{"gpt-4o", "gpt-4o-mini"}, resp.Provider.Models)
	// Created in the default namespace when none is supplied.
	assert.Equal(t, defaultCreateNamespace, resp.Provider.Namespace)
}

// TestConnectViewerForbiddenIs403 proves a viewer whose RBAC denies the Secret
// create is rejected by the API server (as the caller) and the 403 SURFACES — it
// is not swallowed as a success. The key is validated first (fake accepts it),
// then the create is denied.
func TestConnectViewerForbiddenIs403(t *testing.T) {
	prov := fakeProvider(t, "claude-sonnet-4-6")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Resource: "secrets"}, "anthropic", assert.AnError)
			},
		}).
		Build()
	s, _, _ := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers",
		bytes.NewReader(connectBody(t, "anthropic", theTestKey, prov.URL, "prod")))
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, "a viewer's denied create must surface a 403")
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.NotEmpty(t, errBody.Error)
}

// TestConnectReconnectRotatesNotConflict proves re-connecting the same provider
// UPSERTS (ADR 0018): the existing Secret is updated in place — the stored key is
// rotated to the newly pasted value — and the connect SUCCEEDS, never a 409.
func TestConnectReconnectRotatesNotConflict(t *testing.T) {
	prov := fakeProvider(t, "claude-sonnet-4-6")
	// Pre-seed the Secret with an OLD key so we can prove the reconnect rotates it.
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "prod"},
		Data:       map[string][]byte{secretKeyAPIKey: []byte("sk-OLD-key-rotated-away")},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s, _, _ := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers",
		bytes.NewReader(connectBody(t, "anthropic", theTestKey, prov.URL, "prod")))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "re-connecting an existing provider upserts (rotates), never 409")

	// The stored key was rotated to the newly pasted value — the old key is gone.
	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Name: "anthropic", Namespace: "prod"}, &got))
	assert.Equal(t, theTestKey, string(got.Data[secretKeyAPIKey]), "reconnect rotates the stored key")
	assert.NotContains(t, string(got.Data[secretKeyAPIKey]), "OLD", "the old key must be overwritten")
}

// TestConnectMissingFieldsAre400 proves an empty provider or apiKey is a 400
// BEFORE any provider probe or K8s create.
func TestConnectMissingFieldsAre400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newConnectServer(t, c)

	for _, tc := range []struct {
		name string
		body ConnectProviderRequest
	}{
		{"no-provider", ConnectProviderRequest{APIKey: "k"}},
		{"no-key", ConnectProviderRequest{Provider: "anthropic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.body)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/providers", bytes.NewReader(raw))
			req.Header.Set("Authorization", "Bearer developer-persona-token")
			s.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

// TestConnectAnonIs401 proves a token-less connect is rejected with 401 BEFORE
// any provider probe or K8s create.
func TestConnectAnonIs401(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).
		Build()
	factory := &fakeCallerClientFactory{client: c, requireToken: true}
	s := NewServer(Options{
		CallerClients:   factory,
		Scheme:          testScheme(t),
		Auth:            AllowAll{}, // the FACTORY enforces the token here
		ProviderConnect: true,
		ProviderHTTP:    &http.Client{},
		Version:         "test",
		Log:             logr.Discard(),
	})

	rec := httptest.NewRecorder()
	// No Authorization header.
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/providers",
		bytes.NewReader(connectBody(t, "anthropic", theTestKey, "http://unused", "prod"))))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, createCalled, "no K8s create for a token-less request")
}

// --- GET /api/providers: list connected providers ----------------------------

// TestListProvidersEmpty proves the empty case yields [] (not null) under both
// keys — never a null the SPA has to guard.
func TestListProvidersEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"providers":[],"items":[]}`, rec.Body.String())
}

// TestListProvidersNoSecrets proves the list projects connect-managed routes onto
// the flat summary WITHOUT any secret material — only the Secret NAME as a ref.
func TestListProvidersNoSecrets(t *testing.T) {
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "anthropic",
			Namespace:   "prod",
			Labels:      map[string]string{labelManagedBy: managedByConnect, labelProvider: "anthropic"},
			Annotations: map[string]string{annDisplayName: "Anthropic"},
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 1, SecretBindingRef: "anthropic",
			}},
		},
	}
	// A NON-connect route must be excluded from the list (no managed-by label).
	other := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "hand-made", Namespace: "prod"},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{Provider: "mock", Model: "mock-default", Priority: 1}},
		},
	}
	// Also seed the backing Secret to prove the list NEVER reads/echoes its data.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "prod"},
		Data:       map[string][]byte{secretKeyAPIKey: []byte(theTestKey)},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(route, other, secret).Build()
	s, _, _ := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp ProviderListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Providers, 1, "only connect-managed routes are listed")
	p := resp.Providers[0]
	assert.Equal(t, "anthropic", p.Name)
	assert.Equal(t, "anthropic", p.Provider)
	assert.Equal(t, "Anthropic", p.DisplayName)
	assert.Equal(t, []string{"claude-sonnet-4-6"}, p.Models)
	assert.Equal(t, "anthropic", p.SecretName, "only the Secret NAME, never the key")
	// The key material never appears anywhere in the list response.
	assert.NotContains(t, rec.Body.String(), theTestKey)
}

// TestListProvidersForbiddenIs403 proves a Forbidden on the route list surfaces
// as 403, never a swallowed empty list.
func TestListProvidersForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(forbiddenListInterceptor()).
		Build()
	s, _, _ := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"providers"`, "a 403 must not be the empty-list success shape")
}

// --- GET /api/providers/{name}/models: live model list -----------------------

// TestProviderModelsProxiesLiveList proves the models endpoint reads the stored
// key (route → binding → Secret) and re-probes the provider, returning the live
// model list with NO secret material.
func TestProviderModelsProxiesLiveList(t *testing.T) {
	prov := fakeProvider(t, "claude-3-5-haiku", "claude-sonnet-4-6")
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "anthropic",
			Namespace: "prod",
			Labels:    map[string]string{labelManagedBy: managedByConnect, labelProvider: "anthropic"},
			// The connect-time baseURL override is persisted here; the stored-key
			// re-probe reaches the SAME endpoint (the fake) it validated against.
			Annotations: map[string]string{annBaseURL: prov.URL},
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 1, SecretBindingRef: "anthropic",
			}},
		},
	}
	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "prod"},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend:   secretBackendKubernetes,
			SecretRef: agentsv1alpha1.SecretKeyRef{Name: "anthropic", Key: secretKeyAPIKey},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "prod"},
		Data:       map[string][]byte{secretKeyAPIKey: []byte(theTestKey)},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(route, binding, secret).Build()
	s, _, lb := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/providers/anthropic/models?namespace=prod", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp ProviderModelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "anthropic", resp.Provider)
	assert.Equal(t, []string{"claude-3-5-haiku", "claude-sonnet-4-6"}, resp.Models)
	assert.NotContains(t, rec.Body.String(), theTestKey, "no secret material in the models response")
	assert.NotContains(t, lb.String(), theTestKey)
}

// TestProviderModelsNotFoundIs404 proves an unknown provider name is a 404.
func TestProviderModelsNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/providers/ghost/models?namespace=prod", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestProviderModelsSecretForbiddenIs403 proves a viewer denied the Secret read
// gets a 403 (the caller-scoped read denial surfaces, not a swallowed success).
func TestProviderModelsSecretForbiddenIs403(t *testing.T) {
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: "anthropic", Namespace: "prod",
			Labels: map[string]string{labelManagedBy: managedByConnect, labelProvider: "anthropic"},
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 1, SecretBindingRef: "anthropic",
			}},
		},
	}
	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "prod"},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend: secretBackendKubernetes, SecretRef: agentsv1alpha1.SecretKeyRef{Name: "anthropic", Key: secretKeyAPIKey},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(route, binding).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, assert.AnError)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	s, _, _ := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/providers/anthropic/models?namespace=prod", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
}

// --- kill-switch -------------------------------------------------------------

// TestKillSwitchOffIs404 proves that with the connect flow DISABLED, all three
// endpoints 404 (feature-off) — the UI falls back to reference-existing.
func TestKillSwitchOffIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		CallerClients:   newFakeFactory(c),
		Scheme:          testScheme(t),
		Auth:            AllowAll{},
		ProviderConnect: false, // kill-switch OFF
		ProviderHTTP:    &http.Client{},
		Version:         "test",
		Log:             logr.Discard(),
	})

	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/providers"},
		{http.MethodGet, "/api/providers"},
		{http.MethodGet, "/api/providers/anthropic/models"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(`{}`)))
			req.Header.Set("Authorization", "Bearer developer-persona-token")
			s.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotFound, rec.Code, "feature-off endpoints must 404")
		})
	}
}

// TestConnectNilFactoryIs501 proves that with the flow enabled but NO caller
// factory wired, the routes serve an honest 501 (never a BFF-SA fallback).
func TestConnectNilFactoryIs501(t *testing.T) {
	s := NewServer(Options{
		CallerClients:   nil, // no caller-scoping wired
		Scheme:          testScheme(t),
		Auth:            AllowAll{},
		ProviderConnect: true,
		Version:         "test",
		Log:             logr.Discard(),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// --- provider client unit (validation → model list) --------------------------

// TestProviderModelsProbeBadKey proves the low-level probe maps a provider 401
// to a *providerError with status 401 (the honest-error contract), not a 500.
func TestProviderModelsProbeBadKey(t *testing.T) {
	prov := fakeProvider(t, "m1")
	_, err := providerModels(context.Background(), &http.Client{}, "openai", "nope", prov.URL)
	require.Error(t, err)
	pe, ok := isProviderError(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusUnauthorized, pe.status)
}

// TestProviderModelsProbeUnsupported proves an unknown provider is a 400.
func TestProviderModelsProbeUnsupported(t *testing.T) {
	_, err := providerModels(context.Background(), &http.Client{}, "cohere", "k", "")
	require.Error(t, err)
	pe, ok := isProviderError(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusBadRequest, pe.status)
}

// --- POST /api/providers/{name}/rotate + DELETE /api/providers/{name} --------

// seedConnectedProvider builds the route+binding+secret triple a connected
// provider consists of (name "anthropic" in "prod"), with the route's baseURL
// pointed at the fake provider so the rotation's re-probe reaches it.
func seedConnectedProvider(baseURL, key string) []client.Object {
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: "anthropic", Namespace: "prod",
			Labels:      map[string]string{labelManagedBy: managedByConnect, labelProvider: "anthropic"},
			Annotations: map[string]string{annBaseURL: baseURL, annDisplayName: "Anthropic"},
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 1, SecretBindingRef: "anthropic",
			}},
		},
	}
	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "prod"},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend:   secretBackendKubernetes,
			SecretRef: agentsv1alpha1.SecretKeyRef{Name: "anthropic", Key: secretKeyAPIKey},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "prod"},
		Data:       map[string][]byte{secretKeyAPIKey: []byte(key)},
	}
	return []client.Object{route, binding, secret}
}

// TestRotateProviderKeyRotatesInPlace proves rotate validates the NEW key and
// rewrites ONLY the Secret's data — the key never appears in the response or logs.
func TestRotateProviderKeyRotatesInPlace(t *testing.T) {
	prov := fakeProvider(t, "claude-sonnet-4-6")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(seedConnectedProvider(prov.URL, "sk-OLD-key-rotated-away")...).Build()
	s, _, lb := newConnectServer(t, c)

	body, err := json.Marshal(RotateProviderKeyRequest{APIKey: theTestKey, Namespace: "prod"})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers/anthropic/rotate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Name: "anthropic", Namespace: "prod"}, &got))
	assert.Equal(t, theTestKey, string(got.Data[secretKeyAPIKey]), "the stored key is rotated in place")
	assert.NotContains(t, rec.Body.String(), theTestKey, "the key never appears in the response")
	assert.NotContains(t, lb.String(), theTestKey, "the key never appears in any log line")
}

// TestRotateProviderKeyBadKeyIs401NoChange proves a bad NEW key is rejected by the
// live probe (401) and the stored key is left UNCHANGED — never a silent rotate to
// a broken credential.
func TestRotateProviderKeyBadKeyIs401NoChange(t *testing.T) {
	prov := fakeProvider(t, "claude-sonnet-4-6") // accepts only theTestKey
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(seedConnectedProvider(prov.URL, theTestKey)...).Build()
	s, _, _ := newConnectServer(t, c)

	body, err := json.Marshal(RotateProviderKeyRequest{APIKey: "sk-bad-new-key", Namespace: "prod"})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/providers/anthropic/rotate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Name: "anthropic", Namespace: "prod"}, &got))
	assert.Equal(t, theTestKey, string(got.Data[secretKeyAPIKey]), "the stored key is unchanged on a bad rotate")
}

// TestDisconnectProviderDeletesAllThree proves DELETE removes the ModelRoute,
// SecretBinding, and Secret with the caller's client.
func TestDisconnectProviderDeletesAllThree(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(seedConnectedProvider("http://unused", theTestKey)...).Build()
	s, _, _ := newConnectServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/providers/anthropic?namespace=prod", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	for _, o := range []client.Object{
		&agentsv1alpha1.ModelRoute{}, &agentsv1alpha1.SecretBinding{}, &corev1.Secret{},
	} {
		err := c.Get(context.Background(), client.ObjectKey{Name: "anthropic", Namespace: "prod"}, o)
		assert.True(t, apierrors.IsNotFound(err), "%T must be deleted", o)
	}
}
