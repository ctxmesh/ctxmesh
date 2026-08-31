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

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/expand"
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

// --- tool auto-wiring tests (ADR 0066 D2) ------------------------------------

// newGenerateServerWithCatalog builds a generate server with a tool registry store
// seeded with the given ToolRegistry objects (for the tool auto-wiring tests). It
// extends newGenerateServer by wiring wireTRStore so the catalog is reachable
// from buildCallerToolPrompt.
func newGenerateServerWithCatalog(t *testing.T, c client.Client, regs ...*agentsv1alpha1.ToolRegistry) (*Server, *fakeCallerClientFactory) {
	t.Helper()
	s, factory, _ := newGenerateServer(t, c, nil)
	wireTRStore(t, s, nil, regs...)
	return s, factory
}

// TestGeneratePromptContainsCatalogTools proves that when the caller's namespace
// has approved tools in the store the system prompt sent to the model contains
// those tool names (ADR 0066 D2).
func TestGeneratePromptContainsCatalogTools(t *testing.T) {
	prov, lastBody := fakeChatProvider(t, validGeneratedYAML)
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()

	reg := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tools", Namespace: "prod"},
		Spec: agentsv1alpha1.ToolRegistrySpec{
			Tools: []agentsv1alpha1.ToolEntry{
				{
					Name:           "search_docs",
					Description:    "Search the documentation",
					Source:         agentsv1alpha1.SourceCurated,
					ApprovalStatus: agentsv1alpha1.ApprovalApproved,
				},
				{
					Name:           "run_query",
					Description:    "Execute a SQL query",
					Source:         agentsv1alpha1.SourceCurated,
					ApprovalStatus: agentsv1alpha1.ApprovalApproved,
				},
			},
		},
	}
	s, _ := newGenerateServerWithCatalog(t, c, reg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{
			Description: "a support bot",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	// The system prompt sent to the model must contain the real catalog tool names.
	body := string(*lastBody)
	assert.Contains(t, body, "search_docs", "the catalog tool name must appear in the prompt")
	assert.Contains(t, body, "run_query", "the catalog tool name must appear in the prompt")
	assert.Contains(t, body, "Available tools", "the catalog block header must appear in the prompt")
}

// TestGeneratePromptNoToolsWhenCatalogEmpty proves that a caller whose namespace
// has NO visible tools gets a prompt with the "no tools" note and generation
// still succeeds (200 OK).
func TestGeneratePromptNoToolsWhenCatalogEmpty(t *testing.T) {
	prov, lastBody := fakeChatProvider(t, validGeneratedYAML)
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	// No tool registries seeded — empty catalog.
	s, _ := newGenerateServerWithCatalog(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{
			Description: "a support bot",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	body := string(*lastBody)
	assert.Contains(t, body, "No tools are available", "the prompt must instruct the model to omit tools")
	assert.NotContains(t, body, "Available tools", "no catalog list when empty")
}

// TestGeneratePromptExcludesPendingTools proves that pending-approval tools are
// NOT surfaced to the model — only approved tools appear in the catalog block.
func TestGeneratePromptExcludesPendingTools(t *testing.T) {
	prov, lastBody := fakeChatProvider(t, validGeneratedYAML)
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()

	reg := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "mixed-tools", Namespace: "prod"},
		Spec: agentsv1alpha1.ToolRegistrySpec{
			Tools: []agentsv1alpha1.ToolEntry{
				{
					Name:           "approved_tool",
					Description:    "An approved tool",
					Source:         agentsv1alpha1.SourceCurated,
					ApprovalStatus: agentsv1alpha1.ApprovalApproved,
				},
				{
					Name:           "pending_tool",
					Description:    "A tool awaiting approval",
					Source:         agentsv1alpha1.SourceUserAdded,
					ApprovalStatus: agentsv1alpha1.ApprovalPending,
				},
			},
		},
	}
	s, _ := newGenerateServerWithCatalog(t, c, reg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{
			Description: "a support bot",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	body := string(*lastBody)
	assert.Contains(t, body, "approved_tool", "approved tool must appear in catalog")
	assert.NotContains(t, body, "pending_tool", "pending tool must NOT appear in catalog")
}

// TestGenerateCatalogErrorDegradeGracefully proves that when the tool catalog
// store is not wired (nil store, authorization fails) the generation still runs
// successfully — the catalog lookup error degrades gracefully (never fails the
// generate request).
func TestGenerateCatalogErrorDegradeGracefully(t *testing.T) {
	prov, _ := fakeChatProvider(t, validGeneratedYAML)
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	// Use the standard generate server WITHOUT a TR store — the store is nil,
	// so mcpListToolRegistries will error, triggering graceful degrade.
	s, _, _ := newGenerateServer(t, c, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/generate",
		bytes.NewReader(generateBody(t, GenerateAgentRequest{
			Description: "a support bot",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	// Generation must still succeed 200 — the catalog error is a degrade, not a failure.
	require.Equal(t, http.StatusOK, rec.Code, "catalog lookup error must not fail the generate endpoint")
}

// TestBuildGenerationPromptUnit unit-tests buildGenerationPrompt directly for
// the three cases: nil (degrade), empty catalog, non-empty catalog.
func TestBuildGenerationPromptUnit(t *testing.T) {
	// nil catalog → base prompt unchanged (degrade path)
	got := buildGenerationPrompt(nil)
	assert.Equal(t, generationSystemPrompt, got, "nil catalog must return the base prompt verbatim")

	// empty catalog → base + "no tools" note
	got = buildGenerationPrompt([]ToolCatalogEntry{})
	assert.Contains(t, got, "No tools are available", "empty catalog must add a no-tools note")
	assert.NotContains(t, got, "Available tools", "empty catalog must not include a catalog list")

	// non-empty catalog → base + catalog block with the tool name + description
	tools := []ToolCatalogEntry{
		{Name: "alpha", Description: "does alpha things", ApprovalStatus: agentsv1alpha1.ApprovalApproved},
		{Name: "beta", Description: "", ApprovalStatus: agentsv1alpha1.ApprovalApproved},
	}
	got = buildGenerationPrompt(tools)
	assert.Contains(t, got, "Available tools", "non-empty catalog must include the catalog block header")
	assert.Contains(t, got, "- alpha: does alpha things", "catalog entry with description")
	assert.Contains(t, got, "- beta\n", "catalog entry without description has no trailing colon")
	assert.Contains(t, got, generationSystemPrompt, "the base prompt is always present")
}

// TestBuildGenerationPromptTruncatesLongDescriptions proves descriptions longer
// than maxToolDescriptionLen are truncated (the prompt stays bounded).
func TestBuildGenerationPromptTruncatesLongDescriptions(t *testing.T) {
	longDesc := strings.Repeat("x", maxToolDescriptionLen+50)
	tools := []ToolCatalogEntry{
		{Name: "big-tool", Description: longDesc, ApprovalStatus: agentsv1alpha1.ApprovalApproved},
	}
	got := buildGenerationPrompt(tools)
	assert.NotContains(t, got, longDesc, "the full long description must not appear verbatim")
	assert.Contains(t, got, "big-tool", "the tool name must still appear")
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

// TestResolveGeneration_GatewayRoutedSkipsSecret proves the M133 fix: with MODEL_GATEWAY_URL set,
// generation routes THROUGH the gateway by the route NAME — viaGateway=true, no caller key resolved —
// so a persona that cannot read the provider Secret still generates (the secret-read wall is removed).
func TestResolveGeneration_GatewayRoutedSkipsSecret(t *testing.T) {
	t.Setenv("MODEL_GATEWAY_URL", "http://gw.test:4000")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(connectRouteObjects("anthropic", "claude-sonnet-4-6", "")...).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	gen, gerr := s.resolveGeneration(context.Background(), c, "prod", &GenerateAgentRequest{})
	require.Nil(t, gerr, "gateway-routed generation must resolve without reading the Secret")
	assert.True(t, gen.viaGateway, "MODEL_GATEWAY_URL set ⇒ route via the gateway")
	assert.Equal(t, "http://gw.test:4000", gen.baseURL)
	assert.Equal(t, "anthropic", gen.model, "gateway path uses the route NAME as the model alias")
	assert.Empty(t, gen.apiKey, "gateway path must not carry a caller-read key")
}

// TestResolveGeneration_DirectPathReadsKey proves the fallback: with no gateway configured, the legacy
// direct path resolves the caller-scoped key + the provider model (unchanged behavior).
func TestResolveGeneration_DirectPathReadsKey(t *testing.T) {
	t.Setenv("MODEL_GATEWAY_URL", "")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(connectRouteObjects("anthropic", "claude-sonnet-4-6", "")...).Build()
	s, _, _ := newGenerateServer(t, c, nil)

	gen, gerr := s.resolveGeneration(context.Background(), c, "prod", &GenerateAgentRequest{})
	require.Nil(t, gerr)
	assert.False(t, gen.viaGateway)
	assert.Equal(t, theTestKey, gen.apiKey, "direct path reads the caller-scoped key")
	assert.Equal(t, "claude-sonnet-4-6", gen.model, "direct path uses the provider model")
}
