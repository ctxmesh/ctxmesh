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
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/agent-engine/api/v1beta1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
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
	// tenantNetworkPolicyName is the fixed name of the cross-tenant-deny NetworkPolicy.
	tenantNetworkPolicyName = "tenant-isolation"

	// networkIsolationGrandfatheredAnnotation marks a tenant that was OPEN (networkIsolation absent/false)
	// at the ADR-0073 secure-default upgrade and backfilled to explicit `false` so it keeps its old
	// behavior — distinguishing it from a deliberate `false` opt-out. The controller clears it once the
	// tenant converges to isolated.
	networkIsolationGrandfatheredAnnotation = "agents.ctxmesh.ai/network-isolation-grandfathered"

	// conditionNetworkIsolated is the Tenant status condition type reporting the network-isolation posture
	// (Isolated / Grandfathered / Disabled) of the tenant's cross-tenant-deny NetworkPolicy (ADR 0073).
	conditionNetworkIsolated = "NetworkIsolated"
)

// protectedSystemNamespaces are namespaces a Tenant must NEVER claim or stamp (audit P1-3, 2026-08-16):
// stamping the tenant's default-deny NetworkPolicy (or ResourceQuota) on any of them would break cluster
// DNS / the control plane / the platform itself — a `Tenant{spec.namespaces:[kube-system]}` would fence the
// cluster's own plumbing. resolveOwnership routes any requested namespace in this set to `denied` (never
// `owned`, so it is never stamped) and surfaces a `ProtectedNamespaceRefused` status condition. This is the
// cheap, always-on guardrail; the complementary spoofable-namespace-label ValidatingWebhook (so only the
// Tenant controller can set the tenant label) is the bigger half, carded to m52.
var protectedSystemNamespaces = map[string]bool{
	kubeSystemNamespace:        true, // cluster DNS (CoreDNS) + control-plane comms — a default-deny here is a cluster outage
	"kube-public":              true,
	"kube-node-lease":          true,
	agentEngineSystemNamespace: true, // the platform: controllers, BFF, state-layer, model gateway
	kourierSystemNamespace:     true, // Knative ingress
	"knative-serving":          true, // Knative control plane (agent data plane depends on it)
	langfuseNamespace:          true, // telemetry plane
}

