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

// One precedence rule decides every variable the platform sets on the agent container:
//
//   - A variable the platform sets for this pod is the platform's. A spec.env entry of the same name is
//     dropped and the PlatformEnvIgnored condition names it; the spec field that drives the variable is
//     how an author changes it. The pod never carries a name twice: Kubernetes would use whichever entry
//     came last, so precedence would follow list position, and an author could, for one, point
//     MODEL_GATEWAY_URL past the proxy that enforces a budget the console shows as on.
//   - A name the platform does not set for this pod is the author's: PROMPT_FILE for a prompt bundled in
//     the image, TOKEN_SERVICE_URL or OBJECT_STORE_ADDR on an install that provides none.
//   - The identity variables are reserved: a spec.env entry is dropped even where the platform leaves the
//     variable unset, because other parties read them as claims and an absent one is itself a claim (no
//     tenant, no registry). They say which agent, version, tenant, registry and role a pod is: the
//     launcher stamps every trace agent:<POD_NAMESPACE>/<AGENT_NAME> and version:<AGENT_VERSION>, a trace
//     no run maps is authorized by that agent tag (ADR 0078), per-agent spend and per-version scores key on
//     them, TENANT_ID decides whose quota and ledger a call is charged to, and AGENT_REGISTRY_ID and
//     AGENT_ROLE are what an AMP call claims and the callee checks against the attestation (ADR 0153).
//   - A registry member's attestation wiring is reserved on a member (ADR 0153): otherwise an author could
//     switch one agent to accept unattested calls, or to verify against a key of their own.
//
// No variable is user-overridable: each one the platform sets is derived from a spec field or from the
// install, and an override would make the pod differ from what the spec, and the console, say it is.
// platformEnv lists them all; a variable added to the pod without a class fails the census test.
const (
	envTenantID        = "TENANT_ID"
	envAgentRegistryID = "AGENT_REGISTRY_ID"
	envAgentRole       = "AGENT_ROLE"

	envAgentPort               = "AGENT_PORT"
	envModelGatewayURL         = "MODEL_GATEWAY_URL"
	envGatewayUpstreamURL      = "GATEWAY_UPSTREAM_URL"
	envBudgetPerConversation   = "BUDGET_PER_CONVERSATION_USD"
	envBudgetPerAgent          = "BUDGET_PER_AGENT_USD"
	envBudgetSoftPct           = "BUDGET_SOFT_PCT"
	envRecordCapable           = "RECORD_CAPABLE"
	envTenantBudgetUSD         = "TENANT_BUDGET_USD"
	envTenantRPM               = "TENANT_RPM"
	envTenantMaxConcurrent     = "TENANT_MAX_CONCURRENT"
	envTenantQuotaAddr         = "TENANT_QUOTA_ADDR"
	envBFFInternalURL          = "BFF_INTERNAL_URL"
	envAgentRuntime            = "AGENT_RUNTIME"
	envMemoryBackendAddr       = "MEMORY_BACKEND_ADDR"
	envMemoryPort              = "MEMORY_PORT"
	envMemoryKeyNamespace      = "MEMORY_KEY_NAMESPACE"
	envMemoryPerUser           = "MEMORY_PER_USER"
	envMemoryScope             = "MEMORY_SCOPE"
	envMemoryLongTermEnabled   = "MEMORY_LONGTERM_ENABLED"
	envMemoryLongTermScope     = "MEMORY_LONGTERM_SCOPE"
	envEmbeddingRoute          = "EMBEDDING_ROUTE"
	envKnowledgeBaseEnabled    = "KNOWLEDGE_BASE_ENABLED"
	envKnowledgeBases          = "KNOWLEDGE_BASES"
	envA2AMaxDepth             = "A2A_MAX_DEPTH"
	envA2AHopBudget            = "A2A_HOP_BUDGET"
	envAsyncPublishURL         = "ASYNC_PUBLISH_URL"
	envAgentAllowedCallers     = "AGENT_ALLOWED_CALLERS"
	envDelegateEnabled         = "DELEGATE_ENABLED"
	envDelegateRoster          = "DELEGATE_ROSTER"
	envSpawnMaxFanout          = "SPAWN_MAX_FANOUT"
	envSpawnMaxDepth           = "SPAWN_MAX_DEPTH"
	envSpawnMaxTotal           = "SPAWN_MAX_TOTAL"
	envLangfuseHost            = "LANGFUSE_HOST"
	envLangfuseScoresPublicKey = "LANGFUSE_SCORES_PUBLIC_KEY"
	envLangfuseScoresSecretKey = "LANGFUSE_SCORES_SECRET_KEY" //nolint:gosec // env-var NAME, not a secret value.
	envFeedbackPort            = "FEEDBACK_PORT"

	// conditionPlatformEnvIgnored is True while spec.env names a variable the platform owns, naming
	// them; it is absent otherwise.
	conditionPlatformEnvIgnored = "PlatformEnvIgnored"
	reasonPlatformEnvSet        = "PlatformEnvSet"

	// conditionIdentityEnvIgnored is the condition's earlier name, which only covered the identity
	// variables. The controller removes it.
	conditionIdentityEnvIgnored = "IdentityEnvIgnored"
)

