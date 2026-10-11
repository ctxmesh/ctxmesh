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
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// mtbNS is the namespace used in MCPToolBinding tests.
const mtbNS = "team-bindings"

// --- fixture helpers --------------------------------------------------------

// mockMCPToolBinding builds a minimal MCPToolBinding (sidecar mode).
func mockMCPToolBinding(name, ns, agentRef, toolName string) *agentsv1beta1.MCPToolBinding {
	return &agentsv1beta1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1beta1.MCPToolBindingSpec{
			AgentRef:    agentRef,
			RegistryRef: "default-registry",
			ToolName:    toolName,
			Mode:        "sidecar",
			Server: agentsv1beta1.ToolServer{
				Image: "ghcr.io/example/mcp-search:latest",
			},
		},
	}
}

// withReadyCondition sets Ready=True on an MCPToolBinding, simulating that the
// controller has registered, pin-matched, rendered, and hot-updated the tool.
// Ready=True IS the hot-update propagation signal.
func withReadyCondition(b *agentsv1beta1.MCPToolBinding) *agentsv1beta1.MCPToolBinding {
	b.Status.Conditions = []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "ToolHotUpdated",
			Message:            "tool registered, pin-matched, rendered and pushed to discovery sidecar",
			LastTransitionTime: metav1.Now(),
		},
	}
	return b
}

// withFailedCondition sets Ready=False with a specific reason, simulating a
// binding that failed controller reconciliation.
func withFailedCondition(b *agentsv1beta1.MCPToolBinding, reason string) *agentsv1beta1.MCPToolBinding {
	b.Status.Conditions = []metav1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            "binding failed: " + reason,
			LastTransitionTime: metav1.Now(),
		},
	}
	return b
}

// --- request helpers --------------------------------------------------------

func getMCPToolBindings(t *testing.T, s *Server, rawQuery string) (MCPToolBindingListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	url := "/api/mcptoolbindings"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body MCPToolBindingListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

func getMCPToolBinding(t *testing.T, s *Server, name string) (*MCPToolBindingDetail, int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcptoolbindings/"+mtbNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail MCPToolBindingDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func getBindingIn(t *testing.T, s *Server, ns, name string) (*MCPToolBindingDetail, int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcptoolbindings/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, rec.Code, rec.Body.String()
	}
	var detail MCPToolBindingDetail
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	return &detail, rec.Code, rec.Body.String()
}

