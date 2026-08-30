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
	"regexp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

// Guardrail control-plane fail-closed reasons (M66, ADR 0059 §8). Surfaced on the
// AgentDeployment Ready condition when a guardrailPolicyRef cannot be honored — the
// controller then refuses to serve the agent unguarded.
const (
	// reasonGuardrailPolicyNotFound: spec.guardrailPolicyRef points at a
	// GuardrailPolicy that does not exist in the agent's namespace (dangling ref).
	reasonGuardrailPolicyNotFound = "GuardrailPolicyNotFound"
	// reasonGuardrailPolicyInvalid: the referenced GuardrailPolicy exists but one of
	// its RE2 patterns (patternDenylist / piiDetectors.custom) fails to compile, so
	// the guardrail engine could not enforce it — the agent must not run unguarded.
	reasonGuardrailPolicyInvalid = "GuardrailPolicyInvalid"
)

// Pre-K3 note: the resolved GuardrailPolicy JSON used to ride the GUARDRAIL_POLICY env directly.
// K3 (ADR 0059 Fork-2) supersedes it with the MOUNTED file below (the launcher WATCHES it), so the
// controller no longer injects that env — a policy edit reloads without a revision roll. The
// integration test asserts the env's ABSENCE using the string literal directly.

const (
	// envGuardrailPolicyFile is the STATIC env var carrying the in-container PATH to the mounted
	// guardrail-policy file (K3). Its presence forces the launcher's :2996 model proxy on and is
	// the source the launcher reads + fsnotify-watches. NEVER valueFrom — the m5.7 Knative ksvc
	// landmine (the webhook rejects valueFrom in a ksvc pod template); the VALUE is a static path.
	envGuardrailPolicyFile = "GUARDRAIL_POLICY_FILE"

	// guardrailConfigMapSuffix names the per-agent, STABLE-named ConfigMap that materialises the
	// resolved GuardrailPolicy JSON (<agent>-guardrail). STABLE (not content-addressed like the
	// prompt CM) is the point of K3: a policy edit UPDATES this same ConfigMap IN PLACE, so the
	// mounted file changes and the launcher reloads — the revision name never changes, so no roll.
	guardrailConfigMapSuffix = "-guardrail"

	// guardrailConfigMapKey is the data key inside the <agent>-guardrail ConfigMap.
	guardrailConfigMapKey = "policy.json"

	// guardrailMountPath is where the resolved policy file is mounted in the user container. The
	// launcher reads GUARDRAIL_POLICY_FILE (this path + key) and watches the directory (fsnotify).
	guardrailMountPath = "/etc/agent/guardrail"

	// guardrailVolumeName is the pod volume name for the mounted guardrail ConfigMap.
	guardrailVolumeName = "agent-guardrail"
)

// guardrailConfigMapName returns the STABLE per-agent guardrail ConfigMap name (<agent>-guardrail).
// Stable-named on purpose (K3): editing the referenced GuardrailPolicy updates THIS ConfigMap in
// place rather than minting a new one, so the mounted file changes without rolling the revision.
func guardrailConfigMapName(agentName string) string {
	return agentName + guardrailConfigMapSuffix
}

// guardrailResolveError wraps a control-plane fail-closed guardrail failure (M66,
// ADR 0059 §8): a dangling ref (GuardrailPolicy not found) or an invalid ref (an RE2
// pattern does not compile). Mirrors promptResolveError: buildPodTemplate returns it
// BEFORE any workload write, so Reconcile intercepts it, sets Ready=False, and STOPS
// cleanly — the ksvc CreateOrUpdate is never reached, so the OLD revision (guarded, or
// nonexistent) keeps serving and the controller NEVER injects a "no-guardrail" config.
// This is the fail-closed invariant: a guarded agent with a broken policy does not
// serve traffic unguarded. Non-guardrailResolveError errors from resolveGuardrail are
// genuine infra failures (API read errors) and requeue normally.
type guardrailResolveError struct {
	reason string
	msg    string
}

func (e *guardrailResolveError) Error() string { return e.msg }

// asGuardrailResolveError extracts a *guardrailResolveError from an error chain.
func asGuardrailResolveError(err error) (*guardrailResolveError, bool) {
	var ge *guardrailResolveError
	if errors.As(err, &ge) {
		return ge, true
	}
	return nil, false
}

