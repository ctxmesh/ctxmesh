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
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/eval"
	"github.com/ctxmesh/agent-engine/internal/gateway"
	"github.com/ctxmesh/agent-engine/internal/prompt"
	"github.com/ctxmesh/agent-engine/internal/telemetry"
	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

// conditionReady is the condition type name mirrored from the Knative Service
// into AgentDeployment.status.conditions. Kept as a named constant to satisfy
// the goconst linter and to make the value easy to grep.
const conditionReady = "Ready"

// reasonIdentitySAConflict is set on the AgentDeployment Ready condition when
// the per-agent identity ServiceAccount (agent-<name>) already exists and is
// owned by a different controller. The reconcile stops cleanly (no requeue /
// no hot-loop) — the conflict must be resolved by a human before the agent can
// run. Mirrors the promptResolveError / guardrailResolveError pattern.
const reasonIdentitySAConflict = "IdentitySAConflict"

// executionModel values (mirror the CRD enum on AgentDeployment.spec.executionModel).
const (
	execModelServing  = "serving"
	execModelEventing = "eventing"
	execModelJob      = "job"
)

// brokerNameSuffix is appended to the agent's registry name to reference the
// per-registry Knative Eventing Broker (specs/eventing-scaling.md "Broker per
// registry"). The Broker itself is created by the AgentRegistry controller
// (m7.6); the eventing-model AgentDeployment only references it by name from its
// Trigger.
const brokerNameSuffix = "-broker"

// ceTypeAttribute is the CloudEvent context attribute the eventing Trigger
// filters on. An async A2A CloudEvent carries the target agent name as its
// `type` (specs/eventing-scaling.md §12.6: CloudEvent attributes mirror
// routing, `type` = the target/topic), so a Trigger filtered to
// `type == <agentName>` subscribes exactly this agent's events from the
// shared registry broker.
const ceTypeAttribute = "type"

// Blob-offload object store (m7.6b, specs/eventing-scaling.md §"Blob offload").
// A registry member's launcher offloads a >256KiB async payload to the dedicated
// dev MinIO (config/objectstore/) and rehydrates it on consume. The address and
// the DEV-ONLY deterministic credentials are injected as STATIC env — never
// valueFrom (Knative's ksvc webhook rejects it; the m5.7 landmine + tier1 guard).
const (
	// objectStoreAddr is the cluster address of the dedicated dev MinIO Service
	// (config/objectstore/, wired into config/default). It mirrors the S3 API
	// port; the launcher connects to it plain-HTTP in-cluster (dev posture, like
	// the dev Valkey). One store serves every registry member.
	objectStoreAddr = "agent-engine-objectstore.agent-engine-system.svc:9000"

	// objectStoreDevAccessKey / objectStoreDevSecretKey are the DETERMINISTIC
	// DEV-ONLY credentials for the dev MinIO — fixed values committed as such
	// (identical posture to the dev Langfuse / Valkey: NOT a real credential,
	// never rotated, only ever meaningful against the in-cluster dev MinIO). They
	// MUST match the values the config/objectstore/ MinIO Deployment is seeded
	// with. Injected as static env so the launcher authenticates to the dev store.
	objectStoreDevAccessKey = "agent-engine-dev"
	objectStoreDevSecretKey = "agent-engine-dev-secret" //nolint:gosec // dev-only fixed value, not a real credential (see comment).

	// Env-var NAMES for the durable object-store wiring — the launcher (blob offload, M78 record
	// fixtures) and the record-capable egress sidecar (M78 tool fixtures) read them. Named
	// constants so the several injection sites share one spelling (goconst).
	envObjectStoreAddr      = "OBJECT_STORE_ADDR"
	envObjectStoreAccessKey = "OBJECT_STORE_ACCESS_KEY"
	envObjectStoreSecretKey = "OBJECT_STORE_SECRET_KEY" //nolint:gosec // env-var NAME, not a secret value.
)

// objectStoreEnv returns the STATIC env-var triple wiring a container to the dev durable object
// store (address + dev creds). One helper for every injection site (launcher blob offload, M78
// record fixture sinks) so the names + values stay in lockstep. Values are reconcile-time
// constants — NEVER valueFrom (the m5.7 Knative landmine / tier1 no-valueFrom guard).
//
// OPS-2: the triple is DEV-ONLY (the bundled MinIO). It is injected ONLY when the dev data plane is
// enabled (devDataPlane=false ⇒ nil, no injection), so a `profile: production` install never ships
// the dev.local object-store creds into agent pods — a record-capable agent there then C2-fails-
// closed on the absent store rather than pointing at a non-existent dev MinIO (the correct posture:
// production has not provisioned a durable store, so record/blob-offload is feature-off).
func objectStoreEnv(devDataPlane bool) []corev1.EnvVar {
	if !devDataPlane {
		return nil
	}
	return []corev1.EnvVar{
		{Name: envObjectStoreAddr, Value: objectStoreAddr},
		{Name: envObjectStoreAccessKey, Value: objectStoreDevAccessKey},
		{Name: envObjectStoreSecretKey, Value: objectStoreDevSecretKey},
	}
}

const (
	// litellmGatewayURL is the in-cluster address of the LiteLLM model gateway.
	// It is the DEFAULT MODEL_GATEWAY_URL for an unbudgeted agent (the agent calls
	// LiteLLM directly, the M2 path). For a BUDGETED agent (spec.budget set) the
	// controller repoints MODEL_GATEWAY_URL at the launcher's local budget proxy
	// (budgetProxyURL) and passes this address through as GATEWAY_UPSTREAM_URL so
	// the proxy still forwards to LiteLLM after enforcing the cost cap.
	litellmGatewayURL = "http://agent-engine-gateway.agent-engine-system.svc:4000"

	// budgetProxyURL is where MODEL_GATEWAY_URL points when a budget is set: the
	// launcher's OWN outbound gateway proxy (:2996, cmd/launcher/gateway.go). The
	// proxy runs inside the same pod, so localhost. Its port must match
	// defaultGatewayProxyPort in the launcher.
	budgetProxyURL = "http://localhost:2996"

	// envAgentName is the AGENT_NAME env var — the agent's identity, injected
	// unconditionally in the base env (the tracing identity) and reused by the
	// memory, registry, and budget paths (each guards against double-inject).
	envAgentName = "AGENT_NAME"

	// envPodNamespace is the POD_NAMESPACE env var — the agent's namespace. Injected
	// unconditionally in the base env so the launcher can form the UNAMBIGUOUS
	// `<namespace>/<name>` trace identity; also consumed by the A2A path (cluster
	// host resolution) and the memory key namespace. STATIC (deploy.Namespace),
	// NEVER a downward-API valueFrom (the m5.7 Knative ksvc landmine).
	envPodNamespace = "POD_NAMESPACE"
	// envTokenServiceURL points the launcher at the control-plane token-service (OBO + long-term
	// memory proxy, ADR 0045 Amд 1). Reused from the OBO egress config.
	envTokenServiceURL = "TOKEN_SERVICE_URL"
	// envMCPCapPublicKey / envMCPCapAudience let the launcher VERIFY a run capability for per-user
	// long-term memory (ADR 0045) — the same envs the OBO egress sidecar uses.
	envMCPCapPublicKey = "MCP_CAPABILITY_PUBLIC_KEY"
	envMCPCapAudience  = "MCP_CAPABILITY_AUDIENCE"

	// memoryScopeShared is the MemoryBinding scope + MEMORY_SCOPE env value that selects the shared
	// team scratchpad (ADR 0035, m33.3) instead of private per-agent memory.
	memoryScopeShared = "shared"

	// State-layer proxy pod-identity token (M53, ADR 0050 §8 phase 2 + Amд 3): when the
	// tenant quota routes through the proxy, the controller mounts a PROJECTED
	// serviceAccountToken (a Knative-allowed volume, NEVER valueFrom) bound to the
	// proxy audience; the launcher presents it so the proxy derives the tenant from the
	// pod's namespace. The mount path + audience + expiry must match the launcher
	// (defaultPodTokenPath) and the proxy (STATELAYER_POD_AUDIENCE) respectively.
	envStatelayerProxyURL      = "STATELAYER_PROXY_URL"
	envStatelayerTokenPath     = "STATELAYER_TOKEN_PATH"
	statelayerTokenVolume      = "statelayer-proxy-token"
	statelayerTokenMountPath   = "/var/run/secrets/statelayer-proxy"
	statelayerPodTokenFilePath = statelayerTokenMountPath + "/token"
	statelayerPodAudience      = "statelayer-proxy"
	statelayerTokenExpirySecs  = int64(600)
)

// Feedback ingest hook (M9, specs/eval-prompts-feedback.md §3). The :2995
// listener is started by the launcher when these env vars are injected. All
// values are known at reconcile time → STATIC env, NEVER valueFrom (Knative
// ksvc webhook rejects valueFrom; the m5.7 landmine + tier1 no-valueFrom guard).
const (
	// langfuseHost is the in-cluster Langfuse base URL. The feedback hook POSTs
	// scores to <langfuseHost>/api/public/scores. Reuses the dev Langfuse wired
	// by `make -C harness dev-up M=3` (same host as the M3 OTel collector exporter).
	langfuseHost = "http://langfuse-web.langfuse.svc:3000"

	// langfuseDevPublicKey / langfuseDevSecretKey are the DETERMINISTIC DEV-ONLY
	// Langfuse API credentials — fixed values committed as such (identical posture
	// to the dev MinIO OBJECT_STORE_ACCESS_KEY / objectStoreDevAccessKey). They
	// MUST match the public/secret key seeded by `dev-up M=3` into the
	// langfuse-otlp Secret (and into the Langfuse Helm chart's initialApiKey
	// block). NOT a real credential — never rotated, only ever meaningful against
	// the in-cluster dev Langfuse. Injected as STATIC env (no valueFrom) so the
	// launcher's feedback hook can authenticate to the dev scores API.
	langfuseDevPublicKey = "pk-lf-dev-00000000000000000000000000000000"
	langfuseDevSecretKey = "sk-lf-dev-00000000000000000000000000000000" //nolint:gosec // dev-only fixed value, not a real credential (see comment).

	// feedbackPort is the localhost port the launcher's feedback hook binds. Must
	// match defaultFeedbackPort in cmd/launcher/feedback.go. Reserved per
	// specs/agent-mesh.md; must NOT be :2996/:2997/:2998/:2999.
	feedbackPort = "2995"
)

// jobBackoffLimit bounds retries for a one-shot job-model agent. Kept small: a
// job agent runs to completion; a handful of retries covers a transient
// image-pull / node-eviction failure without wedging a poison run
// (specs/eventing-scaling.md — the launcher fronts the container; the pod exits
// when the agent completes).
const jobBackoffLimit int32 = 2

// AgentDeploymentReconciler reconciles a AgentDeployment object.
// The reconcile loop implements a four-step flow (agent-brain specs/agent-serving.md):
//
//  1. Fetch; guard unsupported executionModel.
//  2. Hash spec → ensure AgentVersion snapshot exists.
//  3. CreateOrUpdate the Knative Service (same name/namespace, controller owner ref).
//  4. Mirror ksvc Ready condition + URL into AgentDeployment.status.
//
// Create-or-update strategy: ctrl.CreateOrUpdate (not server-side apply).
// Rationale: SSA introduces TypeMeta/managed-field bookkeeping complexity that is
// unnecessary for M1; CreateOrUpdate is fully idempotent and simpler to test in envtest.
// We retain full spec ownership on every reconcile, so drift is overwritten regardless.
type AgentDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader is an UNCACHED reader (the manager's API reader). The collector's
	// Langfuse telemetry Secret is read through it, NOT the cache: a cached read is
	// racy around informer resync and can render a collector sidecar WITHOUT the
	// LANGFUSE_OTLP env while its ConfigMap already references it — which crash-loops
	// the sidecar. Nil in tests (envtest) ⇒ the reconciler falls back to the cached
	// client, whose reads are consistent there.
	APIReader client.Reader

	// OBOEgress configures OBO egress-sidecar injection (ADR 0030). Disabled (default)
	// ⇒ no sidecar is injected and the pod template is unchanged (no drift).
	OBOEgress OBOEgressConfig

	// CollectorImage / DiscoveryImage override the injected OTel-collector / tool-discovery
	// sidecar images (audit OPS-1; from COLLECTOR_IMAGE / DISCOVERY_IMAGE env, mirror of
	// EGRESS_SIDECAR_IMAGE). Empty ⇒ the project-default constants — which are dev.local/*
	// and ImagePullBackOff off a kind cluster, so a real install MUST set these to a
	// reachable registry (wired through the chart, OPS-4).
	CollectorImage string
	DiscoveryImage string

	// DevDataPlane gates injection of the DEV-ONLY blob-offload/record object-store creds and the
	// dev Langfuse feedback-scores creds into agent pods (OPS-2). Set from the controller's
	// DEV_DATA_PLANE env, which the Helm chart templates from `.Values.devDataPlane.enabled`
	// (default true == the kustomize dev posture; a `profile: production` install sets it false).
	// False ⇒ neither dev credential family is injected, so a production render never ships the
	// bundled dev.local creds — the launcher already treats an absent object store / LANGFUSE_HOST
	// as feature-off (blob offload / record fixtures / feedback scores simply do not run). The
	// creds themselves remain deterministic dev-only non-secrets; this gate governs INJECTION.
	DevDataPlane bool

	// StatelayerProxyURL, when set (from the controller's STATELAYER_PROXY_URL env),
	// is injected into memory-bound agents so the launcher reverse-proxies session/
	// shared memory to the control-plane state-layer proxy instead of Valkey directly
	// (M51, ADR 0050 §8 phase 1 — opt-in dual-mode). Empty (default) ⇒ not injected,
	// agents keep the direct-Valkey path (no drift).
	StatelayerProxyURL string

	// LauncherImage, when set (from the controller's LAUNCHER_IMAGE env), enables C8 launcher injection
	// (ADR 0079): a launcher-inject initContainer stages the platform launcher into a shared emptyDir and
	// the user container's Command is overridden to exec it — so a launcher security fix rolls CENTRALLY
	// (a controller roll → a new revision) instead of a fleet image rebuild. Empty (default) ⇒ no injection
	// (fully backward-compatible: baked-launcher images run unchanged). MUST be a DIGEST-pinned reference
	// (fleet-RCE-equivalent config) and REQUIRES the Knative feature flags kubernetes.podspec-init-containers
	// + kubernetes.podspec-volumes-emptydir (+ -securitycontext) enabled cluster-wide — else the ksvc is
	// REJECTED. Confirm the flags before setting this (m92.2).
	LauncherImage string

	// PromptResolver resolves a PromptVersion git pointer (repo, ref, path) into
	// prompt content for the prompt-only-deploy path (M9). It is the mock⇄real
	// seam: production wires a real (e.g. go-git) resolver; dev / envtest / e2e
	// leave it nil and the reconciler defaults to the deterministic, OFFLINE
	// fixture resolver (prompt.NewFixtureResolver) — no network in CI (ADR 0004).
	PromptResolver prompt.Resolver

	// EvalTracer emits the eval.gate span for the deploy-gate decision (M9). Left
	// nil in dev / envtest / e2e (the controller has no live OTel export wired —
	// the launcher owns runtime spans), where the gate defaults to a no-op tracer
	// so the decision path is exercised OFFLINE; a production build injects a real
	// tracer here to land eval.gate in the trace tree. Mirrors the PromptResolver
	// seam.
	EvalTracer trace.Tracer

	// ScorerFactory builds the Scorer for an EvalSuite scorer (type, name) — the
	// mock⇄Langfuse seam for the deploy gate (M9). Left nil in production and dev
	// (it defaults to eval.ScorerFor: the deterministic mock, Langfuse-unavailable
	// for llm-judge/code offline); envtest/e2e inject a seeded factory to drive a
	// candidate deliberately above/below threshold and exercise the state machine.
	ScorerFactory func(scorerType, name string) (eval.Scorer, error)

	// Registry reads referenced ToolRegistries when resolving an agent's valid
	// bindings for the pod template (ADR 0043, the RegistryReader seam). Nil ⇒ the
	// CRD-backed default (K8s API). At the M43 read-switch this MUST be wired from
	// the SAME reader instance as the MCPToolBinding reconciler's Registry — the
	// pod template and the pushed manifest are computed by the same
	// resolveAgentBindings logic, so different readers would silently drift them.
	Registry RegistryReader

	// OnlineScore is the control-plane online-score aggregate store (from cpDB, the SAME
	// store the RegressionDetectorReconciler reads). The HUMAN rollback actuator's
	// healthy-target damping guard (m69.8, ADR 0062 Fork 4 (c)) reads it to refuse a
	// rollback to a version that itself regressed vs its baseline. nil (dev without cpDB,
	// or envtests that don't exercise the store) ⇒ the store-backed half of the
	// healthy-target check is skipped; the condition-based half (RegressionDetected on the
	// serving version) still applies. This is GUARD-side only — it is never used to
	// auto-trigger a rollback (the auto-trigger is deferred, PRD §17.4).
	OnlineScore onlineScoreReader

	// OnlineConfig publishes each agent's online-scoring policy to cpDB (m84.3, ADR 0062 Fork 2 / ADR 0011):
	// the controller resolves spec.evalSuiteRef → EvalSuite.spec.online and UPSERTS/DELETES the per-(ns,
	// agent) config row that the BFF online-scoring worker READS (cpDB, no agent-CRD RBAC on the BFF SA).
	// This is the ADR-0011-safe replacement for the reverted BFF-SA agent-CRD ClusterRole — the CONTROLLER
	// writes (it has evalsuites RBAC), the worker reads. nil (dev without cpDB, or envtests not exercising
	// it) ⇒ the publish step is a no-op (the worker safely defaults to judge-OFF on a missing row).
	OnlineConfig onlineConfigWriter
}