func createMCPToolBinding(t *testing.T, s *Server, reqBody MCPToolBindingCreateRequest) (*MCPToolBindingDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcptoolbindings", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var detail MCPToolBindingDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

//nolint:unparam
func putMCPToolBinding(t *testing.T, s *Server, name string, reqBody MCPToolBindingUpdateRequest) (*MCPToolBindingDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/mcptoolbindings/"+mtbNS+"/"+name, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail MCPToolBindingDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func deleteMCPToolBinding(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/mcptoolbindings/"+mtbNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// mtbUser is who the explicit-spec binding tests bind as.
const mtbUser = "binder@example.com"

// asUser answers the SelfSubjectReview as mtbUser and hands every other create to next.Create, or to
// the fake store when next has none.
func asUser(next interceptor.Funcs) interceptor.Funcs {
	create := next.Create
	next.Create = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if ssr, ok := obj.(*authnv1.SelfSubjectReview); ok {
			ssr.Status.UserInfo = authnv1.UserInfo{Username: mtbUser}
			return nil
		}
		if create != nil {
			return create(ctx, c, obj, opts...)
		}
		return c.Create(ctx, obj, opts...)
	}
	return next
}

// explicitBindingServer serves the binding routes with a team MCP server "my-registry" in mtbNS that
// provides every tool these tests bind, and every authorization allowed.
func explicitBindingServer(t *testing.T, factory CallerClientFactory) *Server {
	t.Helper()
	s := newCallerServer(t, factory)
	store := toolregistry.NewMemStore()
	_, err := store.Upsert(context.Background(), toolregistry.ToolRegistry{
		Namespace: mtbNS, Name: "my-registry",
		Tools: []toolregistry.ToolEntry{
			{Name: "search", Image: "img:v1"}, {Name: "search-updated", Image: "img:v1"}, {Name: "web-search", Image: "ghcr.io/example/search:v1"},
		},
		Labels: map[string]string{labelManagedBy: managedByMCP, labelMCPVisibility: visibilityTeam},
	})
	require.NoError(t, err)
	s.toolRegistryStore = store
	s.authorizer = &recordingAuthorizer{}
	return s
}

// =============================================================================
// GET /api/mcptoolbindings — list contract
// =============================================================================

// TestListMCPToolBindingsEmpty proves an empty cluster yields [] not null.
func TestListMCPToolBindingsEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getMCPToolBindings(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.NotNil(t, body.Items, "items must be [] not null")
	assert.Empty(t, body.Items)
	assert.Empty(t, body.NextCursor)
}

// TestListMCPToolBindingsReturnsItems proves seeded bindings appear with correct
// projections including propagationStatus.
func TestListMCPToolBindingsReturnsItems(t *testing.T) {
	objs := []client.Object{
		withReadyCondition(mockMCPToolBinding("binding-a", mtbNS, "agent-a", "search")),
		withFailedCondition(mockMCPToolBinding("binding-b", mtbNS, "agent-b", "code-exec"), "UnregisteredTool"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getMCPToolBindings(t, s, "namespace="+mtbNS)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)

	byName := map[string]MCPToolBindingSummary{}
	for _, item := range body.Items {
		byName[item.Name] = item
	}

	// binding-a: Ready=True → "propagated"
	a := byName["binding-a"]
	assert.Equal(t, propagationStatePropagated, a.PropagationStatus,
		"Ready=True must yield propagationStatus=propagated")
	assert.True(t, a.Ready)

	// binding-b: Ready=False(UnregisteredTool) → "UnregisteredTool"
	b := byName["binding-b"]
	assert.Equal(t, "UnregisteredTool", b.PropagationStatus,
		"Ready=False(reason) must yield propagationStatus=reason")
	assert.False(t, b.Ready)
}

// TestListMCPToolBindingsQFilter proves ?q is a case-insensitive windowed filter.
func TestListMCPToolBindingsQFilter(t *testing.T) {
	objs := []client.Object{
		mockMCPToolBinding("prod-search", mtbNS, "agent-a", "search"),
		mockMCPToolBinding("PROD-code", mtbNS, "agent-b", "code-exec"),
		mockMCPToolBinding("dev-search", mtbNS, "agent-c", "search"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getMCPToolBindings(t, s, "q=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 2)
	names := map[string]bool{}
	for _, item := range body.Items {
		names[item.Name] = true
	}
	assert.True(t, names["prod-search"])
	assert.True(t, names["PROD-code"])
	assert.False(t, names["dev-search"])
}

// TestListMCPToolBindingsNamespaceScoping proves ?namespace scopes the list.
func TestListMCPToolBindingsNamespaceScoping(t *testing.T) {
	objs := []client.Object{
		mockMCPToolBinding("prod-b", "prod", "agent-a", "search"),
		mockMCPToolBinding("dev-b", "dev", "agent-b", "search"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	body, code := getMCPToolBindings(t, s, "namespace=prod")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "prod", body.Items[0].Namespace)

	body, code = getMCPToolBindings(t, s, "")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, body.Items, 2)
}

// TestListMCPToolBindingsLimitAndCursor proves limit/cursor paging works.
func TestListMCPToolBindingsLimitAndCursor(t *testing.T) {
	all := []*agentsv1beta1.MCPToolBinding{
		mockMCPToolBinding("b-000", mtbNS, "agent-a", "search"),
		mockMCPToolBinding("b-001", mtbNS, "agent-b", "search"),
		mockMCPToolBinding("b-002", mtbNS, "agent-c", "search"),
		mockMCPToolBinding("b-003", mtbNS, "agent-d", "search"),
		mockMCPToolBinding("b-004", mtbNS, "agent-e", "search"),
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
			bList, ok := list.(*agentsv1beta1.MCPToolBindingList)
			if !ok {
				return fmt.Errorf("unexpected list type %T", list)
			}
			for _, b := range all[start:end] {
				bList.Items = append(bList.Items, *b)
			}
			if end < len(all) {
				bList.Continue = strconv.Itoa(end)
			}
			return nil
		},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(pagingFn).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	page1, code := getMCPToolBindings(t, s, "limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 2)
	require.NotEmpty(t, page1.NextCursor)

	page2, code := getMCPToolBindings(t, s, "limit=2&cursor="+page1.NextCursor)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page2.Items, 2)
	assert.NotEqual(t, page1.Items[0].Name, page2.Items[0].Name)

	seen := len(page1.Items) + len(page2.Items)
	cursor := page2.NextCursor
	for cursor != "" {
		next, code := getMCPToolBindings(t, s, "limit=2&cursor="+cursor)
		require.Equal(t, http.StatusOK, code)
		seen += len(next.Items)
		cursor = next.NextCursor
	}
	assert.Equal(t, 5, seen, "paging must visit every binding exactly once")
}

// TestListMCPToolBindingsForbiddenIs403 proves a Forbidden on the list surfaces
// as 403, not an empty [].
func TestListMCPToolBindingsForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "mcptoolbindings"},
					"", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code := getMCPToolBindings(t, s, "")
	require.Equal(t, http.StatusForbidden, code)
}

// =============================================================================
// GET /api/mcptoolbindings/{ns}/{name} — detail + propagation status
// =============================================================================

// TestGetMCPToolBindingPropagatedWhenReadyTrue is THE PROPAGATION STATUS TEST
// for the Ready=True case. It proves that a binding whose controller has set
// Ready=True reports propagationStatus="propagated" (the tool is hot-updated
// live in the discovery sidecar). This is the HONEST positive case.
func TestGetMCPToolBindingPropagatedWhenReadyTrue(t *testing.T) {
	b := withReadyCondition(mockMCPToolBinding("my-binding", mtbNS, "agent-a", "search"))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(b).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getMCPToolBinding(t, s, "my-binding")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.True(t, detail.Ready)
	assert.Equal(t, phaseReady, detail.Phase)
	// THE HONEST PROPAGATION PROPERTY: Ready=True → "propagated".
	assert.Equal(t, propagationStatePropagated, detail.PropagationStatus,
		"Ready=True must yield propagationStatus=propagated (tool hot-updated live)")
}

// TestGetMCPToolBindingReasonWhenReadyFalseUnregisteredTool is THE PROPAGATION
// STATUS TEST for the Ready=False(UnregisteredTool) case. It proves that a
// binding whose controller rejected it with "UnregisteredTool" reports that
// reason verbatim, not "propagated" or "pending". HONEST failure surfacing.
func TestGetMCPToolBindingReasonWhenReadyFalseUnregisteredTool(t *testing.T) {
	b := withFailedCondition(mockMCPToolBinding("bad-binding", mtbNS, "agent-a", "unknown-tool"), "UnregisteredTool")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(b).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getMCPToolBinding(t, s, "bad-binding")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.False(t, detail.Ready)
	assert.Equal(t, phaseNotReady, detail.Phase)
	// THE HONEST FAILURE PROPERTY: Ready=False(UnregisteredTool) → "UnregisteredTool".
	// NEVER "propagated" — the tool is NOT hot-updated.
	assert.Equal(t, "UnregisteredTool", detail.PropagationStatus,
		"Ready=False(UnregisteredTool) must surface the reason, not 'propagated'")
}

// TestGetMCPToolBindingReasonWhenReadyFalseRegistryMismatch is THE PROPAGATION
// STATUS TEST for the Ready=False(RegistryMismatch) case.
func TestGetMCPToolBindingReasonWhenReadyFalseRegistryMismatch(t *testing.T) {
	b := withFailedCondition(mockMCPToolBinding("mismatch-binding", mtbNS, "agent-a", "pinned-tool"), "RegistryMismatch")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(b).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getMCPToolBinding(t, s, "mismatch-binding")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.False(t, detail.Ready)
	assert.Equal(t, "RegistryMismatch", detail.PropagationStatus,
		"Ready=False(RegistryMismatch) must surface the reason verbatim")
}

// TestGetMCPToolBindingPendingWhenConditionAbsent is THE PROPAGATION STATUS TEST
// for the absent condition case. A just-created binding (controller hasn't
// reconciled yet) must report "pending" — never "propagated", never an error.
// HONEST: the tool is not yet hot-updated until the controller says so.
func TestGetMCPToolBindingPendingWhenConditionAbsent(t *testing.T) {
	// No conditions set — controller hasn't reconciled yet.
	b := mockMCPToolBinding("new-binding", mtbNS, "agent-a", "search")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(b).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getMCPToolBinding(t, s, "new-binding")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.False(t, detail.Ready)
	assert.Equal(t, phasePending, detail.Phase)
	// THE HONEST PENDING PROPERTY: absent condition → "pending". NEVER "propagated".
	assert.Equal(t, propagationStatePending, detail.PropagationStatus,
		"absent Ready condition must yield propagationStatus=pending, not propagated")
}

// TestGetMCPToolBindingReturnsFullSpec proves the detail DTO includes all spec
// fields (agentRef, registryRef, toolName, mode, server).
func TestGetMCPToolBindingReturnsFullSpec(t *testing.T) {
	b := &agentsv1beta1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "my-binding", Namespace: mtbNS},
		Spec: agentsv1beta1.MCPToolBindingSpec{
			AgentRef:    "my-agent",
			RegistryRef: "my-registry",
			ToolName:    "web-search",
			Mode:        "sidecar",
			Server:      agentsv1beta1.ToolServer{Image: "ghcr.io/example/search:v1"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(b).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	detail, code, body := getMCPToolBinding(t, s, "my-binding")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-binding", detail.Name)
	assert.Equal(t, mtbNS, detail.Namespace)
	assert.Equal(t, "my-agent", detail.AgentRef)
	assert.Equal(t, "my-registry", detail.RegistryRef)
	assert.Equal(t, "web-search", detail.ToolName)
	assert.Equal(t, "sidecar", detail.Mode)
	assert.Equal(t, "ghcr.io/example/search:v1", detail.Server.Image)
}

// TestGetMCPToolBindingNotFoundIs404 proves a missing binding yields 404.
func TestGetMCPToolBindingNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getMCPToolBinding(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "not found")
}

// TestGetMCPToolBindingForbiddenIs403 proves a denied Get returns 403.
func TestGetMCPToolBindingForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "mcptoolbindings"},
					"my-binding", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	_, code, body := getMCPToolBinding(t, s, "my-binding")
	require.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "forbidden")
}

