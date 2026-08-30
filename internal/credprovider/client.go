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

package credprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ctxmesh/agentry/internal/credresolve"
)

// defaultTimeout bounds a provider round-trip (a self-refreshing backend may itself hit an
// upstream AS on a miss, so it is generous but bounded).
const defaultTimeout = 20 * time.Second

// maxResponseBytes bounds a provider response body.
const maxResponseBytes = 1 << 16

// Client dials an out-of-tree credprovider backend over (m)TLS and adapts it to
// credresolve.CredentialResolver, so the token-service swaps an in-tree backend for a
// remote one with no other change. It FAILS CLOSED: any transport/5xx failure yields an
// error, never a blank credential.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a client for the provider at baseURL. httpClient supplies the mTLS
// transport + timeout; nil uses a plain default with defaultTimeout.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}
}

// Capabilities fetches the provider's self-declared behavior (spec §A.3), so the plane can
// decide whether to wrap it with the refresh/envelope decorators.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var caps Capabilities
	if err := c.do(ctx, http.MethodGet, PathCapabilities, nil, &caps); err != nil {
		return Capabilities{}, err
	}
	return caps, nil
}

// Resolve maps the provider's structured signals back to the credresolve sentinels, so
// callers branch identically whether resolution is in-tree or remote.
func (c *Client) Resolve(ctx context.Context, ns, boundary, server, userHash string) (credresolve.Credential, error) {
	return c.resolveTenant(ctx, ns, boundary, server, userHash, "")
}

func (c *Client) resolveTenant(ctx context.Context, ns, boundary, server, userHash, tenant string) (credresolve.Credential, error) {
	var resp resolveResponse
	if err := c.do(ctx, http.MethodPost, PathResolve,
		resolveRequest{Namespace: ns, Boundary: boundary, Server: server, UserHash: userHash, Tenant: tenant}, &resp); err != nil {
		return credresolve.Credential{}, err
	}
	switch {
	case resp.Error != "":
		return credresolve.Credential{}, fmt.Errorf("credprovider: backend error (%s)", resp.Error)
	case resp.Signal == SignalConsentRequired:
		return credresolve.Credential{}, credresolve.ErrConsentRequired
	case resp.Signal == SignalNoCredential:
		return credresolve.Credential{}, credresolve.ErrNoCredential
	case resp.Signal != "":
		return credresolve.Credential{}, fmt.Errorf("credprovider: unknown signal %q", resp.Signal)
	default:
		return credresolve.Credential{Kind: resp.Kind, Value: resp.Value}, nil
	}
}

// Revoke delegates a revoke to the provider.
func (c *Client) Revoke(ctx context.Context, ns, boundary, server, userHash string) error {
	var resp ackResponse
	if err := c.do(ctx, http.MethodPost, PathRevoke,
		revokeRequest{Namespace: ns, Boundary: boundary, Server: server, UserHash: userHash}, &resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("credprovider: backend revoke error (%s)", resp.Error)
	}
	return nil
}

// Store persists a grant at the provider (the write-path surface of the contract).
func (c *Client) Store(ctx context.Context, ns, boundary, server, userHash, tenant string, grant GrantMaterial) error {
	var resp ackResponse
	if err := c.do(ctx, http.MethodPost, PathStore,
		storeRequest{Namespace: ns, Boundary: boundary, Server: server, UserHash: userHash, Tenant: tenant, Grant: grant}, &resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("credprovider: backend store error (%s)", resp.Error)
	}
	return nil
}

// StoreGrant adapts the SPI's common write payload (credresolve.Grant) to the provider's
// Store, so a `remote` backend is a config-selected GrantWriter (ADR 0032).
func (c *Client) StoreGrant(ctx context.Context, ns, boundary, server, userHash string, g credresolve.Grant) error {
	gm := GrantMaterial{AccessToken: g.Tokens.AccessToken, RefreshToken: g.Tokens.RefreshToken}
	if !g.Tokens.ExpiresAt.IsZero() {
		gm.ExpiresAtUnix = g.Tokens.ExpiresAt.Unix()
	}
	return c.Store(ctx, ns, boundary, server, userHash, "", gm)
}

// do sends req (nil for GET) as JSON to path and decodes into out. A non-2xx or transport
// failure is a real error — never a silent empty credential (fail closed).
func (c *Client) do(ctx context.Context, method, path string, req, out any) error {
	var body io.Reader
	if req != nil {
		raw, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("credprovider: marshal request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("credprovider: build request: %w", err)
	}
	httpReq.Header.Set(VersionHeader, APIVersion)
	if req != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("credprovider: call backend: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("credprovider: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("credprovider: backend returned %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("credprovider: decode response: %w", err)
		}
	}
	return nil
}

// Compile-time assertions: a drop-in resolver AND a config-selected grant writer.
var (
	_ credresolve.CredentialResolver = (*Client)(nil)
	_ credresolve.GrantWriter        = (*Client)(nil)
)
