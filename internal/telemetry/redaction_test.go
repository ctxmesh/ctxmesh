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

package telemetry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
)

func TestPlatformAttributeKeys(t *testing.T) {
	assert.Equal(t, "agent.name", AttrAgentName)
	assert.Equal(t, "agent.version", AttrAgentVersion)
	assert.Equal(t, "agent.route", AttrAgentRoute)
}

func TestIsSensitive(t *testing.T) {
	assert.True(t, IsSensitive(AttrLLMInputMessages), "llm input is payload")
	assert.True(t, IsSensitive(AttrToolCallArguments), "tool args are payload")
	assert.False(t, IsSensitive(AttrAgentName), "agent.name is metadata, not payload")
	assert.False(t, IsSensitive("random.key"), "unknown key not sensitive")
}

// TestRedactScrubsPIIFromSensitiveAttrs is the core M11 unit test (replacing the
// M3 passthrough assertion): the built-in detectors redact emails / SSNs / keys
// in the sensitive payload attributes, replacing each with a deterministic
// marker while leaving surrounding non-PII text intact.
func TestRedactScrubsPIIFromSensitiveAttrs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "email",
			in:   "reach me at john.doe+test@example.co.uk please",
			want: "reach me at [REDACTED:email] please",
		},
		{
			name: "ssn",
			in:   "my ssn is 123-45-6789 ok",
			want: "my ssn is [REDACTED:ssn] ok",
		},
		{
			name: "openai key",
			in:   "token sk-abcdefghij0123456789ABCDEFGH tail",
			want: "token [REDACTED:key] tail",
		},
		{
			name: "aws access key id",
			in:   "creds AKIAIOSFODNN7EXAMPLE end",
			want: "creds [REDACTED:key] end",
		},
		{
			name: "long high-entropy token",
			in:   "bearer aGVsbG8td29ybGQtdG9rZW4tMTIzNDU2Nzg5MA done",
			want: "bearer [REDACTED:key] done",
		},
		{
			name: "multiple pii in one value",
			in:   "email a@b.com ssn 111-22-3333 key sk-abcdefghij0123456789ZZZZ",
			want: "email [REDACTED:email] ssn [REDACTED:ssn] key [REDACTED:key]",
		},
		{
			name: "clean text untouched",
			in:   "the weather is nice today and nothing here is secret",
			want: "the weather is nice today and nothing here is secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []attribute.KeyValue{attribute.String(AttrLLMInputMessages, tc.in)}
			out := Redact(in)
			require.Len(t, out, 1)
			assert.Equal(t, tc.want, out[0].Value.AsString())
		})
	}
}

// TestRedactLeavesNonSensitiveAttrsAndStructureIntact proves redaction acts ONLY
// on the sensitive payload keys — metadata attributes (agent.name etc.) carrying
// PII-shaped text are left untouched, and the attribute slice order/shape is
// preserved. This is the "don't over-redact / keep the tree" guarantee.
func TestRedactLeavesNonSensitiveAttrsAndStructureIntact(t *testing.T) {
	in := []attribute.KeyValue{
		attribute.String(AttrAgentName, "agent-with-123-45-6789-in-name"),
		attribute.String(AttrLLMInputMessages, "ssn 123-45-6789"),
		attribute.Int("http.status_code", 200),
		attribute.String(AttrOutputValue, "email x@y.io"),
	}
	out := Redact(in)
	require.Len(t, out, len(in), "slice length preserved")

	// Non-sensitive metadata carrying an SSN-shaped substring is NOT redacted.
	assert.Equal(t, "agent-with-123-45-6789-in-name", out[0].Value.AsString(),
		"agent.name (metadata) must be untouched even with PII-shaped text")
	// Sensitive attrs ARE redacted.
	assert.Equal(t, "ssn [REDACTED:ssn]", out[1].Value.AsString())
	// Non-string attr untouched.
	assert.Equal(t, int64(200), out[2].Value.AsInt64())
	assert.Equal(t, "email [REDACTED:email]", out[3].Value.AsString())

	// Determinism: re-running yields the identical result.
	assert.Equal(t, out, Redact(in), "redaction is deterministic")
}

