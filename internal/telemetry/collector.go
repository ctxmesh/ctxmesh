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

// Package telemetry builds the OTel Collector sidecar the AgentDeployment
// controller injects into every agent pod (ADR 0005 / specs/observability.md).
// The collector receives OTLP at localhost:4317 from the agent's base-image
// instrumentation and the launcher boundary span, and exports traces.
package telemetry

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// CollectorImage is the project-owned OTel Collector image (images/otel-
	// collector/): the collector-CONTRIB binary on debian:12-slim. We repackage
	// because upstream distroless arm64 omits the glibc loader (exec failure);
	// debian provides it, so the same image runs on arm64 (local) + amd64 (CI).
	//
	// M11: switched core → contrib because the redaction seam (§13.3) is a
	// `transform` (OTTL replace_pattern) processor, which lives in contrib only.
	// Core carries attributes/filter/batch but no substring-level regex redaction;
	// transform gives surgical PII scrubbing (marker per detector, non-PII text
	// preserved) at the before-export point.
	//
	// Built + side-loaded by the harness (never pulled). The `dev.local/`
	// prefix is in Knative's default registries-skipping-tag-resolving list, so
	// Knative won't try to resolve its digest against a registry (which would
	// fail for a local image → ContainerMissing). Publishing a real registry
	// image is a release-hardening task (tracked for M12).
	CollectorImage = "dev.local/agent-otel-collector:0.116.0"

	// CollectorContainerName is the sidecar container name in the agent pod.
	CollectorContainerName = "otel-collector"

	collectorConfigMountPath = "/etc/otel"
	collectorConfigVolume    = "otel-config"

	// LangfuseSecretName is the Secret (in the agent's namespace) that, when
	// present, switches the collector to also export to Langfuse. Seeded by
	// `make -C harness dev-up M=3`.
	LangfuseSecretName = "langfuse-otlp"
)

// ConfigMapName is the per-agent collector-config ConfigMap name.
func ConfigMapName(agentName string) string { return agentName + "-otel-config" }

// RenderConfig produces the collector config YAML. The `debug` exporter is
// ALWAYS present — it is the automated-assertion sink the e2e slice reads via
// `kubectl logs` (works in CI without Langfuse). When langfuse is true an
// otlphttp exporter is added to the same traces pipeline for the UI backend.
//
// M11 (§13.3): the traces pipeline now runs a `transform/redaction` processor
// BEFORE every exporter, so PII in the sensitive payload attributes is scrubbed
// at the collector before it reaches ANY sink (debug OR Langfuse) — i.e. before
// persistence. detectors is the redaction policy: the built-in defaults plus
// any per-agent tracePolicy extensions. Passing an empty slice omits the
// processor (pipeline unchanged) — defence for a mis-wired caller; production
// callers always pass DefaultDetectors().
func RenderConfig(langfuse bool, detectors []Detector) string {
	exporters := "  debug:\n    verbosity: detailed\n"
	pipelineExporters := "[debug]"
	if langfuse {
		exporters += "" +
			"  otlphttp/langfuse:\n" +
			"    endpoint: ${env:LANGFUSE_OTLP_ENDPOINT}\n" +
			"    headers:\n" +
			"      Authorization: ${env:LANGFUSE_OTLP_AUTH}\n"
		pipelineExporters = "[debug, otlphttp/langfuse]"
	}

	// Build the redaction processor block + pipeline entry from the detectors.
	// The processor is placed AFTER batch and before the exporters, so the
	// batched-but-not-yet-exported spans are redacted on the way out.
	redactionProcessor := ""
	pipelineProcessors := "[batch]"
	if len(detectors) > 0 {
		redactionProcessor = renderRedactionProcessor(detectors)
		pipelineProcessors = "[batch, transform/redaction]"
	}

	return fmt.Sprintf(`receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318
processors:
  batch: {}
%sexporters:
%sservice:
  telemetry:
    logs:
      # info so the debug exporter actually prints spans — it is the e2e
      # assertion sink (ADR 0006). (M11/prod hardening drops the debug exporter.)
      level: info
  pipelines:
    traces:
      receivers: [otlp]
      processors: %s
      exporters: %s
`, redactionProcessor, exporters, pipelineProcessors, pipelineExporters)
}

