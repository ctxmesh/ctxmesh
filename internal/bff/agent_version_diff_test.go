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
// version snapshots rendered as canonical YAML — and for the detail page's version list. Version
// history is in the control-plane store (ADR 0042), so the caller's access is an SSAR on
// `agentversions`.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentversion"
)

const echoUID = types.UID("echo-uid")

// echoAgent is the agent every diff test reads first.
func echoAgent() *agentsv1beta1.AgentDeployment {
	return &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: detailNS, UID: echoUID},
		Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "img:2"},
	}
}

// recordVersion records a version of agent (uid) with the given image, in call order.
func recordVersion(t *testing.T, s agentversion.Store, agent string, uid types.UID, name, image string) {
	t.Helper()
	_, err := s.Record(context.Background(), agentversion.Version{
		Namespace: detailNS, Agent: agent, AgentUID: string(uid), Name: name,
		Snapshot: agentsv1beta1.AgentDeploymentSpec{Image: image},
	})
	require.NoError(t, err)
}

// versionServer is a BFF over store whose caller holds the objects and answers each SSAR with allow.
func versionServer(t *testing.T, store agentversion.Store, allow func(resource, verb, namespace string) bool, objs ...client.Object) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithInterceptorFuncs(ssarInterceptor(allow)).Build()
	return NewServer(Options{
		CallerClients:     &fakeCallerClientFactory{client: c},
		Scheme:            testScheme(t),
		Auth:              AllowAll{},
		Adapters:          Adapters{Expand: NewExpandAdapter()},
		AgentVersionStore: store,
		Version:           "test",
		Log:               logr.Discard(),
	})
}

func allowEveryReview(_, _, _ string) bool { return true }

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
	store := agentversion.NewMemStore()
	recordVersion(t, store, "echo", echoUID, "echo-v1", "img:1")
	recordVersion(t, store, "echo", echoUID, "echo-v2", "img:2")
	s := versionServer(t, store, allowEveryReview, echoAgent())

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
	store := agentversion.NewMemStore()
	recordVersion(t, store, "echo", echoUID, "echo-v1", "img:1")
	recordVersion(t, store, "echo", echoUID, "echo-v1b", "img:1")
	s := versionServer(t, store, allowEveryReview, echoAgent())

	rec := versionDiffReq(t, s, "echo-v1", "echo-v1b")
	require.Equal(t, http.StatusOK, rec.Code)
	var got AgentVersionDiffResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.Identical)
	assert.Empty(t, got.Diff)
}

// A missing from/to query param → 400.
func TestAgentVersionDiff_MissingParam(t *testing.T) {
	store := agentversion.NewMemStore()
	recordVersion(t, store, "echo", echoUID, "echo-v1", "img:1")
	s := versionServer(t, store, allowEveryReview, echoAgent())

	rec := versionDiffReq(t, s, "", "echo-v1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = versionDiffReq(t, s, "echo-v1", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// A version that does not exist → 404, and so does a version of an earlier agent of the same name:
// a recreated agent does not inherit its predecessor's history.
func TestAgentVersionDiff_VersionNotFound(t *testing.T) {
	store := agentversion.NewMemStore()
	recordVersion(t, store, "echo", echoUID, "echo-v1", "img:1")
	recordVersion(t, store, "echo", "echo-previous-uid", "echo-old", "img:0")
	s := versionServer(t, store, allowEveryReview, echoAgent())

	rec := versionDiffReq(t, s, "echo-v1", "echo-missing")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "the to version not found")
	rec = versionDiffReq(t, s, "echo-old", "echo-v1")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "the from version not found")
}

// A version of a DIFFERENT agent → 400 (a cross-agent diff is meaningless).
func TestAgentVersionDiff_CrossAgentIs400(t *testing.T) {
	store := agentversion.NewMemStore()
	recordVersion(t, store, "echo", echoUID, "echo-v1", "img:1")
	recordVersion(t, store, "other", "other-uid", "other-v1", "img:9")
	s := versionServer(t, store, allowEveryReview, echoAgent())

	rec := versionDiffReq(t, s, "echo-v1", "other-v1")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "does not belong to agent")
}

// The agent is read first, through the caller: an agent that does not exist is a 404 before any
// version is looked at.
func TestAgentVersionDiff_AgentNotFound(t *testing.T) {
	store := agentversion.NewMemStore()
	recordVersion(t, store, "echo", echoUID, "echo-v1", "img:1")
	recordVersion(t, store, "echo", echoUID, "echo-v2", "img:2")
	s := versionServer(t, store, allowEveryReview) // no agent

	rec := versionDiffReq(t, s, "echo-v1", "echo-v2")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "agent not found")
}

