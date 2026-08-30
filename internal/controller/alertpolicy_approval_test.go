/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// alertpolicy_approval_test.go — plain unit tests for the HITL approval-waiting notification (M75,
// m75.3, ADR 0069 §3). NO envtest / integration tag: these drive evaluateApprovalWaiting directly with a
// fake controller-runtime client (for AgentDeployment selection), a fake alertstore ledger (dedup +
// resolve), a fake run lister (the plan_approval-waiting runs), and an httptest.Server (channel dispatch).
//
// The load-bearing assertions (ADR 0069 §3):
//   - a per-run alert fires with a run-detail LINK (never a public share link, never an approve-magic-link);
//   - a SECOND simultaneously-waiting run ALSO fires — per-(policy, condition, runID) dedup, NOT the
//     aggregate condition-name key (the verified wrinkle);
//   - a re-tick does NOT re-fire an already-open run (dedup holds);
//   - the alert RESOLVES when a run leaves the waiting state.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
	"github.com/ctxmesh/agentry/internal/controlplane/alertstore"
)

// --- fakes -----------------------------------------------------------------------------------------

// fakeApprovalAlertStore is a minimal in-memory alertstore.Store: it records appended rows and honours
// Resolve so a test can assert the fire-once-per-run + resolve behaviour without Postgres.
type fakeApprovalAlertStore struct {
	mu     sync.Mutex
	nextID int64
	rows   []alertstore.Alert
}

func newFakeApprovalAlertStore() *fakeApprovalAlertStore { return &fakeApprovalAlertStore{} }

func (f *fakeApprovalAlertStore) Append(_ context.Context, a alertstore.Alert) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	a.ID = f.nextID
	if a.FiredAt.IsZero() {
		a.FiredAt = time.Now().UTC()
	}
	f.rows = append(f.rows, a)
	return a.ID, nil
}

func (f *fakeApprovalAlertStore) List(_ context.Context, namespace string, limit int) ([]alertstore.Alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Newest-first, matching the Postgres contract.
	out := make([]alertstore.Alert, 0, len(f.rows))
	for _, row := range slices.Backward(f.rows) {
		if row.Namespace == namespace {
			out = append(out, row)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeApprovalAlertStore) Resolve(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].ID == id && f.rows[i].ResolvedAt == nil {
			t := time.Now().UTC()
			f.rows[i].ResolvedAt = &t
			return nil
		}
	}
	return nil
}

// openApprovalRows returns the unresolved approvalWaiting rows (for the assertions below).
func (f *fakeApprovalAlertStore) openApprovalRows() []alertstore.Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []alertstore.Alert
	for _, r := range f.rows {
		if r.CondType == condTypeApprovalWaiting && r.ResolvedAt == nil {
			out = append(out, r)
		}
	}
	return out
}

// appendCount reports how many approvalWaiting rows were EVER appended (resolved or not) for a runID.
func (f *fakeApprovalAlertStore) appendCount(runID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.rows {
		if r.CondType == condTypeApprovalWaiting && r.Agent == runID {
			n++
		}
	}
	return n
}

// fakeApprovalRunLister returns a fixed set of plan_approval-waiting runs.
type fakeApprovalRunLister struct {
	mu   sync.Mutex
	runs []WaitingApprovalRun
}

func (l *fakeApprovalRunLister) set(runs ...WaitingApprovalRun) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs = runs
}

func (l *fakeApprovalRunLister) ListWaitingApproval(_ context.Context, _ string) ([]WaitingApprovalRun, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]WaitingApprovalRun(nil), l.runs...), nil
}

// --- helpers ---------------------------------------------------------------------------------------

func approvalTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(s))
	require.NoError(t, agentsv1beta1.AddToScheme(s))
	return s
}

// mkApprovalPolicy builds an AlertPolicy with a single approvalWaiting condition selecting agents by name.
func mkApprovalPolicy(name, namespace string, agentNames []string, channels []agentsv1beta1.AlertChannel) *agentsv1beta1.AlertPolicy {
	return &agentsv1beta1.AlertPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AlertPolicySpec{
			Selector:   agentsv1beta1.AlertSelector{Names: agentNames},
			Conditions: []agentsv1beta1.AlertCondition{{Name: "await-approval", Type: condTypeApprovalWaiting}},
			Route:      agentsv1beta1.AlertRoute{Channels: channels},
		},
	}
}

