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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/gateway"
	"github.com/ctxmesh/agent-engine/internal/telemetry"
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

	desiredSpec := servingv1.ServiceSpec{
		ConfigurationSpec: servingv1.ConfigurationSpec{
			Template: servingv1.RevisionTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					// Stable revision name: {service}-{spec-hash}. Knative creates a
					// new revision only when spec.template changes; by fixing the name
					// to the content-addressed hash, re-reconciles of an unchanged spec
					// are a true no-op (no spurious second revision).
					// Knative requires the name to start with "{service}-".
					Name: deploy.Name + "-" + hash,
					Annotations: map[string]string{
						"autoscaling.knative.dev/min-scale": strconv.Itoa(int(minScale)),
						"autoscaling.knative.dev/max-scale": strconv.Itoa(int(maxScale)),
					},
				},
				Spec: servingv1.RevisionSpec{
					PodSpec: corev1.PodSpec{
						Containers: []corev1.Container{
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
						},
						Volumes: []corev1.Volume{collectorVol},
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

	desiredRev := deploy.Name + "-" + hash
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

// SetupWithManager sets up the controller with the Manager.
// The controller owns AgentVersion and Knative Service so that changes to either
// (e.g. a Knative controller updating ksvc status) requeue the parent deployment.
func (r *AgentDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentDeployment{}).
		Owns(&agentsv1alpha1.AgentVersion{}).
		Owns(&servingv1.Service{}).
		Named("agentdeployment").
		Complete(r)
}