// resolvedGuardrail is the outcome of resolving spec.guardrailPolicyRef: whether a
// policy is referenced, its serialized spec (→ GUARDRAIL_POLICY env), and the digest
// component that folds into combinedBindingDigest so a policy edit rolls the revision.
type resolvedGuardrail struct {
	// referenced is true when spec.guardrailPolicyRef is set AND the policy resolved
	// and validated. When false, no guardrail env is injected and the digest is "".
	referenced bool
	// policyJSON is the GuardrailPolicySpec serialized to JSON, injected as
	// GUARDRAIL_POLICY. Empty when not referenced.
	policyJSON string
	// digest is the 8-hex component folded into combinedBindingDigest so a policy
	// change (or a referencing-agent reconcile) rolls the Knative revision. Empty when
	// not referenced (symmetric with the other digest components).
	digest string
}

// resolveGuardrail resolves + validates the agent's spec.guardrailPolicyRef (M66,
// ADR 0059 §8, m66.2). It returns:
//
//   - referenced=false, no error → no guardrailPolicyRef (unguarded path; the agent's
//     MODEL_GATEWAY_URL keeps pointing straight at LiteLLM, byte-compatible pre-M66).
//   - a *guardrailResolveError → CONTROL-PLANE FAIL-CLOSED: the ref is dangling
//     (policy not found) or invalid (an RE2 pattern doesn't compile). The caller sets
//     Ready=False and refuses to write a ksvc — the agent never serves unguarded.
//   - any other error → an infra read failure (requeue).
//
// Validation compiles EVERY patternDenylist[].pattern + piiDetectors.custom[].pattern
// via regexp.Compile (RE2). A single bad pattern fails the whole ref closed — a
// guardrail with a hole is not a guardrail. The digest folds the resolved policy spec
// so a policy edit (via the GuardrailPolicy watch) rolls the referencing agent's
// revision.
func resolveGuardrail(
	ctx context.Context,
	c client.Client,
	deploy *agentsv1alpha1.AgentDeployment,
) (resolvedGuardrail, error) {
	ref := deploy.Spec.GuardrailPolicyRef
	if ref == "" {
		return resolvedGuardrail{}, nil
	}

	var policy agentsv1beta1.GuardrailPolicy
	if err := c.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: ref}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			// Dangling ref → fail closed. The agent must NOT run unguarded.
			return resolvedGuardrail{}, &guardrailResolveError{
				reason: reasonGuardrailPolicyNotFound,
				msg: fmt.Sprintf("guardrailPolicyRef %q not found in namespace %q; "+
					"the agent is held NotReady rather than served unguarded", ref, deploy.Namespace),
			}
		}
		return resolvedGuardrail{}, fmt.Errorf("fetching guardrailPolicyRef %q: %w", ref, err)
	}

	// Validate: every RE2 pattern must compile. A hole in the guardrail is a fail-closed
	// event (ADR 0059 §8 — never a silent fallback-to-built-ins).
	if badName, badPattern, err := firstUncompilablePattern(&policy.Spec); err != nil {
		return resolvedGuardrail{}, &guardrailResolveError{
			reason: reasonGuardrailPolicyInvalid,
			msg: fmt.Sprintf("guardrailPolicy %q has an invalid RE2 pattern in %q (%q): %v; "+
				"the agent is held NotReady rather than served unguarded",
				ref, badName, badPattern, err),
		}
	}

	// Serialize the resolved policy spec for the launcher (GUARDRAIL_POLICY env) and derive
	// the digest from the SAME bytes so the injected config and the revision-roll trigger
	// are one source of truth.
	specBytes, err := json.Marshal(policy.Spec)
	if err != nil {
		return resolvedGuardrail{}, fmt.Errorf("marshaling guardrailPolicy %q spec: %w", ref, err)
	}
	h := sha256.Sum256(specBytes)
	return resolvedGuardrail{
		referenced: true,
		policyJSON: string(specBytes),
		digest:     fmt.Sprintf("%x", h[:])[:8],
	}, nil
}