// TenantReconciler reconciles a Tenant (ADR 0046, M47). A Tenant groups namespaces
// (1 ns ∈ ≤1 tenant) and caps their compute via a ResourceQuota stamped on each
// member namespace. It labels every stamped resource + namespace with tenantLabel
// and prunes via a finalizer + a per-reconcile diff (dropped/contested namespaces).
// The cross-tenant NetworkPolicy (isolation) and the TENANT_* pod injection are
// separate tasks (m47.2b / m47.3).
type TenantReconciler struct {
	client.Client
	// NamespaceTenant mirrors the tenant's owned member namespaces into a small (namespace, tenant)
	// Postgres index (ADR 0067 §6, m73.3) so the m73.4 catalog can resolve tenant membership WITHOUT
	// the BFF reading namespaces (forbidden by ADR 0011). Optional/typed-nil-safe: nil ⇒ the mirror is
	// skipped (envtests without a control-plane DB), mirroring how the other optional stores are treated.
	NamespaceTenant namespacetenant.Store
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=tenants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=knowledgebases,verbs=get;list;watch

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
			// Clear the tenant's rows from the membership mirror before dropping the finalizer — the
			// finalizer is the one guaranteed chance to clean up, so a mirror error REQUEUES (returns)
			// rather than leaking stale (namespace, tenant) rows into the m73.4 catalog.
			if r.NamespaceTenant != nil {
				if err := r.NamespaceTenant.DeleteTenant(ctx, tenant.Name); err != nil {
					return ctrl.Result{}, fmt.Errorf("clearing membership mirror on delete: %w", err)
				}
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
	owned, contested, denied, err := r.resolveOwnership(ctx, &tenant)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving ownership: %w", err)
	}
	if len(denied) > 0 {
		// A protected system/platform namespace was requested — refuse it loudly (audit P1-3). It is
		// already excluded from `owned` (never stamped); log so the operator misconfig is visible beyond
		// the status condition.
		logf.FromContext(ctx).Info("tenant: refused protected system namespaces (never stamped)", "tenant", tenant.Name, "denied", denied)
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

	// Mirror the converged member set into the (namespace, tenant) index (ADR 0067 §6, m73.3) so the
	// m73.4 catalog resolves membership without the BFF reading namespaces (ADR 0011). Best-effort on
	// the converge path: a Postgres blip is LOGGED (not swallowed) but does NOT wedge the K8s
	// convergence (quota/NP/label) that already succeeded — the mirror self-heals on the next reconcile
	// (the namespace/KB watches re-drive it). The tenant-DELETE path (above) requeues instead, since the
	// finalizer is the one chance to clean up.
	r.syncMembershipMirror(ctx, tenant.Name, owned)

	// Aggregate corpus bytes across member namespaces for the storage soft-cap check
	// (ADR 0061 governance #7, M68 m68.12). Pure K8s list+sum — never re-queries Postgres.
	totalBytes, err := r.aggregateCorpusBytes(ctx, owned)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("aggregating corpus bytes: %w", err)
	}

	// ADR 0073 convergence: once a grandfathered tenant becomes isolated (networkIsolation nil/true), the
	// grandfather annotation has served its purpose — clear it so the migration dashboard shrinks. A
	// metadata Update (refreshes resourceVersion in place) before the status write below.
	if isolated := tenant.Spec.NetworkIsolation == nil || *tenant.Spec.NetworkIsolation; isolated {
		if _, ok := tenant.Annotations[networkIsolationGrandfatheredAnnotation]; ok {
			delete(tenant.Annotations, networkIsolationGrandfatheredAnnotation)
			if err := r.Update(ctx, &tenant); err != nil {
				return ctrl.Result{}, fmt.Errorf("clearing grandfather annotation: %w", err)
			}
		}
	}

	return ctrl.Result{}, r.updateStatus(ctx, &tenant, owned, contested, denied, totalBytes)
}

// resolveOwnership splits the tenant's requested namespaces into the ones it owns
// and the ones another tenant already claims (contested, skipped).
func (r *TenantReconciler) resolveOwnership(ctx context.Context, tenant *agentsv1alpha1.Tenant) (owned, contested, denied []string, err error) {
	// Which OTHER tenants merely LIST each namespace in spec (a spec-only claim, not yet
	// stamped). This alone must NOT contest a namespace another tenant already OWNS.
	var all agentsv1alpha1.TenantList
	if err := r.List(ctx, &all); err != nil {
		return nil, nil, nil, err
	}
	listedByOther := map[string]bool{}
	for i := range all.Items {
		other := &all.Items[i]
		if other.Name == tenant.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		for _, ns := range other.Spec.Namespaces {
			listedByOther[ns] = true
		}
	}
	seen := map[string]bool{}
	for _, ns := range tenant.Spec.Namespaces {
		if seen[ns] {
			continue
		}
		seen[ns] = true
		// System-namespace denylist (audit P1-3): a protected system/platform namespace is NEVER claimed
		// or stamped — stamping a default-deny NetworkPolicy on it would break the cluster/platform. Route
		// it to `denied` (a status condition) BEFORE any ownership logic, regardless of labels/listers.
		if protectedSystemNamespaces[ns] {
			denied = append(denied, ns)
			continue
		}
		// The STAMPED owner (the tenantLabel on the namespace) is AUTHORITATIVE (audit
		// FUNC-5): a spec-only claim by a second tenant never contests a namespace THIS
		// tenant already owns — the old "any other lister ⇒ contested" made the incumbent
		// prune its own quota/NP/label the moment a second tenant's spec listed the ns. Only
		// an UNSTAMPED namespace that another tenant also lists is genuinely contested (first
		// to stamp wins; neither stamps until the operator resolves the overlap).
		labelOwner, lErr := r.namespaceTenantLabel(ctx, ns)
		if lErr != nil {
			return nil, nil, nil, lErr
		}
		switch {
		case labelOwner == tenant.Name:
			owned = append(owned, ns) // incumbent — this tenant already owns it
		case labelOwner != "":
			contested = append(contested, ns) // another tenant is the STAMPED owner
		case listedByOther[ns]:
			contested = append(contested, ns) // unstamped + multiple listers → contested
		default:
			owned = append(owned, ns) // unstamped, sole lister → free to claim
		}
	}
	slices.Sort(owned)
	slices.Sort(contested)
	slices.Sort(denied)
	return owned, contested, denied, nil
}