// A caller who may read the agent but not its versions gets a 403, not the snapshots: the store
// holds no RBAC of its own, so the SSAR is the only gate.
func TestAgentVersionDiff_WithoutTheGrantIs403(t *testing.T) {
	store := agentversion.NewMemStore()
	recordVersion(t, store, "echo", echoUID, "echo-v1", "img:1")
	recordVersion(t, store, "echo", echoUID, "echo-v2", "img:2")
	var asked []string
	s := versionServer(t, store, func(resource, verb, namespace string) bool {
		asked = append(asked, verb+" "+resource+" "+namespace)
		return resource != resourceAgentVersions
	}, echoAgent())

	rec := versionDiffReq(t, s, "echo-v1", "echo-v2")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.NotContains(t, rec.Body.String(), "img:")
	assert.Contains(t, asked, "get agentversions "+detailNS, "the version read is authorized as the caller")
}

// The SSAR names the version: an RBAC rule with resourceNames governs which versions a caller sees.
func TestAgentVersionDiff_AuthorizesEachVersionByName(t *testing.T) {
	store := agentversion.NewMemStore()
	recordVersion(t, store, "echo", echoUID, "echo-v1", "img:1")
	recordVersion(t, store, "echo", echoUID, "echo-v2", "img:2")
	var names []string
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(echoAgent()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				ssar, ok := obj.(*authzv1.SelfSubjectAccessReview)
				if !ok {
					return errors.New("unexpected create")
				}
				names = append(names, ssar.Spec.ResourceAttributes.Name)
				ssar.Status.Allowed = ssar.Spec.ResourceAttributes.Name == "echo-v1"
				return nil
			},
		}).Build()
	s := NewServer(Options{
		CallerClients: &fakeCallerClientFactory{client: c}, Scheme: testScheme(t), Auth: AllowAll{},
		Adapters: Adapters{Expand: NewExpandAdapter()}, AgentVersionStore: store, Version: "test", Log: logr.Discard(),
	})

	rec := versionDiffReq(t, s, "echo-v1", "echo-v2")
	assert.Equal(t, http.StatusForbidden, rec.Code, "echo-v2 is not granted")
	assert.Equal(t, []string{"echo-v1", "echo-v2"}, names)
}

// No control-plane store → an honest 501, not a 404 that says the versions do not exist.
func TestAgentVersionDiff_NoStoreIs501(t *testing.T) {
	s := versionServer(t, nil, allowEveryReview, echoAgent())
	rec := versionDiffReq(t, s, "echo-v1", "echo-v2")
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// The detail page lists the agent's versions newest first: the console's default compare is the
// two newest. A version recorded again keeps its first place.
func TestAgentDetail_VersionsNewestFirst(t *testing.T) {
	store := agentversion.NewMemStore()
	// Names sort opposite to recording order, so a name-ordered list would fail this test.
	recordVersion(t, store, "echo", echoUID, "echo-c", "img:1")
	recordVersion(t, store, "echo", echoUID, "echo-b", "img:2")
	recordVersion(t, store, "echo", echoUID, "echo-c", "img:1")
	recordVersion(t, store, "echo", echoUID, "echo-a", "img:3")
	recordVersion(t, store, "echo", "echo-previous-uid", "echo-z", "img:0")
	recordVersion(t, store, "other", "other-uid", "other-v1", "img:9")
	s := versionServer(t, store, allowEveryReview, echoAgent())

	rec := getDetail(t, s, "echo")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, []string{"echo-a", "echo-b", "echo-c"}, got.Versions)
}

// A caller without `list agentversions` still reads the agent; its version list is empty, as a
// forbidden binding list is.
func TestAgentDetail_VersionsWithoutTheGrantAreEmpty(t *testing.T) {
	store := agentversion.NewMemStore()
	recordVersion(t, store, "echo", echoUID, "echo-v1", "img:1")
	var asked []string
	s := versionServer(t, store, func(resource, verb, namespace string) bool {
		asked = append(asked, verb+" "+resource+" "+namespace)
		return resource != resourceAgentVersions
	}, echoAgent())

	rec := getDetail(t, s, "echo")
	require.Equal(t, http.StatusOK, rec.Code)
	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.NotNil(t, got.Versions)
	assert.Empty(t, got.Versions)
	assert.Contains(t, asked, "list agentversions "+detailNS)
}

// An authorization check that fails (not a denial) is a 500, never an empty list that reads as
// "this agent has no history".
func TestAgentDetail_VersionAuthzErrorIs500(t *testing.T) {
	store := agentversion.NewMemStore()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(echoAgent()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return errors.New("api server unavailable")
			},
		}).Build()
	s := NewServer(Options{
		CallerClients: &fakeCallerClientFactory{client: c}, Scheme: testScheme(t), Auth: AllowAll{},
		Adapters: Adapters{Expand: NewExpandAdapter()}, AgentVersionStore: store, Version: "test", Log: logr.Discard(),
	})
	rec := getDetail(t, s, "echo")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
