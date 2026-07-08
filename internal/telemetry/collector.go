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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// CollectorImage is the pinned OTel Collector (contrib) image. Bump
	// deliberately; contrib carries the otlphttp + debug exporters used here.
	CollectorImage = "otel/opentelemetry-collector-contrib:0.116.0"

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
// The redaction/attributes processor is a passthrough STUB in M3; the real
// regex/detector policy lands at M11, applied here at the collector before
// export (§13.3).
func RenderConfig(langfuse bool) string {
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

	return fmt.Sprintf(`receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318
processors:
  batch: {}
  # redaction stub (M11 fills this in): attribute passthrough for now.
exporters:
%sservice:
  telemetry:
    logs:
      # info so the debug exporter actually prints spans — it is the e2e
      # assertion sink (ADR 0006). (M11/prod hardening drops the debug exporter.)
      level: info
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: %s
`, exporters, pipelineExporters)
}

// BasicAuthHeader returns the Langfuse OTLP Authorization header value from a
// public/secret key pair: "Basic base64(public:secret)".
func BasicAuthHeader(publicKey, secretKey string) string {
	raw := publicKey + ":" + secretKey
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

// Container builds the collector sidecar. langfuseEnv carries
// LANGFUSE_OTLP_ENDPOINT + LANGFUSE_OTLP_AUTH when Langfuse export is enabled;
// pass nil for debug-only.
func Container(configMapName string, langfuseEnv []corev1.EnvVar) corev1.Container {
	return corev1.Container{
		Name:  CollectorContainerName,
		Image: CollectorImage,
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
