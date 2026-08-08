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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	"github.com/ctxmesh/agent-engine/internal/expand"
)

// A valid managed agent.yaml the fake provider "generates" — it expand-validates
// to a managed-runtime AgentDeployment (runtime: managed → the pinned image).
const validGeneratedYAML = `name: support-bot
runtime: managed
systemPrompt: You are a friendly support assistant.
tools:
  - search_docs
`

// fakeChatProvider is an httptest server standing in for anthropic/openai's
// chat/completions endpoint. It returns `reply` as the model output when the
// presented key matches theTestKey (proving the key flowed server-side only), a
// 401 otherwise. It records the request body so a test can assert the cost tag
// rode along and the key never did.
func fakeChatProvider(t *testing.T, reply string) (*httptest.Server, *[]byte) {
	t.Helper()
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("x-api-key")
		}
		if got != theTestKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		lastBody = body
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case "/v1/messages": // Anthropic Messages API shape
			_ = json.NewEncoder(w).Encode(map[string]any{
				"content": []map[string]string{{"type": "text", "text": reply}},
			})
		case "/v1/chat/completions": // OpenAI Chat Completions shape
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]string{"content": reply}}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &lastBody
}

// connectRouteObjects returns a connect-managed ModelRoute + SecretBinding +
// Secret for the given provider, with the baseURL annotation pointed at the fake
// chat provider so the generation call reaches it. The Secret holds theTestKey.
func connectRouteObjects(provider, model, baseURL string) []client.Object {
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:        provider,
			Namespace:   "prod",
			Labels:      map[string]string{labelManagedBy: managedByConnect, labelProvider: provider},
			Annotations: map[string]string{annBaseURL: baseURL, annDisplayName: provider},
		},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider: provider, Model: model, Priority: 1, SecretBindingRef: provider,
			}},
		},
	}
	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: provider, Namespace: "prod"},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend:   secretBackendKubernetes,
			SecretRef: agentsv1alpha1.SecretKeyRef{Name: provider, Key: secretKeyAPIKey},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: provider, Namespace: "prod"},
		Data:       map[string][]byte{secretKeyAPIKey: []byte(theTestKey)},
	}
	return []client.Object{route, binding, secret}
}

// newGenerateServer builds a Server with the generation endpoint wired (caller
// factory + expand adapter + a real HTTP client pointed via the route baseURL at
// the fake provider). It returns the server, the token-recording factory, and the
// log buffer (to scan for a leaked key). platformModels pins the generation-model
// list (nil → unpinned).
func newGenerateServer(t *testing.T, c client.Client, platformModels []string) (*Server, *fakeCallerClientFactory, *logBuffer) {
	t.Helper()
	factory := &fakeCallerClientFactory{client: c}
	lb := &logBuffer{}
	log := funcr.New(func(prefix, args string) { lb.write(prefix, args) }, funcr.Options{})
	s := NewServer(Options{
		CallerClients:            factory,
		Scheme:                   testScheme(t),
		Auth:                     AllowAll{},
		Adapters:                 Adapters{Expand: NewExpandAdapter()},
		ProviderHTTP:             &http.Client{},
		PlatformGenerationModels: platformModels,
		Version:                  "test",
		Log:                      log,
	})
	return s, factory, lb
}

func generateBody(t *testing.T, req GenerateAgentRequest) []byte {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err)
	return b
}

