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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// ── helpers ────────────────────────────────────────────────────────────────────

// newMockIngestServer builds an httptest.Server that acts as the BFF guardrail-event ingest.
// It records the received events and returns the given statusCode. The test can inspect
// receivedEvents to assert the PII-safe body the launcher sends.
type mockIngestServer struct {
	statusCode int
	// receivedEvents is appended by the httptest HANDLER goroutine and read by the test body, so it
	// is guarded by mu (K8) — accessed only via append-under-lock in the handler + events() in tests.
	mu             sync.Mutex
	receivedEvents []guardrailAuditEvent
	callCount      atomic.Int64
	server         *httptest.Server
}

// events returns a snapshot of the received audit events under the lock — the race-free read the
// test body uses instead of touching receivedEvents directly (K8).
func (m *mockIngestServer) events() []guardrailAuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]guardrailAuditEvent, len(m.receivedEvents))
	copy(out, m.receivedEvents)
	return out
}

func newMockIngestServer(t *testing.T, statusCode int) *mockIngestServer {
	t.Helper()
	m := &mockIngestServer{statusCode: statusCode}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.callCount.Add(1)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, guardrailAuditIngestPath) {
			body, _ := io.ReadAll(r.Body)
			var evt guardrailAuditEvent
			_ = json.Unmarshal(body, &evt)
			m.mu.Lock()
			m.receivedEvents = append(m.receivedEvents, evt)
			m.mu.Unlock()
		}
		w.WriteHeader(statusCode)
	}))
	t.Cleanup(m.server.Close)
	return m
}

// blockDecision returns a guardrailDecision with blocked=true and a content_hash (no raw content).
func blockDecision() guardrailDecision {
	return guardrailDecision{
		blocked:     true,
		detector:    "ssn",
		action:      actionBlock,
		scanPoint:   scanInput,
		contentHash: "deadbeefdeadbeefdeadbeefdeadbeef",
		startOffset: 5,
		endOffset:   16,
	}
}

// newGuardedProxyWithBFF builds a guardrail proxy with a real GUARDRAIL_POLICY and a mock
// upstream + BFF ingest. capToken is set in the request header to simulate SDK propagation.
func newGuardedProxyWithBFF(t *testing.T, upstreamURL, bffURL string) (*gatewayProxy, *tracetest.SpanRecorder) {
	t.Helper()
	rec, tp := newTestTracer(t)
	gp, err := newGatewayProxy(gatewayConfig{
		UpstreamURL:     upstreamURL,
		AgentName:       "guarded-agent",
		GuardrailPolicy: denylistPolicy("ssn", `\d{3}-\d{2}-\d{4}`, "block"),
		BFFInternalURL:  bffURL,
	}, tp.Tracer("test"), func(string, ...any) {})
	require.NoError(t, err)
	return gp, rec
}

// doBlockRequest sends a chat/completions request that triggers a guardrail BLOCK, with
// an optional run capability header (simulating the SDK's m66.7 propagation).
func doBlockRequest(gp *gatewayProxy, capToken string) *httptest.ResponseRecorder {
	// Body contains an SSN that the denylistPolicy("ssn", `\d{3}-\d{2}-\d{4}`, "block") will block.
	body := `{"messages":[{"role":"user","content":"my ssn is 123-45-6789"}]}`
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if capToken != "" {
		req.Header.Set(runcap.HeaderName, capToken)
	}
	rr := httptest.NewRecorder()
	gp.handler().ServeHTTP(rr, req)
	return rr
}

// fakeCap returns a plausible-looking but NOT cryptographically valid capability token for
// tests that don't need capability verification (the BFF mock doesn't verify it).
const fakeCap = "eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiJ1aGFzaC1hbGljZSJ9.fakesig"

// ── tests ──────────────────────────────────────────────────────────────────────