// registryReader returns the configured RegistryReader. REQUIRED post-retirement
// (ADR 0044) — there is no CRD to fall back to (main.go wires the Postgres reader;
// envtests inject a memstore-backed one).
func (r *AgentDeploymentReconciler) registryReader() RegistryReader {
	return r.Registry
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentversions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentscalingpolicies,verbs=get;list;watch
// PromptVersion retired to Postgres (ADR 0044) — the controller reads the resolved-prompt annotation, no CRD RBAC.
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=evalsuites,verbs=get;list;watch
// +kubebuilder:rbac:groups=serving.knative.dev,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=eventing.knative.dev,resources=triggers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements the AgentDeployment reconciliation loop.
func (r *AgentDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// ── Step 1: Fetch ────────────────────────────────────────────────────────
	var deploy agentsv1alpha1.AgentDeployment
	if err := r.Get(ctx, req.NamespacedName, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching AgentDeployment: %w", err)
	}

	// ── Step 1b: Human rollback actuator (m69.8, ADR 0062 Fork 4) ─────────────
	// A `agents.ctxmesh.ai/rollback=<agentversion>` annotation reverts the serving spec to
	// that version's snapshot, guarded by the damping mechanism. It runs BEFORE the spec-hash
	// / workload steps because a rollback rewrites spec — the ordinary path must reconcile the
	// REVERTED spec, not the pre-revert one. The revert is a spec Update that re-enqueues the
	// object, so the following reconcile rolls the workload through the normal path (the
	// actuator never touches Knative traffic; the auto-trigger is DEFERRED, PRD §17.4).
	if handled, rerr := r.maybeRollback(ctx, &deploy); handled {
		return ctrl.Result{}, rerr
	}

	// Normalise the execution model. The CRD default fills "serving" for omitted
	// values; a belt-and-braces default here covers a client that submits the
	// raw object without defaulting (e.g. the raw envtest client on an older
	// stored object). The CRD enum already rejects any value outside the set.
	model := deploy.Spec.ExecutionModel
	if model == "" {
		model = execModelServing
	}

	// ── Step 1c: Publish the online-scoring policy to cpDB (m84.3, ADR 0062 Fork 2 / ADR 0011) ─
	// Resolve spec.evalSuiteRef → EvalSuite.spec.online and UPSERT/CLEAR the per-(ns, agent) config row
	// the BFF online-scoring worker reads (cpDB, no agent-CRD RBAC on the BFF SA — the controller already
	// holds evalsuites RBAC). This is the ADR-0011-safe replacement for the reverted BFF-SA ClusterRole:
	// the CONTROLLER writes, the worker reads. It is a cpDB side-effect (never a k8s mutation) — idempotent
	// and it does NOT gate the deploy: a cpDB blip is logged, never a reconcile error. No-op when unwired.
	r.reconcileOnlineScoreConfig(ctx, &deploy)

	// ── Step 2: Spec hash → AgentVersion ─────────────────────────────────────
	hash, err := specHash(deploy.Spec)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("computing spec hash: %w", err)
	}
	versionName := deploy.Name + "-" + hash
	log.Info("Ensuring AgentVersion", "version", versionName)

	if err = r.ensureAgentVersion(ctx, &deploy, versionName); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring AgentVersion %s: %w", versionName, err)
	}

	// ── Step 3: Reconcile the workload by execution model ─────────────────────
	// The pod template (env injection, container digest, sidecars) is built the
	// same way for every model; the model only decides the WORKLOAD KIND that
	// wraps it. Switching models is a CREATE of the new kind + a DELETE of the
	// other kinds — a different object, not a ksvc revision roll — so each branch
	// tears down the workloads the other branches own before writing its own.
	log.Info("Reconciling workload", "name", deploy.Name, "executionModel", model)
	result, err := r.reconcileWorkload(ctx, &deploy, model, hash, versionName)

	// Prompt-only deploy (M9): a promptResolveError is USER input (missing
	// PromptVersion / unresolvable git ref/path), surfaced from buildPodTemplate
	// BEFORE any workload write (the ksvc CreateOrUpdate is never reached, so the
	// OLD revision keeps serving — no half-applied prompt swap). Report it on
	// status as Ready=False and STOP cleanly (no requeue on user input), rather
	// than returning a hard reconcile error that would log-spam and back off.
	if pe, ok := asPromptResolveError(err); ok {
		return r.setReadyFalse(ctx, &deploy, pe.reason, pe.msg)
	}
	// Guardrails control-plane FAIL-CLOSED (M66, ADR 0059 §8): a dangling / invalid
	// guardrailPolicyRef is surfaced from buildPodTemplate as a *guardrailResolveError
	// BEFORE any workload write (the ksvc CreateOrUpdate is never reached, so the OLD
	// revision — guarded, or nonexistent — keeps serving; the controller NEVER injects a
	// "no-guardrail" MODEL_GATEWAY_URL). Report Ready=False and STOP cleanly (no requeue
	// on user input). A GuardrailPolicy create/fix re-reconciles via the watch below.
	if ge, ok := asGuardrailResolveError(err); ok {
		return r.setReadyFalse(ctx, &deploy, ge.reason, ge.msg)
	}
	// In-pod tool-call governance FAIL-CLOSED (m82.3, ADR 0074 §2): a sidecar-mode (in-pod) tool with
	// an effective require-approval rule is surfaced from buildPodTemplate as an *inPodRequireApprovalError
	// BEFORE any workload write (no half-governed pod — the ksvc CreateOrUpdate is never reached, so the
	// OLD revision keeps serving). require-approval is not enforceable on an in-pod tool (it binds a
	// localhost port in the shared netns, uncapturable by the approval voucher). Report Ready=False and
	// STOP cleanly (no requeue on user input); a spec fix (drop the rule, or make the tool remote/OBO)
	// re-reconciles via the watch.
	if ie, ok := asInPodRequireApprovalError(err); ok {
		return r.setReadyFalse(ctx, &deploy, ie.reason, ie.msg)
	}
	// SA name-collision (m79.1, m52 C11): an agent-<name> SA already owned by a
	// DIFFERENT controller must fail LOUD, not wedge the reconcile in a hot-loop.
	// Surface it as Ready=False IdentitySAConflict and STOP cleanly (nil error ⇒
	// no requeue). The user must delete or rename the conflicting SA; a re-watch
	// will re-reconcile once the SA is gone.
	if ce, ok := asIdentitySAConflictError(err); ok {
		return r.setReadyFalse(ctx, &deploy, reasonIdentitySAConflict, ce.Error())
	}
	return result, err
}

// reconcileWorkload dispatches to the per-execution-model reconciler. It exists
// so Reconcile can intercept a prompt-resolution user error (M9) uniformly across
// all three models before returning.
func (r *AgentDeploymentReconciler) reconcileWorkload(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	model, hash, versionName string,
) (ctrl.Result, error) {
	switch model {
	case execModelEventing:
		return r.reconcileEventing(ctx, deploy, hash, versionName)
	case execModelJob:
		return r.reconcileJob(ctx, deploy, versionName)
	default: // execModelServing
		return r.reconcileServing(ctx, deploy, hash, versionName)
	}
}

// reconcileServing wraps the pod template in a Knative Service (the M1-M6 path,
// unchanged for non-eventing/non-job agents) and mirrors status. It also applies
// request-rate / custom-metric Knative autoscaling annotations onto the ksvc
// template when an AgentScalingPolicy targets the agent (m7.5 single-writer: the
// AgentDeployment reconciler owns the ksvc and its autoscaling annotations; the
// AgentScalingPolicy controller owns only the KEDA ScaledObject).
func (r *AgentDeploymentReconciler) reconcileServing(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	hash, versionName string,
) (ctrl.Result, error) {
	// Tear down the workloads owned by the other models (transition handling):
	// the job-model Job/CronJob and the eventing Deployment + Service + Trigger.
	if err := r.deleteWorkload(ctx, deploy, &batchv1.Job{}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteWorkload(ctx, deploy, &batchv1.CronJob{}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteWorkload(ctx, deploy, &eventingv1.Trigger{}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteWorkload(ctx, deploy, &appsv1.Deployment{}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteNamedWorkload(ctx, deploy, &corev1.Service{}, eventingServiceName(deploy.Name)); err != nil {
		return ctrl.Result{}, err
	}

	// Deploy gate (M9): when the agent references an EvalSuite, score the CANDIDATE
	// revision and decide before any ksvc write. The gate HOLDS the rollout by
	// gating the ksvc apply: on `blocked` (below threshold, gate:block) or
	// `awaiting-promotion` (passing but not yet human-approved) the candidate is NOT
	// applied — on a FIRST deploy nothing goes live; on an UPDATE the existing ksvc
	// (the previous revision) is left untouched, so the previous revision keeps
	// serving. Only `promoted`/`warned` proceed to the ksvc CreateOrUpdate. This is
	// the scorer-agnostic mechanism: block-then-promote never overbuilds Knative
	// traffic-splitting — a held candidate is simply never written.
	candidateRev, err := r.candidateRevisionName(ctx, deploy, hash)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Resolve the OLD serving revision from the LIVE ksvc BEFORE any candidate write —
	// it is the "old" arm of a canary split (the serving revision that is NOT the
	// candidate). Read here (pre-apply) so the split pins the revision genuinely serving
	// today. "" ⇒ no ksvc yet, or the only serving revision IS the candidate (no old arm).
	oldRev, err := r.currentServingRevision(ctx, deploy, candidateRev)
	if err != nil {
		return ctrl.Result{}, err
	}

	outcome, err := r.evaluateGate(ctx, deploy, candidateRev)
	if err != nil {
		// A missing/invalid EvalSuite is USER input (surfaced from evaluateGate as an
		// evalGateError): report Ready=False and STOP cleanly (the old revision keeps
		// serving), rather than returning a hard reconcile error that log-spams.
		if ge, ok := asEvalGateError(err); ok {
			return r.setGateBlockedStatus(ctx, deploy, ge.reason, ge.Error())
		}
		return ctrl.Result{}, fmt.Errorf("evaluating deploy gate: %w", err)
	}
	// A PASSING candidate on a CANARY-configured serving deployment with a distinct OLD
	// serving revision takes the canary path (ADR 0062 Fork 3) — for BOTH the
	// unapproved case (hold at a named {old, candidate:N%} split so both arms accumulate
	// online scores) AND the human-completion case (promote=<candidate> → 100% candidate,
	// abort → 100% old). The ordinary promote path leaves ksvc.Spec.Traffic alone, so a
	// canary MUST route through reconcileCanary to set the named split/completion traffic.
	// A gate:warn/blocked candidate (Decision != promoted) never canaries. A first deploy
	// with no old arm (oldRev == "") is a degenerate canary → fall through to the ordinary
	// path (hold at 0% until promoted, or promote-all — unchanged).
	if outcome != nil && outcome.status.Decision == eval.DecisionPromoted &&
		canaryConfigured(deploy, execModelServing) && oldRev != "" && oldRev != candidateRev {
		return r.reconcileCanary(ctx, deploy, hash, versionName, candidateRev, oldRev, outcome)
	}
	if outcome != nil && !outcome.promote {
		// Gate held the rollout (blocked or awaiting-promotion): do NOT apply the
		// candidate ksvc. Record the gate status and stop — the previous revision (if
		// any) keeps serving. A later re-reconcile (spec change, or the human-approval
		// annotation) re-evaluates and promotes.
		return r.recordHeldGate(ctx, deploy, versionName, outcome)
	}

	ksvc, err := r.reconcileKnativeService(ctx, deploy, hash)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling Knative Service: %w", err)
	}
	if err = r.syncStatus(ctx, deploy, ksvc, versionName); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing status: %w", err)
	}
	// Promoted/warned: record the terminal gate status (+ annotations) alongside the
	// applied workload. A gated agent that reaches here has an outcome; an ungated
	// agent (outcome == nil) leaves status.gate nil, byte-compatible with pre-M9.
	if outcome != nil {
		if err = r.recordPromotedGate(ctx, deploy, outcome); err != nil {
			return ctrl.Result{}, fmt.Errorf("recording gate status: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

// candidateRevisionName computes the Knative revision name the candidate WOULD
// serve — "<name>-<specHash>" plus, when a binding/membership/prompt resolves,
// "-h<digest8>". It builds the pod template (idempotent — the same call
// reconcileKnativeService makes) to derive the digest so the gate scores and pins
// the EXACT candidate the ksvc would create.
func (r *AgentDeploymentReconciler) candidateRevisionName(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	hash string,
) (string, error) {
	pod, err := r.buildPodTemplate(ctx, deploy)
	if err != nil {
		return "", fmt.Errorf("building pod template for candidate revision: %w", err)
	}
	revName := deploy.Name + "-" + hash
	if pod.digest != "" {
		revName = revName + "-h" + pod.digest
	}
	return revName, nil
}

// currentServingRevision returns the "old" arm for a canary split against
// candidateRev: the revision the agent's Knative Service serves today that is NOT
// the candidate. Returns "" when no ksvc exists yet OR the only serving revision is
// the candidate (a first deploy / already-promoted candidate has no distinct old
// arm). A NotFound ksvc is not an error — it just means there is no old revision to
// split against. See oldServingRevision for the resolution order.
func (r *AgentDeploymentReconciler) currentServingRevision(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	candidateRev string,
) (string, error) {
	var ksvc servingv1.Service
	err := r.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: deploy.Name}, &ksvc)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading current ksvc for serving revision: %w", err)
	}
	return oldServingRevision(&ksvc, candidateRev), nil
}

// reconcileEventing wraps the pod template in a plain apps/v1 Deployment + a
// core/v1 Service (NOT a ksvc — the m7.8 e2e proved KEDA cannot resolve a Knative
// revision Deployment and KEDA/KPA would fight over replicas; see
// specs/eventing-scaling.md "Why eventing is a plain Deployment, not a ksvc").
// The Deployment is named `<agent>` so a queue-depth KEDA ScaledObject
// (scaleTargetRef = the AgentDeployment name, kind Deployment) resolves it. A
// Knative Eventing Trigger subscribes the Service to the agent's registry broker.
//
// Eventing REQUIRES registry membership (the broker is per-registry); a non-member
// agent set to eventing is a user error reported as Ready=False, with the Trigger
// torn down but the Deployment + Service KEPT (owner-ref'd, GCs with the
// AgentDeployment) — a misconfigured eventing agent keeps a working HTTP endpoint
// while membership is fixed.
func (r *AgentDeploymentReconciler) reconcileEventing(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	_ /* hash */, versionName string,
) (ctrl.Result, error) {
	// Tear down the workloads owned by the OTHER models (transition handling):
	// the serving ksvc and the job-model Job/CronJob. The eventing Deployment +
	// Service replace them.
	if err := r.deleteWorkload(ctx, deploy, &servingv1.Service{}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteWorkload(ctx, deploy, &batchv1.Job{}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteWorkload(ctx, deploy, &batchv1.CronJob{}); err != nil {
		return ctrl.Result{}, err
	}

	// Build the model-independent pod template once (same launcher + agent +
	// collector + tool sidecars, env injection, digest as the serving ksvc).
	pod, err := r.buildPodTemplate(ctx, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building pod template: %w", err)
	}
	if err = r.ensureAgentIdentitySA(ctx, deploy, pod.serviceAccountName, pod.membership.RegistryID); err != nil {
		return ctrl.Result{}, err
	}

	// The Deployment + Service are always reconciled (member or not) so a
	// non-member eventing agent keeps a working HTTP endpoint while membership is
	// fixed. Only the Trigger (the broker subscription) is gated on membership.
	if err = r.reconcileEventingDeployment(ctx, deploy, pod); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling eventing Deployment: %w", err)
	}
	if err = r.reconcileEventingService(ctx, deploy, pod.port); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling eventing Service: %w", err)
	}

	if !pod.membership.IsMember {
		// Eventing needs a per-registry broker; without membership there is no
		// broker to subscribe to. Report the error and drop any stale Trigger so
		// no orphaned subscription lingers — but keep the Deployment + Service.
		if err = r.deleteWorkload(ctx, deploy, &eventingv1.Trigger{}); err != nil {
			return ctrl.Result{}, err
		}
		return r.setReadyFalse(ctx, deploy, "NotRegistryMember",
			"executionModel 'eventing' requires the agent to be a member of an AgentRegistry (the Trigger subscribes to the registry broker); no membership resolved")
	}

	if err = r.reconcileTrigger(ctx, deploy, pod.membership); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling Trigger: %w", err)
	}

	// Mirror the Deployment's readiness into status (no ksvc Ready condition).
	var dep appsv1.Deployment
	if err = r.Get(ctx, client.ObjectKey{Name: deploy.Name, Namespace: deploy.Namespace}, &dep); err != nil {
		return ctrl.Result{}, fmt.Errorf("fetching eventing Deployment for status: %w", err)
	}
	if err = r.setEventingReady(ctx, deploy, &dep, versionName); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing status: %w", err)
	}
	return ctrl.Result{}, nil
}

// reconcileJob wraps the pod template in a batch workload: a bare one-shot Job by
// default, or a CronJob when a `schedule` AgentScalingPolicy targets the agent.
// No ksvc / Trigger is emitted; any left over from a previous model is deleted.
func (r *AgentDeploymentReconciler) reconcileJob(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	versionName string,
) (ctrl.Result, error) {
	// Tear down the serving/eventing workloads (transition handling): the serving
	// ksvc and the eventing Deployment + Service + Trigger.
	if err := r.deleteWorkload(ctx, deploy, &servingv1.Service{}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteWorkload(ctx, deploy, &eventingv1.Trigger{}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteWorkload(ctx, deploy, &appsv1.Deployment{}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteNamedWorkload(ctx, deploy, &corev1.Service{}, eventingServiceName(deploy.Name)); err != nil {
		return ctrl.Result{}, err
	}

	pod, err := r.buildPodTemplate(ctx, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building pod template: %w", err)
	}
	if err = r.ensureAgentIdentitySA(ctx, deploy, pod.serviceAccountName, pod.membership.RegistryID); err != nil {
		return ctrl.Result{}, err
	}

	schedule, err := r.scheduleForAgent(ctx, deploy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving schedule policy: %w", err)
	}

	if schedule != "" {
		// A schedule policy targets this agent → CronJob (Job would be torn down).
		if err = r.deleteWorkload(ctx, deploy, &batchv1.Job{}); err != nil {
			return ctrl.Result{}, err
		}
		if err = r.reconcileCronJob(ctx, deploy, pod, schedule); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling CronJob: %w", err)
		}
	} else {
		if err = r.deleteWorkload(ctx, deploy, &batchv1.CronJob{}); err != nil {
			return ctrl.Result{}, err
		}
		if err = r.reconcileBatchJob(ctx, deploy, pod); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling Job: %w", err)
		}
	}

	// A job-model agent has no ksvc Ready condition to mirror; report Ready=True
	// once the workload is converged (the launcher runs to completion in-pod).
	if err = r.setJobReady(ctx, deploy, versionName, schedule != ""); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing status: %w", err)
	}
	return ctrl.Result{}, nil
}

// specHash returns the first 8 hex characters of the SHA-256 of the canonical
// JSON encoding of spec. The JSON encoder is deterministic for Go structs
// (fields in declaration order) and all leaf types used in AgentDeploymentSpec
// (strings, int32, resource.Quantity) serialise deterministically.
func specHash(spec agentsv1alpha1.AgentDeploymentSpec) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshalling spec to JSON: %w", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])[:8], nil
}

