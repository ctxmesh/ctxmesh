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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// AgentResources specifies optional CPU and memory resource requests for the
// agent container. Both fields map directly to Kubernetes resource quantities.
type AgentResources struct {
	// cpu is the CPU resource request for the agent container, e.g. "500m".
	// Maps to resources.requests.cpu on the Knative Service container.
	// +optional
	CPU resource.Quantity `json:"cpu,omitempty"`

	// memory is the memory resource request for the agent container, e.g. "256Mi".
	// Maps to resources.requests.memory on the Knative Service container.
	// +optional
	Memory resource.Quantity `json:"memory,omitempty"`
}

// BudgetSpec defines optional cost-governance caps for an AgentDeployment.
// Either or both USD caps may be set; omitting a cap means that dimension is
// unenforced. softThresholdPct controls the alert percentage for whichever
// caps are set. Values are exact-decimal strings (e.g. "0.50") to avoid
// floating-point drift. The gateway reads these caps via injected env vars and
// enforces them per PRD §14.
type BudgetSpec struct {
	// perConversationUSD is the hard USD cost cap per conversation ID.
	// When a conversation's total spend reaches this value the gateway returns a
	// typed budget_exceeded error and refuses further calls for that conversation.
	// Expressed as an exact decimal string, e.g. "0.50". Optional; omit to leave
	// this dimension unenforced.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,6})?$`
	PerConversationUSD string `json:"perConversationUSD,omitempty"`

	// perAgentUSD is the hard USD cost cap across all conversations for this agent.
	// Expressed as an exact decimal string, e.g. "10.00". Optional; omit to leave
	// this dimension unenforced.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,6})?$`
	PerAgentUSD string `json:"perAgentUSD,omitempty"`

	// softThresholdPct is the percentage of a hard cap at which the gateway emits
	// a one-shot budget.alert event and log line, but continues processing.
	// Applied to whichever caps are set. Defaults to 80.
	// +optional
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=99
	SoftThresholdPct int32 `json:"softThresholdPct,omitempty"`
}

// ScalingSpec controls the Knative autoscaler bounds for a serving agent.
// min and max map to the knative.dev/serving min-scale and max-scale annotations.
type ScalingSpec struct {
	// min is the minimum number of replicas. Set to 0 (default) to enable scale-to-zero.
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	Min int32 `json:"min,omitempty"`

	// max is the maximum number of replicas the autoscaler may create.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	Max int32 `json:"max,omitempty"`
}

// SessionMemorySpec is the session-memory config expressed as an AgentDeployment field (ADR 0037,
// m34.2; the folded home for what was the MemoryBinding CRD, now retired — ADR 0101), since a binding
// is always 1:1 with its agent and is never shared as an object (the m33.3 "shared" scope is a
// data-layer key + a field value, not a shared binding). Absent ⇒ the agent has no conversation memory.
type SessionMemorySpec struct {
	// scope selects the memory key layout. "session" (default) = PRIVATE per-agent
	// (mem:{namespace}/{agent}:{conversationId}); "shared" (m33.3) = a team scratchpad keyed
	// mem:shared:{registry}:{conversationId}, which requires the agent to be a registry member.
	// +kubebuilder:validation:Enum=session;shared
	// +kubebuilder:default=session
	// +optional
	Scope string `json:"scope,omitempty"`

	// perUser isolates each invoking end-user's conversation memory into a separate bucket (M98, EU1a,
	// ADR 0080), so one user's session history never surfaces in another user's — the launcher stamps a
	// hash of the VERIFIED run capability's user id and the state-layer proxy keys under it. It is
	// PRODUCT-grade, not security-grade: isolation is launcher-stamped inside the pod boundary (a
	// compromised pod could still read its own users' buckets, same posture as long-term perUser); the
	// enforcement-point move is EU1b. Only meaningful for the private ("session") scope — ignored for the
	// shared team scratchpad, which is per-conversation by design. Default off: existing agents keep the
	// agent-wide bucket, and it breaks conversation handoff / share-links for the agent, so it is opt-in.
	// Requires the state-layer proxy path (the default install). An async/eventing turn (no run
	// capability) falls back to the agent-wide bucket rather than failing — see ADR 0080.
	// +optional
	PerUser bool `json:"perUser,omitempty"`

	// backend locates the Valkey state-layer instance. Defaulted to the cluster state-layer service
	// when omitted.
	// +optional
	Backend *MemoryBackend `json:"backend,omitempty"`
}

// MemoryBackend locates the Valkey backend used to store session memory. Relocated
// here from the retired MemoryBinding CRD (ADR 0101) since sessionMemory is now its
// only home.
type MemoryBackend struct {
	// addr is the host:port of the Valkey backend.
	// If omitted the controller defaults to
	// ctxmesh-statelayer.ctxmesh.svc:6379.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Addr string `json:"addr,omitempty"`
}

