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
	c := egressSidecarContainer(cfg, "team-alpha", "team-alpha/support", "r:squad-a", `[{"name":"scalekit"}]`, false, false, nil, nil)

	assert.Equal(t, egressSidecarContainerName, c.Name)
	assert.Equal(t, "egress-sidecar:test", c.Image)
	assert.Empty(t, c.Ports, "no container port (Knative single-port rule)")
	// No policy passed ⇒ no TOOL_POLICY_FILE env, no mount (permissive plumbing off).
	_, hasPolicyEnv := envValue(c, "TOOL_POLICY_FILE")
	assert.False(t, hasPolicyEnv, "no toolPolicy ⇒ no TOOL_POLICY_FILE env")
	assert.Empty(t, c.VolumeMounts, "no toolPolicy ⇒ no policy mount")

	for name, want := range map[string]string{
		"MCP_CAPABILITY_PUBLIC_KEY": "PUBKEY",
		"MCP_CAPABILITY_AUDIENCE":   "aud",
		"MCP_CREDENTIAL_NAMESPACE":  "ae-credentials",
		"EGRESS_AGENT":              "team-alpha/support",
		// The trust boundary the sidecar serves — the registry (ADR 0033, m30.3).
		"EGRESS_BOUNDARY": "r:squad-a",
		"EGRESS_ROUTES":   `[{"name":"scalekit"}]`,
		// POD_NAMESPACE is a LITERAL (Knative Serving forbids the downward-API fieldRef).
		"POD_NAMESPACE": "team-alpha",
	} {
		e, ok := envValue(c, name)
		require.True(t, ok, "env %s present", name)
		assert.Equal(t, want, e.Value, name)
		assert.Nil(t, e.ValueFrom, "%s must be a literal value (Knative rejects valueFrom.fieldRef)", name)
	}

	// No delegation env unless a token-service URL is configured.
	_, hasDelegate := envValue(c, "TOKEN_SERVICE_URL")
	assert.False(t, hasDelegate)
	// No record env for a non-record-capable sidecar (byte-for-byte the pre-M78 OBO sidecar).
	_, hasRecord := envValue(c, "RECORD_CAPABLE")
	assert.False(t, hasRecord)
	_, hasStore := envValue(c, "OBJECT_STORE_ADDR")
	assert.False(t, hasStore, "a non-record sidecar gets no object-store env")

	cfg.TokenServiceURL = "https://token-service:8443"
	delegating := egressSidecarContainer(cfg, "team-alpha", "team-alpha/support", "r:squad-a", "[]", false, false, nil, nil)
	tsu, ok := envValue(delegating, "TOKEN_SERVICE_URL")
	require.True(t, ok)
	assert.Equal(t, "https://token-service:8443", tsu.Value)
}

// TestEgressSidecarContainer_RecordMode: a RECORD-CAPABLE sidecar (M78, ADR 0071 §1/C1) gets
// RECORD_CAPABLE=true + the durable object-store env (the fixture sink → C2 fail-closed at startup
// if absent), all STATIC (never valueFrom, the m5.7 Knative landmine).
func TestEgressSidecarContainer_RecordMode(t *testing.T) {
	cfg := OBOEgressConfig{
		SidecarImage:           "egress-sidecar:test",
		CapabilityPublicKeyB64: "PUBKEY",
		CapabilityAudience:     "aud",
		CredentialNamespace:    "ae-credentials",
	}
	c := egressSidecarContainer(cfg, "team-alpha", "team-alpha/support", "r:squad-a", "[]", true, true, nil, nil)

	rec, ok := envValue(c, "RECORD_CAPABLE")
	require.True(t, ok, "record-capable sidecar carries RECORD_CAPABLE")
	assert.Equal(t, "true", rec.Value)

	for _, name := range []string{"OBJECT_STORE_ADDR", "OBJECT_STORE_ACCESS_KEY", "OBJECT_STORE_SECRET_KEY"} {
		e, present := envValue(c, name)
		require.True(t, present, "record-capable sidecar carries %s (the fixture sink)", name)
		assert.NotEmpty(t, e.Value, name)
		assert.Nil(t, e.ValueFrom, "%s must be a static value (Knative rejects valueFrom)", name)
	}
}

