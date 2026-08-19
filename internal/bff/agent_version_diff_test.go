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

// Tests for GET /api/agents/{ns}/{name}/versions/diff (V3, m101.7) — a read-only textual diff of two
// AgentVersion snapshots rendered as canonical YAML.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

func av(name, deployment, image string) *agentsv1alpha1.AgentVersion {
	return &agentsv1alpha1.AgentVersion{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: detailNS},
		Spec: agentsv1alpha1.AgentVersionSpec{
			DeploymentName: deployment,
			Snapshot:       agentsv1alpha1.AgentDeploymentSpec{Image: image},
		},
	}
}

func versionDiffReq(t *testing.T, s *Server, from, to string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	// The path agent is always "echo" across these tests (cross-agent is exercised via a foreign version).
	url := "/api/agents/" + detailNS + "/echo/versions/diff?from=" + from + "&to=" + to
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// A real diff between two versions of the same agent — non-empty unified diff, not identical.
func TestAgentVersionDiff_RealDiff(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(av("echo-v1", "echo", "img:1"), av("echo-v2", "echo", "img:2")).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := versionDiffReq(t, s, "echo-v1", "echo-v2")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got AgentVersionDiffResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "textual", got.ResolveMode)
	assert.Equal(t, "echo-v1", got.FromName)
	assert.Equal(t, "echo-v2", got.ToName)
	assert.False(t, got.Identical)
	// The image change shows as a -/+ pair in the YAML line diff.
	assert.Contains(t, got.Diff, "-image: img:1")
	assert.Contains(t, got.Diff, "+image: img:2")
}

// Two identical snapshots → identical=true, empty diff (a calm "no changes" state, not an error).
func TestAgentVersionDiff_Identical(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(av("echo-v1", "echo", "img:1"), av("echo-v1b", "echo", "img:1")).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := versionDiffReq(t, s, "echo-v1", "echo-v1b")
	require.Equal(t, http.StatusOK, rec.Code)
	var got AgentVersionDiffResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.Identical)
	assert.Empty(t, got.Diff)
}

// A missing from/to query param → 400.
func TestAgentVersionDiff_MissingParam(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(av("echo-v1", "echo", "img:1")).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := versionDiffReq(t, s, "", "echo-v1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = versionDiffReq(t, s, "echo-v1", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// A version that does not exist → 404.
func TestAgentVersionDiff_VersionNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(av("echo-v1", "echo", "img:1")).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := versionDiffReq(t, s, "echo-v1", "echo-missing")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// A version whose deploymentName is a DIFFERENT agent → 400 (a cross-agent diff is meaningless).
func TestAgentVersionDiff_CrossAgentIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(av("echo-v1", "echo", "img:1"), av("other-v1", "other", "img:9")).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := versionDiffReq(t, s, "echo-v1", "other-v1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "does not belong to agent")
}