// TestGenerateHappyPathManagedPreview proves a valid description → the connected
// provider generates a simplified agent.yaml → it is expand-validated → the CRD
// preview (a managed-runtime AgentDeployment) is returned for REVIEW. The key is
// resolved caller-scoped, rides the chat call server-side, and appears NOWHERE in
// the response or logs; the cost tag DOES ride the request.
func TestGenerateHappyPathManagedPreview(t *testing.T) {
	prov, lastBody := fakeChatProvider(t, validGeneratedYAML)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)...).Build()
	s, factory, lb := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{
			Description: "a friendly support bot that can search docs",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "developer-persona-token", factory.gotToken, "the caller's token scoped the key read")

	var resp GenerateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "anthropic", resp.Provider)
	assert.Equal(t, "claude-sonnet-4-6", resp.Model)
	assert.Contains(t, resp.AgentYAML, "runtime: managed")
	// The CRD preview is the expand output — a managed AgentDeployment resolved to
	// the pinned managed image, plus the tool binding.
	assert.Contains(t, resp.Expanded, "kind: AgentDeployment")
	assert.Contains(t, resp.Expanded, "kind: MCPToolBinding")
	assert.Contains(t, resp.Expanded, expand.DefaultManagedImage, "managed runtime resolves the pinned image")
	assert.NotNil(t, resp.Warnings, "warnings is [] not null")

	// THE CRUX: the key appears NOWHERE in the response DTO or any log line, but it
	// DID authorize the chat call (the fake required it) — proving server-side use.
	assert.NotContains(t, rec.Body.String(), theTestKey, "the key must NEVER be in the response")
	assert.NotContains(t, lb.String(), theTestKey, "the key must NEVER be logged")

	// The generation call is cost-tagged (visible), and the key never rode the body.
	require.NotNil(t, *lastBody)
	assert.Contains(t, string(*lastBody), generationCostTag, "the generation call is cost-tagged")
	assert.NotContains(t, string(*lastBody), theTestKey, "the key rides headers, never the body")
}

// TestGenerateInvalidIsRegenerateNot500 proves an UNPARSEABLE/JUNK generation is a
// 422 with the raw output + the expand reason + regenerate=true — NOT a 500, and
// nothing is applied (no CRD create ever happens on this path).
func TestGenerateInvalidIsRegenerateNot500(t *testing.T) {
	prov, _ := fakeChatProvider(t, "I'm sorry, I cannot help with that. :)")
	createCalled := false
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).Build()
	s, _, lb := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{Description: "gibberish please", Namespace: "prod"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "invalid generation is a 422, NOT a 500")
	var resp GenerateInvalidResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Regenerate, "the UI is signalled to regenerate")
	assert.NotEmpty(t, resp.Reason, "the expand validation reason is returned")
	assert.NotEmpty(t, resp.AgentYAML, "the raw output is returned so the user can regenerate")
	assert.False(t, createCalled, "NEVER auto-applies: no CRD create on the generate path")
	assert.NotContains(t, lb.String(), theTestKey)
}

// TestGenerateNeverAutoApplies proves that even a fully VALID generation performs
// NO CRD create — the generate path only expand-validates + returns for review.
func TestGenerateNeverAutoApplies(t *testing.T) {
	prov, _ := fakeChatProvider(t, validGeneratedYAML)
	createCalled := false
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{Description: "a support bot", Namespace: "prod"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.False(t, createCalled, "generate NEVER creates a CRD — Create is the separate POST /api/agents")
}

// TestGenerateStripsCodeFence proves a model that wraps the YAML in a markdown
// code fence still validates (the fence is stripped before expand).
func TestGenerateStripsCodeFence(t *testing.T) {
	fenced := "```yaml\n" + validGeneratedYAML + "```"
	prov, _ := fakeChatProvider(t, fenced)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)...).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{Description: "x", Namespace: "prod"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp GenerateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, strings.Contains(resp.AgentYAML, "```"), "the code fence is stripped")
	assert.Contains(t, resp.Expanded, "kind: AgentDeployment")
}

// TestGenerateOpenAIProvider proves the OpenAI chat/completions shape is supported.
func TestGenerateOpenAIProvider(t *testing.T) {
	prov, lastBody := fakeChatProvider(t, validGeneratedYAML)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(connectRouteObjects("openai", "gpt-4o", prov.URL)...).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{Description: "a bot", Provider: "openai", Namespace: "prod"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp GenerateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "openai", resp.Provider)
	assert.Equal(t, "gpt-4o", resp.Model)
	assert.Contains(t, string(*lastBody), generationCostTag)
}

// TestGenerateNoConnectedProviderIs400 proves a caller with NO connected provider
// gets an honest 400 (connect first) — not a 500.
func TestGenerateNoConnectedProviderIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{Description: "a bot", Namespace: "prod"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.Contains(t, errBody.Error, "connect a provider")
}

