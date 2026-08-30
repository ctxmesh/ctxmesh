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
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

// approvalPolicyResolveError is raised when spec.approvalPolicyRef is set but the policy cannot be
// resolved (dangling ref) — the agent is held NotReady rather than served WITHOUT the declared approval
// gate (fail-closed, ADR 0111 §3; mirrors guardrailResolveError). Reconcile intercepts it.
type approvalPolicyResolveError struct {
	reason string
	msg    string
}

func (e *approvalPolicyResolveError) Error() string { return e.msg }

// asApprovalPolicyResolveError extracts an *approvalPolicyResolveError from an error chain.
func asApprovalPolicyResolveError(err error) (*approvalPolicyResolveError, bool) {
	var ae *approvalPolicyResolveError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// reasonApprovalPolicyNotFound is the Ready=False reason for a dangling approvalPolicyRef.
const reasonApprovalPolicyNotFound = "ApprovalPolicyNotFound"

// resolveApprovalPolicy fetches the agent's spec.approvalPolicyRef (M139, ADR 0111). Nil ref ⇒ (nil, nil)
// (no approval gate). A dangling ref ⇒ a fail-closed approvalPolicyResolveError (the agent is held
// NotReady, never served without the declared gate). Any other Get error is returned as-is.
func resolveApprovalPolicy(ctx context.Context, c client.Client, deploy *agentsv1alpha1.AgentDeployment) (*agentsv1beta1.ApprovalPolicy, error) {
	ref := deploy.Spec.ApprovalPolicyRef
	if ref == "" {
		return nil, nil
	}
	var policy agentsv1beta1.ApprovalPolicy
	if err := c.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: ref}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &approvalPolicyResolveError{
				reason: reasonApprovalPolicyNotFound,
				msg: fmt.Sprintf("approvalPolicyRef %q not found in namespace %q; the agent is held NotReady "+
					"rather than served without its declared approval gate", ref, deploy.Namespace),
			}
		}
		return nil, fmt.Errorf("fetching approvalPolicyRef %q: %w", ref, err)
	}
	return &policy, nil
}

// The normalized tool-policy rules (matches the CRD enum, lower-cased). These are the SAME rule
// strings the egress sidecar's ToolPolicy.RuleFor returns (internal/egress/policy.go) — the
// controller resolves the effective rule for an in-pod tool with identical semantics so structural
// non-deployment (here) and wire enforcement (the sidecar) never disagree on what a tool's rule is.
const (
	toolRuleAllow           = "allow"
	toolRuleDeny            = "deny"
	toolRuleRequireApproval = "require-approval"
)

// Tool-call governance (M82, ADR 0074 §1): the SAME resolved spec.runtime.toolPolicy the SDK
// receives (folded into AGENT_RUNTIME) is ALSO delivered to the egress sidecar — the authoritative
// tool-call chokepoint — as a mounted, controller-owned ConfigMap the sidecar reads + fsnotify-
// watches (the M81-K3 pattern, mirrored from reconcileGuardrailConfigMap). This task lands the
// PLUMBING only: the policy is delivered, parsed, and held; ENFORCEMENT (deny 403 / require-approval
// voucher / fan-out ceiling) is a later M82 task. An absent/empty policy ⇒ no policy (permissive).
const (
	// envToolPolicyFile is the STATIC env var carrying the in-container PATH to the mounted
	// tool-policy file on the egress sidecar. Its presence is the source the sidecar reads +
	// fsnotify-watches. NEVER valueFrom — the m5.7 Knative ksvc landmine (the webhook rejects
	// valueFrom in a ksvc pod template); the VALUE is a static path.
	envToolPolicyFile = "TOOL_POLICY_FILE"

	// toolPolicyConfigMapSuffix names the per-agent, STABLE-named ConfigMap that materialises the
	// resolved toolPolicy JSON (<agent>-toolpolicy). STABLE (not content-addressed) is the point of
	// the K3 pattern: a policy edit UPDATES this same ConfigMap IN PLACE, so the mounted file changes
	// and the sidecar reloads (fsnotify) WITHOUT a revision roll.
	toolPolicyConfigMapSuffix = "-toolpolicy"

	// toolPolicyConfigMapKey is the data key inside the <agent>-toolpolicy ConfigMap.
	toolPolicyConfigMapKey = "policy.json"

	// toolPolicyMountPath is where the resolved policy file is mounted in the EGRESS SIDECAR
	// container. The sidecar reads TOOL_POLICY_FILE (this path + key) and watches the directory.
	toolPolicyMountPath = "/etc/egress/toolpolicy"

	// toolPolicyVolumeName is the pod volume name for the mounted tool-policy ConfigMap.
	toolPolicyVolumeName = "tool-policy"
)

