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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/prompt"
)

// pvNS is the namespace used in PromptVersion tests.
const pvNS = "team-prompts"

// --- fixture helpers --------------------------------------------------------

// mockPromptVersion builds a minimal PromptVersion with a valid git pointer.
func mockPromptVersion(name, ns string) *agentsv1alpha1.PromptVersion {
	return &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.PromptVersionSpec{
			Git: agentsv1alpha1.GitPromptSource{
				Repo: "https://github.com/example/prompts.git",
				Ref:  "sha256:abc123",
				Path: "prompts/my-agent/system.txt",
			},
		},
	}
}

// mockPromptVersionWithConditions sets status conditions on a PromptVersion.
func mockPromptVersionWithConditions(pv *agentsv1alpha1.PromptVersion, ready bool) *agentsv1alpha1.PromptVersion {
	status := metav1.ConditionTrue
	reason := "Resolved"
	msg := "prompt content resolved successfully"
	if !ready {
		status = metav1.ConditionFalse
		reason = "ResolveFailed"
		msg = "ref not found in repository"
	}
	pv.Status.Conditions = []metav1.Condition{
		{
			Type:               "Ready",
			Status:             status,
			Reason:             reason,
			Message:            msg,
			LastTransitionTime: metav1.Now(),
		},
	}
	return pv
}

// --- request helpers --------------------------------------------------------

