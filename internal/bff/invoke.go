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

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// maxInvokeResponseBytes bounds the agent response the Playground reads back. An
// /invoke answer is a small JSON payload; capping it keeps a large/hostile
// upstream from forcing unbounded memory.
const maxInvokeResponseBytes = 4 << 20 // 4 MiB

// invokePath is appended to an agent's base endpoint (status.url) to reach its
// traced /invoke route (the launcher proxy opens the agent.invoke span there).
const invokePath = "/invoke"

// defaultCapabilityAudience is the credential-plane audience a run capability targets
// when MCP_CAPABILITY_AUDIENCE is unset — the value the sidecar / central token service
// verify against by default (runcap, ADR 0030 §2).
const defaultCapabilityAudience = "ctxmesh-credential-plane"

// runCapabilityTTL bounds a minted run capability's lifetime — comfortably longer than a
// single run (which may cold-start + call an LLM) yet short enough that a leaked
// capability expires quickly (ADR 0029 §5). Kept above the invoke round-trip timeout.
const runCapabilityTTL = 5 * time.Minute

// runCapabilityCtxKey carries the minted run capability from the /invoke handler to the
// InvokeAdapter through the request context (like the handler-supplied traceparent seed),
// so the pure-HTTP adapter attaches it without the handler reaching into the adapter.
type runCapabilityCtxKey struct{}

// contextWithRunCapability returns ctx carrying the run-capability token for the adapter
// to attach as the runcap header on the outbound /invoke.
func contextWithRunCapability(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, runCapabilityCtxKey{}, token)
}

// runCapabilityFromContext returns the run-capability token carried on ctx, or "" when
// none was minted (minting disabled, or an unattended/dev path).
func runCapabilityFromContext(ctx context.Context) string {
	token, _ := ctx.Value(runCapabilityCtxKey{}).(string)
	return token
}

// httpInvokeAdapter is the concrete InvokeAdapter (m12.7). It is a PURE HTTP
// invoker: it holds no Kubernetes client and never resolves an agent's address —
// the caller-scoped handler resolves the endpoint (AgentDeployment status.url)
// and passes it in, so the Playground run stays caller-scoped (ADR 0011).
//
// Trace hand-off: before POSTing, it mints a fresh W3C trace id and injects it as
// the `traceparent` header. The launcher's proxy CONTINUES that trace (it calls
// prop.Extract on the inbound headers), so the exported agent.invoke span lands
// under exactly this trace id in Langfuse. The adapter therefore KNOWS the run's
// traceId up front and returns it — no new trace plumbing, no response-body
// parsing, no change to the shipped launcher trace path.
//
// v1 scope: the Playground invokes the agent as an already-deployed short-lived
// Service (its status.url). The shared warm pool (PRD §21) is an optimization the
// spec flags as a v1 limitation — a caller-scoped warm-pool router would slot in
// behind this same seam without changing the handler or the trace hand-off.
type httpInvokeAdapter struct {
	client *http.Client
	// newTraceID mints the 16-byte W3C trace id (32 hex). Overridable in tests for
	// a deterministic traceparent; production uses crypto/rand.
	newTraceID func() (string, error)
}

// InvokeAdapterConfig configures the concrete InvokeAdapter.
type InvokeAdapterConfig struct {
	// HTTPClient overrides the default client (tests inject a transport pointed at
	// an httptest server). Optional.
	HTTPClient *http.Client
	// Timeout bounds a single /invoke round-trip when HTTPClient is not supplied.
	// Zero → a sane default.
	Timeout time.Duration
}

// NewInvokeAdapter returns the concrete InvokeAdapter. It needs no credentials
// and no cluster access — it is a pure HTTP invoker the caller-scoped handler
// drives with a resolved endpoint (the agent's status.url).
func NewInvokeAdapter(cfg InvokeAdapterConfig) InvokeAdapter {
	c := cfg.HTTPClient
	if c == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			// Playground runs may cold-start (scale-from-zero) + call an LLM; give a
			// generous but bounded window so a slow run does not hang the request.
			timeout = 60 * time.Second
		}
		c = &http.Client{Timeout: timeout}
	}
	return &httpInvokeAdapter{
		client:     c,
		newTraceID: randomTraceID,
	}
}

// Invoke implements InvokeAdapter.
func (a *httpInvokeAdapter) Invoke(ctx context.Context, endpoint string, body []byte) ([]byte, string, error) {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if base == "" {
		return nil, "", fmt.Errorf("invoke: empty agent endpoint")
	}

	traceID, err := a.newTraceID()
	if err != nil {
		return nil, "", fmt.Errorf("invoke: mint trace id: %w", err)
	}
	spanID, err := randomSpanID()
	if err != nil {
		return nil, "", fmt.Errorf("invoke: mint span id: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+invokePath, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("invoke: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// W3C traceparent: version 00, our minted trace id + a root span id, sampled
	// (01). The launcher's prop.Extract continues THIS trace, so the span it
	// exports carries traceID — the id we hand back for the trace-tree/deep-view.
	req.Header.Set("traceparent", fmt.Sprintf("00-%s-%s-01", traceID, spanID))
	// Run capability (runcap, ADR 0030 §2): when the caller-scoped handler minted one, it
	// travels here on the context. Attach it as the runcap header — the launcher passes it
	// through to the agent (like traceparent), the SDK relays it on each tool call, and the
	// egress sidecar verifies it to resolve THIS user's OBO credential. Absent ⇒ the run
	// carries no capability (unattended / dev / minting-disabled).
	if capToken := runCapabilityFromContext(ctx); capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("invoke: request to %s failed: %w", base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxInvokeResponseBytes))
	if readErr != nil {
		return nil, "", fmt.Errorf("invoke: read response: %w", readErr)
	}

	// A non-2xx from the agent is surfaced honestly with its status + body so the
	// Playground shows the real failure (never a swallowed success). The traceId is
	// still returned: the (failed) run was traced and its deep-view is useful.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, traceID, &invokeError{status: resp.StatusCode, body: data}
	}
	return data, traceID, nil
}

// invokeError carries the agent's non-2xx status so the handler maps it to an
// honest HTTP response (a failed run is never reported as 200).
type invokeError struct {
	status int
	body   []byte
}

func (e *invokeError) Error() string {
	snippet := strings.TrimSpace(string(e.body))
	if len(snippet) > 256 {
		snippet = snippet[:256]
	}
	return fmt.Sprintf("agent returned %d: %s", e.status, snippet)
}

// randomTraceID returns a random 16-byte W3C trace id as 32 lowercase hex chars.
func randomTraceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// randomSpanID returns a random 8-byte W3C span id as 16 lowercase hex chars.
func randomSpanID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