// KnowledgeBaseRef is a reference to a KnowledgeBase CR that this agent is granted access to
// (ADR 0061 Fork 3 — authz = folded spec field, NOT a binding CRD; the sessionMemory fold set the precedent).
// The controller resolves the ref to inject KNOWLEDGE_BASES roster env; the launcher roster gate is the
// un-forgeable enforcement boundary (mirroring DELEGATE_ROSTER). A namespace-local ref (no Namespace field)
// refers to a KnowledgeBase in the same namespace as the AgentDeployment.
type KnowledgeBaseRef struct {
	// name is the KnowledgeBase CR name. Required.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// namespace is the namespace of the KnowledgeBase CR. When omitted, the AgentDeployment's
	// own namespace is used (the most common case — same-namespace KBs need no namespace field).
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// autoInject is a per-binding opt-in for RAG-style knowledge auto-injection (ADR 0061 governance #5,
	// M10). When true, the in-pod SDK retrieves the most relevant chunks of THIS KB on the user input each
	// turn and prepends them as an ephemeral `<retrieved_context>` block (with citations) to the system
	// prompt — never persisted to conversation history. When false/unset (the default) the KB is TOOL-ONLY:
	// the agent must call the `knowledge_search` tool to retrieve — today's behaviour, byte-for-byte
	// unchanged. This is a per-KB-binding flag (one agent may auto-inject KB A while KB B stays tool-only).
	// +optional
	AutoInject bool `json:"autoInject,omitempty"`
}

// LongTermMemorySpec is the folded long-term-memory config (ADR 0045): `agent`-scope memory that persists
// ACROSS conversations and is retrieved by MEANING (pgvector), orthogonal to sessionMemory's conversation
// scope — an agent can have both. Like sessionMemory it folds into the AgentDeployment (a per-agent 1:1
// capability, ADR 0037). The store lives in the control-plane Postgres, reached via the token-service (agent
// pods hold no DB credentials, ADR 0045 Amд 1); the launcher exposes memory.remember / memory.search_agent
// that proxy there with the capability token.
type LongTermMemorySpec struct {
	// enabled turns on long-term memory for the agent. Optional-with-default so `enabled: false`
	// need not be spelled out (P2-8a; matches AutoRollbackConfig.Enabled).
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// perUser scopes each memory to the invoking user (store scope "agent_user"; the launcher stamps the
	// caller's identity as the subject) rather than agent-wide (store scope "agent", subject empty). Per-user
	// isolation means one user's remembered facts never surface in another user's retrieved context.
	// +optional
	PerUser bool `json:"perUser,omitempty"`

	// embeddingRoute names the gateway model route used to embed memories + queries. If omitted the controller
	// applies the cluster-default embedding route. The route must exist on the agent's model gateway.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	EmbeddingRoute string `json:"embeddingRoute,omitempty"`
}

