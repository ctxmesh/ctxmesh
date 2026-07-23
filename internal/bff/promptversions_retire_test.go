package bff

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
	"github.com/ctxmesh/agent-engine/internal/controlplane/promptversion"
)

// pvRetireServer builds a Server with the store wired — PromptVersion is Postgres-only (ADR 0044), so
// wiring the store IS the retirement mode; writes go to the store behind the injected authorizer.
func pvRetireServer(t *testing.T, auth authz.Authorizer) (*Server, promptversion.Store) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	store := promptversion.NewMemStore()
	s.promptStore = store
	s.authorizer = auth
	return s, store
}

func pvCreateReq(name string) PromptVersionCreateRequest {
	return PromptVersionCreateRequest{
		Name: name, Namespace: pvNS,
		Git: GitPromptSourceDTO{Repo: "github.com/acme/p", Ref: "v1", Path: "s.md"},
	}
}

// CREATE writes to Postgres (no CRD), SSAR-gated on the create verb, and is
// reflected in the store.
func TestPromptVersionRetire_CreateWritesStore(t *testing.T) {
	ctx := context.Background()
	auth := &recordingAuthorizer{}
	s, store := pvRetireServer(t, auth)

	detail, code, body := createPromptVersion(t, s, pvCreateReq("pv1"))
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, "pv1", detail.Name)
	assert.Equal(t, authz.VerbCreate, auth.last.Verb)
	assert.Equal(t, resourcePromptVersions, auth.last.Resource)
	// The SSAR is scoped to the caller-supplied namespace (the anti-cross-namespace
	// invariant), and create carries no name.
	assert.Equal(t, pvNS, auth.last.Namespace)
	assert.Empty(t, auth.last.Name)

	got, err := store.Get(ctx, pvNS, "pv1")
	require.NoError(t, err)
	assert.Equal(t, "v1", got.Ref)
}

// A denied caller cannot create — 403, and no row is written.
func TestPromptVersionRetire_CreateForbidden(t *testing.T) {
	ctx := context.Background()
	s, store := pvRetireServer(t, &recordingAuthorizer{err: authz.ErrForbidden})

	_, code, _ := createPromptVersion(t, s, pvCreateReq("pv1"))
	assert.Equal(t, http.StatusForbidden, code)
	_, err := store.Get(ctx, pvNS, "pv1")
	assert.ErrorIs(t, err, controlplane.ErrNotFound, "no row on a denied create")
}

// An invalid (but non-empty) object name → 422 from the in-app validator, matching
// the API server's DNS-1123 rejection on the CRD path. (Empty git is caught earlier
// at 400 by buildPromptVersionGit, identically on both paths.)
func TestPromptVersionRetire_CreateInvalid422(t *testing.T) {
	s, _ := pvRetireServer(t, &recordingAuthorizer{})
	_, code, _ := createPromptVersion(t, s, pvCreateReq("Bad_Name"))
	assert.Equal(t, http.StatusUnprocessableEntity, code)
}

// Creating an existing name → 409 (CRD create parity).
func TestPromptVersionRetire_CreateConflict409(t *testing.T) {
	ctx := context.Background()
	s, store := pvRetireServer(t, &recordingAuthorizer{})
	_, err := store.Upsert(ctx, promptversion.PromptVersion{Namespace: pvNS, Name: "pv1", Repo: "r", Ref: "v", Path: "p"})
	require.NoError(t, err)

	_, code, _ := createPromptVersion(t, s, pvCreateReq("pv1"))
	assert.Equal(t, http.StatusConflict, code)
}

