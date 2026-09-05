package controller

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func redirectScript(t *testing.T, cfg EgressRedirectConfig) string {
	t.Helper()
	c := egressRedirectInitContainer(cfg)
	require.Len(t, c.Command, 3, "the initContainer runs a shell script")
	return c.Command[2]
}

// The ruleset is ORDERED, and the order is the whole design: the sidecar's own traffic and the in-pod /
// control-plane destinations must RETURN before the catch-all REDIRECT, or the pod severs itself from the
// platform (and the sidecar redirects into itself, forever).
func TestEgressRedirect_RuleOrderExemptsBeforeRedirecting(t *testing.T) {
	rules := egressRedirectRules([]string{"10.96.0.0/16"})

	idx := func(substr string) int {
		for i, r := range rules {
			if strings.Contains(r, substr) {
				return i
			}
		}
		return -1
	}
	require.NotEqual(t, -1, idx("--uid-owner"), "the sidecar's own egress must be exempted")
	require.NotEqual(t, -1, idx("-o lo"), "in-pod loopback must be exempted")
	require.NotEqual(t, -1, idx("10.96.0.0/16"), "the excluded CIDR must be present")
	redirect := idx("REDIRECT")
	require.NotEqual(t, -1, redirect)

	assert.Less(t, idx("--uid-owner"), redirect, "the sidecar exemption must precede the catch-all")
	assert.Less(t, idx("-o lo"), redirect, "loopback must precede the catch-all")
	assert.Less(t, idx("10.96.0.0/16"), redirect, "excluded destinations must precede the catch-all")
	// REDIRECT is the last rule IN THE NAT TABLE — everything unexempted falls into it. The UDP
	// rules that follow live in the filter table and cannot catch TCP, so they do not weaken this.
	natRules := 0
	for _, r := range rules {
		if strings.Contains(r, "-t nat") {
			natRules++
		}
	}
	assert.Equal(t, natRules-1, redirect, "REDIRECT is the last NAT rule — everything unexempted falls into it")
}

// The sidecar is exempted by its UID, which must match the UID its image actually runs as
// (Dockerfile.egress-sidecar). Exempting the wrong UID would loop the sidecar's own forwarded call
// straight back into itself.
//
// Asserted against the CONSTANT, not a literal. The literal is how this drifted: the exemption sat
// on 65532 — the standard distroless `nonroot` uid — while the agent's own container uid is
// deliberately unconstrained, so any agent image following that near-universal convention was
// exempt from egress control entirely and still passed every check.
func TestEgressRedirect_ExemptsTheSidecarByItsRealUID(t *testing.T) {
	rules := egressRedirectRules(nil)
	assert.Contains(t, strings.Join(rules, "\n"), fmt.Sprintf("--uid-owner %d", egressSidecarUID),
		"the exempted UID must be the one Dockerfile.egress-sidecar runs as")
}

// The exempted UID must NOT be a uid a base image is likely to pick, because anything running as it
// bypasses the redirect silently — egress control simply stops applying, with no error anywhere.
func TestEgressRedirect_ExemptedUIDIsNotACommonConvention(t *testing.T) {
	assert.NotEqual(t, distrolessNonRootUID, egressSidecarUID,
		"65532 is the distroless nonroot default; an agent image using it would bypass the redirect")
	for _, common := range []int{0, 1000, 1001, 999, 100} {
		assert.NotEqual(t, common, egressSidecarUID, "uid %d is a common base-image default", common)
	}
}

// UDP is DENIED rather than redirected, because there is nothing to redirect it into — the sidecar
// speaks HTTP. Leaving it open left two real holes: QUIC (HTTP/3 on UDP 443) bypasses the TCP
// redirect completely, and arbitrary UDP is an exfiltration channel that never touches the sidecar.
// DNS to the cluster resolver is kept, because the pod genuinely needs it.
func TestEgressRedirect_DeniesUDPExceptDNS(t *testing.T) {
	joined := strings.Join(egressRedirectRules(nil), "\n")
	assert.Contains(t, joined, "-A CTXMESH_UDP -p udp --dport 53 -j RETURN", "DNS must still work")
	assert.Contains(t, joined, "-A CTXMESH_UDP -j DROP", "everything else UDP is denied")

	udp := egressRedirectRules(nil)
	dnsIdx, dropIdx := -1, -1
	for i, r := range udp {
		if strings.Contains(r, "--dport 53") {
			dnsIdx = i
		}
		if strings.Contains(r, "CTXMESH_UDP -j DROP") {
			dropIdx = i
		}
	}
	require.NotEqual(t, -1, dnsIdx)
	require.NotEqual(t, -1, dropIdx)
	assert.Less(t, dnsIdx, dropIdx, "the DNS exemption must precede the catch-all DROP or DNS breaks")
}