// toolPolicyConfigMapName returns the STABLE per-agent tool-policy ConfigMap name
// (<agent>-toolpolicy). Stable-named on purpose (the K3 pattern): editing spec.runtime.toolPolicy
// re-reconciles the agent, which UPDATES this ConfigMap in place rather than minting a new one, so
// the mounted file changes and the sidecar reloads without rolling the revision.
func toolPolicyConfigMapName(agentName string) string {
	return agentName + toolPolicyConfigMapSuffix
}

// resolvedToolPolicy is the outcome of resolving spec.runtime.toolPolicy for the egress sidecar.
type resolvedToolPolicy struct {
	// referenced is true when spec.runtime.toolPolicy is set. When false, no tool-policy env is
	// injected and no ConfigMap is created (the permissive default, byte-compatible pre-M82).
	referenced bool
	// policyJSON is the ToolPolicySpec serialized to JSON, mounted for the sidecar. Empty when not
	// referenced.
	policyJSON string
	// spec is the resolved ToolPolicySpec (nil when not referenced). It backs ruleFor so the
	// controller can decide the STRUCTURAL treatment of an in-pod tool (deny ⇒ non-deployment;
	// require-approval ⇒ reject) with the SAME first-match-wins semantics the sidecar uses on the
	// wire (M82.3, ADR 0074 §2).
	spec *agentsv1alpha1.ToolPolicySpec
}

// ruleFor returns the effective rule for a tool name: the first matching override's rule, else the
// policy default (empty default ⇒ "allow"). This MIRRORS internal/egress/policy.go's
// ToolPolicy.RuleFor byte-for-byte (first match wins, empty ⇒ allow) so the structural decision the
// controller makes for an in-pod tool (M82.3) and the wire decision the sidecar makes for an
// OBO/remote tool (M82.2) are judged against the SAME policy. The toolName argument is the catalog/
// wire tool name (SidecarTool.ToolName = binding.ToolName = the sidecar's params.name key), so a
// per-tool override keyed on that name matches identically in both places. A nil/not-referenced
// policy ⇒ allow (permissive, pre-M82 behaviour).
func (rp resolvedToolPolicy) ruleFor(toolName string) string {
	if !rp.referenced || rp.spec == nil {
		return toolRuleAllow
	}
	for i := range rp.spec.Overrides {
		if rp.spec.Overrides[i].Name == toolName {
			return normalizeToolRule(rp.spec.Overrides[i].Rule)
		}
	}
	return normalizeToolRule(rp.spec.Default)
}

// normalizeToolRule maps an empty rule to the CRD default ("allow") and lower-cases for a stable
// comparison (mirrors egress/policy.go's normalizeRule). The CRD enum already bounds valid input, so
// an unrecognized value is returned verbatim (defensive only; it compares non-allow ⇒ fail-safe).
func normalizeToolRule(rule string) string {
	r := strings.ToLower(strings.TrimSpace(rule))
	if r == "" {
		return toolRuleAllow
	}
	return r
}

// inPodRequireApprovalError is raised when spec.runtime.toolPolicy's effective rule for an IN-POD
// (sidecar-mode) tool is `require-approval` (M82.3, ADR 0074 §2). An in-pod tool binds a deterministic
// localhost port in the shared pod netns, so it CANNOT be gated by the m82.4 approval-voucher (there
// is no wire chokepoint in front of a localhost-bypassable call, and no platform tool-shim) — a
// half-governed pod would silently let the require-approval tool through. Rather than deploy that,
// buildPodTemplate returns this typed error BEFORE any workload write; Reconcile intercepts it, sets
// Ready=False, and STOPS cleanly (no requeue on user input), exactly like guardrailResolveError. The
// fix is a spec edit (drop the rule, or make the tool remote/OBO where require-approval IS enforceable).
type inPodRequireApprovalError struct {
	reason string
	msg    string
}

func (e *inPodRequireApprovalError) Error() string { return e.msg }

