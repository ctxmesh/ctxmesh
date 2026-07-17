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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

func envValue(c corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func TestEgressSidecarContainer(t *testing.T) {
	cfg := OBOEgressConfig{
		Enabled:                true,
		SidecarImage:           "egress-sidecar:test",
		CapabilityPublicKeyB64: "PUBKEY",
		CapabilityAudience:     "aud",
		CredentialNamespace:    "ae-credentials",
	}
	c := egressSidecarContainer(cfg, "team-alpha/support", `[{"name":"scalekit"}]`)

	assert.Equal(t, egressSidecarContainerName, c.Name)
	assert.Equal(t, "egress-sidecar:test", c.Image)
	assert.Empty(t, c.Ports, "no container port (Knative single-port rule)")

	for name, want := range map[string]string{
		"MCP_CAPABILITY_PUBLIC_KEY": "PUBKEY",
		"MCP_CAPABILITY_AUDIENCE":   "aud",
		"MCP_CREDENTIAL_NAMESPACE":  "ae-credentials",
		"EGRESS_AGENT":              "team-alpha/support",
		"EGRESS_ROUTES":             `[{"name":"scalekit"}]`,
	} {
		e, ok := envValue(c, name)
		require.True(t, ok, "env %s present", name)
		assert.Equal(t, want, e.Value, name)
	}

	// POD_NAMESPACE comes from the downward API, not a literal.
	podNS, ok := envValue(c, "POD_NAMESPACE")
	require.True(t, ok)
	require.NotNil(t, podNS.ValueFrom)
	assert.Equal(t, "metadata.namespace", podNS.ValueFrom.FieldRef.FieldPath)

	// No delegation env unless a token-service URL is configured.
	_, hasDelegate := envValue(c, "TOKEN_SERVICE_URL")
	assert.False(t, hasDelegate)

	cfg.TokenServiceURL = "https://token-service:8443"
	delegating := egressSidecarContainer(cfg, "team-alpha/support", "[]")
	tsu, ok := envValue(delegating, "TOKEN_SERVICE_URL")
	require.True(t, ok)
	assert.Equal(t, "https://token-service:8443", tsu.Value)
}

func TestEgressRoutesJSON(t *testing.T) {
	assert.Equal(t, "[]", egressRoutesJSON(nil))
	assert.JSONEq(t,
		`[{"name":"scalekit","targetURL":"https://mcp.scalekit.com/mcp","oauth":true}]`,
		egressRoutesJSON([]toolmanifest.Route{{Name: "scalekit", TargetURL: "https://mcp.scalekit.com/mcp", OAuth: true}}),
	)
}

func TestEgressDigest(t *testing.T) {
	routes := []toolmanifest.Route{{Name: "scalekit", TargetURL: "https://a", OAuth: true}}
	assert.Empty(t, egressDigest("img", nil), "no routes ⇒ no digest (pod template unchanged)")

	base := egressDigest("img", routes)
	assert.NotEmpty(t, base)
	// A route URL change (the real URL lives in the sidecar env now) rolls the pod.
	assert.NotEqual(t, base, egressDigest("img", []toolmanifest.Route{{Name: "scalekit", TargetURL: "https://b", OAuth: true}}))
	// A sidecar image change rolls the pod.
	assert.NotEqual(t, base, egressDigest("img2", routes))
}
