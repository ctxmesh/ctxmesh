package bff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func getCensus(t *testing.T, s *Server, query string) CensusResponse {
	t.Helper()
	url := "/api/agents/census"
	if query != "" {
		url += "?" + query
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "census body: %s", rec.Body.String())
	var resp CensusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// pagingReader honours Limit and Continue the way a real API server does. The
// controller-runtime fake client ignores both, so a census test that goes through it
// proves the tally and nothing at all about the walk.
type pagingReader struct {
	items    []agentsv1alpha1.AgentDeployment
	calls    int
	maxLimit int64
}

func (p *pagingReader) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	p.calls++
	var o client.ListOptions
	for _, opt := range opts {
		opt.ApplyToList(&o)
	}
	if o.Limit > p.maxLimit {
		p.maxLimit = o.Limit
	}
	start := 0
	if o.Continue != "" {
		if _, err := fmt.Sscanf(o.Continue, "at-%d", &start); err != nil {
			return err
		}
	}
	end := start + int(o.Limit)
	if o.Limit == 0 || end > len(p.items) {
		end = len(p.items)
	}
	out := list.(*agentsv1alpha1.AgentDeploymentList)
	out.Items = p.items[start:end]
	if end < len(p.items) {
		out.Continue = fmt.Sprintf("at-%d", end)
	} else {
		out.Continue = ""
	}
	return nil
}

func agentsNamed(n int) []agentsv1alpha1.AgentDeployment {
	out := make([]agentsv1alpha1.AgentDeployment, 0, n)
	for i := range n {
		out = append(out, *publishedAgent(fmt.Sprintf("agent-%05d", i), "prod"))
	}
	return out
}

// The whole point of the endpoint: a fleet far larger than one public page is counted
// exactly, by walking the cursor rather than reading one window of it.
func TestCensusWalksEveryPage(t *testing.T) {
	const n = maxListLimit*7 + 13
	r := &pagingReader{items: agentsNamed(n)}

	resp, err := censusScan(context.Background(), r, "")
	require.NoError(t, err)

	assert.Equal(t, n, resp.Total, "census must count the whole fleet, not one page of it")
	assert.True(t, resp.Complete, "a fleet under the hard ceiling is a complete count")
	assert.Greater(t, r.calls, 1, "a fleet larger than one page must take more than one call")

	sum := 0
	for _, g := range resp.Groups {
		sum += g.Count
	}
	assert.Equal(t, resp.Total, sum, "the groups must account for every counted agent")
}

// Past the hard ceiling the answer must be a declared lower bound, never a flat number
// — the exact shape of the defect this milestone removes.
func TestCensusPastTheCeilingIsADeclaredBound(t *testing.T) {
	r := &pagingReader{items: agentsNamed(censusMaxAgents + censusPageSize*2)}

	resp, err := censusScan(context.Background(), r, "")
	require.NoError(t, err)

	assert.False(t, resp.Complete, "a scan stopped by the ceiling must say it is incomplete")
	assert.GreaterOrEqual(t, resp.Total, censusMaxAgents, "the bound must be at least the ceiling")
	assert.Less(t, resp.Total, len(r.items), "and it must not claim to have read the whole fleet")
}

// A pathological fleet may not shrink Total. Groups may be truncated and must declare it;
// the count of agents must still be right.
func TestCensusGroupOverflowNeverUndercountsTheTotal(t *testing.T) {
	items := make([]agentsv1alpha1.AgentDeployment, 0, censusMaxGroups*2)
	for i := range censusMaxGroups * 2 {
		a := *publishedAgent(fmt.Sprintf("agent-%05d", i), "prod")
		// Phase/Reason come off the Ready condition, so a distinct reason is a distinct tuple.
		a.Status.Conditions = []metav1.Condition{{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             fmt.Sprintf("Unique%05d", i),
			LastTransitionTime: metav1.Now(),
		}}
		items = append(items, a)
	}
	r := &pagingReader{items: items}

	resp, err := censusScan(context.Background(), r, "")
	require.NoError(t, err)

	assert.Equal(t, len(items), resp.Total, "every agent must be counted even when its tuple does not fit")
	assert.False(t, resp.GroupsComplete, "a truncated table must declare itself truncated")
	assert.LessOrEqual(t, len(resp.Groups), censusMaxGroups, "the table must stay bounded")

	classified := 0
	for _, g := range resp.Groups {
		classified += g.Count
	}
	assert.Less(t, classified, resp.Total, "the remainder is the unclassified count the UI renders")
}

// A read failure must surface, not be silently reported as a fleet of zero.
func TestCensusPropagatesReadErrors(t *testing.T) {
	_, err := censusScan(context.Background(), failingReader{}, "")
	require.Error(t, err, "a failed read must never be flattened into a zero count")
}

type failingReader struct{}

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("forbidden")
}

// Drafts are a lifecycle stage on the bar. Excluding them the way the default list
// does would make that stage permanently read zero.
func TestCensusIncludesDraftsAndFlagsThem(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(draftAgent("d1", "prod"), draftAgent("d2", "prod"), publishedAgent("live", "prod")).Build()
	s := newCallerServer(t, newFakeFactory(c))

	resp := getCensus(t, s, "")
	require.Equal(t, 3, resp.Total)

	drafts := 0
	for _, g := range resp.Groups {
		if g.IsDraft {
			drafts += g.Count
		}
	}
	assert.Equal(t, 2, drafts, "both drafts must be counted and flagged")
}

// The response carries the raw inputs to resolveStatus rather than a verdict, so the
// bucketing heuristic keeps living in exactly one place.
func TestCensusReturnsRawStatusTuplesNotBuckets(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(publishedAgent("a", "prod"), publishedAgent("b", "prod")).Build()
	s := newCallerServer(t, newFakeFactory(c))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents/census", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	assert.Contains(t, body, `"ready"`, "groups must carry the raw ready flag")
	for _, verdict := range []string{"serving", "failing", "comingUp", "halted"} {
		assert.NotContains(t, body, verdict,
			"the server must not name buckets — that heuristic belongs to resolveStatus alone")
	}
}

// A namespace scope must narrow the count, or the workspace picker is decorative.
func TestCensusRespectsNamespaceScope(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(publishedAgent("a", "prod"), publishedAgent("b", "prod"), publishedAgent("c", "research")).Build()
	s := newCallerServer(t, newFakeFactory(c))

	assert.Equal(t, 3, getCensus(t, s, "").Total)
	assert.Equal(t, 2, getCensus(t, s, "namespace=prod").Total)
	assert.Equal(t, 1, getCensus(t, s, "namespace=research").Total)
}

// An empty fleet must answer zero-and-complete, never an error — first run is a
// supported state, not a failure.
func TestCensusOnAnEmptyClusterIsCompleteZero(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, newFakeFactory(c))

	resp := getCensus(t, s, "")
	assert.Equal(t, 0, resp.Total)
	assert.True(t, resp.Complete)
	assert.True(t, resp.GroupsComplete)
	assert.NotNil(t, resp.Groups, "groups must serialise as [] rather than null")
}