// TestEgressSidecarContainer_RecordCapableNoDevDataPlane proves OPS-2: a record-capable sidecar
// rendered with the dev data plane OFF (a production install) carries RECORD_CAPABLE but NO
// object-store creds — the dev.local fixture sink is never injected into production, so the sidecar
// C2-fails-closed on the absent store rather than pointing at a non-existent dev MinIO.
func TestEgressSidecarContainer_RecordCapableNoDevDataPlane(t *testing.T) {
	cfg := OBOEgressConfig{
		SidecarImage:           "egress-sidecar:test",
		CapabilityPublicKeyB64: "PUBKEY",
		CapabilityAudience:     "aud",
		CredentialNamespace:    "ae-credentials",
	}
	c := egressSidecarContainer(cfg, "team-alpha", "team-alpha/support", "r:squad-a", "[]", true, false, nil, nil)

	rec, ok := envValue(c, "RECORD_CAPABLE")
	require.True(t, ok, "record-capable sidecar still carries RECORD_CAPABLE")
	assert.Equal(t, "true", rec.Value)

	for _, name := range []string{"OBJECT_STORE_ADDR", "OBJECT_STORE_ACCESS_KEY", "OBJECT_STORE_SECRET_KEY"} {
		_, present := envValue(c, name)
		assert.False(t, present, "dev data plane OFF ⇒ %s must NOT be injected (OPS-2)", name)
	}
}

// TestEgressSidecarContainer_ToolPolicyMount: when the controller passes a tool-policy mount + env
// (M82, ADR 0074 §1), the sidecar container carries TOOL_POLICY_FILE (static, never valueFrom) and
// the read-only policy volume mount — the DELIVERY plumbing (permissive; not yet enforced).
func TestEgressSidecarContainer_ToolPolicyMount(t *testing.T) {
	cfg := OBOEgressConfig{
		SidecarImage:           "egress-sidecar:test",
		CapabilityPublicKeyB64: "PUBKEY",
		CapabilityAudience:     "aud",
		CredentialNamespace:    "ae-credentials",
	}
	mount := &corev1.VolumeMount{Name: toolPolicyVolumeName, MountPath: toolPolicyMountPath, ReadOnly: true}
	env := []corev1.EnvVar{{Name: envToolPolicyFile, Value: toolPolicyMountPath + "/" + toolPolicyConfigMapKey}}
	c := egressSidecarContainer(cfg, "team-alpha", "team-alpha/support", "r:squad-a", "[]", false, false, mount, env)

	e, ok := envValue(c, envToolPolicyFile)
	require.True(t, ok, "a tool-policy agent's sidecar carries TOOL_POLICY_FILE")
	assert.Equal(t, toolPolicyMountPath+"/"+toolPolicyConfigMapKey, e.Value)
	assert.Nil(t, e.ValueFrom, "TOOL_POLICY_FILE must be static (Knative rejects valueFrom)")

	require.Len(t, c.VolumeMounts, 1, "the policy volume is mounted on the sidecar")
	assert.Equal(t, toolPolicyVolumeName, c.VolumeMounts[0].Name)
	assert.Equal(t, toolPolicyMountPath, c.VolumeMounts[0].MountPath)
	assert.True(t, c.VolumeMounts[0].ReadOnly, "the policy mount is read-only")
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
	assert.Empty(t, egressDigest("img", "r:squad-a", nil, false), "no routes ⇒ no digest (pod template unchanged)")

	base := egressDigest("img", "r:squad-a", routes, false)
	assert.NotEmpty(t, base)
	// A route URL change (the real URL lives in the sidecar env now) rolls the pod.
	assert.NotEqual(t, base, egressDigest("img", "r:squad-a", []toolmanifest.Route{{Name: "scalekit", TargetURL: "https://b", OAuth: true}}, false))
	// A sidecar image change rolls the pod.
	assert.NotEqual(t, base, egressDigest("img2", "r:squad-a", routes, false))
	// A boundary (registry membership) change rolls the pod — the EGRESS_BOUNDARY env must land.
	assert.NotEqual(t, base, egressDigest("img", "r:squad-b", routes, false))
	// Toggling record mode rolls the pod — the RECORD_CAPABLE + object-store env must land.
	assert.NotEqual(t, base, egressDigest("img", "r:squad-a", routes, true))
}
