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

package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// The L7 supervisor checkpoint envelope (ADR 0091). A supervisor that suspends on a delegate_to call
// stores its managed-loop checkpoint in the Run.Cursor column (reused, opaque, executor-owned — the
// store never interprets the payload). The blob format is the real one-way-door, so it is a
// SELF-DESCRIBING, HASH-VERIFIED envelope from day one: resume rejects any envelope it cannot parse,
// whose kind/version it doesn't recognize, or whose hash fails — and falls back to a full re-invoke
// (fail-safe) rather than feeding the SDK a corrupt or version-skewed checkpoint. The hash detects
// CORRUPTION, not forgery (the blob round-trips through the agent that authored it); authorization
// truth (consent/approval) is NEVER trusted from the blob — it is re-derived from the server-side
// store on resume (ADR 0091 fork 3).

const (
	// CheckpointKindSupervisorLoop tags a supervisor managed-loop checkpoint (vs a workflow cursor,
	// which shares the column but is never enveloped this way — the kind disambiguates).
	CheckpointKindSupervisorLoop = "supervisor-loop"
	// checkpointEnvelopeVersion is the current envelope schema version. A resume rejects any other
	// version (fail-safe → full re-invoke), so a new version must roll the SDKs first (ADR 0091).
	checkpointEnvelopeVersion = 1
)

// CheckpointEnvelope wraps the SDK's opaque loop-state blob with a version, a kind, and a content hash.
type CheckpointEnvelope struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	SHA256  string `json:"sha256"`  // hex sha256 of Payload
	Payload string `json:"payload"` // the SDK's opaque managed-loop checkpoint (never interpreted by the store)
}

// NewSupervisorCheckpoint wraps an opaque SDK payload in a versioned, hashed envelope and returns its
// JSON (to store in Run.Cursor). An empty payload is valid (an envelope with an empty blob).
func NewSupervisorCheckpoint(payload string) (string, error) {
	sum := sha256.Sum256([]byte(payload))
	env := CheckpointEnvelope{
		Version: checkpointEnvelopeVersion,
		Kind:    CheckpointKindSupervisorLoop,
		SHA256:  hex.EncodeToString(sum[:]),
		Payload: payload,
	}
	b, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseSupervisorCheckpoint decodes AND VERIFIES a cursor envelope, returning the opaque payload only
// when the envelope parses, is the supervisor-loop kind, is a KNOWN version, and its hash matches.
// ok=false ⇒ the caller (resume) MUST fall back to a full re-invoke (fail-safe, ADR 0091 fork 3) — a
// corrupt / truncated / version-skewed checkpoint is never fed to the SDK. A workflow cursor (not a
// supervisor envelope) also returns ok=false, so the two column uses never cross.
func ParseSupervisorCheckpoint(cursor string) (payload string, ok bool) {
	if cursor == "" {
		return "", false
	}
	var env CheckpointEnvelope
	if err := json.Unmarshal([]byte(cursor), &env); err != nil {
		return "", false
	}
	if env.Kind != CheckpointKindSupervisorLoop || env.Version != checkpointEnvelopeVersion {
		return "", false
	}
	sum := sha256.Sum256([]byte(env.Payload))
	if hex.EncodeToString(sum[:]) != env.SHA256 {
		return "", false
	}
	return env.Payload, true
}
