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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

func usedByReq(t *testing.T, s *Server, kind, name string) UsedByResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/usedby?kind="+kind+"&namespace=default&name="+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var out UsedByResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// TestUsedBy_ByPromptAndRoute proves the reverse-lookup returns the agents that
// reference a PromptVersion (spec.promptRef) or a ModelRoute (MODEL_ROUTE env) —
// and not the agents that don't (m18.8).
func TestUsedBy_ByPromptAndRoute(t *testing.T) {
	usesPrompt := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "a-prompt", Namespace: "default"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "i:1", PromptRef: "sys-v2"},
	}
	usesRoute := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "a-route", Namespace: "default"},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "i:1",
			Env:   []corev1.EnvVar{{Name: "MODEL_ROUTE", Value: "anthropic"}},
		},
	}
	unrelated := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "a-none", Namespace: "default"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "i:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(usesPrompt, usesRoute, unrelated).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	byPrompt := usedByReq(t, s, "promptversion", "sys-v2")
	require.Len(t, byPrompt.Items, 1)
	assert.Equal(t, "a-prompt", byPrompt.Items[0].Name)
	assert.Equal(t, "AgentDeployment", byPrompt.Items[0].Kind)

	byRoute := usedByReq(t, s, "modelroute", "anthropic")
	require.Len(t, byRoute.Items, 1)
	assert.Equal(t, "a-route", byRoute.Items[0].Name)

	none := usedByReq(t, s, "promptversion", "nonexistent")
	assert.Empty(t, none.Items, "no matches → [] not null")
}

// TestUsedBy_BySecretBinding proves it returns the ModelRoutes referencing a
// SecretBinding.
func TestUsedBy_BySecretBinding(t *testing.T) {
	route := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "default"},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{
				Provider: "anthropic", Model: "m", Priority: 1, SecretBindingRef: "anthropic-key",
			}},
		},
	}
	other := &agentsv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "mock", Namespace: "default"},
		Spec: agentsv1alpha1.ModelRouteSpec{
			Providers: []agentsv1alpha1.ProviderRef{{Provider: "mock", Model: "m", Priority: 1}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(route, other).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	got := usedByReq(t, s, "secretbinding", "anthropic-key")
	require.Len(t, got.Items, 1)
	assert.Equal(t, "anthropic", got.Items[0].Name)
	assert.Equal(t, "ModelRoute", got.Items[0].Kind)
}

// TestUsedBy_BadKindOrName proves the honest 400s.
func TestUsedBy_BadKindOrName(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	for _, path := range []string{
		"/api/usedby?kind=bogus&name=x&namespace=default",
		"/api/usedby?kind=modelroute&namespace=default", // no name
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer caller-token")
		s.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "path: %s", path)
	}
}