// renderRedactionProcessor emits the `transform/redaction` processor block:
// one OTTL replace_all_patterns statement per detector, applied VALUE-WIDE
// across every span attribute. This is the before-persistence redaction seam
// (§13.3). The statements share the detector regex SOURCES with the Go Redact()
// path, so the collector and the in-process policy cannot drift.
//
// Why value-wide (not per-key replace_pattern): OpenInference does NOT emit a
// flat `llm.input_messages` attribute — it emits INDEXED sub-keys
// (`llm.input_messages.<N>.message.content`, `.message.role`, and the analogous
// output/tool keys) where N is unbounded. A per-key `replace_pattern` targets
// one exact key and cannot glob those indices, so it silently missed the actual
// message body (the m11.6 e2e leak). `replace_all_patterns(attributes, "value",
// <regex>, <marker>)` applies each detector regex to ALL attribute VALUES, so it
// catches the indexed message content, the flat input.value/output.value, and
// any string attribute regardless of key shape — no PII reaches persistence.
//
// Over-redaction is the deliberate trade-off: value-wide scrubbing also touches
// non-payload attributes (a long high-entropy `session.id`, or a PII-shaped
// `agent.name`). Completeness (no PII leak) is the priority for a redaction
// feature, so this is accepted — a 32+ char high-entropy run is exactly the
// shape of a token/secret we must never persist wherever it appears, and the
// email/SSN shapes don't collide with ordinary prose.
//
// error_mode: ignore — a span with no matching attribute (or none at all) is
// skipped, not failed; redaction must never drop or wedge a span.
func renderRedactionProcessor(detectors []Detector) string {
	var b strings.Builder
	b.WriteString("  transform/redaction:\n")
	b.WriteString("    error_mode: ignore\n")
	b.WriteString("    trace_statements:\n")
	b.WriteString("      - context: span\n")
	b.WriteString("        statements:\n")
	// Deterministic order: detectors in their declared slice order — so the
	// rendered YAML is byte-stable.
	for _, d := range detectors {
		b.WriteString("          - ")
		b.WriteString(redactStatement(d))
		b.WriteString("\n")
	}
	return b.String()
}

// redactStatement builds one value-wide OTTL statement:
//
//	replace_all_patterns(attributes, "value", "<regex>", "<marker>")
//
// The "value" mode applies the regex to every attribute VALUE in the span's
// attribute map (the "key" mode would rewrite keys — we never touch keys/names/
// ids, only values). The regex and marker are emitted as double-quoted OTTL
// string literals with backslashes and quotes escaped, so an RE2 pattern like
// `\d{3}-\d{2}-\d{4}` survives YAML + OTTL parsing intact.
func redactStatement(d Detector) string {
	return fmt.Sprintf(`replace_all_patterns(attributes, "value", %s, %s)`,
		ottlQuote(d.PatternSource), ottlQuote(d.Marker()))
}

// ottlQuote returns s as a Go/OTTL double-quoted string literal. OTTL string
// literals use the same escaping as Go, so strconv.Quote produces a valid
// literal (e.g. `\d` → `"\\d"`), which is also YAML-safe on a single line.
func ottlQuote(s string) string { return strconv.Quote(s) }

// BasicAuthHeader returns the Langfuse OTLP Authorization header value from a
// public/secret key pair: "Basic base64(public:secret)".
func BasicAuthHeader(publicKey, secretKey string) string {
	raw := publicKey + ":" + secretKey
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

// Container builds the collector sidecar. langfuseEnv carries
// LANGFUSE_OTLP_ENDPOINT + LANGFUSE_OTLP_AUTH when Langfuse export is enabled;
// pass nil for debug-only.
func Container(configMapName string, langfuseEnv []corev1.EnvVar, image string) corev1.Container {
	return corev1.Container{
		Name:  CollectorContainerName,
		Image: image,
		Args:  []string{"--config", collectorConfigMountPath + "/config.yaml"},
		Env:   langfuseEnv,
		// No ContainerPorts: Knative allows exactly one port across all
		// containers in a pod (the user container's). The collector still
		// listens on 4317/4318 inside the shared pod netns; the user container
		// reaches it via localhost regardless of a declared containerPort.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			// Memory limit caps a runaway/leaking collector so it can't OOM the
			// node; 256Mi is ample for M3 dev span volume.
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: collectorConfigVolume, MountPath: collectorConfigMountPath, ReadOnly: true},
		},
	}
}

// Volume is the ConfigMap-backed volume the sidecar mounts for its config.
func Volume(configMapName string) corev1.Volume {
	return corev1.Volume{
		Name: collectorConfigVolume,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
			},
		},
	}
}