func getPromptVersions(t *testing.T, s *Server, rawQuery string) (PromptVersionListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	url := "/api/promptversions"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body PromptVersionListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

//nolint:unparam
func getPromptVersion(t *testing.T, s *Server, ns, name string) (*PromptVersionDetail, int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/promptversions/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail PromptVersionDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func createPromptVersion(t *testing.T, s *Server, reqBody PromptVersionCreateRequest) (*PromptVersionDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/promptversions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var detail PromptVersionDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

//nolint:unparam
func putPromptVersion(t *testing.T, s *Server, ns, name string, reqBody PromptVersionUpdateRequest) (*PromptVersionDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/promptversions/"+ns+"/"+name, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail PromptVersionDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func deletePromptVersion(t *testing.T, s *Server, ns, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/promptversions/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

//nolint:unparam
func getPromptVersionDiff(t *testing.T, s *Server, ns, name, fromName string) (*PromptVersionDiffResponse, int, string) {
	t.Helper()
	url := "/api/promptversions/" + ns + "/" + name + "/diff"
	if fromName != "" {
		url += "?from=" + fromName
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var body PromptVersionDiffResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return &body, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// newCallerServerWithResolver builds a Server with a prompt Resolver wired.
func newCallerServerWithResolver(t *testing.T, factory CallerClientFactory, resolver prompt.Resolver) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients:  factory,
		Scheme:         testScheme(t),
		Auth:           AllowAll{},
		Adapters:       Adapters{Expand: NewExpandAdapter()},
		Version:        "test",
		Log:            logr.Discard(),
		PromptResolver: resolver,
	})
}

// =============================================================================
// GET /api/promptversions — list contract
// =============================================================================

// TestListPromptVersionsEmpty proves an empty cluster yields [] not null.
func TestListPromptVersionsEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getPromptVersions(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items, "items must be [] not null")
	assert.Empty(t, body.Items)
	assert.Empty(t, body.NextCursor)
}

// TestListPromptVersionsReturnsItems proves seeded PromptVersions appear in the response.
func TestListPromptVersionsReturnsItems(t *testing.T) {
	objs := []client.Object{
		mockPromptVersion("pv-a", pvNS),
		mockPromptVersion("pv-b", pvNS),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getPromptVersions(t, s, "namespace="+pvNS)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
		assert.Equal(t, pvNS, item.Namespace)
		assert.Equal(t, "https://github.com/example/prompts.git", item.Git.Repo)
		assert.Equal(t, "sha256:abc123", item.Git.Ref)
		assert.Equal(t, "prompts/my-agent/system.txt", item.Git.Path)
	}
	assert.True(t, names["pv-a"])
	assert.True(t, names["pv-b"])
}

// TestListPromptVersionsQFilter proves ?q is a case-insensitive windowed substring filter.
func TestListPromptVersionsQFilter(t *testing.T) {
	objs := []client.Object{
		mockPromptVersion("prod-pv", pvNS),
		mockPromptVersion("PROD-staging-pv", pvNS),
		mockPromptVersion("dev-pv", pvNS),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getPromptVersions(t, s, "q=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
	}
	assert.True(t, names["prod-pv"])
	assert.True(t, names["PROD-staging-pv"])
	assert.False(t, names["dev-pv"])

	// No match → [] not null.
	body, code = getPromptVersions(t, s, "q=zzz-nomatch")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items)
	assert.Empty(t, body.Items)
}

// TestListPromptVersionsNamespaceScoping proves ?namespace scopes the list.
func TestListPromptVersionsNamespaceScoping(t *testing.T) {
	objs := []client.Object{
		mockPromptVersion("prod-pv", "prod"),
		mockPromptVersion("dev-pv", "dev"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getPromptVersions(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "prod", body.Items[0].Namespace)
}

// TestListPromptVersionsLimitAndCursor proves limit/cursor paging works.
func TestListPromptVersionsLimitAndCursor(t *testing.T) {
	all := []*agentsv1alpha1.PromptVersion{
		mockPromptVersion("pv-000", pvNS),
		mockPromptVersion("pv-001", pvNS),
		mockPromptVersion("pv-002", pvNS),
		mockPromptVersion("pv-003", pvNS),
		mockPromptVersion("pv-004", pvNS),
	}

	pagingFn := interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var lo client.ListOptions
			lo.ApplyOptions(opts)
			start := 0
			if lo.Continue != "" {
				n, err := strconv.Atoi(lo.Continue)
				if err != nil {
					return fmt.Errorf("bad continue token %q", lo.Continue)
				}
				start = n
			}
			end := len(all)
			if lo.Limit > 0 && start+int(lo.Limit) < end {
				end = start + int(lo.Limit)
			}
			pvList, ok := list.(*agentsv1alpha1.PromptVersionList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			for _, pv := range all[start:end] {
				pvList.Items = append(pvList.Items, *pv)
			}
			if end < len(all) {
				pvList.Continue = strconv.Itoa(end)
			}
			return nil
		},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(pagingFn).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	page1, code := getPromptVersions(t, s, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 2)
	require.NotEmpty(t, page1.NextCursor)

	seen := len(page1.Items)
	cursor := page1.NextCursor
	for cursor != "" {
		next, nextCode := getPromptVersions(t, s, "limit=2&cursor="+cursor)
		require.Equal(t, http.StatusOK, nextCode)
		seen += len(next.Items)
		cursor = next.NextCursor
	}
	assert.Equal(t, 5, seen, "paging must visit every prompt version exactly once")
}

// TestListPromptVersionsForbiddenIs403 proves a Forbidden on the list surfaces as 403.
func TestListPromptVersionsForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "promptversions"},
					"", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code := getPromptVersions(t, s, "")
	require.Equal(t, http.StatusForbidden, code)
}

// =============================================================================
// GET /api/promptversions/{ns}/{name} — detail
// =============================================================================

// TestGetPromptVersionReturnsDetail proves a seeded PromptVersion is returned with
// the correct git projection (repo/ref/path) and status conditions.
func TestGetPromptVersionReturnsDetail(t *testing.T) {
	pv := mockPromptVersionWithConditions(mockPromptVersion("my-pv", pvNS), true)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(pv).WithObjects(pv).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getPromptVersion(t, s, pvNS, "my-pv")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-pv", detail.Name)
	assert.Equal(t, pvNS, detail.Namespace)
	// Git pointer is projected faithfully.
	assert.Equal(t, "https://github.com/example/prompts.git", detail.Git.Repo)
	assert.Equal(t, "sha256:abc123", detail.Git.Ref)
	assert.Equal(t, "prompts/my-agent/system.txt", detail.Git.Path)
	// Conditions from status must be projected.
	assert.NotNil(t, detail.Conditions, "conditions must be [] not null")
	require.Len(t, detail.Conditions, 1)
	assert.Equal(t, "Ready", detail.Conditions[0].Type)
	assert.Equal(t, "True", detail.Conditions[0].Status)
}

// TestGetPromptVersionProjectsEmptyConditions proves conditions are [] not null
// when status.conditions has not been written yet.
func TestGetPromptVersionProjectsEmptyConditions(t *testing.T) {
	pv := mockPromptVersion("new-pv", pvNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pv).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getPromptVersion(t, s, pvNS, "new-pv")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	assert.NotNil(t, detail.Conditions, "conditions must be [] not null even when no controller run yet")
	assert.Empty(t, detail.Conditions)
}

// TestGetPromptVersionNotFoundIs404 proves a missing PromptVersion yields 404.
func TestGetPromptVersionNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getPromptVersion(t, s, pvNS, "ghost")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// TestGetPromptVersionForbiddenIs403 proves a caller denied Get sees 403.
func TestGetPromptVersionForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "promptversions"},
					"my-pv", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, _ := getPromptVersion(t, s, pvNS, "my-pv")
	assert.Equal(t, http.StatusForbidden, code)
}

