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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

const (
	// tenantLabel marks every resource + namespace a Tenant stamps, so the
	// controller can prune them (a cluster-scoped Tenant cannot ownerRef-GC its
	// namespaced output — K8s disallows cluster→namespaced owner GC). The value is
	// the shared api/v1alpha1.TenantLabel — one source of truth with the
	// state-layer proxy's tenant resolver (ADR 0050 Amд 2 Correction 2a).
	tenantLabel = agentsv1alpha1.TenantLabel
	// tenantFinalizer guards cross-namespace cleanup on Tenant delete.
	tenantFinalizer = "agents.ctxmesh.ai/tenant-cleanup"
	// tenantQuotaName is the fixed name of the ResourceQuota stamped per namespace.
	tenantQuotaName = "tenant-quota"
	// tenantNetworkPolicyName is the fixed name of the cross-tenant-deny NetworkPolicy (opt-in).
	tenantNetworkPolicyName = "tenant-isolation"
)

// TenantReconciler reconciles a Tenant (ADR 0046, M47). A Tenant groups namespaces
// (1 ns ∈ ≤1 tenant) and caps their compute via a ResourceQuota stamped on each
// member namespace. It labels every stamped resource + namespace with tenantLabel
// and prunes via a finalizer + a per-reconcile diff (dropped/contested namespaces).
// The cross-tenant NetworkPolicy (isolation) and the TENANT_* pod injection are
// separate tasks (m47.2b / m47.3).
type TenantReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=tenants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile converges the Tenant's member namespaces: it stamps/updates a
// ResourceQuota on each owned namespace and prunes any it no longer owns.
func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tenant agentsv1alpha1.Tenant
	if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: prune everything this tenant stamped, then drop the finalizer.
	if !tenant.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&tenant, tenantFinalizer) {
			if err := r.pruneNamespaces(ctx, tenant.Name, nil); err != nil {
				return ctrl.Result{}, fmt.Errorf("pruning on delete: %w", err)
			}
			controllerutil.RemoveFinalizer(&tenant, tenantFinalizer)
			if err := r.Update(ctx, &tenant); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	if controllerutil.AddFinalizer(&tenant, tenantFinalizer) {
		if err := r.Update(ctx, &tenant); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// The update re-triggers reconcile; proceed with the object in hand.
	}

	// Resolve which requested namespaces this tenant actually OWNS: a namespace
	// claimed by another tenant is skipped (fail-safe — never double-stamp).
	owned, contested, err := r.resolveOwnership(ctx, &tenant)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving ownership: %w", err)
	}

	for _, ns := range owned {
		if err := r.stampNamespace(ctx, &tenant, ns); err != nil {
			return ctrl.Result{}, fmt.Errorf("stamping namespace %q: %w", ns, err)
		}
	}
	// Prune namespaces previously stamped by this tenant but no longer owned.
	if err := r.pruneNamespaces(ctx, tenant.Name, owned); err != nil {
		return ctrl.Result{}, fmt.Errorf("pruning dropped namespaces: %w", err)
	}

	return ctrl.Result{}, r.updateStatus(ctx, &tenant, owned, contested)
}

// resolveOwnership splits the tenant's requested namespaces into the ones it owns
// and the ones another tenant already claims (contested, skipped).
func (r *TenantReconciler) resolveOwnership(ctx context.Context, tenant *agentsv1alpha1.Tenant) (owned, contested []string, err error) {
	var all agentsv1alpha1.TenantList
	if err := r.List(ctx, &all); err != nil {
		return nil, nil, err
	}
	claimedByOther := map[string]string{} // namespace → owning tenant
	for i := range all.Items {
		other := &all.Items[i]
		if other.Name == tenant.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		for _, ns := range other.Spec.Namespaces {
			if _, seen := claimedByOther[ns]; !seen {
				claimedByOther[ns] = other.Name
			}
		}
	}
	seen := map[string]bool{}
	for _, ns := range tenant.Spec.Namespaces {
		if seen[ns] {
			continue
		}
		seen[ns] = true
		if _, taken := claimedByOther[ns]; taken {
			contested = append(contested, ns)
			continue
		}
		owned = append(owned, ns)
	}
	slices.Sort(owned)
	slices.Sort(contested)
	return owned, contested, nil
}

