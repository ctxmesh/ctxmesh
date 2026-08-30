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

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

// validCreateTeamYAML is a minimal but complete AgentTeam YAML that passes all
// structural validation (supervisor + roster + registryRef) and decodes cleanly.
const validCreateTeamYAML = `apiVersion: agents.ctxmesh.ai/v1beta1
kind: AgentTeam
metadata:
  name: my-team
spec:
  registryRef: prod-registry
  supervisor:
    agentRef: orchestrator-bot
  roster:
    - name: worker-a
      agentRef: worker-bot
      description: does the work
`

// teamCreateBody marshals a CreateTeamRequest to JSON for test requests.
func teamCreateBody(t *testing.T, req CreateTeamRequest) []byte {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err)
	return b
}

// newTeamCreateServer builds a Server for team-create tests (no provider needed —
// create is a pure K8s write).
func newTeamCreateServer(t *testing.T, c client.Client) *Server {
	t.Helper()
	factory := &fakeCallerClientFactory{client: c}
	lb := &logBuffer{}
	log := funcr.New(func(prefix, args string) { lb.write(prefix, args) }, funcr.Options{})
	return NewServer(Options{
		CallerClients: factory,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Version:       "test",
		Log:           log,
	})
}

// --- Tests -------------------------------------------------------------------

// TestCreateTeamHappyPath proves a valid TeamYAML → the AgentTeam is created
// and the response is the AgentTeamSummary with 201 Created.
func TestCreateTeamHappyPath(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).Build()
	s := newTeamCreateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams",
		bytes.NewReader(teamCreateBody(t, CreateTeamRequest{
			TeamYAML:  validCreateTeamYAML,
			Namespace: "default",
		})))
	req.Header.Set("Authorization", "Bearer developer-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	assert.True(t, createCalled, "Create must be called on the K8s API")

	var summary AgentTeamSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &summary))
	assert.Equal(t, "my-team", summary.Name)
	assert.Equal(t, "orchestrator-bot", summary.Supervisor)
	assert.Equal(t, "prod-registry", summary.Registry)
	assert.Len(t, summary.Roster, 1)
	assert.Equal(t, "worker-bot", summary.Roster[0].AgentRef)
}

// TestCreateTeamMissingTeamYAMLIs400 proves that an empty teamYAML is rejected
// with 400 before any K8s call.
func TestCreateTeamMissingTeamYAMLIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newTeamCreateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams",
		bytes.NewReader(teamCreateBody(t, CreateTeamRequest{TeamYAML: "  "})))
	req.Header.Set("Authorization", "Bearer developer-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.Contains(t, errBody.Error, "teamYAML is required")
}