// =============================================================================
// POST /api/promptversions — create
// =============================================================================

// TestCreatePromptVersionReturns201 proves a valid create request returns 201
// with the projected detail DTO.
func TestCreatePromptVersionReturns201(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := PromptVersionCreateRequest{
		Name:      "my-pv",
		Namespace: pvNS,
		Git: GitPromptSourceDTO{
			Repo: "https://github.com/example/prompts.git",
			Ref:  "v1.2.3",
			Path: "prompts/agent/system.txt",
		},
	}
	detail, code, body := createPromptVersion(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-pv", detail.Name)
	assert.Equal(t, pvNS, detail.Namespace)
	assert.Equal(t, "https://github.com/example/prompts.git", detail.Git.Repo)
	assert.Equal(t, "v1.2.3", detail.Git.Ref)
	assert.Equal(t, "prompts/agent/system.txt", detail.Git.Path)
	// Conditions must be [] not null even for a brand-new object.
	assert.NotNil(t, detail.Conditions)
}

// TestCreatePromptVersionDefaultsNamespace proves an empty namespace defaults to "default".
func TestCreatePromptVersionDefaultsNamespace(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := PromptVersionCreateRequest{
		Name: "no-ns-pv",
		Git: GitPromptSourceDTO{
			Repo: "https://github.com/example/prompts.git",
			Ref:  "sha:abc",
			Path: "prompts/system.txt",
		},
	}
	detail, code, _ := createPromptVersion(t, s, req)
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "default", detail.Namespace)
}

// TestCreatePromptVersionMissingNameIs400 proves missing name yields 400.
func TestCreatePromptVersionMissingNameIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := PromptVersionCreateRequest{
		Git: GitPromptSourceDTO{Repo: "r", Ref: "ref", Path: "p"},
	}
	_, code, body := createPromptVersion(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, "name is required")
}

// TestCreatePromptVersionMissingGitFieldsIs400 proves missing git fields yield 400.
func TestCreatePromptVersionMissingGitFieldsIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	tests := []struct {
		name string
		git  GitPromptSourceDTO
		want string
	}{
		{"missing repo", GitPromptSourceDTO{Ref: "ref", Path: "p"}, "git.repo"},
		{"missing ref", GitPromptSourceDTO{Repo: "r", Path: "p"}, "git.ref"},
		{"missing path", GitPromptSourceDTO{Repo: "r", Ref: "ref"}, "git.path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := PromptVersionCreateRequest{Name: "pv", Git: tc.git}
			_, code, body := createPromptVersion(t, s, req)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Contains(t, body, tc.want)
		})
	}
}

// TestCreatePromptVersionConflictIs409 proves a conflict (already exists) surfaces as 409.
func TestCreatePromptVersionConflictIs409(t *testing.T) {
	existing := mockPromptVersion("dup-pv", pvNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := PromptVersionCreateRequest{
		Name:      "dup-pv",
		Namespace: pvNS,
		Git:       GitPromptSourceDTO{Repo: "r", Ref: "ref", Path: "p"},
	}
	_, code, _ := createPromptVersion(t, s, req)
	assert.Equal(t, http.StatusConflict, code)
}

// TestCreatePromptVersionForbiddenIs403 proves a viewer's create returns 403.
func TestCreatePromptVersionForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "promptversions"},
					"pv", errors.New("viewer"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := PromptVersionCreateRequest{
		Name:      "pv",
		Namespace: pvNS,
		Git:       GitPromptSourceDTO{Repo: "r", Ref: "ref", Path: "p"},
	}
	_, code, _ := createPromptVersion(t, s, req)
	assert.Equal(t, http.StatusForbidden, code)
}