func TestRedactWithCustomDetectors(t *testing.T) {
	custom := []CustomDetectorSpec{{Name: "badge", Pattern: `EMP-\d{5}`}}
	detectors, err := DetectorsWithCustom(custom)
	require.NoError(t, err)

	in := []attribute.KeyValue{
		attribute.String(AttrLLMInputMessages, "hi EMP-01234 and a@b.com"),
	}
	out := RedactWith(in, detectors)
	assert.Equal(t, "hi [REDACTED:badge] and [REDACTED:email]", out[0].Value.AsString(),
		"custom detector applied alongside the always-on defaults")
}

func TestDetectorsWithCustomRejectsInvalidPattern(t *testing.T) {
	// Built-ins are still returned; the bad custom pattern surfaces an error so
	// the reconciler refuses the policy rather than silently shipping a weaker one.
	detectors, err := DetectorsWithCustom([]CustomDetectorSpec{{Name: "bad", Pattern: "("}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad")
	assert.Len(t, detectors, len(DefaultDetectors()), "built-in defaults still returned on a bad custom pattern")
}

func TestDefaultDetectorsCopyIsIsolated(t *testing.T) {
	d1 := DefaultDetectors()
	require.NotEmpty(t, d1)
	d1[0] = Detector{Name: "mutated"}
	assert.NotEqual(t, "mutated", DefaultDetectors()[0].Name, "DefaultDetectors returns an isolated copy")
}

func TestBasicAuthHeader(t *testing.T) {
	// pk:sk → base64("pk:sk") == "cGs6c2s="
	got := BasicAuthHeader("pk", "sk")
	assert.Equal(t, "Basic cGs6c2s=", got)
}

// TestRenderConfigWiresRedactionProcessor is the unit-level proof that the
// collector config the reconciler renders WIRES redaction into the traces
// pipeline before the exporters — the load-bearing before-persistence seam.
//
// The redaction is VALUE-WIDE (replace_all_patterns over attributes, "value"),
// NOT per-flat-key (replace_pattern). This is the m11.6 fix: OpenInference emits
// INDEXED sub-keys (llm.input_messages.<N>.message.content) that a per-key
// statement can never glob, so the message body leaked; value-wide matching
// covers every attribute value regardless of key shape.
func TestRenderConfigWiresRedactionProcessor(t *testing.T) {
	cfg := RenderConfig(false, DefaultDetectors())

	// The processor is declared and included in the pipeline BEFORE the exporters.
	assert.Contains(t, cfg, "transform/redaction:", "redaction processor declared")
	assert.Contains(t, cfg, "error_mode: ignore", "a missing attr must not fail the span")
	assert.Contains(t, cfg, "processors: [batch, transform/redaction]",
		"redaction runs in the traces pipeline before the exporters")

	// One VALUE-WIDE replace_all_patterns statement per detector, applied across
	// ALL attribute values (so indexed OpenInference keys are covered).
	assert.Contains(t, cfg, `replace_all_patterns(attributes, "value",`,
		"redaction acts value-wide across all attribute values")
	assert.Contains(t, cfg, "[REDACTED:email]", "email marker present")
	assert.Contains(t, cfg, "[REDACTED:ssn]", "ssn marker present")
	assert.Contains(t, cfg, "[REDACTED:key]", "key marker present")

	// The regex source is escaped as an OTTL/Go string literal (\d not a literal d).
	assert.Contains(t, cfg, `\\d{3}-\\d{2}-\\d{4}`, "ssn regex is escaped in the YAML literal")

	// Exactly one value-wide statement per detector (no per-key fan-out).
	wantStatements := len(DefaultDetectors())
	gotStatements := strings.Count(cfg, "replace_all_patterns(")
	assert.Equal(t, wantStatements, gotStatements,
		"one value-wide replace_all_patterns per detector")

	// Redaction must never touch span structure or attribute KEYS: it targets
	// attribute VALUES only ("value" mode), never keys/names/ids.
	assert.NotContains(t, cfg, `replace_all_patterns(attributes, "key"`,
		"must not rewrite attribute keys")
	assert.NotContains(t, cfg, "replace_pattern(name", "must not rewrite span names")
	assert.NotContains(t, cfg, "replace_all_patterns(name", "must not rewrite span names")
}

// TestRenderConfigRedactsIndexedOpenInferenceKeys is the m11.6 regression guard.
// The old per-flat-key config rendered replace_pattern(attributes["llm.input_
// messages"], ...), but OpenInference NEVER emits that flat key — it emits
// INDEXED sub-keys llm.input_messages.<N>.message.content, so the message body
// (the most sensitive payload) reached Langfuse UNREDACTED. The value-wide
// replace_all_patterns(attributes, "value", ...) statement matches every
// attribute value regardless of key shape, so the indexed content is covered.
//
// The rendered config is fed to the real /otelcol-contrib binary in the m11
// review + e2e; this test locks the config SHAPE that makes that work, and
// separately proves the detector regexes themselves match the indexed content.
func TestRenderConfigRedactsIndexedOpenInferenceKeys(t *testing.T) {
	cfg := RenderConfig(false, DefaultDetectors())

	// The statements must be VALUE-WIDE, not bound to a flat key. A statement
	// bound to attributes["llm.input_messages"] would never touch the indexed
	// llm.input_messages.<N>.message.content the agent actually emits.
	assert.NotContains(t, cfg, `attributes["llm.input_messages"]`,
		"must NOT bind to the flat key — OpenInference emits indexed sub-keys, so a flat-key statement leaks the body (m11.6)")
	assert.Contains(t, cfg, `replace_all_patterns(attributes, "value",`,
		"value-wide statement covers indexed llm.input_messages.<N>.message.content")

	// Prove the detector regexes catch PII inside the REAL indexed OpenInference
	// shape (the value that flows through attributes["llm.input_messages.1.message
	// .content"]). This is the exact sentinel shape the m11.6 e2e seeds.
	const indexedKey = "llm.input_messages.1.message.content"
	require.False(t, IsSensitive(indexedKey),
		"the indexed key is NOT in the flat SensitiveAttributeKeys set — which is exactly why per-key redaction missed it")
	sentinel := "contact pii-abc123@example.com ssn 123-45-6789 key sk-abc123abcdefghijklmnopqrstuvwxyz012345"
	redacted := redactString(sentinel, defaultDetectors)
	assert.Equal(t, "contact [REDACTED:email] ssn [REDACTED:ssn] key [REDACTED:key]", redacted,
		"the indexed message content value is fully redacted by the value-wide detectors — no raw PII reaches persistence")
	assert.NotContains(t, redacted, "pii-abc123@example.com", "raw email sentinel gone")
	assert.NotContains(t, redacted, "123-45-6789", "raw ssn sentinel gone")
	assert.NotContains(t, redacted, "sk-abc123", "raw key sentinel gone")
}

func TestRenderConfigLangfuseExporterStillRedacts(t *testing.T) {
	cfg := RenderConfig(true, DefaultDetectors())
	// Redaction sits before BOTH exporters — Langfuse never sees un-redacted PII.
	assert.Contains(t, cfg, "otlphttp/langfuse:", "langfuse exporter present")
	assert.Contains(t, cfg, "processors: [batch, transform/redaction]",
		"redaction precedes the langfuse export too")
	assert.Contains(t, cfg, "exporters: [debug, otlphttp/langfuse]")
}

func TestRenderConfigCustomDetectorInYAML(t *testing.T) {
	detectors, err := DetectorsWithCustom([]CustomDetectorSpec{{Name: "badge", Pattern: `EMP-\d{5}`}})
	require.NoError(t, err)
	cfg := RenderConfig(false, detectors)
	assert.Contains(t, cfg, "[REDACTED:badge]", "custom detector marker rendered into the collector config")
	assert.Contains(t, cfg, `EMP-\\d{5}`, "custom detector regex escaped into the YAML literal")
}

// TestRenderConfigDeterministic guards the make-generate-style byte-stability
// expectation: identical inputs render identical YAML.
func TestRenderConfigDeterministic(t *testing.T) {
	a := RenderConfig(true, DefaultDetectors())
	b := RenderConfig(true, DefaultDetectors())
	assert.Equal(t, a, b, "RenderConfig must be deterministic")
}
