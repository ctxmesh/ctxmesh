package bff

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agentry/internal/controlplane/authz"
	"github.com/ctxmesh/agentry/internal/controlplane/toolregistry"
)

// recordingAuthorizer drives the read-switch SSAR deterministically and captures
// the Action + call count so tests can assert the exact RBAC query and that the
// gate fired the expected number of times.
type recordingAuthorizer struct {
	err   error
	last  authz.Action
	count int
}

func (r *recordingAuthorizer) Authorize(_ context.Context, _ client.Client, a authz.Action) error {
	r.last = a
	r.count++
	return r.err
}

func readSwitchServer(t *testing.T, auth authz.Authorizer) (*Server, toolregistry.Store) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	store := toolregistry.NewMemStore()
	s.toolRegistryStore = store
	s.authorizer = auth
	return s, store
}

func serveGet(t *testing.T, s *Server, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// m43.4: when the store is wired, a LIST reads from Postgres behind a caller-scoped
// SSAR. Allowed → the store items are returned, and the SSAR asked the exact RBAC
// question (list toolregistries in the namespace).
func TestToolRegistryListReadSwitch_Allowed(t *testing.T) {
	ctx := context.Background()
	auth := &recordingAuthorizer{}
	s, store := readSwitchServer(t, auth)
	_, err := store.Upsert(ctx, toolregistry.ToolRegistry{
		Namespace: trNS, Name: "reg1",
		Tools: []toolregistry.ToolEntry{{Name: "echo", URL: "https://mcp.example"}},
	})
	require.NoError(t, err)

	body, code := getToolRegistries(t, s, "namespace="+trNS)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, body.Items, 1)
	assert.Equal(t, "reg1", body.Items[0].Name)
	assert.Equal(t, authz.VerbList, auth.last.Verb)
	assert.Equal(t, resourceToolRegistries, auth.last.Resource)
	assert.Equal(t, trNS, auth.last.Namespace)
}

// The security-critical case (Fable Risk 1): a denied caller gets 403 and the store
// rows are NOT leaked. The API server is no longer in the read path, so the SSAR is
// the ONLY gate — it must fire.
func TestToolRegistryListReadSwitch_Forbidden(t *testing.T) {
	ctx := context.Background()
	s, store := readSwitchServer(t, &recordingAuthorizer{err: authz.ErrForbidden})
	_, err := store.Upsert(ctx, toolregistry.ToolRegistry{Namespace: trNS, Name: "secret-reg"})
	require.NoError(t, err)

	rec := serveGet(t, s, "/api/toolregistries?namespace="+trNS)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.NotContains(t, rec.Body.String(), "secret-reg", "store data must not leak on a denied read")
}

// A non-forbidden authz/API error is a 500 (never silently allow).
func TestToolRegistryListReadSwitch_AuthzError(t *testing.T) {
	s, _ := readSwitchServer(t, &recordingAuthorizer{err: errors.New("apiserver unavailable")})
	rec := serveGet(t, s, "/api/toolregistries")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestToolRegistryGetReadSwitch_Allowed(t *testing.T) {
	ctx := context.Background()
	auth := &recordingAuthorizer{}
	s, store := readSwitchServer(t, auth)
	_, err := store.Upsert(ctx, toolregistry.ToolRegistry{
		Namespace: trNS, Name: "reg1",
		Tools: []toolregistry.ToolEntry{{Name: "echo", URL: "https://mcp.example"}},
	})
	require.NoError(t, err)

	detail, code, body := getToolRegistry(t, s, "reg1")
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "reg1", detail.Name)
	require.Len(t, detail.Tools, 1)
	assert.Equal(t, "echo", detail.Tools[0].Name)
	assert.Equal(t, authz.VerbGet, auth.last.Verb)
	assert.Equal(t, "reg1", auth.last.Name)
}

func TestToolRegistryGetReadSwitch_Forbidden(t *testing.T) {
	ctx := context.Background()
	s, store := readSwitchServer(t, &recordingAuthorizer{err: authz.ErrForbidden})
	_, err := store.Upsert(ctx, toolregistry.ToolRegistry{Namespace: trNS, Name: "secret-reg"})
	require.NoError(t, err)

	_, code, body := getToolRegistry(t, s, "secret-reg")
	assert.Equal(t, http.StatusForbidden, code)
	assert.NotContains(t, body, "secret-reg")
}

// Allowed but absent → 404 (the store's ErrNotFound maps honestly).
func TestToolRegistryGetReadSwitch_NotFound(t *testing.T) {
	s, _ := readSwitchServer(t, &recordingAuthorizer{})
	_, code, _ := getToolRegistry(t, s, "nope")
	assert.Equal(t, http.StatusNotFound, code)
}

// A store-backed ToolRegistry is always Ready (ADR 0044): the CRD/controller
// reconcile loop is retired, so a persisted Postgres row is authoritative and
// synchronously materialized — there is no async state to wait on. (During the
// m43.4 read-switch this reported Pending, mirroring the statusless CRD read; the
// full retirement in M45 makes "exists ⇒ Ready" the honest projection.)
func TestToolRegistryDetailFromStore_Ready(t *testing.T) {
	d := newToolRegistryDetailFromStore(&toolregistry.ToolRegistry{Namespace: trNS, Name: "reg1"})
	assert.Equal(t, phaseReady, d.Phase)
	assert.True(t, d.Ready)
}