// asInPodRequireApprovalError extracts an *inPodRequireApprovalError from an error chain.
func asInPodRequireApprovalError(err error) (*inPodRequireApprovalError, bool) {
	var ie *inPodRequireApprovalError
	if errors.As(err, &ie) {
		return ie, true
	}
	return nil, false
}

// reasonInPodToolRequireApprovalUnsupported is the Ready=False reason set when an in-pod tool carries
// an effective require-approval rule (ADR 0074 §2 — deny-only-governable in-pod).
const reasonInPodToolRequireApprovalUnsupported = "InPodToolRequireApprovalUnsupported"

// resolveToolPolicy resolves the agent's spec.runtime.toolPolicy (M82, ADR 0074 §1). Unlike the
// guardrail path this has NO fail-closed validation surface (the CRD's enum + CEL already bound the
// shape; there are no operator-authored RE2 patterns to compile), so it never fails the reconcile:
// it simply serializes the SAME ToolPolicySpec the SDK receives (via AGENT_RUNTIME) for delivery to
// the sidecar. Nil runtime / nil toolPolicy ⇒ not referenced (permissive, no ConfigMap).
func resolveToolPolicy(deploy *agentsv1alpha1.AgentDeployment, approvalPolicy *agentsv1beta1.ApprovalPolicy) (resolvedToolPolicy, error) {
	var base *agentsv1alpha1.ToolPolicySpec
	if rt := deploy.Spec.Runtime; rt != nil {
		base = rt.ToolPolicy
	}
	// Fold the ApprovalPolicy's require-approval requirements into the EFFECTIVE tool policy (M139, ADR
	// 0111 §3), max-strictness. THE NIL-TRAP: a ref-only agent (an ApprovalPolicy but no inline toolPolicy)
	// MUST still get a real, restrictive policy — merging fires even when base is nil, so the sidecar is
	// never left unrestricted.
	effective := mergeApprovalPolicy(base, approvalPolicy)
	if effective == nil {
		return resolvedToolPolicy{}, nil // no inline toolPolicy AND no approval requirements → permissive
	}
	b, err := json.Marshal(effective)
	if err != nil {
		return resolvedToolPolicy{}, fmt.Errorf("marshaling the effective toolPolicy: %w", err)
	}
	return resolvedToolPolicy{referenced: true, policyJSON: string(b), spec: effective}, nil
}

// toolRuleStrictness orders the tool rules by how RESTRICTIVE they are (ADR 0111 §3):
// allow(0) < require-approval(1) < deny(2). Used to merge an ApprovalPolicy monotonically.
func toolRuleStrictness(rule string) int {
	switch normalizeToolRule(rule) {
	case toolRuleDeny:
		return 2
	case toolRuleRequireApproval:
		return 1
	default:
		return 0 // allow (or an unrecognized value → treated as the weakest, so the merge only tightens)
	}
}

// maxStrictnessRule returns whichever of a, b is the more restrictive (ADR 0111 §3).
func maxStrictnessRule(a, b string) string {
	if toolRuleStrictness(a) >= toolRuleStrictness(b) {
		return normalizeToolRule(a)
	}
	return normalizeToolRule(b)
}

// mergeApprovalPolicy folds an ApprovalPolicy's require-approval requirements into a base ToolPolicySpec
// under MAX-STRICTNESS (ADR 0111 §3): the policy can only TIGHTEN — an inline allow never defeats a
// policy's require-approval, and an inline deny stays deny (deny already exceeds "at least approval"). All
// other base fields (forcedChoice, parallelLimit, maxToolCallsPerRun) are preserved. Returns base
// unchanged when the policy adds no requirements; returns a fresh spec (never nil) when it does — so a
// ref-only agent gets a real restrictive policy (the nil-trap fix).
func mergeApprovalPolicy(base *agentsv1alpha1.ToolPolicySpec, ap *agentsv1beta1.ApprovalPolicy) *agentsv1alpha1.ToolPolicySpec {
	if ap == nil || len(ap.Spec.Rules) == 0 {
		return base
	}
	merged := &agentsv1alpha1.ToolPolicySpec{}
	if base != nil {
		merged = base.DeepCopy()
	}
	allTools := false
	tools := map[string]bool{}
	for i := range ap.Spec.Rules {
		if ap.Spec.Rules[i].AllTools {
			allTools = true
		}
		for _, t := range ap.Spec.Rules[i].Tools {
			tools[t] = true
		}
	}
	// effective rule for a tool in `merged` (existing override, else the default).
	ruleForTool := func(name string) string {
		for i := range merged.Overrides {
			if merged.Overrides[i].Name == name {
				return normalizeToolRule(merged.Overrides[i].Rule)
			}
		}
		return normalizeToolRule(merged.Default)
	}
	setRule := func(name, rule string) {
		for i := range merged.Overrides {
			if merged.Overrides[i].Name == name {
				merged.Overrides[i].Rule = rule
				return
			}
		}
		merged.Overrides = append(merged.Overrides, agentsv1alpha1.ToolPolicyOverride{Name: name, Rule: rule})
	}
	if allTools {
		// Tighten the default AND every existing override (an explicit allow must also become approval).
		merged.Default = maxStrictnessRule(merged.Default, toolRuleRequireApproval)
		for i := range merged.Overrides {
			merged.Overrides[i].Rule = maxStrictnessRule(merged.Overrides[i].Rule, toolRuleRequireApproval)
		}
	}
	for t := range tools {
		setRule(t, maxStrictnessRule(ruleForTool(t), toolRuleRequireApproval))
	}
	return merged
}