// stampNamespace labels the namespace for this tenant and converges its ResourceQuota.
func (r *TenantReconciler) stampNamespace(ctx context.Context, tenant *agentsv1alpha1.Tenant, ns string) error {
	var namespace corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: ns}, &namespace); err != nil {
		// A requested namespace that does not exist is not fatal — skip it; the
		// namespace watch re-reconciles when it is created.
		return client.IgnoreNotFound(err)
	}
	if namespace.Labels[tenantLabel] != tenant.Name {
		if namespace.Labels == nil {
			namespace.Labels = map[string]string{}
		}
		namespace.Labels[tenantLabel] = tenant.Name
		if err := r.Update(ctx, &namespace); err != nil {
			return fmt.Errorf("labelling namespace: %w", err)
		}
	}

	if err := r.reconcileNetworkPolicy(ctx, tenant, ns); err != nil {
		return fmt.Errorf("network policy: %w", err)
	}

	hard := computeHard(tenant.Spec.Quota)
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: tenantQuotaName, Namespace: ns}}
	if len(hard) == 0 {
		// No compute quota requested — ensure any previously-stamped one is gone.
		return client.IgnoreNotFound(r.Delete(ctx, quota))
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
		if quota.Labels == nil {
			quota.Labels = map[string]string{}
		}
		quota.Labels[tenantLabel] = tenant.Name
		quota.Spec.Hard = hard
		return nil
	})
	return err
}

// reconcileNetworkPolicy stamps (or removes) the opt-in cross-tenant-deny NetworkPolicy on a member
// namespace. When spec.networkIsolation is off it ensures none exists. The policy is SERVING-SAFE: it
// default-denies but explicitly allows same-tenant namespaces + the knative/kourier data plane + the
// platform egress (DNS, langfuse, gateway/valkey/minio) — the exact allowlist the AgentRegistry NP uses,
// scoped by the tenant namespace-label instead of the registry-id pod-label. A missing allowance would
// sever /invoke (an m5.7-class landmine — proven live in m47.8, not envtest which has no CNI).
func (r *TenantReconciler) reconcileNetworkPolicy(ctx context.Context, tenant *agentsv1alpha1.Tenant, ns string) error {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: tenantNetworkPolicyName, Namespace: ns}}
	if !tenant.Spec.NetworkIsolation {
		return client.IgnoreNotFound(r.Delete(ctx, np))
	}
	sameTenant := &metav1.LabelSelector{MatchLabels: map[string]string{tenantLabel: tenant.Name}}
	platformNS := func(name string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{namespaceNameLabel: name}},
		}
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		if np.Labels == nil {
			np.Labels = map[string]string{}
		}
		np.Labels[tenantLabel] = tenant.Name
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // every pod in the member namespace
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: sameTenant}}}, // intra-tenant
				{From: []networkingv1.NetworkPolicyPeer{ // the knative/kourier data plane
					platformNS(knativeServingNamespace),
					platformNS(kourierSystemNamespace),
					platformNS(knativeEventingNamespace),
				}},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{ // DNS (kube-system CoreDNS), both protocols
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{namespaceNameLabel: kubeSystemNamespace}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{dnsAppLabel: dnsAppLabelValue}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: protoPtr(corev1.ProtocolUDP), Port: intstrPtr(dnsPort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(dnsPort)},
					},
				},
				{ // collector → Langfuse (:3000) — the m6.4 export landmine
					To:    []networkingv1.NetworkPolicyPeer{platformNS(langfuseNamespace)},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(langfusePort)}},
				},
				{ // platform backends in agent-engine-system: gateway :4000, direct valkey :6379,
					// minio :9000, state-layer PROXY :8080 (the m53.7 cutover default for
					// memory/quota/dedup), token-service :8443 (long-term-memory OBO). Omitting
					// :8080 makes a member's quota fail-closed (402) post-cutover (audit SEC-1).
					To: []networkingv1.NetworkPolicyPeer{platformNS(agentEngineSystemNamespace)},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(modelGatewayPort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(memoryBackendPort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(objectStorePort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(statelayerProxyPort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(tokenServicePort)},
					},
				},
				{ // intra-tenant A2A + the knative data plane it egresses through
					To: []networkingv1.NetworkPolicyPeer{
						{NamespaceSelector: sameTenant},
						platformNS(knativeServingNamespace),
						platformNS(kourierSystemNamespace),
					},
				},
			},
		}
		return nil
	})
	return err
}

