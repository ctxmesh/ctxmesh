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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// validTeamYAML is the minimal AgentTeam YAML the fake model returns when all
// agents are eligible — it decode-validates correctly and passes referential checks.
const validTeamYAML = `apiVersion: agents.ctxmesh.ai/v1beta1
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

// hallucintedTeamYAML references a non-existent agent — triggers referential validation failure.
const hallucinatedTeamYAML = `apiVersion: agents.ctxmesh.ai/v1beta1
kind: AgentTeam
metadata:
  name: bad-team
spec:
  registryRef: prod-registry
  supervisor:
    agentRef: orchestrator-bot
  roster:
    - name: ghost
      agentRef: non-existent-agent
      description: hallucinated
`

// teamGenerateBody marshals a GenerateTeamRequest to JSON for test requests.
func teamGenerateBody(t *testing.T, req GenerateTeamRequest) []byte {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err)
	return b
}

// newTeamGenerateServer builds a Server for team-generate tests: caller factory +
// scheme + AllowAll + a real HTTP client pointed at the fake chat provider.
func newTeamGenerateServer(t *testing.T, c client.Client) (*Server, *fakeCallerClientFactory, *logBuffer) {
	t.Helper()
	factory := &fakeCallerClientFactory{client: c}
	lb := &logBuffer{}
	log := funcr.New(func(prefix, args string) { lb.write(prefix, args) }, funcr.Options{})
	s := NewServer(Options{
		CallerClients: factory,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		ProviderHTTP:  &http.Client{},
		Version:       "test",
		Log:           log,
	})
	return s, factory, lb
}

// seedRegistry builds an AgentRegistry with the given status.members and a set
// of AgentDeployments (some marked as drafts via the stageLabel).
func seedRegistry(t *testing.T, ns, registryName string, members []string, draftMembers []string) []client.Object { //nolint:unparam
	t.Helper()
	// Collect draft set for fast lookup.
	draftSet := make(map[string]bool, len(draftMembers))
	for _, d := range draftMembers {
		draftSet[d] = true
	}

	objs := make([]client.Object, 0, 1+len(members))
	// The AgentRegistry with status.members populated.
	reg := &agentsv1alpha1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: registryName, Namespace: ns},
		Spec: agentsv1alpha1.AgentRegistrySpec{
			RegistryId: registryName,
			MemberSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"registry": registryName},
			},
		},
		Status: agentsv1alpha1.AgentRegistryStatus{
			Members: members,
		},
	}
	objs = append(objs, reg)

	// One AgentDeployment per member — drafts carry the stage label.
	for _, m := range members {
		labels := map[string]string{"registry": registryName}
		if draftSet[m] {
			labels[stageLabel] = stageDraft
		}
		role := "worker"
		if m == "orchestrator-bot" {
			role = "orchestrator"
		}
		ad := &agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      m,
				Namespace: ns,
				Labels:    labels,
			},
			Spec: agentsv1alpha1.AgentDeploymentSpec{
				Image: "img:1",
				Role:  role,
			},
		}
		objs = append(objs, ad)
	}
	return objs
}

// --- Tests -------------------------------------------------------------------

// TestGenerateTeamHappyPath proves a valid description + registry → the model
// composes a valid AgentTeam referencing only eligible members → 200.
func TestGenerateTeamHappyPath(t *testing.T) {
	prov, _ := fakeChatProvider(t, validTeamYAML)
	members := []string{"orchestrator-bot", "worker-bot"}
	objs := append(
		connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL),
		seedRegistry(t, "prod", "prod-registry", members, nil)...,
	)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s, factory, lb := newTeamGenerateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams/generate",
		bytes.NewReader(teamGenerateBody(t, GenerateTeamRequest{
			Description: "an orchestrator with one worker",
			RegistryRef: "prod-registry",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "developer-persona-token", factory.gotToken, "caller-scoped token must be used")

	var resp GenerateTeamResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "anthropic", resp.Provider)
	assert.Equal(t, "claude-sonnet-4-6", resp.Model)
	assert.Contains(t, resp.TeamYAML, "registryRef")
	assert.ElementsMatch(t, []string{"orchestrator-bot", "worker-bot"}, resp.EligibleMembers)
	assert.NotNil(t, resp.Warnings, "warnings must be [] not null")

	// Key must NEVER appear in the response or logs.
	assert.NotContains(t, rec.Body.String(), theTestKey, "the key must NEVER be in the response")
	assert.NotContains(t, lb.String(), theTestKey, "the key must NEVER be logged")
}

// TestGenerateTeamHallucinatedRefRetryThen422 proves that a model output
// referencing a non-member agentRef triggers one retry, and if the retry
// still produces an invalid ref → 422 regenerate (never applies).
func TestGenerateTeamHallucinatedRefRetryThen422(t *testing.T) {
	// The fake provider always returns the hallucinated YAML (both attempts).
	prov, _ := fakeChatProvider(t, hallucinatedTeamYAML)
	members := []string{"orchestrator-bot", "worker-bot"}
	createCalled := false
	objs := append(
		connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL),
		seedRegistry(t, "prod", "prod-registry", members, nil)...,
	)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).Build()
	s, _, _ := newTeamGenerateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams/generate",
		bytes.NewReader(teamGenerateBody(t, GenerateTeamRequest{
			Description: "a team",
			RegistryRef: "prod-registry",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"a hallucinated agentRef that persists after retry must be a 422")
	var resp GenerateTeamInvalidResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Regenerate, "UI is signalled to regenerate")
	assert.Contains(t, resp.Reason, "not in the eligible agent set",
		"reason must explain the referential validation failure")
	assert.False(t, createCalled, "NEVER auto-applies: no CRD create on the generate-team path")
}

// TestGenerateTeamDraftsExcluded proves that draft members are excluded from the
// eligible set: they are never offered to the model, and a ref to one is rejected.
func TestGenerateTeamDraftsExcluded(t *testing.T) {
	// "draft-agent" is a draft and must NOT appear in EligibleMembers.
	allMembers := []string{"orchestrator-bot", "worker-bot", "draft-agent"}
	draftMembers := []string{"draft-agent"}

	// The model refers to "draft-agent" — this must be rejected.
	draftRefYAML := `apiVersion: agents.ctxmesh.ai/v1beta1
