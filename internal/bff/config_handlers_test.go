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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	sigsyaml "sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/expand"
)

// newConfigServer builds a Server with the expand adapter live and the CRD
// routes running through a caller-scoped client backed by the given fake client
// (ADR 0011): the create/list happen as the caller, so the fake client is what
// the K8s ops land on.
func newConfigServer(t *testing.T, c client.Client) *Server {
	t.Helper()
	return newCallerServer(t, newFakeFactory(c))
}

// sampleAgentYAML is a full-surface form submission (name/image/execModel +
// resources + scaling + model.route). It mirrors the CLI's full.yaml fixture so
// the handler exercises the same mapping the equivalence test proves.
const sampleAgentYAML = `name: echo-agent
image: ghcr.io/ctxmesh/echo:v1
executionModel: serving
resources:
  cpu: "500m"
  memory: "256Mi"
scaling:
  min: 1
  max: 3
model:
  route: default-model
`

// --- POST /api/expand -------------------------------------------------------

func TestExpandHandlerPreview(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/expand", bytes.NewBufferString(sampleAgentYAML))
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "yaml")
	body := rec.Body.String()
	// The preview is the expanded CRD, byte-driven by internal/expand.
	assert.Contains(t, body, "kind: AgentDeployment")
	assert.Contains(t, body, "name: echo-agent")
	assert.Contains(t, body, "image: ghcr.io/ctxmesh/echo:v1")
	assert.Contains(t, body, "MODEL_ROUTE")
}

func TestExpandHandlerValidationIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	// image missing → the mapping's required-field validation → 400, not 500.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/expand", bytes.NewBufferString("name: no-image\n"))
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "image")
}

func TestExpandHandlerEmptyBodyIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/expand", bytes.NewBufferString(""))
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- POST /api/agents (apply) -----------------------------------------------

func TestCreateAgentHandlerCreatesCRD(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: sampleAgentYAML, Namespace: "prod"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody))
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Created, 1)
	assert.Equal(t, "AgentDeployment", resp.Created[0].Kind)
	assert.Equal(t, "echo-agent", resp.Created[0].Name)
	assert.Equal(t, "prod", resp.Created[0].Namespace)

	// The object really landed in the (fake) cluster.
	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "echo-agent", Namespace: "prod"}, &got))
	assert.Equal(t, "ghcr.io/ctxmesh/echo:v1", got.Spec.Image)
}

// TestCreateAgentHandlerModelPickEnsuresRoute pins the m21 model-first create: a picked
// (provider, model) → the BFF ensures a ModelRoute serving it AND the created agent
// references THAT route (via the MODEL_ROUTE env expand emits) — the user picked a model,
// the platform managed the route.
func TestCreateAgentHandlerModelPickEnsuresRoute(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	reqBody, _ := json.Marshal(CreateAgentRequest{
		AgentYAML: sampleAgentYAML,
		Namespace: "prod",
		Model:     &ModelPick{Provider: "anthropic", Model: "claude-sonnet-5"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody))
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	// The ensured ModelRoute was created for the picked (provider, model).
	var mr agentsv1alpha1.ModelRoute
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Name: "anthropic-claude-sonnet-5", Namespace: "prod"}, &mr))
	require.Len(t, mr.Spec.Providers, 1)
	assert.Equal(t, "claude-sonnet-5", mr.Spec.Providers[0].Model)
	assert.Equal(t, "anthropic", mr.Spec.Providers[0].SecretBindingRef)

	// The created agent references THAT route (MODEL_ROUTE env from expand).
	var agent agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Name: "echo-agent", Namespace: "prod"}, &agent))
	var route string
	for _, e := range agent.Spec.Env {
		if e.Name == "MODEL_ROUTE" {
			route = e.Value
		}
	}
	assert.Equal(t, "anthropic-claude-sonnet-5", route,
		"the agent must reference the platform-ensured route for the picked model")
}

func TestCreateAgentHandlerMultiDoc(t *testing.T) {
	// An agent.yaml with eval + prompt expands to EvalSuite + PromptVersion +
	// AgentDeployment — all three must be created, in dependency order.
	const withRefs = `name: rich-agent
image: ghcr.io/ctxmesh/rich:v1
eval:
  suite: quality
  dataset: golden-set
  threshold: "0.8"
  scorers:
    - name: exact-match
      type: heuristic
prompt:
  name: system-prompt
  git:
    repo: https://github.com/acme/prompts
    ref: main
    path: prompts/system.txt
`
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: withRefs, Namespace: "prod"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody)))

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Created, 3)
	kinds := []string{resp.Created[0].Kind, resp.Created[1].Kind, resp.Created[2].Kind}
	assert.Equal(t, []string{"EvalSuite", "PromptVersion", "AgentDeployment"}, kinds)

	var es agentsv1alpha1.EvalSuite
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "quality", Namespace: "prod"}, &es))
	var pv agentsv1alpha1.PromptVersion
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "system-prompt", Namespace: "prod"}, &pv))
}

func TestCreateAgentHandlerBadYAMLIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: "name: x\nbogus: y\n"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgentHandlerMissingBodyIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	reqBody, _ := json.Marshal(CreateAgentRequest{Namespace: "prod"}) // no agentYAML
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// forbiddenClient is a caller-scoped client whose Create fails with a Kubernetes
// Forbidden error — the shape the API server returns for an M11 viewer persona
// whose token the BFF passed through. It proves the BFF surfaces the RBAC denial
// (from the CALLER-SCOPED client, not the BFF SA) as a 403, never swallowed or
// reported as 500. NOTE: this is a fake-client stand-in for the API server's
// decision; TRUE per-persona enforcement (a real viewer token → 403) is a tier2
// e2e assertion in m12.8 — a real API server, not a fake, makes that call.
func forbiddenClient(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				gvr := schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"}
				return apierrors.NewForbidden(gvr, obj.GetName(), errors.New("user cannot create agentdeployments"))
			},
		}).
		Build()
}

func TestCreateAgentHandlerRBACForbiddenIs403(t *testing.T) {
	// The Forbidden comes from the CALLER-SCOPED client the factory hands back —
	// driven through the factory, exactly as a real viewer token would be.
	factory := &fakeCallerClientFactory{client: forbiddenClient(t)}
	s := newCallerServer(t, factory)

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: sampleAgentYAML, Namespace: "prod"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
	// The viewer's token — not the BFF SA — is what the factory was asked to scope.
	assert.Equal(t, "viewer-persona-token", factory.gotToken)
}

func TestCreateAgentHandlerAlreadyExistsIs409(t *testing.T) {
	// Pre-create the object so the second create collides.
	existing := &agentsv1alpha1.AgentDeployment{}
	existing.Name = "echo-agent"
	existing.Namespace = "prod"
	existing.Spec.Image = "old:1"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newConfigServer(t, c)

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: sampleAgentYAML, Namespace: "prod"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody)))
	require.Equal(t, http.StatusConflict, rec.Code)
}

// --- Source-spec annotation (ADR 0017, m15.2) -------------------------------

// TestCreateAgentStampsSourceSpec proves a console create persists the exact
// submitted simplified spec on the AgentDeployment under the source-spec
// annotation as canonical JSON that round-trips back to the submitted intent.
func TestCreateAgentStampsSourceSpec(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: sampleAgentYAML, Namespace: "prod"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody)))
	require.Equal(t, http.StatusCreated, rec.Code)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "echo-agent", Namespace: "prod"}, &got))

	stored, ok := got.Annotations[expand.AnnotationSourceSpec]
	require.True(t, ok, "AgentDeployment must carry the source-spec annotation")
	require.NotEmpty(t, stored)

	// The stored value is canonical JSON and round-trips to the SAME structure the
	// user submitted (YAML → the same generic tree). This is the edit source of truth.
	var fromAnnotation, fromSubmitted any
	require.NoError(t, json.Unmarshal([]byte(stored), &fromAnnotation))
	require.NoError(t, sigsyaml.Unmarshal([]byte(sampleAgentYAML), &fromSubmitted))
	assert.Equal(t, fromSubmitted, fromAnnotation, "source-spec must round-trip the submitted spec")
}

// TestCreateAgentSourceSpecOnlyOnPrimary proves the annotation lands ONLY on the
// AgentDeployment — never on the generated EvalSuite/PromptVersion, which are
// derived state, not the user's intent.
func TestCreateAgentSourceSpecOnlyOnPrimary(t *testing.T) {
	const withRefs = `name: rich-agent
image: ghcr.io/ctxmesh/rich:v1
eval:
  suite: quality
  dataset: golden-set
  threshold: "0.8"
  scorers:
    - name: exact-match
      type: heuristic
prompt:
  name: system-prompt
  git:
    repo: https://github.com/acme/prompts
    ref: main
    path: prompts/system.txt
`
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: withRefs, Namespace: "prod"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody)))
	require.Equal(t, http.StatusCreated, rec.Code)

	var ad agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "rich-agent", Namespace: "prod"}, &ad))
	assert.Contains(t, ad.Annotations, expand.AnnotationSourceSpec, "primary AgentDeployment carries the annotation")

	var es agentsv1alpha1.EvalSuite
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "quality", Namespace: "prod"}, &es))
	assert.NotContains(t, es.Annotations, expand.AnnotationSourceSpec, "generated EvalSuite must NOT carry the annotation")

	var pv agentsv1alpha1.PromptVersion
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "system-prompt", Namespace: "prod"}, &pv))
	assert.NotContains(t, pv.Annotations, expand.AnnotationSourceSpec, "generated PromptVersion must NOT carry the annotation")
}

