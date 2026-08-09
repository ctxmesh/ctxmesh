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

// guardrail_audit.go — fire-and-forget durable compliance record for guardrail BLOCKs (m66.9).
//
// A guardrail block is a compliance event. The launcher posts it to the BFF's
// POST /api/internal/guardrail-event endpoint best-effort, async, so the refusal
// is NEVER delayed or failed by the audit write:
//
//   - The block is returned to the SDK caller first (writeGuardrailBlocked is called BEFORE
//     fireGuardrailBlockAudit).
//   - The audit POST fires in a short-lived background goroutine with an independent timeout.
//   - If the ingest server is down, slow, or the capability is absent, the audit POST is
//     skipped or its failure is logged — but the 403 block response is NEVER affected.
//   - Only "block" decisions produce a durable POST; "auditOnly" and "redact" remain span-only.
//
// PII-safe invariant (ADR 0059 §6): the body carries only a sha256 content_hash, never the
// raw matched substring or any scanned text. This mirrors the span event shape.
//
// Capability requirement (m66.9): the durable audit POST is skipped when no run capability
// is present on the model call (an unattended/offline run, or the capability did not propagate
// from m66.7). A log line is emitted so the gap is visible. The span event is always emitted.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/runcap"
)

const (
	// guardrailAuditIngestPath is the BFF endpoint for durable guardrail block records.
	guardrailAuditIngestPath = "/api/internal/guardrail-event"

	// guardrailAuditTimeout bounds the background goroutine's BFF round-trip. Short by design:
	// the block refusal must never be delayed. The goroutine is fire-and-forget; the timeout
	// just prevents a leaked goroutine when the BFF is unreachable.
	guardrailAuditTimeout = 3 * time.Second
)

// guardrailAuditEvent is the PII-safe payload the launcher sends to the BFF ingest.
// Struct field names must match the BFF's guardrailEventRequest (internal/bff/guardrail_event_handler.go).
// INVARIANT: no raw content is ever included — only a sha256 content_hash.
type guardrailAuditEvent struct {
	Detector     string `json:"detector"`
	ScanPoint    string `json:"scan_point"`
	ContentHash  string `json:"content_hash"`
	Agent        string `json:"agent"`
	PolicyAction string `json:"policy_action"`
}

// fireGuardrailBlockAudit fires the durable compliance record for a guardrail BLOCK as a
// best-effort, fire-and-forget background goroutine. The block refusal MUST already have been
// written to the client before this is called — the audit write NEVER delays or affects the 403.
//
// Preconditions (both checked; no-op when not satisfied):
//   - gp.bffInternalURL must be set (BFF_INTERNAL_URL configured).
//   - The request must carry a valid X-Ctxmesh-Run-Capability header (m66.7 SDK propagation).
//     Without it the actor cannot be attributed and the POST is skipped (logged). The span event
//     (emitted by emitGuardrailDecision) remains the only record in that case.
//
// The goroutine is bounded by guardrailAuditTimeout; it never holds the calling goroutine.
func (gp *gatewayProxy) fireGuardrailBlockAudit(r *http.Request, dec guardrailDecision) {
	if gp.bffInternalURL == "" {
		// BFF not configured — durable POST is skipped; span event is the only record.
		return
	}
	// Extract the run capability token from the model request header. Only the token the SDK
	// relays on the model call is trusted; do NOT attempt to forge a token here.
	capToken := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if capToken == "" {
		gp.logf("launcher: guardrail audit: no run capability on model request — durable block record skipped " +
			"(span event emitted; capability propagation requires m66.7 SDK wiring)")
		return
	}

	evt := guardrailAuditEvent{
		Detector:     dec.detector,
		ScanPoint:    string(dec.scanPoint),
		ContentHash:  dec.contentHash,
		Agent:        gp.cfg.AgentName,
		PolicyAction: "block",
	}
	url := gp.bffInternalURL + guardrailAuditIngestPath

	// Fire-and-forget: launch the HTTP POST in a goroutine. The goroutine owns its context
	// (independent of r.Context() which closes when the request is done). The block response
	// has already been written above, so this goroutine's lifetime is decoupled from the caller.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), guardrailAuditTimeout)
		defer cancel()

		raw, err := json.Marshal(evt)
		if err != nil {
			gp.logf("launcher: guardrail audit: marshal: %v (block already sent)", err)
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			gp.logf("launcher: guardrail audit: build request: %v (block already sent)", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(runcap.HeaderName, capToken)

		// Reuse the gateway's HTTP client (same timeout pool), but with the goroutine's ctx.
		hc := &http.Client{Timeout: guardrailAuditTimeout}
		resp, err := hc.Do(req)
		if err != nil {
			gp.logf("launcher: guardrail audit: POST %s: %v (block already sent; durable record skipped)", url, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			gp.logf("launcher: guardrail audit: POST %s: status %d (block already sent; durable record not persisted)",
				url, resp.StatusCode)
			return
		}
		gp.logf("launcher: guardrail audit: durable block record persisted (detector=%s scan_point=%s content_hash=%s)",
			evt.Detector, evt.ScanPoint, evt.ContentHash)
	}()
}
