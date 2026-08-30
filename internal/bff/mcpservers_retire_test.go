package bff

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/controlplane"
	"github.com/ctxmesh/agentry/internal/controlplane/authz"
	"github.com/ctxmesh/agentry/internal/controlplane/toolregistry"
)

// Retired delete: the server's ToolRegistry is removed from the store (SSAR-gated).
func TestDeleteMCPServer_RetireDeletesStore(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	auth := &recordingAuthorizer{}
	s, _, _ := newMCPServer(t, c, false)
	s.toolRegistryStore = toolregistry.NewMemStore()
	s.authorizer = auth
	_, err := s.toolRegistryStore.Upsert(ctx, crdToolRegistryToStore(scopedRegistry("scalekit-mcp", scopeOrg, "")))
	require.NoError(t, err)

	rec, resp := deleteMCPServer(t, s, "prod", "scalekit-mcp", "any-token")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, resp.Deleted, "ToolRegistry")
	assert.Equal(t, authz.VerbDelete, auth.last.Verb)
	_, gErr := s.toolRegistryStore.Get(ctx, "prod", "scalekit-mcp")
	assert.ErrorIs(t, gErr, controlplane.ErrNotFound)
}

// A caller who can READ but not DELETE is 403'd and the store row survives.
func TestDeleteMCPServer_RetireDeleteDenied(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)
	s.toolRegistryStore = toolregistry.NewMemStore()
	s.authorizer = &verbAuthorizer{deny: map[string]bool{authz.VerbDelete: true}}
	_, err := s.toolRegistryStore.Upsert(ctx, crdToolRegistryToStore(scopedRegistry("scalekit-mcp", scopeOrg, "")))
	require.NoError(t, err)

	rec, _ := deleteMCPServer(t, s, "prod", "scalekit-mcp", "any-token")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	_, gErr := s.toolRegistryStore.Get(ctx, "prod", "scalekit-mcp")
	assert.NoError(t, gErr, "a denied delete leaves the store row intact")
}

// Retired register: the ToolRegistry catalog is written to the store (SSAR-gated,
// validated), NOT the CRD; the other bundle objects still go via the caller.
func TestCreateMCPObjects_RetireWritesStore(t *testing.T) {
	ctx := context.Background()
	auth := &recordingAuthorizer{}
	s, store := trRetireServer(t, auth)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	created, cErr := s.createMCPObjects(ctx, c, mcpCreateSpec{
		name: "byo-mcp", namespace: trNS, url: "http://10.0.0.5:8080/mcp",
		status:   agentsv1alpha1.ApprovalPending, // pending ⇒ no egress NetworkPolicy (no DNS)
		scope:    scopePersonal,
		owner:    "owner-hash",
		authType: oauthAuthType,
		oauthConfig: mcpOAuthConfig{
			AuthorizationEndpoint: "https://as.example/authorize",
			TokenEndpoint:         "https://as.example/token",
			ClientID:              "cid",
		},
		tools: []discoveredTool{{Name: "list_files", Description: "lists files"}},
	})
	require.Nil(t, cErr)
	require.Len(t, created, 1)

	got, err := store.Get(ctx, trNS, "byo-mcp")
	require.NoError(t, err)
	require.Len(t, got.Tools, 1)
	assert.Equal(t, "list_files", got.Tools[0].Name)
	assert.Equal(t, oauthAuthType, got.Annotations[annMCPAuthType])
	assert.Equal(t, "https://as.example/authorize", got.Annotations[annMCPOAuthAuthEndpoint])
	assert.Equal(t, scopePersonal, got.Labels[labelMCPScope])
	assert.Equal(t, "owner-hash", got.Labels[labelMCPOwner])
	assert.Equal(t, authz.VerbCreate, auth.last.Verb)
}

// A denied caller cannot register — 403, no store row.
func TestCreateMCPObjects_RetireForbidden(t *testing.T) {
	ctx := context.Background()
	s, store := trRetireServer(t, &recordingAuthorizer{err: authz.ErrForbidden})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	_, cErr := s.createMCPObjects(ctx, c, mcpCreateSpec{
		name: "byo", namespace: trNS, url: "http://10.0.0.5:8080/mcp",
		status: agentsv1alpha1.ApprovalPending,
		tools:  []discoveredTool{{Name: "t"}},
	})
	require.NotNil(t, cErr)
	assert.Equal(t, http.StatusForbidden, cErr.status)
	_, err := store.Get(ctx, trNS, "byo")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)
}

// A catalog that fails in-app validation (no tools) → 422, no store row.
func TestCreateMCPObjects_RetireInvalid422(t *testing.T) {
	ctx := context.Background()
	s, store := trRetireServer(t, &recordingAuthorizer{})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	_, cErr := s.createMCPObjects(ctx, c, mcpCreateSpec{
		name: "byo", namespace: trNS, url: "http://10.0.0.5:8080/mcp",
		status: agentsv1alpha1.ApprovalPending,
		tools:  nil, // MinItems=1 ⇒ Validate rejects
	})
	require.NotNil(t, cErr)
	assert.Equal(t, http.StatusUnprocessableEntity, cErr.status)
	_, err := store.Get(ctx, trNS, "byo")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)
}

// A duplicate register (same name) → 409 from the atomic store Create.
func TestCreateMCPObjects_RetireConflict409(t *testing.T) {
	ctx := context.Background()
	s, store := trRetireServer(t, &recordingAuthorizer{})
	seedStoreTR(t, store, "byo", []toolregistry.ToolEntry{{Name: "t"}})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	_, cErr := s.createMCPObjects(ctx, c, mcpCreateSpec{
		name: "byo", namespace: trNS, url: "http://10.0.0.5:8080/mcp",
		status: agentsv1alpha1.ApprovalPending,
		tools:  []discoveredTool{{Name: "t"}},
	})
	require.NotNil(t, cErr)
	assert.Equal(t, http.StatusConflict, cErr.status)
}