// TestCreateTeamBadYAMLIs400 proves that YAML that doesn't decode as an
// AgentTeam returns 400.
func TestCreateTeamBadYAMLIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newTeamCreateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams",
		bytes.NewReader(teamCreateBody(t, CreateTeamRequest{
			TeamYAML: "{{invalid yaml}}}",
		})))
	req.Header.Set("Authorization", "Bearer developer-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestCreateTeamMissingSupervisorIs400 proves that an AgentTeam YAML without
// spec.supervisor.agentRef is rejected with 400.
func TestCreateTeamMissingSupervisorIs400(t *testing.T) {
	noSupervisorYAML := `apiVersion: agents.ctxmesh.ai/v1beta1
kind: AgentTeam
metadata:
  name: bad-team
spec:
  registryRef: prod-registry
  roster:
    - name: worker-a
      agentRef: worker-bot
`
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newTeamCreateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams",
		bytes.NewReader(teamCreateBody(t, CreateTeamRequest{TeamYAML: noSupervisorYAML})))
	req.Header.Set("Authorization", "Bearer developer-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.Contains(t, errBody.Error, "supervisor")
}

// TestCreateTeamEmptyRosterIs400 proves that an AgentTeam YAML with no roster
// entries is rejected with 400.
func TestCreateTeamEmptyRosterIs400(t *testing.T) {
	noRosterYAML := `apiVersion: agents.ctxmesh.ai/v1beta1
kind: AgentTeam
metadata:
  name: bad-team
spec:
  registryRef: prod-registry
  supervisor:
    agentRef: orchestrator-bot
`
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newTeamCreateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams",
		bytes.NewReader(teamCreateBody(t, CreateTeamRequest{TeamYAML: noRosterYAML})))
	req.Header.Set("Authorization", "Bearer developer-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.Contains(t, errBody.Error, "roster")
}

// TestCreateTeamMissingRegistryRefIs400 proves that an AgentTeam YAML without
// spec.registryRef is rejected with 400.
func TestCreateTeamMissingRegistryRefIs400(t *testing.T) {
	noRegistryYAML := `apiVersion: agents.ctxmesh.ai/v1beta1
kind: AgentTeam
metadata:
  name: bad-team
spec:
  supervisor:
    agentRef: orchestrator-bot
  roster:
    - name: worker-a
      agentRef: worker-bot
`
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newTeamCreateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams",
		bytes.NewReader(teamCreateBody(t, CreateTeamRequest{TeamYAML: noRegistryYAML})))
	req.Header.Set("Authorization", "Bearer developer-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.Contains(t, errBody.Error, "registryRef")
}

// TestCreateTeamNameCollisionIs409 proves that an AlreadyExists error from the
// K8s API surfaces as 409 to the caller.
func TestCreateTeamNameCollisionIs409(t *testing.T) {
	// Seed the fake with an existing team so Create returns AlreadyExists.
	existing := &agentsv1beta1.AgentTeam{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-team",
			Namespace: "default",
		},
		Spec: agentsv1beta1.AgentTeamSpec{
			RegistryRef: "prod-registry",
			Supervisor:  agentsv1beta1.AgentTeamSupervisor{AgentRef: "orchestrator-bot"},
			Roster: []agentsv1beta1.AgentTeamRosterEntry{
				{Name: "worker-a", AgentRef: "worker-bot"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newTeamCreateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams",
		bytes.NewReader(teamCreateBody(t, CreateTeamRequest{
			TeamYAML:  validCreateTeamYAML,
			Namespace: "default",
		})))
	req.Header.Set("Authorization", "Bearer developer-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
}

// TestCreateTeamForbiddenIs403 proves that an RBAC denial from the K8s API
// surfaces as 403 to the caller (viewer persona).
func TestCreateTeamForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentteams"},
					obj.GetName(), nil)
			},
		}).Build()
	s := newTeamCreateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams",
		bytes.NewReader(teamCreateBody(t, CreateTeamRequest{
			TeamYAML: validCreateTeamYAML,
		})))
	req.Header.Set("Authorization", "Bearer viewer-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

// TestCreateTeamInlineSecretRejected proves that an AgentTeam YAML containing a
// credential-shaped value is rejected before any K8s call (defense-in-depth).
func TestCreateTeamInlineSecretRejected(t *testing.T) {
	secretYAML := `apiVersion: agents.ctxmesh.ai/v1beta1
kind: AgentTeam
metadata:
  name: secret-team
spec:
  registryRef: prod-registry
  supervisor:
    agentRef: orchestrator-bot
  roster:
    - name: worker-a
      agentRef: worker-bot
  apiKey: sk-ant-very-secret-key
`
	createCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).Build()
	s := newTeamCreateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams",
		bytes.NewReader(teamCreateBody(t, CreateTeamRequest{TeamYAML: secretYAML})))
	req.Header.Set("Authorization", "Bearer developer-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "inline secret must be rejected: body=%s", rec.Body.String())
	assert.False(t, createCalled, "Create must NOT be called when an inline secret is detected")
}

// TestCreateTeamDefaultNamespace proves that a missing namespace defaults to
// "default" and the team is stamped accordingly.
func TestCreateTeamDefaultNamespace(t *testing.T) {
	var createdTeam agentsv1beta1.AgentTeam
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if t, ok := obj.(*agentsv1beta1.AgentTeam); ok {
					createdTeam = *t
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	s := newTeamCreateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams",
		// no Namespace field → should default to "default"
		bytes.NewReader(teamCreateBody(t, CreateTeamRequest{TeamYAML: validCreateTeamYAML})))
	req.Header.Set("Authorization", "Bearer developer-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "default", createdTeam.Namespace)
}
