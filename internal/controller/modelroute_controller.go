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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctx-mesh/agent-engine/api/v1alpha1"
	"github.com/ctx-mesh/agent-engine/internal/gateway"
)

const (
	// configHashAnnotation is the pod-template annotation the controller uses to
	// force a Deployment rollout when the rendered gateway config changes.
	configHashAnnotation = "agent-engine.ctxmesh.ai/config-hash"

	// sbEnvPrefix is the env-var prefix for SecretBinding credentials on the
	// gateway Deployment. Must match gateway.EnvVarName's prefix.
	sbEnvPrefix = "SB_"
)

// ModelRouteReconciler reconciles ModelRoute objects.
//
// Reconcile strategy: each reconcile call operates on the COMPLETE set of
// ModelRoutes across all namespaces. A trigger from any single route, binding,
// or secret enqueues all existing routes so the shared gateway ConfigMap and
// Deployment always reflect the full desired state. This is safe because:
//   - ConfigMap CreateOrUpdate: identical content → resourceVersion unchanged.
//   - Deployment update: identical annotation + env → resourceVersion unchanged.
//   - Status updates use optimistic locking; transient conflicts are non-fatal.
//
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=modelroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=modelroutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=modelroutes/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=secretbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
type ModelRouteReconciler struct {
	client.Client
}

// Reconcile is the main reconcile loop. It is triggered by any change to a
// ModelRoute, SecretBinding, or Secret (via watchers in SetupWithManager) and
// renders the complete gateway config from the full set of ModelRoutes.
func (r *ModelRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Check whether the triggering ModelRoute still exists. If deleted we still
	// fall through to renderAndSync so the config reflects the deletion.
	var trigger agentsv1alpha1.ModelRoute
	if err := r.Get(ctx, req.NamespacedName, &trigger); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("fetching triggering ModelRoute: %w", err)
		}
		log.Info("Triggering ModelRoute not found (deleted); re-rendering config")
	}

	return r.renderAndSync(ctx)
}

// renderAndSync is the core reconcile logic: it lists all ModelRoutes across
// all namespaces, resolves their SecretBindings and Secrets, renders the
// LiteLLM config, updates the gateway ConfigMap and Deployment, and sets the
// Ready condition on every ModelRoute.
func (r *ModelRouteReconciler) renderAndSync(ctx context.Context) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// ── 1. List all ModelRoutes ────────────────────────────────────────────────
	var mrList agentsv1alpha1.ModelRouteList
	if err := r.List(ctx, &mrList); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing ModelRoutes: %w", err)
	}

	// ── 2. Resolve SecretBindings and Secrets ─────────────────────────────────
	bindings := make(map[string]agentsv1alpha1.SecretBinding)
	secretRVs := make(map[string]string)

	for i := range mrList.Items {
		mr := &mrList.Items[i]
		for _, p := range mr.Spec.Providers {
			if p.Provider == "mock" || p.SecretBindingRef == "" {
				continue
			}

			bindingKey := mr.Namespace + "/" + p.SecretBindingRef
			if _, seen := bindings[bindingKey]; seen {
				continue
			}

			var sb agentsv1alpha1.SecretBinding
			if err := r.Get(ctx, client.ObjectKey{
				Namespace: mr.Namespace,
				Name:      p.SecretBindingRef,
			}, &sb); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("getting SecretBinding %s: %w", bindingKey, err)
				}
				log.Info("SecretBinding not found; route will be excluded", "binding", bindingKey)
				continue
			}
			bindings[bindingKey] = sb

			secretKey := mr.Namespace + "/" + sb.Spec.SecretRef.Name
			if _, seen := secretRVs[secretKey]; seen {
				continue
			}

			var secret corev1.Secret
			if err := r.Get(ctx, client.ObjectKey{
				Namespace: mr.Namespace,
				Name:      sb.Spec.SecretRef.Name,
			}, &secret); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("getting Secret %s: %w", secretKey, err)
				}
				// empty RV signals "not found" to the render function
				secretRVs[secretKey] = ""
				log.Info("Secret not found; route will be excluded", "secret", secretKey)
				continue
			}
			secretRVs[secretKey] = secret.ResourceVersion
		}
	}

	// ── 3. Render config ──────────────────────────────────────────────────────
	renderResult := gateway.Render(mrList.Items, bindings, secretRVs)

	// ── 4. CreateOrUpdate gateway ConfigMap ───────────────────────────────────
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gateway.GatewayConfigMapName,
			Namespace: gateway.GatewayNamespace,
		},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["config.yaml"] = renderResult.ConfigYAML
		return nil
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("upserting gateway ConfigMap: %w", err)
	}

	// ── 5. Patch gateway Deployment env + pod-template annotation ─────────────
	if err := r.syncGatewayDeployment(ctx, renderResult); err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing gateway Deployment: %w", err)
	}

	// ── 6. Update Ready conditions on all ModelRoutes ─────────────────────────
	excludedSet := make(map[string]bool, len(renderResult.Excluded))
	for _, e := range renderResult.Excluded {
		excludedSet[e] = true
	}

	for i := range mrList.Items {
		mr := &mrList.Items[i]
		routeKey := mr.Namespace + "/" + mr.Name

		var condStatus metav1.ConditionStatus
		var reason, message string

		if excludedSet[routeKey] {
			condStatus = metav1.ConditionFalse
			reason = "SecretUnresolved"
			message = "one or more referenced SecretBindings or Secrets could not be resolved; " +
				"route is excluded from the gateway config"
		} else {
			condStatus = metav1.ConditionTrue
			reason = "Rendered"
			message = "route rendered into the gateway config"
		}

		apimeta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
			Type:               conditionReady,
			Status:             condStatus,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: mr.Generation,
		})

		if err := r.Status().Update(ctx, mr); err != nil {
			if apierrors.IsConflict(err) {
				log.Info("conflict updating ModelRoute status; will requeue", "route", routeKey)
			} else {
				log.Error(err, "updating ModelRoute status", "route", routeKey)
			}
		}
	}

	return ctrl.Result{}, nil
}

