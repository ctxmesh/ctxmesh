package bff

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agentry/internal/controlplane"
	"github.com/ctxmesh/agentry/internal/controlplane/authz"
	"github.com/ctxmesh/agentry/internal/controlplane/toolregistry"
)

// trRetireServer builds a Server with the ToolRegistry store wired AND retire mode
// ON (RETIRE_TR) — writes go to the store behind the injected authorizer. Unlike
// PromptVersion, the store alone is not retirement; the flag flips the write path.
func trRetireServer(t *testing.T, auth authz.Authorizer) (*Server, toolregistry.Store) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	store := toolregistry.NewMemStore()
	s.toolRegistryStore = store
	s.authorizer = auth
	return s, store
}

func trCreateReq(name string) ToolRegistryCreateRequest {
	return ToolRegistryCreateRequest{
		Name: name, Namespace: trNS,
		Tools: []ToolEntryCreateDTO{{Name: "search", Description: "web search", Source: "curated"}},
	}
}

//nolint:unparam // name is fixed across current callers but the helper stays general.
func seedStoreTR(t *testing.T, store toolregistry.Store, name string, tools []toolregistry.ToolEntry) {
	t.Helper()
	_, err := store.Upsert(context.Background(), toolregistry.ToolRegistry{Namespace: trNS, Name: name, Tools: tools})
	require.NoError(t, err)
}

// CREATE writes Postgres (no CRD), SSAR-gated on the create verb, reflected in the store.
func TestToolRegistryRetire_CreateWritesStore(t *testing.T) {
	ctx := context.Background()
	auth := &recordingAuthorizer{}
	s, store := trRetireServer(t, auth)

	detail, code, body := createToolRegistry(t, s, trCreateReq("acme-mcp"))
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, "acme-mcp", detail.Name)
	assert.Equal(t, authz.VerbCreate, auth.last.Verb)
	assert.Equal(t, resourceToolRegistries, auth.last.Resource)
	assert.Equal(t, trNS, auth.last.Namespace)
	assert.Empty(t, auth.last.Name)

	got, err := store.Get(ctx, trNS, "acme-mcp")
	require.NoError(t, err)
	require.Len(t, got.Tools, 1)
	assert.Equal(t, "search", got.Tools[0].Name)
}

// A denied caller cannot create — 403, no row.
func TestToolRegistryRetire_CreateForbidden(t *testing.T) {
	ctx := context.Background()
	s, store := trRetireServer(t, &recordingAuthorizer{err: authz.ErrForbidden})
	_, code, _ := createToolRegistry(t, s, trCreateReq("acme-mcp"))
	assert.Equal(t, http.StatusForbidden, code)
	_, err := store.Get(ctx, trNS, "acme-mcp")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)
}

// An invalid object name → 422 from the in-app validator (DNS-1123 parity).
func TestToolRegistryRetire_CreateInvalid422(t *testing.T) {
	s, _ := trRetireServer(t, &recordingAuthorizer{})
	_, code, _ := createToolRegistry(t, s, trCreateReq("Bad_Name"))
	assert.Equal(t, http.StatusUnprocessableEntity, code)
}

// A duplicate tool name in the body → 422 from the uniqueness rule (CRD XValidation parity).
func TestToolRegistryRetire_CreateDuplicateToolName422(t *testing.T) {
	s, _ := trRetireServer(t, &recordingAuthorizer{})
	req := trCreateReq("acme-mcp")
	req.Tools = []ToolEntryCreateDTO{{Name: "dup"}, {Name: "dup"}}
	_, code, _ := createToolRegistry(t, s, req)
	assert.Equal(t, http.StatusUnprocessableEntity, code)
}

// Empty tools → 400 (buildStoreToolEntries, before the store branch — CRD-path parity).
func TestToolRegistryRetire_CreateEmptyTools400(t *testing.T) {
	s, _ := trRetireServer(t, &recordingAuthorizer{})
	req := trCreateReq("acme-mcp")
	req.Tools = nil
	_, code, _ := createToolRegistry(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code)
}

// Creating an existing name → 409 (atomic Create, CRD-create parity).
func TestToolRegistryRetire_CreateConflict409(t *testing.T) {
	s, store := trRetireServer(t, &recordingAuthorizer{})
	seedStoreTR(t, store, "acme-mcp", []toolregistry.ToolEntry{{Name: "search"}})
	_, code, _ := createToolRegistry(t, s, trCreateReq("acme-mcp"))
	assert.Equal(t, http.StatusConflict, code)
}

// A non-forbidden authz error on a WRITE is a 500 (fail-closed — never write).
func TestToolRegistryRetire_CreateAuthzError500(t *testing.T) {
	ctx := context.Background()
	s, store := trRetireServer(t, &recordingAuthorizer{err: errors.New("apiserver down")})
	_, code, _ := createToolRegistry(t, s, trCreateReq("acme-mcp"))
	assert.Equal(t, http.StatusInternalServerError, code)
	_, err := store.Get(ctx, trNS, "acme-mcp")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)
}

