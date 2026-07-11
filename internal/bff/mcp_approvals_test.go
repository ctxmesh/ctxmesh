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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// pendingMCPURL is the IP-literal MCP endpoint the approval tests register. An IP
// literal keeps the egress builder deterministic (no DNS lookup) so the approve
// path opens a stable /32 ipBlock without a resolver stub.
const pendingMCPURL = "http://10.0.0.5:8080/mcp"

// pendingMCPRegistry builds a register-managed ToolRegistry in the pending state
// (the shape the register flow leaves on a hardened cluster: managed-by label +
// status annotation + URL annotation, and every entry ApprovalPending). withKey
// stamps the Secret-name annotation so a reject test can prove the credential
// artifacts are cleaned up.
func pendingMCPRegistry(name string, withKey bool) *agentsv1alpha1.ToolRegistry {
	ann := map[string]string{
		annMCPURL:    pendingMCPURL,
		annMCPStatus: agentsv1alpha1.ApprovalPending,
	}
	if withKey {
		ann[annMCPSecret] = name
	}
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "prod",
			Labels:      map[string]string{labelManagedBy: managedByMCP},
			Annotations: ann,
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{
			Name:           "get_weather",
			URL:            pendingMCPURL,
			Source:         agentsv1alpha1.SourceUserAdded,
			ApprovalStatus: agentsv1alpha1.ApprovalPending,
		}}},
	}
}

// approvedMCPRegistry builds a register-managed ToolRegistry already approved (the
// self-serve shape), for the "only pending is listed" assertion.
func approvedMCPRegistry(name, url string) *agentsv1alpha1.ToolRegistry {
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "prod",
			Labels:    map[string]string{labelManagedBy: managedByMCP},
			Annotations: map[string]string{
				annMCPURL:    url,
				annMCPStatus: agentsv1alpha1.ApprovalApproved,
			},
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{
			Name:           "echo",
			URL:            url,
			Source:         agentsv1alpha1.SourceUserAdded,
			ApprovalStatus: agentsv1alpha1.ApprovalApproved,
		}}},
	}
}

// --- GET /api/mcp/approvals: only pending is listed ---------------------------

// TestListMCPApprovalsReturnsOnlyPending proves the queue lists ONLY the pending
// register-managed servers — an already-approved one and a curated (non-managed)
// registry are both excluded.
func TestListMCPApprovalsReturnsOnlyPending(t *testing.T) {
	pending := pendingMCPRegistry("pending-mcp", false)
	approved := approvedMCPRegistry("approved-mcp", "http://10.0.0.6:8080/mcp")
	curated := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "default-tools", Namespace: "prod"},
		Spec:       agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{Name: "curated_tool"}}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pending, approved, curated).Build()
	s, _, _ := newMCPServer(t, c, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcp/approvals", nil)
	req.Header.Set("Authorization", "Bearer operator-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp MCPServerListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Servers, 1, "only the pending server is queued")
	assert.Equal(t, "pending-mcp", resp.Servers[0].Name)
	assert.Equal(t, agentsv1alpha1.ApprovalPending, resp.Servers[0].Status)
}

// TestListMCPApprovalsEmpty proves an empty queue is [] not null.
func TestListMCPApprovalsEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcp/approvals", nil)
	req.Header.Set("Authorization", "Bearer operator-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"servers":[],"items":[]}`, rec.Body.String())
}

// TestListMCPApprovalsSelfServeIsEmpty proves that in self-serve mode (approval off)
// nothing is pending — every register-managed server is already approved, so the
// queue is empty but the endpoint exists and behaves honestly.
func TestListMCPApprovalsSelfServeIsEmpty(t *testing.T) {
	approved := approvedMCPRegistry("self-serve-mcp", "http://10.0.0.7:8080/mcp")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(approved).Build()
	s, _, _ := newMCPServer(t, c, false) // requireApproval = false (self-serve)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcp/approvals", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"servers":[],"items":[]}`, rec.Body.String(), "self-serve → nothing pending")
}

// TestListMCPApprovalsForbiddenIs403 proves a Forbidden on the list surfaces as 403,
// never a swallowed empty list.
func TestListMCPApprovalsForbiddenIs403(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(forbiddenListInterceptor()).
		Build()
	s, _, _ := newMCPServer(t, c, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcp/approvals", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// --- POST /api/mcp/approvals/{ns}/{name}: approve -----------------------------

// TestApproveMCPFlipsStatusAndOpensEgress is the core invariant proof: a pending
// server has NO egress and un-bindable (pending) tools; approving flips
// pending→approved (bindable) AND opens the per-server egress. It asserts BOTH the
// before-state (no NetworkPolicy while pending) and the after-state (approved
// entries + a bounded egress NetworkPolicy).
func TestApproveMCPFlipsStatusAndOpensEgress(t *testing.T) {
	pending := pendingMCPRegistry("weather-mcp", false)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pending).Build()
	s, factory, _ := newMCPServer(t, c, true)

	// BEFORE: pending → NO egress NetworkPolicy exists, and the tools are pending
	// (not bindable). This is the m14.6 B1 property the approve must be the sole
	// transition to break.
	var npBefore networkingv1.NetworkPolicy
	errBefore := c.Get(context.Background(),
		client.ObjectKey{Name: "weather-mcp" + networkPolicyMCPSuffix, Namespace: "prod"}, &npBefore)
	require.True(t, apierrors.IsNotFound(errBefore), "a pending server must have NO egress before approval")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/approvals/prod/weather-mcp", nil)
	req.Header.Set("Authorization", "Bearer operator-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "operator-persona-token", factory.gotToken, "the caller's token scoped the approve")

	var resp MCPServerSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, agentsv1alpha1.ApprovalApproved, resp.Status)

	// AFTER: the ToolRegistry entries + the status annotation are approved (bindable).
	var tr agentsv1alpha1.ToolRegistry
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "weather-mcp", Namespace: "prod"}, &tr))
	assert.Equal(t, agentsv1alpha1.ApprovalApproved, tr.Annotations[annMCPStatus])
	for _, e := range tr.Spec.Tools {
		assert.Equal(t, agentsv1alpha1.ApprovalApproved, e.ApprovalStatus, "approve makes every entry bindable")
	}

	// AFTER: the per-server egress NetworkPolicy now EXISTS, egress-only + bounded to
	// the server port (never a blanket open). Approving is the ONLY transition that
	// opened it.
	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Name: "weather-mcp" + networkPolicyMCPSuffix, Namespace: "prod"}, &np))
	require.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
	require.Len(t, np.Spec.Egress, 1)
	require.Len(t, np.Spec.Egress[0].Ports, 1, "egress is scoped to the server port, not a blanket open")
	require.Len(t, np.Spec.Egress[0].To, 1, "egress `to` must be a bounded ipBlock, never empty")
	require.NotNil(t, np.Spec.Egress[0].To[0].IPBlock)
	assert.Equal(t, "10.0.0.5/32", np.Spec.Egress[0].To[0].IPBlock.CIDR)
}

