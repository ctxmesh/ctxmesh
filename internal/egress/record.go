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

package egress

// Record-mode TOOL capture at the egress sidecar (M78, ADR 0071 §1/C1). This is the SECOND
// capture seam (the launcher gateway is the model seam, m78.2). A RECORD-CAPABLE agent
// (RECORD_CAPABLE=true → newToolRecorder built in main) fronts EVERY tool through this sidecar
// (the controller's record-mode manifest rewrite), and for each tool call of a RECORDED run it
// captures the pre-injection request + verbatim upstream response into the run's portable replay
// fixture (internal/replay, TOOL channel).
//
// Two things are orthogonal, exactly as the model seam (gateway_record.go):
//
//   - ENABLEMENT is per-DEPLOYMENT: the controller injects the sidecar with RECORD_CAPABLE=true +
//     a fixture sink (OBJECT_STORE_ADDR) for a record-capable agent, and fronts all tools. No
//     store ⇒ a HARD startup error (C2 fail-closed) — never a silent no-capture.
//   - CAPTURE is per-RUN: the SDK relays X-Ctxmesh-Record: <runId> on each tool call of a recorded
//     run; a non-recorded run through the same record-capable sidecar carries no header ⇒ nothing
//     is captured. The run keying itself is off the cryptographically-VERIFIED runCap.RunID (not
//     the relayed value) so a forged/mismatched header cannot mis-key another run's fixture; the
//     header is a boolean-ish gate (present ⇒ capture this call).
//
// The fixture is a DURABLE object-store blob keyed fixtures/{runId}/{digest}.json — the SAME
// prefix the launcher gateway's model seam writes under. The two seams write SEPARATE partial
// blobs (gateway = Model channel, this sidecar = Tools channel) in their own processes; the
// assembler/replayer (m78.5) MERGES all blobs under a run's prefix. This recorder therefore only
// ever writes a Tools-channel fixture and NEVER read-modify-writes the gateway's model blob (two
// processes — a read-modify-write would race). It accumulates a run's Tools interactions in memory
// and re-Puts the per-run Tools fixture after each call: the Put is content-addressed + idempotent
// (store.go), so the last call's Put carries the complete tool channel.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ctxmesh/agent-engine/internal/objectstore"
	"github.com/ctxmesh/agent-engine/internal/replay"
)

// RecordHeaderName is the per-run capture toggle the SDK relays on each tool-call egress of a
// recorded run (X-Ctxmesh-Record). Its PRESENCE gates capture for this call; the run the fixture
// is keyed on comes from the VERIFIED run capability (runCap.RunID), not this header's value —
// so a forged header cannot mis-key or overwrite another run's fixture. MUST match recordHeaderName
// on the launcher-gateway side (cmd/launcher) and RECORD_HEADER in the SDKs. Case-insensitive on
// the wire (http.Header.Get canonicalizes).
const RecordHeaderName = "X-Ctxmesh-Record"

// recordPutTimeout bounds a single fixture Put to the object store so a slow/unreachable store
// never wedges the tool-call path (the Put is best-effort relative to the tool response, which has
// already been relayed to the agent by the time we Put).
const recordPutTimeout = 15 * time.Second

// toolRecorder accumulates the TOOL-channel interactions of each recorded run in memory and Puts a
// per-run fixture to the durable object store. Per-POD (one recorder serves every run this sidecar
// handles); ONE in-flight fixture per run id, so concurrent runs on a warm pod do not
// cross-contaminate. Safe for concurrent use.
type ToolRecorder struct {
	store *replay.FixtureStore
	agent string // "<namespace>/<name>" (or bare name) — fixture provenance only
	logf  func(string, ...any)

	mu   sync.Mutex
	runs map[string]*replay.Fixture // runId → the run's growing Tools-channel fixture
}