// UPDATE upserts the store row.
func TestToolRegistryRetire_UpdateWritesStore(t *testing.T) {
	ctx := context.Background()
	s, store := trRetireServer(t, &recordingAuthorizer{})
	seedStoreTR(t, store, "acme-mcp", []toolregistry.ToolEntry{{Name: "search"}})

	_, code, body := putToolRegistry(t, s, "acme-mcp", ToolRegistryUpdateRequest{
		Tools: []ToolEntryCreateDTO{{Name: "search"}, {Name: "fetch"}},
	})
	require.Equal(t, http.StatusOK, code, body)
	got, err := store.Get(ctx, trNS, "acme-mcp")
	require.NoError(t, err)
	assert.Len(t, got.Tools, 2)
}

// UPDATE preserves each existing entry's controller-owned approvalStatus (the
// console PUT can never flip it) — read from the STORE's live row, not the CRD.
func TestToolRegistryRetire_UpdatePreservesApproval(t *testing.T) {
	ctx := context.Background()
	s, store := trRetireServer(t, &recordingAuthorizer{})
	seedStoreTR(t, store, "acme-mcp", []toolregistry.ToolEntry{
		{Name: "byo", Source: "user-added", ApprovalStatus: "pending"},
	})

	// PUT the same tool — the DTO carries no approvalStatus (the struct has no such field).
	_, code, body := putToolRegistry(t, s, "acme-mcp", ToolRegistryUpdateRequest{
		Tools: []ToolEntryCreateDTO{{Name: "byo", Description: "edited"}},
	})
	require.Equal(t, http.StatusOK, code, body)
	got, err := store.Get(ctx, trNS, "acme-mcp")
	require.NoError(t, err)
	require.Len(t, got.Tools, 1)
	assert.Equal(t, "pending", got.Tools[0].ApprovalStatus, "approvalStatus must survive a console PUT")
	assert.Equal(t, "edited", got.Tools[0].Description)
}

// UPDATE of an absent object → 404 (store live-read miss).
func TestToolRegistryRetire_UpdateNotFound404(t *testing.T) {
	s, _ := trRetireServer(t, &recordingAuthorizer{})
	_, code, _ := putToolRegistry(t, s, "nope", ToolRegistryUpdateRequest{
		Tools: []ToolEntryCreateDTO{{Name: "search"}},
	})
	assert.Equal(t, http.StatusNotFound, code)
}

// PUT rename guard: a body name mismatching the URL name → 400.
func TestToolRegistryRetire_UpdateRenameGuard400(t *testing.T) {
	s, store := trRetireServer(t, &recordingAuthorizer{})
	seedStoreTR(t, store, "acme-mcp", []toolregistry.ToolEntry{{Name: "search"}})
	_, code, _ := putToolRegistry(t, s, "acme-mcp", ToolRegistryUpdateRequest{
		Name: "different", Tools: []ToolEntryCreateDTO{{Name: "search"}},
	})
	assert.Equal(t, http.StatusBadRequest, code)
}

// DELETE removes the store row (SSAR-gated), 204.
func TestToolRegistryRetire_DeleteRemovesStore(t *testing.T) {
	ctx := context.Background()
	auth := &recordingAuthorizer{}
	s, store := trRetireServer(t, auth)
	seedStoreTR(t, store, "acme-mcp", []toolregistry.ToolEntry{{Name: "search"}})

	rec := deleteToolRegistry(t, s, "acme-mcp")
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, authz.VerbDelete, auth.last.Verb)
	assert.Equal(t, trNS, auth.last.Namespace)
	assert.Equal(t, "acme-mcp", auth.last.Name)
	_, err := store.Get(ctx, trNS, "acme-mcp")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)
}

// DELETE of an absent object → 404.
func TestToolRegistryRetire_DeleteAbsent404(t *testing.T) {
	s, _ := trRetireServer(t, &recordingAuthorizer{})
	rec := deleteToolRegistry(t, s, "nope")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// DELETE denied → 403, row survives.
func TestToolRegistryRetire_DeleteForbidden(t *testing.T) {
	ctx := context.Background()
	s, store := trRetireServer(t, &recordingAuthorizer{err: authz.ErrForbidden})
	seedStoreTR(t, store, "acme-mcp", []toolregistry.ToolEntry{{Name: "search"}})

	rec := deleteToolRegistry(t, s, "acme-mcp")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	_, err := store.Get(ctx, trNS, "acme-mcp")
	assert.NoError(t, err, "row survives a denied delete")
}

// toolRegistryIndexFromStore: agent-create's tool→registry resolution reads the
// store when retired, mapping each tool to its registry + pins + scope/owner.
func TestToolRegistryIndex_FromStore(t *testing.T) {
	ctx := context.Background()
	store := toolregistry.NewMemStore()
	_, err := store.Upsert(ctx, toolregistry.ToolRegistry{
		Namespace: trNS, Name: "scalekit-mcp-server",
		Tools:  []toolregistry.ToolEntry{{Name: "create_organization", URL: "https://mcp.scalekit.com/"}},
		Labels: map[string]string{labelMCPScope: "personal", labelMCPOwner: "owner-hash"},
	})
	require.NoError(t, err)

	// the store path.
	idx := toolRegistryIndex(ctx, store, trNS)
	loc, ok := idx["create_organization"]
	require.True(t, ok)
	assert.Equal(t, "scalekit-mcp-server", loc.registry)
	assert.Equal(t, "https://mcp.scalekit.com/", loc.url)
	assert.Equal(t, "personal", loc.scope)
	assert.Equal(t, "owner-hash", loc.owner)

	// A namespace with no registries → empty (not nil-panic).
	assert.Empty(t, toolRegistryIndex(ctx, store, "other-ns"))
}