// =============================================================================
// POST /api/mcptoolbindings — create
// =============================================================================

// TestCreateMCPToolBindingSucceeds proves a valid binding create returns 201 with
// propagationStatus="pending" (controller hasn't reconciled yet — HONEST).
func TestCreateMCPToolBindingSucceeds(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(asUser(interceptor.Funcs{})).Build()
	s := explicitBindingServer(t, &fakeCallerClientFactory{client: c})

	req := MCPToolBindingCreateRequest{
		Name:        "new-binding",
		Namespace:   mtbNS,
		AgentRef:    "my-agent",
		RegistryRef: "my-registry",
		ToolName:    "web-search",
		Mode:        "sidecar",
		Server:      ToolServerDTO{Image: "ghcr.io/example/search:v1"},
	}
	detail, code, body := createMCPToolBinding(t, s, req)
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "new-binding", detail.Name)
	assert.Equal(t, mtbNS, detail.Namespace)
	assert.Equal(t, "my-agent", detail.AgentRef)
	assert.Equal(t, "web-search", detail.ToolName)
	assert.Equal(t, "sidecar", detail.Mode)
	// Propagation status must be "pending" immediately after create (controller
	// hasn't reconciled yet) — HONEST: we never say "propagated" until Ready=True.
	assert.Equal(t, propagationStatePending, detail.PropagationStatus,
		"propagationStatus must be pending immediately after create (controller hasn't reconciled)")
	assert.False(t, detail.Ready)

	// Confirm it landed in the fake store.
	var got agentsv1beta1.MCPToolBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: mtbNS, Name: "new-binding"}, &got))
	assert.Equal(t, "my-agent", got.Spec.AgentRef)
	assert.Equal(t, "web-search", got.Spec.ToolName)
}