// TestGenerateUnknownProviderIs400 proves naming an unconnected/unknown provider
// route is an honest 404/400 (not a 500).
func TestGenerateUnknownProviderIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{Description: "a bot", Provider: "ghost", Namespace: "prod"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "an unknown provider route is a 404, not a 500")
}

// TestGenerateViewerSecretForbiddenIs403 proves a viewer who cannot read the
// provider Secret is denied by the API server → a 403 SURFACES (not a 500, not a
// swallowed success). No generation call is made.
func TestGenerateViewerSecretForbiddenIs403(t *testing.T) {
	// The chat provider must NOT be reached — if the key read is denied first there
	// is no call. Point the baseURL at an unreachable host to prove no call fires.
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", "http://127.0.0.1:1")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, assert.AnError)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{
			Description: "a bot", Provider: "anthropic", Namespace: "prod",
		})))
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, "a viewer denied the Secret read must surface a 403")
}

// TestGeneratePlatformPinnedModelResolves proves the platform-pinned-model path
// (the UI dropdown source): a request Model that IS in the pinned list is used for
// generation, and one OUTSIDE the list is a 400.
func TestGeneratePlatformPinnedModelResolves(t *testing.T) {
	prov, _ := fakeChatProvider(t, validGeneratedYAML)
	pinned := []string{"claude-opus-4-1", "claude-sonnet-4-6"}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)...).Build()
	s, _, _ := newGenerateServer(t, c, pinned)

	// A pinned model → used for generation.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{
			Description: "a bot", Model: "claude-opus-4-1", Namespace: "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp GenerateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "claude-opus-4-1", resp.Model, "the pinned model the caller picked is used")

	// A model OUTSIDE the pinned list → an honest 400.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{
			Description: "a bot", Model: "gpt-4o-forbidden", Namespace: "prod",
		})))
	req2.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusBadRequest, rec2.Code, "a model outside the pinned list is a 400")
}

// TestGenerateBadKeyIs422 proves that if the stored key is rejected by the provider
// (fake returns 401), the endpoint surfaces a 422 — NOT a bare 401 (FUNC-9, ADR 0027):
// an upstream key rejection is a rotated/revoked key, not the caller's session dying,
// so the SPA must NOT log the user out mid-create. Never a 500, never leaks the key.
func TestGenerateBadKeyIs422(t *testing.T) {
	prov, _ := fakeChatProvider(t, validGeneratedYAML)
	// Seed the Secret with a WRONG key so the fake provider 401s.
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)
	for _, o := range objs {
		if secret, ok := o.(*corev1.Secret); ok {
			secret.Data[secretKeyAPIKey] = []byte("wrong-key")
		}
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s, _, lb := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{Description: "a bot", Namespace: "prod"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"an upstream key rejection is a 422, not a bare 401 that logs the user out (FUNC-9)")
	assert.NotContains(t, rec.Body.String(), "wrong-key")
	assert.NotContains(t, lb.String(), "wrong-key")
}

// TestGenerateMissingDescriptionIs400 proves an empty description is a 400 BEFORE
// any provider resolution or chat call.
func TestGenerateMissingDescriptionIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{Description: "  "})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestGenerateNilExpandIs501 proves that without the expand adapter wired, the
// route serves an honest 501 (the endpoint needs the one-mapping validator).
func TestGenerateNilExpandIs501(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		// No Expand adapter.
		Version: "test",
		Log:     funcr.New(func(string, string) {}, funcr.Options{}),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{Description: "x"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// TestGenerateNilFactoryIs501 proves that without the caller-client factory (no
// caller-scoping), the route serves 501 — never a BFF-SA fallback (ADR 0011).
func TestGenerateNilFactoryIs501(t *testing.T) {
	s := NewServer(Options{
		CallerClients: nil,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Expand: NewExpandAdapter()},
		Version:       "test",
		Log:           funcr.New(func(string, string) {}, funcr.Options{}),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{Description: "x"})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