func mkApprovalAgent(name, namespace string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

// --- tests -----------------------------------------------------------------------------------------

// TestApprovalWaiting_PerRunDedup_SecondRunAlsoFires is THE load-bearing test (ADR 0069 §3): two runs
// waiting AT ONCE both fire an alert (per-run dedup), a re-tick does NOT re-fire either (dedup holds),
// and resolving one leaves the other open + re-fires nothing.
func TestApprovalWaiting_PerRunDedup_SecondRunAlsoFires(t *testing.T) {
	const ns = "default"
	s := approvalTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(mkApprovalAgent("agent-a", ns)).Build()

	alerts := newFakeApprovalAlertStore()
	lister := &fakeApprovalRunLister{}
	r := &AlertPolicyReconciler{Client: c, Alerts: alerts, Runs: lister}

	ap := mkApprovalPolicy("hitl", ns, []string{"agent-a"}, []agentsv1beta1.AlertChannel{{Type: "console"}})
	cond := ap.Spec.Conditions[0]
	agents := []agentsv1alpha1.AgentDeployment{*mkApprovalAgent("agent-a", ns)}

	// Tick 1: run-1 and run-2 both waiting → BOTH fire.
	lister.set(
		WaitingApprovalRun{ID: "run-1", Agent: "agent-a", Message: "approve plan A"},
		WaitingApprovalRun{ID: "run-2", Agent: "agent-a", Message: "approve plan B"},
	)
	r.evaluateApprovalWaiting(context.Background(), ap, cond, agents, metav1.Now())

	require.Equal(t, 1, alerts.appendCount("run-1"), "run-1 must fire exactly once")
	require.Equal(t, 1, alerts.appendCount("run-2"),
		"run-2 (the SECOND simultaneously-waiting run) MUST also fire — per-run dedup, not condition-name")
	require.Len(t, alerts.openApprovalRows(), 2, "both waiting runs must have an open alert")

	// Tick 2: same two runs still waiting → NO re-fire (dedup by (policy, condition, runID)).
	r.evaluateApprovalWaiting(context.Background(), ap, cond, agents, metav1.Now())
	assert.Equal(t, 1, alerts.appendCount("run-1"), "run-1 must not re-fire while still waiting")
	assert.Equal(t, 1, alerts.appendCount("run-2"), "run-2 must not re-fire while still waiting")

	// Tick 3: run-1 leaves the waiting state (approved/denied), run-2 still waits → run-1 resolves,
	// run-2 stays open, nothing re-fires.
	lister.set(WaitingApprovalRun{ID: "run-2", Agent: "agent-a", Message: "approve plan B"})
	r.evaluateApprovalWaiting(context.Background(), ap, cond, agents, metav1.Now())

	open := alerts.openApprovalRows()
	require.Len(t, open, 1, "run-1's alert must resolve when it leaves the waiting state")
	assert.Equal(t, "run-2", open[0].Agent, "the still-waiting run-2 alert must remain open")
	assert.Equal(t, 1, alerts.appendCount("run-2"), "run-2 must not re-fire on the resolve tick")
}

// TestApprovalWaiting_PayloadCarriesRunDetailLink asserts the dispatched webhook payload carries a
// run-detail POINTER (runId + a console link), and NEVER a public share link or an approve-magic-link.
func TestApprovalWaiting_PayloadCarriesRunDetailLink(t *testing.T) {
	const ns = "team-x"
	var (
		mu   sync.Mutex
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b := make([]byte, req.ContentLength)
		_, _ = req.Body.Read(b)
		mu.Lock()
		body = b
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := approvalTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(mkApprovalAgent("planner", ns)).Build()

	alerts := newFakeApprovalAlertStore()
	lister := &fakeApprovalRunLister{}
	lister.set(WaitingApprovalRun{ID: "run-xyz", Agent: "planner", Message: "approve the plan"})

	r := &AlertPolicyReconciler{
		Client:     c,
		Alerts:     alerts,
		Runs:       lister,
		HTTPClient: srv.Client(),
		ConsoleURL: "https://console.example.com",
	}
	ap := mkApprovalPolicy("hitl", ns, []string{"planner"}, []agentsv1beta1.AlertChannel{
		{Type: "webhook", Webhook: &agentsv1beta1.WebhookChannel{URL: srv.URL}},
	})
	cond := ap.Spec.Conditions[0]
	agents := []agentsv1alpha1.AgentDeployment{*mkApprovalAgent("planner", ns)}

	r.evaluateApprovalWaiting(context.Background(), ap, cond, agents, metav1.Now())

	mu.Lock()
	got := body
	mu.Unlock()
	require.NotEmpty(t, got, "webhook must receive a POST for the waiting run")

	var payload struct {
		Type  string `json:"type"`
		RunID string `json:"runId"`
		Link  string `json:"link"`
	}
	require.NoError(t, json.Unmarshal(got, &payload))

	assert.Equal(t, condTypeApprovalWaiting, payload.Type)
	assert.Equal(t, "run-xyz", payload.RunID, "payload must carry the waiting run id (the pointer)")

	// The link is a deep-link to the AUTHENTICATED console approval view — it contains the run id and
	// targets the console origin. It must NOT be a public share link or an approve-magic-link.
	assert.True(t, strings.HasPrefix(payload.Link, "https://console.example.com/"),
		"link must target the console origin, got %q", payload.Link)
	assert.Contains(t, payload.Link, "run-xyz", "link must point at the run")
	assert.NotContains(t, payload.Link, "/api/shared/", "link must NOT be the public share-link route")
	assert.NotContains(t, payload.Link, "token", "link must NOT carry a share/capability token")
	assert.NotContains(t, strings.ToLower(payload.Link), "approve",
		"link must NOT be an approve-magic-link — approval stays caller-scoped via /api/runs/{id}/resume")
}

// TestApprovalWaiting_OnlySelectedAgents asserts a waiting run whose agent the policy does NOT select
// fires nothing (a namespace holds many agents; a policy scopes to a subset).
func TestApprovalWaiting_OnlySelectedAgents(t *testing.T) {
	const ns = "default"
	s := approvalTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(mkApprovalAgent("watched", ns)).Build()

	alerts := newFakeApprovalAlertStore()
	lister := &fakeApprovalRunLister{}
	lister.set(
		WaitingApprovalRun{ID: "run-watched", Agent: "watched", Message: "approve"},
		WaitingApprovalRun{ID: "run-other", Agent: "unwatched", Message: "approve"},
	)
	r := &AlertPolicyReconciler{Client: c, Alerts: alerts, Runs: lister}

	ap := mkApprovalPolicy("hitl-scoped", ns, []string{"watched"}, []agentsv1beta1.AlertChannel{{Type: "console"}})
	cond := ap.Spec.Conditions[0]
	agents := []agentsv1alpha1.AgentDeployment{*mkApprovalAgent("watched", ns)}

	r.evaluateApprovalWaiting(context.Background(), ap, cond, agents, metav1.Now())

	assert.Equal(t, 1, alerts.appendCount("run-watched"), "the selected agent's run must fire")
	assert.Equal(t, 0, alerts.appendCount("run-other"), "an unselected agent's run must NOT fire")
}

// TestApprovalWaiting_RelativeLinkWhenNoConsoleURL asserts the payload still carries a POINTER (a
// relative path) when ConsoleURL is unset.
func TestApprovalWaiting_RelativeLinkWhenNoConsoleURL(t *testing.T) {
	r := &AlertPolicyReconciler{} // ConsoleURL empty
	link := r.consoleRunLink("run-1")
	assert.True(t, strings.HasPrefix(link, "/"), "empty ConsoleURL ⇒ a relative path, got %q", link)
	assert.Equal(t, "/runs/run-1", link, "the deep-link targets the per-run detail page /runs/:id, keyed by run id")
}

// TestApprovalWaiting_NilRunsSafe asserts a nil run lister disables the eval without panicking.
func TestApprovalWaiting_NilRunsSafe(t *testing.T) {
	const ns = "default"
	s := approvalTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	alerts := newFakeApprovalAlertStore()
	r := &AlertPolicyReconciler{Client: c, Alerts: alerts /* Runs: nil */}

	ap := mkApprovalPolicy("hitl", ns, []string{"agent-a"}, []agentsv1beta1.AlertChannel{{Type: "console"}})
	cond := ap.Spec.Conditions[0]

	require.NotPanics(t, func() {
		r.evaluateApprovalWaiting(context.Background(), ap, cond, nil, metav1.Now())
	})
	assert.Empty(t, alerts.openApprovalRows(), "nil Runs ⇒ no alerts fired")
}