// TestGuardrailAudit_BlockTriggersPostToBFF asserts that a guardrail BLOCK decision causes
// the launcher to POST the PII-safe body to the BFF ingest endpoint. The mock ingest
// server receives the call and we assert:
//   - the body contains detector + scan_point + content_hash (no raw match)
//   - policy_action is "block"
//   - the 403 block response is returned to the client REGARDLESS (fire-and-forget)
func TestGuardrailAudit_BlockTriggersPostToBFF(t *testing.T) {
	// The upstream is never reached (block fires before forwarding) — a 500 server is fine.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	mock := newMockIngestServer(t, http.StatusNoContent)
	gp, _ := newGuardedProxyWithBFF(t, upstream.URL, mock.server.URL)

	rec := doBlockRequest(gp, fakeCap)
	assert.Equal(t, http.StatusForbidden, rec.Code, "the block must be returned to the SDK caller")
	assert.Contains(t, rec.Body.String(), guardrailBlockedType, "body must contain guardrail_blocked type")

	// Allow the background goroutine to complete.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && mock.callCount.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, int64(1), mock.callCount.Load(), "exactly one POST to the BFF ingest")
	require.Len(t, mock.events(), 1)
	evt := mock.events()[0]

	// PII-safe: the body must contain a hash, not the raw SSN "123-45-6789".
	assert.Equal(t, "ssn", evt.Detector)
	assert.Equal(t, "input", evt.ScanPoint)
	assert.NotEmpty(t, evt.ContentHash, "content_hash must be present")
	assert.NotContains(t, evt.ContentHash, "123-45-6789", "content_hash must be a hash, not the raw match")
	assert.Equal(t, "guarded-agent", evt.Agent)
	assert.Equal(t, "block", evt.PolicyAction)
}

// TestGuardrailAudit_BlockReturnedImmediatelyWhenIngestDown asserts the core fire-and-forget
// invariant: even when the BFF ingest server is DOWN (connection refused), the 403 block
// response is returned promptly. The audit is best-effort; the refusal must never be delayed.
func TestGuardrailAudit_BlockReturnedImmediatelyWhenIngestDown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	// Point the BFF URL at a port where nothing is listening (guaranteed connection-refused).
	gp, _ := newGuardedProxyWithBFF(t, upstream.URL, "http://127.0.0.1:1") // port 1 = refused

	start := time.Now()
	rec := doBlockRequest(gp, fakeCap)
	elapsed := time.Since(start)

	// The block response is returned well before the audit timeout. We allow a generous
	// 1 second — the block decision itself is immediate; only the audit goroutine contacts
	// the BFF (async). A 1s bound is ample for the synchronous handler path.
	assert.Less(t, elapsed, time.Second, "block response must be returned promptly even when BFF ingest is down")
	assert.Equal(t, http.StatusForbidden, rec.Code, "the block must still be 403 when the ingest is down")
}

// TestGuardrailAudit_MissingCapabilitySkipsDurablePost asserts that when no run capability
// is present on the model request (capability not propagated), the durable BFF POST is skipped.
// The 403 block response is still returned — the capability absence only affects the audit.
func TestGuardrailAudit_MissingCapabilitySkipsDurablePost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	mock := newMockIngestServer(t, http.StatusNoContent)
	gp, _ := newGuardedProxyWithBFF(t, upstream.URL, mock.server.URL)

	// Send WITHOUT a capability header — the launcher cannot attribute the row.
	rec := doBlockRequest(gp, "") // no cap token
	assert.Equal(t, http.StatusForbidden, rec.Code, "block must still be 403 without a capability")

	// Give the goroutine a moment to fire (if it incorrectly did).
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), mock.callCount.Load(),
		"no durable POST must be sent when the run capability is absent (no actor to attribute)")
}