// ensureAgentVersion creates an AgentVersion named versionName if it does not
// already exist. Existing versions are never modified (spec is CEL-immutable).
func (r *AgentDeploymentReconciler) ensureAgentVersion(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	versionName string,
) error {
	var av agentsv1alpha1.AgentVersion
	err := r.Get(ctx, client.ObjectKey{Name: versionName, Namespace: deploy.Namespace}, &av)
	if err == nil {
		return nil // already exists — nothing to do
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	av = agentsv1alpha1.AgentVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name:      versionName,
			Namespace: deploy.Namespace,
		},
		Spec: agentsv1alpha1.AgentVersionSpec{
			DeploymentName: deploy.Name,
			Snapshot:       deploy.Spec,
		},
	}
	if err = ctrl.SetControllerReference(deploy, &av, r.Scheme); err != nil {
		return fmt.Errorf("setting owner ref on AgentVersion: %w", err)
	}
	return r.Create(ctx, &av)
}

// podTemplate is the fully-built, model-independent pod spec for an agent: the
// container list (user container + collector + tool sidecars), the volumes, the
// membership pod label, and the structural digest suffix (the "-h<digest8>"
// component that rolls the workload when a binding/membership changes). It is
// built once by buildPodTemplate and wrapped by the per-model reconcilers into a
// ksvc revision template (serving/eventing) or a batch Job pod template (job).
type podTemplate struct {
	// containers is the ordered container list (user container first).
	containers []corev1.Container
	// initContainers run to completion before the pod's containers start (C8, ADR 0079): the
	// launcher-inject initContainer that stages the platform launcher into the shared emptyDir. Empty
	// unless LAUNCHER_IMAGE is configured. NOTE: initContainers on a Knative serving ksvc require the
	// `kubernetes.podspec-init-containers` feature flag.
	initContainers []corev1.Container
	// volumes are the pod volumes (collector config, tools).
	volumes []corev1.Volume
	// labels are the pod-template labels (registry membership) or nil.
	labels map[string]string
	// digest is the combined structural digest ("" when no binding/membership
	// resolves), used as the "-h<digest>" revision/name suffix.
	digest string
	// membership is the resolved M6 registry context (used by the eventing
	// Trigger for the broker name and the CloudEvent filter).
	membership registryMembership
	// port is the resolved container port.
	port int32
	// serviceAccountName is the per-agent identity ServiceAccount the pod runs as
	// ("agent-<name>"), set ONLY when the pod presents a projected token to the
	// state-layer proxy (injectPodToken). It gives the proxy a cryptographic
	// (ns,agent) identity via TokenReview instead of the shared namespace default
	// SA — the basis for server-side per-agent scope on the memory/quota/dedup
	// paths (ADR 0052 §C6 RESOLUTION). Empty ⇒ the pod runs the default SA
	// (unchanged, no drift for non-proxy agents).
	serviceAccountName string
}

