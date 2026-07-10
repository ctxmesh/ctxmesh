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
	"fmt"
	"regexp"
	"slices"

	"go.opentelemetry.io/otel/attribute"
)

// Detector is one named redaction rule (§13.3 trace governance): a regular
// expression that identifies a class of sensitive substring and the stable
// marker that replaces every match. Detectors are the single source of truth
// for redaction — both the in-process Redact() below and the OTTL statements
// generated for the collector (RenderConfig) are derived from the SAME set, so
// the collector and the Go path can never drift.
//
// Redaction is deliberately SUBSTRING-level, not whole-value: a match is
// replaced by its marker while the surrounding non-PII text of the attribute
// value is preserved. This keeps the trace readable and the tree intact while
// scrubbing only the sensitive bytes.
type Detector struct {
	// Name is the detector's short label; it appears in the marker, e.g. an
	// "email" detector replaces matches with "[REDACTED:email]".
	Name string
	// Pattern is the compiled Go regexp. It MUST be RE2-compatible (no
	// backreferences, no lookaround) so the identical PatternSource string is
	// also a valid OTTL/RE2 pattern in the collector.
	Pattern *regexp.Regexp
	// PatternSource is the raw regexp source. The collector's OTTL
	// replace_pattern needs the source string (not a compiled object); keeping
	// it here guarantees the Go regexp and the collector regexp are byte-identical.
	PatternSource string
}

// Marker is the stable replacement token for this detector, e.g.
// "[REDACTED:email]". Deterministic: the same input always yields the same
// marker so redacted traces are diff-stable and the e2e can assert on it.
func (d Detector) Marker() string { return "[REDACTED:" + d.Name + "]" }

// newDetector compiles source and pairs it with its raw form. It uses
// MustCompile because the built-in patterns are compile-time constants; a bad
// pattern is a programming error caught by the first test/run, never user input.
func newDetector(name, source string) Detector {
	return Detector{
		Name:          name,
		Pattern:       regexp.MustCompile(source),
		PatternSource: source,
	}
}

// Built-in detector pattern sources (§13.3). RE2-compatible so they work
// identically in Go and in the collector's OTTL replace_pattern.
const (
	// emailPattern matches an RFC-shaped email address.
	emailPattern = `[\w.+-]+@[\w-]+\.[\w.-]+`
	// ssnPattern matches a US Social Security Number (###-##-####).
	ssnPattern = `\d{3}-\d{2}-\d{4}`
	// keyPattern matches common API-key / token shapes: OpenAI-style secret
	// keys (sk-…), AWS access-key ids (AKIA…), and long high-entropy tokens
	// (a run of 32+ base64url/hex characters — enough length that ordinary
	// prose words never trip it, catching bearer tokens / opaque secrets).
	keyPattern = `sk-[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|[A-Za-z0-9_\-]{32,}`
)

// defaultDetectors is the always-on built-in policy: emails, US SSNs, and API
// keys / tokens. ORDER MATTERS — the collector applies statements in sequence
// and Redact() below iterates in this order, so the more specific key shapes
// (sk-/AKIA) are covered by the same keyPattern alternation and the generic
// long-token arm is last within it. The order across detectors (email, ssn,
// key) is stable and deterministic.
var defaultDetectors = []Detector{
	newDetector("email", emailPattern),
	newDetector("ssn", ssnPattern),
	newDetector("key", keyPattern),
}

// DefaultDetectors returns a copy of the built-in, always-on detector set
// (emails / SSNs / keys). Callers must not mutate the returned slice's backing
// array; extension detectors are appended onto a copy.
func DefaultDetectors() []Detector {
	return slices.Clone(defaultDetectors)
}

// CustomDetectorSpec is a per-agent extension rule (name + RE2 pattern source)
// sourced from AgentDeployment.spec.tracePolicy. It is defined here (rather than
// importing the API package) so the telemetry package stays dependency-free and
// the caller adapts its CRD type into this shape.
type CustomDetectorSpec struct {
	Name    string
	Pattern string
}

// DetectorsWithCustom returns the always-on built-in detectors followed by the
// caller's custom detectors, in order. A custom pattern that fails to compile
// as RE2 returns an error and NO detectors are appended past it — an invalid
// tracePolicy must never silently disable redaction or wedge the config; the
// caller falls back to the built-in defaults and surfaces the error. Built-in
// detectors are always returned regardless of a bad custom pattern.
func DetectorsWithCustom(custom []CustomDetectorSpec) ([]Detector, error) {
	out := DefaultDetectors()
	for _, c := range custom {
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			return out, fmt.Errorf("tracePolicy detector %q: invalid pattern: %w", c.Name, err)
		}
		out = append(out, Detector{Name: c.Name, Pattern: re, PatternSource: c.Pattern})
	}
	return out, nil
}

// IsSensitive reports whether a span-attribute key carries user/model payload
// that the redaction policy targets.
func IsSensitive(key string) bool {
	return slices.Contains(SensitiveAttributeKeys, key)
}

// Redact applies the built-in detector policy to the value of every attribute
// whose key is in SensitiveAttributeKeys, replacing each PII match with its
// detector's stable marker. Non-sensitive attributes and non-matching text are
// returned unchanged, and the input slice order is preserved.
//
// This is a Go-side substring redactor derived from the SAME defaultDetectors
// the collector uses, so the detector regexes and markers can never drift from
// the enforcement point. NOTE: the collector (RenderConfig) is authoritative for
// spans crossing the export boundary and applies its detectors VALUE-WIDE across
// ALL attribute values — because the payload-bearing OpenInference attributes are
// INDEXED sub-keys (llm.input_messages.<N>.message.content) that a flat key set
// cannot enumerate. This Go form scopes to the known flat SensitiveAttributeKeys
// and is used for in-process testing / any Go-side producer that sets those flat
// keys; it is not the persistence seam.
func Redact(attrs []attribute.KeyValue) []attribute.KeyValue {
	return RedactWith(attrs, defaultDetectors)
}

// RedactWith is Redact parameterised by the detector set (built-in defaults
// plus any per-agent tracePolicy extensions). It never mutates the input: only
// sensitive-key string attributes whose value actually changes are rewritten.
func RedactWith(attrs []attribute.KeyValue, detectors []Detector) []attribute.KeyValue {
	out := make([]attribute.KeyValue, len(attrs))
	copy(out, attrs)
	for i, kv := range out {
		if !IsSensitive(string(kv.Key)) {
			continue
		}
		// Only string-valued payload attributes carry redactable text; the
		// OpenInference payload keys are all strings.
		if kv.Value.Type() != attribute.STRING {
			continue
		}
		redacted := redactString(kv.Value.AsString(), detectors)
		if redacted != kv.Value.AsString() {
			out[i] = attribute.String(string(kv.Key), redacted)
		}
	}
	return out
}

// redactString applies every detector to s in order, replacing each match with
// that detector's marker.
func redactString(s string, detectors []Detector) string {
	for _, d := range detectors {
		s = d.Pattern.ReplaceAllString(s, d.Marker())
	}
	return s
}
