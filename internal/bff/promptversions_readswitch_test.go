package bff

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/controlplane/authz"
	"github.com/ctxmesh/agentry/internal/controlplane/promptversion"
	"github.com/ctxmesh/agentry/internal/prompt"
)

func pvReadSwitchServer(t *testing.T, auth authz.Authorizer, resolver prompt.Resolver) (*Server, promptversion.Store) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	var s *Server
	if resolver != nil {
		s = newCallerServerWithResolver(t, &fakeCallerClientFactory{client: c}, resolver)
	} else {
		s = newCallerServer(t, &fakeCallerClientFactory{client: c})
	}
	store := promptversion.NewMemStore()
	s.promptStore = store
	s.authorizer = auth
	return s, store
}

func seedPV(t *testing.T, store promptversion.Store, name, repo, ref, path string) {
	t.Helper()
	_, err := store.Upsert(context.Background(), promptversion.PromptVersion{
		Namespace: pvNS, Name: name, Repo: repo, Ref: ref, Path: path,
	})
	require.NoError(t, err)
}

// m43.5: LIST reads from the store behind a caller-scoped SSAR (list promptversions).
func TestPromptVersionListReadSwitch_Allowed(t *testing.T) {
	auth := &recordingAuthorizer{}
	s, store := pvReadSwitchServer(t, auth, nil)
	seedPV(t, store, "pv1", "github.com/acme/p", "v1", "a.md")

	body, code := getPromptVersions(t, s, "namespace="+pvNS)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "pv1", body.Items[0].Name)
	assert.Equal(t, "v1", body.Items[0].Git.Ref)
	assert.Equal(t, authz.VerbList, auth.last.Verb)
	assert.Equal(t, resourcePromptVersions, auth.last.Resource)
}

// The security-critical denial: 403 and no store data leaks.
func TestPromptVersionListReadSwitch_Forbidden(t *testing.T) {
	s, store := pvReadSwitchServer(t, &recordingAuthorizer{err: authz.ErrForbidden}, nil)
	seedPV(t, store, "secret-pv", "r", "ref", "p")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/promptversions?namespace="+pvNS, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.NotContains(t, rec.Body.String(), "secret-pv")
}

func TestPromptVersionGetReadSwitch_Allowed(t *testing.T) {
	auth := &recordingAuthorizer{}
	s, store := pvReadSwitchServer(t, auth, nil)
	seedPV(t, store, "pv1", "github.com/acme/p", "v1.2.3", "sys.md")

	detail, code, body := getPromptVersion(t, s, pvNS, "pv1")
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "pv1", detail.Name)
	assert.Equal(t, "v1.2.3", detail.Git.Ref)
	assert.Equal(t, "sys.md", detail.Git.Path)
	assert.Equal(t, authz.VerbGet, auth.last.Verb)
	assert.Equal(t, "pv1", auth.last.Name)
}

func TestPromptVersionGetReadSwitch_Forbidden(t *testing.T) {
	s, store := pvReadSwitchServer(t, &recordingAuthorizer{err: authz.ErrForbidden}, nil)
	seedPV(t, store, "secret-pv", "r", "ref", "p")

	_, code, body := getPromptVersion(t, s, pvNS, "secret-pv")
	assert.Equal(t, http.StatusForbidden, code)
	assert.NotContains(t, body, "secret-pv")
}

func TestPromptVersionGetReadSwitch_NotFound(t *testing.T) {
	s, _ := pvReadSwitchServer(t, &recordingAuthorizer{}, nil)
	_, code, _ := getPromptVersion(t, s, pvNS, "nope")
	assert.Equal(t, http.StatusNotFound, code)
}

// The diff endpoint reads BOTH versions' git pointers from the store (behind an
// SSAR each) and resolves them — proving the store-backed diff path (m43.5).
func TestPromptVersionDiffReadSwitch_Allowed(t *testing.T) {
	fromSrc := agentsv1alpha1.GitPromptSource{Repo: "github.com/acme/p", Ref: "v1", Path: "s.md"}
	toSrc := agentsv1alpha1.GitPromptSource{Repo: "github.com/acme/p", Ref: "v2", Path: "s.md"}
	resolver := prompt.NewFixtureResolver().
		Seed(fromSrc, "one\n").
		Seed(toSrc, "one\ntwo\n")

	auth := &recordingAuthorizer{}
	s, store := pvReadSwitchServer(t, auth, resolver)
	seedPV(t, store, "pv-v1", fromSrc.Repo, fromSrc.Ref, fromSrc.Path)
	seedPV(t, store, "pv-v2", toSrc.Repo, toSrc.Ref, toSrc.Path)

	resp, code, body := getPromptVersionDiff(t, s, pvNS, "pv-v2", "pv-v1")
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "textual", resp.ResolveMode)
	assert.Contains(t, resp.Diff, "+two")
	assert.Equal(t, authz.VerbGet, auth.last.Verb)
	assert.Equal(t, 2, auth.count, "the diff must SSAR-gate BOTH versions (from + to), no partial disclosure")
}

// A denied caller can't diff store-backed versions either.
func TestPromptVersionDiffReadSwitch_Forbidden(t *testing.T) {
	resolver := prompt.NewFixtureResolver()
	s, store := pvReadSwitchServer(t, &recordingAuthorizer{err: authz.ErrForbidden}, resolver)
	seedPV(t, store, "pv-v1", "r", "v1", "s.md")
	seedPV(t, store, "pv-v2", "r", "v2", "s.md")

	_, code, _ := getPromptVersionDiff(t, s, pvNS, "pv-v2", "pv-v1")
	assert.Equal(t, http.StatusForbidden, code)
}

