package bff

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/promptversion"
)

// m40.4: the PromptVersion CRUD handlers dual-write to the control-plane store when it's configured —
// the CRD stays the RBAC-gated source of truth, the store is a best-effort mirror. nil store → no-op.
func TestPromptVersionDualWriteToStore(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	store := promptversion.NewMemStore()
	s.promptStore = store

	git := GitPromptSourceDTO{Repo: "https://git/x.git", Ref: "v1", Path: "p/s.txt"}

	// Create → mirrored to the store.
	_, code, body := createPromptVersion(t, s, PromptVersionCreateRequest{Name: "greeter", Namespace: "default", Git: git})
	require.Equal(t, http.StatusCreated, code, body)
	got, err := store.Get(ctx, "default", "greeter")
	require.NoError(t, err)
	assert.Equal(t, "v1", got.Ref)
	assert.Equal(t, "https://git/x.git", got.Repo)

	// Update → the mirror reflects the new ref.
	git.Ref = "v2"
	_, code, body = putPromptVersion(t, s, "default", "greeter", PromptVersionUpdateRequest{Name: "greeter", Git: git})
	require.Equal(t, http.StatusOK, code, body)
	got, err = store.Get(ctx, "default", "greeter")
	require.NoError(t, err)
	assert.Equal(t, "v2", got.Ref)
	assert.EqualValues(t, 2, got.Version) // second write bumped the store version

	// Delete → removed from the store.
	rec := deletePromptVersion(t, s, "default", "greeter")
	require.Equal(t, http.StatusNoContent, rec.Code)
	_, err = store.Get(ctx, "default", "greeter")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)
}

// With no store configured, the CRUD path is unchanged (the mirror is a no-op, never a nil panic).
func TestPromptVersionNoStore_NoOp(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	require.Nil(t, s.promptStore)

	_, code, body := createPromptVersion(t, s, PromptVersionCreateRequest{
		Name: "x", Namespace: "default", Git: GitPromptSourceDTO{Repo: "r", Ref: "v", Path: "p"},
	})
	require.Equal(t, http.StatusCreated, code, body)
	rec := deletePromptVersion(t, s, "default", "x")
	require.Equal(t, http.StatusNoContent, rec.Code)
}
