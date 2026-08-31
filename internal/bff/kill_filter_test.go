package bff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/killscope"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// Layer (b) of the scoped kill switch (M146, ADR 0126 §3): a killed scope's queued runs must not be
// CLAIMED — and must STAY QUEUED, not be claimed-then-released, so they resume intact after the un-kill.

// killServer builds a Server with a kill store + run store and no other machinery.
func killServer(t *testing.T, ks killscope.Store) *Server {
	t.Helper()
	return &Server{runStore: run.NewMemStore(), killScopes: ks, log: logr.Discard()}
}

func queueRun(t *testing.T, s *Server, id, ns, agent string) {
	t.Helper()
	require.NoError(t, s.runStore.Create(run.New(id, ns, agent, []byte(`{"input":"x"}`), "", time.Now())))
}

// THE BAR: a queued run under a killed scope is not claimed, and an unaffected one still is — proven
// together, because a filter that excluded everything would pass the first half alone.
func TestClaimFilter_AKilledScopesQueuedRunsAreNotClaimed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope killscope.Scope
	}{
		{"agent scope", killscope.Scope{Level: killscope.LevelAgent, Namespace: "team-a", Agent: "bot"}},
		{"namespace scope", killscope.Scope{Level: killscope.LevelNamespace, Namespace: "team-a"}},
		{"fleet scope", killscope.Scope{Level: killscope.LevelFleet}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ks := killscope.NewMemStore()
			s := killServer(t, ks)
			queueRun(t, s, "killed", "team-a", "bot")
			queueRun(t, s, "healthy", "team-b", "other")
			require.NoError(t, ks.Kill(context.Background(), killscope.Kill{
				Scope: tc.scope, Reason: "incident", Principal: "alice",
			}))

			filter, err := s.claimFilter(context.Background())
			require.NoError(t, err)

			claimed, err := s.runStore.ClaimQueued("w1", time.Minute, filter)
			if tc.scope.Level == killscope.LevelFleet {
				require.ErrorIs(t, err, run.ErrNoQueuedRun, "a fleet stop claims nothing at all")
			} else {
				require.NoError(t, err)
				assert.Equal(t, "healthy", claimed.ID,
					"the unaffected run is still claimed — the kill must DISCRIMINATE, not halt everything")
			}

			// The killed run is untouched: still queued, no worker, ready to resume on un-kill.
			got, err := s.runStore.Get("killed")
			require.NoError(t, err)
			assert.Equal(t, run.StatusQueued, got.Status,
				"a killed scope's run must STAY QUEUED — claiming then releasing would flip it to running")
			assert.Empty(t, got.WorkerID)
		})
	}
}

// The un-kill half: the backlog resumes. This is what proves the queued runs were HELD rather than lost.
func TestClaimFilter_UnkillLetsTheBacklogResume(t *testing.T) {
	ks := killscope.NewMemStore()
	s := killServer(t, ks)
	queueRun(t, s, "held", "team-a", "bot")
	scope := killscope.Scope{Level: killscope.LevelNamespace, Namespace: "team-a"}
	require.NoError(t, ks.Kill(context.Background(), killscope.Kill{Scope: scope, Reason: "r", Principal: "p"}))

	f, err := s.claimFilter(context.Background())
	require.NoError(t, err)
	_, err = s.runStore.ClaimQueued("w1", time.Minute, f)
	require.ErrorIs(t, err, run.ErrNoQueuedRun)

	lifted, err := ks.Unkill(context.Background(), scope)
	require.NoError(t, err)
	assert.True(t, lifted)

	s.killFilter = killFilterCache{} // the cache would otherwise hold the stale filter for its TTL
	f, err = s.claimFilter(context.Background())
	require.NoError(t, err)
	claimed, err := s.runStore.ClaimQueued("w1", time.Minute, f)
	require.NoError(t, err, "after the un-kill the held backlog must resume")
	assert.Equal(t, "held", claimed.ID)
}

