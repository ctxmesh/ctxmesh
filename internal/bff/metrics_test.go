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

package bff

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeQueued struct {
	n   int
	err error
}

func (f fakeQueued) CountQueued() (int, error) { return f.n, f.err }

// scrape renders the exporter the way Prometheus would and returns the body.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func TestMetrics_RunOutcomesAndDuration(t *testing.T) {
	m := newMetrics(nil)

	m.observeRun("succeeded", 1.5)
	m.observeRun("succeeded", 3.0)
	m.observeRun("failed", 0.4)
	m.observeRun("canceled", 2.0) // US spelling folds to cancelled

	body := scrape(t, m)

	assert.Contains(t, body, `agentengine_run_outcomes_total{outcome="succeeded"} 2`)
	assert.Contains(t, body, `agentengine_run_outcomes_total{outcome="failed"} 1`)
	assert.Contains(t, body, `agentengine_run_outcomes_total{outcome="cancelled"} 1`,
		"the US spelling must normalize to cancelled (bounded cardinality)")
	// The histogram is present with the outcome label → p95 is derivable.
	assert.Contains(t, body, `agentengine_run_duration_seconds_bucket{outcome="succeeded"`)
	assert.Contains(t, body, `agentengine_run_duration_seconds_count{outcome="succeeded"} 2`)
}

func TestMetrics_OutcomeCardinalityBounded(t *testing.T) {
	m := newMetrics(nil)
	m.observeRun("some-weird-status", 1.0) // must NOT create an unbounded label
	body := scrape(t, m)
	assert.Contains(t, body, `agentengine_run_outcomes_total{outcome="unknown"} 1`)
	assert.NotContains(t, body, "some-weird-status")
}

func TestMetrics_WorkerActiveGauge(t *testing.T) {
	m := newMetrics(nil)
	m.incWorkerActive()
	m.incWorkerActive()
	m.decWorkerActive()
	body := scrape(t, m)
	assert.Contains(t, body, "agentengine_run_worker_active 1")
}

func TestMetrics_QueueDepth_FromStore(t *testing.T) {
	m := newMetrics(fakeQueued{n: 7})
	body := scrape(t, m)
	assert.Contains(t, body, "agentengine_run_queue_depth 7")
}

func TestMetrics_QueueDepth_OmittedOnError(t *testing.T) {
	// A store read error must OMIT the sample (Prometheus marks it stale) rather than emit a
	// false 0 that would silently clear a backlog alert.
	m := newMetrics(fakeQueued{err: errors.New("db down")})
	body := scrape(t, m)
	assert.NotContains(t, body, "agentengine_run_queue_depth ")
}

func TestMetrics_QueueDepth_AbsentWhenStoreLacksCapability(t *testing.T) {
	// A store that doesn't implement CountQueued (a hot store) simply omits the gauge.
	m := newMetrics(nil)
	body := scrape(t, m)
	assert.False(t, strings.Contains(body, "agentengine_run_queue_depth"),
		"queue-depth gauge must be absent when the store can't count")
}

func TestMetrics_NilSafe(t *testing.T) {
	// The update helpers are nil-safe so a Server built without metrics never panics.
	var m *Metrics
	assert.NotPanics(t, func() {
		m.observeRun("succeeded", 1)
		m.incWorkerActive()
		m.decWorkerActive()
	})
}
