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

	"github.com/ctxmesh/ctxmesh/internal/toolmanifest"
)

const (
	// toolsMountPath is where the <agent>-tools ConfigMap is mounted in the agent container. The
	// launcher serves the manifest on :2999 and follows its updates; the SDKs read the file directly
	// when :2999 is not answering. Not /etc/agent itself: the prompt, guardrail and skill volumes mount
	// beneath that.
	toolsMountPath = "/etc/agent/tools"

	// toolsVolumeName is the pod volume name for the tools ConfigMap.
	toolsVolumeName = "agent-tools"

	// envToolsJSONPath names the mounted manifest for the launcher and the SDKs.
	envToolsJSONPath = "TOOLS_JSON_PATH"
)

// toolsMount mounts the <agent>-tools ConfigMap read-only into the agent container. A whole-volume
// mount, never subPath: the kubelet does not update a subPath mount when the ConfigMap changes.
func toolsMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: toolsVolumeName, MountPath: toolsMountPath, ReadOnly: true}
}

// toolsVolume is the ConfigMap-backed volume mounting the <agent>-tools CM.
// Optional so the pod starts even before the binding controller has written the
// CM: the launcher serves an empty manifest until the kubelet projects it.
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
