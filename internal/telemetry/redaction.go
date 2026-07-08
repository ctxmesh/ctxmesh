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
	"slices"

	"go.opentelemetry.io/otel/attribute"
)

// IsSensitive reports whether a span-attribute key carries user/model payload
// that the redaction policy targets.
func IsSensitive(key string) bool {
	return slices.Contains(SensitiveAttributeKeys, key)
}

// Redact is the M3 redaction STUB. It is a passthrough: attributes are
// returned unchanged. It exists so the redaction seam is present and tested
// now; M11 (§13.3) replaces the body with real regex/named-detector policy
// (emails, keys, SSNs) applied at the collector before persistence, driven by
// a per-agent/tenant tracePolicy. The key set it will act on is
// SensitiveAttributeKeys.
//
// Keeping this a passthrough (rather than absent) means the pipeline shape,
// call sites, and tests are stable across the M3→M11 change.
func Redact(attrs []attribute.KeyValue) []attribute.KeyValue {
	// M11: for each attr whose Key is sensitive, apply the configured detectors.
	return attrs
}
