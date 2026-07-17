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

package credplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/credresolve"
)

// defaultTimeout bounds a delegation round-trip. The central service may itself refresh a
// token at the AS on a miss, so it is generous but bounded.
const defaultTimeout = 20 * time.Second

// maxResponseBytes bounds the delegation response body.
const maxResponseBytes = 1 << 16

// Client is the sidecar-side delegating resolver: it implements
// credresolve.CredentialResolver by calling the central token service over (m)TLS, so the
// sidecar swaps an embedded backend for this with no other change (ADR 0030 §1).
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a delegating client for the central service at baseURL. httpClient
// supplies the (m)TLS transport + timeout; nil uses a default with defaultTimeout.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}
}

// Resolve delegates to the central service and maps its structured error codes back to the
// credresolve sentinels, so callers branch identically whether resolution is embedded or
// delegated.
func (c *Client) Resolve(ctx context.Context, ns, server, userHash string) (credresolve.Credential, error) {
	var resp resolveResponse
	if err := c.post(ctx, pathResolve, resolveRequest{Namespace: ns, Server: server, UserHash: userHash}, &resp); err != nil {
		return credresolve.Credential{}, err
	}
	switch resp.Error {
	case "":
		return credresolve.Credential{Kind: resp.Kind, Value: resp.Value}, nil
	case errCodeConsentRequired:
		return credresolve.Credential{}, credresolve.ErrConsentRequired
	case errCodeNoCredential:
		return credresolve.Credential{}, credresolve.ErrNoCredential
	default:
		return credresolve.Credential{}, fmt.Errorf("credplane: central service error (%s)", resp.Error)
	}
}

// Revoke delegates a revoke to the central service.
func (c *Client) Revoke(ctx context.Context, ns, server, userHash string) error {
	var resp revokeResponse
	if err := c.post(ctx, pathRevoke, revokeRequest{Namespace: ns, Server: server, UserHash: userHash}, &resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("credplane: central service revoke error (%s)", resp.Error)
	}
	return nil
}

// post sends req as JSON to path and decodes the response into out. A non-2xx or transport
// failure is a real error (never a silent empty credential).
func (c *Client) post(ctx context.Context, path string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("credplane: marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("credplane: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("credplane: call central service: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("credplane: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("credplane: central service returned %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("credplane: decode response: %w", err)
	}
	return nil
}

// Compile-time assertion that Client is a drop-in credresolve.CredentialResolver.
var _ credresolve.CredentialResolver = (*Client)(nil)