// AgentDeploymentSpec defines the desired state of an AgentDeployment.
// +kubebuilder:validation:XValidation:rule="!(has(self.endUserAccess) && self.endUserAccess && (self.executionModel == 'job' || self.executionModel == 'eventing'))",message="endUserAccess is only valid on a serving execution model — end-user chat is interactive request-driven (M137/ADR 0107)"
type AgentDeploymentSpec struct {
	// image is the fully-qualified container image for the agent,
	// e.g. "ghcr.io/ctxmesh/echo-agent:latest". Required.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// executionModel determines how the agent runtime is hosted:
	//   - serving (default): a Knative Service (request-driven), unchanged from M1.
	//   - eventing: a Knative Service PLUS a Knative Eventing Trigger subscribing it
	//     to the agent's registry broker (the agent must be a registry member).
	//   - job: a one-shot Kubernetes Job (restartPolicy Never), or a CronJob when a
	//     schedule AgentScalingPolicy targets the agent.
	// The reconciler branches on this value; serving stays the default so every
	// M1-M6 agent is unaffected.
	// +optional
	// +kubebuilder:default=serving
	// +kubebuilder:validation:Enum=serving;eventing;job
	ExecutionModel string `json:"executionModel,omitempty"`

	// port is the TCP port the agent HTTP server listens on. Passed to the
	// container as the $AGENT_PORT environment variable. Defaults to 8080.
	// +optional
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// resources optionally constrains the CPU and memory available to the agent
	// container. When omitted, no resource requests are set (Knative default).
	// +optional
	Resources *AgentResources `json:"resources,omitempty"`

	// env is an optional list of environment variables injected directly into
	// the agent container alongside the controller-managed variables such as
	// $AGENT_PORT. Uses the standard Kubernetes EnvVar schema.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// scaling configures the Knative autoscaler bounds for this agent.
	// When omitted, defaults to min=0 (scale-to-zero) and max=3.
	// +optional
	Scaling *ScalingSpec `json:"scaling,omitempty"`

	// sessionMemory is the agent's conversation-memory config (ADR 0037, m34.2; the folded home for
	// the retired MemoryBinding CRD, ADR 0101): when set, this agent has conversation memory — scope
	// selects private-per-agent or a registry-shared scratchpad (m33.3), backend locates the Valkey.
	// +optional
	SessionMemory *SessionMemorySpec `json:"sessionMemory,omitempty"`

	// longTermMemory optionally enables `agent`/long-term semantic memory (ADR 0045) — persistent across
	// conversations, retrieved by meaning — orthogonal to sessionMemory.
	// +optional
	LongTermMemory *LongTermMemorySpec `json:"longTermMemory,omitempty"`

	// knowledgeBases lists the managed RAG corpora this agent is granted access to (ADR 0061, M68).
	// This is a CAPABILITY field — the same pattern as longTermMemory — enforced un-forgeably at the
	// launcher roster gate (KNOWLEDGE_BASES env, mirroring DELEGATE_ROSTER), NOT by a binding CRD.
	// The controller resolves each ref to a KnowledgeBase CR (reading spec.embeddingRoute) and injects
	// KNOWLEDGE_BASE_ENABLED=true + KNOWLEDGE_BASES as a JSON roster; the launcher's knowledgeProxy
	// gates every /knowledge/search against the roster — a model cannot forge KB membership.
	// Dangling (unresolvable) refs surface a condition and are skipped; the remaining resolvable KBs
	// are still injected (KB is an ADDITIVE capability, not a fail-closed safety gate like guardrails).
	// An agent with no knowledgeBases gets no KNOWLEDGE_BASE_ENABLED env (the proxy stays off).
	// +listType=atomic
	// +optional
	// +kubebuilder:validation:MaxItems=16
	KnowledgeBases []KnowledgeBaseRef `json:"knowledgeBases,omitempty"`

	// role is the agent's role within its AgentRegistry (PRD §12.4 role-based
	// access control). The three built-in roles always exist: "orchestrator",
	// "worker", and "reviewer"; a registry may also declare custom roles via
	// AgentRegistry.spec.roles. When set on a registry member, the controller
	// injects it as the static AGENT_ROLE env var, which the launcher stamps
	// into the A2A message envelope and checks against the callee's role policy.
	// Bounded at 63 characters (the AGENT_ROLE value is a short label, never a
	// free-form string). Ignored for non-members.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Role string `json:"role,omitempty"`

	// capabilities is what this agent CAN DO, in the agent's own words — the basis for CAPABILITY-BASED
	// semantic discovery (M141, ADR 0120): an agent is found by what it does, not its DNS name. The control
	// plane embeds the descriptor (offline embedder, ADR 0116) so a capability QUERY retrieves + reranks the
	// right agent (ADR 0084/0117). Optional — an agent with NO descriptor is simply not semantically
	// discoverable; it stays reachable by name, exactly as before.
	// +optional
	Capabilities *CapabilityDescriptor `json:"capabilities,omitempty"`

	// allowedCallers is the per-agent inbound allowlist (PRD §12.4 layer 3):
	// the names of peer agents permitted to call this agent over A2A. The
	// controller comma-joins it into the static AGENT_ALLOWED_CALLERS env var;
	// the callee's launcher rejects a caller not on the list with a typed
	// caller_not_allowed error. An empty/omitted list means the launcher applies
	// its default policy (registry-membership check only). Bounded at 64 entries
	// of at most 63 characters each (DNS-label-sized agent names).
	// +listType=atomic
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	AllowedCallers []string `json:"allowedCallers,omitempty"`

	// budget optionally sets USD cost-governance caps for this agent (PRD §14).
	// When set, the gateway enforces the caps per conversation and/or per agent and
	// emits a soft alert at softThresholdPct% of each cap. When omitted, no cost
	// enforcement is applied. See BudgetSpec for field details.
	// +optional
	Budget *BudgetSpec `json:"budget,omitempty"`

	// evalSuiteRef optionally names an EvalSuite (same namespace) whose scorers
	// gate this deployment. When set, the controller runs the suite against the
	// candidate revision and promotes or blocks based on the result and the suite's
	// gate policy. When omitted, no eval gate is applied — the deploy proceeds
	// unchanged (PRD §17).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	EvalSuiteRef string `json:"evalSuiteRef,omitempty"`

	// promptRef optionally names a prompt version (by name, same namespace) whose git-backed
	// prompt is injected into the agent. Prompt versions are Postgres-resident control-plane
	// records (ADR 0044 — the PromptVersion CRD was retired to the store), resolved by the
	// controller via the prompt service. Swapping promptRef rolls a new Knative revision with
	// the new prompt without an image rebuild. When omitted, the image-bundled prompt is used.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	PromptRef string `json:"promptRef,omitempty"`

	// tracePolicy optionally extends the always-on trace-redaction policy
	// (PRD §13.3). The built-in detectors (emails, US SSNs, API keys/tokens) are
	// ALWAYS applied to the sensitive payload attributes at the collector before
	// persistence, regardless of this field. tracePolicy lets an agent add extra
	// regex detectors for its own domain-specific PII. When omitted, only the
	// built-in defaults apply.
	// +optional
	TracePolicy *TracePolicy `json:"tracePolicy,omitempty"`

	// runtime optionally configures runtime authoring primitives: structured
	// output schemas, tool-use policies, and per-turn resilience settings.
	// When omitted, the agent's current behaviour is preserved unchanged.
	// This field is types-only in m65; injection by the managed loop is a
	// subsequent task.
	// +optional
	Runtime *RuntimeSpec `json:"runtime,omitempty"`

	// guardrailPolicyRef optionally names a GuardrailPolicy (same namespace) that governs this
	// agent's content at inference time. The guardrail engine enforces the policy via a sidecar
	// (m66.3); a missing or invalid ref fails the agent closed — the request is denied rather than
	// passed through unguarded (m66.2 controller validates the ref and sets a condition).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	GuardrailPolicyRef string `json:"guardrailPolicyRef,omitempty"`

	// approvalPolicyRef optionally names an ApprovalPolicy (same namespace) that declaratively requires
	// human approval for named tool calls (and optionally narrows who may approve) — M139, ADR 0111. The
	// controller merges its require-approval requirements into this agent's effective tool policy
	// (reusing the pause/resume/voucher runtime); a dangling ref sets a NotReady condition on the agent.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ApprovalPolicyRef string `json:"approvalPolicyRef,omitempty"`

	// feedbackStoreRef optionally names a FeedbackStore (same namespace) that declares this agent's
	// multi-source feedback model (M139, ADR 0112, PRD §17.3). It is DECLARATIVE config: the BFF write path
	// gates ingestion by the declared score names and the read path attributes scores to their source;
	// Langfuse remains the store of record (ADR 0008). Absent ⇒ today's open :2995→Langfuse relay, unchanged.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	FeedbackStoreRef string `json:"feedbackStoreRef,omitempty"`

	// rollout optionally selects a progressive-delivery strategy for a GATED serving
	// agent (ADR 0062 Fork 3, M69). Absent (or strategy "") ⇒ today's promote-all/hold
	// behavior, byte-for-byte unchanged — a no-rollout deployment's Knative Service is
	// untouched. When strategy is "canary", a candidate that PASSES the offline gate is
	// served a NAMED-revision traffic split {old: 100-canaryPercent, candidate:
	// canaryPercent} instead of being held at 0%, so both arms accumulate online scores;
	// the human completes it with `promote=<candidate>` (100% candidate) or aborts
	// (100% old). SERVING execution model only — for eventing/job the canary strategy is
	// deferred and the agent falls back to promote-all/hold. Shadow rollout is rejected
	// (double side-effects) and auto-progression is deferred.
	// +optional
	Rollout *RolloutSpec `json:"rollout,omitempty"`

	// record marks this agent RECORD-CAPABLE (M78, ADR 0071 §1). Record mode captures a
	// run's model + tool I/O into a portable replay fixture, and the capture rides the two
	// platform proxies (the launcher gateway for model I/O, the egress sidecar for tool I/O).
	// Those proxies are CONDITIONALLY interposed (ADR 0071 C2), so recording needs its own
	// interposition reason: when record is true the controller FORCES the launcher gateway on
	// (a new reason alongside budget/quota/guardrail) so it is present to capture.
	//
	// Enablement is PER-DEPLOYMENT (this flag) but capture is PER-RUN: a specific run opts in
	// via POST /api/runs {record:true}, and the BFF fails that run CLOSED if the agent is not
	// record-capable (no gateway to capture at — ADR 0071 C2, never a silent no-capture). So
	// this flag is "may record"; the run flag is "record THIS run". Default false ⇒ the agent
	// is not record-capable and the gateway interposition is byte-for-byte unchanged.
	// +optional
	Record bool `json:"record,omitempty"`

	// suspend stops this agent DECLARATIVELY (M146, ADR 0126 §4). A suspended agent accepts no new
	// runs and its queued runs are not claimed — the same halt the imperative kill switch applies,
	// but expressed as INTENT rather than as an incident action.
	//
	// The two are deliberately different tools. The kill switch is for "stop now": fast, no spec write,
	// no reconcile race, and it can express a tenant- or fleet-wide stop that no per-object field can.
	// This field is for "stay off": it survives a GitOps re-apply, which the imperative marker does not
	// — without it, the next `kubectl apply` would silently un-suspend an agent someone deliberately
	// turned off, the worst possible failure mode for a safety control.
	//
	// Default false ⇒ every existing AgentDeployment is byte-unchanged.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// endUserAccess opts THIS agent into being reachable by END-USERS via the standalone /chat runtime
	// (M137/EU1b, ADR 0107 — the second of the two keys). End-user access requires BOTH the agent's
	// tenant to enable an end-user IdP (Tenant.spec.endUserIdentity.enabled — WHO may log in) AND this
	// flag (WHAT they may reach). Default false ⇒ the agent is invisible to end-users (an internal agent
	// is never exposed just because its tenant turned on end-user login). When true, the controller
	// mirrors the agent's endpoint + spec into the control-plane `end_user_agents` table so the BFF
	// resolves an end-user run WITHOUT a K8s read; row-existence is the exposure gate (fail-closed). Only
	// valid on a `serving` execution model (rejected at validation for eventing/job) — end-user chat is
	// interactive request-driven.
	// +optional
	EndUserAccess bool `json:"endUserAccess,omitempty"`

	// mountServiceAccountToken opts THIS agent into auto-mounting the default kube-API
	// ServiceAccount token at /var/run/secrets/kubernetes.io/serviceaccount/ (m79.4, m52 C10).
	//
	// SECURE BY DEFAULT: nil/false ⇒ the token is NOT mounted. The agent runtime does not use
	// the default kube-API token (the state-layer proxy is authed by a dedicated audience-scoped
	// PROJECTED token, a separate volume), so the auto-mounted default token is unused attack
	// surface co-resident with user (semi-trusted) code. The controller sets
	// AutomountServiceAccountToken=false on the agent's per-agent identity ServiceAccount
	// (agent-<name>) to strip it.
	//
	// Set true ONLY for an agent that legitimately builds an in-cluster kube config (talks to
	// the kube API from inside the pod) — then the default token is auto-mounted again.
	//
	// COVERAGE: this hardening applies only to agents that HAVE a per-agent identity SA (memory /
	// proxy / on-behalf-of agents). A plain agent running the namespace's shared `default` SA is
	// out of scope — the controller must not toggle automount on a shared SA, as that would
	// affect every workload in the namespace. Broadening to a universal identity SA is a separate
	// follow-up.
	// +optional
	MountServiceAccountToken *bool `json:"mountServiceAccountToken,omitempty"`
}

