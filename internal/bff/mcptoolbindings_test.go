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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// mtbNS is the namespace used in MCPToolBinding tests.
const mtbNS = "team-bindings"

// --- fixture helpers --------------------------------------------------------

// mockMCPToolBinding builds a minimal MCPToolBinding (sidecar mode).
func mockMCPToolBinding(name, ns, agentRef, toolName string) *agentsv1alpha1.MCPToolBinding {
	return &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.MCPToolBindingSpec{
			AgentRef:    agentRef,
			RegistryRef: "default-registry",
			ToolName:    toolName,
			Mode:        "sidecar",
			Server: agentsv1alpha1.ToolServer{
				Image: "ghcr.io/example/mcp-search:latest",
			},
		},
	}
}

// withReadyCondition sets Ready=True on an MCPToolBinding, simulating that the
// controller has registered, pin-matched, rendered, and hot-updated the tool.
// Ready=True IS the hot-update propagation signal.
func withReadyCondition(b *agentsv1alpha1.MCPToolBinding) *agentsv1alpha1.MCPToolBinding {
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
func withFailedCondition(b *agentsv1alpha1.MCPToolBinding, reason string) *agentsv1alpha1.MCPToolBinding {
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
	all := []*agentsv1alpha1.MCPToolBinding{
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
			bList, ok := list.(*agentsv1alpha1.MCPToolBindingList)
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
	b := &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "my-binding", Namespace: mtbNS},
		Spec: agentsv1alpha1.MCPToolBindingSpec{
			AgentRef:    "my-agent",
			RegistryRef: "my-registry",
			ToolName:    "web-search",
			Mode:        "sidecar",
			Server:      agentsv1alpha1.ToolServer{Image: "ghcr.io/example/search:v1"},
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
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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
	var got agentsv1alpha1.MCPToolBinding
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

// TestCreateMCPToolBindingMissingModeIs400 proves a missing mode yields 400.
func TestCreateMCPToolBindingMissingModeIs400(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	req := MCPToolBindingCreateRequest{
		Name:        "bad-binding",
		Namespace:   mtbNS,
		AgentRef:    "agent-a",
		RegistryRef: "my-registry",
		ToolName:    "search",
		// Mode missing
		Server: ToolServerDTO{Image: "img:v1"},
	}
	_, code, body := createMCPToolBinding(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "mode")
}

// TestCreateMCPToolBindingAlreadyExistsIs409 proves a duplicate create yields 409.
func TestCreateMCPToolBindingAlreadyExistsIs409(t *testing.T) {
	existing := mockMCPToolBinding("my-binding", mtbNS, "agent-a", "search")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: mcpToolBindingKind},
					obj.GetName(), nil)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "mcptoolbindings"},
					obj.GetName(), errors.New("viewer cannot create"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

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
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: agentsAPIGroup, Resource: "mcptoolbindings"},
					obj.GetName(), errors.New("viewer cannot update"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

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
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: agentsAPIGroup, Kind: mcpToolBindingKind},
					obj.GetName(), nil)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

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

	var got agentsv1alpha1.MCPToolBinding
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
