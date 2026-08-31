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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

// The EGRESS REDIRECT (M142.4, ADR 0123 / m52.J8) — making plain-remote tool deny structural.
//
// M82 wire-denies a plain-remote tool at the egress sidecar, but Kubernetes NetworkPolicy is POD-scoped,
// not container-scoped: the agent container can reach every destination the sidecar can, so a custom loop
// in user code can dial a denied tool's real URL itself. URL secrecy plus default-deny egress is the v1
// floor; it is not a boundary.
//
// This installs the Istio pattern — an initContainer that writes netfilter rules redirecting the pod's
// outbound TCP into the sidecar — so the deny stops depending on the agent not trying.
//
// Two properties make it safe to put NET_ADMIN in the pod at all:
//
//   - the capability lives ONLY on the initContainer. The agent container never gets it, so code running
//     there cannot remove the rules it is bound by. An attacker with execution in the agent container
//     inherits the redirect rather than the ability to lift it.
//   - the initContainer runs to completion before the agent starts, so there is no window in which the
//     agent is running without the rules.
//
// The sidecar's OWN egress is exempted by UID, not by port or destination: it is the one process that
// legitimately talks to the real tool URL, and it is ours, so we pin its UID and let netfilter recognise
// it. Everything else in the pod — including the launcher — is redirected unless its destination is
// explicitly excluded.

const (
	// capabilityAll is the wildcard every hardened container drops before adding back only what it needs.
	capabilityAll corev1.Capability = "ALL"

	// egressRedirectInitName is the initContainer that installs the rules.
	egressRedirectInitName = "egress-redirect-init"

	// egressSidecarUID is the UID the egress sidecar image runs as (Dockerfile.egress-sidecar:
	// `USER 65532:65532`). Netfilter exempts traffic owned by this UID so the sidecar's forwarded calls
	// reach the real tool instead of being redirected back into itself — an infinite loop, and the reason
	// a UID exemption is required rather than optional.
	egressSidecarUID = 65532

	// egressRedirectPort is where redirected traffic lands: the sidecar's listener.
	egressRedirectPort = 8899
)

// EgressRedirectConfig configures the L4 redirect (ADR 0123).
type EgressRedirectConfig struct {
	// Enabled turns the redirect on. DEFAULT OFF: switching every agent pod's networking is the class of
	// change M128/M134 established must ship as a proven mechanism first and be flipped deliberately
	// after, not defaulted on in the release that introduces it.
	Enabled bool
	// Image is the initContainer image, which must carry an iptables binary (the sidecar image is
	// distroless and deliberately has none). Empty ⇒ the redirect is skipped even when Enabled.
	Image string
	// ExcludeCIDRs are destinations that must NOT be redirected — the in-cluster control plane the
	// launcher itself depends on (the BFF spawn/discovery edges, the model gateway, the state-layer
	// proxy, the token service) plus Knative's own queue-proxy path. Redirecting those would not harden
	// anything; it would sever the pod from the platform that runs it.
	ExcludeCIDRs []string
}

// egressRedirectRules is the netfilter program, in order. It is a string constant rather than assembled
// at runtime so the exact ruleset is reviewable in one place and asserted verbatim by tests.
//
//	-t nat -N CTXMESH_OUT              a chain of our own; never edit the built-ins in place
//	-A OUTPUT -p tcp -j CTXMESH_OUT    all outbound TCP enters it
//	--uid-owner <sidecar> -j RETURN    the sidecar's own calls leave the pod untouched
//	-o lo -j RETURN                    in-pod loopback (launcher→sidecar, queue-proxy→app) is untouched
//	-d <excluded> -j RETURN            the control plane the launcher must reach
//	-j REDIRECT --to-ports <port>      everything else is bent into the sidecar
//
// The final REDIRECT is what makes deny structural: a request the sidecar has no route for is refused
// there, and a TLS connection to a host the sidecar is not fails its handshake. Either way the agent does
// not reach the destination.
func egressRedirectRules(excludeCIDRs []string) []string {
	rules := []string{
		"iptables -t nat -N CTXMESH_OUT",
		"iptables -t nat -A OUTPUT -p tcp -j CTXMESH_OUT",
		fmt.Sprintf("iptables -t nat -A CTXMESH_OUT -m owner --uid-owner %d -j RETURN", egressSidecarUID),
		"iptables -t nat -A CTXMESH_OUT -o lo -j RETURN",
	}
	for _, cidr := range excludeCIDRs {
		if c := strings.TrimSpace(cidr); c != "" {
			rules = append(rules, fmt.Sprintf("iptables -t nat -A CTXMESH_OUT -d %s -j RETURN", c))
		}
	}
	return append(rules,
		fmt.Sprintf("iptables -t nat -A CTXMESH_OUT -p tcp -j REDIRECT --to-ports %d", egressRedirectPort))
}

// egressRedirectInitContainer builds the initContainer that installs the rules.
//
// `set -e` matters more than it looks: if a rule fails to apply, the initContainer must FAIL and the pod
// must not start. A partially-installed ruleset is worse than none — it would look enforced while leaving
// a hole, which is the precise failure mode this whole milestone exists to remove.
func egressRedirectInitContainer(cfg EgressRedirectConfig) corev1.Container {
	script := "set -e\n" + strings.Join(egressRedirectRules(cfg.ExcludeCIDRs), "\n") + "\n"
	return corev1.Container{
		Name:    egressRedirectInitName,
		Image:   cfg.Image,
		Command: []string{"/bin/sh", "-c", script},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			// NET_ADMIN + NET_RAW are the minimum for writing nat rules. Root is required for the same
			// reason; it is bounded to this initContainer, which exits before the agent starts.
			RunAsNonRoot:             ptr.To(false),
			RunAsUser:                ptr.To(int64(0)),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{capabilityAll},
				Add:  []corev1.Capability{"NET_ADMIN", "NET_RAW"},
			},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
}

// egressRedirectRequiredKnativeFlags are the Knative feature flags the redirect needs on the ksvc pod
// template, ON TOP of the launcher-injection set. `containerspec-addcapabilities` is the one that gates
// NET_ADMIN and is disabled by default; without it Knative REJECTS the ksvc outright, so the controller
// must check before injecting — the C8c lesson, where a misconfiguration would otherwise reject every
// ksvc into a self-inflicted fleet outage.
var egressRedirectRequiredKnativeFlags = []string{
	"kubernetes.podspec-init-containers",
	"kubernetes.podspec-securitycontext",
	"kubernetes.containerspec-addcapabilities",
}

// egressRedirectReady reports whether the redirect is BOTH configured and safe to apply. Fail-safe: any
// missing piece skips injection (agents run as they do today, with the sidecar's wire-deny as the floor)
// and the caller logs loudly, rather than rejecting ksvcs.
func (r *AgentDeploymentReconciler) egressRedirectReady(flagsEnabled func([]string) bool) bool {
	if !r.EgressRedirect.Enabled || strings.TrimSpace(r.EgressRedirect.Image) == "" {
		return false
	}
	return flagsEnabled(egressRedirectRequiredKnativeFlags)
}