// TestCreateMCPToolBindingMissingAgentRefIs400 proves a missing agentRef yields 400.
func TestCreateMCPToolBindingMissingAgentRefIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := MCPToolBindingCreateRequest{
		Name:        "bad-binding",
		Namespace:   mtbNS,
		RegistryRef: "my-registry",
		ToolName:    "search",
		Mode:        "sidecar",
		Server:      ToolServerDTO{Image: "img:v1"},
	}
	_, code, body := createMCPToolBinding(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "agentRef")
}

// The bind wizard's request and the BFF's answers to it. The console's tool-catalog test writes
// the request from what the wizard sends; this file's contract test writes the responses, which
// that test then renders. Regenerate the responses with
//
//	UPDATE_FIXTURES=1 go test ./internal/bff -run TestCreateMCPToolBinding_ConsoleRequest
var (
	bindRequestFixture   = filepath.Join("..", "..", "ui", "src", "test", "contract", "bind-tool.request.json")
	bindResponsesFixture = filepath.Join("..", "..", "ui", "src", "test", "contract", "bind-tool.responses.json")
)

const searchMCPURL = "http://search-mcp.default.svc.cluster.local/mcp"

// bindServer is a server whose caller is username, with the namespace's MCP servers in store
// and every authorization allowed (recorded).
func bindServer(t *testing.T, username string, store toolregistry.Store) (*Server, client.WithWatch, *recordingAuthorizer) {
	t.Helper()
	c := identityClient(t, username)
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	s.toolRegistryStore = store
	auth := &recordingAuthorizer{}
	s.authorizer = auth
	return s, c, auth
}

func postBinding(t *testing.T, s *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcptoolbindings", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestCreateMCPToolBinding_ConsoleRequest posts the bind wizard's own request to the real handler:
// the binding is created on the MCP server the tool came from, at the URL that server pins, and
// the detail the wizard then polls says whether it has propagated, as spec 90 reads it.
func TestCreateMCPToolBinding_ConsoleRequest(t *testing.T) {
	store := toolregistry.NewMemStore()
	_, err := store.Upsert(context.Background(), toolregistry.ToolRegistry{
		Namespace: "default", Name: "search-mcp",
		Tools:  []toolregistry.ToolEntry{{Name: "code-search", URL: searchMCPURL}},
		Labels: map[string]string{labelManagedBy: managedByMCP, labelMCPVisibility: visibilityTeam},
	})
	require.NoError(t, err)
	s, c, auth := bindServer(t, aliceUser, store)

	raw, err := os.ReadFile(bindRequestFixture)
	require.NoError(t, err, "the console's bind request fixture is written by ui/src/pages/tool-catalog-page.test.tsx")
	rec := postBinding(t, s, raw)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := rec.Body.Bytes()
	assert.Equal(t, authz.Action{Verb: authz.VerbCreate, Group: agentsAPIGroup, Resource: resourceMCPToolBindings, Namespace: "default"},
		auth.last, "the bind is authorized before the namespace's MCP servers are read")

	var got agentsv1beta1.MCPToolBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-agent-code-search"}, &got))
	assert.Equal(t, agentsv1beta1.MCPToolBindingSpec{
		AgentRef: "my-agent", RegistryRef: "search-mcp", ToolName: "code-search",
		Mode: "remote", Server: agentsv1beta1.ToolServer{URL: searchMCPURL},
	}, got.Spec)

	got.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "ToolHotUpdated", Message: "pushed to discovery",
		LastTransitionTime: metav1.NewTime(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)),
	}}
	require.NoError(t, c.Update(context.Background(), &got))
	detail, code, body := getBindingIn(t, s, "default", "my-agent-code-search")
	require.Equal(t, http.StatusOK, code, body)
	assert.True(t, detail.Ready)
	assert.Equal(t, propagationStatePropagated, detail.PropagationStatus)
	assert.Equal(t, "my-agent", detail.AgentRef)

	var responses struct {
		Created    json.RawMessage `json:"created"`
		Propagated json.RawMessage `json:"propagated"`
	}
	responses.Created, responses.Propagated = json.RawMessage(created), json.RawMessage(body)
	out, err := json.MarshalIndent(responses, "", "  ")
	require.NoError(t, err)
	out = append(out, '\n')
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		require.NoError(t, os.WriteFile(bindResponsesFixture, out, 0o644))
	}
	want, err := os.ReadFile(bindResponsesFixture)
	require.NoError(t, err, "the committed fixture is missing; regenerate it with UPDATE_FIXTURES=1")
	assert.Equal(t, string(want), string(out), "the bind responses changed; regenerate %s (UPDATE_FIXTURES=1)", bindResponsesFixture)
}

