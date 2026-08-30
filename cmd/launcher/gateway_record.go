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

// Record-mode model capture at the launcher gateway (M78, ADR 0071 §1). A RECORD-CAPABLE agent
// (RECORD_CAPABLE=true → newModelRecorder built in newGatewayProxy) captures each model
// request+response of a RECORDED run into a portable replay fixture (internal/replay). Two things
// are orthogonal on purpose:
//
//   - ENABLEMENT is per-DEPLOYMENT: the controller forces the gateway on for a record-capable agent
//     (a new interposition reason, ADR 0071 C2). If a run asks to be recorded but the agent is not
//     record-capable, the BFF fails that run CLOSED at create time (there is no gateway to capture
//     at) — never a silent no-capture.
//   - CAPTURE is per-RUN: the BFF stamps X-Ctxmesh-Record: <runId> on a recorded run's /invoke; the
//     SDK relays it on each model call (recordRunIDFromRequest). A non-recorded run through the same
//     record-capable agent carries no header ⇒ nothing is captured (zero fixture writes).
//
// The fixture is a DURABLE object-store blob keyed fixtures/{runId}/{digest}.json (m78.1
// FixtureStore over internal/objectstore — never the launcher's ephemeral A2A store). The gateway
// accumulates a run's MODEL-channel interactions in memory and re-Puts the per-run fixture after
// each model call: the Put is content-addressed + idempotent (store.go), so the LAST call's Put
// carries the complete model channel, and no cross-listener run-terminal signal is needed. The
// egress sidecar (m78.3) writes the TOOL-channel fixture under the SAME fixtures/{runId}/ prefix in
// its own process; the assembler/replayer (m78.5) MERGES all blobs under a run's prefix into the
// one logical fixture — that is the two-seam assembly contract (see the doc note on Put below).

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ctxmesh/agentry/internal/objectstore"
	"github.com/ctxmesh/agentry/internal/replay"
)

// recordHeaderName is the per-run capture toggle the BFF stamps on a recorded run's /invoke (when
// run.Record) and the SDK relays on each model call. Its VALUE is the run id the fixture is keyed
// on; an empty/absent header ⇒ this call belongs to a non-recorded run and is not captured. It is a
// launcher-internal signal (never forwarded upstream to LiteLLM — see budgetHeaderSet) exactly like
// the run capability.
const recordHeaderName = "X-Ctxmesh-Record"

// recordPutTimeout bounds a single fixture Put to the object store so a slow/unreachable store never
// wedges the model-call path (the Put is best-effort relative to the model response, which has
// already been relayed to the agent by the time we Put).
const recordPutTimeout = 15 * time.Second

// modelRecorder accumulates the MODEL-channel interactions of each recorded run in memory and Puts a
// per-run fixture to the durable object store. It is per-POD (one recorder serves every run this
// launcher handles) and keeps ONE in-flight fixture per run id, so concurrent runs on a warm pod do
// not cross-contaminate. Safe for concurrent use.
type modelRecorder struct {
	store *replay.FixtureStore
	agent string // "<namespace>/<name>" (or bare name) — fixture provenance only
	logf  func(string, ...any)

	mu   sync.Mutex
	runs map[string]*replay.Fixture // runId → the run's growing model-channel fixture
}

// newModelRecorder builds the record-mode recorder for a record-capable agent from the durable
// object store configured via OBJECT_STORE_ADDR (objectstore.NewMinioStore). It FAILS CLOSED (ADR
// 0071 C2): a record-capable agent whose durable object store is not configured (OBJECT_STORE_ADDR
// unset ⇒ NewMinioStore returns a nil store) has NO sink for the fixture, which is a hard
// construction error — the caller turns it into a launcher startup error rather than interposing a
// gateway that would silently capture nothing.
func newModelRecorder(agentName, namespace string, logf func(string, ...any)) (*modelRecorder, error) {
	store, err := objectstore.NewMinioStore()
	if err != nil {
		return nil, fmt.Errorf("record mode: build object store: %w", err)
	}
	if store == nil {
		// OBJECT_STORE_ADDR unset — no durable fixture sink. Record was explicitly requested
		// (RECORD_CAPABLE=true), so this is a misconfiguration to surface loudly, never swallow.
		return nil, fmt.Errorf("record mode: OBJECT_STORE_ADDR is unset — a record-capable agent " +
			"needs a durable object store to write fixtures to")
	}
	return newModelRecorderWithStore(store, agentName, namespace, logf)
}

// newModelRecorderWithStore builds the recorder over an explicit durable object store (the seam the
// unit tests inject a MemObjectStore through). store must be non-nil (the C2 fail-closed check lives
// in newModelRecorder, above the env read); NewFixtureStore rejects a nil store defensively.
func newModelRecorderWithStore(
	store objectstore.ObjectStore, agentName, namespace string, logf func(string, ...any),
) (*modelRecorder, error) {
	fs, err := replay.NewFixtureStore(store)
	if err != nil {
		return nil, fmt.Errorf("record mode: %w", err)
	}
	return &modelRecorder{
		store: fs,
		agent: agentIdentity(agentName, namespace),
		logf:  logf,
		runs:  map[string]*replay.Fixture{},
	}, nil
}

