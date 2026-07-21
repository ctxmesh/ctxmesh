package bff

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

// m41.2: the ToolRegistry CRUD handlers dual-write to the control-plane store when configured — the CRD
// stays the RBAC-gated source of truth, the store is a best-effort mirror. nil store → no-op.
func TestToolRegistryDualWriteToStore(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	store := toolregistry.NewMemStore()
	s.toolRegistryStore = store

	// Create → mirrored to the store (tools[] round-trips).
	_, code, body := createToolRegistry(t, s, ToolRegistryCreateRequest{
		Name: "reg1", Namespace: trNS,
		Tools: []ToolEntryCreateDTO{{Name: "t1", URL: "https://mcp.example", Source: "user-added"}},
	})
	require.Equal(t, http.StatusCreated, code, body)
	got, err := store.Get(ctx, trNS, "reg1")
	require.NoError(t, err)
	require.Len(t, got.Tools, 1)
	assert.Equal(t, "t1", got.Tools[0].Name)
	assert.Equal(t, "https://mcp.example", got.Tools[0].URL)

	// Update → the mirror reflects the new catalog.
	_, code, body = putToolRegistry(t, s, "reg1", ToolRegistryUpdateRequest{
		Name:  "reg1",
		Tools: []ToolEntryCreateDTO{{Name: "t1"}, {Name: "t2"}},
	})
	require.Equal(t, http.StatusOK, code, body)
	got, err = store.Get(ctx, trNS, "reg1")
	require.NoError(t, err)
	assert.Len(t, got.Tools, 2)
	assert.EqualValues(t, 2, got.Version)

	// Delete → removed from the store.
	rec := deleteToolRegistry(t, s, "reg1")
	require.Equal(t, http.StatusNoContent, rec.Code)
	_, err = store.Get(ctx, trNS, "reg1")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)
}

// No store → the CRUD path is unchanged (mirror is a no-op, never a nil panic).
func TestToolRegistryNoStore_NoOp(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	require.Nil(t, s.toolRegistryStore)

	_, code, body := createToolRegistry(t, s, ToolRegistryCreateRequest{
		Name: "x", Namespace: trNS, Tools: []ToolEntryCreateDTO{{Name: "t"}},
	})
	require.Equal(t, http.StatusCreated, code, body)
	require.Equal(t, http.StatusNoContent, deleteToolRegistry(t, s, "x").Code)
}
