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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

const (
	// egressSidecarContainerName is the injecting egress proxy container in the agent pod.
	egressSidecarContainerName = "egress-sidecar"

	// egressSidecarListenAddr is where the sidecar binds inside the shared pod netns. It must
	// avoid every other in-pod port: the managed-agent's upstream (8081) + AGENT_PORT (8080),
	// discovery (2999), sidecar tools (3001+), feedback (2995), memory (2998), and the Knative
	// queue-proxy ports — so 8899. Injected as EGRESS_LISTEN_ADDR so the sidecar binds exactly
	// what the manifest rewrite points at. Like the other sidecars it declares NO container
	// port (Knative single-port rule) and binds inside the netns.
	egressSidecarListenAddr = "127.0.0.1:8899"

	// egressSidecarBaseURL is what the manifest rewrite points remote tool endpoints at (it
	// must match egressSidecarListenAddr — the agent reaches the sidecar there on localhost).
	egressSidecarBaseURL = "http://" + egressSidecarListenAddr
)

// OBOEgressConfig configures OBO egress-sidecar injection (ADR 0030). It is set once at
// controller start-up from env. Enabled is the opt-in flag: when false the controllers
// behave EXACTLY as before (no rewrite, no sidecar) — default-off keeps generated manifests
// drift-free (the m25.1c pattern). When true, an agent with remote OBO tools gets the
// sidecar injected and its remote endpoints rewritten through it.
type OBOEgressConfig struct {
	Enabled bool
	// SidecarImage is the egress-sidecar image to inject.
	SidecarImage string
	// CapabilityPublicKeyB64 is the platform public key the sidecar verifies run
	// capabilities against; CapabilityAudience is the audience it accepts.
	CapabilityPublicKeyB64 string
	CapabilityAudience     string
	// CredentialNamespace is the locked namespace holding grant Secrets (the sidecar reads
	// them in embedded mode, or the central service does in delegation mode).
	CredentialNamespace string
	// TokenServiceURL, when set, makes the sidecar DELEGATE resolution to the central token
	// service (the scaling split); empty ⇒ the sidecar embeds the backend (first working cut).
	TokenServiceURL string
}

// egressSidecarContainer builds the injecting egress proxy container for an agent pod. It
// declares no container port (Knative single-port rule), binds localhost in the pod netns,
// and carries the verify + resolve config as env — the platform public key, audience, the
// grant namespace, the agent identity (act-scope), and the route table (real MCP URLs, kept
// out of the agent's own manifest). POD_NAMESPACE (the grant source ns) is the agent's own
// namespace, set as a LITERAL (not the downward-API fieldRef, which Knative Serving forbids —
// `kubernetes.podspec-fieldref` is off by default; the controller knows the namespace anyway).
func egressSidecarContainer(cfg OBOEgressConfig, namespace, agentIdentity, routesJSON string) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "MCP_CAPABILITY_PUBLIC_KEY", Value: cfg.CapabilityPublicKeyB64},
		{Name: "MCP_CAPABILITY_AUDIENCE", Value: cfg.CapabilityAudience},
		{Name: "MCP_CREDENTIAL_NAMESPACE", Value: cfg.CredentialNamespace},
		{Name: "EGRESS_AGENT", Value: agentIdentity},
		{Name: "EGRESS_ROUTES", Value: routesJSON},
		{Name: "POD_NAMESPACE", Value: namespace},
		// Bind the port the manifest rewrite points at — chosen to avoid every other in-pod
		// port (the managed-agent's own listener collided at the default 8081).
		{Name: "EGRESS_LISTEN_ADDR", Value: egressSidecarListenAddr},
	}
	if cfg.TokenServiceURL != "" {
		env = append(env, corev1.EnvVar{Name: "TOKEN_SERVICE_URL", Value: cfg.TokenServiceURL})
	}
	return corev1.Container{
		Name:  egressSidecarContainerName,
		Image: cfg.SidecarImage,
		Env:   env,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("25m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	}
}

// egressRoutesJSON serializes the sidecar route table to the EGRESS_ROUTES env value. An
// empty/failed marshal yields "[]" so the sidecar starts with no routes rather than crashing.
func egressRoutesJSON(routes []toolmanifest.Route) string {
	if len(routes) == 0 {
		return "[]"
	}
	b, err := json.Marshal(routes)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// egressDigest folds the injected egress sidecar (image + routes) into the pod-template
// structural digest, so adding/removing the sidecar OR changing a route (the real URL now
// lives in the sidecar env, not the hot-path manifest) rolls a new revision. Empty when no
// OBO route is present (the pod template is unchanged).
func egressDigest(image string, routes []toolmanifest.Route) string {
	if len(routes) == 0 {
		return ""
	}
	type shape struct {
		Image      string               `json:"image"`
		ListenAddr string               `json:"listenAddr"`
		Routes     []toolmanifest.Route `json:"routes"`
	}
	b, err := json.Marshal(shape{Image: image, ListenAddr: egressSidecarListenAddr, Routes: routes})
	if err != nil {
		return "invalid"
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])[:8]
}
