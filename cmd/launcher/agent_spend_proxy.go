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

// httpAgentSpendStore is the Q8 agentSpendStore that books per-agent spend through the state-layer
// PROXY over HTTP (POST /quota/agent-spend) instead of a direct Valkey connection — the proxy-mode
// analogue of redisAgentSpendStore, mirroring httpTenantStore. The proxy resolves the per-agent
// identity ({ns}/{name}) SERVER-SIDE from this launcher's pod-identity token, so the store sends NO
// scope id (the scopeID arg is ignored, kept only to satisfy the agentSpendStore interface). It reuses
// the shared readPodToken / refuseRedirect helpers.
type httpAgentSpendStore struct {
	baseURL   string
	tokenPath string
	client    *http.Client
}

func newHTTPAgentSpendStore(baseURL, tokenPath string) *httpAgentSpendStore {
	return &httpAgentSpendStore{
		baseURL:   strings.TrimRight(baseURL, "/"),
		tokenPath: tokenPath,
		client:    &http.Client{Timeout: quotaProxyTimeout, CheckRedirect: refuseRedirect},
	}
}

// AddSpend posts the per-agent spend delta to the proxy's /quota/agent-spend. Best-effort, like the
// direct-Valkey booker: a lost add merely under-counts the durable rollup, never blocks a model call
// (the accountant's postCall logs + swallows the error). A 404 (untenanted / no per-agent scope) is a
// no-op. The scopeID is ignored — the proxy derives {ns}/{name} un-forgeably from the pod token.
func (s *httpAgentSpendStore) AddSpend(ctx context.Context, _ string, deltaUSD float64) error {
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(struct {
		DeltaUSD float64 `json:"deltaUSD"`
	}{DeltaUSD: deltaUSD})

	tok, err := readPodToken(s.tokenPath)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/quota/agent-spend", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return statusErr("agent-spend", resp.StatusCode)
	}
}