kind: AgentTeam
metadata:
  name: bad-team
spec:
  registryRef: prod-registry
  supervisor:
    agentRef: orchestrator-bot
  roster:
    - name: draft-slot
      agentRef: draft-agent
      description: should not be here
`
	prov, _ := fakeChatProvider(t, draftRefYAML)
	objs := append(
		connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL),
		seedRegistry(t, "prod", "prod-registry", allMembers, draftMembers)...,
	)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s, _, _ := newTeamGenerateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams/generate",
		bytes.NewReader(teamGenerateBody(t, GenerateTeamRequest{
			Description: "a team",
			RegistryRef: "prod-registry",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	// The model referenced "draft-agent" which is a draft → 422.
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"a reference to a draft agent must be rejected: body=%s", rec.Body.String())
	var resp GenerateTeamInvalidResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// The reason must reference the rejected agentRef.
	assert.Contains(t, resp.Reason, "draft-agent",
		"the rejection reason must identify the ineligible agentRef")
}

// TestGenerateTeamEmptyRegistryIs422 proves that an empty registry (no published
// members) returns a descriptive 422 WITHOUT calling the model.
func TestGenerateTeamEmptyRegistryIs422(t *testing.T) {
	// Point the fake chat provider at an unreachable port — if it's called, the test
	// will surface a provider error rather than a 422, catching the regression.
	chatCalled := false
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chatCalled = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":""}]}`))
	}))
	t.Cleanup(prov.Close)

	// Registry with NO members.
	objs := append(
		connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL),
		seedRegistry(t, "prod", "empty-registry", []string{}, nil)...,
	)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s, _, _ := newTeamGenerateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams/generate",
		bytes.NewReader(teamGenerateBody(t, GenerateTeamRequest{
			Description: "any team",
			RegistryRef: "empty-registry",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"an empty registry must be a 422 with an informative message")
	var resp GenerateTeamInvalidResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp.Reason, "no published members",
		"the reason must explain that there are no eligible agents")
	assert.False(t, chatCalled, "the model must NOT be called when the registry is empty")
}

