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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
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
// +kubebuilder:rbac:groups=serving.knative.dev,resources=services,verbs=get;list;watch;create;update;patch;delete
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

	// Belt-and-braces guard: the CRD enum already prevents non-"serving" values
	// from being admitted, but we handle it defensively here as well.
	if deploy.Spec.ExecutionModel != "serving" {
		log.Info("Unsupported executionModel; setting Ready=False",
			"executionModel", deploy.Spec.ExecutionModel)
		return r.setReadyFalse(ctx, &deploy, "UnsupportedExecutionModel",
			fmt.Sprintf("executionModel %q is not supported in v1alpha1; only 'serving' is valid", deploy.Spec.ExecutionModel))
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

	// ── Step 3: Knative Service ───────────────────────────────────────────────
	log.Info("Reconciling Knative Service", "name", deploy.Name)
	ksvc, err := r.reconcileKnativeService(ctx, &deploy, hash)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling Knative Service: %w", err)
	}

	// ── Step 4: Mirror status ─────────────────────────────────────────────────
	if err = r.syncStatus(ctx, &deploy, ksvc, versionName); err != nil {
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

// reconcileKnativeService creates or updates the Knative Service whose name and
// namespace match the AgentDeployment. It returns the ksvc as it stands on the
// API server after the operation (including any pre-existing status from the
// Knative controller).
//
// hash is the spec-hash used as the stable RevisionTemplate name suffix. Setting
// a deterministic name makes CreateOrUpdate idempotent across reconcile cycles:
// the same hash → same revision name → Knative sees no template change → no new
// revision is created on re-reconcile. A changed spec produces a new hash and
// therefore a new revision, which is the desired behaviour.
func (r *AgentDeploymentReconciler) reconcileKnativeService(
	ctx context.Context,
	deploy *agentsv1alpha1.AgentDeployment,
	hash string,
) (*servingv1.Service, error) {
	port := deploy.Spec.Port
	if port == 0 {
		port = 8080
	}

	minScale := int32(0)
	maxScale := int32(3)
	if deploy.Spec.Scaling != nil {
		minScale = deploy.Spec.Scaling.Min
		if deploy.Spec.Scaling.Max > 0 {
			maxScale = deploy.Spec.Scaling.Max
		}
	}

	// Platform env vars come first so they are always present; user env is appended
	// last and may override platform defaults if the operator deliberately sets the
	// same name (consistent with the treatment of AGENT_PORT).
	env := make([]corev1.EnvVar, 0, 2+len(deploy.Spec.Env))
	env = append(env, corev1.EnvVar{Name: "AGENT_PORT", Value: strconv.Itoa(int(port))})
	env = append(env, corev1.EnvVar{
		Name:  "MODEL_GATEWAY_URL",
		Value: "http://agent-engine-gateway.agent-engine-system.svc:4000",
	})
	env = append(env, deploy.Spec.Env...)

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
		return nil, err
	}

	// MCP tools (M4): resolve the agent's valid bindings. When ≥1 exists, inject
	// the discovery sidecar + tools ConfigMap volume + (sidecar-mode) tool
	// containers. The binding controller owns the CM CONTENT and the push; this
	// reconciler is the single writer of the pod template.
	validBindings, _, err := resolveAgentBindings(ctx, r.Client, deploy.Namespace, deploy.Name)
	if err != nil {
		return nil, fmt.Errorf("resolving tool bindings: %w", err)
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
		return nil, fmt.Errorf("resolving memory binding: %w", err)
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
		// AGENT_NAME: inject only if not already present in spec.env (user
		// override must win). The launcher uses AGENT_NAME for Valkey key
		// composition and as the agent.invoke span attribute.
		if !envVarPresent(deploy.Spec.Env, "AGENT_NAME") {
			env = append(env, corev1.EnvVar{Name: "AGENT_NAME", Value: deploy.Name})
		}
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

	// Revision name: bare spec-hash when there are no bindings (unchanged pre-M4
	// behaviour). With ANY binding (tool and/or memory), suffix ONE combined
	// structural digest ("-h<digest8>") so that ADD/REMOVE of a binding, a
	// sidecar image change, or a memory addr change each roll a new revision
	// (the containers/env must actually land — the CreateOrUpdate guard below
	// skips re-applying the spec when the revision name is unchanged; a stale
	// name means the injection is SILENTLY LOST, the M4 landmine). The tool
	// component deliberately EXCLUDES remote URLs and the manifest version, so
	// a manifest-ONLY change (remote URL edit) keeps the SAME revision name →
	// no roll → restart-free hot path (specs/mcp-tools.md — "Hot path vs cold
	// path"). Knative requires the name to start with "{service}-", which the
	// spec-hash prefix satisfies.
	//
	// Name budget (why ONE combined suffix, not stacked per-binding suffixes):
	// Knative revision names are DNS-1035 labels, max 63 chars. The suffix is
	// bounded at 19 chars — "-" + 8 (spec hash) + "-h" + 8 (combined digest) —
	// NO MATTER how many binding types future milestones add, leaving 44 chars
	// of agent-name budget, which the CRD enforces via a root CEL rule on
	// metadata.name (size <= 44). Stacked suffixes (the pre-fix "-b<8>-m<8>"
	// form) grew 10 chars per binding type and silently wedged reconcile for
	// admission-valid 35+-char names.
	revName := deploy.Name + "-" + hash
	toolDigest := toolmanifest.StructuralDigest(sidecarTools, hasBindings)
	memDigest := memoryBindingDigest(hasMemoryBinding, memAddr)
	if combined := combinedBindingDigest(toolDigest, memDigest); combined != "" {
		revName = revName + "-h" + combined
	}

	desiredSpec := servingv1.ServiceSpec{
		ConfigurationSpec: servingv1.ConfigurationSpec{
			Template: servingv1.RevisionTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name: revName,
					Annotations: map[string]string{
						"autoscaling.knative.dev/min-scale": strconv.Itoa(int(minScale)),
						"autoscaling.knative.dev/max-scale": strconv.Itoa(int(maxScale)),
					},
				},
				Spec: servingv1.RevisionSpec{
					PodSpec: corev1.PodSpec{
						Containers: containers,
						Volumes:    volumes,
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
// Rationale: stacking one suffix per binding type ("-b<8>-m<8>") grows the
// revision name 10 chars per type and blows the 63-char DNS-1035 label limit
// for admission-valid agent names; one combined digest bounds the total suffix
// at 19 chars forever (see the revision-name comment in reconcileKnativeService).
//
// Properties:
//   - "" when NO binding of any type resolves (bare pre-M4 revision name).
//   - Changes when EITHER component changes (each component is embedded whole).
//   - Cannot collide across presence combinations: components are hex-only
//     (never contain '=' or ';'), so the "b=<x>;m=<y>" framing is unambiguous —
//     tools-only, memory-only, and both always hash different strings.
//   - Deterministic: fixed field order, deterministic component derivations
//     (tool digest sorts by binding name; memory digest hashes the resolved addr).
func combinedBindingDigest(toolDigest, memDigest string) string {
	if toolDigest == "" && memDigest == "" {
		return ""
	}
	h := sha256.Sum256([]byte("b=" + toolDigest + ";m=" + memDigest))
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentDeployment{}).
		Owns(&agentsv1alpha1.AgentVersion{}).
		Owns(&servingv1.Service{}).
		Watches(&agentsv1alpha1.MCPToolBinding{}, mapBindingToAgent).
		Watches(&agentsv1alpha1.MemoryBinding{}, mapMemoryBindingToAgent).
		Named("agentdeployment").
		Complete(r)
}
