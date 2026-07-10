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
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// theMCPKey is the bearer key the BYO-MCP tests paste. DELIBERATELY recognizable
// so a leak scan (response body + log buffer) proves it appears NOWHERE it must
// not (the ADR 0016 crux, mirroring theTestKey for the provider flow).
const theMCPKey = "mcp-bearer-SECRET-do-not-leak-abcdef123456"

// fakeMCPServer is an httptest server standing in for a user's MCP server. It
// answers the streamable-http handshake: initialize → (notifications/initialized)
// → tools/list, returning tools with an inputSchema. When requireKey is set, it
// 401s any request whose Authorization bearer does not match theMCPKey — so the
// tests exercise both the open-server and keyed-server paths, and prove the key
// flowed on the wire SERVER-SIDE only.
func fakeMCPServer(t *testing.T, requireKey bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireKey {
			if r.Header.Get("Authorization") != "Bearer "+theMCPKey {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"missing or bad bearer"}`))
				return
			}
		}
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Mcp-Session-Id", "sess-123")
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[
				{"name":"get_weather","description":"Get the weather","inputSchema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
				{"name":"echo","description":"Echo text","inputSchema":{"type":"object","properties":{"text":{"type":"string"}}}}
			]}}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":9,"error":{"code":-32601,"message":"method not found"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newMCPServer builds a Server with the BYO-MCP flow ENABLED, the fake caller
// factory, and a real HTTP client (the probe reaches the httptest MCP server). It
// returns the server, the token-recording factory, and the log buffer.
func newMCPServer(t *testing.T, c client.Client, requireApproval bool) (*Server, *fakeCallerClientFactory, *logBuffer) {
	t.Helper()
	factory := &fakeCallerClientFactory{client: c}
	lb := &logBuffer{}
	log := funcr.New(func(prefix, args string) { lb.write(prefix, args) }, funcr.Options{})
	s := NewServer(Options{
		CallerClients:      factory,
		Scheme:             testScheme(t),
		Auth:               AllowAll{},
		MCPEnabled:         true,
		MCPRequireApproval: requireApproval,
		ProviderHTTP:       &http.Client{},
		Version:            "test",
		Log:                log,
	})
	return s, factory, lb
}

// rawExt wraps raw JSON as a *RawExtension for seeding ToolRegistry entries in
// tests (mirrors how the register handler stores a captured inputSchema).
func rawExt(raw string) *k8sruntime.RawExtension {
	return &k8sruntime.RawExtension{Raw: []byte(raw)}
}

// registerBody marshals a register request into the "prod" namespace (the test
// namespace all these cases use).
func registerBody(t *testing.T, name, url, key string) []byte {
	t.Helper()
	b, err := json.Marshal(RegisterMCPServerRequest{Name: name, URL: url, APIKey: key, Namespace: "prod"})
	require.NoError(t, err)
	return b
}

// --- POST /api/mcpservers: the register flow ---------------------------------

// TestRegisterMCPDiscoversToolsAndCapturesInputSchema is the happy path: probe
// discovers the tools + captures inputSchema; a Secret + SecretBinding hold the
// key (server-side only); a ToolRegistry entry per tool stores inputSchema; a
// per-server egress NetworkPolicy is created. The key never appears in the DTO or
// any log line.
func TestRegisterMCPDiscoversToolsAndCapturesInputSchema(t *testing.T) {
	mcp := fakeMCPServer(t, true)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, factory, lb := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers",
		bytes.NewReader(registerBody(t, "My Weather MCP", mcp.URL, theMCPKey)))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "developer-persona-token", factory.gotToken, "the caller's token scoped the create")

	var resp RegisterMCPServerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "my-weather-mcp", resp.Server.Name)
	assert.Equal(t, mcp.URL, resp.Server.URL)
	assert.Equal(t, 2, resp.Server.ToolCount)
	assert.Equal(t, agentsv1alpha1.ApprovalApproved, resp.Server.Status, "self-serve default")
	assert.Equal(t, "my-weather-mcp", resp.Server.SecretName)

	// The response carries the tools with their inputSchema.
	require.Len(t, resp.Tools, 2)
	byName := map[string]ToolCatalogEntry{}
	for _, tool := range resp.Tools {
		byName[tool.Name] = tool
	}
	weather := byName["get_weather"]
	require.NotNil(t, weather.InputSchema, "inputSchema MUST be captured (the m14.3-review requirement)")
	var parsedSchema map[string]any
	require.NoError(t, json.Unmarshal(weather.InputSchema, &parsedSchema))
	assert.Equal(t, "object", parsedSchema["type"])
	assert.Equal(t, agentsv1alpha1.SourceUserAdded, weather.Source)

	// Created objects: Secret, SecretBinding, ToolRegistry, NetworkPolicy.
	require.Len(t, resp.Created, 4)
	assert.Equal(t, "Secret", resp.Created[0].Kind)
	assert.Equal(t, "SecretBinding", resp.Created[1].Kind)
	assert.Equal(t, "ToolRegistry", resp.Created[2].Kind)
	assert.Equal(t, "NetworkPolicy", resp.Created[3].Kind)

	// The Secret holds the key under the api-key data key.
	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "my-weather-mcp", Namespace: "prod"}, &secret))
	assert.Equal(t, theMCPKey, string(secret.Data[secretKeyAPIKey]))
	assert.Equal(t, managedByMCP, secret.Labels[labelManagedBy])

	// The ToolRegistry stores each tool's inputSchema verbatim.
	var tr agentsv1alpha1.ToolRegistry
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "my-weather-mcp", Namespace: "prod"}, &tr))
	require.Len(t, tr.Spec.Tools, 2)
	for _, e := range tr.Spec.Tools {
		require.NotNil(t, e.InputSchema, "ToolRegistry entry MUST store inputSchema for m14.6b")
		assert.NotEmpty(t, e.InputSchema.Raw)
		assert.Equal(t, agentsv1alpha1.SourceUserAdded, e.Source)
		assert.Equal(t, agentsv1alpha1.ApprovalApproved, e.ApprovalStatus)
		assert.Equal(t, mcp.URL, e.URL)
	}

	// The per-server egress NetworkPolicy exists, is egress-only, and bounds to the
	// server's port (never a blanket open).
	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Name: "my-weather-mcp" + networkPolicyMCPSuffix, Namespace: "prod"}, &np))
	require.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
	require.Len(t, np.Spec.Egress, 1)
	require.Len(t, np.Spec.Egress[0].Ports, 1, "egress is scoped to the server port, not a blanket open")

	// THE CRUX (ADR 0016): the key appears in the Secret ONLY — never in the
	// response DTO and never in any log line.
	assert.NotContains(t, rec.Body.String(), theMCPKey, "the key must NEVER appear in the response DTO")
	assert.NotContains(t, lb.String(), theMCPKey, "the key must NEVER be logged")
}

// TestRegisterMCPOpenServerNoSecret proves an MCP server registered WITHOUT a key
// creates NO Secret/SecretBinding (only the ToolRegistry + NetworkPolicy), and the
// summary carries no secret name.
func TestRegisterMCPOpenServerNoSecret(t *testing.T) {
	mcp := fakeMCPServer(t, false)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers",
		bytes.NewReader(registerBody(t, "open-mcp", mcp.URL, "")))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp RegisterMCPServerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Server.SecretName)
	// Only ToolRegistry + NetworkPolicy — no Secret/SecretBinding.
	require.Len(t, resp.Created, 2)
	assert.Equal(t, "ToolRegistry", resp.Created[0].Kind)
	assert.Equal(t, "NetworkPolicy", resp.Created[1].Kind)

	var secret corev1.Secret
	err := c.Get(context.Background(), client.ObjectKey{Name: "open-mcp", Namespace: "prod"}, &secret)
	assert.True(t, apierrors.IsNotFound(err), "no Secret for an open server")
}

// TestRegisterMCPHardenedIsPendingApproval proves the values-gated hardened mode
// (requireApproval) marks the entry pending-approval instead of approved.
func TestRegisterMCPHardenedIsPendingApproval(t *testing.T) {
	mcp := fakeMCPServer(t, false)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, true) // requireApproval = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers",
		bytes.NewReader(registerBody(t, "hardened-mcp", mcp.URL, "")))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp RegisterMCPServerResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, agentsv1alpha1.ApprovalPending, resp.Server.Status, "hardened → pending-approval")

	var tr agentsv1alpha1.ToolRegistry
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "hardened-mcp", Namespace: "prod"}, &tr))
	for _, e := range tr.Spec.Tools {
		assert.Equal(t, agentsv1alpha1.ApprovalPending, e.ApprovalStatus)
	}
}

// TestRegisterMCPUnreachableIs4xxNoOrphan proves an unreachable / mis-speaking MCP
// server yields an honest 4xx (NOT a 500) and NO CRDs are created (no partial
// orphan) — the probe fails before any create.
func TestRegisterMCPUnreachableIs4xxNoOrphan(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).
		Build()
	s, _, _ := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	// A URL that resolves to nothing listening → the probe's Do fails → 502.
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers",
		bytes.NewReader(registerBody(t, "dead-mcp", "http://127.0.0.1:1/mcp", "")))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	// A 502 (upstream fault) is the honest teaching status for an unreachable
	// server; the crux is it is NOT a 500 (our fault) and creates nothing.
	assert.Equal(t, http.StatusBadGateway, rec.Code, "an unreachable MCP server is a 502 teaching error, never a 500")
	assert.False(t, createCalled, "a failed probe must create NO objects (no partial orphan)")
}

// TestRegisterMCPNonMCPEndpointIs4xx proves a URL that answers HTTP but does not
// speak MCP (a JSON-RPC error / non-JSON) is a 4xx, not a 500.
func TestRegisterMCPNonMCPEndpointIs4xx(t *testing.T) {
	notMCP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
	}))
	t.Cleanup(notMCP.Close)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers",
		bytes.NewReader(registerBody(t, "not-mcp", notMCP.URL, "")))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "a non-MCP endpoint is a 422 teaching error")
}

// TestRegisterMCPBadKeyIs401 proves a keyed server that rejects the bearer surfaces
// a clean 401 (not a 500), and no objects are created.
func TestRegisterMCPBadKeyIs401(t *testing.T) {
	mcp := fakeMCPServer(t, true) // requires theMCPKey
	createCalled := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).
		Build()
	s, _, lb := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers",
		bytes.NewReader(registerBody(t, "keyed-mcp", mcp.URL, "wrong-bearer")))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, createCalled, "a rejected key creates NO objects")
	assert.NotContains(t, rec.Body.String(), "wrong-bearer")
	assert.NotContains(t, lb.String(), "wrong-bearer")
}

// TestRegisterMCPViewerForbiddenIs403 proves a viewer whose RBAC denies the create
// gets a 403 surfaced (the caller-scoped create denial, not a swallowed success).
func TestRegisterMCPViewerForbiddenIs403(t *testing.T) {
	mcp := fakeMCPServer(t, false)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "toolregistries"}, "hardened", assert.AnError)
			},
		}).
		Build()
	s, _, _ := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers",
		bytes.NewReader(registerBody(t, "viewer-mcp", mcp.URL, "")))
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, "a viewer's denied create must surface a 403")
}

// TestRegisterMCPReconnectIs409 proves re-registering the same server (deterministic
// name) collides → a clean 409, never a silent overwrite.
func TestRegisterMCPReconnectIs409(t *testing.T) {
	mcp := fakeMCPServer(t, false)
	existing := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "dup-mcp", Namespace: "prod"},
		Spec:       agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{Name: "x"}}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s, _, _ := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers",
		bytes.NewReader(registerBody(t, "dup-mcp", mcp.URL, "")))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
}

// TestRegisterMCPMissingFieldsAre400 proves an empty name or url is a 400 before
// any probe or create.
func TestRegisterMCPMissingFieldsAre400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)
	for _, tc := range []struct {
		name string
		body RegisterMCPServerRequest
	}{
		{"no-name", RegisterMCPServerRequest{URL: "http://x/mcp"}},
		{"no-url", RegisterMCPServerRequest{Name: "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.body)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/mcpservers", bytes.NewReader(raw))
			req.Header.Set("Authorization", "Bearer developer-persona-token")
			s.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

// TestRegisterMCPAnonIs401 proves a token-less register is 401 before any probe or
// create.
func TestRegisterMCPAnonIs401(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).
		Build()
	factory := &fakeCallerClientFactory{client: c, requireToken: true}
	s := NewServer(Options{
		CallerClients: factory,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		MCPEnabled:    true,
		ProviderHTTP:  &http.Client{},
		Version:       "test",
		Log:           logr.Discard(),
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/mcpservers",
		bytes.NewReader(registerBody(t, "x", "http://unused/mcp", ""))))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, createCalled)
}

// --- GET /api/mcpservers -----------------------------------------------------

func TestListMCPServersEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcpservers", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"servers":[],"items":[]}`, rec.Body.String())
}