// TestCreateMCPToolBinding_OldConsoleRequestIsRefused pins the defect: the wizard used to send the
// agent as an object, which the handler cannot decode.
func TestCreateMCPToolBinding_OldConsoleRequestIsRefused(t *testing.T) {
	s, _, _ := bindServer(t, aliceUser, toolregistry.NewMemStore())
	rec := postBinding(t, s, []byte(`{"name":"b","namespace":"default","agentRef":{"namespace":"default","name":"my-agent"},"toolName":"code-search"}`))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestCreateMCPToolBinding_FillsFromItsServer: a request naming only the agent and the tool is
// bound to the first MCP server, by name, that the caller may use and that provides the tool;
// another user's private server is never one of them, and naming a server picks it.
func TestCreateMCPToolBinding_FillsFromItsServer(t *testing.T) {
	cases := []struct {
		name, user, registry  string
		wantRegistry, wantURL string
	}{
		{"the owner of a private server gets it first", aliceUser, "", alicePrivateRegistry, alicePrivateURL},
		{"anyone else gets the team server", "bob@example.com", "", teamEchoRegistry, teamEchoURL},
		{"a named server is the one bound", aliceUser, teamEchoRegistry, teamEchoRegistry, teamEchoURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c, _ := bindServer(t, tc.user, echoRegistries(t, mtbNS))
			body, err := json.Marshal(MCPToolBindingCreateRequest{
				Name: "b", Namespace: mtbNS, AgentRef: "agent-a", RegistryRef: tc.registry, ToolName: "echo",
			})
			require.NoError(t, err)
			rec := postBinding(t, s, body)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var got agentsv1beta1.MCPToolBinding
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: mtbNS, Name: "b"}, &got))
			assert.Equal(t, tc.wantRegistry, got.Spec.RegistryRef)
			assert.Equal(t, "remote", got.Spec.Mode)
			assert.Equal(t, tc.wantURL, got.Spec.Server.URL)
		})
	}
}

// TestCreateMCPToolBinding_RefusesWhatItCannotResolve: a tool no usable server provides, another
// user's private server named outright, no store, and a caller who may not bind are each refused
// with nothing created; the last before any server is read.
func TestCreateMCPToolBinding_RefusesWhatItCannotResolve(t *testing.T) {
	cases := []struct {
		name     string
		user     string
		req      MCPToolBindingCreateRequest
		noStore  bool
		authzErr error
		want     int
		wantMsg  string
	}{
		{
			"an unknown tool", aliceUser,
			MCPToolBindingCreateRequest{ToolName: "nope"},
			false, nil,
			http.StatusBadRequest, `tool "nope" is not provided by any MCP server you can use`,
		},
		{
			"another user's private server", "bob@example.com",
			MCPToolBindingCreateRequest{ToolName: "echo", RegistryRef: alicePrivateRegistry},
			false, nil,
			http.StatusBadRequest, `is not provided by an MCP server "a-alice-private" you can use`,
		},
		{
			"no control-plane store", aliceUser,
			MCPToolBindingCreateRequest{ToolName: "echo"},
			true, nil,
			http.StatusNotImplemented, msgToolRegistryStoreRequired,
		},
		{
			"a caller who may not bind", aliceUser,
			MCPToolBindingCreateRequest{ToolName: "echo"},
			false, authz.ErrForbidden,
			http.StatusForbidden, "you do not have permission to bind tools",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var store toolregistry.Store
			if !tc.noStore {
				store = echoRegistries(t, mtbNS)
			}
			s, c, auth := bindServer(t, tc.user, store)
			auth.err = tc.authzErr
			tc.req.Name, tc.req.Namespace, tc.req.AgentRef = "b", mtbNS, "agent-a"
			body, err := json.Marshal(tc.req)
			require.NoError(t, err)
			rec := postBinding(t, s, body)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
			var refusal struct {
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &refusal))
			assert.Contains(t, refusal.Error, tc.wantMsg)
			var list agentsv1beta1.MCPToolBindingList
			require.NoError(t, c.List(context.Background(), &list))
			assert.Empty(t, list.Items)
		})
	}
}