// TestCreatePromptVersionUnauthorizedIs401 proves a missing token yields 401 before
// any Kubernetes call.
func TestCreatePromptVersionUnauthorizedIs401(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(PromptVersionCreateRequest{
		Name:      "pv",
		Namespace: pvNS,
		Git:       GitPromptSourceDTO{Repo: "r", Ref: "ref", Path: "p"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/promptversions", bytes.NewReader(b))
	// No Authorization header.
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// =============================================================================
// PUT /api/promptversions/{ns}/{name} — SSA edit
// =============================================================================

// TestUpdatePromptVersionReturns200 proves a valid PUT returns 200 with the
// updated git projection.
func TestUpdatePromptVersionReturns200(t *testing.T) {
	pv := mockPromptVersion("my-pv", pvNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pv).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := PromptVersionUpdateRequest{
		Git: GitPromptSourceDTO{
			Repo: "https://github.com/example/prompts.git",
			Ref:  "v2.0.0",
			Path: "prompts/agent/system.txt",
		},
	}
	detail, code, body := putPromptVersion(t, s, pvNS, "my-pv", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-pv", detail.Name)
	// Updated ref is reflected.
	assert.Equal(t, "v2.0.0", detail.Git.Ref)
}

// TestUpdatePromptVersionRenameGuardIs400 proves a body name that mismatches the
// URL name yields 400 (rename guard).
func TestUpdatePromptVersionRenameGuardIs400(t *testing.T) {
	pv := mockPromptVersion("my-pv", pvNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pv).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := PromptVersionUpdateRequest{
		Name: "other-pv", // mismatches URL
		Git:  GitPromptSourceDTO{Repo: "r", Ref: "ref", Path: "p"},
	}
	_, code, body := putPromptVersion(t, s, pvNS, "my-pv", req)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, "rename is not supported")
}

// TestUpdatePromptVersionForbiddenIs403 proves a viewer's PUT returns the real 403
// from the API server (caller-scoped, ADR 0011).
func TestUpdatePromptVersionForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "promptversions"},
					"my-pv", errors.New("viewer"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := PromptVersionUpdateRequest{
		Git: GitPromptSourceDTO{Repo: "r", Ref: "ref", Path: "p"},
	}
	_, code, _ := putPromptVersion(t, s, pvNS, "my-pv", req)
	assert.Equal(t, http.StatusForbidden, code)
}

// =============================================================================
// DELETE /api/promptversions/{ns}/{name}
// =============================================================================

// TestDeletePromptVersionReturns204 proves a delete of an existing object returns 204.
func TestDeletePromptVersionReturns204(t *testing.T) {
	pv := mockPromptVersion("old-pv", pvNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pv).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deletePromptVersion(t, s, pvNS, "old-pv")
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// TestDeletePromptVersionNotFoundIs404 proves deleting a missing object yields 404.
func TestDeletePromptVersionNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deletePromptVersion(t, s, pvNS, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestDeletePromptVersionForbiddenIs403 proves a viewer's delete returns 403.
func TestDeletePromptVersionForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "promptversions"},
					"my-pv", errors.New("viewer"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deletePromptVersion(t, s, pvNS, "my-pv")
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// =============================================================================
// GET /api/promptversions/{ns}/{name}/diff — textual line diff
// =============================================================================

// TestPromptVersionDiffTextualReturns200 proves that when both PromptVersions
// exist and their git pointers resolve, the diff endpoint returns 200 with
// resolveMode:"textual", the correct from/to names, version identifiers, and a
// non-empty diff for different content.
func TestPromptVersionDiffTextualReturns200(t *testing.T) {
	fromSrc := agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  "v1.0.0",
		Path: "prompts/system.txt",
	}
	toSrc := agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  "v2.0.0",
		Path: "prompts/system.txt",
	}

	fromContent := "You are a helpful assistant.\nBe concise.\n"
	toContent := "You are a helpful assistant.\nBe concise and accurate.\nAlways cite sources.\n"

	resolver := prompt.NewFixtureResolver().
		Seed(fromSrc, fromContent).
		Seed(toSrc, toContent)

	fromPV := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-v1", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: fromSrc},
	}
	toPV := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-v2", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: toSrc},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(fromPV, toPV).Build()
	s := newCallerServerWithResolver(t, &fakeCallerClientFactory{client: c}, resolver)

	resp, code, body := getPromptVersionDiff(t, s, pvNS, "pv-v2", "pv-v1")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, resp)

	// HONEST RESOLVE-MODE: must always be "textual", not semantic/structural.
	assert.Equal(t, "textual", resp.ResolveMode,
		"resolveMode must be 'textual' — not a semantic diff")
	assert.Equal(t, "pv-v1", resp.FromName)
	assert.Equal(t, "pv-v2", resp.ToName)
	// Version identifiers must be non-empty (deterministic from the git pointer + content).
	assert.NotEmpty(t, resp.FromVersion)
	assert.NotEmpty(t, resp.ToVersion)
	// Different content → diff must be non-empty, identical must be false.
	assert.NotEmpty(t, resp.Diff, "diff must be non-empty when content differs")
	assert.False(t, resp.Identical)
	// Diff must contain the added line (prefixed with "+").
	assert.Contains(t, resp.Diff, "+Be concise and accurate.")
	assert.Contains(t, resp.Diff, "+Always cite sources.")
	// Deleted line must have "-" prefix.
	assert.Contains(t, resp.Diff, "-Be concise.")
}

// TestPromptVersionDiffIdenticalReturns200WithEmptyDiff proves that when both
// PromptVersions resolve to the same content, the response has identical:true
// and diff:"".
func TestPromptVersionDiffIdenticalReturns200WithEmptyDiff(t *testing.T) {
	sharedSrc := agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  "v1.0.0",
		Path: "prompts/system.txt",
	}
	content := "You are a helpful assistant.\n"

	resolver := prompt.NewFixtureResolver().Seed(sharedSrc, content)

	pv1 := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-copy-a", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: sharedSrc},
	}
	pv2 := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-copy-b", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: sharedSrc},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pv1, pv2).Build()
	s := newCallerServerWithResolver(t, &fakeCallerClientFactory{client: c}, resolver)

	resp, code, _ := getPromptVersionDiff(t, s, pvNS, "pv-copy-b", "pv-copy-a")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "textual", resp.ResolveMode)
	assert.True(t, resp.Identical, "identical must be true when content is the same")
	assert.Empty(t, resp.Diff, "diff must be empty when content is identical")
}