// buildPodTemplate resolves all M3-M6 injection (collector sidecar, MCP tool
// sidecars, memory env, registry membership env + label) and assembles the
// model-independent pod spec plus its combined structural digest. Every
// execution model reuses this so the container contract (env, digest,
// no-valueFrom Knative constraint) is identical whether the workload is a ksvc,
// a Trigger-backed ksvc, or a Job/CronJob.
//
//nolint:gocyclo // sequential injection steps (collector/tools/memory/registry) kept in one auditable place
func (r *AgentDeploymentReconciler) buildPodTemplate(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
) (podTemplate, error) {
	port := deploy.Spec.Port
	if port == 0 {
		port = 8080
	}

	// Platform env vars come first so they are always present; user env is appended
	// last and may override platform defaults if the operator deliberately sets the
	// same name (consistent with the treatment of AGENT_PORT).
	env := make([]corev1.EnvVar, 0, 2+len(deploy.Spec.Env))
	env = append(env, corev1.EnvVar{Name: "AGENT_PORT", Value: strconv.Itoa(int(port))})

	// Tenancy (M47, ADR 0046): resolve the owning tenant EARLY — it decides both whether the gateway
	// quota proxy is interposed (a tenant's model caps flow through the SAME launcher proxy as an M8
	// budget) and the TENANT_* env injected below. Read from the namespace's authoritative tenant label
	// (m47.3). tenantQuota is true only when the tenant actually carries model caps to enforce.
	tenantCtx, hasTenant, err := resolveTenantForNamespace(ctx, r.Client, deploy.Namespace)
	if err != nil {
		return podTemplate{}, fmt.Errorf("resolving tenant: %w", err)
	}
	tenantQuota := hasTenant && tenantCtx.hasModelCaps()

	// Guardrails (M66, ADR 0059 §8): resolve + validate spec.guardrailPolicyRef EARLY — like the
	// tenant, it decides whether the launcher :2996 model proxy is forced on (a guarded agent must
	// route its LLM calls THROUGH the proxy so the in-path guardrail engine can inspect them, even if
	// it has no budget/quota). CONTROL-PLANE FAIL-CLOSED: a dangling ref (policy not found) or an
	// invalid ref (an RE2 pattern doesn't compile) returns a *guardrailResolveError, which propagates
	// out of buildPodTemplate BEFORE any ksvc write — Reconcile sets Ready=False and the agent is NEVER
	// served unguarded (no "no-guardrail" MODEL_GATEWAY_URL, no serving revision that bypasses the
	// guardrail). Absent ref → gr.referenced is false and this path is byte-compatible pre-M66.
	gr, err := resolveGuardrail(ctx, r.Client, deploy)
	if err != nil {
		return podTemplate{}, err
	}

	// Cost budget (M8, specs/cost-governance.md) + tenant model quota (M47) + guardrails (M66): when
	// ANY is set, the agent's LLM calls must flow through the launcher's in-pod gateway proxy so it can
	// enforce the cap / inspect content BEFORE the provider is hit. So MODEL_GATEWAY_URL points at the
	// proxy (localhost:2996), the real LiteLLM address travels as GATEWAY_UPSTREAM_URL, and the knobs
	// are injected as STATIC env (values known at reconcile time — NEVER valueFrom, the m5.7 Knative
	// landmine / tier1 no-valueFrom guard). An agent with none gets the plain LiteLLM URL and no proxy
	// env — byte-for-byte M2 behavior.
	// Record mode (M78, ADR 0071 §1): a RECORD-CAPABLE agent (spec.record) is a NEW interposition
	// reason for the launcher gateway — the capture rides the :2996 proxy (raw model I/O incl. SSE),
	// which is conditionally interposed (ADR 0071 C2), so recording must force it on the same way a
	// budget/quota/guardrail does. Enablement is per-DEPLOYMENT here; the per-RUN capture toggle rides
	// the invoke (the BFF stamps X-Ctxmesh-Record when run.Record) and the BFF fails a recorded run
	// CLOSED when the agent is NOT record-capable (no gateway to capture at). Non-record-capable agents
	// are byte-for-byte unchanged (this reason is off, no RECORD_CAPABLE env).
	recordCapable := deploy.Spec.Record

	gatewayURL := litellmGatewayURL
	if deploy.Spec.Budget != nil || tenantQuota || gr.referenced || recordCapable {
		gatewayURL = budgetProxyURL
		env = append(env, corev1.EnvVar{Name: "GATEWAY_UPSTREAM_URL", Value: litellmGatewayURL})
	}
	if deploy.Spec.Budget != nil {
		env = append(
			env,
			// Either cap may be empty (that dimension unenforced); softThreshold
			// carries the CRD default (80) when unset because the field defaults
			// server-side, but guard against a zero value defensively.
			corev1.EnvVar{Name: "BUDGET_PER_CONVERSATION_USD", Value: deploy.Spec.Budget.PerConversationUSD},
			corev1.EnvVar{Name: "BUDGET_PER_AGENT_USD", Value: deploy.Spec.Budget.PerAgentUSD},
			corev1.EnvVar{Name: "BUDGET_SOFT_PCT", Value: strconv.Itoa(int(budgetSoftPct(deploy.Spec.Budget)))},
		)
	}
	env = append(env, corev1.EnvVar{Name: "MODEL_GATEWAY_URL", Value: gatewayURL})

	// Guardrails (M66, ADR 0059 §8; K3, ADR 0059 Fork-2): deliver the resolved+validated GuardrailPolicy
	// JSON as a MOUNTED, read-only per-agent ConfigMap file (<agent>-guardrail) rather than the pre-K3
	// GUARDRAIL_POLICY env. The launcher reads GUARDRAIL_POLICY_FILE at startup AND watches it (fsnotify),
	// so a GuardrailPolicy edit — which updates this ConfigMap in place via the existing watch-driven
	// reconcile — propagates to the RUNNING agent WITHOUT a revision roll (the reverse of the M4 landmine;
	// the point of K3). The mounted file's presence (GUARDRAIL_POLICY_FILE) flips GatewayProxyEnabled()
	// true, so the :2996 proxy still starts and runs the in-path guardrail engine even for a
	// guardrailed-but-unbudgeted agent — the interposition trigger is preserved, now keyed on the file.
	// Injected ONLY when the ref resolved and validated — a broken ref already failed closed above
	// (buildPodTemplate returned a *guardrailResolveError), so we never reach here with an unenforceable
	// policy. The env carries only the static FILE PATH (no valueFrom — the m5.7 Knative landmine).
	//
	// BFF_INTERNAL_URL (m66.15): the guardrail block audit POST (m66.9) targets BFF_INTERNAL_URL to write
	// the durable guardrail.block audit row. The delegate path (delegateEnv) injects it for supervisors,
	// but a PLAIN guarded agent (guardrailPolicyRef set, not a delegate supervisor) never enters that
	// branch — its block audit is span-only and the durable row is silently skipped. Fix: inject
	// BFF_INTERNAL_URL whenever a guardrailPolicyRef is present, using envVarPresent() to dedup so a
	// guarded supervisor (both paths active) gets it exactly once. Unguarded non-delegate agents are
	// unchanged (no BFF_INTERNAL_URL injected).
	guardrailVol, guardrailMount, guardrailEnv, err := r.reconcileGuardrailConfigMap(ctx, deploy, gr)
	if err != nil {
		return podTemplate{}, err
	}
	if gr.referenced {
		env = append(env, guardrailEnv...)
		if !envVarPresent(env, "BFF_INTERNAL_URL") && !envVarPresent(deploy.Spec.Env, "BFF_INTERNAL_URL") {
			env = append(env, corev1.EnvVar{Name: "BFF_INTERNAL_URL", Value: bffInternalURL})
		}
	}

	// Record mode (M78, ADR 0071 §1): a record-capable agent gets RECORD_CAPABLE=true, which flips
	// the launcher gateway's record-mode on (GatewayProxyEnabled + the per-run capture path). Its
	// presence — combined with GATEWAY_UPSTREAM_URL forced above — is the interposition reason. The
	// fixture is a DURABLE object-store blob (internal/objectstore, keyed fixtures/{runId}/…), so the
	// record-capable agent also needs OBJECT_STORE_ADDR + the dev creds to reach MinIO. Inject them
	// here (env-present-guarded so a record-capable registry MEMBER — which already got them in the
	// membership block below is not the concern; that block runs later, so guard defensively) — a
	// record-capable non-member would otherwise have no store to write the fixture to. STATIC env,
	// NEVER valueFrom (the m5.7 Knative landmine). All values are reconcile-time constants.
	if recordCapable {
		env = append(env, corev1.EnvVar{Name: "RECORD_CAPABLE", Value: gatewaySyncValue})
		if !envVarPresent(env, envObjectStoreAddr) && !envVarPresent(deploy.Spec.Env, envObjectStoreAddr) {
			env = append(env, objectStoreEnv(r.DevDataPlane)...)
		}
	}

	// Feedback ingest hook (M9, specs/eval-prompts-feedback.md §3): the launcher
	// starts the :2995 endpoint when LANGFUSE_HOST is present. The host, dev
	// credentials, and port are STATIC env (values known at reconcile time — NEVER
	// valueFrom, the m5.7 Knative ksvc landmine; tier1 no-valueFrom guard asserts
	// this). The dev creds match those seeded by `dev-up M=3` into the
	// langfuse-otlp Secret and the Langfuse Helm chart.
	//
	// OPS-2: these are DEV-ONLY Langfuse creds, so they inject ONLY when the dev data plane is
	// enabled (devDataPlane=false ⇒ skipped) — a `profile: production` render never ships the
	// dev.local feedback creds; with LANGFUSE_HOST absent the launcher simply does not start the
	// feedback relay (feature-off). Prod-functional feedback against an operator-provided Langfuse
	// (resolved from the langfuse-otlp Secret, like the collector path) is a carded follow-up
	// (m52.G1a) — beyond this gate, whose charter is only to keep the dev creds out of production.
	if r.DevDataPlane {
		env = append(
			env,
			corev1.EnvVar{Name: "LANGFUSE_HOST", Value: langfuseHost},
			corev1.EnvVar{Name: "LANGFUSE_SCORES_PUBLIC_KEY", Value: langfuseDevPublicKey},
			corev1.EnvVar{Name: "LANGFUSE_SCORES_SECRET_KEY", Value: langfuseDevSecretKey},
			corev1.EnvVar{Name: "FEEDBACK_PORT", Value: feedbackPort},
		)
	}

	// Runtime config (M65, ADR 0058): when spec.runtime is set, marshal the entire
	// RuntimeSpec as JSON and inject it as AGENT_RUNTIME — a STATIC platform env var
	// (NEVER valueFrom, the m5.7 Knative landmine). The SDK parses it at startup.
	// Injected before spec.env, which is appended last; by the platform convention a
	// user's spec.env may override a platform default (K8s uses the last value for a
	// duplicate name). That's harmless here — the authoritative output-schema validator
	// reads spec.runtime from the CRD (m65.4), not this env var. When nil, inject
	// nothing — the no-runtime path is byte-for-byte unchanged (backward-compat).
	if deploy.Spec.Runtime != nil {
		rtBytes, err := json.Marshal(deploy.Spec.Runtime)
		if err != nil {
			return podTemplate{}, fmt.Errorf("marshaling spec.runtime: %w", err)
		}
		env = append(env, corev1.EnvVar{Name: "AGENT_RUNTIME", Value: string(rtBytes)})
	}

	env = append(env, deploy.Spec.Env...)

	// AGENT_NAME + POD_NAMESPACE identify the agent to the launcher's tracing:
	// together they form the UNAMBIGUOUS `<namespace>/<name>` identity the launcher
	// stamps as the Langfuse trace-level tag `agent:<ns>/<name>` (proxy.go), so the
	// console can list the runs of exactly ONE agent without mixing two same-named
	// agents in different namespaces. Injected UNCONDITIONALLY for every agent (a
	// plain agent with no budget/memory/registry still needs a filterable trace
	// identity), STATIC (values known at reconcile time — NEVER valueFrom, the m5.7
	// Knative ksvc landmine / tier1 no-valueFrom guard), user-override-wins, and
	// BEFORE the budget/memory/registry blocks so they observe it present (their own
	// double-injection guards then no-op). These same vars also key per-agent spend
	// (budget proxy), the Valkey memory prefix, and the A2A envelope identity — one
	// injection now serves all of them.
	if !envVarPresent(env, envAgentName) && !envVarPresent(deploy.Spec.Env, envAgentName) {
		env = append(env, corev1.EnvVar{Name: envAgentName, Value: deploy.Name})
	}
	if !envVarPresent(env, envPodNamespace) && !envVarPresent(deploy.Spec.Env, envPodNamespace) {
		env = append(env, corev1.EnvVar{Name: envPodNamespace, Value: deploy.Namespace})
	}

	var resources corev1.ResourceRequirements
	if deploy.Spec.Resources != nil {
		resources.Requests = corev1.ResourceList{}
		if !deploy.Spec.Resources.CPU.IsZero() {
			resources.Requests[corev1.ResourceCPU] = deploy.Spec.Resources.CPU
		}
		if !deploy.Spec.Resources.Memory.IsZero() {
			resources.Requests[corev1.ResourceMemory] = deploy.Spec.Resources.Memory
		}
	}

	// Observability (M3): ensure the collector-config ConfigMap and build the
	// sidecar to inject alongside the user container.
	collector, collectorVol, err := r.reconcileCollector(ctx, deploy)
	if err != nil {
		return podTemplate{}, err
	}

	// Prompt-only deploy (M9): when spec.promptRef is set, resolve the referenced
	// PromptVersion's git pointer → prompt content, materialise it into the
	// <agent>-prompt ConfigMap, mount it read-only into the user container, and
	// inject PROMPT_FILE + PROMPT_VERSION as STATIC env (no valueFrom — the m5.7
	// Knative ksvc landmine). The prompt folds into the combined binding digest
	// (promptDig below) so a prompt swap rolls a NEW revision while the container
	// IMAGE (spec.Image, set on the user container, never touched here) keeps an
	// UNCHANGED digest — the prompt-only-deploy invariant. A missing PromptVersion
	// or an unresolvable git ref/path is USER input: resolvePrompt returns a
	// promptResolveError, propagated so the caller sets Ready=False and the old
	// revision keeps serving (no half-applied swap). Absent promptRef → the
	// image-bundled prompt is used and this is byte-compatible with the pre-M9 path.
	rp, err := r.resolvePrompt(ctx, deploy)
	if err != nil {
		return podTemplate{}, err
	}
	promptVol, promptMount, promptEnv, err := r.reconcilePromptConfigMap(ctx, deploy, rp)
	if err != nil {
		return podTemplate{}, err
	}
	// Append the platform prompt env (PROMPT_FILE / PROMPT_VERSION) only for names
	// the operator has NOT already set in spec.env — a duplicate container env var
	// name is invalid, and a deliberate user override must win (consistent with the
	// AGENT_PORT / AGENT_NAME treatment).
	for _, e := range promptEnv {
		if !envVarPresent(deploy.Spec.Env, e.Name) {
			env = append(env, e)
		}
	}

	// MCP tools (M4): resolve the agent's valid bindings. When ≥1 exists, inject
	// the discovery sidecar + tools ConfigMap volume + (sidecar-mode) tool
	// containers. The binding controller owns the CM CONTENT and the push; this
	// reconciler is the single writer of the pod template.
	validBindings, _, err := resolveAgentBindings(ctx, r.Client, r.registryReader(), deploy.Namespace, deploy.Name)
	if err != nil {
		return podTemplate{}, fmt.Errorf("resolving tool bindings: %w", err)
	}

	// Tool-call governance (M82.3, ADR 0074 §2): resolve spec.runtime.toolPolicy up front, because for
	// IN-POD (sidecar-mode) tools the enforcement is STRUCTURAL — a wire check at the egress sidecar is
	// THEATER (a sidecar tool binds a deterministic localhost port in the SHARED pod netns, so a
	// prompt-injected/custom loop just hits 127.0.0.1:<port> directly, around the sidecar). So a denied
	// in-pod tool must simply NOT be deployed. resolveToolPolicy never fails on user input (the CRD
	// enum bounds the shape); it only errors on a marshal bug.
	tp, terr := resolveToolPolicy(deploy)
	if terr != nil {
		return podTemplate{}, terr
	}
	// Apply the in-pod structural policy to the bindings BEFORE Render, so the manifest, the sidecar
	// container list, AND the egress route table are all derived from the SAME post-filter binding set
	// (Render assigns localhost ports in binding-name order; filtering here keeps the port↔manifest↔
	// container mapping inherently consistent — a denied middle tool cannot mis-map the survivors,
	// since both the manifest endpoint and the container port come from one Render over the same
	// slice). An OBO/remote tool is NOT touched here — its deny/require-approval is the m82.2 wire
	// enforcement (it is genuinely fronted). If an in-pod tool carries an effective require-approval
	// rule, this returns an *inPodRequireApprovalError → Reconcile sets Ready=False (no half-governed
	// pod). A pure-allow / policy-less agent keeps every binding — byte-for-byte unchanged.
	validBindings, err = filterInPodToolsByPolicy(validBindings, tp)
	if err != nil {
		return podTemplate{}, err
	}

	renderedManifest, sidecarTools := toolmanifest.Render(validBindings)
	hasBindings := len(validBindings) > 0

	// Tool-call governance (M82, ADR 0074 §1): front-all-tools is now the ONLY manifest mode —
	// always-on, no flag. EVERY tool of EVERY tool-having agent is fronted through the per-pod
	// egress sidecar (RewriteAllForEgress derives the route table from the rendered manifest), so
	// the sidecar is the authoritative tool-call chokepoint (OBO credential injection + record
	// capture + — in later M82 tasks — policy enforcement). This supersedes the M78 record-only /
	// ADR 0030 OBO-only variants: OBO tools keep their ServerName route + OAuth injection, so the
	// front-all table is a strict superset of the OBO-only one; record capture stays gated on the
	// per-run record header at the sidecar, unaffected. An agent with NO tool gets no routes and no
	// sidecar (byte-for-byte unchanged). NOTE (ADR 0074 Consequences): this changes the content-
	// addressed manifest version + adds the sidecar to pods that today have only in-pod tools — a
	// one-time fleet ROLL, expected.
	_, egressRoutes := toolmanifest.RewriteAllForEgress(renderedManifest, validBindings, egressSidecarBaseURL)

	// Tool-call governance (M82, ADR 0074 §1/§6): deliver the SAME resolved spec.runtime.toolPolicy
	// the SDK receives (via AGENT_RUNTIME) to the egress sidecar — the authoritative tool-call
	// chokepoint — as a controller-owned, STABLE-named <agent>-toolpolicy ConfigMap mounted
	// read-only on the sidecar with the static TOOL_POLICY_FILE path env (the M81-K3 pattern:
	// the sidecar reads + fsnotify-watches it, so a policy edit reloads live without a revision
	// roll). This task lands the PLUMBING only — the sidecar parses + holds the policy but does
	// NOT enforce it yet (enforcement is a later M82 task); behavior stays PERMISSIVE. Only
	// materialised when the agent has ≥1 tool (an egress route ⇒ a sidecar to mount it on) AND a
	// toolPolicy is set; a policy-less or tool-less agent gets no ConfigMap (byte-compatible
	// pre-M82). The ConfigMap is SECURITY-CRITICAL (§6a): the owner-ref + CreateOrUpdate reverts a
	// namespace-level edit so policy can't be flipped out from under the CR.
	var toolPolicyVol *corev1.Volume
	var toolPolicyMount *corev1.VolumeMount
	var toolPolicyEnv []corev1.EnvVar
	if len(egressRoutes) > 0 {
		// tp was resolved above (it drives the in-pod structural filter); here it materialises the
		// controller-owned <agent>-toolpolicy ConfigMap for the sidecar's OBO/remote wire enforcement.
		toolPolicyVol, toolPolicyMount, toolPolicyEnv, err = r.reconcileToolPolicyConfigMap(ctx, deploy, tp)
		if err != nil {
			return podTemplate{}, err
		}
	}

	// Routes hot-reload delivery (J7): deliver the sidecar's remote-tool URL table as a
	// controller-owned, STABLE-named <agent>-egress-routes ConfigMap mounted read-only on the sidecar
	// with the static EGRESS_ROUTES_FILE path env (the same K3 mounted-ConfigMap + fsnotify pattern as
	// the tool policy). A remote-URL edit updates the ConfigMap content — not the pod template (the
	// route URLs are excluded from the egress digest) — so the sidecar reloads it live WITHOUT a
	// revision roll. Only when the agent has ≥1 route (a sidecar to mount it on).
	var routesVol *corev1.Volume
	var routesMount *corev1.VolumeMount
	var routesEnv []corev1.EnvVar
	if len(egressRoutes) > 0 {
		routesVol, routesMount, routesEnv, err = r.reconcileEgressRoutesConfigMap(ctx, deploy, egressRoutesJSON(egressRoutes))
		if err != nil {
			return podTemplate{}, err
		}
	}

	// Memory (M5): resolve the agent's MemoryBinding (if any). When present,
	// inject MEMORY_BACKEND_ADDR, MEMORY_PORT, MEMORY_KEY_NAMESPACE (downward
	// API — pod namespace), and AGENT_NAME (for Valkey key composition).
	// The single-writer rule applies: only this reconciler writes the pod
	// template; the MemoryBinding controller only sets the binding's status.
	memAddr, memScope, hasMemoryBinding, err := resolveMemory(ctx, r.Client, deploy)
	if err != nil {
		return podTemplate{}, fmt.Errorf("resolving memory binding: %w", err)
	}
	if hasMemoryBinding {
		// MEMORY_BACKEND_ADDR is the DIRECT Valkey path. Injected ONLY when the
		// state-layer proxy is NOT configured; once the proxy is on (the m53.7
		// cutover, ADR 0050 §8 phase 3), the agent memory-forwards THROUGH it and
		// holds no direct Valkey path — the launcher's :2998 listener uses the proxy
		// forward when MEMORY_BACKEND_ADDR is absent + STATELAYER_PROXY_URL is set.
		if r.StatelayerProxyURL == "" {
			env = append(env, corev1.EnvVar{Name: "MEMORY_BACKEND_ADDR", Value: memAddr})
		}
		env = append(
			env,
			corev1.EnvVar{Name: "MEMORY_PORT", Value: "2998"},
			// MEMORY_KEY_NAMESPACE: the agent's own namespace, the Valkey key
			// prefix. Set as a STATIC value (the pod always runs in the
			// AgentDeployment's namespace, known here at reconcile time) — a
			// downward-API fieldRef is rejected by Knative Serving's webhook,
			// which forbids valueFrom in a ksvc pod template unless the
			// non-default kubernetes.podspec-fieldref feature flag is enabled.
			// The key prefix is the agent's own namespace regardless of where
			// the backend lives, so no downward reference is needed. (Unused on the
			// proxy path — the proxy derives the key namespace from the token — but
			// harmless + still correct for a proxy-less install.)
			corev1.EnvVar{Name: "MEMORY_KEY_NAMESPACE", Value: deploy.Namespace},
		)
		// AGENT_NAME: inject only if not already present. The launcher uses
		// AGENT_NAME for Valkey key composition and as the agent.invoke span
		// attribute. Check the ACCUMULATED env (the M8 budget block earlier may
		// have already injected it for a budgeted agent) AND spec.env (user
		// override must win) so a memory+budget agent gets AGENT_NAME exactly once
		// — a duplicate container env var is invalid.
		if !envVarPresent(env, envAgentName) && !envVarPresent(deploy.Spec.Env, envAgentName) {
			env = append(env, corev1.EnvVar{Name: envAgentName, Value: deploy.Name})
		}
		// STATELAYER_PROXY_URL (M51, ADR 0050 §8 phase 1): when the controller is
		// configured with it, route this agent's session/shared memory through the
		// state-layer proxy (the launcher reverse-proxies; it still holds MEMORY_BACKEND_ADDR
		// in phase 1 for dual-mode fallback). Empty ⇒ direct Valkey, no drift.
		if r.StatelayerProxyURL != "" {
			env = append(env, corev1.EnvVar{Name: "STATELAYER_PROXY_URL", Value: r.StatelayerProxyURL})
		}
		// Per-user session memory (M98, EU1a, ADR 0080): isolate each invoking end-user's conversation
		// memory into its own bucket. The launcher stamps X-Memory-User from the VERIFIED run capability
		// and the state-layer proxy keys under it. Injected ONLY when (a) the folded sessionMemory opts in
		// (perUser), (b) the scope is PRIVATE — never the shared team scratchpad, which is per-conversation
		// by design — and (c) the state-layer proxy path is on (there is no proxy to compose the per-user
		// key in a proxy-less install). Default off ⇒ inert for every existing agent. Adding this env rolls
		// a new revision (it is part of the pod template), so a toggle takes effect on the next reconcile.
		if sm := deploy.Spec.SessionMemory; sm != nil && sm.PerUser &&
			memScope != memoryScopeShared && r.StatelayerProxyURL != "" {
			env = append(env, corev1.EnvVar{Name: "MEMORY_PER_USER", Value: annotationTrue})
		}
	}

	// Long-term memory (M46, ADR 0045): when spec.longTermMemory.enabled, the launcher exposes
	// memory.remember / memory.search_agent that PROXY to the token-service (which holds the pgvector
	// store + CONTROLPLANE_DSN — agent pods never get DB credentials, ADR 0045 Amд 1). Inject the store
	// scope (agent-wide vs per-user), the embedding route (empty ⇒ launcher/token-service default), and
	// reuse the OBO token-service URL. AGENT_NAME (already ensured for a memory agent above) is the store
	// partition key.
	if lt := deploy.Spec.LongTermMemory; lt != nil && lt.Enabled {
		ltScope := "agent"
		if lt.PerUser {
			ltScope = "agent_user"
		}
		env = append(env,
			corev1.EnvVar{Name: "MEMORY_LONGTERM_ENABLED", Value: "true"},
			corev1.EnvVar{Name: "MEMORY_LONGTERM_SCOPE", Value: ltScope},
		)
		if lt.EmbeddingRoute != "" {
			env = append(env, corev1.EnvVar{Name: "EMBEDDING_ROUTE", Value: lt.EmbeddingRoute})
		}
		// The launcher proxies memory.remember/search_agent to the token-service; reuse the OBO
		// token-service URL (guard against a duplicate the OBO path may already have set).
		if r.OBOEgress.TokenServiceURL != "" &&
			!envVarPresent(env, envTokenServiceURL) && !envVarPresent(deploy.Spec.Env, envTokenServiceURL) {
			env = append(env, corev1.EnvVar{Name: envTokenServiceURL, Value: r.OBOEgress.TokenServiceURL})
		}
		// Per-user memory: the launcher verifies the invoking user's run capability, so inject the platform
		// capability public key + audience on the MAIN container (the OBO path sets them on the egress
		// sidecar). Without them the launcher fail-closes per-user memory (never falls back to agent-wide).
		if lt.PerUser && r.OBOEgress.CapabilityPublicKeyB64 != "" &&
			!envVarPresent(env, envMCPCapPublicKey) && !envVarPresent(deploy.Spec.Env, envMCPCapPublicKey) {
			env = append(env, corev1.EnvVar{Name: envMCPCapPublicKey, Value: r.OBOEgress.CapabilityPublicKeyB64})
			if r.OBOEgress.CapabilityAudience != "" &&
				!envVarPresent(env, envMCPCapAudience) && !envVarPresent(deploy.Spec.Env, envMCPCapAudience) {
				env = append(env, corev1.EnvVar{Name: envMCPCapAudience, Value: r.OBOEgress.CapabilityAudience})
			}
		}
		if !envVarPresent(env, envAgentName) && !envVarPresent(deploy.Spec.Env, envAgentName) {
			env = append(env, corev1.EnvVar{Name: envAgentName, Value: deploy.Name})
		}
	}

	// Knowledge bases (M68, ADR 0061 Fork 3): when spec.knowledgeBases is non-empty, resolve each
	// referenced KnowledgeBase CR (reading spec.embeddingRoute) and inject:
	//   KNOWLEDGE_BASE_ENABLED=true  — the launcher knowledgeProxy gate reads this.
	//   KNOWLEDGE_BASES=<JSON roster> — [{name,namespace,embeddingRoute}] — the un-forgeable roster
	//     (mirroring DELEGATE_ROSTER) that the launcher gates every /knowledge/search against.
	// Dangling (unresolvable) refs: KB is an ADDITIVE capability, NOT a fail-closed safety gate like
	// guardrails, so a missing KB surfaces a condition + skips that entry (the resolvable KBs are still
	// injected). The controller sets a "KnowledgeBasesResolved" condition (False/PartiallyResolved) when
	// any ref is unresolvable. An empty/nil slice ⇒ no KNOWLEDGE_BASE_ENABLED env (proxy stays off).
	kbRes, kbErr := resolveKnowledgeBases(ctx, r.Client, deploy)
	if kbErr != nil {
		return podTemplate{}, fmt.Errorf("resolving knowledge bases: %w", kbErr)
	}
	if len(kbRes.danglingNames) > 0 {
		// Surface a clear condition so the operator sees exactly which refs are broken.
		// We do NOT return an error — the resolvable entries are still injected.
		apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
			Type:               "KnowledgeBasesResolved",
			Status:             metav1.ConditionFalse,
			Reason:             "DanglingRef",
			Message:            fmt.Sprintf("KnowledgeBase refs not found (skipped): %s", strings.Join(kbRes.danglingNames, ", ")),
			ObservedGeneration: deploy.Generation,
		})
		if err := r.Status().Update(ctx, deploy); err != nil {
			return podTemplate{}, fmt.Errorf("updating status for dangling KB refs: %w", err)
		}
	}
	env = append(env, kbRes.env...)

	// Tenancy (M47, ADR 0046): when a Tenant owns this agent's namespace, inject the tenant id + its
	// model caps as STATIC env (known at reconcile time — NEVER valueFrom, the m5.7 Knative landmine). The
	// launcher reads TENANT_ID for the trace attribute (m47.3) and the caps + shared-Valkey address for the
	// quota enforcement (m47.4). tenantCtx/tenantQuota were resolved with the gateway-URL decision above.
	// injectPodToken records whether to mount the projected proxy token (set below).
	injectPodToken := false
	if hasTenant {
		env = append(env, corev1.EnvVar{Name: "TENANT_ID", Value: tenantCtx.id})
		if tenantCtx.budgetUSD != "" {
			env = append(env, corev1.EnvVar{Name: "TENANT_BUDGET_USD", Value: tenantCtx.budgetUSD})
		}
		if tenantCtx.rpm > 0 {
			env = append(env, corev1.EnvVar{Name: "TENANT_RPM", Value: strconv.Itoa(int(tenantCtx.rpm))})
		}
		if tenantCtx.maxConcurrent > 0 {
			env = append(env, corev1.EnvVar{Name: "TENANT_MAX_CONCURRENT", Value: strconv.Itoa(int(tenantCtx.maxConcurrent))})
		}
		if tenantQuota {
			// M53 (ADR 0050 §8): route tenant quota through the state-layer proxy
			// (pod-identity authed) when the controller is configured with it — the
			// launcher then holds NO Valkey credential for quota.
			if r.StatelayerProxyURL != "" {
				if !envVarPresent(env, envStatelayerProxyURL) && !envVarPresent(deploy.Spec.Env, envStatelayerProxyURL) {
					env = append(env, corev1.EnvVar{Name: envStatelayerProxyURL, Value: r.StatelayerProxyURL})
				}
				if !envVarPresent(env, envStatelayerTokenPath) && !envVarPresent(deploy.Spec.Env, envStatelayerTokenPath) {
					env = append(env, corev1.EnvVar{Name: envStatelayerTokenPath, Value: statelayerPodTokenFilePath})
				}
				injectPodToken = true
			} else {
				// Proxy NOT configured (proxy-less install): the launcher gateway enforces
				// the caps against the shared Valkey DIRECTLY, so inject its address. Once
				// the proxy is on (the m53.7 cutover, phase 3), this is NOT injected — the
				// agent has no direct Valkey path; quota flows through the proxy on the
				// SAME accumulator.
				env = append(env, corev1.EnvVar{Name: "TENANT_QUOTA_ADDR", Value: memoryDefaultAddr})
			}
		}
	}

	// Registry membership (M6): resolve the agent's AgentRegistry (if any). When a
	// member, inject the STATIC mesh env the launcher's /a2a server reads
	// (AGENT_REGISTRY_ID gates the endpoint; AGENT_ROLE / AGENT_ALLOWED_CALLERS
	// feed the L7 access-control checks; A2A_MAX_DEPTH / A2A_HOP_BUDGET seed the
	// conversation guards) and stamp the membership pod label the generated
	// NetworkPolicy selects on. All values are known at reconcile time → plain
	// static env, NEVER valueFrom (the Knative webhook rejects valueFrom in a
	// ksvc — the m5.7 landmine; a tier1 guard asserts no ksvc env uses it).
	membership, err := resolveAgentRegistry(ctx, r.Client, deploy)
	if err != nil {
		return podTemplate{}, fmt.Errorf("resolving registry membership: %w", err)
	}
	if membership.IsMember {
		env = append(
			env,
			corev1.EnvVar{Name: "AGENT_REGISTRY_ID", Value: membership.RegistryID},
			corev1.EnvVar{Name: "A2A_MAX_DEPTH", Value: strconv.Itoa(int(membership.MaxDepth))},
			corev1.EnvVar{Name: "A2A_HOP_BUDGET", Value: strconv.Itoa(int(membership.HopBudget))},
		)
		// Shared memory scope (ADR 0035, m33.3): a MemoryBinding scope=shared keys the team
		// scratchpad under this registry (mem:shared:{registry}:{conversationId}), so agents in the
		// conversation collaborate on ONE context. Injected ONLY here — inside the membership block —
		// because the shared key needs a registry boundary; a scope=shared binding on a NON-member
		// agent gets no MEMORY_SCOPE and the launcher keeps its private layout (a visible misconfig,
		// not a broken key).
		if hasMemoryBinding && memScope == memoryScopeShared {
			env = append(env, corev1.EnvVar{Name: "MEMORY_SCOPE", Value: memoryScopeShared})
		}
		// POD_NAMESPACE: the namespace A2A targets resolve in — the launcher's
		// clusterHost() builds http://{target}.{POD_NAMESPACE}.svc.cluster.local.
		// STATIC (deploy.Namespace, known here), never a downward-API fieldRef:
		// Knative's webhook rejects valueFrom in a ksvc pod template (the m5.7
		// landmine; a tier1 guard asserts no ksvc env uses valueFrom). Now injected
		// UNCONDITIONALLY in the base env for the trace identity, so guard against a
		// duplicate here (a duplicate container env var name is invalid); the base
		// injection already covers a registry member without a MemoryBinding.
		if !envVarPresent(env, envPodNamespace) && !envVarPresent(deploy.Spec.Env, envPodNamespace) {
			env = append(env, corev1.EnvVar{Name: envPodNamespace, Value: deploy.Namespace})
		}
		// AGENT_NAME: the launcher's senderAgentId / envelope path identity. The
		// memory path (earlier in this function) may have already appended it, and
		// the user may have overridden it in spec.env — inject exactly ONCE. Check
		// the ACCUMULATED env slice (covers the memory path) AND spec.env (user
		// override wins) so a member with BOTH memory and registry gets AGENT_NAME
		// once, and a member without memory still gets it (the A2A path needs it).
		if !envVarPresent(env, envAgentName) && !envVarPresent(deploy.Spec.Env, envAgentName) {
			env = append(env, corev1.EnvVar{Name: envAgentName, Value: deploy.Name})
		}
		// AGENT_ROLE: from spec.role, user-override-wins (like AGENT_NAME). The
		// launcher stamps it into the envelope role field.
		if !envVarPresent(deploy.Spec.Env, "AGENT_ROLE") {
			env = append(env, corev1.EnvVar{Name: "AGENT_ROLE", Value: deploy.Spec.Role})
		}
		// AGENT_ALLOWED_CALLERS: comma-join of spec.allowedCallers (user-override-
		// wins). Empty when the list is unset → the launcher applies its default
		// (registry-membership) policy.
		if !envVarPresent(deploy.Spec.Env, "AGENT_ALLOWED_CALLERS") {
			env = append(env, corev1.EnvVar{
				Name:  "AGENT_ALLOWED_CALLERS",
				Value: strings.Join(deploy.Spec.AllowedCallers, ","),
			})
		}

		// Delegate (M64, ADR 0057): if this registry member is the SUPERVISOR of an AgentTeam, inject the
		// launcher's delegate wiring so its SDK gets the delegate_to tool + can spawn its roster as sub-runs.
		// The spawn guard needs a counter store to fail-closed on: post-cutover (M94, audit P1-2) that is the
		// pod-authed state-layer PROXY (no direct Valkey path); pre-cutover it is the direct Valkey addr.
		team, tErr := resolveSupervisedTeam(ctx, r.Client, deploy)
		if tErr != nil {
			return podTemplate{}, fmt.Errorf("resolving supervised team: %w", tErr)
		}
		if team != nil {
			for _, e := range delegateEnv(team) {
				// Guard against duplicates: a guarded supervisor already has BFF_INTERNAL_URL
				// injected by the guardrail block (m66.15). All other delegateEnv vars are
				// delegate-only so the check is symmetric with the rest of the platform env
				// injection pattern (envVarPresent before append).
				if !envVarPresent(env, e.Name) && !envVarPresent(deploy.Spec.Env, e.Name) {
					env = append(env, e)
				}
			}
			// M94: route the spawn guard through the state-layer proxy when configured (the supervisor then
			// holds NO direct :6379 path — the last direct-Valkey consumer, so `:6379` can leave the egress
			// allowlist). Otherwise (proxy-less) fall back to the direct Valkey addr.
			if r.StatelayerProxyURL != "" {
				if !envVarPresent(env, envStatelayerProxyURL) && !envVarPresent(deploy.Spec.Env, envStatelayerProxyURL) {
					env = append(env, corev1.EnvVar{Name: envStatelayerProxyURL, Value: r.StatelayerProxyURL})
				}
				if !envVarPresent(env, envStatelayerTokenPath) && !envVarPresent(deploy.Spec.Env, envStatelayerTokenPath) {
					env = append(env, corev1.EnvVar{Name: envStatelayerTokenPath, Value: statelayerPodTokenFilePath})
				}
				injectPodToken = true
			} else if !envVarPresent(env, "TENANT_QUOTA_ADDR") && !envVarPresent(deploy.Spec.Env, "TENANT_QUOTA_ADDR") {
				env = append(env, corev1.EnvVar{Name: "TENANT_QUOTA_ADDR", Value: memoryDefaultAddr})
			}
		}

		// Blob offload (m7.6b): a registry member participates in the async A2A
		// path — as a publisher (offload a >256KiB payload) and/or a consumer
		// (rehydrate a $ref before invoke). Inject the dedicated dev object store's
		// address + the deterministic DEV-ONLY credentials so the launcher's
		// publish/consume path can reach MinIO. Scope: EVERY registry member, not
		// only executionModel=eventing — a member's launcher wires offload on the
		// same gate the async consumer/publisher use (registry membership /
		// A2AEnabled), independent of its own workload KIND, so a producer that
		// publishes and a Trigger-backed consumer both get it. All three are known
		// constants → STATIC env, NEVER valueFrom (Knative ksvc webhook rejects it;
		// the m5.7 landmine + tier1 no-valueFrom guard). The launcher gate is
		// OBJECT_STORE_ADDR: with it absent (a non-member), offload is disabled and
		// async payloads pass through capped. Guard against double-injection: a
		// record-capable agent (M78) may have already been given the same env by the
		// record-mode block above — inject only when not already present (last-write
		// semantics would otherwise leave a duplicate env entry).
		if !envVarPresent(env, envObjectStoreAddr) && !envVarPresent(deploy.Spec.Env, envObjectStoreAddr) {
			env = append(env, objectStoreEnv(r.DevDataPlane)...)
		}

		// (Async dedup through the proxy needs the same pod token; it is now set by the
		// hoisted block below so a NON-member memory agent gets it too — Fable audit.)
	}

	// State-layer proxy pod token for the MEMORY (+ async-dedup) path (ADR 0050 §8 / ADR
	// 0052 §C6 RESOLUTION): a memory-bound agent authenticates to the proxy with its POD
	// token, so it needs the projected-token env + mount + per-agent SA. HOISTED out of the
	// registry-member and tenant-quota branches (Fable audit 2026-08-08): a plain memory
	// agent (non-member, non-quota) MUST still get the token, else its session memory 401s
	// at the proxy. STATELAYER_PROXY_URL itself is injected by the memory block above for
	// any memory agent; this adds the token path + flips injectPodToken. The env-present
	// guard keeps it duplicate-safe against the tenant-quota block.
	if r.StatelayerProxyURL != "" && hasMemoryBinding {
		if !envVarPresent(env, envStatelayerTokenPath) && !envVarPresent(deploy.Spec.Env, envStatelayerTokenPath) {
			env = append(env, corev1.EnvVar{Name: envStatelayerTokenPath, Value: statelayerPodTokenFilePath})
		}
		injectPodToken = true
	}

	// The user container's volume mounts: the resolved-prompt file (M9) when the
	// agent has a promptRef, else none. nil is a valid empty mount list.
	var userMounts []corev1.VolumeMount
	if promptMount != nil {
		userMounts = append(userMounts, *promptMount)
	}
	if guardrailMount != nil {
		// Guardrails K3: mount the <agent>-guardrail ConfigMap read-only so the launcher can read +
		// fsnotify-watch the policy file. A core ConfigMap volume (Knative Serving admits it, same as
		// the prompt CM). No image change — a pod-VOLUME + config change only.
		userMounts = append(userMounts, *guardrailMount)
	}
	if injectPodToken {
		// Mount the projected proxy token read-only (M53). The launcher runs in this
		// container (baked into the agent image), so the token is co-resident with agent
		// code — acceptable: the token scopes to the pod's OWN namespace/tenant, so it is
		// bounded self-harm within the tenant, never cross-tenant (ADR 0050 §8, Option A).
		userMounts = append(userMounts, corev1.VolumeMount{
			Name:      statelayerTokenVolume,
			MountPath: statelayerTokenMountPath,
			ReadOnly:  true,
		})
	}

	containers := []corev1.Container{
		{
			// Named explicitly: multi-container Knative pods require
			// every container to be named, else Knative auto-names it
			// ("user-container-0") and re-reconcile drifts.
			Name:  "user-container",
			Image: deploy.Spec.Image,
			Ports: []corev1.ContainerPort{
				{ContainerPort: port},
			},
			Env:          env,
			Resources:    resources,
			VolumeMounts: userMounts,
			ReadinessProbe: &corev1.Probe{
				// SuccessThreshold=1 explicitly: Knative defaults it on
				// create and rejects a re-applied 0 (must be >= 1).
				SuccessThreshold: 1,
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/readyz",
						Port: intstr.FromInt32(port),
					},
				},
			},
			LivenessProbe: &corev1.Probe{
				SuccessThreshold: 1,
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/healthz",
						Port: intstr.FromInt32(port),
					},
				},
			},
		},
		collector,
	}
	volumes := []corev1.Volume{collectorVol}
	if promptVol != nil {
		// Prompt-only deploy (M9): the resolved-prompt ConfigMap volume, mounted
		// read-only into the user container above. Added to the pod's volumes so the
		// mount resolves. No image change — this is a pod-VOLUME + config-revision
		// change only.
		volumes = append(volumes, *promptVol)
	}
	if guardrailVol != nil {
		// Guardrails K3 (ADR 0059 Fork-2): the <agent>-guardrail ConfigMap volume, mounted
		// read-only into the user container above. Because the policy is NO LONGER in the
		// structural digest (see below), a policy edit updates this ConfigMap in place and the
		// launcher reloads via the watch — the revision name is unchanged, so no roll.
		volumes = append(volumes, *guardrailVol)
	}
	if toolPolicyVol != nil {
		// Tool-call governance (M82, ADR 0074 §1): the <agent>-toolpolicy ConfigMap volume, mounted
		// read-only on the EGRESS SIDECAR container below. Like the guardrail volume, the policy
		// rides the watched mounted file (the sidecar reloads via fsnotify), so a policy edit updates
		// this ConfigMap in place without rolling the revision — the mount itself is structurally
		// constant for any tool-having agent.
		volumes = append(volumes, *toolPolicyVol)
	}
	if routesVol != nil {
		// Routes hot-reload (J7): the <agent>-egress-routes ConfigMap volume, mounted read-only on the
		// EGRESS SIDECAR. The route URLs ride this watched file (the sidecar reloads via fsnotify) +
		// are excluded from the egress digest, so a remote-URL edit updates this ConfigMap in place
		// WITHOUT rolling the revision — the mount is structurally constant for any tool-having agent.
		volumes = append(volumes, *routesVol)
	}
	if injectPodToken {
		// Projected serviceAccountToken bound to the proxy audience (M53, ADR 0050 Amд 3).
		// A projected VOLUME (not valueFrom) — Knative admits it (verified on 1.22.1). The
		// short expiry (10 min) keeps the revocation/stale window tight; kubelet rotates it
		// in place and the launcher re-reads on each request.
		expiry := statelayerTokenExpirySecs
		volumes = append(volumes, corev1.Volume{
			Name: statelayerTokenVolume,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Path:              "token",
							Audience:          statelayerPodAudience,
							ExpirationSeconds: &expiry,
						},
					}},
				},
			},
		})
	}

	if hasBindings {
		containers = append(containers, discoverySidecarContainer(r.discoveryImage()))
		// Sidecar-mode tool containers, in the deterministic binding-name order
		// Render assigned (matches the manifest's localhost ports).
		for _, st := range sidecarTools {
			containers = append(containers, sidecarToolContainer(st))
		}
		volumes = append(volumes, toolsVolume(deploy.Name))
	}

	// Tool-call governance (M82, ADR 0074 §1): the egress sidecar is the ALWAYS-ON authoritative
	// tool-call chokepoint — inject it whenever the agent has ≥1 tool (front-all rewrote every
	// endpoint above, so len(egressRoutes) > 0 iff the agent has a tool). This supersedes the
	// prior gate on (OBO egress enabled || record-capable): the sidecar now fronts every tool for
	// every tool-having agent (OBO credential injection when a route is OAuth, record capture gated
	// on the per-run header, and — in later M82 tasks — policy enforcement). recordCapable still
	// adds RECORD_CAPABLE + the object-store env (fail-closed C2 sink) to the container. A non-tool
	// agent gets no routes and no sidecar — byte-for-byte unchanged.
	if len(egressRoutes) > 0 {
		agentIdentity := deploy.Namespace + "/" + deploy.Name
		containers = append(containers, egressSidecarContainer(
			r.OBOEgress, deploy.Namespace, agentIdentity, agentEgressBoundary(deploy, membership),
			egressRoutesJSON(egressRoutes), recordCapable, r.DevDataPlane, toolPolicyMount, toolPolicyEnv,
			routesMount, routesEnv))
	}

	// Combined structural digest: "" when no binding/membership resolves (bare
	// pre-M4 name). With ANY binding (tool and/or memory) or membership, one
	// combined digest ("-h<digest8>") rolls the workload when a binding is
	// added/removed, a sidecar image changes, a memory addr changes, or
	// membership changes (the containers/env must actually land — the
	// CreateOrUpdate guard in reconcileKnativeService skips re-applying the spec
	// when the revision name is unchanged; a stale name means the injection is
	// SILENTLY LOST, the M4 landmine). The tool component EXCLUDES remote URLs
	// and the manifest version so a manifest-ONLY change keeps the SAME name →
	// no roll (specs/mcp-tools.md "Hot path vs cold path").
	//
	// Name budget: Knative revision names are DNS-1035 labels, max 63 chars. The
	// suffix is bounded at 19 chars — "-" + 8 (spec hash) + "-h" + 8 (combined
	// digest) — NO MATTER how many binding types future milestones add, leaving
	// 44 chars of agent-name budget, enforced by a root CEL rule on metadata.name
	// (size <= 44). executionModel does NOT feed the digest: switching model is a
	// create/delete of a different workload KIND (handled by the per-model
	// reconcilers), not a revision roll within the ksvc.
	toolDigest := toolmanifest.StructuralDigest(sidecarTools, hasBindings)
	if ed := egressDigest(r.OBOEgress.SidecarImage, agentEgressBoundary(deploy, membership), egressRoutes, recordCapable); ed != "" {
		// The egress sidecar (image + routes — the real URLs now live in pod env, not the
		// hot-path manifest) is pod-template state, so fold it into the tool component: adding/
		// removing the sidecar or changing a route rolls a new revision. Inert when the agent has
		// no tool (egressRoutes is nil ⇒ egressDigest is "").
		toolDigest += "e" + ed
	}
	// Tool-call governance (M82, ADR 0074 §1): fold the PRESENCE of a tool policy into the tool
	// component — a PRESENCE marker, not the content (like the guardrail presence digest). Adding or
	// removing spec.runtime.toolPolicy is a STRUCTURAL pod change (the <agent>-toolpolicy volume +
	// mount + TOOL_POLICY_FILE env on the sidecar appear/disappear), so it must roll a new revision;
	// EDITING an already-present policy's CONTENT keeps the SAME marker → the SAME revision name →
	// NO roll (the content rides the watched mounted ConfigMap, which the sidecar reloads live —
	// the K3 point). "" when no policy, symmetric with the other components.
	if toolPolicyMount != nil {
		toolDigest += "p" + toolPolicyPresenceDigest()
	}
	memDigest := memoryBindingDigest(hasMemoryBinding, memAddr)
	// Long-term memory (M46, ADR 0045) is a structural pod change (env injected) → fold it into the
	// memory digest so enabling/disabling it or changing perUser/embeddingRoute rolls a new revision
	// (even when the agent has no session memory, i.e. memDigest is "").
	if lt := deploy.Spec.LongTermMemory; lt != nil && lt.Enabled {
		sum := sha256.Sum256([]byte(memDigest + fmt.Sprintf("|lt:%t:%s", lt.PerUser, lt.EmbeddingRoute)))
		memDigest = fmt.Sprintf("%x", sum[:])[:8]
	}
	// The object-store (blob-offload) env is injected 1:1 with membership.IsMember
	// and its values are package constants (objectStoreAddr + the dev creds,
	// identical for every member), so it needs no digest component of its own: the
	// registry-membership digest already rolls the revision on join/leave — exactly
	// when OBJECT_STORE_ADDR + the creds appear/disappear. If the store address
	// ever becomes per-agent it must fold into registryMembershipDigest.
	regDigest := registryMembershipDigest(membership, deploy.Spec.Role, deploy.Spec.AllowedCallers)
	// Cost budget (M8): a budget add/remove/change repoints MODEL_GATEWAY_URL and
	// injects/removes the BUDGET_* env — a STRUCTURAL change that must roll the
	// revision, so it folds into the combined digest like the other components.
	budgetDig := budgetDigest(deploy.Spec.Budget)
	// Prompt-only deploy (M9): the resolved prompt (pointer + content, via its
	// version) folds in as a new component like the budget (g=<w>). A promptRef
	// swap OR a PromptVersion.spec.git.ref swap changes rp.digest → a new combined
	// "-h" suffix → a NEW Knative revision (clean rollout, new prompt takes effect),
	// while the container IMAGE (spec.Image on the user container) is untouched — so
	// the image digest stays IDENTICAL across a prompt swap. "" when no promptRef,
	// symmetric with the other components (byte-compatible pre-M9 revision name).
	promptDig := rp.digest
	tenantDig := tenantDigest(tenantCtx, hasTenant)
	// The proxy URL is injected for a memory binding (M51) OR tenant quota + token (M53);
	// fold it into the digest so enabling/changing it rolls a new revision (M4 landmine).
	// The default-token automount opt-in (m79.4) is a property of the SAME per-agent identity
	// SA this component already tracks (the "|sa" tag), so it folds in here: toggling
	// spec.mountServiceAccountToken flips the SA's AutomountServiceAccountToken, which must
	// reach a running pod — so it rolls a new revision like every other pod-template change.
	proxyDig := statelayerProxyDigest(r.StatelayerProxyURL, hasMemoryBinding || injectPodToken, mountAPIToken(deploy))
	// Runtime config (M65): a spec.runtime add/remove/change injects/removes the
	// AGENT_RUNTIME env — a STRUCTURAL change that must roll the revision, so it
	// folds into the combined digest like the other components.
	runtimeDig := runtimeDigest(deploy.Spec.Runtime)
	// Guardrails (M66, ADR 0059 §8; K3, ADR 0059 Fork-2): the digest folds PRESENCE ONLY, NOT the
	// policy CONTENT. Adding/removing the guardrailPolicyRef is a STRUCTURAL pod change — it
	// repoints MODEL_GATEWAY_URL at the proxy and adds/removes the mounted <agent>-guardrail volume +
	// GUARDRAIL_POLICY_FILE env — so it MUST roll a new revision (the M4 silent-loss landmine); the
	// presence bit does exactly that. But EDITING an already-referenced policy's CONTENT must NOT roll
	// (the whole point of K3): the content lands in the mounted ConfigMap the launcher watches, so it
	// reloads live. Dropping gr.digest (the content hash) from the digest is what makes a policy-only
	// edit keep the SAME revision name — the reverse of the M66 behavior, by design. "" when
	// unreferenced (symmetric with the other components; byte-compatible pre-M66 revision name).
	guardrailDig := guardrailPresenceDigest(gr.referenced)
	// Knowledge bases (M68, ADR 0061): a spec.knowledgeBases add/remove/change (or a change to a bound
	// KnowledgeBase's embeddingRoute — picked up via the watch wired in SetupWithManager) injects/removes
	// KNOWLEDGE_BASE_ENABLED + KNOWLEDGE_BASES — a STRUCTURAL change that must roll the revision.
	// "" when no KBs are bound (symmetric with the other components; byte-compatible pre-M68).
	kbDig := knowledgeBasesDigest(kbRes.roster)
	combinedDigest := combinedBindingDigest(
		toolDigest, memDigest, regDigest, budgetDig, promptDig, tenantDig, proxyDig, runtimeDig, guardrailDig, kbDig)

	// Membership pod label: when the agent is a registry member, stamp the
	// controller-owned registry-id label on the pod template so the pods carry
	// it — the generated NetworkPolicy's podSelector and intra-registry
	// from-selector match on exactly this label.
	var templateLabels map[string]string
	if membership.IsMember {
		templateLabels = map[string]string{registryIDLabel: membership.RegistryID}
	}

	// Per-agent identity SA (ADR 0052 §C6 RESOLUTION): the pod runs as "agent-<name>"
	// exactly when it presents a projected token to the state-layer proxy, so
	// TokenReview yields a cryptographic (ns,agent) identity. Empty otherwise (default
	// SA, no drift). injectPodToken already feeds proxyDig, so enabling it rolls a new
	// revision — and the "sa" version bump in statelayerProxyDigest re-rolls agents that
	// were ALREADY proxy-attached onto the new SA (a real pod-spec change must roll a
	// revision — the M4 silent-loss landmine).
	var saName string
	if injectPodToken {
		saName = agentIdentitySAName(deploy.Name)
	}

	// C8 launcher injection (ADR 0079): when LAUNCHER_IMAGE is configured, inject the launcher-inject
	// initContainer + shared emptyDir and override the user container's Command. Fold the launcher image
	// into the structural digest so a LAUNCHER_IMAGE change ROLLS A NEW REVISION — initContainers run only
	// at pod start, so a central launcher fix reaches agents via a revision roll (the M4 silent-loss
	// landmine class: a real pod-spec change MUST roll a revision).
	var initContainers []corev1.Container
	if r.launcherInjectionReady(ctx) { // C8c: only inject when LAUNCHER_IMAGE is set AND the Knative flags are on
		initContainers, containers, volumes = injectPlatformLauncher(containers, volumes, r.LauncherImage)
		sum := sha256.Sum256([]byte(combinedDigest + "|launcher:" + r.LauncherImage))
		combinedDigest = fmt.Sprintf("%x", sum[:])[:8]
	}

	return podTemplate{
		containers:         containers,
		initContainers:     initContainers,
		volumes:            volumes,
		labels:             templateLabels,
		digest:             combinedDigest,
		membership:         membership,
		port:               port,
		serviceAccountName: saName,
	}, nil
}