// TestCreateMCPToolBindingAlreadyExistsIs409 proves a duplicate create yields 409.
func TestCreateMCPToolBindingAlreadyExistsIs409(t *testing.T) {
	existing := mockMCPToolBinding("my-binding", mtbNS, "agent-a", "search")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).WithInterceptorFuncs(asUser(interceptor.Funcs{})).Build()
	s := explicitBindingServer(t, &fakeCallerClientFactory{client: c})

	req := MCPToolBindingCreateRequest{
		Name:        "my-binding",
		Namespace:   mtbNS,
		AgentRef:    "agent-a",
		RegistryRef: "my-registry",
		ToolName:    "search",
		Mode:        "sidecar",
		Server:      ToolServerDTO{Image: "img:v1"},
	}
	_, code, body := createMCPToolBinding(t, s, req)
	assert.Equal(t, http.StatusConflict, code, "body: %s", body)
	assert.Contains(t, body, "already exists")
}

// TestCreateMCPToolBindingAPIServerRejectionSurfaces4xx proves API server
// Invalid surfaces as 4xx (422).
func TestCreateMCPToolBindingAPIServerRejectionSurfaces4xx(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(asUser(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: mcpToolBindingKind},
					obj.GetName(), nil)
			},
		})).Build()
	s := explicitBindingServer(t, &fakeCallerClientFactory{client: c})

	req := MCPToolBindingCreateRequest{
		Name:        "bad-binding",
		Namespace:   mtbNS,
		AgentRef:    "agent-a",
		RegistryRef: "my-registry",
		ToolName:    "search",
		Mode:        "sidecar",
		Server:      ToolServerDTO{Image: "img:v1"},
	}
	_, code, body := createMCPToolBinding(t, s, req)
	assert.True(t, code >= 400 && code < 500, "API server rejection must surface as 4xx, got %d: %s", code, body)
}

// TestCreateMCPToolBindingForbiddenIs403 proves a viewer's create returns 403.
func TestCreateMCPToolBindingForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(asUser(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "mcptoolbindings"},
					obj.GetName(), errors.New("viewer cannot create"))
			},
		})).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := explicitBindingServer(t, factory)

	req := MCPToolBindingCreateRequest{
		Name:        "no-perm",
		Namespace:   mtbNS,
		AgentRef:    "agent-a",
		RegistryRef: "my-registry",
		ToolName:    "search",
		Mode:        "sidecar",
		Server:      ToolServerDTO{Image: "img:v1"},
	}
	_, code, body := createMCPToolBinding(t, s, req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestCreateMCPToolBindingWithoutTokenIs401 proves a token-less POST is rejected 401.
func TestCreateMCPToolBindingWithoutTokenIs401(t *testing.T) {
	createCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				createCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(MCPToolBindingCreateRequest{
		Name:        "binding",
		Namespace:   mtbNS,
		AgentRef:    "agent-a",
		RegistryRef: "my-registry",
		ToolName:    "search",
		Mode:        "sidecar",
		Server:      ToolServerDTO{Image: "img:v1"},
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/mcptoolbindings", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, createCalled, "no K8s create must run for a token-less request")
}

// =============================================================================
// PUT /api/mcptoolbindings/{ns}/{name} — SSA edit
// =============================================================================

// TestUpdateMCPToolBindingEditsSpec proves a PUT updates the spec via SSA
// (ForceOwnership) and the change is visible in the fake store.
func TestUpdateMCPToolBindingEditsSpec(t *testing.T) {
	existing := mockMCPToolBinding("my-binding", mtbNS, "agent-a", "search")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).WithInterceptorFuncs(asUser(interceptor.Funcs{})).Build()
	s := explicitBindingServer(t, &fakeCallerClientFactory{client: c})

	req := MCPToolBindingUpdateRequest{
		AgentRef:    "agent-b", // changed
		RegistryRef: "my-registry",
		ToolName:    "search",
		Mode:        "sidecar",
		Server:      ToolServerDTO{Image: "ghcr.io/example/search:v2"}, // changed
	}
	detail, code, body := putMCPToolBinding(t, s, "my-binding", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "agent-b", detail.AgentRef)
	assert.Equal(t, "ghcr.io/example/search:v2", detail.Server.Image)
}

// TestUpdateMCPToolBindingRenameGuardIs400 proves a name mismatch yields 400.
func TestUpdateMCPToolBindingRenameGuardIs400(t *testing.T) {
	existing := mockMCPToolBinding("my-binding", mtbNS, "agent-a", "search")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := MCPToolBindingUpdateRequest{
		Name:        "different-name", // mismatch
		AgentRef:    "agent-a",
		RegistryRef: "my-registry",
		ToolName:    "search",
		Mode:        "sidecar",
		Server:      ToolServerDTO{Image: "img:v1"},
	}
	_, code, body := putMCPToolBinding(t, s, "my-binding", req)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "rename")
}

// TestUpdateMCPToolBindingAbsentNameInBodyIsOK proves omitting Name is fine.
func TestUpdateMCPToolBindingAbsentNameInBodyIsOK(t *testing.T) {
	existing := mockMCPToolBinding("my-binding", mtbNS, "agent-a", "search")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).WithInterceptorFuncs(asUser(interceptor.Funcs{})).Build()
	s := explicitBindingServer(t, &fakeCallerClientFactory{client: c})

	req := MCPToolBindingUpdateRequest{
		// Name is empty — URL is authoritative.
		AgentRef:    "agent-a",
		RegistryRef: "my-registry",
		ToolName:    "search-updated",
		Mode:        "sidecar",
		Server:      ToolServerDTO{Image: "img:v1"},
	}
	detail, code, body := putMCPToolBinding(t, s, "my-binding", req)
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	require.NotNil(t, detail)
	assert.Equal(t, "my-binding", detail.Name)
}

