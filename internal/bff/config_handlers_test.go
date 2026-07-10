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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// newConfigServer builds a Server with the expand adapter + a write seam so the
// config-builder routes are live. The reader/writer is the SAME fake client-go
// client (mirrors production, where both are the manager's client.Client).
func newConfigServer(t *testing.T, c client.Client) *Server {
	t.Helper()
	return NewServer(Options{
		Reader:   c,
		Writer:   c,
		Scheme:   testScheme(t),
		Auth:     AllowAll{},
		Adapters: Adapters{Expand: NewExpandAdapter()},
		Version:  "test",
		Log:      logr.Discard(),
	})
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

// forbiddenWriter makes Create fail with a Kubernetes Forbidden error — the
// shape the API server returns for an M11 viewer persona. It proves the BFF
// surfaces the RBAC denial as a 403 instead of swallowing it or reporting 500.
type forbiddenWriter struct{}

func (forbiddenWriter) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	gvr := schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"}
	return apierrors.NewForbidden(gvr, obj.GetName(), errors.New("user cannot create agentdeployments"))
}

func TestCreateAgentHandlerRBACForbiddenIs403(t *testing.T) {
	s := NewServer(Options{
		Reader:   fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Writer:   forbiddenWriter{},
		Scheme:   testScheme(t),
		Auth:     AllowAll{},
		Adapters: Adapters{Expand: NewExpandAdapter()},
		Log:      logr.Discard(),
	})

	reqBody, _ := json.Marshal(CreateAgentRequest{AgentYAML: sampleAgentYAML, Namespace: "prod"})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(reqBody)))

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
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

// --- Auth + seam discipline -------------------------------------------------

func TestConfigRoutesRequireAuth(t *testing.T) {
	// With the real bearer authenticator, an anonymous caller is rejected before
	// the handler runs (M11 edge case).
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		Reader:   c,
		Writer:   c,
		Scheme:   testScheme(t),
		Auth:     BearerAuthenticator{},
		Adapters: Adapters{Expand: NewExpandAdapter()},
		Log:      logr.Discard(),
	})

	for _, path := range []string{"/api/expand", "/api/agents"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{}")))
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "path %s must require auth", path)
	}
}

func TestExpandSeamNotWiredServes501(t *testing.T) {
	// No Expand adapter + no writer → both config routes honestly report 501.
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		Reader: c,
		Auth:   AllowAll{},
		Log:    logr.Discard(),
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/expand", bytes.NewBufferString("x")))
	assert.Equal(t, http.StatusNotImplemented, rec.Code)

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewBufferString("{}")))
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