// A killed scope's ABANDONED run must not be RECLAIMED either — otherwise a kill merely changes which
// worker picks it up.
func TestClaimFilter_AKilledScopesAbandonedRunIsNotReclaimed(t *testing.T) {
	ks := killscope.NewMemStore()
	s := killServer(t, ks)
	queueRun(t, s, "abandoned", "team-a", "bot")
	_, err := s.runStore.ClaimQueued("dead-worker", time.Millisecond, run.ClaimFilter{})
	require.NoError(t, err)
	require.NoError(t, s.runStore.ReleaseLease("abandoned", "dead-worker"))

	require.NoError(t, ks.Kill(context.Background(), killscope.Kill{
		Scope: killscope.Scope{Level: killscope.LevelNamespace, Namespace: "team-a"}, Reason: "r", Principal: "p",
	}))
	f, err := s.claimFilter(context.Background())
	require.NoError(t, err)

	_, err = s.runStore.ClaimReclaimable("w2", time.Minute, f)
	assert.ErrorIs(t, err, run.ErrNoQueuedRun,
		"a kill must stop the reclaim path too, not just the queue")
}

// FAIL-CLOSED: an unreadable kill set means we do not know what is stopped, so the worker declines to
// START work. It must be an error (retried next tick), never a silent empty filter.
func TestClaimFilter_AnUnreadableKillSetFailsClosed(t *testing.T) {
	s := killServer(t, errKillStore{})
	_, err := s.claimFilter(context.Background())
	require.Error(t, err, "an unreadable kill set must not resolve to 'nothing is killed'")
}

// ...but a platform with NO kill store configured is inert, not wedged — an install that never uses the
// feature must behave exactly as it did before it existed.
func TestClaimFilter_NoStoreIsInertNotWedged(t *testing.T) {
	s := &Server{runStore: run.NewMemStore(), log: logr.Discard()}
	f, err := s.claimFilter(context.Background())
	require.NoError(t, err)
	assert.True(t, f.Empty())
}

// A killed TENANT expands to its member namespaces — the run store knows nothing about tenants, so the
// control plane must resolve it before the query runs.
func TestClaimFilter_AKilledTenantExpandsToItsNamespaces(t *testing.T) {
	f, err := killscope.Expand(context.Background(),
		[]killscope.Kill{{Scope: killscope.Scope{Level: killscope.LevelTenant, Tenant: "acme"}}},
		func(_ context.Context, tenant string) ([]string, error) {
			assert.Equal(t, "acme", tenant)
			return []string{"team-a", "team-b"}, nil
		})
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a", "team-b"}, f.Namespaces)
	assert.True(t, f.Excludes("team-a", "anything"))
	assert.False(t, f.Excludes("team-c", "anything"), "a namespace outside the tenant is unaffected")
}

// An unresolvable tenant membership is an ERROR, not a silently narrower filter — otherwise a mirror
// blip would quietly un-kill a tenant mid-incident.
func TestClaimFilter_AnUnresolvableTenantFailsClosed(t *testing.T) {
	_, err := killscope.Expand(context.Background(),
		[]killscope.Kill{{Scope: killscope.Scope{Level: killscope.LevelTenant, Tenant: "acme"}}},
		func(context.Context, string) ([]string, error) { return nil, errors.New("mirror down") })
	require.Error(t, err)
}

type errKillStore struct{}

func (errKillStore) Kill(context.Context, killscope.Kill) error { return errors.New("down") }
func (errKillStore) Unkill(context.Context, killscope.Scope) (bool, error) {
	return false, errors.New("down")
}

func (errKillStore) Active(context.Context) ([]killscope.Kill, error) {
	return nil, errors.New("control plane unreachable")
}

