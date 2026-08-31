package bff

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// retireApproveServer wires an MCP server in retire mode with a store seeded from a
// CRD fixture; the fake client `c` still backs the non-TR objects (the egress NP).
func retireApproveServer(t *testing.T, c client.Client, auth authz.Authorizer, seed *agentsv1alpha1.ToolRegistry) (*Server, toolregistry.Store) {
	t.Helper()
	s, _, _ := newMCPServer(t, c, true)
	store := toolregistry.NewMemStore()
	s.toolRegistryStore = store
	s.authorizer = auth
	_, err := store.Upsert(context.Background(), crdToolRegistryToStore(seed))
	require.NoError(t, err)
	return s, store
}

// Retired approve: the approvalStatus flip persists to the STORE (no CRD), and the
// egress NetworkPolicy is still opened via the caller (a K8s object).
func TestApproveMCP_RetireWritesStore(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, store := retireApproveServer(t, c, &recordingAuthorizer{}, pendingMCPRegistry("weather-mcp", false))

	req := httptest.NewRequest(http.MethodPost, "/api/mcp/approvals/prod/weather-mcp", nil)
	req.Header.Set("Authorization", "Bearer operator-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got, err := store.Get(ctx, "prod", "weather-mcp")
	require.NoError(t, err)
	assert.Equal(t, agentsv1alpha1.ApprovalApproved, got.Annotations[annMCPStatus])
	for _, e := range got.Tools {
		assert.Equal(t, agentsv1alpha1.ApprovalApproved, e.ApprovalStatus, "approve makes every entry bindable")
	}
	// The egress NetworkPolicy WAS opened (still a K8s object).
	var np networkingv1.NetworkPolicy
	assert.NoError(t, c.Get(ctx, client.ObjectKey{Name: "weather-mcp" + networkPolicyMCPSuffix, Namespace: "prod"}, &np))
}

// A denied caller's approve is a 403 with NO side effects: the row stays pending
// and NO egress is opened (the operator-only invariant survives retirement).
func TestApproveMCP_RetireForbiddenNoEgress(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, store := retireApproveServer(t, c, &recordingAuthorizer{err: authz.ErrForbidden}, pendingMCPRegistry("weather-mcp", false))

	req := httptest.NewRequest(http.MethodPost, "/api/mcp/approvals/prod/weather-mcp", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	got, err := store.Get(ctx, "prod", "weather-mcp")
	require.NoError(t, err)
	assert.Equal(t, agentsv1alpha1.ApprovalPending, got.Annotations[annMCPStatus], "denied approve leaves the row pending")
	var np networkingv1.NetworkPolicy
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Name: "weather-mcp" + networkPolicyMCPSuffix, Namespace: "prod"}, &np)),
		"a denied approve opens NO egress")
}
