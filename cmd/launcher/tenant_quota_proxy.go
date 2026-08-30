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
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ctxmesh/agentry/internal/gateway/budget"
)

// ErrQuotaProxyRejected is returned when the proxy DEFINITIVELY rejects the
// launcher's pod token (HTTP 401) — the token is invalid, not a transient blip. Per
// ADR 0050 Amд 3, preCall must fail RATE + CONCURRENCY (and budget) CLOSED on this,
// NOT open: a definitively-unauthenticated caller must not slip past the caps.
var ErrQuotaProxyRejected = errors.New("quota proxy: pod token rejected (401)")

// httpTenantStore is the M53 tenantQuotaStore that talks to the control-plane
// state-layer proxy over HTTP instead of holding the Valkey credential directly
// (ADR 0050 §8 phase 2). The proxy derives the tenant SERVER-SIDE from this
// launcher's pod-identity token, so the store sends NO tenant id — the tenantID
// args are ignored (kept only to satisfy the tenantQuotaStore interface shared
// with the legacy direct-Valkey store).
//
// Fail-mode mapping (ADR 0050 Amд 3, refined — see the note on 401 below): a 404
// means the proxy has no tenant for this namespace → return the PERMISSIVE value
// (the launcher's existing nil-quota "allow" path). Any other non-2xx (401 / 503 /
// 502 / a transport error) is returned as an error so preCall applies its existing
// policy — BUDGET fails CLOSED, RATE + CONCURRENCY fail OPEN.
type httpTenantStore struct {
	baseURL   string
	tokenPath string
	client    *http.Client
}

func newHTTPTenantStore(baseURL, tokenPath string) *httpTenantStore {
	return &httpTenantStore{
		baseURL:   strings.TrimRight(baseURL, "/"),
		tokenPath: tokenPath,
		client:    &http.Client{Timeout: quotaProxyTimeout, CheckRedirect: refuseRedirect},
	}
}

const quotaProxyTimeout = 3 * time.Second

// token re-reads the projected SA token on every call. The file is on tmpfs and
// kubelet rotates it in place, so re-reading (rather than caching) always presents
// the current token without a rotation-staleness bug (ADR 0050 Amд 3).
func (s *httpTenantStore) token() (string, error) {
	return readPodToken(s.tokenPath)
}

// readPodToken reads + validates the mounted projected SA token. Shared by the
// quota and dedup proxy clients. An empty/whitespace file (a mid-rotation partial
// snapshot) is a hard error, never a silent empty Bearer.
func readPodToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read pod token %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("pod token %s is empty or all-whitespace", path)
	}
	return tok, nil
}

// resolvePodTokenPath returns the explicit STATELAYER_TOKEN_PATH or the default
// mount path — shared by the gateway quota client and the async dedup client.
func resolvePodTokenPath(explicit string) string {
	if p := strings.TrimSpace(explicit); p != "" {
		return p
	}
	return defaultPodTokenPath
}

// call issues an authenticated request and returns the response for the caller to
// decode. The caller MUST close resp.Body.
func (s *httpTenantStore) call(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	tok, err := s.token()
	if err != nil {
		return nil, err
	}
	// bytes.NewReader handles a nil body (an empty reader → Content-Length 0).
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

// statusErr turns a non-2xx/404 response into an error (so preCall's fail policy
// fires). A 401 becomes ErrQuotaProxyRejected — a DEFINITIVE rejection preCall
// fails CLOSED on (Amд 3); any other non-2xx is a transient error preCall handles
// per its default policy (budget CLOSED, rate/concurrency OPEN).
func statusErr(op string, code int) error {
	if code == http.StatusUnauthorized {
		return fmt.Errorf("quota proxy %s: %w", op, ErrQuotaProxyRejected)
	}
	return fmt.Errorf("quota proxy %s: status %d", op, code)
}

func (s *httpTenantStore) IncrRPM(ctx context.Context, _ string, _ int64) (int64, error) {
	resp, err := s.call(ctx, http.MethodPost, "/quota/rpm", nil)
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
		return 0, nil // untenanted → the count-0 allow path
	default:
		return 0, statusErr("rpm", resp.StatusCode)
	}
}

func (s *httpTenantStore) Spend(ctx context.Context, _ string) (float64, error) {
	resp, err := s.call(ctx, http.MethodGet, "/quota/spend", nil)
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
		// Untenanted → spent-0 allow (the launcher's existing nil-quota path). This is
		// the ONE spot budget "fails open": if the proxy's namespace informer hasn't yet
		// synced the label the controller already stamped (a sub-second lag it bounds,
		// ADR 0050 Amд 2 §5), a budgeted tenant sees spent=0 in that window. Bounded +
		// not attacker-inducible (an agent can't unlabel its own namespace), so accepted.
		return 0, nil
	default:
		return 0, statusErr("spend", resp.StatusCode)
	}
}