// TestGuardrailAudit_NoBFFURLSkipsDurablePost asserts that when BFF_INTERNAL_URL is not
// configured, the durable POST is skipped silently (span event is the only record). The block
// refusal is unaffected.
func TestGuardrailAudit_NoBFFURLSkipsDurablePost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	// Build a proxy WITHOUT a bffInternalURL.
	gp, _ := newGuardedProxyWithBFF(t, upstream.URL, "") // no BFF URL
	rec := doBlockRequest(gp, fakeCap)
	assert.Equal(t, http.StatusForbidden, rec.Code, "block must still be 403 without a BFF URL")
	// No mock to assert on; the test simply verifies the block works and nothing panics.
}

// TestGuardrailAudit_FireGuardrailBlockAudit_DirectUnit directly tests fireGuardrailBlockAudit
// against a mock BFF ingest server. This is a unit test of the method, decoupled from
// the full gateway proxy (which requires a running upstream for the request path).
func TestGuardrailAudit_FireGuardrailBlockAudit_DirectUnit(t *testing.T) {
	mock := newMockIngestServer(t, http.StatusNoContent)

	gp := &gatewayProxy{
		cfg:            gatewayConfig{AgentName: "unit-agent"},
		bffInternalURL: mock.server.URL,
		logf:           func(string, ...any) {},
	}
	dec := blockDecision()

	// Build a synthetic request with a capability header.
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(runcap.HeaderName, fakeCap)

	// The block must already be sent (simulate by not writing to a ResponseWriter).
	gp.fireGuardrailBlockAudit(req, dec)

	// Wait for the goroutine to post.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && mock.callCount.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, int64(1), mock.callCount.Load(), "exactly one POST to the BFF ingest")
	require.Len(t, mock.events(), 1)
	evt := mock.events()[0]
	assert.Equal(t, "ssn", evt.Detector)
	assert.Equal(t, "input", evt.ScanPoint)
	assert.Equal(t, "deadbeefdeadbeefdeadbeefdeadbeef", evt.ContentHash)
	assert.Equal(t, "unit-agent", evt.Agent)
	assert.Equal(t, "block", evt.PolicyAction)
}

// TestGuardrailAudit_FireGuardrailBlockAudit_NoRawContent verifies the PII-safe invariant:
// the body POSTed to the BFF ingest never contains raw content — only a content_hash.
func TestGuardrailAudit_FireGuardrailBlockAudit_NoRawContent(t *testing.T) {
	// K8: the audit POST fires from a goroutine, so hand the captured body back over a channel — a
	// shared `receivedBody` var written by the handler + polled by the test body is a data race.
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case bodyCh <- b:
		default:
		}
	}))
	t.Cleanup(server.Close)

	gp := &gatewayProxy{
		cfg:            gatewayConfig{AgentName: "pii-agent"},
		bffInternalURL: server.URL,
		logf:           func(string, ...any) {},
	}
	dec := guardrailDecision{
		blocked:     true,
		detector:    "credit-card",
		action:      actionBlock,
		scanPoint:   scanInput,
		contentHash: "sha256ofcreditcardnumber",
		startOffset: 10,
		endOffset:   26,
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(runcap.HeaderName, fakeCap)

	gp.fireGuardrailBlockAudit(req, dec)

	var receivedBody []byte
	select {
	case receivedBody = <-bodyCh:
	case <-time.After(3 * time.Second):
	}

	require.NotEmpty(t, receivedBody, "body was received")
	bodyStr := string(receivedBody)

	// PII-safe: must never contain a raw credit card number, only the hash.
	assert.NotContains(t, bodyStr, "4111111111111111", "raw credit card number must never be in the audit body")
	assert.Contains(t, bodyStr, "sha256ofcreditcardnumber", "content_hash must be present")
	assert.Contains(t, bodyStr, `"policy_action":"block"`, "policy_action must be block")
	assert.Contains(t, bodyStr, `"detector":"credit-card"`)
	assert.NotContains(t, bodyStr, `"content":`, "raw 'content' field must never appear in the body")
}
