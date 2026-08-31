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
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePrometheus stubs the Prometheus instant-query API, returning the given
// JSON envelope and recording the query + bearer token so the test can assert
// the token stays server-side.
func fakePrometheus(t *testing.T, envelope string) (*httptest.Server, *promRecorded) {
	t.Helper()
	rec := &promRecorded{}
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

type promRecorded struct {
	query      string
	authHeader string
}

func TestNewPrometheusAdapterRequiresURL(t *testing.T) {
	_, err := NewPrometheusAdapter(PrometheusConfig{})
	assert.Error(t, err, "missing BaseURL must error → caller leaves adapter nil")
}

func TestPrometheusQueryProjectsVector(t *testing.T) {
	envelope := `{
		"status":"success",
		"data":{"resultType":"vector","result":[
			{"metric":{"agent":"echo"},"value":[1720000000,"3"]},
			{"metric":{"agent":"summarizer"},"value":[1720000000,"1"]}
		]}
	}`
	srv, rec := fakePrometheus(t, envelope)
	a, err := NewPrometheusAdapter(PrometheusConfig{BaseURL: srv.URL, BearerToken: "tok-secret"})
	require.NoError(t, err)

	pts, err := a.Query(context.Background(), "sum by (agent) (ctxmesh_agent_replicas)")
	require.NoError(t, err)
	require.Len(t, pts, 2)
	// Sorted deterministically by label.
	assert.Equal(t, "echo", pts[0].Label)
	assert.InDelta(t, 3.0, pts[0].Value, 1e-9)
	assert.Equal(t, "summarizer", pts[1].Label)
	assert.InDelta(t, 1.0, pts[1].Value, 1e-9)

	// The bearer token is sent server-side and never returned in a DTO.
	assert.Equal(t, "Bearer tok-secret", rec.authHeader)
	assert.Contains(t, rec.query, "ctxmesh_agent_replicas")
}

func TestPrometheusQueryEmptyIsNonNil(t *testing.T) {
	srv, _ := fakePrometheus(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	a, err := NewPrometheusAdapter(PrometheusConfig{BaseURL: srv.URL})
	require.NoError(t, err)

	pts, err := a.Query(context.Background(), "up")
	require.NoError(t, err)
	assert.NotNil(t, pts)
	assert.Empty(t, pts)
}

func TestPrometheusQueryErrorStatusSurfaces(t *testing.T) {
	srv, _ := fakePrometheus(t, `{"status":"error","error":"parse error"}`)
	a, err := NewPrometheusAdapter(PrometheusConfig{BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = a.Query(context.Background(), "bad{query")
	require.Error(t, err, "a Prometheus error status must not be swallowed")
	assert.Contains(t, err.Error(), "parse error")
}

func TestPrometheusNoTokenSendsNoAuthHeader(t *testing.T) {
	srv, rec := fakePrometheus(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	a, err := NewPrometheusAdapter(PrometheusConfig{BaseURL: srv.URL})
	require.NoError(t, err)

	_, err = a.Query(context.Background(), "up")
	require.NoError(t, err)
	assert.Empty(t, rec.authHeader, "no token configured → no Authorization header")
}