// reconcileToolPolicyConfigMap materialises the resolved toolPolicy JSON into the per-agent,
// STABLE-named <agent>-toolpolicy ConfigMap (owner-ref'd so it GCs with the AgentDeployment,
// mirroring reconcileGuardrailConfigMap) and returns the volume + mount + static env the EGRESS
// SIDECAR container needs (ADR 0074 §1 / §6). It is a no-op returning zero values when the agent has
// no toolPolicy — the permissive default.
//
// SECURITY-CRITICAL (ADR 0074 §6a): the controller OWNS + reconciles this ConfigMap so a
// namespace-level edit cannot silently flip tool policy out from under the CR — the owner-ref +
// CreateOrUpdate reverts drift, exactly the M81-K3 pattern. The policy is delivered as a mounted
// file (not env) so the sidecar can WATCH it; the env carries only the static FILE PATH (no
// valueFrom — the m5.7 Knative landmine).
func (r *AgentDeploymentReconciler) reconcileToolPolicyConfigMap(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	tp resolvedToolPolicy,
) (vol *corev1.Volume, mount *corev1.VolumeMount, env []corev1.EnvVar, err error) {
	if !tp.referenced {
		return nil, nil, nil, nil
	}

	cmName := toolPolicyConfigMapName(deploy.Name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: deploy.Namespace},
	}
	if _, err = ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[toolPolicyConfigMapKey] = tp.policyJSON
		return ctrl.SetControllerReference(deploy, cm, r.Scheme)
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("upserting tool-policy ConfigMap: %w", err)
	}

	v := corev1.Volume{
		Name: toolPolicyVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
			},
		},
	}
	m := corev1.VolumeMount{Name: toolPolicyVolumeName, MountPath: toolPolicyMountPath, ReadOnly: true}
	e := []corev1.EnvVar{
		{Name: envToolPolicyFile, Value: toolPolicyMountPath + "/" + toolPolicyConfigMapKey},
	}
	return &v, &m, e, nil
}

// toolPolicyPresenceDigest is the PRESENCE-ONLY tool-policy component of the pod-template digest
// (ADR 0074 §1, mirroring guardrailPresenceDigest). It returns a fixed non-empty 8-hex token; the
// caller folds it in ONLY when a toolPolicy is present, so ADDING or REMOVING the policy rolls the
// revision (the mounted volume + TOOL_POLICY_FILE env appear/disappear), while EDITING an
// already-present policy's CONTENT keeps the SAME token → the SAME revision name → NO roll (the
// content rides the watched, mounted ConfigMap the sidecar reloads live — the K3 point).
func toolPolicyPresenceDigest() string {
	h := sha256.Sum256([]byte("toolpolicy:referenced"))
	return fmt.Sprintf("%x", h[:])[:8]
}

