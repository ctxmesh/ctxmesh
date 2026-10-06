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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/gateway"
	"github.com/ctxmesh/ctxmesh/internal/telemetry"
)

const (
	// configHashAnnotation is the pod-template annotation the controller uses to
	// force a Deployment rollout when the rendered gateway config changes.
	configHashAnnotation = "ctxmesh.ai/config-hash"

	// sbEnvPrefix is the env-var prefix for SecretBinding credentials on the
	// gateway Deployment. Must match gateway.EnvVarName's prefix.
	sbEnvPrefix = "SB_"

	// gatewaySyncLabel marks a Secret in the gateway namespace as a controller-
	// managed MIRROR of a provider Secret that lives in another namespace, so the
	// gateway Deployment's SB_* secretKeyRefs (which resolve in the gateway
	// namespace) can mount it. Mirrors are GC'd when no route references them.
	gatewaySyncLabel = "agents.ctxmesh.ai/gateway-secret-sync"
	gatewaySyncValue = "true"
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
type ModelRouteReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=modelroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=modelroutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=modelroutes/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=secretbindings,verbs=get;list;watch
// SEC-3: cluster-wide READ (provider-key Secrets live in arbitrary tenant namespaces —
// unavoidable, same posture as cert-manager/ESO) but the WRITES (the gateway-Secret mirror,
// syncGatewaySecrets) only ever land in ctxmesh, so scope create/update/delete
// to a namespaced Role there — a compromised manager can't write Secrets cluster-wide.
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,namespace=ctxmesh,verbs=create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch

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
	// mirrors: provider Secrets to sync into the gateway namespace so the Deployment's SB_*
	// secretKeyRefs can mount a provider connected in another namespace (ADR 0018), keyed by
	// their gateway-namespace name (gateway.MirrorSecretName).
	mirrors := make(map[string]gatewayMirror)

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
			// Mirror the resolved Secret into the gateway namespace unless it already
			// lives there — otherwise the gateway pod's secretKeyRef can't mount it.
			if mr.Namespace != gateway.GatewayNamespace {
				mirrors[gateway.MirrorSecretName(mr.Namespace, sb.Spec.SecretRef.Name)] = gatewayMirror{
					source: secretKey, secret: secret,
				}
			}
		}
	}

	// ── 2b. Mirror provider Secrets into the gateway namespace ────────────────
	unwritten, err := r.syncGatewaySecrets(ctx, mirrors)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing gateway secrets: %w", err)
	}
	// A route whose mirror could not be written must not render: its reference would resolve to
	// whatever Secret holds that name in the gateway namespace. Marking the source "not found"
	// excludes it.
	for _, source := range unwritten {
		secretRVs[source] = ""
	}

	// ── 3. Render config ──────────────────────────────────────────────────────
	// Enable gateway trace spans when Langfuse is configured (secret present in
	// the gateway namespace); otherwise render clean (CI has no Langfuse).
	otel, staleOTelHeaders, err := r.resolveOTelConfig(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	renderResult := gateway.Render(mrList.Items, bindings, secretRVs, otel)

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
	serving, err := r.syncGatewayDeployment(ctx, renderResult)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing gateway Deployment: %w", err)
	}
	if staleOTelHeaders {
		if err := r.deleteStaleOTelHeaders(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}

	// ── 6. Update Ready conditions on all ModelRoutes ─────────────────────────
	excludedSet := make(map[string]bool, len(renderResult.Excluded))
	for _, e := range renderResult.Excluded {
		excludedSet[e] = true
	}

	for i := range mrList.Items {
		mr := &mrList.Items[i]
		routeKey := mr.Namespace + "/" + mr.Name

		ready := routeReadyCondition(mr, excludedSet[routeKey], serving)
		changed := mr.Status.ObservedGeneration != mr.Generation
		mr.Status.ObservedGeneration = mr.Generation
		if apimeta.SetStatusCondition(&mr.Status.Conditions, ready) {
			changed = true
		}
		if !changed {
			continue // a requeue while the gateway rolls must not rewrite every route's status
		}

		if err := r.Status().Update(ctx, mr); err != nil {
			// Return the error so the reconcile REQUEUES (audit FUNC-6): a conflict or a
			// transient API failure otherwise left status stale until an unrelated event —
			// the old "will requeue" log was a lie (it returned nil and never requeued).
			return ctrl.Result{}, fmt.Errorf("updating ModelRoute status %s: %w", routeKey, err)
		}
	}

	// Nothing watches the gateway Deployment (that would cache every Deployment in the cluster), so
	// look again until it serves the config.
	switch serving {
	case gatewayRolling:
		return ctrl.Result{RequeueAfter: gatewayRolloutPoll}, nil
	case gatewayAbsent:
		return ctrl.Result{RequeueAfter: gatewayAbsentPoll}, nil
	}
	return ctrl.Result{}, nil
}

