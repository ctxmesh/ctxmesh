package bff

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/ctxmesh/agentry/internal/controlplane/authz"
	"github.com/ctxmesh/agentry/internal/controlplane/toolregistry"
)

func TestStoreToolRegistryToCRD_Projection(t *testing.T) {
	r := &toolregistry.ToolRegistry{
		Namespace:   trNS,
		Name:        "scalekit-mcp-server",
		Annotations: map[string]string{"agents.ctxmesh.ai/mcp-url": "https://mcp.example"},
		Labels:      map[string]string{labelMCPScope: "personal", labelManagedBy: managedByMCP},
		Tools: []toolregistry.ToolEntry{
			{
				Name: "list_files", Image: "img:1", Source: "curated", ApprovalStatus: "approved",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
	}
	crd := storeToolRegistryToCRD(r)
	assert.Equal(t, trNS, crd.Namespace)
	assert.Equal(t, "scalekit-mcp-server", crd.Name)
	assert.Equal(t, "https://mcp.example", crd.Annotations["agents.ctxmesh.ai/mcp-url"])
	assert.Equal(t, managedByMCP, crd.Labels[labelManagedBy])
	require.Len(t, crd.Spec.Tools, 1)
	assert.Equal(t, "list_files", crd.Spec.Tools[0].Name)
	assert.Equal(t, "approved", crd.Spec.Tools[0].ApprovalStatus)
	require.NotNil(t, crd.Spec.Tools[0].InputSchema)
	assert.JSONEq(t, `{"type":"object"}`, string(crd.Spec.Tools[0].InputSchema.Raw))
}

func TestMCPGetToolRegistry_StorePath(t *testing.T) {
	ctx := context.Background()
	auth := &recordingAuthorizer{}
	s, store := trRetireServer(t, auth)
	_, err := store.Upsert(ctx, toolregistry.ToolRegistry{
		Namespace: trNS, Name: "srv",
		Annotations: map[string]string{annMCPAuthType: oauthAuthType},
		Labels:      map[string]string{labelManagedBy: managedByMCP},
		Tools:       []toolregistry.ToolEntry{{Name: "t1"}},
	})
	require.NoError(t, err)

	tr, err := s.mcpGetToolRegistry(ctx, nil, trNS, "srv")
	require.NoError(t, err)
	assert.Equal(t, "srv", tr.Name)
	assert.Equal(t, oauthAuthType, tr.Annotations[annMCPAuthType])
	assert.Equal(t, managedByMCP, tr.Labels[labelManagedBy])
	// SSAR was the get verb, scoped to ns/name (RBAC parity with the CRD read).
	assert.Equal(t, authz.VerbGet, auth.last.Verb)
	assert.Equal(t, resourceToolRegistries, auth.last.Resource)
	assert.Equal(t, trNS, auth.last.Namespace)
	assert.Equal(t, "srv", auth.last.Name)
}

func TestMCPGetToolRegistry_NotFoundIsAPIError(t *testing.T) {
	s, _ := trRetireServer(t, &recordingAuthorizer{})
	_, err := s.mcpGetToolRegistry(context.Background(), nil, trNS, "nope")
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), "a store miss must present as a k8s NotFound so callers are unchanged")
}

func TestMCPGetToolRegistry_ForbiddenIsAPIError(t *testing.T) {
	ctx := context.Background()
	s, store := trRetireServer(t, &recordingAuthorizer{err: authz.ErrForbidden})
	_, err := store.Upsert(ctx, toolregistry.ToolRegistry{Namespace: trNS, Name: "srv", Tools: []toolregistry.ToolEntry{{Name: "t"}}})
	require.NoError(t, err)

	_, err = s.mcpGetToolRegistry(ctx, nil, trNS, "srv")
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err), "a denial must present as a k8s Forbidden (→ 403)")
}

func TestMCPListToolRegistries_StorePath_LabelFilter(t *testing.T) {
	ctx := context.Background()
	auth := &recordingAuthorizer{}
	s, store := trRetireServer(t, auth)
	_, err := store.Upsert(ctx, toolregistry.ToolRegistry{
		Namespace: trNS, Name: "byo", Labels: map[string]string{labelManagedBy: managedByMCP},
		Tools: []toolregistry.ToolEntry{{Name: "t"}},
	})
	require.NoError(t, err)
	_, err = store.Upsert(ctx, toolregistry.ToolRegistry{
		Namespace: trNS, Name: "curated", Tools: []toolregistry.ToolEntry{{Name: "t"}},
	})
	require.NoError(t, err)

	// Managed-by filter → only the BYO server.
	list, err := s.mcpListToolRegistries(ctx, nil, trNS, map[string]string{labelManagedBy: managedByMCP})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "byo", list.Items[0].Name)
	assert.Equal(t, authz.VerbList, auth.last.Verb)

	// No filter → both.
	all, err := s.mcpListToolRegistries(ctx, nil, trNS, nil)
	require.NoError(t, err)
	assert.Len(t, all.Items, 2)
}

func TestMCPListToolRegistries_ForbiddenIsAPIError(t *testing.T) {
	s, _ := trRetireServer(t, &recordingAuthorizer{err: authz.ErrForbidden})
	_, err := s.mcpListToolRegistries(context.Background(), nil, trNS, nil)
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err))
}