// RolloutSpec selects a progressive-delivery strategy for a serving agent's rollout
// (ADR 0062 Fork 3, M69). It applies only when the agent references an EvalSuite (the
// gate decides which revision serves) and uses the serving execution model; otherwise
// it is ignored and today's promote-all/hold behavior applies.
type RolloutSpec struct {
	// strategy selects the rollout strategy. "" (default) is today's promote-all/hold —
	// a passing-but-unapproved candidate is held at 0% until the human promotes it.
	// "canary" serves a named-revision Knative traffic split {old, candidate:N%} while
	// the human decides, so both arms accumulate online scores (ADR 0062 Fork 3).
	// +optional
	// +kubebuilder:validation:Enum="";canary
	Strategy string `json:"strategy,omitempty"`

	// canaryPercent is the percent of live traffic routed to the CANDIDATE revision
	// during a canary rollout; the remainder (100 - canaryPercent) stays on the old
	// serving revision. Bounded to 1..99 so the split is a real canary — 0 would serve
	// no candidate traffic (indistinguishable from a hold) and 100 would be a full
	// promote (use `promote` for that). Only consulted when strategy == "canary".
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=99
	CanaryPercent int32 `json:"canaryPercent,omitempty"`

	// autoRollback optionally enables OPT-IN automatic rollback to the last-healthy
	// (prior) version when the online-score regression detector flags the serving
	// version (RegressionDetected=True) — ADR 0062 Fork 4, PRD §17.4. Absent (the
	// default) ⇒ detection-only, byte-for-byte the pre-auto behavior: the controller
	// NEVER rolls back on its own; a human drives the one-click rollback. When
	// enabled, the auto-path runs the SAME damping guards as the human rollback
	// (cooldown, two-version flap, healthy-target, freeze-after-auto-action) — it can
	// only roll back to a HEALTHY prior version and, on success, sets
	// status.rollback.frozenUntilAck to freeze further AUTO-actions until a human acks
	// (the anti-runaway guard). A subsequent auto-attempt while frozen is refused.
	// +optional
	AutoRollback *AutoRollbackConfig `json:"autoRollback,omitempty"`

	// autoProgress optionally enables OPT-IN automatic canary PROGRESSION (M139/N4, ADR 0113): a healthy
	// candidate (RegressionDetected=False — an evidence-backed non-inferiority verdict with a per-window
	// sample floor) auto-advances through a step schedule and auto-promotes at 100%, instead of holding at
	// canaryPercent for a human. Absent (the default) ⇒ hold-for-human, byte-for-byte unchanged. Fail-safe:
	// an Unknown verdict (dev without cpDB / sparse data) HOLDS, one step per reconcile (never fast-forward),
	// and a human promote/abort always wins. Only consulted when strategy == "canary".
	// +optional
	AutoProgress *AutoProgressConfig `json:"autoProgress,omitempty"`
}