// NewToolRecorder builds the record-mode TOOL recorder for a record-capable sidecar from the
// durable object store configured via OBJECT_STORE_ADDR (objectstore.NewMinioStore). It FAILS
// CLOSED (ADR 0071 C2): a record-capable sidecar whose durable object store is not configured
// (OBJECT_STORE_ADDR unset ⇒ NewMinioStore returns a nil store) has NO sink for the fixture,
// which is a hard construction error — the caller turns it into a sidecar STARTUP error rather
// than fronting all tools while silently capturing nothing.
func NewToolRecorder(agentIdentity string, logf func(string, ...any)) (*ToolRecorder, error) {
	store, err := objectstore.NewMinioStore()
	if err != nil {
		return nil, fmt.Errorf("record mode: build object store: %w", err)
	}
	if store == nil {
		// OBJECT_STORE_ADDR unset — no durable fixture sink. Record was explicitly requested
		// (RECORD_CAPABLE=true), so this is a misconfiguration to surface loudly, never swallow.
		return nil, fmt.Errorf("record mode: OBJECT_STORE_ADDR is unset — a record-capable egress " +
			"sidecar needs a durable object store to write tool fixtures to")
	}
	return NewToolRecorderWithStore(store, agentIdentity, logf)
}

// NewToolRecorderWithStore builds the recorder over an explicit durable object store (the seam the
// unit tests inject a MemObjectStore through). store must be non-nil (the C2 fail-closed check
// lives in NewToolRecorder, above the env read); NewFixtureStore rejects a nil store defensively.
func NewToolRecorderWithStore(
	store objectstore.ObjectStore, agentIdentity string, logf func(string, ...any),
) (*ToolRecorder, error) {
	fs, err := replay.NewFixtureStore(store)
	if err != nil {
		return nil, fmt.Errorf("record mode: %w", err)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &ToolRecorder{
		store: fs,
		agent: agentIdentity,
		logf:  logf,
		runs:  map[string]*replay.Fixture{},
	}, nil
}

// capture appends one tool interaction to the run's growing Tools-channel fixture and Puts the
// fixture to the durable store. runID is the VERIFIED run capability's run id (the fixture key).
// callID + toolName are the replay matchers (best-effort parsed from the JSON-RPC request);
// argsBody is the agent-visible request BODY (the raw JSON-RPC tools/call message — it carries NO
// credential; the OBO bearer lives in the Authorization HEADER, which is injected DOWNSTREAM of
// this capture, so C4 holds by construction). responseBytes are the VERBATIM upstream response
// bytes, INCLUDING any SSE/streamable-http framing (do NOT parse-and-reassemble — replay re-serves
// them byte-identically, ADR 0071 §1/§3).
//
// The Put runs on every call (idempotent, content-addressed): the last call's Put carries the whole
// tool channel, so no separate run-terminal flush is needed. A store error is logged and swallowed
// — capture is best-effort relative to the live run (the tool response was already relayed); a
// recording failure must NEVER fail the agent's tool call. Before persisting we call
// Fixture.AssertNoCredentials (defense-in-depth, C4; FixtureStore.Put enforces it again).
func (rec *ToolRecorder) capture(ctx context.Context, runID, callID, toolName string, argsBody, responseBytes []byte, contentType string) {
	if rec == nil || strings.TrimSpace(runID) == "" {
		return
	}

	rec.mu.Lock()
	f, ok := rec.runs[runID]
	if !ok {
		f = replay.NewFixture(runID, rec.agent)
		rec.runs[runID] = f
	}
	f.AppendTool(callID, toolName, argsBody, responseBytes, contentType)
	// Snapshot the fixture pointer under the lock; the Put below reads the slice we just appended.
	toPut := f
	rec.mu.Unlock()

	if err := toPut.AssertNoCredentials(); err != nil {
		// A credential slipped into the fixture — refuse to persist it (an incident, the
		// non-negotiables). Log loudly; do not write. This should be impossible (we capture the
		// request BODY pre-injection, never the Authorization header), so it fires only on a bug.
		rec.logf("egress: record: REFUSING to store tool fixture for run %s (credential leak): %v", runID, err)
		return
	}

	putCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordPutTimeout)
	defer cancel()
	ref, err := rec.store.Put(putCtx, toPut)
	if err != nil {
		rec.logf("egress: record: tool fixture Put failed for run %s "+
			"(capture best-effort, run unaffected): %v", runID, err)
		return
	}
	rec.logf("egress: record: captured tool call for run %s (%d tool interactions) → %s",
		runID, len(toPut.Tools), ref)
}