// syncGatewayDeployment patches the gateway Deployment with the config-hash
// pod-template annotation and SB_* env vars derived from the render result.
// If the Deployment does not exist yet the function returns nil — it will be
// synced on the next reconcile after the operator is deployed.
func (r *ModelRouteReconciler) syncGatewayDeployment(ctx context.Context, result gateway.Result) error {
	var deploy appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: gateway.GatewayNamespace,
		Name:      gateway.GatewayDeploymentName,
	}, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting gateway Deployment: %w", err)
	}

	// Set config-hash annotation on the pod template to trigger rollout on change.
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = map[string]string{}
	}
	deploy.Spec.Template.Annotations[configHashAnnotation] = result.Hash

	// Merge env vars: preserve non-SB_ vars, replace all SB_* vars with the new set.
	if len(deploy.Spec.Template.Spec.Containers) > 0 {
		existing := deploy.Spec.Template.Spec.Containers[0].Env
		merged := make([]corev1.EnvVar, 0, len(existing)+len(result.EnvVars))
		for _, e := range existing {
			if !strings.HasPrefix(e.Name, sbEnvPrefix) {
				merged = append(merged, e)
			}
		}
		merged = append(merged, result.EnvVars...)
		deploy.Spec.Template.Spec.Containers[0].Env = merged
	}

	if err := r.Update(ctx, &deploy); err != nil {
		return fmt.Errorf("updating gateway Deployment: %w", err)
	}
	return nil
}

// SetupWithManager registers the ModelRouteReconciler and its secondary watches.
//
// Watch sources:
//   - ModelRoute (primary): any change triggers reconcile for that route.
//   - SecretBinding: any change enqueues ALL existing ModelRoutes so the config
//     reflects the updated binding resolution state.
//   - Secret: any change (including rotation) enqueues ALL existing ModelRoutes
//     so the new resourceVersion propagates into the config-hash and rolls the gateway.
func (r *ModelRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueueAll := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, _ client.Object) []reconcile.Request {
			var mrList agentsv1alpha1.ModelRouteList
			if err := mgr.GetClient().List(ctx, &mrList); err != nil {
				return nil
			}
			reqs := make([]reconcile.Request, len(mrList.Items))
			for i := range mrList.Items {
				reqs[i] = reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(&mrList.Items[i]),
				}
			}
			return reqs
		},
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.ModelRoute{}).
		Watches(&agentsv1alpha1.SecretBinding{}, enqueueAll).
		Watches(&corev1.Secret{}, enqueueAll).
		Named("modelroute").
		Complete(r)
}