// namespaceTenantLabel returns the tenant that has STAMPED its ownership label
// (agents.ctxmesh.ai/tenant) on ns, or "" when the namespace is missing or unstamped.
func (r *TenantReconciler) namespaceTenantLabel(ctx context.Context, ns string) (string, error) {
	var namespace corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: ns}, &namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil // missing namespace → treat as unstamped
		}
		return "", err
	}
	return namespace.Labels[tenantLabel], nil
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
	// ADR 0073 secure-default: networkIsolation is a *bool with +kubebuilder:default=true, so a
	// field-absent (nil) tenant is served as TRUE (isolate). Only an EXPLICIT false — a deliberate,
	// grandfathered opt-out — removes the policy. (A nil here is defensive; the API default fills it.)
	if tenant.Spec.NetworkIsolation != nil && !*tenant.Spec.NetworkIsolation {
		return client.IgnoreNotFound(r.Delete(ctx, np))
	}
	sameTenant := &metav1.LabelSelector{MatchLabels: map[string]string{tenantLabel: tenant.Name}}
	// ADR 0073: peerTenants opens named east-west — allow ingress from + egress to any member namespace
	// whose tenant label is in the allowlist. Empty ⇒ strict isolation (same-tenant + platform only).
	var peerPeers []networkingv1.NetworkPolicyPeer
	if len(tenant.Spec.PeerTenants) > 0 {
		peerPeers = []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: tenantLabel, Operator: metav1.LabelSelectorOpIn, Values: tenant.Spec.PeerTenants},
				},
			},
		}}
	}
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
				{From: append([]networkingv1.NetworkPolicyPeer{{NamespaceSelector: sameTenant}}, peerPeers...)}, // intra-tenant + peerTenants
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
				{ // platform backends in agent-engine-system: gateway :4000, minio :9000, state-layer PROXY
					// :8080 (memory/quota/dedup/control/SPAWN — the pod-authed, per-tenant-scoped choke point),
					// token-service :8443 (long-term-memory OBO). Omitting :8080 makes a member's quota
					// fail-closed (402) post-cutover (audit SEC-1).
					//
					// RESOLVED (audit P1-2, M94, 2026-08-17): raw Valkey `:6379` is NO LONGER in this
					// allowlist. It was direct access to the SHARED, UNAUTHENTICATED Valkey (ADR 0049) — an
					// agent could issue arbitrary Redis against every tenant's keys, bypassing the :8080
					// proxy's per-tenant scoping. The last direct-`:6379` consumer was the AgentTeam-supervisor
					// spawn guard; M94 added `/spawn/acquire`+`/spawn/release` to the state-layer proxy and a
					// launcher httpSpawnStore, and the controller now injects `STATELAYER_PROXY_URL` (not
					// `TENANT_QUOTA_ADDR`) for a supervisor when the proxy is configured. So no agent reaches
					// raw Valkey anymore — every memory/quota/dedup/control/spawn op flows through the pod-authed
					// proxy. (Live supervisor-fail-closed proof on kind+Calico is user-gated — carded m52.C13.)
					To: []networkingv1.NetworkPolicyPeer{platformNS(agentEngineSystemNamespace)},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(modelGatewayPort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(objectStorePort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(statelayerProxyPort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(tokenServicePort)},
					},
				},
				{ // intra-tenant A2A (+ peerTenants east-west) + the knative data plane it egresses through
					To: append([]networkingv1.NetworkPolicyPeer{
						{NamespaceSelector: sameTenant},
						platformNS(knativeServingNamespace),
						platformNS(kourierSystemNamespace),
					}, peerPeers...),
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
//
// The quota caps REQUESTS only (`requests.cpu`/`requests.memory`), NOT limits (audit
// FUNC-2). A ResourceQuota that tracks `limits.*` forces EVERY pod in the namespace to
// declare limits — but the controller builds agent pods requests-only (and Knative's
// queue-proxy is requests-only too), so a `limits.*` quota made admission REJECT every
// agent pod, bricking the namespace the moment a tenant set `quota.cpu`. Requests are the
// scheduler-guaranteed allocation, so capping them is the correct, standard meaning of a
// tenant compute quota. (Also capping/defaulting limits — via a per-namespace LimitRange —
// is a follow-on hardening, not needed to make the quota work.)
func computeHard(q *agentsv1alpha1.TenantComputeQuota) corev1.ResourceList {
	if q == nil {
		return nil
	}
	hard := corev1.ResourceList{}
	if q.CPU != "" {
		if v, err := resource.ParseQuantity(q.CPU); err == nil {
			hard[corev1.ResourceRequestsCPU] = v
		}
	}
	if q.Memory != "" {
		if v, err := resource.ParseQuantity(q.Memory); err == nil {
			hard[corev1.ResourceRequestsMemory] = v
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

// syncMembershipMirror upserts+prunes the (namespace, tenant) mirror to exactly the tenant's owned
// member set (ADR 0067 §6, m73.3). It is a no-op when the store is unconfigured (nil — envtest without
// a control-plane DB). Best-effort on the reconcile converge path: a store error is LOGGED (not
// swallowed, not returned) so a Postgres blip never rolls back the K8s convergence that already
// succeeded; the next reconcile re-drives the mirror. The tenant-delete path handles its own error
// (it requeues) because the finalizer is the sole cleanup opportunity.
func (r *TenantReconciler) syncMembershipMirror(ctx context.Context, tenantName string, owned []string) {
	if r.NamespaceTenant == nil {
		return
	}
	if err := r.NamespaceTenant.SetMembers(ctx, tenantName, owned); err != nil {
		logf.FromContext(ctx).Error(err, "membership mirror sync failed; will re-converge next reconcile",
			"tenant", tenantName)
	}
}

// aggregateCorpusBytes sums KnowledgeBase.status.sizeBytes across all member namespaces.
// It reads only K8s objects (no Postgres) — the KB controller projects sizeBytes from the
// corpus-status row. An empty owned list returns 0. Per-namespace List errors are returned
// so the reconcile requeues; a single-namespace error is not suppressed (partial aggregation
// would silently under-report and miss a soft-cap breach).
func (r *TenantReconciler) aggregateCorpusBytes(ctx context.Context, owned []string) (int64, error) {
	var total int64
	for _, ns := range owned {
		var kbList agentsv1beta1.KnowledgeBaseList
		if err := r.List(ctx, &kbList, client.InNamespace(ns)); err != nil {
			return 0, fmt.Errorf("listing KnowledgeBases in namespace %q: %w", ns, err)
		}
		for i := range kbList.Items {
			total += kbList.Items[i].Status.SizeBytes
		}
	}
	return total, nil
}

// updateStatus writes memberNamespaces + the Ready / NamespaceConflict / StorageSoftCapExceeded
// conditions and updates the corpus-bytes gauge + totalCorpusBytes status field.
func (r *TenantReconciler) updateStatus(ctx context.Context, tenant *agentsv1alpha1.Tenant, owned, contested, denied []string, totalCorpusBytes int64) error {
	tenant.Status.MemberNamespaces = int32(len(owned))
	tenant.Status.TotalCorpusBytes = totalCorpusBytes

	// Update the per-tenant corpus-bytes gauge (tenant label, SOFT signal — no block).
	tenantCorpusBytesGauge.WithLabelValues(tenant.Name).Set(float64(totalCorpusBytes))

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

	// Protected-namespace refusal (audit P1-3): a requested system/platform namespace was refused (never
	// stamped) so the tenant cannot fence the cluster's own plumbing. A distinct condition from
	// NamespaceConflict — the cause is a protected namespace, not another tenant's claim.
	if len(denied) > 0 {
		meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
			Type:               "ProtectedNamespaceRefused",
			Status:             metav1.ConditionTrue,
			Reason:             "SystemNamespaceDenylisted",
			Message:            fmt.Sprintf("refused to claim protected system/platform namespaces (never stamped): %v", denied),
			ObservedGeneration: tenant.Generation,
		})
	} else {
		meta.RemoveStatusCondition(&tenant.Status.Conditions, "ProtectedNamespaceRefused")
	}

	// Storage soft-cap check (ADR 0061 governance #7, M68 m68.12).
	// SOFT: exceeding the cap WARNS (condition + event) — it NEVER blocks ingestion.
	if tenant.Spec.Storage != nil && tenant.Spec.Storage.CorpusBytesSoftCap != "" {
		capQ, err := resource.ParseQuantity(tenant.Spec.Storage.CorpusBytesSoftCap)
		if err == nil { // parse errors are rejected at admission (CEL) — be defensive
			capBytes := capQ.Value()
			if totalCorpusBytes > capBytes {
				meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
					Type:   "StorageSoftCapExceeded",
					Status: metav1.ConditionTrue,
					Reason: "CorpusBytesExceedSoftCap",
					Message: fmt.Sprintf(
						"tenant corpus is %d bytes, exceeding the soft cap of %d bytes (%s); "+
							"ingestion is NOT blocked — this is a warning only. "+
							"Hard enforcement is corpusBytesHardCap (m80.3).",
						totalCorpusBytes, capBytes, tenant.Spec.Storage.CorpusBytesSoftCap),
					ObservedGeneration: tenant.Generation,
				})
			} else {
				meta.RemoveStatusCondition(&tenant.Status.Conditions, "StorageSoftCapExceeded")
			}
		}
	} else {
		meta.RemoveStatusCondition(&tenant.Status.Conditions, "StorageSoftCapExceeded")
	}

	// Storage HARD-cap check (ADR 0061 governance #7 hard enforcement, m80.3, m52 Theme M).
	// HARD: at/over the cap the controller sets a StorageHardCapExceeded condition AND
	// enforcement blocks new corpus growth (upload → 413, ingestion → fast typed failure).
	// The controller OWNS the cross-namespace aggregation (ADR 0011): it computes the at-cap
	// state here and PROJECTS it into the namespace→tenant mirror below, so the BFF/run-worker
	// enforce off the projection WITHOUT any cross-namespace K8s read (no BFF-SA/run-worker RBAC).
	// >= (not >) — the cap is inclusive: totalCorpusBytes exactly at the cap is already "full".
	hardCapExceeded := false
	if tenant.Spec.Storage != nil && tenant.Spec.Storage.CorpusBytesHardCap != "" {
		capQ, err := resource.ParseQuantity(tenant.Spec.Storage.CorpusBytesHardCap)
		if err == nil { // parse errors are rejected at admission (CEL) — be defensive
			capBytes := capQ.Value()
			if totalCorpusBytes >= capBytes {
				hardCapExceeded = true
				meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
					Type:   "StorageHardCapExceeded",
					Status: metav1.ConditionTrue,
					Reason: "CorpusBytesReachedHardCap",
					Message: fmt.Sprintf(
						"tenant corpus is %d bytes, at or over the hard cap of %d bytes (%s); "+
							"new uploads and ingestion runs are BLOCKED until the corpus drops below the cap.",
						totalCorpusBytes, capBytes, tenant.Spec.Storage.CorpusBytesHardCap),
					ObservedGeneration: tenant.Generation,
				})
			} else {
				meta.RemoveStatusCondition(&tenant.Status.Conditions, "StorageHardCapExceeded")
			}
		}
	} else {
		meta.RemoveStatusCondition(&tenant.Status.Conditions, "StorageHardCapExceeded")
	}

	// ADR 0073: surface the network-isolation state as a condition — the secure default (nil ⇒ true) is
	// Isolated; an explicit `false` is either Grandfathered (backfilled at the upgrade, converge by
	// setting true) or a deliberate Disabled opt-out. The printcolumn/queryable condition is the migration
	// dashboard ("how many tenants remain grandfathered?", reported at each milestone close).
	isolated := tenant.Spec.NetworkIsolation == nil || *tenant.Spec.NetworkIsolation
	switch {
	case isolated:
		meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
			Type: conditionNetworkIsolated, Status: metav1.ConditionTrue, Reason: "Isolated",
			Message:            "cross-tenant traffic is denied (secure default, ADR 0073); peerTenants opens named east-west",
			ObservedGeneration: tenant.Generation,
		})
	case tenant.Annotations[networkIsolationGrandfatheredAnnotation] == "true":
		meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
			Type: conditionNetworkIsolated, Status: metav1.ConditionFalse, Reason: "Grandfathered",
			Message:            "network isolation OFF — grandfathered at the secure-default upgrade (ADR 0073); set networkIsolation:true (add peerTenants for legitimate east-west) to converge",
			ObservedGeneration: tenant.Generation,
		})
	default:
		meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
			Type: conditionNetworkIsolated, Status: metav1.ConditionFalse, Reason: "Disabled",
			Message:            "network isolation is explicitly disabled (networkIsolation:false)",
			ObservedGeneration: tenant.Generation,
		})
	}

	// Project the at-hard-cap state into the membership mirror so enforcement reads it ADR-0011-clean
	// (the BFF/run-worker read the control-plane DB they already hold, not the K8s API). Best-effort:
	// a mirror write blip is logged (not swallowed) and self-heals next reconcile, exactly like
	// syncMembershipMirror — the K8s status write below is the authoritative record either way.
	r.syncStorageState(ctx, tenant.Name, hardCapExceeded)

	return r.Status().Update(ctx, tenant)
}