// agentIdentitySAName is the per-agent identity ServiceAccount name for an
// AgentDeployment: "agent-<name>". Identity-only (no RBAC) — its sole purpose is a
// distinct TokenReview subject so the state-layer proxy can scope per-agent (ADR
// 0052 §C6 RESOLUTION).
func agentIdentitySAName(deployName string) string {
	return "agent-" + deployName
}

// C8 launcher-injection constants (ADR 0079).
const (
	// platformLauncherVolume is the emptyDir the launcher-inject initContainer stages the launcher into,
	// mounted read-only into the user container.
	platformLauncherVolume = "platform-launcher"
	// platformLauncherMountDir / platformLauncherBinary: the mount point + the staged launcher path.
	platformLauncherMountDir = "/platform"
	platformLauncherBinary   = "/platform/launcher"
	// launcherInjectContainer is the initContainer name; userContainerName is the user container it targets
	// (by NAME, so a future sidecar is never Command-clobbered).
	launcherInjectContainer = "launcher-inject"
	userContainerName       = "user-container"
	// launcherInstallSubcommand mirrors cmd/launcher/install.go's launcherInstallFlag — the launcher
	// self-copy subcommand the distroless (no-cp) launcher image runs as the initContainer.
	launcherInstallSubcommand = "--install"
	// launcherImageInContainer is the launcher binary's path inside LAUNCHER_IMAGE (Dockerfile.launcher).
	launcherImageInContainer = "/launcher"
	// launcherImageAnnotation stamps the injected launcher image onto the ksvc revision (C8d auditability):
	// "which agents run launcher X" is then one query. It changes exactly when LAUNCHER_IMAGE does (which
	// already rolls a revision via the structural digest), so it never causes a spurious roll.
	launcherImageAnnotation = "agents.ctxmesh.ai/launcher-image"
)