// TestPromptVersionDiffNoResolverIs501 proves that when no prompt Resolver is
// configured, the diff endpoint returns an honest 501 ("prompt resolution not
// configured") — never a fabricated diff.
func TestPromptVersionDiffNoResolverIs501(t *testing.T) {
	pv := mockPromptVersion("my-pv", pvNS)
	pv2 := mockPromptVersion("baseline-pv", pvNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pv, pv2).Build()

	// newCallerServer does NOT wire a PromptResolver (nil by default).
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getPromptVersionDiff(t, s, pvNS, "my-pv", "baseline-pv")
	assert.Equal(t, http.StatusNotImplemented, code,
		"diff must return 501 when no resolver is configured — not a fabricated diff")
	assert.Contains(t, body, "prompt resolution not configured")
}

// TestPromptVersionDiffFromNotFoundIs404 proves that when the "from" PromptVersion's
// git pointer does not resolve (ErrNotFound — bad ref / missing path), the diff
// endpoint returns an HONEST 404 ("prompt content not found"), distinct from a
// transient resolve failure (502). It NEVER fabricates a resolved template.
func TestPromptVersionDiffFromNotFoundIs404(t *testing.T) {
	fromSrc := agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  "dead-ref",
		Path: "prompts/missing.txt",
	}
	toSrc := agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  "v2.0.0",
		Path: "prompts/system.txt",
	}

	// The "from" pointer is seeded as not found; "to" resolves normally.
	resolver := prompt.NewFixtureResolver().
		SeedNotFound(fromSrc).
		Seed(toSrc, "good content")

	fromPV := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-ref-pv", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: fromSrc},
	}
	toPV := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "good-pv", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: toSrc},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(fromPV, toPV).Build()
	s := newCallerServerWithResolver(t, &fakeCallerClientFactory{client: c}, resolver)

	_, code, body := getPromptVersionDiff(t, s, pvNS, "good-pv", "bad-ref-pv")
	// NOT-FOUND is 404, not 502 (transient) — these are distinct honest reasons.
	assert.Equal(t, http.StatusNotFound, code,
		"ErrNotFound from resolver must yield 404 (bad ref / missing path), not 502")
	assert.Contains(t, body, "prompt content not found",
		"body must explain the not-found reason, not a generic error")
}

