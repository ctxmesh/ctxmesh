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

package promql

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recorded struct {
	query      string
	authHeader string
}

// fakeProm stubs the Prometheus instant-query API, returning the given JSON
// envelope and recording the query + bearer token so the test can assert the
// token stays server-side.
func fakeProm(t *testing.T, envelope string) (*httptest.Server, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.query = r.URL.Query().Get("query")
		rec.authHeader = r.Header.Get("Authorization")
		if r.URL.Path == "/api/v1/query" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(envelope))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestNewRequiresURL(t *testing.T) {
	_, err := New(Config{})
	assert.Error(t, err, "missing BaseURL must error → caller leaves the client nil")
}

func TestQueryProjectsVectorSortedByLabel(t *testing.T) {
	envelope := `{
		"status":"success",
		"data":{"resultType":"vector","result":[
			{"metric":{"agent":"summarizer"},"value":[1720000000,"1"]},
			{"metric":{"agent":"echo"},"value":[1720000000,"3"]}
		]}
	}`
	srv, rec := fakeProm(t, envelope)
	c, err := New(Config{BaseURL: srv.URL, BearerToken: "tok-secret"})
	require.NoError(t, err)

	out, err := c.Query(context.Background(), "sum by (agent) (agent_engine_agent_replicas)")
	require.NoError(t, err)
	require.Len(t, out, 2)
	// Deterministic label order regardless of the server's result order.
	assert.Equal(t, "echo", out[0].Label)
	assert.InDelta(t, 3.0, out[0].Value, 1e-9)
	assert.Equal(t, "summarizer", out[1].Label)
	assert.InDelta(t, 1.0, out[1].Value, 1e-9)

	assert.Equal(t, "Bearer tok-secret", rec.authHeader)
	assert.Contains(t, rec.query, "agent_engine_agent_replicas")
}

func TestSeriesLabelFallback(t *testing.T) {
	// No "agent" label → prefer __name__.
	envelope := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"__name__":"up","instance":"x"},"value":[1720000000,"1"]}
	]}}`
	srv, _ := fakeProm(t, envelope)
	c, err := New(Config{BaseURL: srv.URL})
	require.NoError(t, err)

	out, err := c.Query(context.Background(), "up")
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "up", out[0].Label)
}

func TestQueryEmptyResultIsNonNil(t *testing.T) {
	srv, _ := fakeProm(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	c, err := New(Config{BaseURL: srv.URL})
	require.NoError(t, err)

	out, err := c.Query(context.Background(), "up")
	require.NoError(t, err)
	assert.NotNil(t, out)
	assert.Empty(t, out)
}

func TestQueryEmptyStringErrors(t *testing.T) {
	c, err := New(Config{BaseURL: "http://prometheus.example:9090"})
	require.NoError(t, err)
	_, err = c.Query(context.Background(), "   ")
	assert.Error(t, err, "an empty query must error, not hit the API")
}

func TestQueryErrorStatusSurfaces(t *testing.T) {
	srv, _ := fakeProm(t, `{"status":"error","error":"parse error"}`)
	c, err := New(Config{BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = c.Query(context.Background(), "bad{query")
	require.Error(t, err, "a Prometheus error status must not be swallowed")
	assert.Contains(t, err.Error(), "parse error")
}

func TestQueryNoTokenSendsNoAuthHeader(t *testing.T) {
	srv, rec := fakeProm(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	c, err := New(Config{BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = c.Query(context.Background(), "up")
	require.NoError(t, err)
	assert.Empty(t, rec.authHeader, "no token configured → no Authorization header")
}
