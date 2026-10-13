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
	"strconv"

	corev1 "k8s.io/api/core/v1"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// The token service authenticates every caller by a projected ServiceAccount token (ADR 0154). Two
// audiences, so a token the agent's code can read is refused where only the sidecar may call:
//   - the sidecar's token is mounted into the egress sidecar container only. Mounts are per container and
//     no agent pod shares its process namespace, so the agent's code cannot read it;
//   - the agent's token is mounted into the user container of an agent with long-term memory or knowledge
//     bases. It proves only which agent the pod is.
const (
	// envTokenServiceTokenPath is the token file the launcher and the sidecar present.
	envTokenServiceTokenPath = "TOKEN_SERVICE_TOKEN_PATH"

	tokenServiceSidecarTokenVolume    = "token-service-token"
	tokenServiceSidecarTokenMountPath = "/var/run/secrets/ctxmesh-token-service"
	tokenServiceAgentTokenVolume      = "token-service-agent-token"
	tokenServiceAgentTokenMountPath   = "/var/run/secrets/ctxmesh-token-service-agent"

	// projectedTokenFile is the file a projected token volume holds.
	projectedTokenFile = "token"
)

// projectedTokenVolume is a kubelet-rotated projected ServiceAccount token for one audience.
func projectedTokenVolume(name, audience string) corev1.Volume {
	expiry := statelayerTokenExpirySecs
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Path: projectedTokenFile, Audience: audience, ExpirationSeconds: &expiry,
					},
				}},
			},
		},
	}
}

// withSidecarTokenServiceToken mounts the sidecar audience's token into the egress sidecar container
// and points it there, returning the volume the pod needs.
func withSidecarTokenServiceToken(sidecar *corev1.Container) corev1.Volume {
	sidecar.Env = append(sidecar.Env, corev1.EnvVar{
		Name: envTokenServiceTokenPath, Value: tokenServiceSidecarTokenMountPath + "/" + projectedTokenFile,
	})
	sidecar.VolumeMounts = append(sidecar.VolumeMounts, corev1.VolumeMount{
		Name: tokenServiceSidecarTokenVolume, MountPath: tokenServiceSidecarTokenMountPath, ReadOnly: true,
	})
	return projectedTokenVolume(tokenServiceSidecarTokenVolume, runcap.TokenServicePodAudience)
}

// agentTokenServiceTokenMount is the agent audience's token mount on the user container.
func agentTokenServiceTokenMount() corev1.VolumeMount {
	return corev1.VolumeMount{
		Name: tokenServiceAgentTokenVolume, MountPath: tokenServiceAgentTokenMountPath, ReadOnly: true,
	}
}

// agentTokenServiceTokenVolume is the agent audience's token volume.
func agentTokenServiceTokenVolume() corev1.Volume {
	return projectedTokenVolume(tokenServiceAgentTokenVolume, runcap.TokenServiceAgentAudience)
}

// tokenServiceTokenFold is what the two token mounts fold into the revision digest, "" when the pod has
// neither. They are read by no digest, and a pod-template change that keeps the revision name never
// reaches a running pod.
func tokenServiceTokenFold(sidecar, agent bool) string {
	if !sidecar && !agent {
		return ""
	}
	return "|token-service-auth:" + strconv.FormatBool(sidecar) + ":" + strconv.FormatBool(agent)
}