// knativeFeaturesConfigMap is the Knative Serving feature-flags ConfigMap (knative-serving namespace).
const knativeFeaturesConfigMap = "config-features"

// launcherRequiredKnativeFlags are the Knative feature flags C8 launcher injection needs on the ksvc pod
// template (ADR 0079): initContainers, an emptyDir volume, and a hardened initContainer securityContext.
// With any of them off, Knative REJECTS the ksvc — so the controller must not inject unless all are on.
var launcherRequiredKnativeFlags = []string{
	"kubernetes.podspec-init-containers",
	"kubernetes.podspec-volumes-emptydir",
	"kubernetes.podspec-securitycontext",
}

// launcherInjectionReady reports whether C8 launcher injection is BOTH configured (LAUNCHER_IMAGE set) AND
// safe to apply — the required Knative feature flags are enabled in knative-serving/config-features (C8c
// preflight, ADR 0079). If LAUNCHER_IMAGE is set but a flag is absent/off (or the ConfigMap can't be read),
// it returns false FAIL-SAFE — injection is skipped (baked-launcher agents run unchanged) and a loud warning
// is logged — so a misconfiguration (LAUNCHER_IMAGE set + flags off) never rejects every ksvc into a
// self-inflicted fleet outage.
func (r *AgentDeploymentReconciler) launcherInjectionReady(ctx context.Context) bool {
	if r.LauncherImage == "" {
		return false
	}
	// C8b (pinning MVP): LAUNCHER_IMAGE is fleet-RCE-equivalent config — whoever sets it owns PID 1 in every
	// agent pod. Require a DIGEST-pinned reference (…@sha256:…); a mutable tag would let a mutated registry
	// image silently swap the launcher. Reject fail-safe (skip injection) rather than inject a mutable tag.
	// (Auto tag→digest resolution + cosign verification are carded — m52.C8b.)
	if !strings.Contains(r.LauncherImage, "@sha256:") {
		logf.FromContext(ctx).Info("WARNING: C8 launcher injection SKIPPED — LAUNCHER_IMAGE is not digest-pinned; it MUST be a …@sha256:… reference (a mutable tag is fleet-RCE-equivalent). Keeping baked-launcher behavior.",
			"launcherImage", r.LauncherImage)
		return false
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: knativeServingNamespace, Name: knativeFeaturesConfigMap}, &cm); err != nil {
		logf.FromContext(ctx).Info("WARNING: C8 launcher injection SKIPPED — cannot read knative-serving/config-features to confirm the required feature flags; keeping baked-launcher behavior. Set LAUNCHER_IMAGE only after enabling the flags.",
			"err", err.Error(), "flags", launcherRequiredKnativeFlags)
		return false
	}
	for _, flag := range launcherRequiredKnativeFlags {
		switch strings.ToLower(strings.TrimSpace(cm.Data[flag])) {
		case "enabled", "allowed":
			// on
		default:
			logf.FromContext(ctx).Info("WARNING: C8 launcher injection SKIPPED — a required Knative feature flag is not enabled; keeping baked-launcher behavior (fail-safe, no fleet outage).",
				"flag", flag, "value", cm.Data[flag])
			return false
		}
	}
	return true
}

// injectPlatformLauncher rewrites the pod template to run the PLATFORM-pinned launcher (C8, ADR 0079). A
// launcher-inject initContainer self-copies the launcher into a shared emptyDir; the USER container (matched
// by name, never a sidecar) mounts it read-only and its Command is overridden to exec it — so the user image
// needs no baked launcher and a launcher fix rolls centrally. Off unless launcherImage is set (backward-
// compatible). The initContainer is HARDENED (nonroot uid 65532, no caps, RO rootfs, RuntimeDefault seccomp)
// and RESOURCE-BOUNDED so restricted-PSS / ResourceQuota'd namespaces admit it; the emptyDir is size-capped.
// REQUIRES the Knative podspec-init-containers + podspec-volumes-emptydir (+ -securitycontext) feature flags.
func injectPlatformLauncher(containers []corev1.Container, volumes []corev1.Volume, launcherImage string) (initContainers, outContainers []corev1.Container, outVolumes []corev1.Volume) {
	for i := range containers {
		if containers[i].Name != userContainerName {
			continue // only the user container execs the launcher; sidecars keep their own Command
		}
		containers[i].Command = []string{platformLauncherBinary} // Args left absent: k8s ignores the image CMD entirely
		containers[i].VolumeMounts = append(containers[i].VolumeMounts, corev1.VolumeMount{
			Name: platformLauncherVolume, MountPath: platformLauncherMountDir, ReadOnly: true,
		})
	}

	initC := corev1.Container{
		Name:    launcherInjectContainer,
		Image:   launcherImage,
		Command: []string{launcherImageInContainer, launcherInstallSubcommand, platformLauncherBinary},
		VolumeMounts: []corev1.VolumeMount{
			{Name: platformLauncherVolume, MountPath: platformLauncherMountDir},
		},
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
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To(int64(65532)),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
	vol := corev1.Volume{
		Name: platformLauncherVolume,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("64Mi"))},
		},
	}
	return []corev1.Container{initC}, containers, append(volumes, vol)
}

