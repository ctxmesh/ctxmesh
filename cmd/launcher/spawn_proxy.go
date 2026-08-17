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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// httpSpawnStore implements spawnGuardStore over the state-layer proxy (M94, closing audit P1-2). It moves
// the AgentTeam-supervisor spawn guard OFF direct Valkey (:6379) and behind the pod-authed proxy, so no agent
// reaches the shared unauthenticated store. The proxy scopes the counter keys by the pod's verified
// namespace. Any proxy error → (false, err), and SpawnGuard.Admit maps that to SpawnDeniedError — i.e. the
// guard **fails CLOSED** (a spawn is denied when the proxy is unreachable, never silently admitted).
type httpSpawnStore struct {
	baseURL   string
	tokenPath string
	client    *http.Client
}

func newHTTPSpawnStore(baseURL, tokenPath string) *httpSpawnStore {
	return &httpSpawnStore{
		baseURL:   strings.TrimRight(baseURL, "/"),
		tokenPath: tokenPath,
		client:    &http.Client{Timeout: quotaProxyTimeout},
	}
}

func (s *httpSpawnStore) call(ctx context.Context, path string, body []byte) (*http.Response, error) {
	tok, err := readPodToken(s.tokenPath)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	return s.client.Do(req)
}

func (s *httpSpawnStore) acquire(ctx context.Context, scope, rootRunID, counter string, max int) (bool, error) {
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(struct {
		Scope     string `json:"scope"`
		RootRunID string `json:"rootRunId"`
		Counter   string `json:"counter"`
		Max       int    `json:"max"`
	}{scope, rootRunID, counter, max})
	resp, err := s.call(ctx, "/spawn/acquire", body)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, statusErr("spawn/acquire", resp.StatusCode)
	}
	var r struct {
		Acquired bool `json:"acquired"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return false, err
	}
	return r.Acquired, nil
}

func (s *httpSpawnStore) release(ctx context.Context, scope, rootRunID, counter string) error {
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(struct {
		Scope     string `json:"scope"`
		RootRunID string `json:"rootRunId"`
		Counter   string `json:"counter"`
	}{scope, rootRunID, counter})
	resp, err := s.call(ctx, "/spawn/release", body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return statusErr("spawn/release", resp.StatusCode)
	}
	return nil
}

func (s *httpSpawnStore) AcquireInflight(ctx context.Context, scope, rootRunID string, max int) (bool, error) {
	return s.acquire(ctx, scope, rootRunID, "inflight", max)
}

func (s *httpSpawnStore) ReleaseInflight(ctx context.Context, scope, rootRunID string) error {
	return s.release(ctx, scope, rootRunID, "inflight")
}

func (s *httpSpawnStore) AcquireTotal(ctx context.Context, scope, rootRunID string, max int) (bool, error) {
	return s.acquire(ctx, scope, rootRunID, "count", max)
}

func (s *httpSpawnStore) ReleaseTotal(ctx context.Context, scope, rootRunID string) error {
	return s.release(ctx, scope, rootRunID, "count")
}