// TestUpdateMCPToolBindingForbiddenIs403 proves a viewer's PUT returns 403.
func TestUpdateMCPToolBindingForbiddenIs403(t *testing.T) {
	existing := mockMCPToolBinding("my-binding", mtbNS, "agent-a", "search")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(asUser(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "mcptoolbindings"},
					obj.GetName(), errors.New("viewer cannot update"))
			},
		})).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := explicitBindingServer(t, factory)

	req := MCPToolBindingUpdateRequest{
		AgentRef:    "agent-a",
		RegistryRef: "my-registry",
		ToolName:    "search",
		Mode:        "sidecar",
		Server:      ToolServerDTO{Image: "img:v1"},
	}
	_, code, body := putMCPToolBinding(t, s, "my-binding", req)
	require.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestUpdateMCPToolBindingInvalidWriteSurfaces422 proves API server Invalid → 422.
func TestUpdateMCPToolBindingInvalidWriteSurfaces422(t *testing.T) {
	existing := mockMCPToolBinding("my-binding", mtbNS, "agent-a", "search")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).
		WithInterceptorFuncs(asUser(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: mcpToolBindingKind},
					obj.GetName(), nil)
			},
		})).Build()
	s := explicitBindingServer(t, &fakeCallerClientFactory{client: c})

	req := MCPToolBindingUpdateRequest{
		AgentRef:    "agent-a",
		RegistryRef: "my-registry",
		ToolName:    "search",
		Mode:        "sidecar",
		Server:      ToolServerDTO{Image: "img:v1"},
	}
	_, code, body := putMCPToolBinding(t, s, "my-binding", req)
	assert.Equal(t, http.StatusUnprocessableEntity, code, "API-server Invalid must surface as 422, got %d: %s", code, body)
}

// TestUpdateMCPToolBindingWithoutTokenIs401 proves a token-less PUT is rejected 401.
func TestUpdateMCPToolBindingWithoutTokenIs401(t *testing.T) {
	patchCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patchCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	b, _ := json.Marshal(MCPToolBindingUpdateRequest{
		AgentRef: "agent-a", RegistryRef: "my-registry", ToolName: "search", Mode: "sidecar",
		Server: ToolServerDTO{Image: "img:v1"},
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/mcptoolbindings/"+mtbNS+"/my-binding", bytes.NewReader(b)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, patchCalled, "no K8s patch must run for a token-less request")
}

// =============================================================================
// DELETE /api/mcptoolbindings/{ns}/{name} — delete
// =============================================================================

// TestDeleteMCPToolBindingRemovesObject proves a DELETE succeeds (204).
func TestDeleteMCPToolBindingRemovesObject(t *testing.T) {
	b := mockMCPToolBinding("my-binding", mtbNS, "agent-a", "search")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(b).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteMCPToolBinding(t, s, "my-binding")
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	var got agentsv1beta1.MCPToolBinding
	err := c.Get(context.Background(), client.ObjectKey{Namespace: mtbNS, Name: "my-binding"}, &got)
	require.True(t, apierrors.IsNotFound(err), "MCPToolBinding must be gone after a successful DELETE")
}

// TestDeleteMCPToolBindingNotFoundIs404 proves deleting a missing binding yields 404.
func TestDeleteMCPToolBindingNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteMCPToolBinding(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestDeleteMCPToolBindingForbiddenIs403 proves a viewer's DELETE returns 403.
func TestDeleteMCPToolBindingForbiddenIs403(t *testing.T) {
	b := mockMCPToolBinding("my-binding", mtbNS, "agent-a", "search")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(b).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "mcptoolbindings"},
					obj.GetName(), errors.New("viewer cannot delete"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := deleteMCPToolBinding(t, s, "my-binding")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestDeleteMCPToolBindingWithoutTokenIs401 proves a token-less DELETE is rejected 401.
func TestDeleteMCPToolBindingWithoutTokenIs401(t *testing.T) {
	deleteCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				deleteCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/mcptoolbindings/"+mtbNS+"/my-binding", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, deleteCalled, "no K8s delete must run for a token-less request")
}

// =============================================================================
// propagationStatusFromConditions — unit tests (the honest-propagation core)
// =============================================================================

// TestPropagationStatusFromConditionsThreeStates exercises all three states of
// the propagation status helper directly. This is the unit test for the
// honest-propagation core — the function that the detail endpoint uses.
func TestPropagationStatusFromConditionsThreeStates(t *testing.T) {
	// State 1: Ready=True → "propagated"
	readyTrue := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "ToolHotUpdated"},
	}
	assert.Equal(t, propagationStatePropagated, propagationStatusFromConditions(readyTrue),
		"Ready=True must return propagated")

	// State 2: Ready=False(UnregisteredTool) → "UnregisteredTool" (never "propagated")
	readyFalseUnreg := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse, Reason: "UnregisteredTool"},
	}
	assert.Equal(t, "UnregisteredTool", propagationStatusFromConditions(readyFalseUnreg),
		"Ready=False(UnregisteredTool) must return the reason, not propagated")

	// State 3: Ready=False(RegistryMismatch) → "RegistryMismatch"
	readyFalseMismatch := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse, Reason: "RegistryMismatch"},
	}
	assert.Equal(t, "RegistryMismatch", propagationStatusFromConditions(readyFalseMismatch),
		"Ready=False(RegistryMismatch) must return the reason, not propagated")

	// State 4: absent condition → "pending" (never "propagated")
	assert.Equal(t, propagationStatePending, propagationStatusFromConditions(nil),
		"absent condition must return pending, not propagated")

	// State 5: Ready=Unknown → "pending" (not yet determined)
	readyUnknown := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionUnknown, Reason: "Reconciling"},
	}
	assert.Equal(t, propagationStatePending, propagationStatusFromConditions(readyUnknown),
		"Ready=Unknown must return pending, not propagated")
}