// platformEnvClass is what happens to a spec.env entry named like a platform variable.
type platformEnvClass int

const (
	// envPlatformWhenSet: dropped when the platform sets the variable for this pod.
	envPlatformWhenSet platformEnvClass = iota + 1
	// envReservedOnMember: dropped on a registry member, as envPlatformWhenSet elsewhere.
	envReservedOnMember
	// envReserved: always dropped.
	envReserved
)

// platformEnvVar is one variable the platform sets on the agent container.
type platformEnvVar struct {
	name  string
	class platformEnvClass
}

// platformEnv is every variable the platform sets on the agent container, in the order a condition names
// them.
var platformEnv = []platformEnvVar{
	{envPodNamespace, envReserved},
	{envAgentName, envReserved},
	{envAgentVersion, envReserved},
	{envTenantID, envReserved},
	{envAgentRegistryID, envReserved},
	{envAgentRole, envReserved},

	{envBFFPodTokenPath, envReservedOnMember},
	{envMCPCapPublicKey, envReservedOnMember},
	{envMCPCapAudience, envReservedOnMember},
	{envAMPRequireAttestation, envReservedOnMember},

	// spec.port: the launcher listens on it and the probes target it.
	{envAgentPort, envPlatformWhenSet},
	// spec.budget, a tenant's model caps, spec.guardrailPolicyRef and spec.record route model calls
	// through the in-pod proxy, which enforces them.
	{envModelGatewayURL, envPlatformWhenSet},
	{envGatewayUpstreamURL, envPlatformWhenSet},
	{envBudgetPerConversation, envPlatformWhenSet},
	{envBudgetPerAgent, envPlatformWhenSet},
	{envBudgetSoftPct, envPlatformWhenSet},
	{envGuardrailPolicyFile, envPlatformWhenSet},
	{envRecordCapable, envPlatformWhenSet},
	{envTenantBudgetUSD, envPlatformWhenSet},
	{envTenantRPM, envPlatformWhenSet},
	{envTenantMaxConcurrent, envPlatformWhenSet},
	{envTenantQuotaAddr, envPlatformWhenSet},
	// The launcher binds the run capability at the BFF before the agent's code sees it (ADR 0124).
	{envBFFInternalURL, envPlatformWhenSet},
	// spec.runtime, spec.promptRef and spec.skillRefs, delivered to the SDK.
	{envAgentRuntime, envPlatformWhenSet},
	{envPromptFile, envPlatformWhenSet},
	{envPromptVersion, envPlatformWhenSet},
	{envSkillRefs, envPlatformWhenSet},
	{envSkillDescriptions, envPlatformWhenSet},
	{envSkillDir, envPlatformWhenSet},
	// MCP tool bindings: the mounted manifest the launcher serves on :2999.
	{envToolsJSONPath, envPlatformWhenSet},
	// spec.sessionMemory and spec.longTermMemory: where memory lives, its key prefix and its scope.
	{envMemoryBackendAddr, envPlatformWhenSet},
	{envMemoryPort, envPlatformWhenSet},
	{envMemoryKeyNamespace, envPlatformWhenSet},
	{envMemoryPerUser, envPlatformWhenSet},
	{envMemoryScope, envPlatformWhenSet},
	{envMemoryLongTermEnabled, envPlatformWhenSet},
	{envMemoryLongTermScope, envPlatformWhenSet},
	{envEmbeddingRoute, envPlatformWhenSet},
	{envStatelayerProxyURL, envPlatformWhenSet},
	{envStatelayerTokenPath, envPlatformWhenSet},
	{envTokenServiceURL, envPlatformWhenSet},
	// spec.knowledgeBases: the roster the launcher admits searches against.
	{envKnowledgeBaseEnabled, envPlatformWhenSet},
	{envKnowledgeBases, envPlatformWhenSet},
	// Registry membership, spec.allowedCallers and a supervised AgentTeam: the mesh guards, the
	// callee's inbound policy and the delegation budget.
	{envA2AMaxDepth, envPlatformWhenSet},
	{envA2AHopBudget, envPlatformWhenSet},
	{envAsyncPublishURL, envPlatformWhenSet},
	{envAgentAllowedCallers, envPlatformWhenSet},
	{envDelegateEnabled, envPlatformWhenSet},
	{envDelegateRoster, envPlatformWhenSet},
	{envSpawnMaxFanout, envPlatformWhenSet},
	{envSpawnMaxDepth, envPlatformWhenSet},
	{envSpawnMaxTotal, envPlatformWhenSet},
	// The dev data plane: the object store blob offload and record fixtures land in, and the feedback
	// relay to the Langfuse the platform reads scores from.
	{envObjectStoreAddr, envPlatformWhenSet},
	{envObjectStoreAccessKey, envPlatformWhenSet},
	{envObjectStoreSecretKey, envPlatformWhenSet},
	{envLangfuseHost, envPlatformWhenSet},
	{envLangfuseScoresPublicKey, envPlatformWhenSet},
	{envLangfuseScoresSecretKey, envPlatformWhenSet},
	{envFeedbackPort, envPlatformWhenSet},
}

