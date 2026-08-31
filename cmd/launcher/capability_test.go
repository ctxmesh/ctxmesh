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

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// TestLauncherForwardsRunCapability locks the M25 OBO contract (ADR 0030 §2): the launcher
// proxy passes the run-capability header through to the agent (user) container UNCHANGED —
// like traceparent — so the SDK can relay the invoking user's identity to the egress
// sidecar. The reverse proxy forwards it for free today (it is not a hop-by-hop header);
// this test guards against a future header-allowlist silently dropping it.
func TestLauncherForwardsRunCapability(t *testing.T) {
	var gotCap string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCap = r.Header.Get(runcap.HeaderName)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)

	_, tp := newTestTracer(t)
	cfg := Config{AgentName: "test-agent"}
	handler := buildHandler(tp.Tracer(tracerName), propagation.TraceContext{}, upstreamURL, cfg, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/invoke", nil)
	req.Header.Set(runcap.HeaderName, "cap-token-abc")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "cap-token-abc", gotCap, "the launcher must pass the run capability through to the agent")
}
