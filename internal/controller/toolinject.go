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

package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ctxmesh/agentry/internal/toolmanifest"
)

const (
	// DiscoveryImage is the tool-discovery sidecar image (Dockerfile.discovery,
	// built by `make docker-build-discovery`). dev.local/ prefix → Knative
	// skips tag→digest resolution for the local image (same rationale as the
	// collector image).
	DiscoveryImage = "dev.local/agent-discovery:0.1.0"

	// DiscoveryContainerName is the discovery sidecar container name in the pod.
	DiscoveryContainerName = "tool-discovery"

	// discoveryPort is the sidecar's control/query port. NOT declared as a
	// container port — Knative permits exactly one port across the pod (the
	// user container's). The sidecar still binds it inside the shared netns and
	// the controller reaches it on the pod IP.
	discoveryPort = 2999

	// toolsMountPath is where the <agent>-tools ConfigMap is mounted; the
	// sidecar reads TOOLS_JSON_PATH=<toolsMountPath>/tools.json for cold start.
	toolsMountPath = "/etc/agent"

	// toolsVolumeName is the pod volume name for the tools ConfigMap.
	toolsVolumeName = "agent-tools"
)

// discoverySidecarContainer builds the discovery sidecar container. It declares
// no container ports (Knative single-port rule), mounts the <agent>-tools
// ConfigMap read-only, and carries the cold-start env the sidecar reads. image is the
// resolved sidecar image (COLLECTOR_IMAGE-style override, OPS-1) — DiscoveryImage default.
func discoverySidecarContainer(image string) corev1.Container {
	return corev1.Container{
		Name:  DiscoveryContainerName,
		Image: image,
		Env: []corev1.EnvVar{
			{Name: "DISCOVERY_PORT", Value: fmt.Sprintf("%d", discoveryPort)},
			{Name: "TOOLS_JSON_PATH", Value: toolsMountPath + "/" + toolsConfigMapKey},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("25m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: toolsVolumeName, MountPath: toolsMountPath, ReadOnly: true},
		},
	}
}

// toolsVolume is the ConfigMap-backed volume mounting the <agent>-tools CM.
// Optional so the pod starts even before the binding controller has written the
// CM (cold start then reads an absent file and serves empty until the push).
func toolsVolume(agentName string) corev1.Volume {
	optional := true
	return corev1.Volume{
		Name: toolsVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: toolsConfigMapName(agentName)},
				Optional:             &optional,
			},
		},
	}
}

// sidecarToolContainer builds a sidecar-mode MCP tool-server container from an
// assigned SidecarTool. It declares no container ports (Knative single-port
// rule); the server binds the assigned port inside the pod netns and the agent
// reaches it at http://127.0.0.1:<port>/mcp. The container name is derived from
// the binding name so multiple tools coexist deterministically.
func sidecarToolContainer(st toolmanifest.SidecarTool) corev1.Container {
	return corev1.Container{
		Name:  "tool-" + st.BindingName,
		Image: st.Image,
		Env: []corev1.EnvVar{
			{Name: "PORT", Value: fmt.Sprintf("%d", st.Port)},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("25m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	}
}