// TestMCPToolBinding_AnExplicitServerIsCheckedLikeAFilledOne: naming registryRef, mode and server
// outright gets no further than leaving them for the BFF to fill in. Another user's private server is
// refused with the very same response either way, a personal server shared with the team binds only
// for its owner, an update cannot move a binding onto either, and a server the caller may use still
// binds.
func TestMCPToolBinding_AnExplicitServerIsCheckedLikeAFilledOne(t *testing.T) {
	const sharedPersonal, sharedPersonalURL = "b-alice-shared", "https://alice-shared.example/mcp"
	newServer := func(t *testing.T) (*Server, client.WithWatch) {
		t.Helper()
		store := echoRegistries(t, mtbNS)
		_, err := store.Upsert(context.Background(), toolregistry.ToolRegistry{
			Namespace: mtbNS, Name: sharedPersonal,
			Tools: []toolregistry.ToolEntry{{Name: "echo", URL: sharedPersonalURL}},
			Labels: map[string]string{
				labelManagedBy: managedByMCP, labelMCPScope: scopePersonal, labelMCPOwner: userGrantHash(aliceUser),
				labelMCPVisibility: visibilityTeam,
			},
		})
		require.NoError(t, err)
		existing := mockMCPToolBinding("bob-echo", mtbNS, "agent-a", "echo")
		existing.Spec.RegistryRef, existing.Spec.Mode = teamEchoRegistry, "remote"
		existing.Spec.Server = agentsv1beta1.ToolServer{URL: teamEchoURL}
		s, c, _ := bindServer(t, "bob@example.com", store)
		require.NoError(t, c.Create(context.Background(), existing))
		return s, c
	}
	post := func(t *testing.T, s *Server, req MCPToolBindingCreateRequest) *httptest.ResponseRecorder {
		t.Helper()
		req.Name, req.Namespace, req.AgentRef, req.ToolName = "b", mtbNS, "agent-a", "echo"
		body, err := json.Marshal(req)
		require.NoError(t, err)
		return postBinding(t, s, body)
	}
	explicit := func(registry, url string) MCPToolBindingCreateRequest {
		return MCPToolBindingCreateRequest{RegistryRef: registry, Mode: "remote", Server: ToolServerDTO{URL: url}}
	}

	for _, tc := range []struct {
		name, registry, url string
		want                int
	}{
		{"another user's private server", alicePrivateRegistry, alicePrivateURL, http.StatusBadRequest},
		{"another user's personal server shared with the team", sharedPersonal, sharedPersonalURL, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newServer(t)
			filled := post(t, s, MCPToolBindingCreateRequest{RegistryRef: tc.registry})
			given := post(t, s, explicit(tc.registry, tc.url))
			assert.Equal(t, tc.want, filled.Code, filled.Body.String())
			assert.Equal(t, filled.Code, given.Code, "an explicit server is refused like a filled-in one")
			assert.Equal(t, filled.Body.String(), given.Body.String())

			_, code, body := putMCPToolBinding(t, s, "bob-echo", MCPToolBindingUpdateRequest{
				AgentRef: "agent-a", RegistryRef: tc.registry, ToolName: "echo", Mode: "remote", Server: ToolServerDTO{URL: tc.url},
			})
			assert.Equal(t, tc.want, code, "an update cannot move a binding onto it: %s", body)

			var list agentsv1beta1.MCPToolBindingList
			require.NoError(t, c.List(context.Background(), &list))
			require.Len(t, list.Items, 1, "nothing was created")
			assert.Equal(t, teamEchoRegistry, list.Items[0].Spec.RegistryRef, "nothing was moved")
		})
	}

	t.Run("a server the caller may use still binds", func(t *testing.T) {
		s, c := newServer(t)
		rec := post(t, s, explicit(teamEchoRegistry, teamEchoURL))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var got agentsv1beta1.MCPToolBinding
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: mtbNS, Name: "b"}, &got))
		assert.Equal(t, teamEchoRegistry, got.Spec.RegistryRef)
	})
}