// TestPromptVersionDiffToNotFoundIs404 proves that when the "to" PromptVersion's
// git pointer does not resolve (ErrNotFound), the diff returns an honest 404.
func TestPromptVersionDiffToNotFoundIs404(t *testing.T) {
	fromSrc := agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  "v1.0.0",
		Path: "prompts/system.txt",
	}
	toSrc := agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  "dead-ref",
		Path: "prompts/missing.txt",
	}

	resolver := prompt.NewFixtureResolver().
		Seed(fromSrc, "baseline content").
		SeedNotFound(toSrc)

	fromPV := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "base-pv", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: fromSrc},
	}
	toPV := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-to-pv", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: toSrc},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(fromPV, toPV).Build()
	s := newCallerServerWithResolver(t, &fakeCallerClientFactory{client: c}, resolver)

	_, code, body := getPromptVersionDiff(t, s, pvNS, "bad-to-pv", "base-pv")
	assert.Equal(t, http.StatusNotFound, code,
		"ErrNotFound from the 'to' resolver must also yield 404, not 502")
	assert.Contains(t, body, "prompt content not found")
}

// TestPromptVersionDiffTransientErrorIs502 proves that a transient / infra resolve
// failure (any error that is NOT ErrNotFound) yields an honest 502 ("resolve failed"),
// distinct from the 404 returned for ErrNotFound. The resolver must NEVER be
// allowed to silently return empty / fabricated content on a transient error.
func TestPromptVersionDiffTransientErrorIs502(t *testing.T) {
	fromSrc := agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  "v1.0.0",
		Path: "prompts/system.txt",
	}
	toSrc := agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  "v2.0.0",
		Path: "prompts/system.txt",
	}

	// errTransient is not ErrNotFound — it is a transient/infra failure (e.g.
	// network error, git remote unavailable).
	errTransient := errors.New("git remote: connection refused")

	// brokenResolver always returns a transient error (never ErrNotFound).
	brokenResolver := &alwaysErrorResolver{err: errTransient}

	fromPV := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "from-pv", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: fromSrc},
	}
	toPV := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "to-pv", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: toSrc},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(fromPV, toPV).Build()
	s := newCallerServerWithResolver(t, &fakeCallerClientFactory{client: c}, brokenResolver)

	_, code, body := getPromptVersionDiff(t, s, pvNS, "to-pv", "from-pv")
	// TRANSIENT error → 502 (not 404, not 500, not a fabricated 200).
	assert.Equal(t, http.StatusBadGateway, code,
		"transient resolve error must yield 502, distinct from ErrNotFound → 404")
	assert.Contains(t, body, "could not be fetched",
		"body must explain the transient failure reason")
}

// TestPromptVersionDiffMissingFromParamIs400 proves missing ?from= yields 400.
func TestPromptVersionDiffMissingFromParamIs400(t *testing.T) {
	pv := mockPromptVersion("my-pv", pvNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pv).Build()
	resolver := prompt.NewFixtureResolver()
	s := newCallerServerWithResolver(t, &fakeCallerClientFactory{client: c}, resolver)

	_, code, body := getPromptVersionDiff(t, s, pvNS, "my-pv", "") // no from= param
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, "from")
}

