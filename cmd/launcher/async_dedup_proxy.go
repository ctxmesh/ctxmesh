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

	"github.com/ctxmesh/ctxmesh/internal/deliveryclaim"
)

// httpSeenSet is the M53 SeenSet that dedupes through the state-layer proxy instead
// of holding the Valkey credential (ADR 0050 §6 phase 2). It presents the launcher's
// projected pod token; the proxy scopes the seen-key by the pod's namespace
// SERVER-SIDE. Any non-200 (including a rejected token or an unreachable proxy) is
// an error, so a claim FAILS CLOSED (NACK — never double-process; M11).
type httpSeenSet struct {
	baseURL   string
	tokenPath string
	client    *http.Client
}

func newHTTPSeenSet(baseURL, tokenPath string) *httpSeenSet {
	return &httpSeenSet{
		baseURL:   strings.TrimRight(baseURL, "/"),
		tokenPath: tokenPath,
		client:    &http.Client{Timeout: dedupeOpTimeout, CheckRedirect: refuseRedirect},
	}
}

func (s *httpSeenSet) Claim(
	ctx context.Context, messageID, token string, ttl time.Duration,
) (deliveryclaim.State, error) {
	var r struct {
		FirstSeen bool                `json:"firstSeen"`
		State     deliveryclaim.State `json:"state"`
	}
	call := dedupCall{MessageID: messageID, TTLSeconds: int(ttl.Seconds()), Op: "claim", Token: token}
	if err := s.post(ctx, call, &r); err != nil {
		return "", err
	}
	switch r.State {
	case deliveryclaim.Claimed, deliveryclaim.Done, deliveryclaim.InFlight:
		return r.State, nil
	case "":
		// A proxy that predates claims answered a MarkSeen: process a first sighting, and
		// take anything else for a duplicate, as before.
		if r.FirstSeen {
			return deliveryclaim.Claimed, nil
		}
		return deliveryclaim.Done, nil
	default:
		return "", fmt.Errorf("dedup proxy: unknown claim state %q", r.State)
	}
}

func (s *httpSeenSet) Complete(ctx context.Context, messageID string, ttl time.Duration) error {
	return s.post(ctx, dedupCall{MessageID: messageID, TTLSeconds: int(ttl.Seconds()), Op: "complete"}, nil)
}

func (s *httpSeenSet) Release(ctx context.Context, messageID, token string) error {
	return s.post(ctx, dedupCall{MessageID: messageID, Op: "release", Token: token}, nil)
}

// dedupCall is the proxy's /dedup request (internal/statelayer dedupRequest).
type dedupCall struct {
	MessageID  string `json:"messageID"`
	TTLSeconds int    `json:"ttlSeconds,omitempty"`
	Op         string `json:"op"`
	Token      string `json:"token,omitempty"`
}

// post sends one /dedup call and decodes the answer into out, when out is non-nil.
func (s *httpSeenSet) post(ctx context.Context, call dedupCall, out any) error {
	tok, err := readPodToken(s.tokenPath)
	if err != nil {
		return err
	}
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(call)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/dedup", bytes.NewReader(body))
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
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dedup proxy: status %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
