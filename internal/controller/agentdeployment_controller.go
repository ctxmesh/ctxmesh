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
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/gateway"
	"github.com/ctxmesh/agent-engine/internal/telemetry"
	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
)

// conditionReady is the condition type name mirrored from the Knative Service
// into AgentDeployment.status.conditions. Kept as a named constant to satisfy
// the goconst linter and to make the value easy to grep.
const conditionReady = "Ready"

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
)

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

	// envAgentName is the AGENT_NAME env var — the agent's identity, injected by
	// the memory, registry, and budget paths (each guards against double-inject).
	envAgentName = "AGENT_NAME"
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
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentversions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentscalingpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=serving.knative.dev,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=eventing.knative.dev,resources=triggers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

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

	// Normalise the execution model. The CRD default fills "serving" for omitted
	// values; a belt-and-braces default here covers a client that submits the
	// raw object without defaulting (e.g. the raw envtest client on an older
	// stored object). The CRD enum already rejects any value outside the set.
	model := deploy.Spec.ExecutionModel
	if model == "" {
		model = execModelServing
	}

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
	switch model {
	case execModelEventing:
		return r.reconcileEventing(ctx, &deploy, hash, versionName)
	case execModelJob:
		return r.reconcileJob(ctx, &deploy, versionName)
	default: // execModelServing
		return r.reconcileServing(ctx, &deploy, hash, versionName)
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

	ksvc, err := r.reconcileKnativeService(ctx, deploy, hash)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling Knative Service: %w", err)
	}
	if err = r.syncStatus(ctx, deploy, ksvc, versionName); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing status: %w", err)
	}
	return ctrl.Result{}, nil
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

	// Cost budget (M8, specs/cost-governance.md): when spec.budget is set, the
	// agent's LLM calls must flow through the launcher's in-pod budget proxy so it
	// can enforce the cap BEFORE the provider is hit. So MODEL_GATEWAY_URL points
	// at the proxy (localhost:2996), the real LiteLLM address travels as
	// GATEWAY_UPSTREAM_URL, and the three budget knobs are injected as STATIC env
	// (values known at reconcile time — NEVER valueFrom, the m5.7 Knative landmine
	// / tier1 no-valueFrom guard). Unbudgeted agents get the plain LiteLLM URL and
	// no budget env, so their path is byte-for-byte the M2 behavior.
	gatewayURL := litellmGatewayURL
	if deploy.Spec.Budget != nil {
		gatewayURL = budgetProxyURL
		env = append(env,
			corev1.EnvVar{Name: "GATEWAY_UPSTREAM_URL", Value: litellmGatewayURL},
			// Either cap may be empty (that dimension unenforced); softThreshold
			// carries the CRD default (80) when unset because the field defaults
			// server-side, but guard against a zero value defensively.
			corev1.EnvVar{Name: "BUDGET_PER_CONVERSATION_USD", Value: deploy.Spec.Budget.PerConversationUSD},
			corev1.EnvVar{Name: "BUDGET_PER_AGENT_USD", Value: deploy.Spec.Budget.PerAgentUSD},
			corev1.EnvVar{Name: "BUDGET_SOFT_PCT", Value: strconv.Itoa(int(budgetSoftPct(deploy.Spec.Budget)))},
		)
	}
	env = append(env, corev1.EnvVar{Name: "MODEL_GATEWAY_URL", Value: gatewayURL})
	env = append(env, deploy.Spec.Env...)

	// AGENT_NAME keys the per-agent spend in the budget proxy. A budgeted agent
	// needs it even without a MemoryBinding or registry membership (those paths
	// also inject it, guarded against double-injection). Inject once, user-
	// override-wins, BEFORE the memory/registry blocks so they see it present.
	if deploy.Spec.Budget != nil && !envVarPresent(env, envAgentName) && !envVarPresent(deploy.Spec.Env, envAgentName) {
		env = append(env, corev1.EnvVar{Name: envAgentName, Value: deploy.Name})
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

	// MCP tools (M4): resolve the agent's valid bindings. When ≥1 exists, inject
	// the discovery sidecar + tools ConfigMap volume + (sidecar-mode) tool
	// containers. The binding controller owns the CM CONTENT and the push; this
	// reconciler is the single writer of the pod template.
	validBindings, _, err := resolveAgentBindings(ctx, r.Client, deploy.Namespace, deploy.Name)
	if err != nil {
		return podTemplate{}, fmt.Errorf("resolving tool bindings: %w", err)
	}
	_, sidecarTools := toolmanifest.Render(validBindings)
	hasBindings := len(validBindings) > 0

	// Memory (M5): resolve the agent's MemoryBinding (if any). When present,
	// inject MEMORY_BACKEND_ADDR, MEMORY_PORT, MEMORY_KEY_NAMESPACE (downward
	// API — pod namespace), and AGENT_NAME (for Valkey key composition).
	// The single-writer rule applies: only this reconciler writes the pod
	// template; the MemoryBinding controller only sets the binding's status.
	memAddr, hasMemoryBinding, err := resolveMemoryBinding(ctx, r.Client, deploy.Namespace, deploy.Name)
	if err != nil {
		return podTemplate{}, fmt.Errorf("resolving memory binding: %w", err)
	}
	if hasMemoryBinding {
		env = append(env,
			corev1.EnvVar{Name: "MEMORY_BACKEND_ADDR", Value: memAddr},
			corev1.EnvVar{Name: "MEMORY_PORT", Value: "2998"},
			// MEMORY_KEY_NAMESPACE: the agent's own namespace, the Valkey key
			// prefix. Set as a STATIC value (the pod always runs in the
			// AgentDeployment's namespace, known here at reconcile time) — a
			// downward-API fieldRef is rejected by Knative Serving's webhook,
			// which forbids valueFrom in a ksvc pod template unless the
			// non-default kubernetes.podspec-fieldref feature flag is enabled.
			// The key prefix is the agent's own namespace regardless of where
			// the backend lives, so no downward reference is needed.
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
		env = append(env,
			corev1.EnvVar{Name: "AGENT_REGISTRY_ID", Value: membership.RegistryID},
			corev1.EnvVar{Name: "A2A_MAX_DEPTH", Value: strconv.Itoa(int(membership.MaxDepth))},
			corev1.EnvVar{Name: "A2A_HOP_BUDGET", Value: strconv.Itoa(int(membership.HopBudget))},
			// POD_NAMESPACE: the namespace A2A targets resolve in — the launcher's
			// clusterHost() builds http://{target}.{POD_NAMESPACE}.svc.cluster.local.
			// STATIC (deploy.Namespace, known here), never a downward-API fieldRef:
			// Knative's webhook rejects valueFrom in a ksvc pod template (the m5.7
			// landmine; a tier1 guard asserts no ksvc env uses valueFrom). Injected
			// UNCONDITIONALLY for a member: the memory path does NOT set it, so a
			// registry member without a MemoryBinding would otherwise resolve
			// {target}..svc.cluster.local (empty namespace → NXDOMAIN → every A2A
			// call fails unknown_target).
			corev1.EnvVar{Name: "POD_NAMESPACE", Value: deploy.Namespace},
		)
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
		// async payloads pass through capped.
		env = append(env,
			corev1.EnvVar{Name: "OBJECT_STORE_ADDR", Value: objectStoreAddr},
			corev1.EnvVar{Name: "OBJECT_STORE_ACCESS_KEY", Value: objectStoreDevAccessKey},
			corev1.EnvVar{Name: "OBJECT_STORE_SECRET_KEY", Value: objectStoreDevSecretKey},
		)
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
			Env:       env,
			Resources: resources,
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

	if hasBindings {
		containers = append(containers, discoverySidecarContainer())
		// Sidecar-mode tool containers, in the deterministic binding-name order
		// Render assigned (matches the manifest's localhost ports).
		for _, st := range sidecarTools {
			containers = append(containers, sidecarToolContainer(st))
		}
		volumes = append(volumes, toolsVolume(deploy.Name))
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
	memDigest := memoryBindingDigest(hasMemoryBinding, memAddr)
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
	combinedDigest := combinedBindingDigest(toolDigest, memDigest, regDigest, budgetDig)

	// Membership pod label: when the agent is a registry member, stamp the
	// controller-owned registry-id label on the pod template so the pods carry
	// it — the generated NetworkPolicy's podSelector and intra-registry
	// from-selector match on exactly this label.
	var templateLabels map[string]string
	if membership.IsMember {
		templateLabels = map[string]string{registryIDLabel: membership.RegistryID}
	}

	return podTemplate{
		containers: containers,
		volumes:    volumes,
		labels:     templateLabels,
		digest:     combinedDigest,
		membership: membership,
		port:       port,
	}, nil
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

	revName := deploy.Name + "-" + hash
	if pod.digest != "" {
		revName = revName + "-h" + pod.digest
	}

	annotations, err := r.autoscalingAnnotations(ctx, deploy)
	if err != nil {
		return nil, fmt.Errorf("resolving autoscaling annotations: %w", err)
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
						Containers: pod.containers,
						Volumes:    pod.volumes,
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
	err := r.Get(ctx, client.ObjectKey{Namespace: deploy.Namespace, Name: telemetry.LangfuseSecretName}, &sec)
	if apierrors.IsNotFound(err) && deploy.Namespace != gateway.GatewayNamespace {
		err = r.Get(ctx, client.ObjectKey{Namespace: gateway.GatewayNamespace, Name: telemetry.LangfuseSecretName}, &sec)
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
				string(sec.Data["public-key"]), string(sec.Data["secret-key"]))},
		}
	case apierrors.IsNotFound(err):
		// debug-only; not an error.
	default:
		return corev1.Container{}, corev1.Volume{}, fmt.Errorf("checking langfuse secret: %w", err)
	}

	cmName := telemetry.ConfigMapName(deploy.Name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: deploy.Namespace},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["config.yaml"] = telemetry.RenderConfig(langfuse)
		return ctrl.SetControllerReference(deploy, cm, r.Scheme)
	}); err != nil {
		return corev1.Container{}, corev1.Volume{}, fmt.Errorf("upserting collector ConfigMap: %w", err)
	}

	return telemetry.Container(cmName, langfuseEnv), telemetry.Volume(cmName), nil
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

	if ksvc.Status.URL != nil {
		deploy.Status.URL = ksvc.Status.URL.String()
	}
	deploy.Status.LatestVersion = latestVersion
	deploy.Status.ObservedGeneration = deploy.Generation

	return r.Status().Update(ctx, deploy)
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
// New structural inputs (M6 registry membership, M8 cost budget) fold in HERE,
// extending the hashed framing — they never add a new suffix.
//
// Properties:
//   - "" when NO structural input of any type resolves (bare pre-M4 revision name).
//   - Changes when ANY component changes (each component is embedded whole).
//   - Cannot collide across presence combinations: components are hex-only
//     (never contain '=' or ';'), so the "b=<x>;m=<y>;r=<z>;g=<w>" framing is
//     unambiguous — every presence combination hashes a distinct string.
//   - Deterministic: fixed field order, deterministic component derivations
//     (tool digest sorts by binding name; memory digest hashes the resolved addr;
//     registry digest hashes the resolved registryId + role + allowedCallers;
//     budget digest hashes the caps + soft percentage).
func combinedBindingDigest(toolDigest, memDigest, regDigest, budgetDigest string) string {
	if toolDigest == "" && memDigest == "" && regDigest == "" && budgetDigest == "" {
		return ""
	}
	h := sha256.Sum256([]byte("b=" + toolDigest + ";m=" + memDigest + ";r=" + regDigest + ";g=" + budgetDigest))
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
		Named("agentdeployment").
		Complete(r)
}
