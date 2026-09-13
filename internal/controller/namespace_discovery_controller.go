package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
)

// NamespaceDiscoveryReconciler mirrors the EXISTENCE of every namespace into the control-plane DB,
// independent of tenancy.
//
// WHY THIS EXISTS
// ---------------
// The console resolves its working namespace from GET /api/namespaces. A caller bound
// PER-NAMESPACE — the shape ADR 0046 and catalog.go's tenant-isolation precondition require, and
// the one the chart ships — cannot `list namespaces` cluster-wide, so the BFF enumerates candidates
// from this mirror and filters them with a caller-scoped SSAR (ADR 0011: the BFF holds `rules: []`
// and never reads Kubernetes with its own identity).
//
// That mirror's only writer was the Tenant controller. Tenancy is opt-in, so a stock `helm install`
// has zero Tenant CRs, the mirror is empty, the fallback learns nothing, and the console gets an
// empty picker → workingNamespace "" → a CLUSTER-scoped capability probe no namespaced RoleBinding
// can satisfy → every flow reports read-only. A user with real operator rights in their namespace is
// told they have none, with no namespace offered to select their way out.
//
// This reconciler closes that gap at the only layer that may hold the privilege: the controller,
// which already watches namespaces for tenant labelling.
//
// WHAT IT DELIBERATELY DOES NOT DO
// --------------------------------
// It writes ONLY to console_namespaces and never to namespace_tenants. Tenant attribution is the
// Tenant controller's to converge, and writing it here would race that reconcile and put a
// discovery concern inside a security-bearing column.
type NamespaceDiscoveryReconciler struct {
	client.Client
	// NamespaceTenant is the control-plane mirror. Nil-safe: with no cpDB the reconciler is inert
	// rather than crash-looping, matching how the Tenant controller treats an absent store.
	NamespaceTenant namespacetenant.Store
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile records a namespace's existence, or forgets it once deleted.
func (r *NamespaceDiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.NamespaceTenant == nil {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			// Gone. Drop the discovery row so a deleted namespace stops being offered as a
			// candidate; its tenant row (if any) is the Tenant controller's to prune.
			if ferr := r.NamespaceTenant.ForgetNamespace(ctx, req.Name); ferr != nil {
				return ctrl.Result{}, ferr
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// A terminating namespace is still a namespace the caller may be working in, so it is recorded
	// until it actually goes. The NotFound path above is what removes it.
	if err := r.NamespaceTenant.RecordNamespace(ctx, ns.Name); err != nil {
		log.Error(err, "could not record namespace for console discovery", "namespace", ns.Name)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler.
func (r *NamespaceDiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Named("namespacediscovery").
		Complete(r)
}