// routeReadyCondition is a route's Ready condition. Ready means a call by this alias works now, so it
// waits for the gateway to serve the config that holds the route: "rendered" alone let a first call
// made right after `kubectl wait` fail with LiteLLM's "Invalid model name". A route already served at
// this generation stays served while another route's change rolls the gateway, because the old pods'
// config holds it too.
func routeReadyCondition(mr *agentsv1alpha1.ModelRoute, excluded bool, serving gatewayState) metav1.Condition {
	c := metav1.Condition{Type: conditionReady, Status: metav1.ConditionFalse, ObservedGeneration: mr.Generation}
	prev := apimeta.FindStatusCondition(mr.Status.Conditions, conditionReady)
	alreadyServed := prev != nil && prev.Status == metav1.ConditionTrue && prev.Reason == reasonServed &&
		prev.ObservedGeneration == mr.Generation
	switch {
	case excluded:
		c.Reason = "SecretUnresolved"
		c.Message = "one or more referenced SecretBindings or Secrets could not be resolved; " +
			"route is excluded from the gateway config"
	case serving == gatewayServing, serving == gatewayRolling && alreadyServed:
		c.Status, c.Reason, c.Message = metav1.ConditionTrue, reasonServed, "the gateway serves this route"
	case serving == gatewayAbsent:
		c.Reason, c.Message = "GatewayAbsent", "route rendered; the gateway Deployment does not exist"
	case prev != nil && (prev.Reason == reasonServed || prev.Reason == reasonGatewayRolling):
		// Served before: the pods still running serve the previous version of this route, so a call
		// by the alias works while the new version rolls out.
		c.Reason = reasonGatewayRolling
		c.Message = "the gateway is rolling out this route's new version; the previous one is still served"
	default:
		// Never served: a call by this alias fails ("Invalid model name") until the roll completes.
		c.Reason = reasonNotYetServed
		c.Message = "route rendered; the gateway has not started serving it yet"
	}
	return c
}

// Ready reasons a route's consumers act on: Served (calls work), GatewayRolling (a new version is
// rolling out and the previous one still answers), NotYetServed (a new route; calls fail until served).
const (
	reasonServed         = "Served"
	reasonGatewayRolling = "GatewayRolling"
	reasonNotYetServed   = "NotYetServed"
)

// gatewayState is how far the gateway is from serving the config just rendered.
type gatewayState int

const (
	gatewayServing gatewayState = iota
	gatewayRolling
	gatewayAbsent
)

const (
	gatewayRolloutPoll = 5 * time.Second
	gatewayAbsentPoll  = 30 * time.Second
)

// gatewayServes reports whether the Deployment has fully rolled out the config with this hash: the
// rollout `kubectl rollout status` would call complete, with no pod of an older template left.
func gatewayServes(d *appsv1.Deployment, hash string) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	st := d.Status
	return want > 0 &&
		d.Spec.Template.Annotations[configHashAnnotation] == hash &&
		st.ObservedGeneration >= d.Generation &&
		st.UpdatedReplicas == want && st.Replicas == want && st.AvailableReplicas == want
}

// gatewayOTelHeadersSecret holds the gateway's pre-built OTEL_HEADERS value. LiteLLM wants the
// whole "Authorization=Basic ..." string, so it is derived here from the langfuse-otlp Secret
// and kept in a Secret rather than rendered into the Deployment's env as a literal.
const gatewayOTelHeadersSecret = "ctxmesh-gateway-otel"

// gatewayOTelRoleLabel marks the derived Secret as the controller's. The controller writes or
// deletes a Secret of that name only when it carries this label.
const (
	gatewayOTelRoleLabel = "agents.ctxmesh.ai/role"
	gatewayOTelRoleValue = "gateway-otel-headers"
)