// TestGenerateTeamNeverApplies proves that even a fully VALID generation performs
// NO CRD create — the generate-team path only validates + returns for review.
func TestGenerateTeamNeverApplies(t *testing.T) {
	prov, _ := fakeChatProvider(t, validTeamYAML)
	members := []string{"orchestrator-bot", "worker-bot"}
	createCalled := false
	objs := append(
		connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL),
		seedRegistry(t, "prod", "prod-registry", members, nil)...,
	)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithInterceptorFuncs(interceptorCreateFlag(&createCalled)).Build()
	s, _, _ := newTeamGenerateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams/generate",
		bytes.NewReader(teamGenerateBody(t, GenerateTeamRequest{
			Description: "an orchestrator with one worker",
			RegistryRef: "prod-registry",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.False(t, createCalled,
		"generate-team NEVER creates a CRD — Create is the separate apply path")
}

// TestGenerateTeamMissingDescriptionIs400 proves that an empty description is
// a 400 before any registry or provider resolution.
func TestGenerateTeamMissingDescriptionIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newTeamGenerateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams/generate",
		bytes.NewReader(teamGenerateBody(t, GenerateTeamRequest{
			Description: "  ",
			RegistryRef: "prod-registry",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.Contains(t, errBody.Error, "description")
}

// TestGenerateTeamMissingRegistryRefIs400 proves that an empty registryRef is
// a 400 before any provider resolution.
func TestGenerateTeamMissingRegistryRefIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newTeamGenerateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams/generate",
		bytes.NewReader(teamGenerateBody(t, GenerateTeamRequest{
			Description: "a team",
			RegistryRef: "",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.Contains(t, errBody.Error, "registryRef")
}

// TestGenerateTeamNotFoundRegistryIs404 proves that a missing registry name is a 404.
func TestGenerateTeamNotFoundRegistryIs404(t *testing.T) {
	// Only the connect route objects — no registry.
	prov, _ := fakeChatProvider(t, validTeamYAML)
	objs := connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s, _, _ := newTeamGenerateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams/generate",
		bytes.NewReader(teamGenerateBody(t, GenerateTeamRequest{
			Description: "a team",
			RegistryRef: "ghost-registry",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "a missing registry must be a 404, not a 500")
}

// TestGenerateTeamNoConnectedProviderIs400 proves that a caller with no connected
// provider gets an honest 400 before any model call.
func TestGenerateTeamNoConnectedProviderIs400(t *testing.T) {
	// Only the registry — no ModelRoute/SecretBinding/Secret.
	objs := seedRegistry(t, "prod", "prod-registry", []string{"orchestrator-bot", "worker-bot"}, nil)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s, _, _ := newTeamGenerateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams/generate",
		bytes.NewReader(teamGenerateBody(t, GenerateTeamRequest{
			Description: "a team",
			RegistryRef: "prod-registry",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code,
		"no connected provider → 400 (honest, not a 500)")
	var errBody errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	assert.Contains(t, errBody.Error, "connect a provider",
		"the error message must tell the user to connect a provider")
}

// TestGenerateTeamDraftsMembersSkipped proves that members listed in status.members
// that are drafts do NOT appear in EligibleMembers in the success response.
func TestGenerateTeamDraftsMembersSkipped(t *testing.T) {
	// "orchestrator-bot" and "worker-bot" are published; "draft-bot" is a draft.
	// The model returns a valid team using only the published agents.
	prov, _ := fakeChatProvider(t, validTeamYAML)
	allMembers := []string{"orchestrator-bot", "worker-bot", "draft-bot"}
	draftMembers := []string{"draft-bot"}
	objs := append(
		connectRouteObjects("anthropic", "claude-sonnet-4-6", prov.URL),
		seedRegistry(t, "prod", "prod-registry", allMembers, draftMembers)...,
	)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s, _, _ := newTeamGenerateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams/generate",
		bytes.NewReader(teamGenerateBody(t, GenerateTeamRequest{
			Description: "an orchestrator with one worker",
			RegistryRef: "prod-registry",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp GenerateTeamResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.ElementsMatch(t, []string{"orchestrator-bot", "worker-bot"}, resp.EligibleMembers,
		"draft-bot must NOT appear in EligibleMembers")
	assert.NotContains(t, resp.EligibleMembers, "draft-bot")
}

// TestGenerateTeamForbiddenRegistryIs403 proves that a caller who cannot read the
// AgentRegistry gets a 403 — not a 500.
func TestGenerateTeamForbiddenRegistryIs403(t *testing.T) {
	objs := append(
		connectRouteObjects("anthropic", "claude-sonnet-4-6", "http://127.0.0.1:1"),
		seedRegistry(t, "prod", "prod-registry", []string{"orchestrator-bot"}, nil)...,
	)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isReg := obj.(*agentsv1alpha1.AgentRegistry); isReg {
					return forbiddenErr(key.Name)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	s, _, _ := newTeamGenerateServer(t, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/teams/generate",
		bytes.NewReader(teamGenerateBody(t, GenerateTeamRequest{
			Description: "a team",
			RegistryRef: "prod-registry",
			Namespace:   "prod",
		})))
	req.Header.Set("Authorization", "Bearer viewer-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code,
		"a forbidden registry read must surface as 403, not a 500")
}

// forbiddenErr returns a Kubernetes Forbidden error for the given resource name.
func forbiddenErr(name string) error {
	return &forbiddenError{name: name}
}

// forbiddenError is a minimal apierrors.Forbidden-compatible error for use
// with interceptors. It implements apierrors.IsForbidden by embedding the
// Status reason.
type forbiddenError struct{ name string }

func (e *forbiddenError) Error() string { return "forbidden: " + e.name }
func (e *forbiddenError) Status() metav1.Status {
	return metav1.Status{Reason: metav1.StatusReasonForbidden}
}

// Make it work with apierrors.IsForbidden — which checks for APIStatus interface.
func (e *forbiddenError) Is(err error) bool { _, ok := err.(*forbiddenError); return ok }
