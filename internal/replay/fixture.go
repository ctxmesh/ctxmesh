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

// Package replay defines the PORTABLE record/replay fixture format (ADR 0071 §2) — the
// load-bearing one-way-door artifact of M78. The two capture seams (the launcher gateway for
// model I/O, the egress sidecar for tool I/O) are replaceable; the FIXTURE SCHEMA is what gets
// shared + CI-pinned, so it is spec'd first and versioned from day one (SchemaVersion).
//
// Shape: the VCR-cassette family — ordered interactions PER CHANNEL with request matchers, and
// deliberately NO global total order. Two processes write entries and parallel tool dispatch
// exists (the SDK batches tool results), so a global order across channels is meaningless within a
// batch. Instead:
//
//   - the MODEL channel is an ordered list matched at replay by request INDEX (+ a requestHash
//     divergence check — lenient on bytes, strict on shape, ADR 0071 §3);
//   - the TOOL channel is a list matched by tool-call ID, falling back to name+argsHash.
//
// Sensitivity (ADR 0071 C4): a fixture carries the FULL agent-visible bytes — prompts, tool
// arguments, tool results — so it is SENSITIVE-BY-DEFAULT and NOT-FOR-GIT. It must carry NO
// credential bytes: capture happens PRE-injection (before the launcher's gateway credential and
// before the egress sidecar's OBO bearer are added), and AssertNoCredentials enforces the
// no-token invariant on the capture path (a fixture with an Authorization header fails validation).
// Read access is caller-scoped at the object-store seam (store.go, ADR 0011).
package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SchemaVersion is the CURRENT fixture schema major version. It is written into every fixture
// (Fixture.SchemaVersion) and checked on load: a fixture whose recorded version is NEWER than this
// binary understands is rejected with a clear error (UnmarshalFixture), never silently
// mis-replayed. Bump this ONLY on a breaking change to the on-disk shape; additive, backward-
// compatible fields do not require a bump. Versioned from day one because the fixture is a
// one-way-door artifact shared across the record→replay→CI loop (ADR 0071 §2).
const SchemaVersion = 1

// Fixture is a portable, versioned record of one run's model + tool I/O, assembled from both
// capture seams into a single object-store blob (store.go). It is JSON-serializable and
// self-describing (SchemaVersion + provenance) so a shared fixture is independently replayable.
//
// The two channels are SEPARATE ordered lists (no interleaved global order): Model responses are
// matched by request index, Tools by call id / name+args-hash. RecordedAt + RunID + Agent are
// provenance only (never load-bearing for matching) — a divergence on prompt timestamps is
// expected and tolerated (ADR 0071 §3 divergence policy).
type Fixture struct {
	// SchemaVersion is the on-disk format version (see SchemaVersion). Always set by NewFixture and
	// verified by UnmarshalFixture.
	SchemaVersion int `json:"schemaVersion"`
	// RunID is the run this fixture was recorded from (provenance / correlation, not a matcher).
	RunID string `json:"runId"`
	// Agent is the "<namespace>/<name>" (or bare name) the run targeted (provenance).
	Agent string `json:"agent"`
	// RecordedAt is when recording completed (provenance; wall-clock, never a matcher).
	RecordedAt time.Time `json:"recordedAt"`
	// Model is the ordered list of model-channel interactions, in the order the launcher gateway
	// observed them. Matched at replay by Index (+ RequestHash divergence check).
	Model []ModelInteraction `json:"model"`
	// Tools is the tool-channel interactions (order is captured but NOT load-bearing — parallel
	// dispatch means several tool calls in one batch have no meaningful relative order). Matched at
	// replay by CallID, falling back to ToolName+ArgsHash.
	Tools []ToolInteraction `json:"tools"`
}