// resolveOTelConfig returns the gateway's trace-export settings from the langfuse-otlp Secret in
// the gateway namespace. An absent or incomplete Secret gives the zero value, which disables the
// otel callback (CI / no-Langfuse). staleDerived reports a derived header Secret left from when
// export was on; the caller deletes it once the Deployment no longer references it, so a
// credential does not outlive the Secret it came from.
func (r *ModelRouteReconciler) resolveOTelConfig(ctx context.Context) (gateway.OTelConfig, bool, error) {
	derivedKey := client.ObjectKey{Namespace: gateway.GatewayNamespace, Name: gatewayOTelHeadersSecret}
	var existing corev1.Secret
	derivedExists := false
	switch err := r.Get(ctx, derivedKey, &existing); {
	case err == nil:
		if existing.Labels[gatewayOTelRoleLabel] != gatewayOTelRoleValue {
			return gateway.OTelConfig{}, false, fmt.Errorf(
				"gateway namespace holds a Secret named %s that the controller does not manage; refusing to overwrite it",
				gatewayOTelHeadersSecret)
		}
		derivedExists = true
	case !apierrors.IsNotFound(err):
		return gateway.OTelConfig{}, false, fmt.Errorf("reading gateway OTel header Secret: %w", err)
	}

	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: gateway.GatewayNamespace, Name: telemetry.LangfuseSecretName,
	}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return gateway.OTelConfig{}, derivedExists, nil
		}
		// Not "tracing off": treating a transient read error as absence would roll the gateway
		// off and back on.
		return gateway.OTelConfig{}, false, fmt.Errorf("reading %s Secret: %w", telemetry.LangfuseSecretName, err)
	}
	if missing := telemetry.MissingLangfuseKeys(sec.Data); len(missing) > 0 {
		logf.FromContext(ctx).Info("langfuse-otlp Secret is incomplete; gateway tracing stays off",
			"namespace", gateway.GatewayNamespace, "missing", missing)
		return gateway.OTelConfig{}, derivedExists, nil
	}

	derived := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: derivedKey.Name, Namespace: derivedKey.Namespace}}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, derived, func() error {
		if derived.Labels == nil {
			derived.Labels = map[string]string{}
		}
		// Deliberately NOT gatewaySyncLabel: syncGatewaySecrets garbage-collects by that label,
		// and this Secret is not a provider mirror.
		derived.Labels["app.kubernetes.io/managed-by"] = "ctxmesh-controller"
		derived.Labels[gatewayOTelRoleLabel] = gatewayOTelRoleValue
		derived.Type = corev1.SecretTypeOpaque
		derived.Data = map[string][]byte{gateway.OTelHeadersKey: []byte("Authorization=" + telemetry.BasicAuthHeader(
			string(sec.Data[telemetry.LangfuseKeyPublic]), string(sec.Data[telemetry.LangfuseKeySecret])))}
		return nil
	}); err != nil {
		return gateway.OTelConfig{}, false, fmt.Errorf("writing gateway OTel header Secret: %w", err)
	}
	return gateway.OTelConfig{
		Endpoint:        string(sec.Data[telemetry.LangfuseKeyEndpoint]),
		HeadersSecret:   gatewayOTelHeadersSecret,
		HeadersSecretRV: derived.ResourceVersion,
	}, false, nil
}

// deleteStaleOTelHeaders removes the derived header Secret after the gateway has stopped
// referencing it. It re-checks the label rather than trusting the earlier read.
func (r *ModelRouteReconciler) deleteStaleOTelHeaders(ctx context.Context) error {
	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: gateway.GatewayNamespace, Name: gatewayOTelHeadersSecret}, &sec); err != nil {
		return client.IgnoreNotFound(err)
	}
	if sec.Labels[gatewayOTelRoleLabel] != gatewayOTelRoleValue {
		return nil
	}
	if err := r.Delete(ctx, &sec); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("removing stale gateway OTel header Secret: %w", err)
	}
	return nil
}

// gatewayMirror is a provider Secret to mirror into the gateway namespace, with the
// "<namespace>/<name>" it came from.
type gatewayMirror struct {
	source string
	secret corev1.Secret
}

// mirrorSourceAnnotation records which Secret a mirror copies, for whoever reads the gateway
// namespace: the mirror's own name is a hash.
const mirrorSourceAnnotation = "agents.ctxmesh.ai/mirrored-from"