// TestApproveMCPViewerForbiddenIsRealNoEgress is the operator-only proof: a
// developer/viewer whose RBAC denies the UPDATE gets the API server's real 403 —
// AND (the crux) NO egress is opened by a denied approve. There is no bypass.
func TestApproveMCPViewerForbiddenIsRealNoEgress(t *testing.T) {
	pending := pendingMCPRegistry("weather-mcp", false)
	npCreated := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(pending).
		WithInterceptorFuncs(interceptor.Funcs{
			// The operator-level write is an UPDATE on toolregistries — deny it as the
			// API server would for a viewer/developer.
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "toolregistries"},
					"weather-mcp", assert.AnError)
			},
			// Flag any NetworkPolicy create so we can prove a denied approve opens none.
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					npCreated = true
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	s, _, _ := newMCPServer(t, c, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/approvals/prod/weather-mcp", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, "a non-operator's approve is the API server's real 403")
	assert.False(t, npCreated, "a DENIED approve must NOT open egress — approve is the ONLY opener, and it failed")

	// The catalog entry stays pending (the failed update did not persist).
	var tr agentsv1alpha1.ToolRegistry
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "weather-mcp", Namespace: "prod"}, &tr))
	assert.Equal(t, agentsv1alpha1.ApprovalPending, tr.Spec.Tools[0].ApprovalStatus, "denied approve leaves it pending")
	var np networkingv1.NetworkPolicy
	err := c.Get(context.Background(),
		client.ObjectKey{Name: "weather-mcp" + networkPolicyMCPSuffix, Namespace: "prod"}, &np)
	assert.True(t, apierrors.IsNotFound(err), "still no egress after a denied approve")
}