// TestPromptVersionDiffFromObjectNotFoundIs404 proves a missing "from" PromptVersion
// object (the K8s object doesn't exist, distinct from the pointer not resolving) yields 404.
func TestPromptVersionDiffFromObjectNotFoundIs404(t *testing.T) {
	toPV := mockPromptVersion("to-pv", pvNS)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(toPV).Build()
	resolver := prompt.NewFixtureResolver()
	s := newCallerServerWithResolver(t, &fakeCallerClientFactory{client: c}, resolver)

	// "ghost-pv" does not exist in the fake client.
	_, code, body := getPromptVersionDiff(t, s, pvNS, "to-pv", "ghost-pv")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// =============================================================================
// computeTextualLineDiff — unit tests for the internal diff engine
// =============================================================================

// TestComputeTextualLineDiffIdentical proves identical strings return "".
func TestComputeTextualLineDiffIdentical(t *testing.T) {
	result := computeTextualLineDiff("same content\n", "same content\n")
	assert.Empty(t, result)
}

// TestComputeTextualLineDiffAddedLine proves a line added in "to" is prefixed with "+".
func TestComputeTextualLineDiffAddedLine(t *testing.T) {
	from := "line one\n"
	to := "line one\nnew line\n"
	result := computeTextualLineDiff(from, to)
	assert.Contains(t, result, "+new line")
	assert.Contains(t, result, " line one")
}

// TestComputeTextualLineDiffDeletedLine proves a line removed in "to" is prefixed with "-".
func TestComputeTextualLineDiffDeletedLine(t *testing.T) {
	from := "line one\nline two\n"
	to := "line one\n"
	result := computeTextualLineDiff(from, to)
	assert.Contains(t, result, "-line two")
	assert.Contains(t, result, " line one")
}

// TestComputeTextualLineDiffEmptyInputs proves diffs against empty strings work.
func TestComputeTextualLineDiffEmptyInputs(t *testing.T) {
	// from empty: everything is an insertion.
	result := computeTextualLineDiff("", "hello\nworld\n")
	assert.Contains(t, result, "+hello")
	assert.Contains(t, result, "+world")

	// to empty: everything is a deletion.
	result = computeTextualLineDiff("hello\nworld\n", "")
	assert.Contains(t, result, "-hello")
	assert.Contains(t, result, "-world")
}

// --- helpers for transient-error test ----------------------------------------

// alwaysErrorResolver is a test-only Resolver that returns a fixed error for
// every Resolve call — used to exercise the transient-error → 502 path without
// needing to corrupt the FixtureResolver's not-found behaviour.
type alwaysErrorResolver struct {
	err error
}

func (r *alwaysErrorResolver) Resolve(_ context.Context, _ agentsv1alpha1.GitPromptSource) (prompt.Resolved, error) {
	return prompt.Resolved{}, r.err
}

// TestPromptVersionDiffResolveModeIsAlwaysTextual is a smoke test that the
// resolveMode field in any 200 response is always "textual" and never something
// else like "semantic" or "structural".
func TestPromptVersionDiffResolveModeIsAlwaysTextual(t *testing.T) {
	src := agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  "v1.0.0",
		Path: "prompts/a.txt",
	}
	src2 := agentsv1alpha1.GitPromptSource{
		Repo: "https://github.com/example/prompts.git",
		Ref:  "v2.0.0",
		Path: "prompts/a.txt",
	}

	resolver := prompt.NewFixtureResolver().
		Seed(src, "content v1").
		Seed(src2, "content v2")

	pv1 := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "pv1", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: src},
	}
	pv2 := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "pv2", Namespace: pvNS},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: src2},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pv1, pv2).Build()
	s := newCallerServerWithResolver(t, &fakeCallerClientFactory{client: c}, resolver)

	// Parse the raw JSON and check the resolveMode field directly.
	rec := httptest.NewRecorder()
	reqURL := "/api/promptversions/" + pvNS + "/pv2/diff?from=pv1"
	httpReq := httptest.NewRequest(http.MethodGet, reqURL, nil)
	httpReq.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, httpReq)
	require.Equal(t, http.StatusOK, rec.Code)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, "textual", raw["resolveMode"],
		"resolveMode MUST be 'textual' in every diff response — never 'semantic' or 'structural'")

	// Also verify the diff contains some line-diff markers.
	diffStr, _ := raw["diff"].(string)
	assert.True(t,
		strings.Contains(diffStr, "+") || strings.Contains(diffStr, "-") || strings.Contains(diffStr, " "),
		"diff must contain line-diff prefixes (+/-/ )")
}