// syncGatewaySecrets mirrors each resolved provider Secret into the gateway namespace, so the
// gateway Deployment's SB_* secretKeyRefs — which resolve in the gateway namespace — can mount a
// provider connected in ANY namespace (ADR 0018). Each mirror is labelled for GC: a
// previously-synced Secret no longer referenced by any route is removed.
//
// A Secret of a mirror's name that the controller did not write is never overwritten. Its source
// is returned in unwritten, and the caller excludes the routes that need it.
func (r *ModelRouteReconciler) syncGatewaySecrets(ctx context.Context, mirrors map[string]gatewayMirror) ([]string, error) {
	log := logf.FromContext(ctx)
	referenced := make(map[string]bool, len(mirrors))
	var unwritten []string
	for name, m := range mirrors {
		referenced[name] = true
		var cur corev1.Secret
		err := r.Get(ctx, client.ObjectKey{Namespace: gateway.GatewayNamespace, Name: name}, &cur)
		switch {
		case err == nil && cur.Labels[gatewaySyncLabel] != gatewaySyncValue:
			log.Info("gateway namespace already has an un-synced Secret of this name; its routes are excluded",
				"name", name, "source", m.source)
			unwritten = append(unwritten, m.source)
			continue
		case err != nil && !apierrors.IsNotFound(err):
			return nil, fmt.Errorf("checking gateway Secret %s: %w", name, err)
		}
		data := m.secret.Data
		source := m.source
		mirror := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gateway.GatewayNamespace}}
		if _, err := ctrl.CreateOrUpdate(ctx, r.Client, mirror, func() error {
			if mirror.Labels == nil {
				mirror.Labels = map[string]string{}
			}
			mirror.Labels[gatewaySyncLabel] = gatewaySyncValue
			if mirror.Annotations == nil {
				mirror.Annotations = map[string]string{}
			}
			mirror.Annotations[mirrorSourceAnnotation] = source
			mirror.Type = corev1.SecretTypeOpaque
			mirror.Data = data
			return nil
		}); err != nil {
			return nil, fmt.Errorf("mirroring gateway Secret %s: %w", name, err)
		}
	}

	// GC: remove synced mirrors no longer referenced by any route.
	var existing corev1.SecretList
	if err := r.List(ctx, &existing,
		client.InNamespace(gateway.GatewayNamespace),
		client.MatchingLabels{gatewaySyncLabel: gatewaySyncValue}); err != nil {
		return nil, fmt.Errorf("listing synced gateway Secrets: %w", err)
	}
	for i := range existing.Items {
		s := &existing.Items[i]
		if referenced[s.Name] {
			continue
		}
		if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("gc gateway Secret %s: %w", s.Name, err)
		}
	}
	return unwritten, nil
}

// syncGatewayDeployment patches the gateway Deployment with the config-hash
// pod-template annotation and SB_* env vars derived from the render result, and
// reports whether the gateway already serves that config. A missing Deployment is
// gatewayAbsent, not an error: the reconcile checks again later.
func (r *ModelRouteReconciler) syncGatewayDeployment(ctx context.Context, result gateway.Result) (gatewayState, error) {
	var deploy appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: gateway.GatewayNamespace,
		Name:      gateway.GatewayDeploymentName,
	}, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return gatewayAbsent, nil
		}
		return gatewayRolling, fmt.Errorf("getting gateway Deployment: %w", err)
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
			if !strings.HasPrefix(e.Name, sbEnvPrefix) && !strings.HasPrefix(e.Name, gateway.OTelEnvPrefix) {
				merged = append(merged, e)
			}
		}
		merged = append(merged, result.EnvVars...)
		deploy.Spec.Template.Spec.Containers[0].Env = merged
	}

	if err := r.Update(ctx, &deploy); err != nil {
		return gatewayRolling, fmt.Errorf("updating gateway Deployment: %w", err)
	}
	if gatewayServes(&deploy, result.Hash) {
		return gatewayServing, nil
	}
	return gatewayRolling, nil
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
		// One worker, deliberately: every route renders into the ONE gateway ConfigMap and Deployment, so
		// concurrent reconciles of different routes would only race each other's writes.
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		For(&agentsv1alpha1.ModelRoute{}).
		Watches(&agentsv1alpha1.SecretBinding{}, enqueueAll).
		// SEC-3: metadata-only Secret watch — the informer caches PartialObjectMetadata
		// (name/ns/labels/RV), NEVER Secret Data, so cluster-wide Secret events (rotation)
		// still roll the gateway without mirroring every Secret's payload into memory. Reads
		// go live (DisableFor Secret on the manager client).
		WatchesMetadata(&corev1.Secret{}, enqueueAll).
		Named("modelroute").
		Complete(r)
}