// syncStorageState projects the tenant's at-hard-cap flag into the namespace→tenant mirror (ADR 0067 §6,
// m80.3) so the BFF upload handler + ingestion executor can read it WITHOUT a cross-namespace K8s read
// (ADR 0011 — the enforcement point owns no Tenant/agent-CRD RBAC; the CONTROLLER owns the aggregation
// and projects the state). No-op when the store is unconfigured (nil — envtest without a control-plane
// DB). Best-effort on the converge path: a store error is LOGGED (not swallowed, not returned) so a
// Postgres blip never rolls back the K8s status write; the next reconcile re-drives the projection.
func (r *TenantReconciler) syncStorageState(ctx context.Context, tenantName string, hardCapExceeded bool) {
	if r.NamespaceTenant == nil {
		return
	}
	if err := r.NamespaceTenant.SetStorageHardCapExceeded(ctx, tenantName, hardCapExceeded); err != nil {
		logf.FromContext(ctx).Error(err, "storage hard-cap state projection failed; will re-converge next reconcile",
			"tenant", tenantName)
	}
}

// mapObjectNSToTenants maps a namespaced object (e.g. a KnowledgeBase or Namespace) to the
// Tenant(s) whose member namespace list includes that namespace. The tenant controller must
// re-reconcile when a KnowledgeBase.status.sizeBytes changes so the soft-cap check stays current.
func mapObjectNSToTenants(ctx context.Context, mgr ctrl.Manager, objNS string) []reconcile.Request {
	var list agentsv1alpha1.TenantList
	if err := mgr.GetClient().List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if slices.Contains(list.Items[i].Spec.Namespaces, objNS) {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: client.ObjectKey{Name: list.Items[i].Name},
			})
		}
	}
	return reqs
}

// SetupWithManager wires the Tenant controller. It also watches Namespaces (so a
// member namespace created after the Tenant gets quota'd) and KnowledgeBases (so a
// sizeBytes update re-triggers the storage soft-cap check — ADR 0061 governance #7).
func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapNamespaceToTenants := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			return mapObjectNSToTenants(ctx, mgr, obj.GetName())
		})
	mapKBToTenants := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			return mapObjectNSToTenants(ctx, mgr, obj.GetNamespace())
		})
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Tenant{}).
		Watches(&corev1.Namespace{}, mapNamespaceToTenants).
		Watches(&agentsv1beta1.KnowledgeBase{}, mapKBToTenants).
		Named("tenant").
		Complete(r)
}