// The store-backed list pushes q (search), limit (page size), and cursor (page token) down to the store —
// the handler-level contract after the read-switch (the store's filter/sort/paginate is conformance-tested
// separately). Closes the coverage the deleted CRD-path list tests held (ADR 0044 m44.5b, Fable Risk 2).
func TestPromptVersionListReadSwitch_FilterAndPaginate(t *testing.T) {
	s, store := pvReadSwitchServer(t, &recordingAuthorizer{}, nil)
	seedPV(t, store, "alpha", "r", "v", "p")
	seedPV(t, store, "alpha-2", "r", "v", "p")
	seedPV(t, store, "beta", "r", "v", "p")

	// q= filters by name substring (2 of 3 match "alpha").
	body, code := getPromptVersions(t, s, "namespace="+pvNS+"&q=alpha")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, body.Items, 2)

	// limit=1 paginates; the opaque cursor fetches the next page (a distinct item).
	page1, code := getPromptVersions(t, s, "namespace="+pvNS+"&q=alpha&limit=1")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page1.Items, 1)
	require.NotEmpty(t, page1.NextCursor)
	page2, code := getPromptVersions(t, s, "namespace="+pvNS+"&q=alpha&limit=1&cursor="+page1.NextCursor)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, page2.Items, 1)
	assert.NotEqual(t, page1.Items[0].Name, page2.Items[0].Name)
}

// Diff error paths (S1 coverage — these handler branches survived the CRD-path test deletion):
// no resolver → 501, missing ?from → 400, a version absent from the store → 404, an unresolvable git
// pointer → 404, and byte-identical content → 200 identical:true. (A transient resolve error → 502 needs a
// custom erroring resolver; the fixture only models Seed/SeedNotFound.)
func TestPromptVersionDiffReadSwitch_NoResolver501(t *testing.T) {
	s, store := pvReadSwitchServer(t, &recordingAuthorizer{}, nil) // nil resolver
	seedPV(t, store, "pv-a", "r", "v1", "s.md")
	seedPV(t, store, "pv-b", "r", "v2", "s.md")
	_, code, _ := getPromptVersionDiff(t, s, pvNS, "pv-b", "pv-a")
	assert.Equal(t, http.StatusNotImplemented, code)
}

func TestPromptVersionDiffReadSwitch_MissingFrom400(t *testing.T) {
	s, store := pvReadSwitchServer(t, &recordingAuthorizer{}, prompt.NewFixtureResolver())
	seedPV(t, store, "pv-b", "r", "v2", "s.md")
	_, code, _ := getPromptVersionDiff(t, s, pvNS, "pv-b", "") // no ?from
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestPromptVersionDiffReadSwitch_VersionNotFound404(t *testing.T) {
	s, _ := pvReadSwitchServer(t, &recordingAuthorizer{}, prompt.NewFixtureResolver())
	_, code, _ := getPromptVersionDiff(t, s, pvNS, "ghost-to", "ghost-from") // neither in the store
	assert.Equal(t, http.StatusNotFound, code)
}

func TestPromptVersionDiffReadSwitch_Unresolvable404(t *testing.T) {
	fromSrc := agentsv1alpha1.GitPromptSource{Repo: "github.com/acme/p", Ref: "v1", Path: "s.md"}
	toSrc := agentsv1alpha1.GitPromptSource{Repo: "github.com/acme/p", Ref: "v2", Path: "s.md"}
	resolver := prompt.NewFixtureResolver().Seed(fromSrc, "one\n").SeedNotFound(toSrc)
	s, store := pvReadSwitchServer(t, &recordingAuthorizer{}, resolver)
	seedPV(t, store, "pv-v1", fromSrc.Repo, fromSrc.Ref, fromSrc.Path)
	seedPV(t, store, "pv-v2", toSrc.Repo, toSrc.Ref, toSrc.Path)
	_, code, _ := getPromptVersionDiff(t, s, pvNS, "pv-v2", "pv-v1")
	assert.Equal(t, http.StatusNotFound, code)
}

func TestPromptVersionDiffReadSwitch_Identical200(t *testing.T) {
	fromSrc := agentsv1alpha1.GitPromptSource{Repo: "github.com/acme/p", Ref: "v1", Path: "s.md"}
	toSrc := agentsv1alpha1.GitPromptSource{Repo: "github.com/acme/p", Ref: "v2", Path: "s.md"}
	resolver := prompt.NewFixtureResolver().Seed(fromSrc, "same\n").Seed(toSrc, "same\n")
	s, store := pvReadSwitchServer(t, &recordingAuthorizer{}, resolver)
	seedPV(t, store, "pv-v1", fromSrc.Repo, fromSrc.Ref, fromSrc.Path)
	seedPV(t, store, "pv-v2", toSrc.Repo, toSrc.Ref, toSrc.Path)
	resp, code, body := getPromptVersionDiff(t, s, pvNS, "pv-v2", "pv-v1")
	require.Equal(t, http.StatusOK, code, body)
	assert.True(t, resp.Identical)
	assert.Empty(t, resp.Diff)
}