// mountAPIToken reports whether the agent opts into auto-mounting the default kube-API
// ServiceAccount token (m79.4, m52 C10). Default (nil/false) is HARDENED: the token is
// stripped from the pod because the agent runtime never uses it (the state-layer proxy
// authenticates with a dedicated projected token on a separate volume). true opts the
// agent back in — only for one that legitimately builds an in-cluster kube config. It
// feeds both ensureAgentIdentitySA (the SA-level AutomountServiceAccountToken) and the
// structural digest (so toggling it rolls a new revision, the M4 landmine).
func mountAPIToken(deploy *agentsv1alpha1.AgentDeployment) bool {
	return deploy.Spec.MountServiceAccountToken != nil && *deploy.Spec.MountServiceAccountToken
}

// identitySAConflictError is a sentinel returned by ensureAgentIdentitySA when
// the per-agent identity SA (agent-<name>) already exists and is owned by a
// DIFFERENT controller. The reconcile must stop cleanly (no requeue / no
// hot-loop): Reconcile intercepts this type, sets Ready=False
// (reasonIdentitySAConflict), and returns nil so the controller does not
// back-off-requeue. Mirrors the promptResolveError / guardrailResolveError
// pattern — a human must resolve the naming collision before the agent can run.
type identitySAConflictError struct {
	saName    string
	ownerKind string
	ownerName string
}

func (e *identitySAConflictError) Error() string {
	return fmt.Sprintf("ServiceAccount %q is already owned by %s %q; delete or rename it to resolve the conflict",
		e.saName, e.ownerKind, e.ownerName)
}

// asIdentitySAConflictError extracts an *identitySAConflictError from err (supports wrapping).
func asIdentitySAConflictError(err error) (*identitySAConflictError, bool) {
	var ce *identitySAConflictError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

// ensureAgentIdentitySA reconciles the per-agent identity ServiceAccount the pod runs
// as when it presents a projected token to the state-layer proxy (ADR 0052 §C6
// RESOLUTION). saName == "" (a non-proxy agent) is a no-op: the pod keeps the namespace
// default SA and nothing is created. The SA is IDENTITY-ONLY — no RoleBindings, no
// imagePullSecrets — so it grants nothing; its sole purpose is a distinct TokenReview
// subject (system:serviceaccount:<ns>:agent-<name>) the proxy scopes per-agent. Owned by
// the AgentDeployment, so it is garbage-collected with the agent. Idempotent.
//
// registryID, when non-empty, is stamped as the registryIDLabel LABEL on the SA — the
// SERVER-TRUSTED source the state-layer proxy reads to derive the SHARED-scope memory
// boundary (`mem:shared:{registry}:`), which used to be the runcap `bnd` claim (ADR 0052
// §C6 shared-scope resolution). Empty ⇒ the label is removed (the agent left the
// registry), so a stale boundary can never linger.
//
// If the SA already exists and is owned by a DIFFERENT controller,
// SetControllerReference returns a *controllerutil.AlreadyOwnedError.
// ensureAgentIdentitySA wraps it as an *identitySAConflictError so the caller
// (Reconcile) can surface Ready=False and stop cleanly — no hot-loop.
func (r *AgentDeploymentReconciler) ensureAgentIdentitySA(ctx context.Context, deploy *agentsv1alpha1.AgentDeployment, saName, registryID string) error {
	if saName == "" {
		return nil
	}
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: deploy.Namespace},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, sa, func() error {
		// Manage ONLY our registry label — never clobber labels another actor set.
		if registryID != "" {
			if sa.Labels == nil {
				sa.Labels = map[string]string{}
			}
			sa.Labels[registryIDLabel] = registryID
		} else {
			delete(sa.Labels, registryIDLabel)
		}
		// Strip the unused default kube-API token from agent pods by default (m79.4, m52 C10).
		//
		// Setting it on THIS identity SA (not the ksvc PodSpec) is deliberate: Knative Serving
		// restricts the RevisionSpec.PodSpec and has no automountServiceAccountToken feature flag,
		// so setting it on the pod template would be stripped/rejected (the m5.7 landmine class).
		// The SA-level setting removes ONLY the default auto-mounted token; the explicit projected
		// proxy-token volume (declared on the pod spec) is UNAFFECTED.
		//
		// Default (hardened): false — the identity SA has zero RBAC and the runtime never reads
		// the default token. Opt-in: true when spec.mountServiceAccountToken is set — for an agent
		// that legitimately builds an in-cluster kube config. Reconciled here (idempotently) like
		// the registry label so a toggle re-converges.
		//
		// COVERAGE: this only reaches agents that HAVE an identity SA (memory/proxy/OBO). A plain
		// agent on the namespace's shared `default` SA is out of scope — toggling automount on a
		// shared SA would affect every workload in the namespace; a universal identity SA is a
		// separate follow-up.
		sa.AutomountServiceAccountToken = ptr.To(mountAPIToken(deploy))
		return ctrl.SetControllerReference(deploy, sa, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil // namespace going away — nothing to own; not an error
		}
		// A foreign controller already owns this SA: surface it as a clear,
		// actionable conflict rather than propagating an opaque error that
		// would wedge the reconcile in an endless requeue loop.
		var alreadyOwned *controllerutil.AlreadyOwnedError
		if errors.As(err, &alreadyOwned) {
			return &identitySAConflictError{
				saName:    saName,
				ownerKind: alreadyOwned.Owner.Kind,
				ownerName: alreadyOwned.Owner.Name,
			}
		}
		return fmt.Errorf("reconciling agent identity ServiceAccount %s: %w", saName, err)
	}
	return nil
}

// reconcileKnativeService builds the pod template and wraps it in a Knative
// Service whose name and namespace match the AgentDeployment. It returns the
// ksvc as it stands on the API server after the operation (including any
// pre-existing status from the Knative controller).
//
// The stable revision name ("{service}-{specHash}" plus, when a binding/
// membership resolves, "-h<digest8>") makes CreateOrUpdate idempotent: the same
// inputs → same revision name → Knative sees no template change → no new
// revision. A changed spec/binding produces a new name → a new revision.
//
// Autoscaling annotations: the base min/max come from spec.scaling
// (scale-to-zero default). When a request-rate / custom-metric AgentScalingPolicy
// targets the agent, its class/metric/min/max annotations are applied HERE
// (m7.5: the AgentDeployment reconciler is the single writer of the ksvc and its
// autoscaling annotations; the AgentScalingPolicy controller owns only the
// separate KEDA ScaledObject).
func (r *AgentDeploymentReconciler) reconcileKnativeService(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	hash string,
) (*servingv1.Service, error) {
	pod, err := r.buildPodTemplate(ctx, deploy)
	if err != nil {
		return nil, fmt.Errorf("building pod template: %w", err)
	}
	if err = r.ensureAgentIdentitySA(ctx, deploy, pod.serviceAccountName, pod.membership.RegistryID); err != nil {
		return nil, err
	}

	revName := deploy.Name + "-" + hash
	if pod.digest != "" {
		revName = revName + "-h" + pod.digest
	}

	annotations, err := r.autoscalingAnnotations(ctx, deploy)
	if err != nil {
		return nil, fmt.Errorf("resolving autoscaling annotations: %w", err)
	}
	// C8d auditability: when the launcher was injected, stamp its image on the revision so an operator can
	// query "which agents still run launcher X" (the injected image is also on the initContainer, but the
	// annotation makes it a metadata-level query).
	if len(pod.initContainers) > 0 {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[launcherImageAnnotation] = r.LauncherImage
	}

	desiredSpec := servingv1.ServiceSpec{
		ConfigurationSpec: servingv1.ConfigurationSpec{
			Template: servingv1.RevisionTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:        revName,
					Labels:      pod.labels,
					Annotations: annotations,
				},
				Spec: servingv1.RevisionSpec{
					PodSpec: corev1.PodSpec{
						ServiceAccountName: pod.serviceAccountName,
						InitContainers:     pod.initContainers,
						Containers:         pod.containers,
						Volumes:            pod.volumes,
					},
				},
			},
		},
	}

	ksvc := &servingv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploy.Name,
			Namespace: deploy.Namespace,
		},
	}

	desiredRev := revName
	_, err = ctrl.CreateOrUpdate(ctx, r.Client, ksvc, func() error {
		// Only (re)apply the spec when the desired revision differs from what
		// exists. On an unchanged spec-hash, leave the live ksvc.Spec alone so
		// Knative's create-time defaults (container names, probe thresholds,
		// timeouts) are preserved — re-applying our bare spec would reset those
		// to invalid zero-values and the Knative webhook rejects the update
		// ("changes without a name change"). A changed spec carries a new
		// revision name, which Knative requires for pod-spec changes.
		if ksvc.Spec.Template.Name != desiredRev {
			ksvc.Spec = desiredSpec
		}
		return ctrl.SetControllerReference(deploy, ksvc, r.Scheme)
	})
	if err != nil {
		return nil, err
	}

	return ksvc, nil
}

// autoscalingAnnotations returns the Knative autoscaling annotations for the
// ksvc revision template. The min/max scale defaults come from spec.scaling; a
// request-rate / custom-metric AgentScalingPolicy that targets the agent
// overrides them and adds the class/metric annotations.
func (r *AgentDeploymentReconciler) autoscalingAnnotations(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
) (map[string]string, error) {
	minScale := int32(0)
	maxScale := int32(3)
	if deploy.Spec.Scaling != nil {
		minScale = deploy.Spec.Scaling.Min
		if deploy.Spec.Scaling.Max > 0 {
			maxScale = deploy.Spec.Scaling.Max
		}
	}

	annotations := map[string]string{
		"autoscaling.knative.dev/min-scale": strconv.Itoa(int(minScale)),
		"autoscaling.knative.dev/max-scale": strconv.Itoa(int(maxScale)),
	}

	policy, err := r.knativeScalingPolicy(ctx, deploy)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return annotations, nil
	}

	// A request-rate / custom-metric policy drives the Knative autoscaler: its
	// min/max bound the revision, and the class/metric annotations select the
	// autoscaling behaviour. The policy min/max override spec.scaling (the policy
	// is the explicit scaling intent).
	annotations["autoscaling.knative.dev/min-scale"] = strconv.Itoa(int(policy.Spec.Min))
	annotations["autoscaling.knative.dev/max-scale"] = strconv.Itoa(int(policy.Spec.Max))

	switch policy.Spec.Trigger {
	case triggerRequestRate:
		// Request-rate → concurrency-based KPA autoscaling (the Knative default
		// class). Making the class/metric explicit keeps the annotation set
		// self-describing and stable across Knative default changes.
		annotations["autoscaling.knative.dev/class"] = "kpa.autoscaling.knative.dev"
		annotations["autoscaling.knative.dev/metric"] = "concurrency"
	case triggerCustomMetric:
		// Custom-metric → pass the policy's class/metric straight through.
		if policy.Spec.Metric != nil {
			annotations["autoscaling.knative.dev/class"] = policy.Spec.Metric.Class
			annotations["autoscaling.knative.dev/metric"] = policy.Spec.Metric.Metric
		}
	}
	return annotations, nil
}

// collectorImage / discoveryImage resolve the injected sidecar images: the configured
// override (COLLECTOR_IMAGE / DISCOVERY_IMAGE, OPS-1) or the project-default constant. The
// defaults are dev.local/* (ImagePullBackOff off a kind cluster), so a real install sets
// these — wired through the chart (OPS-4).
func (r *AgentDeploymentReconciler) collectorImage() string {
	if r.CollectorImage != "" {
		return r.CollectorImage
	}
	return telemetry.CollectorImage
}

func (r *AgentDeploymentReconciler) discoveryImage() string {
	if r.DiscoveryImage != "" {
		return r.DiscoveryImage
	}
	return DiscoveryImage
}

// reconcileCollector ensures the per-agent collector-config ConfigMap and
// returns the collector sidecar container + its config volume. Langfuse export
// is enabled only when a `langfuse-otlp` Secret exists in the agent's namespace
// (seeded by `dev-up M=3`); otherwise the collector runs debug-only, which is
// the automated-assertion sink the e2e slice reads via `kubectl logs`.
func (r *AgentDeploymentReconciler) reconcileCollector(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
) (corev1.Container, corev1.Volume, error) {
	var langfuseEnv []corev1.EnvVar
	langfuse := false

	// Secret lookup: the agent's own namespace acts as a per-namespace
	// override; the platform namespace (where dev-up seeds the dev keys) is
	// the fallback default. Without the fallback, agents outside
	// agent-engine-system silently ran debug-only and nothing ever reached
	// Langfuse (caught 2026-07-08 by querying the Langfuse API at M3 close).
	var sec corev1.Secret
	// UNCACHED read (see APIReader): a cached read is racy around informer resync and can
	// render a collector without the LANGFUSE_OTLP env while its ConfigMap references it —
	// crash-looping the sidecar. Fall back to the cached client when no APIReader is wired.
	secretReader := client.Reader(r.Client)
	if r.APIReader != nil {
		secretReader = r.APIReader
	}
	err := secretReader.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: telemetry.LangfuseSecretName}, &sec)
	if apierrors.IsNotFound(err) && deploy.Namespace != gateway.GatewayNamespace {
		err = secretReader.Get(ctx, client.ObjectKey{Namespace: gateway.GatewayNamespace, Name: telemetry.LangfuseSecretName}, &sec)
	}
	switch {
	case err == nil:
		langfuse = true
		// Dev keys are deterministic and non-secret; wiring the endpoint + basic
		// auth as literal env is acceptable for the M3 dev posture (production
		// would use a mounted secret ref). See specs/observability.md.
		langfuseEnv = []corev1.EnvVar{
			{Name: "LANGFUSE_OTLP_ENDPOINT", Value: string(sec.Data["otlp-endpoint"])},
			{Name: "LANGFUSE_OTLP_AUTH", Value: telemetry.BasicAuthHeader(
				string(sec.Data["public-key"]), string(sec.Data["secret-key"]),
			)},
		}
	case apierrors.IsNotFound(err):
		// debug-only; not an error.
	default:
		return corev1.Container{}, corev1.Volume{}, fmt.Errorf("checking langfuse secret: %w", err)
	}

	// Redaction policy (§13.3): the built-in email/SSN/key detectors are always
	// on; spec.tracePolicy may append per-agent custom detectors. A custom
	// pattern that fails RE2 compilation is a fatal reconcile error — we do NOT
	// silently drop it and ship a weaker policy than the operator declared;
	// admission-level validation (the Pattern length/charset) is best-effort, so
	// the reconciler is the backstop that refuses an un-compilable policy.
	detectors, err := telemetry.DetectorsWithCustom(customDetectors(deploy))
	if err != nil {
		return corev1.Container{}, corev1.Volume{}, fmt.Errorf("building trace-redaction policy: %w", err)
	}

	cmName := telemetry.ConfigMapName(deploy.Name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: deploy.Namespace},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["config.yaml"] = telemetry.RenderConfig(langfuse, detectors)
		return ctrl.SetControllerReference(deploy, cm, r.Scheme)
	}); err != nil {
		return corev1.Container{}, corev1.Volume{}, fmt.Errorf("upserting collector ConfigMap: %w", err)
	}

	return telemetry.Container(cmName, langfuseEnv, r.collectorImage()), telemetry.Volume(cmName), nil
}

// customDetectors adapts the AgentDeployment's optional spec.tracePolicy into
// the telemetry package's detector-spec shape. Returns nil when no tracePolicy
// (or no custom detectors) is set, so only the built-in defaults apply.
func customDetectors(deploy *agentsv1alpha1.AgentDeployment) []telemetry.CustomDetectorSpec {
	if deploy.Spec.TracePolicy == nil || len(deploy.Spec.TracePolicy.CustomDetectors) == 0 {
		return nil
	}
	out := make([]telemetry.CustomDetectorSpec, 0, len(deploy.Spec.TracePolicy.CustomDetectors))
	for _, d := range deploy.Spec.TracePolicy.CustomDetectors {
		out = append(out, telemetry.CustomDetectorSpec{Name: d.Name, Pattern: d.Pattern})
	}
	return out
}

// syncStatus mirrors the Knative Service's Ready condition and URL into the
// AgentDeployment status and sets latestVersion + observedGeneration.
// If the ksvc has no Ready condition (e.g. in envtest where no Knative
// controller runs), status is set to Unknown.
func (r *AgentDeploymentReconciler) syncStatus(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	ksvc *servingv1.Service,
	latestVersion string,
) error {
	readyStatus := metav1.ConditionUnknown
	readyReason := "AwaitingKnativeController"
	readyMsg := ""

	for _, c := range ksvc.Status.Conditions {
		if string(c.Type) == conditionReady {
			readyStatus = metav1.ConditionStatus(c.Status)
			if c.Reason != "" {
				readyReason = c.Reason
			} else {
				readyReason = "KnativeServiceReady"
			}
			readyMsg = c.Message
			break
		}
	}

	apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMsg,
		ObservedGeneration: deploy.Generation,
	})

	if u := preferredAgentURL(ksvc); u != "" {
		deploy.Status.URL = u
	}
	deploy.Status.LatestVersion = latestVersion
	deploy.Status.ObservedGeneration = deploy.Generation

	return r.Status().Update(ctx, deploy)
}