// AutoProgressConfig configures OPT-IN automatic canary progression (M139/N4, ADR 0113). It is the ONLY
// switch that arms auto-advance; every deployment without it holds the canary at canaryPercent for a human.
type AutoProgressConfig struct {
	// enabled, when true, arms auto-advance + auto-promote on a healthy online-score verdict. Default false.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// steps is the percent ladder the canary auto-advances through (intended ascending, each > canaryPercent).
	// A step element is a STRUCT so future per-step knobs are additive. Default [{percent: 100}] ⇒ soak at
	// canaryPercent then auto-promote. A schedule MAY top out below 100 (machine advances that far, then holds
	// for a human) — a useful safety dial. Ordering is NOT enforced by admission (a strict-ascending CEL on a
	// list-of-structs is fragile and a malformed rule breaks the whole CRD); the controller treats steps as a
	// SET — the next rung is the minimum percent strictly greater than the current one — so a mis-ordered
	// schedule is harmless (monotone, never regresses traffic), just cosmetically odd.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=10
	Steps []CanaryStep `json:"steps,omitempty"`

	// dwellSeconds is the minimum soak per step before an advance is considered (M139/N4). Default 3600 —
	// one aggregate window, so each step sees fresh online-score evidence; a shorter dwell would advance
	// multiple steps on one window's data.
	// +optional
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=60
	DwellSeconds int32 `json:"dwellSeconds,omitempty"`
}

// CanaryStep is one rung of the auto-progression ladder (ADR 0113). A struct (not a bare percent) so
// per-step knobs (dwell, min-samples) are future non-breaking adds.
type CanaryStep struct {
	// percent is the candidate-arm traffic percent at this rung (> canaryPercent, ≤ 100).
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=100
	Percent int32 `json:"percent"`
}

// AutoRollbackConfig configures OPT-IN automatic rollback for a gated serving agent
// (ADR 0062 Fork 4, PRD §17.4). It is the ONLY switch that arms the auto-path; every
// deployment without it is unaffected (detection stays advisory, human-driven rollback).
type AutoRollbackConfig struct {
	// enabled, when true, arms automatic rollback to the last-healthy (prior) version on
	// RegressionDetected=True. Default false ⇒ detection-only (no auto-action). The
	// auto-path reuses the human rollback's damping guards verbatim and freezes further
	// auto-actions (status.rollback.frozenUntilAck) until a human acknowledges.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`
}

// RuntimeSpec configures runtime authoring primitives applied by the managed
// loop / platform: structured outputs, tool-use policies, and per-turn resilience.
// Absent => today's behavior, unchanged.
type RuntimeSpec struct {
	// outputSchema is a JSON Schema the agent's final answer must conform to.
	// The value is stored verbatim (arbitrary JSON) and is not structurally validated
	// by the CRD admission; it is passed to the platform at runtime.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	OutputSchema *k8sruntime.RawExtension `json:"outputSchema,omitempty"`

	// toolPolicy optionally constrains which tools the agent may use and how.
	// +optional
	ToolPolicy *ToolPolicySpec `json:"toolPolicy,omitempty"`

	// resilience optionally configures per-turn retry and circuit-breaker behaviour.
	// +optional
	Resilience *ResilienceSpec `json:"resilience,omitempty"`
}