// TestCreateAgentInlineSecretRejected proves a spec carrying inline credential
// material is rejected with a teaching 4xx and NO object is created.
func TestCreateAgentInlineSecretRejected(t *testing.T) {
	// A managed agent with an inline apiKey — the exact anti-pattern ADR 0017 §2
	// forbids (annotations are readable by anyone with `get`).
	const withInlineSecret = `name: leaky-agent
runtime: managed
systemPrompt: hi
model:
  route: default-model
apiKey: sk-super-secret-value
`
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: withInlineSecret, Namespace: "prod"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody)))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "inline secrets are not allowed")

	// Nothing landed: the reject happens before any create.
	var got agentsv1alpha1.AgentDeployment
	err := c.Get(context.Background(), client.ObjectKey{Name: "leaky-agent", Namespace: "prod"}, &got)
	assert.True(t, apierrors.IsNotFound(err), "no AgentDeployment must be created when the spec is rejected")
}

// TestCreateAgentOversizeSpecRejected proves a source-spec over the size ceiling
// is rejected with a teaching 4xx and NO object is created.
func TestCreateAgentOversizeSpecRejected(t *testing.T) {
	// A valid-shaped spec whose systemPrompt alone pushes the canonical JSON well
	// past the 128KB ceiling.
	huge := strings.Repeat("x", maxSourceSpecBytes+1)
	oversize := "name: big-agent\nruntime: managed\nsystemPrompt: " + huge + "\n"

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newConfigServer(t, c)

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: oversize, Namespace: "prod"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody)))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "too large")

	var got agentsv1alpha1.AgentDeployment
	err := c.Get(context.Background(), client.ObjectKey{Name: "big-agent", Namespace: "prod"}, &got)
	assert.True(t, apierrors.IsNotFound(err), "no AgentDeployment must be created when the spec is oversize")
}

// TestFindInlineSecret is a focused unit test for the secret detector: it fires
// on inline credential values (by key and by the `value:` pattern) but NOT on
// by-name references or ordinary fields — the conservative-match contract.
func TestFindInlineSecret(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		rejects bool
	}{
		{"clean full spec", sampleAgentYAML, false},
		{"inline apiKey", "name: a\napiKey: sk-123\n", true},
		{"inline token nested", "name: a\nauth:\n  token: abc\n", true},
		{"inline password", "name: a\npassword: hunter2\n", true},
		{"inline bearer", "name: a\nbearer: xyz\n", true},
		{"secret block with inline value", "name: a\nsecret:\n  name: db\n  value: raw-pw\n", true},
		{"by-name secret reference (map value)", "name: a\napiKey:\n  secretName: prod-key\n", false},
		{"by-name secret ref block, no value", "name: a\nsecretRef:\n  secret: prod-key\n", true},
		{"ordinary name/value pair (not secret-shaped)", "name: a\nenv:\n  - name: FOO\n    value: bar\n", false},
		{"empty credential key", "name: a\ntoken: \"\"\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tree any
			require.NoError(t, sigsyaml.Unmarshal([]byte(tc.yaml), &tree))
			got := findInlineSecret(tree, "")
			if tc.rejects {
				assert.NotEmpty(t, got, "expected an inline-secret path")
			} else {
				assert.Empty(t, got, "expected no inline-secret match, got path %q", got)
			}
		})
	}
}

// --- Auth + seam discipline -------------------------------------------------

func TestConfigRoutesRequireAuth(t *testing.T) {
	// With the real bearer authenticator, an anonymous caller is rejected at the
	// edge (401) before the handler — and so before any caller-client is built or
	// any K8s call is made (M11 edge case).
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          BearerAuthenticator{},
		Adapters:      Adapters{Expand: NewExpandAdapter()},
		Log:           logr.Discard(),
	})

	for _, path := range []string{"/api/expand", "/api/agents"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{}")))
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "path %s must require auth", path)
	}
}

func TestExpandSeamNotWiredServes501(t *testing.T) {
	// No Expand adapter + no caller-client factory → both config routes honestly
	// report 501 (the BFF never falls back to an SA client for user CRD ops).
	s := NewServer(Options{
		Auth: AllowAll{},
		Log:  logr.Discard(),
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/expand", bytes.NewBufferString("x")))
	assert.Equal(t, http.StatusNotImplemented, rec.Code)

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewBufferString("{}")))
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