// Redirected traffic must land on the sidecar's actual listener port.
func TestEgressRedirect_RedirectsToTheSidecarPort(t *testing.T) {
	rules := egressRedirectRules(nil)
	joined := strings.Join(rules, "\n")
	assert.Contains(t, joined, "--to-ports 8899",
		"the redirect target is the egress sidecar's listen port")
	assert.Contains(t, egressSidecarListenAddr, "8899",
		"and that port is the one the sidecar is configured to bind — if these drift, egress black-holes")
}

// A partially-applied ruleset would look enforced while leaving a hole, so the script aborts on the first
// failure and the pod never starts.
func TestEgressRedirect_ScriptAbortsOnFirstFailure(t *testing.T) {
	script := redirectScript(t, EgressRedirectConfig{Image: "img", ExcludeCIDRs: []string{"10.96.0.0/16"}})
	assert.True(t, strings.HasPrefix(script, "set -e\n"),
		"a half-installed ruleset is worse than none — it must fail the pod, not proceed")
}

// NET_ADMIN lives ONLY on the initContainer. That is what makes the redirect binding rather than
// advisory: code running in the agent container inherits the rules and has no capability to remove them.
func TestEgressRedirect_CapabilityIsBoundedToTheInitContainer(t *testing.T) {
	c := egressRedirectInitContainer(EgressRedirectConfig{Image: "img"})

	require.NotNil(t, c.SecurityContext)
	require.NotNil(t, c.SecurityContext.Capabilities)
	assert.Equal(t, []corev1.Capability{capabilityAll}, c.SecurityContext.Capabilities.Drop)
	assert.Contains(t, c.SecurityContext.Capabilities.Add, corev1.Capability("NET_ADMIN"))
	assert.False(t, *c.SecurityContext.AllowPrivilegeEscalation,
		"root is needed to write nat rules; escalating beyond it is not")
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	assert.False(t, c.Resources.Requests.Cpu().IsZero(),
		"resource-bounded so a restricted/quota'd namespace still admits it")
}

// Every excluded CIDR gets its own RETURN — a destination the launcher depends on must not be swallowed.
func TestEgressRedirect_EveryExcludedCIDRGetsARule(t *testing.T) {
	rules := strings.Join(egressRedirectRules([]string{"10.96.0.0/16", " 10.244.0.0/16 ", ""}), "\n")
	assert.Contains(t, rules, "-d 10.96.0.0/16 -j RETURN")
	assert.Contains(t, rules, "-d 10.244.0.0/16 -j RETURN", "whitespace is trimmed, not treated as a CIDR")
	assert.NotContains(t, rules, "-d  -j RETURN", "an empty entry must not produce a malformed rule")
}

// Fail-safe gating: the redirect is skipped unless it is enabled, has an image, AND the Knative flags are
// on. Emitting a NET_ADMIN pod template without the capabilities flag would have Knative reject the ksvc
// — a fleet outage rather than a missing feature (the C8c lesson).
func TestEgressRedirect_ReadinessIsFailSafe(t *testing.T) {
	allFlagsOn := func([]string) bool { return true }
	flagsOff := func([]string) bool { return false }

	r := &AgentDeploymentReconciler{}
	assert.False(t, r.egressRedirectReady(allFlagsOn), "disabled by default")

	r.EgressRedirect = EgressRedirectConfig{Enabled: true}
	assert.False(t, r.egressRedirectReady(allFlagsOn), "enabled but no image ⇒ skipped")

	r.EgressRedirect = EgressRedirectConfig{Enabled: true, Image: "img"}
	assert.False(t, r.egressRedirectReady(flagsOff), "a missing Knative flag ⇒ skipped, never a rejected ksvc")
	assert.True(t, r.egressRedirectReady(allFlagsOn))
}

// The capabilities flag is what gates NET_ADMIN and is disabled by default, so it must be in the required
// set — checking only the launcher's flags would let the controller emit a template Knative rejects.
func TestEgressRedirect_RequiresTheAddCapabilitiesFlag(t *testing.T) {
	assert.Contains(t, egressRedirectRequiredKnativeFlags, "kubernetes.containerspec-addcapabilities")
	assert.Contains(t, egressRedirectRequiredKnativeFlags, "kubernetes.podspec-init-containers")
}

// The redirect only applies to a pod that HAS the sidecar — redirecting into an absent listener would
// black-hole the pod's egress instead of governing it.
func TestEgressRedirect_RequiresTheSidecarToBePresent(t *testing.T) {
	assert.False(t, hasEgressSidecar([]corev1.Container{{Name: userContainerName}}))
	assert.True(t, hasEgressSidecar([]corev1.Container{
		{Name: userContainerName}, {Name: egressSidecarContainerName},
	}))
}
