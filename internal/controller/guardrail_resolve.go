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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
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

// envGuardrailPolicy is the STATIC env var carrying the resolved GuardrailPolicy spec
// (serialized to JSON) into the launcher, which forces its :2996 model proxy on and
// runs the in-path guardrail engine (m66.3). NEVER valueFrom — the m5.7 Knative ksvc
// landmine (the webhook rejects valueFrom in a ksvc pod template).
const envGuardrailPolicy = "GUARDRAIL_POLICY"

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