// agentIdentity builds the unambiguous "<namespace>/<name>" fixture provenance, falling back to the
// bare name when the namespace is unknown (a visible-but-non-fatal misconfig, never a crash).
func agentIdentity(name, namespace string) string {
	if strings.TrimSpace(namespace) == "" {
		return name
	}
	return namespace + "/" + name
}

// recordRunIDFromRequest returns the run id this model call is being recorded under, or "" when the
// call is NOT part of a recorded run. The BFF stamps X-Ctxmesh-Record: <runId> on a recorded run's
// /invoke (only when run.Record) and the SDK relays it on each model call; a non-recorded run
// carries no header. The value is the run id used to key the fixture object-store blob.
//
// v1 trust posture (ADR 0071, benign author-DX opt-in): the run id is taken from this
// launcher-internal header. Forging it cannot leak or overwrite another run's fixture — the store
// key is content-addressed (a forged blob has a distinct digest, so it never clobbers a legitimate
// fixture) and fixture READ is caller-scoped (ADR 0011). A cryptographically-trusted run id (from
// the verified run capability) is available only for OBO/guardrail agents; record mode does not
// require that surface, so it reads the relayed header — the same way spawn/conversation context
// already ride launcher-internal headers.
func recordRunIDFromRequest(r httpHeaderGetter) string {
	return strings.TrimSpace(r.Get(recordHeaderName))
}

// httpHeaderGetter is the tiny surface recordRunIDFromRequest needs (http.Header satisfies it),
// kept minimal so the toggle is unit-testable without a full *http.Request.
type httpHeaderGetter interface {
	Get(string) string
}

// capture appends one model interaction to the run's growing fixture and Puts the fixture to the
// durable store. requestBody is the agent-visible request BODY (JSON — it carries NO credential;
// the gateway credential lives in the Authorization HEADER, which is never captured, so C4 holds by
// construction). responseBytes are the VERBATIM bytes relayed to the agent, INCLUDING SSE framing
// (do NOT parse-and-reassemble — replay re-serves them byte-identically, ADR 0071 §1/§3).
//
// The Put runs on every call (idempotent, content-addressed): the last call's Put carries the whole
// model channel, so no separate run-terminal flush is needed. A store error is logged and swallowed
// — capture is best-effort relative to the live run (the model response was already relayed); a
// recording failure must never fail the agent's model call. Before persisting we call
// Fixture.AssertNoCredentials (defense-in-depth, C4; FixtureStore.Put enforces it again).
func (rec *modelRecorder) capture(
	ctx context.Context, runID string, requestBody, responseBytes []byte, contentType string, statusCode int,
) {
	if rec == nil || runID == "" {
		return
	}

	rec.mu.Lock()
	f, ok := rec.runs[runID]
	if !ok {
		f = replay.NewFixture(runID, rec.agent)
		rec.runs[runID] = f
	}
	f.AppendModel(requestBody, responseBytes, contentType, statusCode)
	// Snapshot the fixture pointer under the lock; the Put below reads the slice we just appended.
	// (AppendModel mutates f in place; the mutex serializes concurrent captures for the same run.)
	toPut := f
	rec.mu.Unlock()

	if err := toPut.AssertNoCredentials(); err != nil {
		// A credential slipped into the fixture — refuse to persist it (an incident, the
		// non-negotiables). Log loudly; do not write. This should be impossible (we capture the
		// request BODY, never the Authorization header), so it fires only on a capture-path bug.
		rec.logf("launcher: gateway: record: REFUSING to store fixture for run %s (credential leak): %v", runID, err)
		return
	}

	putCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordPutTimeout)
	defer cancel()
	ref, err := rec.store.Put(putCtx, toPut)
	if err != nil {
		rec.logf("launcher: gateway: record: fixture Put failed for run %s "+
			"(capture best-effort, run unaffected): %v", runID, err)
		return
	}
	rec.logf("launcher: gateway: record: captured model call for run %s (%d model interactions) → %s",
		runID, len(toPut.Model), ref)
}

// stripCredentialHeaders returns a copy of the request BODY suitable for the fixture. The request
// body is JSON (an OpenAI-shape chat/completions payload) and carries no credential — the gateway
// credential is the Authorization HEADER the SDK stamps, which we never capture. This helper exists
// as the explicit C4 seam: capture records the body only, so the Authorization/api-key headers can
// never enter the fixture. It defensively rejects an accidental header-line leak by returning the
// body unchanged (AssertNoCredentials is the enforcing gate).
func recordRequestBody(body []byte) []byte {
	// Return a copy so a later mutation of the caller's buffer cannot alter the captured bytes.
	if len(body) == 0 {
		return nil
	}
	return bytes.Clone(body)
}
