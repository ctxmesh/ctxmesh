package bff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/skill"
)

// newSkillServer builds a BFF whose skill store is the in-memory twin, and whose caller client
// answers every SSAR with `allow`.
func newSkillServer(t *testing.T, store skill.Store, allow bool) *Server {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(ssarInterceptor(func(_, _, _ string) bool { return allow })).
		Build()
	return NewServer(Options{
		CallerClients: &fakeCallerClientFactory{client: c},
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Expand: NewExpandAdapter()},
		SkillStore:    store,
		Version:       "test",
		Log:           logr.Discard(),
	})
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer test-token")
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// TestSkillsWithoutAStoreAre501NotEmpty. "No skills exist here" and "skills are not available
// on this install" are different facts. Returning an empty list for the second would tell a
// user their skills vanished, which is the collapse this codebase keeps having to undo.
func TestSkillsWithoutAStoreAre501NotEmpty(t *testing.T) {
	t.Parallel()

	s := newSkillServer(t, nil, true)
	rec := do(t, s, http.MethodGet, "/api/skills?namespace=team-a", "")
	require.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"skills":[]`,
		"an absent store must not be reported as an empty list")
}

// TestSkillWritesAreCallerScoped. The BFF holds no privilege of its own (ADR 0011): every write
// is authorized by a SelfSubjectAccessReview made AS THE CALLER, so a denied caller gets a 403
// and nothing is persisted.
func TestSkillWritesAreCallerScoped(t *testing.T) {
	t.Parallel()

	store := skill.NewMemStore()
	s := newSkillServer(t, store, false) // every SSAR denies

	rec := do(t, s, http.MethodPost, "/api/skills",
		`{"namespace":"team-a","name":"summarise","description":"d"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)

	got, err := store.ListSkills(context.Background(), "team-a")
	require.NoError(t, err)
	assert.Empty(t, got, "a denied write must persist nothing")
}

// TestABranchRefIs422. Validation moved into Go because the CRD markers that would have run in
// the API server do not exist for a Postgres-resident entity (ADR 0044 §2). A mutable ref is
// refused with a reason, not accepted and discovered later by a replay that silently changed.
func TestABranchRefIs422(t *testing.T) {
	t.Parallel()

	store := skill.NewMemStore()
	require.NoError(t, store.UpsertSkill(context.Background(),
		skill.Skill{Namespace: "team-a", Name: "summarise"}))
	s := newSkillServer(t, store, true)

	rec := do(t, s, http.MethodPost, "/api/skills/team-a/summarise/versions",
		`{"digest":"sha256:abc","source":"git","repo":"https://x/y.git","ref":"main","path":"a/SKILL.md"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "not an immutable pin")
}

// TestVersionAddIsIdempotent. Same bytes are the same version, so a retry must succeed rather
// than 409 — and must not fork one skill's history into two entries for one thing.
func TestVersionAddIsIdempotent(t *testing.T) {
	t.Parallel()

	store := skill.NewMemStore()
	ctx := context.Background()
	require.NoError(t, store.UpsertSkill(ctx, skill.Skill{Namespace: "team-a", Name: "summarise"}))
	s := newSkillServer(t, store, true)

	body := `{"digest":"sha256:abc","source":"upload","objectKey":"uploads/abc"}`
	for i := range 2 {
		rec := do(t, s, http.MethodPost, "/api/skills/team-a/summarise/versions", body)
		require.Equalf(t, http.StatusOK, rec.Code, "attempt %d", i+1)
	}
	vs, err := store.ListVersions(ctx, "team-a", "summarise")
	require.NoError(t, err)
	assert.Len(t, vs, 1)
}

// TestSkillDetailCarriesItsHistory — the detail view is what a console renders, and a version
// list without digests would leave the UI with nothing stable to pin.
func TestSkillDetailCarriesItsHistory(t *testing.T) {
	t.Parallel()

	store := skill.NewMemStore()
	ctx := context.Background()
	require.NoError(t, store.UpsertSkill(ctx, skill.Skill{
		Namespace: "team-a", Name: "summarise", Description: "Summarises documents.",
	}))
	require.NoError(t, store.AddVersion(ctx, skill.SkillVersion{
		Namespace: "team-a", Skill: "summarise", Digest: skill.Digest([]byte("v1")),
		Source: skill.SourceUpload, ObjectKey: "uploads/v1",
	}))
	s := newSkillServer(t, store, true)

	rec := do(t, s, http.MethodGet, "/api/skills/team-a/summarise", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var out SkillDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "Summarises documents.", out.Description)
	require.Len(t, out.Versions, 1)
	assert.Equal(t, skill.Digest([]byte("v1")), out.Versions[0].Digest)
}