// platformEnvIndex is name → position in platformEnv.
var platformEnvIndex = func() map[string]int {
	m := make(map[string]int, len(platformEnv))
	for i, v := range platformEnv {
		m[v.name] = i
	}
	return m
}()

// reservedEnv reports whether spec.env may never set name on this pod.
func reservedEnv(name string, member bool) bool {
	i, ok := platformEnvIndex[name]
	if !ok {
		return false
	}
	switch platformEnv[i].class {
	case envReserved:
		return true
	case envReservedOnMember:
		return member
	default:
		return false
	}
}

// withSpecEnv inserts spec.env into the platform's env at index at, without the entries the platform owns
// (a reserved name, or any name the platform set). It returns the env and the names it dropped, each once,
// in platformEnv order.
func withSpecEnv(platform []corev1.EnvVar, at int, spec []corev1.EnvVar, member bool) ([]corev1.EnvVar, []string) {
	kept := make([]corev1.EnvVar, 0, len(spec))
	var ignored []string
	for _, e := range spec {
		if !reservedEnv(e.Name, member) && !envVarPresent(platform, e.Name) {
			kept = append(kept, e)
			continue
		}
		if !slices.Contains(ignored, e.Name) {
			ignored = append(ignored, e.Name)
		}
	}
	slices.SortStableFunc(ignored, func(a, b string) int { return envOrder(a) - envOrder(b) })
	out := make([]corev1.EnvVar, 0, len(platform)+len(kept))
	out = append(out, platform[:at]...)
	out = append(out, kept...)
	return append(out, platform[at:]...), ignored
}

// envOrder places a name the census has not classified after every one it has.
func envOrder(name string) int {
	if i, ok := platformEnvIndex[name]; ok {
		return i
	}
	return len(platformEnv)
}

// syncPlatformEnvCondition reports the platform variables spec.env sets, or clears the report. It writes
// status only when the condition changes.
func (r *AgentDeploymentReconciler) syncPlatformEnvCondition(
	ctx context.Context,
	deploy *agentsv1beta1.AgentDeployment,
	ignored []string,
) error {
	changed := apimeta.RemoveStatusCondition(&deploy.Status.Conditions, conditionIdentityEnvIgnored)
	if len(ignored) == 0 {
		changed = apimeta.RemoveStatusCondition(&deploy.Status.Conditions, conditionPlatformEnvIgnored) || changed
	} else {
		changed = apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
			Type:               conditionPlatformEnvIgnored,
			Status:             metav1.ConditionTrue,
			Reason:             reasonPlatformEnvSet,
			ObservedGeneration: deploy.Generation,
			Message: fmt.Sprintf("spec.env sets %s, which the platform sets for this agent; the values in "+
				"spec.env are not used. Remove them from spec.env, and change the spec field that sets them instead.",
				strings.Join(ignored, ", ")),
		}) || changed
	}
	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, deploy); err != nil {
		return fmt.Errorf("updating %s: %w", conditionPlatformEnvIgnored, err)
	}
	return nil
}