// preferredAgentURL picks the address recorded in AgentDeployment status.url. It
// prefers the ksvc's CLUSTER-LOCAL address (status.address.url) over the external
// route URL (status.url), because the primary consumer is the IN-CLUSTER BFF
// Playground invoke (m12.7, ADR 0011): the external ksvc URL resolves to the ingress,
// which is not hairpin-routable from inside the cluster (on kind it points at the
// calling pod's own localhost) so dispatching there fails with a 502. The cluster-local
// address is what an in-cluster caller must use, and it aligns serving with the eventing
// execution model (which already records a cluster-local URL). Returns "" when the ksvc
// has neither yet, so the caller leaves any existing status.url untouched.
func preferredAgentURL(ksvc *servingv1.Service) string {
	if ksvc.Status.Address != nil && ksvc.Status.Address.URL != nil {
		return ksvc.Status.Address.URL.String()
	}
	if ksvc.Status.URL != nil {
		return ksvc.Status.URL.String()
	}
	return ""
}

// setReadyFalse sets the Ready condition to False with the given reason and
// message, then updates the status subresource. It returns an empty Result and
// nil error so the reconciler stops without requeueing (the next spec change or
// watch trigger will re-evaluate).
func (r *AgentDeploymentReconciler) setReadyFalse(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	reason, message string,
) (ctrl.Result, error) {
	apimeta.SetStatusCondition(&deploy.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: deploy.Generation,
	})
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := r.Status().Update(ctx, deploy); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}
	return ctrl.Result{}, nil
}

// memoryBindingDigest returns a short hash capturing whether a MemoryBinding
// exists and what addr it resolves to. It is one COMPONENT of the combined
// revision-name digest (combinedBindingDigest) so bind/unbind/addr-change each
// roll a new revision.
//
// Returns "" when hasBinding is false (no memory state → the component
// contributes the empty string, symmetric with the tool-binding path).
func memoryBindingDigest(hasBinding bool, addr string) string {
	if !hasBinding {
		return ""
	}
	h := sha256.Sum256([]byte(addr))
	return fmt.Sprintf("%x", h[:])[:8]
}

// combinedBindingDigest folds the per-binding-type structural digests into the
// SINGLE 8-hex digest used as the revision-name suffix ("-h<digest8>").
//
// Rationale: stacking one suffix per binding type ("-b<8>-m<8>-r<8>") grows the
// revision name 10 chars per type and blows the 63-char DNS-1035 label limit
// for admission-valid agent names; one combined digest bounds the total suffix
// at 19 chars forever (see the revision-name comment in reconcileKnativeService).
// New structural inputs (M6 registry membership, M8 cost budget, M9 prompt) fold
// in HERE, extending the hashed framing — they never add a new suffix.
//
// Properties:
//   - "" when NO structural input of any type resolves (bare pre-M4 revision name).
//   - Changes when ANY component changes (each component is embedded whole).
//   - Cannot collide across presence combinations: components are hex-only
//     (never contain '=' or ';'), so the "b=<x>;m=<y>;r=<z>;g=<w>;p=<v>" framing is
//     unambiguous — every presence combination hashes a distinct string.
//   - Deterministic: fixed field order, deterministic component derivations
//     (tool digest sorts by binding name; memory digest hashes the resolved addr;
//     registry digest hashes the resolved registryId + role + allowedCallers;
//     budget digest hashes the caps + soft percentage; prompt digest hashes the
//     git pointer + the resolved prompt version).
//
// The prompt component (M9) is what makes a prompt-only deploy roll a new Knative
// revision WITHOUT an image rebuild: a promptRef/ref swap changes promptDigest →
// a new combined suffix → a new revision, while spec.Image (the user container's
// image) is untouched → the image digest is unchanged.
func combinedBindingDigest(toolDigest, memDigest, regDigest, budgetDigest, promptDigest, tenantDigest,
	proxyDigest, runtimeDigest, guardrailDigest, kbDigest string,
) string {
	if toolDigest == "" && memDigest == "" && regDigest == "" && budgetDigest == "" && promptDigest == "" &&
		tenantDigest == "" && proxyDigest == "" && runtimeDigest == "" && guardrailDigest == "" &&
		kbDigest == "" {
		return ""
	}
	h := sha256.Sum256([]byte("b=" + toolDigest + ";m=" + memDigest + ";r=" + regDigest +
		";g=" + budgetDigest + ";p=" + promptDigest + ";t=" + tenantDigest + ";x=" + proxyDigest +
		";rt=" + runtimeDigest + ";gr=" + guardrailDigest + ";kb=" + kbDigest))
	return fmt.Sprintf("%x", h[:])[:8]
}

// statelayerProxyDigest captures the state-layer proxy URL as a revision-digest
// component (M53). It is non-empty ONLY when the proxy URL is set AND this agent
// actually gets it injected (a memory binding OR tenant quota) — so flipping the
// proxy on/off, or changing its URL, rolls a new revision for exactly the agents
// whose pod template changes (fixing the M4-landmine gap where enabling the proxy
// on live agents would otherwise NOT roll — the pod would keep the old direct-Valkey
// wiring). No spurious roll for agents that don't inject it.
func statelayerProxyDigest(proxyURL string, injected, mountAPIToken bool) string {
	if proxyURL == "" || !injected {
		return ""
	}
	// "|sa" version tag (ADR 0052 §C6 RESOLUTION): pod-token pods now run a per-agent
	// identity SA. Folding it here rolls a new revision for agents that were ALREADY
	// proxy-attached (same proxyURL, previously default SA) so the serviceAccountName
	// pod-spec change actually lands — a change that didn't move the revision name would
	// be silently dropped by the CreateOrUpdate name-guard (the M4 landmine).
	//
	// "|automount=<bool>" (m79.4): the identity SA's AutomountServiceAccountToken tracks
	// spec.mountServiceAccountToken. It is part of the pod's effective spec (the kubelet
	// mounts or omits the default token per the SA setting), so a toggle must roll a new
	// revision too — otherwise an opt-in change wouldn't reach a running pod until an
	// unrelated restart. Default false ⇒ "automount=false" (byte-different from the pre-m79.4
	// digest, so live proxy agents roll ONCE onto the hardened SA — a real spec change).
	h := sha256.Sum256([]byte(proxyURL + "|sa" + "|automount=" + strconv.FormatBool(mountAPIToken)))
	return fmt.Sprintf("%x", h[:])[:8]
}

// registryMembershipDigest returns a short hash capturing the agent's M6
// registry membership as it affects the pod template: whether it is a member,
// the registryId (→ AGENT_REGISTRY_ID + the membership pod label), the injected
// guard defaults, the role (→ AGENT_ROLE), and the allowedCallers (→
// AGENT_ALLOWED_CALLERS). It is one COMPONENT of combinedBindingDigest so
// join/leave, a registryId change, a guard change, or a role/allowlist edit each
// roll a new revision — the env/label must actually land, and the CreateOrUpdate
// guard skips re-applying the spec when the revision name is unchanged (the M4
// silent-loss landmine).
//
// Returns "" when the agent is NOT a member (no mesh state → the component
// contributes the empty string, symmetric with the tool/memory paths). Role and
// allowedCallers are included even though they come from the AgentDeployment
// spec (already in the spec-hash) so that a role/allowlist edit rolls the SAME
// combined "-h" suffix rather than only the spec-hash prefix — keeping the
// membership-driven env changes bundled in one place is harmless and explicit.
func registryMembershipDigest(m registryMembership, role string, allowedCallers []string) string {
	if !m.IsMember {
		return ""
	}
	payload := fmt.Sprintf("id=%s;depth=%d;budget=%d;role=%s;callers=%s",
		m.RegistryID, m.MaxDepth, m.HopBudget, role, strings.Join(allowedCallers, ","))
	h := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", h[:])[:8]
}

// budgetSoftPct returns the soft-threshold percentage to inject, defaulting to
// 80 (the CRD default) when the field is zero. The CRD applies the default
// server-side on create, but a client-side build (envtest, direct construction)
// may leave it 0; defaulting here keeps the injected BUDGET_SOFT_PCT sane in
// every path.
func budgetSoftPct(b *agentsv1alpha1.BudgetSpec) int32 {
	if b == nil || b.SoftThresholdPct == 0 {
		return 80
	}
	return b.SoftThresholdPct
}

// budgetDigest returns a short hash capturing the agent's cost budget as it
// affects the pod template: whether a budget is set, the two caps, and the soft
// percentage (all → injected env + MODEL_GATEWAY_URL repointing). It is one
// COMPONENT of combinedBindingDigest so a budget add/remove/change rolls a new
// revision — the env/URL change must actually land, and the CreateOrUpdate guard
// skips re-applying the spec when the revision name is unchanged (the M4
// silent-loss landmine). Returns "" when no budget is set (symmetric with the
// tool/memory/registry components).
func budgetDigest(b *agentsv1alpha1.BudgetSpec) string {
	if b == nil {
		return ""
	}
	payload := fmt.Sprintf("conv=%s;agent=%s;soft=%d",
		b.PerConversationUSD, b.PerAgentUSD, budgetSoftPct(b))
	h := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", h[:])[:8]
}

// runtimeDigest returns a short hash capturing the agent's runtime config as it
// affects the pod template: whether spec.runtime is set and its marshaled JSON
// content (→ injected AGENT_RUNTIME env). It is one COMPONENT of
// combinedBindingDigest so a runtime add/remove/change rolls a new revision —
// the env change must actually land, and the CreateOrUpdate guard skips
// re-applying the spec when the revision name is unchanged (the M4
// silent-loss landmine). Returns "" when spec.runtime is nil (symmetric with
// the other components, backward-compatible: pre-M65 revision names unchanged).
func runtimeDigest(rt *agentsv1alpha1.RuntimeSpec) string {
	if rt == nil {
		return ""
	}
	b, err := json.Marshal(rt)
	if err != nil {
		// Marshal of a well-typed struct should never fail; treat as empty to
		// avoid a silent break in the digest path (the injection path returns
		// the error explicitly, so it will surface before the digest is used).
		return ""
	}
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h[:])[:8]
}

// envVarPresent reports whether name appears in the given env slice.
// Used to avoid double-injecting AGENT_NAME when the operator has already set it.
func envVarPresent(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
// The controller owns AgentVersion and Knative Service so that changes to either
// (e.g. a Knative controller updating ksvc status) requeue the parent deployment.
//
// It also watches MCPToolBinding and MemoryBinding: add/remove/change events map
// to a requeue of the referenced agent so this reconciler re-renders the pod
// template (the STRUCTURAL side of a binding change — sidecar containers and
// env injection). The annotation-free requeue mechanism reads spec.agentRef
// straight off the event object; no field index is used.
func (r *AgentDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapBindingToAgent := handler.EnqueueRequestsFromMapFunc(
		func(_ context.Context, obj client.Object) []reconcile.Request {
			b, ok := obj.(*agentsv1alpha1.MCPToolBinding)
			if !ok || b.Spec.AgentRef == "" {
				return nil
			}
			return []reconcile.Request{{
				NamespacedName: client.ObjectKey{Namespace: b.Namespace, Name: b.Spec.AgentRef},
			}}
		},
	)

	// MemoryBinding → requeue the referenced AgentDeployment so this reconciler
	// re-resolves memory bindings and re-renders the pod template (env injection).
	// Delete events are included: the binding object remains readable until
	// DeletionTimestamp is set, and listAgentMemoryBindings excludes it so the
	// env drops on the re-render.
	mapMemoryBindingToAgent := handler.EnqueueRequestsFromMapFunc(
		func(_ context.Context, obj client.Object) []reconcile.Request {
			b, ok := obj.(*agentsv1alpha1.MemoryBinding)
			if !ok || b.Spec.AgentRef == "" {
				return nil
			}
			return []reconcile.Request{{
				NamespacedName: client.ObjectKey{Namespace: b.Namespace, Name: b.Spec.AgentRef},
			}}
		},
	)

	// AgentRegistry → requeue every AgentDeployment in the registry's namespace
	// (M6): a registry create/delete, a memberSelector change, a registryId
	// change, or a guard edit alters which agents are members and what mesh env
	// they carry. The selector could match any agent's labels, and a delete event
	// carries the last-known selector, so the cheap correct move is to re-resolve
	// every agent in the namespace — this reconciler recomputes membership and
	// re-renders (env + membership label + revision roll) or drops it. Bounded:
	// registries are few and this only fires on registry events, not steady state.
	mapRegistryToAgents := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			reg, ok := obj.(*agentsv1alpha1.AgentRegistry)
			if !ok {
				return nil
			}
			var list agentsv1alpha1.AgentDeploymentList
			if err := mgr.GetClient().List(ctx, &list, client.InNamespace(reg.Namespace)); err != nil {
				return nil
			}
			reqs := make([]reconcile.Request, 0, len(list.Items))
			for i := range list.Items {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
				})
			}
			return reqs
		},
	)

	// AgentScalingPolicy → requeue the referenced AgentDeployment (m7.5). A
	// request-rate / custom-metric policy changes the ksvc's autoscaling
	// annotations; a schedule policy toggles the job-model agent between a bare
	// Job and a CronJob. Reads spec.agentRef straight off the event object (delete
	// events retain it), matching the binding→agent maps. queue-depth policies are
	// harmless here — the ksvc/job re-render is a no-op for them (that trigger is
	// the AgentScalingPolicy controller's KEDA path).
	mapScalingPolicyToAgent := handler.EnqueueRequestsFromMapFunc(
		func(_ context.Context, obj client.Object) []reconcile.Request {
			p, ok := obj.(*agentsv1alpha1.AgentScalingPolicy)
			if !ok || p.Spec.AgentRef == "" {
				return nil
			}
			return []reconcile.Request{{
				NamespacedName: client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.AgentRef},
			}}
		},
	)

	// Tenant → requeue every AgentDeployment in the tenant's namespaces (M47). A
	// tenant create/delete or a model-cap change alters the TENANT_* env every
	// member agent carries, so re-resolve + re-render them (the caps also feed the
	// digest, so a change rolls the revision). Bounded: tenants are few and this
	// only fires on tenant events.
	mapTenantToAgents := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			t, ok := obj.(*agentsv1alpha1.Tenant)
			if !ok {
				return nil
			}
			var reqs []reconcile.Request
			for _, ns := range t.Spec.Namespaces {
				var list agentsv1alpha1.AgentDeploymentList
				if err := mgr.GetClient().List(ctx, &list, client.InNamespace(ns)); err != nil {
					continue
				}
				for i := range list.Items {
					reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
				}
			}
			return reqs
		},
	)

	// AgentTeam → requeue its SUPERVISOR AgentDeployment (M64). A roster or spawn-budget change alters
	// the DELEGATE_* env the supervisor carries, so re-render it (the change also feeds the digest, so it
	// rolls the revision). Reads spec.supervisor.agentRef straight off the event object (delete events
	// retain it), matching the other spec-ref maps.
	mapTeamToSupervisor := handler.EnqueueRequestsFromMapFunc(
		func(_ context.Context, obj client.Object) []reconcile.Request {
			t, ok := obj.(*agentsv1beta1.AgentTeam)
			if !ok || t.Spec.Supervisor.AgentRef == "" {
				return nil
			}
			return []reconcile.Request{{
				NamespacedName: client.ObjectKey{Namespace: t.Namespace, Name: t.Spec.Supervisor.AgentRef},
			}}
		},
	)

	// GuardrailPolicy → requeue every AgentDeployment in the policy's namespace that
	// references it via spec.guardrailPolicyRef (M66, ADR 0059 §8). A policy edit changes
	// the resolved-policy hash → the referencing agent's combined digest → the Knative
	// revision rolls (compliance tightening propagates). A policy CREATE also re-reconciles
	// a previously-dangling referencer (Ready=False → resolves → serves guarded). Delete
	// events carry the last-known name, so a dropped policy re-reconciles its referencers
	// and they fail closed. Scoped to referencers only (not the whole namespace) — the
	// name is a stable field on each AgentDeployment, so the exact set is cheap to compute.
	mapGuardrailPolicyToAgents := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			gp, ok := obj.(*agentsv1beta1.GuardrailPolicy)
			if !ok {
				return nil
			}
			var list agentsv1alpha1.AgentDeploymentList
			if err := mgr.GetClient().List(ctx, &list, client.InNamespace(gp.Namespace)); err != nil {
				return nil
			}
			var reqs []reconcile.Request
			for i := range list.Items {
				if list.Items[i].Spec.GuardrailPolicyRef == gp.Name {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
					})
				}
			}
			return reqs
		},
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentDeployment{}).
		Owns(&agentsv1alpha1.AgentVersion{}).
		Owns(&servingv1.Service{}).
		Owns(&eventingv1.Trigger{}).
		Owns(&batchv1.Job{}).
		Owns(&batchv1.CronJob{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Watches(&agentsv1alpha1.MCPToolBinding{}, mapBindingToAgent).
		Watches(&agentsv1alpha1.MemoryBinding{}, mapMemoryBindingToAgent).
		Watches(&agentsv1alpha1.AgentRegistry{}, mapRegistryToAgents).
		Watches(&agentsv1alpha1.AgentScalingPolicy{}, mapScalingPolicyToAgent).
		Watches(&agentsv1alpha1.Tenant{}, mapTenantToAgents).
		Watches(&agentsv1beta1.AgentTeam{}, mapTeamToSupervisor).
		Watches(&agentsv1beta1.GuardrailPolicy{}, mapGuardrailPolicyToAgents).
		Named("agentdeployment").
		Complete(r)
}
