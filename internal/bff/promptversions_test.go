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

// Shared PromptVersion test helpers. The behavioural tests live in
// promptversions_readswitch_test.go (list/get/diff) + promptversions_retire_test.go
// (create/update/delete) — PromptVersion is Postgres-only (ADR 0044), so every
// endpoint is exercised against the store, not a CRD fake client.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agentry/internal/prompt"
)

// pvNS is the namespace used in PromptVersion tests.
const pvNS = "team-prompts"

// --- request helpers --------------------------------------------------------

func getPromptVersions(t *testing.T, s *Server, rawQuery string) (PromptVersionListResponse, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	url := "/api/promptversions"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body PromptVersionListResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

//nolint:unparam
func getPromptVersion(t *testing.T, s *Server, ns, name string) (*PromptVersionDetail, int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/promptversions/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail PromptVersionDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func createPromptVersion(t *testing.T, s *Server, reqBody PromptVersionCreateRequest) (*PromptVersionDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/promptversions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		var detail PromptVersionDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

//nolint:unparam
func putPromptVersion(t *testing.T, s *Server, ns, name string, reqBody PromptVersionUpdateRequest) (*PromptVersionDetail, int, string) {
	t.Helper()
	b, err := json.Marshal(reqBody)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/promptversions/"+ns+"/"+name, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var detail PromptVersionDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		return &detail, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

func deletePromptVersion(t *testing.T, s *Server, ns, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/promptversions/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

//nolint:unparam
func getPromptVersionDiff(t *testing.T, s *Server, ns, name, fromName string) (*PromptVersionDiffResponse, int, string) {
	t.Helper()
	url := "/api/promptversions/" + ns + "/" + name + "/diff"
	if fromName != "" {
		url += "?from=" + fromName
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		var body PromptVersionDiffResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return &body, rec.Code, rec.Body.String()
	}
	return nil, rec.Code, rec.Body.String()
}

// newCallerServerWithResolver builds a Server with a prompt Resolver wired.
func newCallerServerWithResolver(t *testing.T, factory CallerClientFactory, resolver prompt.Resolver) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients:  factory,
		Scheme:         testScheme(t),
		Auth:           AllowAll{},
		Adapters:       Adapters{Expand: NewExpandAdapter()},
		Version:        "test",
		Log:            logr.Discard(),
		PromptResolver: resolver,
	})
}
