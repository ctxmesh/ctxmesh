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
	"time"
)

// httpSeenSet is the M53 SeenSet that dedupes through the state-layer proxy instead
// of holding the Valkey credential (ADR 0050 §6 phase 2). It presents the launcher's
// projected pod token; the proxy scopes the seen-key by the pod's namespace
// SERVER-SIDE. Any non-200 (including a rejected token or an unreachable proxy) is
// an error, so the consumer FAILS CLOSED (NACK — never double-process; M11).
type httpSeenSet struct {
	baseURL   string
	tokenPath string
	client    *http.Client
}

func newHTTPSeenSet(baseURL, tokenPath string) *httpSeenSet {
	return &httpSeenSet{
		baseURL:   strings.TrimRight(baseURL, "/"),
		tokenPath: tokenPath,
		client:    &http.Client{Timeout: dedupeOpTimeout},
	}
}

func (s *httpSeenSet) MarkSeen(ctx context.Context, messageID string, ttl time.Duration) (bool, error) {
	tok, err := readPodToken(s.tokenPath)
	if err != nil {
		return false, err
	}
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(struct {
		MessageID  string `json:"messageID"`
		TTLSeconds int    `json:"ttlSeconds"`
	}{MessageID: messageID, TTLSeconds: int(ttl.Seconds())})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/dedup", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("dedup proxy: status %d", resp.StatusCode)
	}
	var r struct {
		FirstSeen bool `json:"firstSeen"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return false, err
	}
	return r.FirstSeen, nil
}