// ToolPolicySpec constrains tool selection and concurrent tool-call behaviour.
type ToolPolicySpec struct {
	// default is the rule applied to any tool without an explicit override.
	// +optional
	// +kubebuilder:validation:Enum=allow;deny;require-approval
	// +kubebuilder:default=allow
	Default string `json:"default,omitempty"`

	// overrides is the per-tool-name policy list. Items are keyed on name and
	// applied in order; the first matching entry wins.
	// +optional
	// +listType=map
	// +listMapKey=name
	Overrides []ToolPolicyOverride `json:"overrides,omitempty"`

	// forcedChoice steers tool selection: "" or "auto" lets the model choose,
	// "required" forces at least one tool call, or a specific tool name forces
	// exactly that tool.
	// +optional
	ForcedChoice string `json:"forcedChoice,omitempty"`

	// parallelLimit caps concurrent tool calls per turn. 0 means unlimited.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ParallelLimit int32 `json:"parallelLimit,omitempty"`

	// maxToolCallsPerRun caps the total number of tool calls a single run may
	// forward through the egress sidecar (an anti-DoS fan-out ceiling). 0 means
	// unlimited (the default). Enforced fail-closed at the sidecar: once a run
	// exceeds this, further tool calls are denied with a terminal 403.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxToolCallsPerRun int32 `json:"maxToolCallsPerRun,omitempty"`
}

// ToolPolicyOverride is one named tool-level policy override.
type ToolPolicyOverride struct {
	// name is the exact tool name this override applies to.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// rule is the access rule for this tool.
	// +kubebuilder:validation:Enum=allow;deny;require-approval
	Rule string `json:"rule"`

	// retryable opts this tool in to retries on transient failure. Default false —
	// tool retries are off unless the tool is explicitly declared idempotent/safe.
	// +optional
	Retryable bool `json:"retryable,omitempty"`
}

// ResilienceSpec configures per-turn retry and timeout behaviour for model and
// tool calls.
type ResilienceSpec struct {
	// modelCall configures timeout and retry for model API calls.
	// +optional
	ModelCall *CallResilience `json:"modelCall,omitempty"`

	// toolCall configures timeout, retry, and circuit-breaker for tool calls.
	// +optional
	ToolCall *ToolCallResilience `json:"toolCall,omitempty"`
}

// CallResilience is timeout + retry settings for a single call category.
type CallResilience struct {
	// timeoutSeconds is the per-call hard deadline. 0 means no per-call timeout.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// maxRetries is the maximum number of retries after a transient failure. 0 means no retries.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxRetries int32 `json:"maxRetries,omitempty"`
}

// ToolCallResilience extends CallResilience with an optional circuit breaker for
// tool calls.
type ToolCallResilience struct {
	// timeoutSeconds is the per-call hard deadline. 0 means no per-call timeout.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// maxRetries is the maximum number of retries after a transient failure. 0 means no retries.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxRetries int32 `json:"maxRetries,omitempty"`

	// circuitBreaker opens the circuit after failureThreshold consecutive failures,
	// blocking calls for cooldownSeconds before half-opening.
	// +optional
	CircuitBreaker *CircuitBreakerSpec `json:"circuitBreaker,omitempty"`
}

// CircuitBreakerSpec defines the parameters of a simple count-based circuit breaker.
type CircuitBreakerSpec struct {
	// failureThreshold is the number of consecutive failures before the circuit opens.
	// +kubebuilder:validation:Minimum=1
	FailureThreshold int32 `json:"failureThreshold"`

	// cooldownSeconds is the duration the circuit stays open before half-opening.
	// 0 means the implementation applies its own default.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CooldownSeconds int32 `json:"cooldownSeconds,omitempty"`
}

// TracePolicy is the per-agent extension of the built-in trace-redaction policy
// (PRD §13.3). v1 supports adding custom regex detectors; the built-in
// email/SSN/key detectors are always on and cannot be disabled here (removing a
// default detector is a deliberate non-goal for the security baseline).
type TracePolicy struct {
	// customDetectors are additional named regex redaction rules applied — after
	// the built-in defaults — to the sensitive payload attributes. Each match is
	// replaced with a "[REDACTED:<name>]" marker. Patterns MUST be RE2-compatible
	// (Go regexp / the collector's OTTL replace_pattern: no backreferences or
	// lookaround). Bounded at 16 detectors so the rendered collector config stays
	// small and predictable.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	CustomDetectors []CustomDetector `json:"customDetectors,omitempty"`
}

// CustomDetector is one per-agent redaction rule: a name (used in the marker)
// and an RE2 regex whose matches are scrubbed from the sensitive payload
// attributes.
type CustomDetector struct {
	// name labels the detector and appears in its marker, e.g. name "badge"
	// replaces matches with "[REDACTED:badge]". Lowercase alnum + dashes, so the
	// marker is a stable, readable token.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*$`
	Name string `json:"name"`

	// pattern is the RE2 regular expression identifying the sensitive substring
	// to redact. Applied to every sensitive payload attribute's value; each match
	// is replaced by the detector's marker.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Pattern string `json:"pattern"`
}