// firstUncompilablePattern returns the (name, pattern, error) of the FIRST RE2 pattern
// in the policy that fails regexp.Compile, or ("","",nil) when every pattern compiles.
// It checks patternDenylist[].pattern and piiDetectors.custom[].pattern — the two
// operator-authored RE2 surfaces the guardrail engine matches against. Shared by the
// AgentDeployment fail-closed path (m66.2) and the GuardrailPolicy status controller so
// both judge validity identically.
func firstUncompilablePattern(spec *agentsv1beta1.GuardrailPolicySpec) (string, string, error) {
	for i := range spec.PatternDenylist {
		rule := &spec.PatternDenylist[i]
		if _, err := regexp.Compile(rule.Pattern); err != nil {
			return "patternDenylist[" + rule.Name + "]", rule.Pattern, err
		}
	}
	if spec.PIIDetectors != nil {
		for i := range spec.PIIDetectors.Custom {
			d := &spec.PIIDetectors.Custom[i]
			if _, err := regexp.Compile(d.Pattern); err != nil {
				return "piiDetectors.custom[" + d.Name + "]", d.Pattern, err
			}
		}
	}
	return "", "", nil
}

// reconcileGuardrailConfigMap materialises the resolved GuardrailPolicy JSON into the per-agent,
// STABLE-named <agent>-guardrail ConfigMap (owner-ref'd so it GCs with the AgentDeployment, mirroring
// ensureAgentIdentitySA / the prompt CM) and returns the volume + mount + static env the user
// container needs (K3, ADR 0059 Fork-2). It is a no-op returning zero values when the agent has no
// (resolved) guardrail policy.
//
// STABLE name (NOT content-addressed): a GuardrailPolicy edit re-reconciles the agent (the existing
// watch), which UPDATES this same ConfigMap in place. The mounted file changes and the launcher
// reloads (fsnotify) WITHOUT a revision roll — the reverse of the M4 landmine, and the whole point of
// K3. The policy is delivered as a mounted file (not env) so it can be WATCHED; the env carries only
// the static FILE PATH (no valueFrom — the m5.7 Knative ksvc landmine).
func (r *AgentDeploymentReconciler) reconcileGuardrailConfigMap(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	gr resolvedGuardrail,
) (vol *corev1.Volume, mount *corev1.VolumeMount, env []corev1.EnvVar, err error) {
	if !gr.referenced {
		return nil, nil, nil, nil
	}

	cmName := guardrailConfigMapName(deploy.Name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: deploy.Namespace},
	}
	if _, err = ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[guardrailConfigMapKey] = gr.policyJSON
		return ctrl.SetControllerReference(deploy, cm, r.Scheme)
	}); err != nil {
		return nil, nil, nil, fmt.Errorf("upserting guardrail ConfigMap: %w", err)
	}

	v := corev1.Volume{
		Name: guardrailVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
			},
		},
	}
	m := corev1.VolumeMount{Name: guardrailVolumeName, MountPath: guardrailMountPath, ReadOnly: true}
	e := []corev1.EnvVar{
		{Name: envGuardrailPolicyFile, Value: guardrailMountPath + "/" + guardrailConfigMapKey},
	}
	return &v, &m, e, nil
}

// guardrailPresenceDigest is the PRESENCE-ONLY guardrail component of combinedBindingDigest
// (K3, ADR 0059 Fork-2). It returns a fixed non-empty 8-hex token when the agent references a
// (resolved) GuardrailPolicy and "" when it does not — so ADDING or REMOVING the ref rolls the
// revision (a structural pod change: the mounted volume + GUARDRAIL_POLICY_FILE env + the
// gateway repoint appear/disappear), while EDITING an already-referenced policy's CONTENT keeps
// the SAME token → the SAME revision name → NO roll. The content now rides the watched, mounted
// ConfigMap, so a policy edit reloads live rather than rolling a new revision (the point of K3).
func guardrailPresenceDigest(referenced bool) string {
	if !referenced {
		return ""
	}
	// A stable, arbitrary token (the sha256 of a fixed marker, truncated to 8 hex). Its VALUE never
	// changes; only its presence/absence toggles the roll — exactly the presence semantics we want.
	h := sha256.Sum256([]byte("guardrail:referenced"))
	return fmt.Sprintf("%x", h[:])[:8]
}

// guardrailPolicyHash is the canonical policy hash surfaced on GuardrailPolicy.status
// and used by the fail-closed digest: the sha256 of the marshaled spec, truncated to 8
// hex. Returns ("", err) only if the well-typed spec fails to marshal (never in
// practice). Shared so the AgentDeployment digest and the GuardrailPolicy status agree
// on what "the policy" hashes to.
func guardrailPolicyHash(spec *agentsv1beta1.GuardrailPolicySpec) (string, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshaling GuardrailPolicySpec: %w", err)
	}
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h[:])[:8], nil
}