// ModelInteraction is one model request/response pair captured at the launcher gateway (ADR 0071
// §1). ResponseBytes are the RAW upstream response bytes INCLUDING SSE framing — replay re-serves
// them verbatim (do NOT parse-and-reassemble a streamed response, ADR 0071 §1/§3), so the agent
// sees byte-identical output. The gateway credential is stripped before capture (C4).
type ModelInteraction struct {
	// Index is the 0-based position of this request in the model channel — the PRIMARY replay
	// matcher (the Nth model call gets the Nth recorded response).
	Index int `json:"index"`
	// RequestHash is a stable digest of the canonicalized request (HashModelRequest) — the
	// divergence check: a content mismatch on the Nth request serves the recorded response + logs a
	// diff (lenient on bytes); a STRUCTURAL mismatch is the replayer's hard-fail (ADR 0071 §3).
	RequestHash string `json:"requestHash"`
	// Request is the raw agent-visible request body (credential-stripped), retained for the
	// divergence report + the (deferred) fixture stepper's I/O inspector. Opaque here.
	Request json.RawMessage `json:"request,omitempty"`
	// ResponseBytes is the VERBATIM upstream response, INCLUDING SSE framing, re-served on replay.
	ResponseBytes []byte `json:"responseBytes"`
	// ContentType is the upstream response Content-Type (e.g. text/event-stream for a streamed
	// completion) so replay serves the recorded response with the right framing header.
	ContentType string `json:"contentType,omitempty"`
	// StatusCode is the upstream HTTP status (0 ⇒ treat as 200 on replay).
	StatusCode int `json:"statusCode,omitempty"`
}

// ToolInteraction is one tool (MCP) request/response pair captured at the egress sidecar (ADR 0071
// §1), recorded BEFORE OBO-bearer injection (C4 — the fixture holds only agent-visible bytes).
// Matched at replay by CallID (the tool-call id the model assigned) and, as a fallback for a
// captured-without-id call, by ToolName + ArgsHash.
type ToolInteraction struct {
	// CallID is the model-assigned tool-call id — the PRIMARY tool matcher. May be empty for a call
	// the seam could not correlate to an id, in which case ToolName+ArgsHash is the matcher.
	CallID string `json:"callId,omitempty"`
	// ToolName is the invoked tool's name (the secondary matcher's first key).
	ToolName string `json:"toolName"`
	// ArgsHash is a stable digest of the canonicalized call arguments (HashToolArgs) — the secondary
	// matcher's second key, and a divergence check when CallID matches.
	ArgsHash string `json:"argsHash"`
	// Request is the raw agent-visible tool request (credential-stripped), retained for the
	// divergence report + inspector. Opaque here.
	Request json.RawMessage `json:"request,omitempty"`
	// ResponseBytes is the verbatim tool result bytes re-served on replay.
	ResponseBytes []byte `json:"responseBytes"`
	// ContentType is the tool response's Content-Type (e.g. text/event-stream for a streamed MCP
	// result), captured at the egress sidecar (O9) so replay serves the recorded framing instead of
	// SNIFFING it from the bytes. omitempty + a sniff fallback on replay keep OLD fixtures (schema v1,
	// no tool content-type) working unchanged — a backward-compatible additive evolution.
	ContentType string `json:"contentType,omitempty"`
}

// NewFixture builds an empty, current-schema fixture for a run. RecordedAt is stamped by the caller
// via the returned struct (or left to Put time); NewFixture stamps it to now so a bare fixture is
// still self-describing.
func NewFixture(runID, agent string) *Fixture {
	return &Fixture{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		Agent:         agent,
		RecordedAt:    time.Now().UTC(),
		Model:         []ModelInteraction{},
		Tools:         []ToolInteraction{},
	}
}

// AppendModel appends a model interaction, assigning its channel Index automatically (the next
// position in the model channel) so callers cannot mis-number the primary matcher. requestBody is
// the agent-visible request (credential-stripped by the caller); responseBytes are the verbatim
// upstream bytes incl. SSE framing. Returns the assigned index.
func (f *Fixture) AppendModel(requestBody, responseBytes []byte, contentType string, statusCode int) int {
	idx := len(f.Model)
	f.Model = append(f.Model, ModelInteraction{
		Index:         idx,
		RequestHash:   HashModelRequest(requestBody),
		Request:       rawOrNil(requestBody),
		ResponseBytes: responseBytes,
		ContentType:   contentType,
		StatusCode:    statusCode,
	})
	return idx
}

// AppendTool appends a tool interaction. callID is the model-assigned tool-call id (may be empty);
// toolName + argsBody drive the fallback matcher; responseBytes are the verbatim tool result. The
// args hash is computed from argsBody so the fallback matcher is stable across whitespace/key-order.
func (f *Fixture) AppendTool(callID, toolName string, argsBody, responseBytes []byte, contentType string) {
	f.Tools = append(f.Tools, ToolInteraction{
		CallID:        callID,
		ToolName:      toolName,
		ArgsHash:      HashToolArgs(argsBody),
		Request:       rawOrNil(argsBody),
		ResponseBytes: responseBytes,
		ContentType:   contentType,
	})
}