// TestApproveMCPNotFoundIs404 proves approving a missing server is a 404.
func TestApproveMCPNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/approvals/prod/ghost-mcp", nil)
	req.Header.Set("Authorization", "Bearer operator-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestApproveMCPRejectsNonManagedRegistryIs404 proves the approval surface acts
// ONLY on register-managed BYO servers — an operator-curated ToolRegistry (no
// managed-by=mcp label) is a 404, never approvable/egress-opened through this path.
func TestApproveMCPRejectsNonManagedRegistryIs404(t *testing.T) {
	curated := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "curated-tools", Namespace: "prod"},
		Spec:       agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{Name: "curated_tool"}}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(curated).Build()
	s, _, _ := newMCPServer(t, c, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/approvals/prod/curated-tools", nil)
	req.Header.Set("Authorization", "Bearer operator-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, "a non-managed registry is not an approval target")
}

// --- POST /api/mcp/approvals/{ns}/{name}/reject: deny --------------------------

// TestRejectMCPRemovesEntryAndArtifactsStaysNoEgress proves rejecting a pending
// (keyed) server removes the ToolRegistry (so the tools disappear from the catalog
// and stay non-bindable) AND cleans up the Secret + SecretBinding — and NEVER opens
// egress (a pending server has none; reject keeps it that way).
func TestRejectMCPRemovesEntryAndArtifactsStaysNoEgress(t *testing.T) {
	pending := pendingMCPRegistry("keyed-mcp", true)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed-mcp", Namespace: "prod"},
		Data:       map[string][]byte{secretKeyAPIKey: []byte(theMCPKey)},
	}
	binding := &agentsv1alpha1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed-mcp", Namespace: "prod"},
		Spec: agentsv1alpha1.SecretBindingSpec{
			Backend:   secretBackendKubernetes,
			SecretRef: agentsv1alpha1.SecretKeyRef{Name: "keyed-mcp", Key: secretKeyAPIKey},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pending, secret, binding).Build()
	s, _, _ := newMCPServer(t, c, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/approvals/prod/keyed-mcp/reject", nil)
	req.Header.Set("Authorization", "Bearer operator-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp MCPApprovalActionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, mcpApprovalRejected, resp.Status)

	// The ToolRegistry is gone → the tools are no longer in the catalog (not bindable).
	var tr agentsv1alpha1.ToolRegistry
	errTR := c.Get(context.Background(), client.ObjectKey{Name: "keyed-mcp", Namespace: "prod"}, &tr)
	assert.True(t, apierrors.IsNotFound(errTR), "reject removes the catalog entry")

	// The credential artifacts are cleaned up.
	var sec corev1.Secret
	assert.True(t, apierrors.IsNotFound(
		c.Get(context.Background(), client.ObjectKey{Name: "keyed-mcp", Namespace: "prod"}, &sec)),
		"reject deletes the Secret")
	var sb agentsv1alpha1.SecretBinding
	assert.True(t, apierrors.IsNotFound(
		c.Get(context.Background(), client.ObjectKey{Name: "keyed-mcp", Namespace: "prod"}, &sb)),
		"reject deletes the SecretBinding")

	// No egress was ever opened (pending had none; reject never opens one).
	var np networkingv1.NetworkPolicy
	assert.True(t, apierrors.IsNotFound(
		c.Get(context.Background(),
			client.ObjectKey{Name: "keyed-mcp" + networkPolicyMCPSuffix, Namespace: "prod"}, &np)),
		"a rejected server stays with NO egress")
}

// TestRejectMCPViewerForbiddenIs403 proves a developer/viewer whose RBAC denies the
// DELETE gets the API server's real 403 — no bypass — and the entry stays.
func TestRejectMCPViewerForbiddenIs403(t *testing.T) {
	pending := pendingMCPRegistry("weather-mcp", false)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(pending).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "toolregistries"},
					"weather-mcp", assert.AnError)
			},
		}).
		Build()
	s, _, _ := newMCPServer(t, c, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/approvals/prod/weather-mcp/reject", nil)
	req.Header.Set("Authorization", "Bearer viewer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, "a non-operator's reject is the API server's real 403")
}

// TestRejectMCPNotFoundIs404 proves rejecting a missing server is a 404.
func TestRejectMCPNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/approvals/prod/ghost-mcp/reject", nil)
	req.Header.Set("Authorization", "Bearer operator-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// --- gating ------------------------------------------------------------------

// TestMCPApprovalsKillSwitchOffIs404 proves that with the BYO-MCP flow DISABLED,
// the approval endpoints 404 (feature-off).
func TestMCPApprovalsKillSwitchOffIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		MCPEnabled:    false, // kill-switch OFF
		ProviderHTTP:  &http.Client{},
		Version:       "test",
		Log:           logr.Discard(),
	})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/mcp/approvals"},
		{http.MethodPost, "/api/mcp/approvals/prod/x"},
		{http.MethodPost, "/api/mcp/approvals/prod/x/reject"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer operator-persona-token")
			s.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotFound, rec.Code, "feature-off endpoints must 404")
		})
	}
}

// TestMCPApprovalsNilFactoryIs501 proves the flow enabled but no caller factory →
// honest 501 (never a BFF-SA fallback).
func TestMCPApprovalsNilFactoryIs501(t *testing.T) {
	s := NewServer(Options{
		CallerClients: nil,
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		MCPEnabled:    true,
		Version:       "test",
		Log:           logr.Discard(),
	})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/mcp/approvals"},
		{http.MethodPost, "/api/mcp/approvals/prod/x"},
		{http.MethodPost, "/api/mcp/approvals/prod/x/reject"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer operator-persona-token")
			s.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotImplemented, rec.Code)
		})
	}
}