// UPDATE upserts the store row.
func TestPromptVersionRetire_UpdateWritesStore(t *testing.T) {
	ctx := context.Background()
	s, store := pvRetireServer(t, &recordingAuthorizer{})
	_, err := store.Upsert(ctx, promptversion.PromptVersion{Namespace: pvNS, Name: "pv1", Repo: "r", Ref: "v1", Path: "p"})
	require.NoError(t, err)

	_, code, body := putPromptVersion(t, s, pvNS, "pv1", PromptVersionUpdateRequest{
		Git: GitPromptSourceDTO{Repo: "github.com/acme/p", Ref: "v2", Path: "s.md"},
	})
	require.Equal(t, http.StatusOK, code, body)
	got, err := store.Get(ctx, pvNS, "pv1")
	require.NoError(t, err)
	assert.Equal(t, "v2", got.Ref)
}

// DELETE removes the store row (SSAR-gated), 204.
func TestPromptVersionRetire_DeleteRemovesStore(t *testing.T) {
	ctx := context.Background()
	auth := &recordingAuthorizer{}
	s, store := pvRetireServer(t, auth)
	_, err := store.Upsert(ctx, promptversion.PromptVersion{Namespace: pvNS, Name: "pv1", Repo: "r", Ref: "v", Path: "p"})
	require.NoError(t, err)

	rec := deletePromptVersion(t, s, pvNS, "pv1")
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, authz.VerbDelete, auth.last.Verb)
	assert.Equal(t, pvNS, auth.last.Namespace)
	assert.Equal(t, "pv1", auth.last.Name)
	_, err = store.Get(ctx, pvNS, "pv1")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)
}

// A non-forbidden SSAR/API error on a WRITE is a 500 (fail-closed — never write).
func TestPromptVersionRetire_CreateAuthzError500(t *testing.T) {
	ctx := context.Background()
	s, store := pvRetireServer(t, &recordingAuthorizer{err: errors.New("apiserver down")})
	_, code, _ := createPromptVersion(t, s, pvCreateReq("pv1"))
	assert.Equal(t, http.StatusInternalServerError, code)
	_, err := store.Get(ctx, pvNS, "pv1")
	assert.ErrorIs(t, err, controlplane.ErrNotFound, "no row on an authz error")
}

// DELETE of an absent object → 404 (CRD delete parity).
func TestPromptVersionRetire_DeleteAbsent404(t *testing.T) {
	s, _ := pvRetireServer(t, &recordingAuthorizer{})
	rec := deletePromptVersion(t, s, pvNS, "nope")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// DELETE denied → 403, row survives.
func TestPromptVersionRetire_DeleteForbidden(t *testing.T) {
	ctx := context.Background()
	s, store := pvRetireServer(t, &recordingAuthorizer{err: authz.ErrForbidden})
	_, err := store.Upsert(ctx, promptversion.PromptVersion{Namespace: pvNS, Name: "pv1", Repo: "r", Ref: "v", Path: "p"})
	require.NoError(t, err)

	rec := deletePromptVersion(t, s, pvNS, "pv1")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	_, err = store.Get(ctx, pvNS, "pv1")
	assert.NoError(t, err, "row survives a denied delete")
}

// PUT rename guard: a body name that mismatches the URL name → 400 (S2 coverage).
func TestPromptVersionRetire_UpdateRenameGuard400(t *testing.T) {
	s, _ := pvRetireServer(t, &recordingAuthorizer{})
	_, code, _ := putPromptVersion(t, s, pvNS, "pv1", PromptVersionUpdateRequest{
		Name: "different", Git: GitPromptSourceDTO{Repo: "r", Ref: "v", Path: "p"},
	})
	assert.Equal(t, http.StatusBadRequest, code)
}

// buildPromptVersionGit rejects an empty git pointer at 400 BEFORE the store branch — identically on the
// create path (S3 coverage; proves the comment on the invalid-name test, not just asserts it).
func TestPromptVersionRetire_CreateMissingGit400(t *testing.T) {
	s, _ := pvRetireServer(t, &recordingAuthorizer{})
	req := pvCreateReq("pv1")
	req.Git.Repo = ""
	_, code, _ := createPromptVersion(t, s, req)
	assert.Equal(t, http.StatusBadRequest, code)
}