// rawOrNil returns a copy of b as json.RawMessage, or nil for empty input (so an absent body
// serializes as an omitted field rather than an empty-but-present one).
func rawOrNil(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(append([]byte(nil), b...))
}

// HashModelRequest returns the stable divergence digest for a model request body. It canonicalizes
// JSON (sorted keys, no insignificant whitespace) when the body parses as JSON so semantically equal
// requests hash equally regardless of key order/formatting; a non-JSON body is hashed verbatim. The
// hash is the DIVERGENCE check, never the primary matcher (that is the request index).
func HashModelRequest(body []byte) string {
	return hashCanonical(body)
}

// HashToolArgs returns the stable digest of a tool call's arguments — the fallback matcher's second
// key. Same canonicalization as HashModelRequest.
func HashToolArgs(args []byte) string {
	return hashCanonical(args)
}

// hashCanonical hashes the canonical form of b: if b is valid JSON, it is re-marshaled with sorted
// keys (Go's encoding/json sorts map keys) so key order + whitespace do not change the hash; if not,
// b is hashed as-is. Returns a hex sha256.
func hashCanonical(b []byte) string {
	canon := b
	var v any
	if json.Unmarshal(b, &v) == nil {
		if c, err := json.Marshal(v); err == nil {
			canon = c
		}
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// MarshalJSON serializes the fixture, pinning SchemaVersion to the current constant so a fixture is
// never written with a stale/zero version even if a caller zeroed the field. It uses an alias type
// to avoid infinite recursion.
func (f Fixture) MarshalJSON() ([]byte, error) {
	type alias Fixture
	out := alias(f)
	out.SchemaVersion = SchemaVersion
	if out.Model == nil {
		out.Model = []ModelInteraction{}
	}
	if out.Tools == nil {
		out.Tools = []ToolInteraction{}
	}
	return json.Marshal(out)
}

// UnmarshalFixture decodes fixture bytes, enforcing the schema-version contract: a fixture whose
// recorded major version is GREATER than this binary's SchemaVersion is rejected (an older binary
// must not silently mis-replay a newer format), and a zero/absent version is rejected as malformed
// (every fixture NewFixture writes carries a version). This is the load-side half of "version the
// schema from day one".
func UnmarshalFixture(data []byte) (*Fixture, error) {
	var f Fixture
	if err := json.Unmarshal(data, (*fixtureNoMethods)(&f)); err != nil {
		return nil, fmt.Errorf("replay: decode fixture: %w", err)
	}
	if f.SchemaVersion == 0 {
		return nil, fmt.Errorf("replay: fixture has no schemaVersion (malformed or pre-versioning)")
	}
	if f.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("replay: fixture schemaVersion %d is newer than supported version %d — upgrade ctxmesh to replay it", f.SchemaVersion, SchemaVersion)
	}
	return &f, nil
}

// fixtureNoMethods is Fixture without its MarshalJSON/UnmarshalJSON methods, so UnmarshalFixture can
// use encoding/json's default struct decoding without recursing through a custom UnmarshalJSON.
type fixtureNoMethods Fixture

// credentialHeaderNames are the request-header names whose PRESENCE in a fixture means a credential
// leaked past the pre-injection capture point (a fixture-format bug or a wrong capture seam). The
// no-token invariant (C4) is that agent-VISIBLE bytes never include these; capture strips them.
var credentialHeaderNames = []string{
	"authorization",
	"proxy-authorization",
	"cookie",
	"set-cookie",
	"x-api-key",
	"x-goog-api-key",
	"api-key",
	"x-amz-security-token",
	"openai-api-key",
	"anthropic-api-key",
}

// AssertNoCredentials enforces the C4 no-token invariant: the fixture must carry NO credential
// bytes. The capture path (m78.2/m78.3) MUST call this before a fixture is persisted — capture
// happens pre-injection (before the gateway credential / OBO bearer are added) and this is the
// belt-and-braces that a strip was not missed. It scans every interaction's request + response bytes
// for a credential HTTP header line (e.g. "Authorization: ..."). Returns an error naming the first
// offending header + location so the leak is diagnosable, never swallowed.
//
// It matches a header at a line boundary ("\nAuthorization:" or a body-leading "Authorization:") so
// the word merely APPEARING inside a prompt ("explain the Authorization header") does not false-
// positive — only an actual header line trips it.
func (f *Fixture) AssertNoCredentials() error {
	for i := range f.Model {
		if h, ok := scanForCredentialHeader(f.Model[i].Request); ok {
			return fmt.Errorf("replay: fixture carries credential header %q in model[%d].request — capture must strip credentials pre-injection (C4)", h, i)
		}
		if h, ok := scanForCredentialHeader(f.Model[i].ResponseBytes); ok {
			return fmt.Errorf("replay: fixture carries credential header %q in model[%d].responseBytes (C4)", h, i)
		}
	}
	for i := range f.Tools {
		if h, ok := scanForCredentialHeader(f.Tools[i].Request); ok {
			return fmt.Errorf("replay: fixture carries credential header %q in tools[%d].request — capture must record pre-injection, before the OBO bearer (C4)", h, i)
		}
		if h, ok := scanForCredentialHeader(f.Tools[i].ResponseBytes); ok {
			return fmt.Errorf("replay: fixture carries credential header %q in tools[%d].responseBytes (C4)", h, i)
		}
	}
	return nil
}

// scanForCredentialHeader reports the first credential header name present as a header LINE in b (a
// "<name>:" at the start of a line or of the buffer), and whether one was found. Case-insensitive on
// the header name. It is deliberately conservative: it looks for the "name:" token at a line
// boundary, so a credential word appearing in prose/JSON-value position is not flagged.
func scanForCredentialHeader(b []byte) (string, bool) {
	if len(b) == 0 {
		return "", false
	}
	// Canonicalize once for cheap case-insensitive line scanning. Normalize CRLF to LF.
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	lower := strings.ToLower(text)
	for _, name := range credentialHeaderNames {
		token := name + ":"
		if strings.HasPrefix(lower, token) {
			return http.CanonicalHeaderKey(name), true
		}
		if strings.Contains(lower, "\n"+token) {
			return http.CanonicalHeaderKey(name), true
		}
	}
	return "", false
}

// -----------------------------------------------------------------------------
// Per-channel matchers (ADR 0071 §2/§3) — used by the m78.5 replayer. Defined
// here with the schema so the matching contract lives WITH the format it matches.
// -----------------------------------------------------------------------------

// MatchModel returns the recorded model response for the index-th model request and whether the
// request DIVERGED from what was recorded (a byte/content mismatch on RequestHash). The matcher is
// the INDEX (the Nth model call → the Nth recorded response); RequestHash divergence is advisory
// per ADR 0071 §3 (lenient on bytes: serve the recorded response + let the caller log a diff). A
// missing index (more model calls than recorded) returns ok=false — the caller's HARD-FAIL
// structural divergence.
func (f *Fixture) MatchModel(index int, requestBody []byte) (mi ModelInteraction, diverged bool, ok bool) {
	if index < 0 || index >= len(f.Model) {
		return ModelInteraction{}, false, false
	}
	m := f.Model[index]
	diverged = m.RequestHash != "" && m.RequestHash != HashModelRequest(requestBody)
	return m, diverged, true
}

// MatchTool returns the recorded tool result for a tool call, matched PER ADR 0071 §2: first by
// CallID (exact), else by ToolName + ArgsHash. Each recorded interaction is matched AT MOST ONCE
// (used tracks consumed indices) so two identical tool calls in one run consume two distinct
// recordings in order rather than both matching the first. An unmatched call returns ok=false — the
// caller's hard-fail (an unrecorded tool call is a structural divergence, ADR 0071 §3).
//
// used is caller-owned matcher state (a set of already-consumed indices into f.Tools); MatchTool
// records the chosen index into it. Passing a fresh map replays independently.
func (f *Fixture) MatchTool(callID, toolName string, argsBody []byte, used map[int]bool) (ti ToolInteraction, ok bool) {
	if used == nil {
		used = map[int]bool{}
	}
	// Primary: exact tool-call id.
	if callID != "" {
		for i := range f.Tools {
			if used[i] {
				continue
			}
			if f.Tools[i].CallID == callID {
				used[i] = true
				return f.Tools[i], true
			}
		}
	}
	// Fallback: name + args-hash (a call captured without a correlatable id).
	argsHash := HashToolArgs(argsBody)
	for i := range f.Tools {
		if used[i] {
			continue
		}
		if f.Tools[i].ToolName == toolName && f.Tools[i].ArgsHash == argsHash {
			used[i] = true
			return f.Tools[i], true
		}
	}
	return ToolInteraction{}, false
}
