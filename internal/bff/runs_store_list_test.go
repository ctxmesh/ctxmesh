package bff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ctxmesh/ctxmesh/internal/run"
)

// seedStoreRuns fills a run store the way a stock install's would look after a few runs.
func seedStoreRuns(t *testing.T, rs run.Store) {
	t.Helper()
	base := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	add := func(id, ns, agent string, minute int, status run.Status, parent string) {
		r := run.New(id, ns, agent, json.RawMessage(`{"input":"hi"}`), "", base.Add(time.Duration(minute)*time.Minute))
		r.TraceID = "t-" + id
		r.ParentRunID = parent
		if parent != "" {
			r.RootRunID = parent
		}
		r.Status = status
		if status.IsTerminal() {
			r.UpdatedAt = r.CreatedAt.Add(1500 * time.Millisecond)
		}
		require.NoError(t, rs.Create(r))
	}
	add("ok-1", "team", "a", 1, run.StatusSucceeded, "")
	add("err-1", "team", "a", 2, run.StatusFailed, "")
	add("live-1", "team", "a", 3, run.StatusRunning, "")
	add("child-1", "team", "a", 4, run.StatusSucceeded, "ok-1")
	add("ghost-1", "team", "deleted-agent", 5, run.StatusSucceeded, "") // its agent no longer exists
	add("rival-1", "rival", "a", 6, run.StatusSucceeded, "")            // another tenant, same agent name
}

func storeRunsServer(t *testing.T, funcsMod func(*interceptor.Funcs), objs ...client.Object) *Server {
	t.Helper()
	rs := run.NewMemStore()
	seedStoreRuns(t, rs)
	funcs := ssrInterceptor("dev@example.com", nil)
	if funcsMod != nil {
		funcsMod(&funcs)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
	return NewServer(Options{
		CallerClients: newFakeFactory(c),
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		Adapters:      Adapters{Invoke: &fakeInvokeAdapter{}}, // no trace store
		RunStore:      rs,
		Version:       "test",
		Log:           logr.Discard(),
	})
}

func getStoreRuns(t *testing.T, s *Server, query string) (*httptest.ResponseRecorder, storeRunListResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs"+query, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body storeRunListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return rec, body
}

func runIDs(rows []storeRunSummary) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.RunID)
	}
	return out
}

// Without a trace store the Runs list reads the run store, scoped to the agents the caller can list: a
// deleted agent's runs, another namespace's agent of the same name, and child runs never appear.
func TestStoreRuns_ListsOnlyTheCallersRootRuns(t *testing.T) {
	s := storeRunsServer(t, nil, readyAgent("a", "team", "http://a.team.svc"))

	rec, body := getStoreRuns(t, s, "?namespace=team")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"live-1", "err-1", "ok-1"}, runIDs(body.Runs))
	assert.Empty(t, body.NextCursor)

	byID := map[string]storeRunSummary{}
	for _, r := range body.Runs {
		byID[r.RunID] = r
	}
	assert.Equal(t, "ok", byID["ok-1"].Status)
	assert.Equal(t, "error", byID["err-1"].Status)
	assert.Equal(t, "running", byID["live-1"].Status)
	assert.Equal(t, "t-ok-1", byID["ok-1"].TraceID)
	assert.Equal(t, "team", byID["ok-1"].AgentNs)
	assert.Equal(t, "a", byID["ok-1"].AgentName)
	require.NotNil(t, byID["ok-1"].LatencyMs)
	assert.InDelta(t, 1500, *byID["ok-1"].LatencyMs, 0.1)
	assert.Nil(t, byID["live-1"].LatencyMs, "a run still going has no duration yet")

	// Cost and tokens are unknown without a trace store; the wire says nothing rather than 0.
	var raw struct {
		Runs []map[string]any `json:"runs"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	for _, r := range raw.Runs {
		assert.NotContains(t, r, "costUSD")
		assert.NotContains(t, r, "tokens")
	}
}

func TestStoreRuns_AgentFilterAuthorizesThatAgent(t *testing.T) {
	s := storeRunsServer(t, nil, readyAgent("a", "team", "http://a.team.svc"))

	rec, body := getStoreRuns(t, s, "?agent=team/a")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"live-1", "err-1", "ok-1"}, runIDs(body.Runs))

	rec, _ = getStoreRuns(t, s, "?agent=rival/a")
	assert.Equal(t, http.StatusNotFound, rec.Code, "an agent the caller cannot see is not listed, not even empty")
}

func TestStoreRuns_PagesWithoutGapOrRepeat(t *testing.T) {
	s := storeRunsServer(t, nil, readyAgent("a", "team", "http://a.team.svc"))

	var got []string
	cursor := ""
	for range 5 {
		q := "?namespace=team&limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		rec, body := getStoreRuns(t, s, q)
		require.Equal(t, http.StatusOK, rec.Code)
		got = append(got, runIDs(body.Runs)...)
		cursor = body.NextCursor
		if cursor == "" {
			break
		}
	}
	assert.Equal(t, []string{"live-1", "err-1", "ok-1"}, got)

	rec, _ := getStoreRuns(t, s, "?cursor=not-a-cursor")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec, _ = getStoreRuns(t, s, "?from=yesterday")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// A caller who may not list agents cluster-wide gets a 403 for the global list, not every run.
func TestStoreRuns_ClusterWideListNeedsClusterWideRBAC(t *testing.T) {
	deny := func(f *interceptor.Funcs) {
		f.List = func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lo := &client.ListOptions{}
			for _, o := range opts {
				o.ApplyToList(lo)
			}
			if lo.Namespace == "" {
				return apierrors.NewForbidden(schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"}, "", assert.AnError)
			}
			return cl.List(ctx, list, opts...)
		}
	}
	s := storeRunsServer(t, deny, readyAgent("a", "team", "http://a.team.svc"))

	rec, _ := getStoreRuns(t, s, "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestStoreAgentRuns_ListsTheAgentsRuns(t *testing.T) {
	s := storeRunsServer(t, nil, readyAgent("a", "team", "http://a.team.svc"))

	rec := getRuns(t, s, "team", "a", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body storeAgentRunsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, []string{"live-1", "err-1", "ok-1"}, runIDs(body.Runs))

	rec = getRuns(t, s, "team", "deleted-agent", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// The same caller, allowed in one namespace, lists that namespace's runs through ?namespace=.
func TestStoreRuns_NamespaceScopedCallerListsTheirNamespace(t *testing.T) {
	deny := func(f *interceptor.Funcs) {
		f.List = func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lo := &client.ListOptions{}
			for _, o := range opts {
				o.ApplyToList(lo)
			}
			if lo.Namespace != "team" {
				return apierrors.NewForbidden(schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"}, "", assert.AnError)
			}
			return cl.List(ctx, list, opts...)
		}
	}
	s := storeRunsServer(t, deny, readyAgent("a", "team", "http://a.team.svc"), readyAgent("a", "rival", "http://a.rival.svc"))

	rec, body := getStoreRuns(t, s, "?namespace=team")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"live-1", "err-1", "ok-1"}, runIDs(body.Runs), "rival/a exists but is not the caller's")
	rec, _ = getStoreRuns(t, s, "?namespace=rival")
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