// computeHard builds the ResourceQuota hard limits from the tenant's compute quota.
// Invalid quantities are skipped (a partial quota beats a failed reconcile); an
// empty quota returns nil (no ResourceQuota stamped).
func computeHard(q *agentsv1alpha1.TenantComputeQuota) corev1.ResourceList {
	if q == nil {
		return nil
	}
	hard := corev1.ResourceList{}
	if q.CPU != "" {
		if v, err := resource.ParseQuantity(q.CPU); err == nil {
			hard[corev1.ResourceRequestsCPU] = v
			hard[corev1.ResourceLimitsCPU] = v
		}
	}
	if q.Memory != "" {
		if v, err := resource.ParseQuantity(q.Memory); err == nil {
			hard[corev1.ResourceRequestsMemory] = v
			hard[corev1.ResourceLimitsMemory] = v
		}
	}
	if q.Pods > 0 {
		hard[corev1.ResourcePods] = *resource.NewQuantity(q.Pods, resource.DecimalSI)
	}
	if len(hard) == 0 {
		return nil
	}
	return hard
}

// pruneNamespaces removes this tenant's stamp (ResourceQuota + namespace label)
// from every namespace it labels that is NOT in keep. keep=nil prunes all (delete).
func (r *TenantReconciler) pruneNamespaces(ctx context.Context, tenantName string, keep []string) error {
	keepSet := map[string]bool{}
	for _, ns := range keep {
		keepSet[ns] = true
	}
	var namespaces corev1.NamespaceList
	if err := r.List(ctx, &namespaces, client.MatchingLabels{tenantLabel: tenantName}); err != nil {
		return err
	}
	for i := range namespaces.Items {
		ns := &namespaces.Items[i]
		if keepSet[ns.Name] {
			continue
		}
		quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: tenantQuotaName, Namespace: ns.Name}}
		if err := r.Delete(ctx, quota); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting quota in %q: %w", ns.Name, err)
		}
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: tenantNetworkPolicyName, Namespace: ns.Name}}
		if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting network policy in %q: %w", ns.Name, err)
		}
		delete(ns.Labels, tenantLabel)
		if err := r.Update(ctx, ns); err != nil {
			return fmt.Errorf("unlabelling %q: %w", ns.Name, err)
		}
	}
	return nil
}

// updateStatus writes memberNamespaces + the Ready / NamespaceConflict conditions.
func (r *TenantReconciler) updateStatus(ctx context.Context, tenant *agentsv1alpha1.Tenant, owned, contested []string) error {
	tenant.Status.MemberNamespaces = int32(len(owned))
	meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            fmt.Sprintf("%d namespace(s) reconciled", len(owned)),
		ObservedGeneration: tenant.Generation,
	})
	if len(contested) > 0 {
		meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
			Type:               "NamespaceConflict",
			Status:             metav1.ConditionTrue,
			Reason:             "ClaimedByAnotherTenant",
			Message:            fmt.Sprintf("skipped namespaces already owned by another tenant: %v", contested),
			ObservedGeneration: tenant.Generation,
		})
	} else {
		meta.RemoveStatusCondition(&tenant.Status.Conditions, "NamespaceConflict")
	}
	return r.Status().Update(ctx, tenant)
}

// SetupWithManager wires the Tenant controller. It also watches Namespaces so a
// member namespace created after the Tenant gets quota'd, mapping a namespace to
// every tenant that lists it.
func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapNamespaceToTenants := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			var list agentsv1alpha1.TenantList
			if err := mgr.GetClient().List(ctx, &list); err != nil {
				return nil
			}
			var reqs []reconcile.Request
			for i := range list.Items {
				if slices.Contains(list.Items[i].Spec.Namespaces, obj.GetName()) {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: client.ObjectKey{Name: list.Items[i].Name},
					})
				}
			}
			return reqs
		})
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Tenant{}).
		Watches(&corev1.Namespace{}, mapNamespaceToTenants).
		Named("tenant").
		Complete(r)
}
