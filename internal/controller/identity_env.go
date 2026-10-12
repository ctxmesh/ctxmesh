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
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// The identity variables say which agent, version, tenant, registry and role a pod is. The launcher stamps
// every trace agent:<POD_NAMESPACE>/<AGENT_NAME> and version:<AGENT_VERSION>; a trace no run maps is
// authorized by that agent tag (ADR 0078); per-agent spend is keyed by the agent's namespace and name;
// per-version scores drive regression detection, rollback and canary scoring; TENANT_ID decides whose
// quota and spend ledger a call is charged to; and AGENT_REGISTRY_ID and AGENT_ROLE are the registry and
// role an AMP call claims, which the callee checks against the platform's attestation (ADR 0153). A value
// an author could set would let one agent act under another's name, so they are always the platform's: a
// value for one in spec.env is dropped and reported in the IdentityEnvIgnored condition.
//
// A registry member's attestation wiring is the platform's on the same terms (memberPlatformEnvNames).
//
// AGENT_ALLOWED_CALLERS is not among them: it is the callee's own inbound policy, which whoever can edit
// spec.env can equally set through spec.allowedCallers, and it says nothing about the agent to anyone else.
const (
	envTenantID        = "TENANT_ID"
	envAgentRegistryID = "AGENT_REGISTRY_ID"
	envAgentRole       = "AGENT_ROLE"

	// conditionIdentityEnvIgnored is True while spec.env sets an identity variable, naming them; it is
	// absent otherwise.
	conditionIdentityEnvIgnored = "IdentityEnvIgnored"
	reasonIdentityEnvSet        = "IdentityEnvSet"
)

// identityEnvNames are the identity variables, in the order a condition names them.
var identityEnvNames = []string{
	envPodNamespace, envAgentName, envAgentVersion, envTenantID, envAgentRegistryID, envAgentRole,
}

// memberPlatformEnvNames decide how a registry member's launcher checks who calls it (ADR 0153): its BFF
// token, the key and audience it verifies attestations and run capabilities with, and the posture. On a
// member they are the platform's, or an author could switch one agent to accept unattested calls or to
// verify against a key of their own, and the revision name would fold a posture the pod does not have.
// An agent in no registry keeps what spec.env says: these do not decide who may call it.
var memberPlatformEnvNames = []string{envBFFPodTokenPath, envMCPCapPublicKey, envMCPCapAudience, envAMPRequireAttestation}

// withoutIdentityEnv returns env without the variables the platform owns (the identity variables, and a
// registry member's attestation wiring), and the ones it held, each once, in the order a condition names
// them.
func withoutIdentityEnv(env []corev1.EnvVar, member bool) ([]corev1.EnvVar, []string) {
	owned := identityEnvNames
	if member {
		owned = slices.Concat(identityEnvNames, memberPlatformEnvNames)
	}
	kept := make([]corev1.EnvVar, 0, len(env))
	var ignored []string
	for _, e := range env {
		if !slices.Contains(owned, e.Name) {
			kept = append(kept, e)
			continue
		}
		if !slices.Contains(ignored, e.Name) {
			ignored = append(ignored, e.Name)
		}
	}
	slices.SortFunc(ignored, func(a, b string) int {
		return slices.Index(owned, a) - slices.Index(owned, b)
	})
	return kept, ignored
}

// syncIdentityEnvCondition reports the identity variables spec.env sets, or clears the report. It
// writes status only when the condition changes.
func (r *AgentDeploymentReconciler) syncIdentityEnvCondition(
	ctx context.Context,
	deploy *agentsv1beta1.AgentDeployment,
	ignored []string,
) error {
	changed := false
	if len(ignored) == 0 {
		changed = apimeta.RemoveStatusCondition(&deploy.Status.Conditions, conditionIdentityEnvIgnored)
	} else {
		changed = apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
			Type:               conditionIdentityEnvIgnored,
			Status:             metav1.ConditionTrue,
			Reason:             reasonIdentityEnvSet,
			ObservedGeneration: deploy.Generation,
			Message: fmt.Sprintf("spec.env sets %s, which identify the agent in traces, cost and authorization, "+
				"or decide how a registry member checks its callers, and are always set by the platform; the values "+
				"in spec.env are not used. Remove them from spec.env.",
				strings.Join(ignored, ", ")),
		})
	}
	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, deploy); err != nil {
		return fmt.Errorf("updating %s: %w", conditionIdentityEnvIgnored, err)
	}
	return nil
}
