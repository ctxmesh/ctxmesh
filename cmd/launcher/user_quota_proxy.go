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
	"fmt"
	"net/http"
	"strings"
)

// httpUserStore is the M107 C20 userQuotaStore that talks to the control-plane
// state-layer proxy over HTTP instead of holding the Valkey credential directly.
// It is the proxy-mode analogue of redisUserStore (cmd/launcher/user_quota.go),
// mirroring httpTenantStore (cmd/launcher/tenant_quota_proxy.go).
//
// Unlike the tenant store, the proxy CANNOT derive the end-user from the pod token
// (a pod token identifies the agent, not the invoking user). The store therefore PASSES
// the userHash in the POST body (or GET query) for every op — the same trust model as
// direct-Valkey mode: the launcher is the enforcement point, the proxy is the store.
//
// A 404 response means the user-quota endpoint is not configured on this proxy → return
// the PERMISSIVE value (the launcher's existing nil-quota "allow" path), matching the
// behaviour of httpTenantStore on an untenanted namespace.
type httpUserStore struct {
	baseURL   string
	tokenPath string
	client    *http.Client
}

func newHTTPUserStore(baseURL, tokenPath string) *httpUserStore {
	return &httpUserStore{
		baseURL:   strings.TrimRight(baseURL, "/"),
		tokenPath: tokenPath,
		client:    &http.Client{Timeout: quotaProxyTimeout, CheckRedirect: refuseRedirect},
	}
}

// call issues an authenticated request to the user-quota endpoint and returns the
// response for the caller to decode. The caller MUST close resp.Body.
func (s *httpUserStore) call(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	tok, err := readPodToken(s.tokenPath)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.client.Do(req)
}

func (s *httpUserStore) IncrRPM(ctx context.Context, userHash string, window int64) (int64, error) {
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(struct {
		UserHash string `json:"userHash"`
		Window   int64  `json:"window"`
	}{UserHash: userHash, Window: window})
	resp, err := s.call(ctx, http.MethodPost, "/quota/user-rpm", body)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var r struct {
			Count int64 `json:"count"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return 0, err
		}
		return r.Count, nil
	case http.StatusNotFound:
		return 0, nil // not configured → count-0 allow path
	default:
		return 0, fmt.Errorf("quota proxy user-rpm: status %d", resp.StatusCode)
	}
}

func (s *httpUserStore) Spend(ctx context.Context, userHash string) (float64, error) {
	resp, err := s.call(ctx, http.MethodGet,
		"/quota/user-spend?userHash="+userHash, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var r struct {
			SpentUSD float64 `json:"spentUSD"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return 0, err
		}
		return r.SpentUSD, nil
	case http.StatusNotFound:
		// Not configured → spent-0 allow (the launcher's existing nil-quota path).
		return 0, nil
	default:
		return 0, fmt.Errorf("quota proxy user-spend: status %d", resp.StatusCode)
	}
}

func (s *httpUserStore) AddSpend(ctx context.Context, userHash string, deltaUSD float64) error {
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(struct {
		UserHash string  `json:"userHash"`
		DeltaUSD float64 `json:"deltaUSD"`
	}{UserHash: userHash, DeltaUSD: deltaUSD})
	resp, err := s.call(ctx, http.MethodPost, "/quota/user-spend", body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound: // 404 not configured ⇒ no-op
		return nil
	default:
		return fmt.Errorf("quota proxy user-spend add: status %d", resp.StatusCode)
	}
}

func (s *httpUserStore) AcquireSlot(ctx context.Context, userHash string, maxSlots int) (bool, error) {
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(struct {
		UserHash string `json:"userHash"`
		Max      int    `json:"max"`
	}{UserHash: userHash, Max: maxSlots})
	resp, err := s.call(ctx, http.MethodPost, "/quota/user-slot", body)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var r struct {
			Acquired bool `json:"acquired"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return false, err
		}
		return r.Acquired, nil
	case http.StatusNotFound:
		return true, nil // not configured → grant (no concurrency cap applies)
	default:
		return false, fmt.Errorf("quota proxy user-slot: status %d", resp.StatusCode)
	}
}

func (s *httpUserStore) ReleaseSlot(ctx context.Context, userHash string) error {
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(struct {
		UserHash string `json:"userHash"`
	}{UserHash: userHash})
	resp, err := s.call(ctx, http.MethodDelete, "/quota/user-slot", body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("quota proxy user-slot release: status %d", resp.StatusCode)
	}
}