// TestListMCPServersProjectsNoSecrets proves the list projects register-managed
// registries onto the summary WITHOUT any secret material — only the Secret NAME.
func TestListMCPServersProjectsNoSecrets(t *testing.T) {
	tr := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "weather-mcp",
			Namespace: "prod",
			Labels:    map[string]string{labelManagedBy: managedByMCP},
			Annotations: map[string]string{
				annMCPURL:    "http://weather.example/mcp",
				annMCPStatus: agentsv1alpha1.ApprovalApproved,
				annMCPSecret: "weather-mcp",
			},
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{Name: "get_weather"}}},
	}
	// A curated (non-register) registry must be excluded (no managed-by=mcp label).
	curated := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "default-tools", Namespace: "prod"},
		Spec:       agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{Name: "curated_tool"}}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "weather-mcp", Namespace: "prod"},
		Data:       map[string][]byte{secretKeyAPIKey: []byte(theMCPKey)},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(tr, curated, secret).Build()
	s, _, _ := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcpservers", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp MCPServerListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Servers, 1, "only register-managed registries are listed")
	assert.Equal(t, "weather-mcp", resp.Servers[0].Name)
	assert.Equal(t, "http://weather.example/mcp", resp.Servers[0].URL)
	assert.Equal(t, 1, resp.Servers[0].ToolCount)
	assert.Equal(t, "weather-mcp", resp.Servers[0].SecretName)
	assert.NotContains(t, rec.Body.String(), theMCPKey)
}

func TestListMCPServersForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(forbiddenListInterceptor()).
		Build()
	s, _, _ := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcpservers", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// --- GET /api/tools: the merged catalog --------------------------------------

// TestListToolsMergesCuratedAndUserAdded proves GET /api/tools merges curated +
// user-added tools, includes inputSchema, no secrets, [] not null.
func TestListToolsMergesCuratedAndUserAdded(t *testing.T) {
	curated := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "default-tools", Namespace: "prod"},
		Spec: agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{
			// A legacy curated entry with no explicit source/status/schema.
			{Name: "curated_tool", Description: "an operator tool"},
		}},
	}
	userAdded := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "weather-mcp",
			Namespace: "prod",
			Labels:    map[string]string{labelManagedBy: managedByMCP},
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{
			Name:           "get_weather",
			Description:    "Get the weather",
			Source:         agentsv1alpha1.SourceUserAdded,
			ApprovalStatus: agentsv1alpha1.ApprovalApproved,
			InputSchema:    rawExt(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "weather-mcp", Namespace: "prod"},
		Data:       map[string][]byte{secretKeyAPIKey: []byte(theMCPKey)},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(curated, userAdded, secret).Build()
	s, _, _ := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp ToolCatalogResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Tools, 2)

	byName := map[string]ToolCatalogEntry{}
	for _, tool := range resp.Tools {
		byName[tool.Name] = tool
	}
	// The curated legacy entry defaults to curated/approved; no schema.
	cur := byName["curated_tool"]
	assert.Equal(t, agentsv1alpha1.SourceCurated, cur.Source)
	assert.Equal(t, agentsv1alpha1.ApprovalApproved, cur.ApprovalStatus)
	// A legacy curated entry has no schema → the wire carries JSON null.
	assert.Equal(t, "null", string(cur.InputSchema))
	// The user-added entry carries its inputSchema + source.
	wea := byName["get_weather"]
	assert.Equal(t, agentsv1alpha1.SourceUserAdded, wea.Source)
	require.NotNil(t, wea.InputSchema, "the merged catalog MUST include inputSchema")
	var parsedSchema map[string]any
	require.NoError(t, json.Unmarshal(wea.InputSchema, &parsedSchema))
	assert.Equal(t, "object", parsedSchema["type"])
	assert.Equal(t, "weather-mcp", wea.Registry)

	// No secret material anywhere in the catalog.
	assert.NotContains(t, rec.Body.String(), theMCPKey)
}

func TestListToolsEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"tools":[],"items":[]}`, rec.Body.String())
}

func TestListToolsForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(forbiddenListInterceptor()).
		Build()
	s, _, _ := newMCPServer(t, c, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// --- kill-switch -------------------------------------------------------------

// TestMCPKillSwitchOffIs404 proves that with the BYO-MCP flow DISABLED, all three
// endpoints 404 (feature-off).
func TestMCPKillSwitchOffIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		MCPEnabled:    false, // kill-switch OFF
		ProviderHTTP:  &http.Client{},
		Version:       "test",
		Log:           logr.Discard(),
	})
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/mcpservers"},
		{http.MethodGet, "/api/mcpservers"},
		{http.MethodGet, "/api/tools"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(`{}`)))
			req.Header.Set("Authorization", "Bearer developer-persona-token")
			s.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotFound, rec.Code, "feature-off endpoints must 404")
		})
	}
}

// TestMCPNilFactoryIs501 proves the flow enabled but no caller factory → honest
// 501 (never a BFF-SA fallback).
func TestMCPNilFactoryIs501(t *testing.T) {
	s := NewServer(Options{
		CallerClients: nil,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		MCPEnabled:    true,
		Version:       "test",
		Log:           logr.Discard(),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcpservers", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// --- MCP probe unit ----------------------------------------------------------

// TestProbeMCPServerSSE proves the probe parses a text/event-stream (SSE) response,
// the streamable-http variant, and still captures inputSchema.
func TestProbeMCPServerSSE(t *testing.T) {
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		switch req.Method {
		case "initialize":
			_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":[{\"name\":\"ping\",\"inputSchema\":{\"type\":\"object\"}}]}}\n\n"))
		}
	}))
	t.Cleanup(sse.Close)

	tools, err := probeMCPServer(context.Background(), &http.Client{}, sse.URL, "")
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "ping", tools[0].Name)
	require.NotEmpty(t, tools[0].InputSchema)
	var parsedSchema map[string]any
	require.NoError(t, json.Unmarshal(tools[0].InputSchema, &parsedSchema))
	assert.Equal(t, "object", parsedSchema["type"])
}

// TestProbeMCPServerNoTools proves a server that speaks MCP but advertises no
// tools is a 422 (nothing to add).
func TestProbeMCPServerNoTools(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "tools/list" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(empty.Close)

	_, err := probeMCPServer(context.Background(), &http.Client{}, empty.URL, "")
	require.Error(t, err)
	me, ok := isMCPError(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusUnprocessableEntity, me.status)
}

// TestHostPortFromURL covers the egress-peer host:port parse (scheme defaults +
// explicit port + rejects).
func TestHostPortFromURL(t *testing.T) {
	cases := []struct {
		url      string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"http://mcp.example/mcp", "mcp.example", 80, false},
		{"https://mcp.example/mcp", "mcp.example", 443, false},
		{"http://10.0.0.5:8080/mcp", "10.0.0.5", 8080, false},
		{"not a url", "", 0, true},
		{"ftp://x/y", "", 0, true},
	}
	for _, tc := range cases {
		host, port, err := hostPortFromURL(tc.url)
		if tc.wantErr {
			assert.Error(t, err, tc.url)
			continue
		}
		require.NoError(t, err, tc.url)
		assert.Equal(t, tc.wantHost, host, tc.url)
		assert.Equal(t, tc.wantPort, port, tc.url)
	}
}