// GateStatus reports the deploy-gate state for an AgentDeployment that references
// an EvalSuite (spec.evalSuiteRef). It is set only when a gate is active; a
// deployment without an evalSuiteRef leaves it nil (byte-compatible with the
// pre-M9 status). The phase drives the human-gated promotion state machine
// (specs/eval-prompts-feedback.md §1):
//
//	pending → scoring → awaiting-promotion → promoted   (pass, then human approval)
//	                 → blocked                            (fail, gate:block)
//	                 → warned                             (fail, gate:warn → promote anyway)
type GateStatus struct {
	// phase is the current gate state: pending | scoring | awaiting-promotion |
	// promoted | blocked | warned | canary | aborted. A passing candidate rests at
	// awaiting-promotion until a human approval signal (the agents.ctxmesh.ai/promote
	// annotation) flips it to promoted — v1 does NOT auto-promote (PRD §17.4). When the
	// deployment requests a canary rollout (spec.rollout.strategy == "canary"), a
	// passing candidate rests at `canary` instead (serving a named traffic split); the
	// human completes it (promote → promoted) or aborts it (→ aborted). ADR 0062 Fork 3.
	// +optional
	// +kubebuilder:validation:Enum=pending;scoring;awaiting-promotion;promoted;blocked;warned;canary;aborted
	Phase string `json:"phase,omitempty"`

	// score is the candidate's weighted-mean suite score for the scored revision,
	// as an exact decimal string in [0,1] (e.g. "0.8123"). Empty until scoring has
	// produced a value. Stored as a string, not a float, so the status round-trips
	// byte-stably. Empty when the gate failed closed unscored (Langfuse down).
	// +optional
	// +kubebuilder:validation:Pattern=`^$|^[01](\.[0-9]{1,4})?$`
	Score string `json:"score,omitempty"`

	// threshold echoes the EvalSuite threshold the score was compared against, as
	// an exact decimal string, so the gate decision is self-describing on status
	// without a second lookup.
	// +optional
	// +kubebuilder:validation:Pattern=`^$|^[01](\.[0-9]{1,4})?$`
	Threshold string `json:"threshold,omitempty"`

	// decision is the terminal gate decision recorded for the scored revision:
	// promoted | blocked | warned. Empty while pending/scoring.
	// +optional
	// +kubebuilder:validation:Enum=promoted;blocked;warned
	Decision string `json:"decision,omitempty"`

	// scoredRevision is the candidate revision name the gate scored and decided on
	// (the "-h<digest>" revision the candidate would serve). It pins the decision to
	// an exact candidate so a later spec/prompt change re-scores rather than reusing
	// a stale decision, and so a human approval targets the reviewed candidate.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	ScoredRevision string `json:"scoredRevision,omitempty"`

	// reason is a short machine reason for the current phase (e.g. ScorePassed,
	// ScoreBelowThreshold, LangfuseUnavailable). Human-readable detail lives on the
	// Ready condition message.
	// +optional
	// +kubebuilder:validation:MaxLength=316
	Reason string `json:"reason,omitempty"`
}

// RollbackEvent is one record in the rollback history the damping guards read
// (ADR 0062 Fork 4, M69). It records a completed spec-revert: which version the
// serving spec was reverted TO, which version it was reverted FROM, and when. The
// two-version flap detector reads recent events to refuse rolling back TO a version
// that was recently rolled back FROM.
type RollbackEvent struct {
	// toVersion is the AgentVersion the serving spec was reverted to (the rollback
	// target — an explicit AgentVersion name).
	// +kubebuilder:validation:MaxLength=253
	ToVersion string `json:"toVersion"`

	// fromVersion is the AgentVersion the serving spec matched immediately before the
	// revert (the version rolled back FROM), captured from status.latestVersion. Empty
	// when no serving version was recorded yet.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	FromVersion string `json:"fromVersion,omitempty"`

	// at is when the rollback was actuated.
	At metav1.Time `json:"at"`
}

// RollbackStatus reports the human-rollback actuator state (ADR 0062 Fork 4, M69 —
// the HUMAN actuator; the AUTO-rollback trigger is DEFERRED per PRD §17.4). It is
// set only after at least one rollback (or refused rollback) has been evaluated;
// nil otherwise (byte-compatible with the pre-M69 status). The damping guards
// (cooldown, two-version flap detector, freeze-after-auto-action, healthy-target)
// read this to decide whether to honour a `agents.ctxmesh.ai/rollback=<version>`
// annotation.
type RollbackStatus struct {
	// rolledBackTo is the AgentVersion the serving spec was last reverted to by a
	// human rollback. Empty until the first successful rollback.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	RolledBackTo string `json:"rolledBackTo,omitempty"`

	// lastRollbackAt is when the last SUCCESSFUL rollback was actuated. The cooldown
	// guard refuses a second rollback within rollbackCooldown of this time.
	// +optional
	LastRollbackAt *metav1.Time `json:"lastRollbackAt,omitempty"`

	// history is the recent rollback events (most-recent-first, bounded), the input
	// to the two-version flap detector: a rollback TO a version that appears as a
	// fromVersion within the flap window is refused.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	History []RollbackEvent `json:"history,omitempty"`

	// frozenUntilAck, when true, freezes further actions on this deployment until a human
	// acknowledges (ADR 0062 Fork 4 damping (d)) — the anti-runaway guard. An auto-action
	// (auto-rollback) SETS it; the human ACKS by CLEARING it (agents.ctxmesh.ai/rollback-ack),
	// which then permits the next action. NOTE: while frozen, `rollbackGuards` refuses ALL
	// rollbacks including a human-driven one — the human must clear the freeze first, THEN roll
	// back (the ack is the human's deliberate resume). Auto-progression (ADR 0113) RESPECTS this
	// freeze (holds) but never SETS it (forward motion is already bounded).
	// +optional
	FrozenUntilAck bool `json:"frozenUntilAck,omitempty"`
}

