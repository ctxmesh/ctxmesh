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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ctxmesh/agentry/internal/run"
)

// The platform-side structured-output re-ask (m143.6, m52.J4, ADR 0058's deferred recovery tier).
//
// M65 made the terminal-output check platform-authoritative and FAIL-CLOSED: a non-conforming answer
// is an honest `failed`, never a swallowed success. That is the right verdict, but it left the
// recovery tier unevenly distributed. An SDK agent repairs IN-LOOP — it sees the validation error and
// tries again before it ever answers — so it rarely reaches the platform check at all. A non-SDK or
// custom-loop agent, which the platform explicitly supports (any HTTP server that speaks the invoke
// envelope), has no repair tier whatsoever: one near-miss, one dead run.
//
// So the platform gives every agent ONE bounded re-ask before it fails the run. It is deliberately
// the crudest possible mechanism, because it has to work for an agent that implements NOTHING: the
// repair instruction is appended to the `input` STRING, the one field every agent already reads
// (sdk/python serve.py `_parse_body`), rather than a structured field only the SDK would honour.
//
// Cost is bounded at exactly one extra invoke, on a run that was otherwise about to fail — never on
// a healthy run, never a loop.

// reaskInputSuffix formats the repair turn appended to the agent's original input. It states the
// contract, shows what the agent actually returned, and names the violation, because "try again"
// without the diagnosis mostly reproduces the same near-miss.
const reaskInputSuffix = `

---
SYSTEM: Your previous answer was REJECTED because it does not conform to the output schema this
agent is required to satisfy. Return ONLY a JSON value that validates against the schema — no prose,
no markdown fences, no commentary before or after it.

Required schema:
%s

Your previous answer:
%s

Why it was rejected:
%s`

// maxReaskEcho bounds how much of the rejected answer and the schema are echoed back. A pathological
// answer must not turn one re-ask into a context-blowing prompt.
const maxReaskEcho = 4000

// reaskInvokeBody rewrites an invoke body so the agent is asked once more, with the schema, its own
// rejected answer, and the validation error appended to the input it already understands.
//
// It handles both body shapes the platform produces: the normal JSON object carrying "input", and a
// bare JSON string (a run whose Input is just the prompt). Anything it cannot parse safely returns
// ok=false — a re-ask it cannot build correctly is skipped, not guessed at.
func reaskInvokeBody(input []byte, schema, badOutput string, verr error) ([]byte, bool) {
	suffix := fmt.Sprintf(reaskInputSuffix,
		truncateForReask(schema), truncateForReask(badOutput), truncateForReask(verr.Error()))

	var body map[string]json.RawMessage
	if err := json.Unmarshal(input, &body); err != nil || body == nil {
		// Not a JSON object. If it is a valid JSON string it IS the input verbatim; wrap it.
		var bare string
		if err := json.Unmarshal(input, &bare); err != nil {
			return nil, false
		}
		return marshalReask(map[string]json.RawMessage{}, bare+suffix)
	}

	var prompt string
	if raw, ok := body[invokeInputField]; ok {
		if err := json.Unmarshal(raw, &prompt); err != nil {
			// A non-string "input" is a shape this re-ask was not written for; don't mangle it.
			return nil, false
		}
	}
	// A checkpoint replays the loop that produced the rejected answer, so carrying it into a repair
	// turn would re-derive the same output. Drop it: the re-ask is a fresh, self-contained ask.
	delete(body, "checkpoint")
	return marshalReask(body, prompt+suffix)
}

func marshalReask(body map[string]json.RawMessage, prompt string) ([]byte, bool) {
	encoded, err := json.Marshal(prompt)
	if err != nil {
		return nil, false
	}
	body[invokeInputField] = encoded
	out, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	return out, true
}

func truncateForReask(s string) string {
	if len(s) <= maxReaskEcho {
		return s
	}
	return s[:maxReaskEcho] + "… (truncated)"
}

// reaskForOutputSchema performs the single platform re-ask and returns the repaired output.
//
// It returns ok=false — leaving the caller's original fail-closed verdict intact — whenever a re-ask
// is inappropriate or does not help:
//   - the schema itself does not compile (the agent cannot satisfy an unexpressable contract);
//   - execution is no longer ours to drive (ctx cancelled, or this worker self-fenced per m143.3) —
//     re-invoking there would be a DUPLICATE execution racing the peer that reclaimed the run;
//   - the body cannot be rewritten safely, the re-invoke errors, or the second answer is also
//     non-conforming.
//
// Only a genuinely conforming second answer returns ok=true.
func (s *Server) reaskForOutputSchema(
	ctx context.Context,
	runID, endpoint string,
	input []byte,
	schema, badOutput string,
	verr error,
) (string, bool) {
	if errors.Is(verr, errSchemaUncompilable) {
		return "", false
	}
	if ctx.Err() != nil || selfFenced(ctx) {
		return "", false
	}
	body, ok := reaskInvokeBody(input, schema, badOutput, verr)
	if !ok {
		return "", false
	}

	s.log.Info("run: terminal output rejected — re-asking the agent once (m143.6)",
		"run", runID, "reason", verr.Error())
	// Make the repair visible on the run's own stream: a user watching a run that suddenly takes a
	// second round-trip should see WHY, not an unexplained pause.
	// A `label` makes it render as human text in the console's step indicator; without one, a step
	// frame the UI does not recognise falls back to showing the RAW JSON to the user.
	_ = s.runStore.AppendEvent(runID, run.EventStep,
		`{"kind":"output_schema_reask","label":"Re-asking — the answer didn't match the required format"}`)

	resp, _, err := s.adapters.Invoke.Invoke(ctx, endpoint, body)
	if err != nil {
		s.log.Info("run: the structured-output re-ask failed to invoke", "run", runID, "err", err.Error())
		return "", false
	}
	// A re-ask that comes back asking for consent/approval/delegation is NOT a repaired answer — the
	// agent changed course. Refuse to interpret those envelopes here; the original verdict stands.
	if len(parseConsentRequired(resp)) > 0 || parseApprovalRequired(resp) != nil ||
		parseDelegateWaiting(resp) != nil || handoffMarkerPresent(resp) {
		s.log.Info("run: the re-ask returned a non-terminal envelope; keeping the original verdict",
			"run", runID)
		return "", false
	}

	repaired := extractRunOutput(resp)
	if rerr := validateTerminalOutput(schema, strings.TrimSpace(repaired)); rerr != nil {
		s.log.Info("run: the re-asked answer still does not conform — failing honestly",
			"run", runID, "reason", rerr.Error())
		return "", false
	}
	s.log.Info("run: the re-asked answer conforms; the run succeeds", "run", runID)
	return strings.TrimSpace(repaired), true
}
