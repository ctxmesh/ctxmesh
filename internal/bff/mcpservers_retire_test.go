package bff

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

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

	// The catalog is NOT in the K8s API.
	var crd agentsv1alpha1.ToolRegistry
	err = c.Get(ctx, client.ObjectKey{Namespace: trNS, Name: "byo-mcp"}, &crd)
	assert.True(t, apierrors.IsNotFound(err), "no ToolRegistry CRD when retired")
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