// postRun POSTs /api/runs and returns the RAW recorder — createRun asserts 202, which is exactly what
// the kill gate tests need to observe instead of assert.
func postRun(t *testing.T, s *Server, body InvokeRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// ─── layer (c): the create edge refuses, fail-closed (ADR 0126 §3) ────────────────────────────────

// THE BAR for layer (c): a killed scope gains NO new queued runs. Without this a kill is advisory — the
// backlog simply rebuilds while the operator watches the in-flight runs stop.
func TestKillGate_ARunUnderAKilledScopeIsRefused(t *testing.T) {
	agent := readyAgent("bot", "team-a", "http://bot.team-a.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	ks := killscope.NewMemStore()
	require.NoError(t, ks.Kill(context.Background(), killscope.Kill{
		Scope:  killscope.Scope{Level: killscope.LevelNamespace, Namespace: "team-a"},
		Reason: "prompt-injection incident", Principal: "alice",
	}))

	store := run.NewMemStore()
	s := NewServer(Options{
		CallerClients: newFakeFactory(c), Scheme: testScheme(t), Auth: AllowAll{},
		Adapters: Adapters{Invoke: &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"x"}`)}},
		Version:  "test", Log: logr.Discard(), RunStore: store, KillScopes: ks,
	})

	rec := postRun(t, s, InvokeRequest{Agent: "bot", Namespace: "team-a", Input: json.RawMessage(`{}`)})
	assert.Equal(t, http.StatusLocked, rec.Code,
		"423 Locked — deliberately held by an operator, not a conflict and not a transient outage")
	assert.Contains(t, rec.Body.String(), "emergency stop")
	assert.Empty(t, store.List(), "a refused create must not leave a queued run behind to run later")
}

// The discriminating half: an unaffected agent still accepts runs. A gate that refused everything would
// pass the test above while taking the platform down.
func TestKillGate_AnUnaffectedAgentStillAcceptsRuns(t *testing.T) {
	agent := readyAgent("bot", "team-b", "http://bot.team-b.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	ks := killscope.NewMemStore()
	require.NoError(t, ks.Kill(context.Background(), killscope.Kill{
		Scope:  killscope.Scope{Level: killscope.LevelNamespace, Namespace: "team-a"},
		Reason: "r", Principal: "p",
	}))

	s := NewServer(Options{
		CallerClients: newFakeFactory(c), Scheme: testScheme(t), Auth: AllowAll{},
		Adapters: Adapters{Invoke: &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"x"}`)}},
		Version:  "test", Log: logr.Discard(), RunStore: run.NewMemStore(), KillScopes: ks,
	})

	rec := postRun(t, s, InvokeRequest{Agent: "bot", Namespace: "team-b", Input: json.RawMessage(`{}`)})
	assert.Equal(t, http.StatusAccepted, rec.Code, "a namespace outside the kill is unaffected")
}

// FAIL-CLOSED: the property layer (a) cannot provide. When the kill set cannot be read at all, the edge
// REFUSES — admitting work we cannot prove is permitted is the exact failure the feature exists to
// prevent, and it is most likely during the incident the kill was pulled for.
func TestKillGate_AnUnreadableKillSetRefusesRatherThanAdmits(t *testing.T) {
	agent := readyAgent("bot", "team-a", "http://bot.team-a.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()

	s := NewServer(Options{
		CallerClients: newFakeFactory(c), Scheme: testScheme(t), Auth: AllowAll{},
		Adapters: Adapters{Invoke: &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"x"}`)}},
		Version:  "test", Log: logr.Discard(), RunStore: run.NewMemStore(), KillScopes: errKillStore{},
	})

	rec := postRun(t, s, InvokeRequest{Agent: "bot", Namespace: "team-a", Input: json.RawMessage(`{}`)})
	assert.Equal(t, http.StatusLocked, rec.Code,
		"an unreadable kill set must refuse — a fail-open edge would silently un-kill during an outage")
	assert.Contains(t, rec.Body.String(), "cannot confirm")
}

// An install that never provisions a kill store behaves exactly as it did pre-M146.
func TestKillGate_NoKillStoreAdmitsEverything(t *testing.T) {
	agent := readyAgent("bot", "team-a", "http://bot.team-a.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()

	s := NewServer(Options{
		CallerClients: newFakeFactory(c), Scheme: testScheme(t), Auth: AllowAll{},
		Adapters: Adapters{Invoke: &fakeInvokeAdapter{traceID: "t", resp: []byte(`{"output":"x"}`)}},
		Version:  "test", Log: logr.Discard(), RunStore: run.NewMemStore(),
	})

	rec := postRun(t, s, InvokeRequest{Agent: "bot", Namespace: "team-a", Input: json.RawMessage(`{}`)})
	assert.Equal(t, http.StatusAccepted, rec.Code, "the feature must be inert when unprovisioned")
}