// filterInPodToolsByPolicy applies the STRUCTURAL in-pod tool-call governance (M82.3, ADR 0074 §2)
// to a binding set BEFORE toolmanifest.Render is called. It returns the bindings that should be
// rendered — every OBO/remote binding verbatim (their deny/require-approval is the m82.2 WIRE
// enforcement, since they are genuinely fronted by the sidecar) plus every sidecar-mode binding whose
// effective rule is NOT deny. A denied in-pod binding is DROPPED here, so Render produces neither its
// manifest entry, its sidecar container, nor (via RewriteAllForEgress on the same rendered manifest)
// its egress route: the tool is GONE — no container, not reachable, not advertised. That is stronger
// than any wire check, which a localhost hop would bypass.
//
// Filtering BEFORE Render is what keeps the port↔manifest↔container mapping consistent (the footgun):
// Render assigns localhost ports 3001, 3002… in binding-name order over the slice it is GIVEN, and
// the caller derives BOTH the manifest endpoints and the sidecar container ports from that one Render
// output — so a denied MIDDLE tool simply isn't in the slice and the survivors renumber together,
// manifest and container in lockstep. There is no post-Render re-map to get wrong.
//
// A sidecar-mode binding whose effective rule is require-approval is INVALID: an in-pod tool can't do
// the m82.4 approval-voucher (no wire chokepoint in front of a localhost-bypassable call, no platform
// tool-shim), so half-governing it is worse than refusing. It returns an *inPodRequireApprovalError,
// which Reconcile surfaces as Ready=False (naming the tool) and STOPS — no pod is deployed.
//
// A pure-allow / policy-less agent (rp not referenced, or every in-pod tool effectively allow) keeps
// EVERY binding, so Render's output is byte-for-byte identical to pre-M82.3. OBO/remote bindings are
// never inspected here.
func filterInPodToolsByPolicy(bindings []toolmanifest.Binding, rp resolvedToolPolicy) ([]toolmanifest.Binding, error) {
	if !rp.referenced {
		return bindings, nil
	}
	kept := make([]toolmanifest.Binding, 0, len(bindings))
	for _, b := range bindings {
		if b.Mode != toolmanifest.ModeSidecar {
			// OBO/remote: wire-enforced at the sidecar (m82.2), never structurally filtered here.
			kept = append(kept, b)
			continue
		}
		switch rp.ruleFor(b.ToolName) {
		case toolRuleDeny:
			// Structural non-deployment: drop the binding so it is rendered nowhere.
			continue
		case toolRuleRequireApproval:
			return nil, &inPodRequireApprovalError{
				reason: reasonInPodToolRequireApprovalUnsupported,
				msg: fmt.Sprintf(
					"in-pod (sidecar-mode) tool %q has toolPolicy rule require-approval, which is unsupported: "+
						"a sidecar tool binds a localhost port in the shared pod netns and cannot be gated by the "+
						"approval voucher. Use rule allow or deny for in-pod tools, or make the tool remote/OBO.",
					b.ToolName),
			}
		default: // allow (or an unrecognized rule, which is not deny → deploy)
			kept = append(kept, b)
		}
	}
	return kept, nil
}

// dropUngovernableInPodTools is the NON-erroring sibling of filterInPodToolsByPolicy used by the
// MCPToolBinding reconciler when it renders the advertised <agent>-tools manifest (tools.json). The
// two reconcilers MUST derive the manifest from the SAME post-policy binding set or the SDK-facing
// manifest drifts from the pod template the AgentDeployment reconciler injects (the drift the M82
// front-all comment warns about). It drops EVERY in-pod tool that is NOT effectively allow — both
// deny (structural non-deployment) AND require-approval (which makes the AgentDeployment go
// Ready=False, so no pod exists to read this manifest anyway): an in-pod tool the pod won't or can't
// run must never be advertised. Unlike the AgentDeployment path this NEVER errors — the binding
// reconciler only reflects the pod-template reality; the authoritative require-approval REJECTION is
// the AgentDeployment's Ready=False. OBO/remote bindings pass through verbatim (wire-enforced). A
// not-referenced policy keeps every binding (byte-for-byte pre-M82.3).
func dropUngovernableInPodTools(bindings []toolmanifest.Binding, rp resolvedToolPolicy) []toolmanifest.Binding {
	if !rp.referenced {
		return bindings
	}
	kept := make([]toolmanifest.Binding, 0, len(bindings))
	for _, b := range bindings {
		if b.Mode != toolmanifest.ModeSidecar {
			kept = append(kept, b)
			continue
		}
		switch rp.ruleFor(b.ToolName) {
		case toolRuleDeny, toolRuleRequireApproval:
			// Not advertised: no container will run it (deny) / the pod won't exist (require-approval).
			continue
		default: // allow (or an unrecognized rule, which is not deny/require-approval → deployable)
			kept = append(kept, b)
		}
	}
	return kept
}
