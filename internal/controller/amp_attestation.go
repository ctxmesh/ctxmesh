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
	"crypto/sha256"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// envAMPRequireAttestation tells a registry member's launcher its posture for in-mesh calls (ADR 0153):
// "false" also admits a call that carries no attestation and no run capability. Anything else requires
// one.
const envAMPRequireAttestation = "AMP_REQUIRE_ATTESTATION"

// ampAttestationEnv is what a registry member's launcher needs to take part in attested AMP: the path of
// its BFF-audience pod token, the capability public key (and audience, when the install sets one) to
// verify attestations and run capabilities, and the posture. env is the platform's so far, with a
// member's spec.env values for these already dropped (memberPlatformEnvNames); a variable the platform
// already set there is not repeated.
func ampAttestationEnv(env []corev1.EnvVar, obo OBOEgressConfig, require bool) []corev1.EnvVar {
	var out []corev1.EnvVar
	present := func(name string) bool { return envVarPresent(env, name) || envVarPresent(out, name) }
	if !present(envBFFPodTokenPath) {
		out = append(out, corev1.EnvVar{Name: envBFFPodTokenPath, Value: bffPodTokenMountPath + "/token"})
	}
	if obo.CapabilityPublicKeyB64 != "" {
		if !present(envMCPCapPublicKey) {
			out = append(out, corev1.EnvVar{Name: envMCPCapPublicKey, Value: obo.CapabilityPublicKeyB64})
		}
		if obo.CapabilityAudience != "" && !present(envMCPCapAudience) {
			out = append(out, corev1.EnvVar{Name: envMCPCapAudience, Value: obo.CapabilityAudience})
		}
	}
	if !present(envAMPRequireAttestation) {
		out = append(out, corev1.EnvVar{Name: envAMPRequireAttestation, Value: strconv.FormatBool(require)})
	}
	return out
}

// ampAttestationFold is the revision-digest input for a member's attestation wiring: the posture and the
// key, so flipping one or rotating the other reaches running members.
func ampAttestationFold(obo OBOEgressConfig, require bool) string {
	key := "none"
	if obo.CapabilityPublicKeyB64 != "" {
		sum := sha256.Sum256([]byte(obo.CapabilityPublicKeyB64 + "|" + obo.CapabilityAudience))
		key = fmt.Sprintf("%x", sum[:])[:8]
	}
	return fmt.Sprintf("|amp-attestation:v1:require=%t:key=%s", require, key)
}
