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

// Package toolpush is the controller's manifest push channel to the discovery
// sidecar. It POSTs a Manifest to http://<podIP>:2999/control with a short
// timeout and a small retry — the propagation path of the M4 hot update
// (specs/mcp-tools.md — "Hot path vs cold path"). Pushes are best-effort: the
// ConfigMap backing guarantees eventual consistency even if every push fails,
// so failures are logged, not fatal.
package toolpush

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ctxmesh/agentry/internal/toolmanifest"
)

const (
	// ControlPort is the discovery sidecar's HTTP port.
	ControlPort = 2999

	// controlPath is the manifest-replace endpoint on the sidecar.
	controlPath = "/control"

	// pushTimeout bounds a single POST /control so a hung/restarting pod never
	// stalls the reconciler. Kept short — the sidecar is in-cluster.
	pushTimeout = 2 * time.Second

	// maxAttempts is the total attempts per pod (1 try + retries). Small: the
	// ConfigMap backing is the durable fallback; we don't block reconciles on a
	// flaky pod.
	maxAttempts = 2
)

// Pusher POSTs manifests to discovery sidecars. Zero value is usable; it lazily
// builds an http.Client with the push timeout. Inject Client in tests to point
// at an httptest server.
type Pusher struct {
	// Client is the HTTP client used for POSTs. If nil, a client with
	// pushTimeout is created on first use.
	Client *http.Client
}

func (p *Pusher) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	p.Client = &http.Client{Timeout: pushTimeout}
	return p.Client
}

// PushURL returns the /control URL for a pod IP.
func PushURL(podIP string) string {
	return fmt.Sprintf("http://%s:%d%s", podIP, ControlPort, controlPath)
}

// Push POSTs the manifest to a single sidecar at controlURL, retrying up to
// maxAttempts on transport error or non-2xx. It returns the last error (nil on
// a 2xx). Callers treat a non-nil error as non-fatal (log + rely on the CM
// backing). Each attempt is bounded by pushTimeout via the client and a
// per-attempt context derived from ctx.
func (p *Pusher) Push(ctx context.Context, controlURL string, m toolmanifest.Manifest) error {
	body, err := json.Marshal(m)
	if err != nil {
		// Manifest is string-only; Marshal cannot fail in practice.
		return fmt.Errorf("marshalling manifest: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = p.pushOnce(ctx, controlURL, body)
		if lastErr == nil {
			return nil
		}
		// If the parent context is done, stop retrying.
		if ctx.Err() != nil {
			return lastErr
		}
	}
	return lastErr
}

// pushOnce performs a single POST with a per-attempt timeout.
func (p *Pusher) pushOnce(ctx context.Context, controlURL string, body []byte) error {
	attemptCtx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, controlURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client().Do(req)
	if err != nil {
		return fmt.Errorf("posting to %s: %w", controlURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("push to %s returned status %d", controlURL, resp.StatusCode)
	}
	return nil
}