func (s *httpTenantStore) AddSpend(ctx context.Context, _ string, deltaUSD float64) error {
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(struct {
		DeltaUSD float64 `json:"deltaUSD"`
	}{DeltaUSD: deltaUSD})
	resp, err := s.call(ctx, http.MethodPost, "/quota/spend", body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound: // 404 untenanted ⇒ no-op
		return nil
	default:
		return statusErr("addspend", resp.StatusCode)
	}
}

func (s *httpTenantStore) AcquireSlot(ctx context.Context, _ string, maxSlots int) (bool, error) {
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(struct {
		Max int `json:"max"`
	}{Max: maxSlots})
	resp, err := s.call(ctx, http.MethodPost, "/quota/slot", body)
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
		return true, nil // untenanted → grant (no concurrency cap applies)
	default:
		return false, statusErr("slot", resp.StatusCode)
	}
}

func (s *httpTenantStore) ReleaseSlot(ctx context.Context, _ string) error {
	resp, err := s.call(ctx, http.MethodDelete, "/quota/slot", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return statusErr("release", resp.StatusCode)
	}
}

// Control reads a run's CONTROL verb from the pod-authed /control endpoint (m70.8, the real-kill cancel
// channel). It GETs /control/{runID} with the launcher's projected pod token and parses {"control":"…"}.
// The gateway's caller FAILS OPEN: a transport/proxy error → ("", err) so the model call proceeds (a
// control-plane blip must never spuriously kill a live run). A 200 with an empty verb (the common case:
// no cancel marker) returns ("", nil). "cancel" is the only verb the gateway acts on in v1.
func (s *httpTenantStore) Control(ctx context.Context, runID string) (string, error) {
	resp, err := s.call(ctx, http.MethodGet, "/control/"+runID, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var r struct {
			Control string `json:"control"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return "", err
		}
		return r.Control, nil
	default:
		// Any non-200 (401 / 404 / 502 / 503) → no trusted verb; the caller fails open (no cancel).
		return "", statusErr("control", resp.StatusCode)
	}
}

// ── F2 (ADR 0099): budget.SpendBackend over the statelayer-proxy ──────────────
// The per-AGENT identity is proxy-derived from the pod token (no name sent); the conversation id is
// supplied as a query param. Reads FAIL CLOSED (any non-200 → error → the Enforcer refuses the call).

func (s *httpTenantStore) AgentSpent(ctx context.Context) (budget.Money, error) {
	resp, err := s.call(ctx, http.MethodGet, "/quota/agent-spend", nil)
	if err != nil {
		return budget.Zero(), err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return budget.Zero(), statusErr("agent-spend", resp.StatusCode)
	}
	return decodeSpendUSD(resp.Body)
}

func (s *httpTenantStore) ConvSpent(ctx context.Context, convID string) (budget.Money, error) {
	resp, err := s.call(ctx, http.MethodGet, "/quota/conv-spend?conversation="+url.QueryEscape(convID), nil)
	if err != nil {
		return budget.Zero(), err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return budget.Zero(), statusErr("conv-spend", resp.StatusCode)
	}
	return decodeSpendUSD(resp.Body)
}

func (s *httpTenantStore) AddConvSpend(ctx context.Context, convID string, delta budget.Money) error {
	//nolint:errcheck // a struct of scalar fields cannot fail to marshal.
	body, _ := json.Marshal(struct {
		DeltaUSD float64 `json:"deltaUSD"`
	}{DeltaUSD: moneyToFloat(delta.String())})
	resp, err := s.call(ctx, http.MethodPost, "/quota/conv-spend?conversation="+url.QueryEscape(convID), body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return statusErr("add-conv-spend", resp.StatusCode)
	}
	return nil
}

// decodeSpendUSD reads {"spentUSD":float} and converts to exact Money (float64 → big.Rat; the
// accepted ADR-0099 precision trade — exact well past cents for realistic caps).
func decodeSpendUSD(body io.Reader) (budget.Money, error) {
	var r struct {
		SpentUSD float64 `json:"spentUSD"`
	}
	if err := json.NewDecoder(body).Decode(&r); err != nil {
		return budget.Zero(), err
	}
	return budget.MoneyFromRat(new(big.Rat).SetFloat64(r.SpentUSD)), nil
}