// RolloutStatus reports the auto-progression actuator state for a canary (M139/N4, ADR 0113). It is keyed
// by candidateRevision (a new candidate resets progression) and stores the LIVE currentPercent (not a step
// index — survives a spec `steps` edit; the next step is the first schedule entry > currentPercent).
type RolloutStatus struct {
	// candidateRevision pins this progression to a specific candidate Knative revision. When it no longer
	// matches the current candidate (a new push mid-canary), the controller resets progression to step 0.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	CandidateRevision string `json:"candidateRevision,omitempty"`

	// currentPercent is the candidate-arm traffic percent auto-progression has advanced to. 0/absent ⇒
	// spec.canaryPercent (the implicit step 0). The controller converges the ksvc split to this level.
	// +optional
	CurrentPercent int32 `json:"currentPercent,omitempty"`

	// lastAdvanceAt is when the controller last advanced a step (or opened the canary) — the dwell clock.
	// +optional
	LastAdvanceAt *metav1.Time `json:"lastAdvanceAt,omitempty"`

	// reason is the most recent auto-progression outcome for operator visibility (e.g. Advanced,
	// AutoProgressHeld, InsufficientData, Frozen, AutoPromoted).
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Reason string `json:"reason,omitempty"`
}

// CapabilityDescriptor advertises what an agent can do, so peers can DISCOVER it by capability instead of
// by DNS name (M141, ADR 0120). The shape deliberately mirrors the A2A Agent Card's skill advertisement
// (a natural-language description plus coarse tags) — the prevailing standard for agent capability
// publication — rather than a bespoke taxonomy: the discovery stack is semantic (embeddings +
// cross-encoder rerank), and prose is what those models consume natively. A structured skill
// hierarchy remains a future, additive extension.
type CapabilityDescriptor struct {
	// description is a SHORT natural-language statement of the agent's capability, written for a peer to
	// match against ("Summarizes long documents and extracts action items."). This is the text the control
	// plane embeds, so it carries the semantic weight — an empty description makes the agent
	// undiscoverable even if tags are set.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description"`

	// tags are coarse, lowercase capability labels ("summarization", "pdf", "sql") used to FILTER the
	// candidate set before semantic ranking, and appended to the embedded text as extra lexical signal.
	// Tags narrow; they never rank. Bounded at 16 entries of at most 63 characters.
	// +listType=atomic
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	Tags []string `json:"tags,omitempty"`
}

// AgentDeploymentStatus defines the observed state of AgentDeployment.
type AgentDeploymentStatus struct {
	// conditions reflect the current reconciliation state of the AgentDeployment.
	// The "Ready" condition mirrors the underlying Knative Service Ready condition.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// gate reports the deploy-gate state when the AgentDeployment references an
	// EvalSuite (spec.evalSuiteRef). Nil when no gate is active — a deployment
	// without an evalSuiteRef is byte-compatible with the pre-M9 status (PRD §17).
	// +optional
	Gate *GateStatus `json:"gate,omitempty"`

	// rollback reports the human-rollback actuator state (ADR 0062 Fork 4, M69). Nil
	// until a `agents.ctxmesh.ai/rollback=<version>` annotation is first evaluated —
	// byte-compatible with the pre-M69 status. The AUTO-rollback trigger is DEFERRED
	// (PRD §17.4); this records only human-actuated rollbacks + their damping state.
	// +optional
	Rollback *RollbackStatus `json:"rollback,omitempty"`

	// rollout reports the auto-progression actuator state (M139/N4, ADR 0113). Nil when
	// autoProgress is off / no canary. Keyed by candidateRevision (a new candidate resets
	// progression) + currentPercent (the live step, not an index — survives spec edits).
	// +optional
	Rollout *RolloutStatus `json:"rollout,omitempty"`

	// url is the public HTTP endpoint assigned to the agent, copied verbatim from
	// the Knative Service status once it becomes ready.
	// +optional
	URL string `json:"url,omitempty"`

	// latestVersion is the name of the most recently created AgentVersion snapshot
	// for this deployment, e.g. "echo-agent-7d9f4c1a".
	// +optional
	LatestVersion string `json:"latestVersion,omitempty"`

	// observedGeneration is the .metadata.generation that was last fully reconciled.
	// Used to detect spec changes that have not yet been processed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:deprecatedversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories={agents}
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.url"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.latestVersion"
// +kubebuilder:printcolumn:name="Gate",type="string",JSONPath=".status.gate.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 44",message="metadata.name must be at most 44 characters: the controller appends a 19-character revision-name suffix and Knative revision names are DNS-1035 labels capped at 63 characters"

// AgentDeployment is the Schema for the agentdeployments API.
// It describes a long-running AI agent deployed as a Knative Service.
// The controller reconciles each AgentDeployment into an immutable AgentVersion
// snapshot and a Knative Service that serves HTTP traffic on /invoke.
//
// The name-length CEL rule above guards the revision-name budget: the
// controller derives Knative revision names as "<name>-<specHash8>" plus, when
// any binding resolves, "-h<digest8>" — a suffix bounded at 19 characters.
// 63 (DNS-1035 label max) - 19 = 44 chars of name budget. Without the guard an
// admission-valid 45+-char name would silently wedge reconcile at the Knative
// webhook.
type AgentDeployment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard Kubernetes object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of the AgentDeployment.
	// +required
	Spec AgentDeploymentSpec `json:"spec"`

	// status defines the observed state of the AgentDeployment.
	// +optional
	Status AgentDeploymentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgentDeploymentList contains a list of AgentDeployment.
type AgentDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AgentDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *k8sruntime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AgentDeployment{}, &AgentDeploymentList{})
		return nil
	})
}
