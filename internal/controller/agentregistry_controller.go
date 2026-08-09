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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	eventingduckv1 "knative.dev/eventing/pkg/apis/duck/v1"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

const (
	// registryIDLabel is the pod/label key that marks a pod as a member of an
	// AgentRegistry, carrying the registry's stable registryId as its value. The
	// AgentDeployment reconciler (single writer of the pod template) stamps it on
	// the revision template of every member; the generated NetworkPolicy selects
	// member pods and intra-registry sources by it. Using a controller-owned
	// label (rather than the user's arbitrary memberSelector labels) keeps the
	// network identity deterministic and independent of how the operator chose to
	// tag membership.
	registryIDLabel = "agents.ctxmesh.ai/registry-id"

	// networkPolicyNameSuffix names the per-registry NetworkPolicy
	// (<registry>-registry). One policy per AgentRegistry, owned by it so it is
	// garbage-collected when the registry is deleted (no finalizer needed).
	networkPolicyNameSuffix = "-registry"

	// namespaceNameLabel is the well-known label the kubelet/apiserver stamps on
	// every Namespace (NamespaceDefaultLabelName). We select the platform
	// namespaces (knative-serving activator, kourier-system ingress) by it in the
	// NetworkPolicy ingress rules so cold-start A2A and external /invoke are not
	// blocked (ADR 0007 consequences).
	namespaceNameLabel = "kubernetes.io/metadata.name"

	// knativeServingNamespace hosts the Knative activator, which buffers requests
	// to scaled-to-zero revisions and initiates the pod. Under a default-deny
	// ingress policy, omitting an allow rule for it wedges cold-start A2A.
	knativeServingNamespace = "knative-serving"

	// kourierSystemNamespace hosts the kourier ingress gateway, the entry point
	// for external /invoke traffic. Must be allowed alongside the activator.
	kourierSystemNamespace = "kourier-system"

	// knativeEventingNamespace hosts the broker/InMemoryChannel dispatcher that
	// delivers CloudEvents to plain-Deployment `eventing` agents. Must be allowed
	// or async event delivery is dropped by the registry NetworkPolicy (m7.8).
	knativeEventingNamespace = "knative-eventing"

	// ─── Egress allowlist peers (m11.3, spec §3 "Egress lockdown") ──────────────
	// The egress direction is default-deny + this allowlist. Each namespace below
	// is the REAL destination an agent-pod (its user container, its OTel collector
	// sidecar, and its launcher) actually talks to; anything else is denied to
	// prevent exfiltration. Namespaces are selected by the well-known
	// namespace-name label (namespaceNameLabel) so we do not depend on the
	// operator labelling those namespaces themselves — the same discipline the
	// ingress platform rule uses.

	// kubeSystemNamespace hosts kube-dns/CoreDNS. DNS egress (:53 UDP AND TCP) is
	// the FIRST allowlist entry — without it every name resolution fails and the
	// pod cannot reach ANY service by DNS (gateway, Langfuse, memory all die).
	kubeSystemNamespace = "kube-system"

	// dnsAppLabel / dnsAppLabelValue select the CoreDNS/kube-dns pods within
	// kube-system (they carry k8s-app=kube-dns). Narrowing DNS egress to the DNS
	// pods (not all of kube-system) keeps the allowlist tight while Calico still
	// resolves the peer.
	dnsAppLabel      = "k8s-app"
	dnsAppLabelValue = "kube-dns"
	dnsPort          = 53

	// langfuseNamespace hosts langfuse-web — the OTLP trace sink the agent's OTel
	// collector sidecar exports to (LANGFUSE_OTLP_ENDPOINT →
	// http://langfuse-web.langfuse.svc:3000/api/public/otel). THIS IS THE M6.4
	// LANDMINE: default-deny egress without this entry silently severs the
	// collector→Langfuse export (it did in the m6.4 review — the reason M6 shipped
	// ingress-only). It MUST stay open. The port is the langfuse-web http port.
	langfuseNamespace = "langfuse"
	langfusePort      = 3000

	// agentEngineSystemNamespace hosts the platform backends the agent pod's
	// launcher reaches on the model/memory/object-store/proxy paths:
	//   - the LiteLLM model gateway (agent-engine-gateway :4000) — MODEL_GATEWAY_URL;
	//     breaking it breaks every LLM call.
	//   - the Valkey state layer (agent-engine-statelayer :6379) —
	//     MEMORY_BACKEND_ADDR; the PROXY-LESS (pre-cutover) memory + async-dedupe path.
	//   - the MinIO object store (agent-engine-objectstore :9000) —
	//     OBJECT_STORE_ADDR; breaking it breaks m7.6b blob offload.
	//   - the state-layer proxy (agent-engine-statelayer-proxy :8080) — the m53.7
	//     cutover DEFAULT for memory + quota + async-dedup (STATELAYER_PROXY_URL); with
	//     no direct-Valkey fallback, omitting it makes a registry member's quota
	//     fail-closed (402 on every LLM call) + dedup NACK-loop + memory silently lost.
	//   - the token-service (agent-engine-token-service :8443) — long-term-memory OBO
	//     (ADR 0045); omitting it breaks per-user long-term memory for members.
	// All live in this one namespace, so a single namespace-scoped egress peer
	// (restricted to these ports) covers them.
	agentEngineSystemNamespace = "agent-engine-system"
	modelGatewayPort           = 4000
	memoryBackendPort          = 6379
	objectStorePort            = 9000
	statelayerProxyPort        = 8080
	tokenServicePort           = 8443
	// bffPort is the BFF's API port. A team SUPERVISOR's launcher calls the BFF's capability-authorized
	// spawn edge (POST /api/internal/spawn) here (M64, ADR 0057); a registry member's default-deny egress
	// must allow it or the spawn silently fails closed. Every member gets it (cheap, single port) — a
	// non-supervisor member simply never dials it.
	bffPort = 9090

	// registryDefaultMaxDepth / registryDefaultHopBudget mirror the CRD kubebuilder
	// defaults for AgentRegistry.spec.guards (maxDepth=8, hopBudget=32). They are
	// applied when spec.guards is nil (the whole struct omitted) so the injected
	// A2A_MAX_DEPTH / A2A_HOP_BUDGET always carry a sane value even for a registry
	// created without a guards block.
	registryDefaultMaxDepth  = int32(8)
	registryDefaultHopBudget = int32(32)

	// Ready-condition reasons for AgentRegistry reconciliation.
	reasonRegistryReady   = "Ready"
	reasonMultiRegistry   = "MultiRegistryConflict"
	reasonInvalidSelector = "InvalidSelector"

	// dlqNameSuffix names the per-registry dead-letter-queue sink
	// (<registry>-dlq). It is a Knative Service the registry's Broker points its
	// deadLetterSink at; a poison message that fails the delivery retries lands
	// here where an e2e can observe it (its container logs the dead CloudEvent).
	// Owned by the AgentRegistry so it GCs on delete alongside the Broker.
	dlqNameSuffix = "-dlq"

	// brokerRetry is the minimum number of delivery attempts the Broker makes
	// before moving an event to the deadLetterSink. Kept small so a poison
	// message reaches the DLQ promptly (the 🧪 observes it) without an unbounded
	// retry storm (specs/eventing-scaling.md §"DLQ": "never infinitely retried").
	brokerRetry = int32(3)

	// brokerBackoffDelay is the ISO-8601 base delay between delivery retries.
	// With backoffPolicy=exponential the effective delay is
	// backoffDelay*2^<attempt> (PT0.2S → 0.2s, 0.4s, 0.8s), so three retries
	// complete quickly enough for the DLQ 🧪 while still spacing out a flapping
	// receiver.
	brokerBackoffDelay = "PT0.2S"

	// dlqImage is the receiver image the per-registry DLQ sink runs. The Knative
	// event-display image simply logs every CloudEvent it receives — the
	// canonical, dependency-free dead-letter observation sink for an e2e (the
	// harness eventing smoke test uses the same pinned digest). It is the sink;
	// the engine records nothing else about a dead event beyond what the broker's
	// DLQ delivery carries.
	dlqImage = "gcr.io/knative-releases/knative.dev/eventing/cmd/event_display@sha256:dad3a055c4179948f8ec5d9330b0f207598065371a335f25e66d70b5a53a2281"
)

// AgentRegistryReconciler reconciles an AgentRegistry object (specs/agent-mesh.md).
//
// On each reconcile it:
//
//  1. resolves spec.memberSelector to the member AgentDeployments in the
//     registry's namespace and writes status.members (sorted) + a Ready
//     condition;
//  2. CreateOrUpdate's a single NetworkPolicy (owned by the registry) that
//     enforces registry isolation at L3/L4 (ADR 0007): default-deny ingress for
//     member pods, allow intra-registry ingress, allow the Knative activator +
//     kourier ingress (so scale-from-zero A2A and external /invoke work); PLUS
//     (m11.3) default-deny EGRESS with a backend allowlist — DNS, the
//     collector→Langfuse export, the model gateway / memory / object store, and
//     the intra-registry A2A route path — everything else denied (exfiltration
//     lockdown, spec §3), without re-severing the Langfuse export (the M6.4
//     landmine).
//
// NO FINALIZER: the NetworkPolicy is owned by the registry (owner ref) so
// Kubernetes garbage-collects it on delete; the member label + injected env are
// dropped by the AgentDeployment reconciler when membership no longer resolves
// (it watches AgentRegistry). Nothing needs converging on delete that owner-ref
// GC does not already handle, and adding a finalizer would risk wedging a
// terminating namespace (creates are rejected once a namespace is terminating).
//
// SINGLE-WRITER rule (as in M4/M5): this controller does NOT touch the pod
// template. It writes only status.members, the Ready condition, and the
// NetworkPolicy. The member label and AGENT_REGISTRY_ID / AGENT_ROLE /
// AGENT_ALLOWED_CALLERS / guard-default env are injected by the AgentDeployment
// reconciler, which watches AgentRegistry and re-renders on membership change.
type AgentRegistryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// RBAC — STANDALONE marker block. Markers inside a type's doc comment are
// silently ignored by controller-gen; they must be their own comment group
// immediately above a func for the generator to pick them up.
//
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentregistries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentregistries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=agentregistries/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=eventing.knative.dev,resources=brokers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.knative.dev,resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile resolves registry membership, writes status, and converges the
// per-registry NetworkPolicy.
func (r *AgentRegistryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var registry agentsv1alpha1.AgentRegistry
	if err := r.Get(ctx, req.NamespacedName, &registry); err != nil {
		if apierrors.IsNotFound(err) {
			// Registry deleted — its owned NetworkPolicy is GC'd by Kubernetes and
			// the AgentDeployment reconciler drops the member env on its next
			// reconcile (triggered by the registry-delete watch). Nothing to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching AgentRegistry: %w", err)
	}

	// ── Step 1: resolve membership ────────────────────────────────────────────
	members, err := r.resolveMembers(ctx, &registry)
	if err != nil {
		// An invalid selector is a user error, not a transient one: report it on
		// status and stop (a spec edit re-triggers reconcile).
		log.Info("invalid memberSelector; setting Ready=False", "registry", registry.Name, "err", err.Error())
		return ctrl.Result{}, r.setStatus(ctx, &registry, nil, metav1.ConditionFalse, reasonInvalidSelector, err.Error())
	}

	memberNames := make([]string, 0, len(members))
	for i := range members {
		memberNames = append(memberNames, members[i].Name)
	}
	slices.Sort(memberNames)

	// ── Step 2: converge the NetworkPolicy ────────────────────────────────────
	if err := r.reconcileNetworkPolicy(ctx, &registry); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling NetworkPolicy: %w", err)
	}

	// ── Step 3: converge the async-eventing plane (DLQ sink + Broker) ─────────
	// The per-registry Broker is the async A2A transport (specs/eventing-scaling.md
	// §"Broker per registry"); an eventing-model agent's Trigger (m7.5) subscribes
	// its ksvc to `<registryName>-broker`. The Broker's deadLetterSink points at
	// the per-registry DLQ ksvc created here — the DLQ must exist BEFORE the Broker
	// references it, so it is reconciled first.
	if err := r.reconcileDLQSink(ctx, &registry); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling DLQ sink: %w", err)
	}
	if err := r.reconcileBroker(ctx, &registry); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling Broker: %w", err)
	}

	// ── Step 4: status ────────────────────────────────────────────────────────
	msg := fmt.Sprintf("registry %q resolved %d member(s); NetworkPolicy + Broker converged", registry.Spec.RegistryId, len(memberNames))
	return ctrl.Result{}, r.setStatus(ctx, &registry, memberNames, metav1.ConditionTrue, reasonRegistryReady, msg)
}

// resolveMembers lists the AgentDeployments in the registry's namespace that
// match spec.memberSelector. It filters client-side after a namespace List so
// the same code path serves the cached manager client and the raw envtest
// client alike (mirrors listAgentBindings — no field index, no label selector
// pushed to the apiserver, which lets an empty selector still behave sanely).
//
// An empty memberSelector ({}) matches nothing rather than every agent: an
// all-selecting registry would be a footgun (it would network-isolate every
// agent in the namespace into one mesh). The selector must name at least one
// matchLabels entry or matchExpressions to select members.
func (r *AgentRegistryReconciler) resolveMembers(
	ctx context.Context,
	registry *agentsv1alpha1.AgentRegistry,
) ([]agentsv1alpha1.AgentDeployment, error) {
	selector, err := metav1.LabelSelectorAsSelector(&registry.Spec.MemberSelector)
	if err != nil {
		return nil, fmt.Errorf("parsing memberSelector: %w", err)
	}
	if selector.Empty() {
		// Empty selector → no members (see doc comment). Not an error.
		return nil, nil
	}

	var all agentsv1alpha1.AgentDeploymentList
	if err := r.List(ctx, &all, client.InNamespace(registry.Namespace)); err != nil {
		return nil, fmt.Errorf("listing AgentDeployments: %w", err)
	}
	out := make([]agentsv1alpha1.AgentDeployment, 0, len(all.Items))
	for i := range all.Items {
		if selector.Matches(labels.Set(all.Items[i].Labels)) {
			out = append(out, all.Items[i])
		}
	}
	return out, nil
}

// reconcileNetworkPolicy CreateOrUpdate's the single per-registry NetworkPolicy,
// owned by the registry (so it GC's on delete). The policy selects member pods
// by the controller-owned registryIDLabel and enforces (ADR 0007):
//
//   - Ingress default-deny for member pods (the policy attaches an Ingress type
//     with only the explicit allow rules below; a NetworkPolicy that selects a
//     pod and lists Ingress denies everything else to it);
//   - Ingress allow from pods carrying the same registryId label (intra-registry
//     A2A);
//   - Ingress allow from the knative-serving (activator) and kourier-system
//     (ingress) namespaces — REQUIRED or scale-from-zero A2A and external
//     /invoke break (ADR 0007 consequences).
//
// Egress (m11.3, spec §3 "Egress lockdown"): the policy now ALSO lists Egress,
// making it default-deny for EGRESS from member pods with an explicit allowlist
// (exfiltration prevention — an agent cannot reach an arbitrary internet host).
// The allowlist is the complete backend inventory the M6.4 review demanded,
// derived peer-by-peer from what the pod actually talks to (the injected
// LANGFUSE_OTLP_ENDPOINT / MODEL_GATEWAY_URL / MEMORY_BACKEND_ADDR /
// OBJECT_STORE_ADDR and the A2A Knative-route path):
//
//   - DNS (:53 UDP+TCP → kube-dns in kube-system) — FIRST, or all name
//     resolution dies and every other peer becomes unreachable by name;
//   - collector→Langfuse (langfuse-web :3000 in the langfuse namespace) — the
//     OTLP export is egress from the agent pod; THE M6.4 LANDMINE, must stay open;
//   - model gateway / memory backend / object store (agent-engine-system :4000 /
//     :6379 / :9000) — the launcher's LLM, M5 memory, and blob-offload paths;
//   - intra-registry A2A (pods carrying the same registry-id label) AND the
//     Knative data plane (activator in knative-serving, kourier ingress in
//     kourier-system): A2A resolves to a Knative route
//     (http://{target}.{ns}.svc.cluster.local), so the outbound hop egresses to
//     kourier/activator, NOT pod-to-pod — omitting them severs A2A (M6/M7).
//
// Everything else is denied. This satisfies the M11 zero-trust backlog (ADR 0007)
// WITHOUT re-severing the collector→Langfuse export that made M6 ship ingress-only.
func (r *AgentRegistryReconciler) reconcileNetworkPolicy(
	ctx context.Context,
	registry *agentsv1alpha1.AgentRegistry,
) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      registry.Name + networkPolicyNameSuffix,
			Namespace: registry.Namespace,
		},
	}

	registryID := registry.Spec.RegistryId

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Spec = networkingv1.NetworkPolicySpec{
			// Select the member pods by the controller-owned membership label.
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{registryIDLabel: registryID},
			},
			// Listing Ingress AND Egress makes this default-deny in BOTH
			// directions for member pods, allowing only the explicit rules below.
			// Ingress is the M6 cross-registry isolation; Egress is the m11.3
			// exfiltration lockdown (spec §3) — default-deny egress with the
			// backend allowlist built above, keeping the collector→Langfuse export
			// alive (the M6.4 landmine).
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Intra-registry: allow ingress from any pod carrying the same
					// registryId label (same namespace — pod selectors are namespace
					// scoped unless a namespaceSelector is added).
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{registryIDLabel: registryID},
							},
						},
					},
				},
				{
					// Platform ingress: allow the Knative activator (scale-from-zero
					// buffer), the kourier ingress gateway, and the Knative Eventing
					// broker dispatcher (which delivers CloudEvents to plain-Deployment
					// `eventing` agents from the knative-eventing namespace — omitting
					// it silently drops async event delivery, m7.8 e2e finding).
					// Selected by the well-known namespace-name label so we do not
					// depend on the operator labelling those namespaces themselves.
					From: []networkingv1.NetworkPolicyPeer{
						{NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{namespaceNameLabel: knativeServingNamespace},
						}},
						{NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{namespaceNameLabel: kourierSystemNamespace},
						}},
						{NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{namespaceNameLabel: knativeEventingNamespace},
						}},
					},
				},
			},
			// Egress default-deny + allowlist (m11.3, spec §3). Each rule pairs a
			// destination peer with the exact port(s) it serves, so a member pod
			// can reach ONLY these backends and nothing else on the network.
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// DNS (:53 UDP AND TCP) → the CoreDNS/kube-dns pods in
					// kube-system. Both protocols: TCP is used for large responses
					// / zone transfers and by some resolvers, so UDP-only would
					// intermittently break resolution. Narrowed to the DNS pods by
					// their k8s-app=kube-dns label. WITHOUT this, no name resolves
					// and every other peer below is unreachable by DNS.
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{namespaceNameLabel: kubeSystemNamespace},
							},
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{dnsAppLabel: dnsAppLabelValue},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: protoPtr(corev1.ProtocolUDP), Port: intstrPtr(dnsPort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(dnsPort)},
					},
				},
				{
					// collector → Langfuse (langfuse-web :3000). THE M6.4 LANDMINE:
					// the OTLP export is egress from the agent pod's collector
					// sidecar; default-deny without this severed it in the m6.4
					// review. MUST stay open.
					To: []networkingv1.NetworkPolicyPeer{
						{NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{namespaceNameLabel: langfuseNamespace},
						}},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(langfusePort)},
					},
				},
				{
					// Platform backends in agent-engine-system: the LiteLLM model
					// gateway (:4000), the direct Valkey state layer (:6379,
					// proxy-less path), the MinIO object store (:9000), the
					// state-layer PROXY (:8080, the m53.7 cutover default for
					// memory/quota/dedup), and the token-service (:8443, long-term
					// memory OBO). One namespace peer, scoped to these ports.
					To: []networkingv1.NetworkPolicyPeer{
						{NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{namespaceNameLabel: agentEngineSystemNamespace},
						}},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(modelGatewayPort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(memoryBackendPort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(objectStorePort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(statelayerProxyPort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(tokenServicePort)},
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intstrPtr(bffPort)},
					},
				},
				{
					// Intra-registry A2A. Two peer shapes, both required:
					//   - same-registry pods (pod-to-pod, if a target is addressed
					//     directly), selected by the registry-id label;
					//   - the Knative data plane — the activator (knative-serving,
					//     scale-from-zero buffer) and the kourier ingress
					//     (kourier-system): A2A resolves to a Knative route
					//     (http://{target}.{ns}.svc.cluster.local), so the outbound
					//     hop egresses THROUGH kourier/activator, not straight to the
					//     callee pod. Omitting them severs A2A (M6/M7) even though the
					//     intra-registry pod peer is present. No port restriction:
					//     A2A/HTTP hits arbitrary ksvc ports via the route.
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{registryIDLabel: registryID},
						}},
						{NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{namespaceNameLabel: knativeServingNamespace},
						}},
						{NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{namespaceNameLabel: kourierSystemNamespace},
						}},
					},
				},
			},
		}
		return ctrl.SetControllerReference(registry, np, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			// Namespace is being torn down — the NetworkPolicy (and everything
			// else) is going away; nothing to converge.
			return nil
		}
		return fmt.Errorf("upserting NetworkPolicy %s: %w", np.Name, err)
	}
	return nil
}

// protoPtr / intstrPtr are small constructors for the NetworkPolicy port fields,
// which take pointers. Kept local to this file (the only user) — a NetworkPolicy
// port entry needs a *Protocol and an *intstr.IntOrString.
func protoPtr(p corev1.Protocol) *corev1.Protocol { return &p }

func intstrPtr(port int) *intstr.IntOrString {
	v := intstr.FromInt32(int32(port))
	return &v
}

// reconcileDLQSink CreateOrUpdate's the per-registry dead-letter-queue sink
// (<registry>-dlq), a minimal Knative Service running the event-display receiver.
// The registry's Broker points its deadLetterSink at it, so a poison message that
// exhausts the delivery retries lands here — observable in the sink's logs
// (specs/eventing-scaling.md §"DLQ"). Owned by the registry so it GCs on delete
// alongside the Broker; a single set-once spec (the image never changes) keeps
// Knative's create-time revision defaults intact on re-reconcile.
func (r *AgentRegistryReconciler) reconcileDLQSink(
	ctx context.Context,
	registry *agentsv1alpha1.AgentRegistry,
) error {
	dlq := &servingv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      registry.Name + dlqNameSuffix,
			Namespace: registry.Namespace,
		},
	}

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, dlq, func() error {
		// Set the pod spec only on create (empty containers ⇒ fresh object). The
		// image is static, so re-applying our bare spec on update would reset
		// Knative's create-time defaults (container name, probe thresholds) to
		// zero-values the webhook rejects — the same set-once discipline the agent
		// ksvc uses. The revision name is left to Knative's default so it stays a
		// single stable revision.
		if len(dlq.Spec.Template.Spec.Containers) == 0 {
			dlq.Spec = servingv1.ServiceSpec{
				ConfigurationSpec: servingv1.ConfigurationSpec{
					Template: servingv1.RevisionTemplateSpec{
						Spec: servingv1.RevisionSpec{
							PodSpec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Image: dlqImage,
								}},
							},
						},
					},
				},
			}
		}
		return ctrl.SetControllerReference(registry, dlq, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil
		}
		return fmt.Errorf("upserting DLQ sink %s: %w", dlq.Name, err)
	}
	return nil
}

// reconcileBroker CreateOrUpdate's the per-registry Knative Eventing Broker
// (<registryName>-broker) — the async A2A transport for the registry's mesh
// (specs/eventing-scaling.md §"Broker per registry"). Its name MUST match the
// one the m7.5 AgentDeployment reconciler stamps on an eventing agent's Trigger
// (`membership.RegistryName + brokerNameSuffix`) or delivery silently breaks.
//
// spec.delivery configures at-least-once delivery with a bounded retry budget and
// a dead-letter fallback (§"DLQ"): `retry` attempts with `backoffPolicy:
// exponential`, then the event is moved to `deadLetterSink` — the per-registry
// DLQ ksvc (reconcileDLQSink), referenced by KReference so Knative resolves its
// addressable URL. Owned by the registry so it GCs on delete.
func (r *AgentRegistryReconciler) reconcileBroker(
	ctx context.Context,
	registry *agentsv1alpha1.AgentRegistry,
) error {
	retry := brokerRetry
	backoffDelay := brokerBackoffDelay
	backoffPolicy := eventingduckv1.BackoffPolicyExponential

	broker := &eventingv1.Broker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      registry.Name + brokerNameSuffix,
			Namespace: registry.Namespace,
		},
	}

	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, broker, func() error {
		broker.Spec = eventingv1.BrokerSpec{
			Delivery: &eventingduckv1.DeliverySpec{
				Retry:         &retry,
				BackoffPolicy: &backoffPolicy,
				BackoffDelay:  &backoffDelay,
				DeadLetterSink: &duckv1.Destination{
					Ref: &duckv1.KReference{
						APIVersion: knativeServiceAPIVersion,
						Kind:       knativeServiceKind,
						Name:       registry.Name + dlqNameSuffix,
						Namespace:  registry.Namespace,
					},
				},
			},
		}
		return ctrl.SetControllerReference(registry, broker, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil
		}
		return fmt.Errorf("upserting Broker %s: %w", broker.Name, err)
	}
	return nil
}

// setStatus writes status.members and the Ready condition, hitting the API only
// when something actually changed (avoids resourceVersion churn / watch storms).
func (r *AgentRegistryReconciler) setStatus(
	ctx context.Context,
	registry *agentsv1alpha1.AgentRegistry,
	members []string,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	membersChanged := !slices.Equal(registry.Status.Members, members)
	condChanged := apimeta.SetStatusCondition(&registry.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: registry.Generation,
	})
	if !membersChanged && !condChanged {
		return nil
	}
	registry.Status.Members = members

	if err := r.Status().Update(ctx, registry); err != nil {
		// Return the error (conflict included) so the reconcile REQUEUES — returning nil on
		// conflict left status stale until an unrelated event (audit FUNC-6).
		return fmt.Errorf("updating AgentRegistry status: %w", err)
	}
	return nil
}

// registryMembership is the resolved registry context for a single agent, used
// by the AgentDeployment reconciler to inject the member env + membership label
// and to roll the revision when membership changes. The zero value (IsMember
// false) means the agent belongs to no registry.
type registryMembership struct {
	IsMember bool
	// RegistryName is the AgentRegistry object's metadata.name. It (not the
	// RegistryID) names the per-registry Knative Eventing Broker
	// (`<RegistryName>-broker`) an eventing-model agent's Trigger subscribes to
	// (specs/eventing-scaling.md "Broker per registry").
	RegistryName string
	RegistryID   string
	MaxDepth     int32
	HopBudget    int32
}

// resolveAgentRegistry determines which AgentRegistry (if any) an agent belongs
// to, resolving guard defaults. It lists the namespace's registries and matches
// the agent's labels against each memberSelector client-side (same no-index
// pattern as listAgentBindings, so it serves the cached and raw envtest clients
// identically).
//
// v1 rule: an agent is in AT MOST ONE registry. If the agent's labels match more
// than one registry the first by name is chosen (deterministic); the extra
// membership is a warning surfaced on that registry's status by its own
// reconciler, not here. Terminating registries are excluded so membership drops
// immediately on registry delete (revision roll).
//
// Guard defaults follow the CRD: maxDepth=8, hopBudget=32 when spec.guards is
// nil or a field is zero. The AgentDeployment reconciler injects them as the
// static A2A_MAX_DEPTH / A2A_HOP_BUDGET env for the launcher.
func resolveAgentRegistry(
	ctx context.Context,
	c client.Client,
	agent *agentsv1alpha1.AgentDeployment,
) (registryMembership, error) {
	var all agentsv1alpha1.AgentRegistryList
	if err := c.List(ctx, &all, client.InNamespace(agent.Namespace)); err != nil {
		return registryMembership{}, fmt.Errorf("listing AgentRegistries: %w", err)
	}

	agentLabels := labels.Set(agent.Labels)
	var matches []agentsv1alpha1.AgentRegistry
	for i := range all.Items {
		reg := &all.Items[i]
		if !reg.DeletionTimestamp.IsZero() {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(&reg.Spec.MemberSelector)
		if err != nil || selector.Empty() {
			// A malformed or empty selector matches no members (consistent with
			// resolveMembers); skip it rather than failing the agent's reconcile.
			continue
		}
		if selector.Matches(agentLabels) {
			matches = append(matches, *reg)
		}
	}
	if len(matches) == 0 {
		return registryMembership{}, nil
	}

	best := &matches[0]
	for i := 1; i < len(matches); i++ {
		if matches[i].Name < best.Name {
			best = &matches[i]
		}
	}

	m := registryMembership{
		IsMember:     true,
		RegistryName: best.Name,
		RegistryID:   best.Spec.RegistryId,
		MaxDepth:     registryDefaultMaxDepth,
		HopBudget:    registryDefaultHopBudget,
	}
	if best.Spec.Guards != nil {
		if best.Spec.Guards.MaxDepth > 0 {
			m.MaxDepth = best.Spec.Guards.MaxDepth
		}
		if best.Spec.Guards.HopBudget > 0 {
			m.HopBudget = best.Spec.Guards.HopBudget
		}
	}
	return m, nil
}

// SetupWithManager registers the reconciler, owns the NetworkPolicy it
// generates, and watches AgentDeployments so membership changes (an agent that
// starts/stops carrying the selector label, or is created/deleted) re-resolve
// the registries whose selectors could match it.
//
// The AgentDeployment → AgentRegistry map enqueues EVERY registry in the agent's
// namespace: an agent's labels can change to match/unmatch any registry's
// selector, and the mapping cannot cheaply know which selectors the agent's
// (old or new) labels satisfy from the event object alone (delete events carry
// the last-known labels). Re-resolving all namespace registries is bounded
// (registries are few per namespace) and keeps status.members correct — the same
// broad-requeue tradeoff the M4/M5 binding→agent maps make.
func (r *AgentRegistryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapAgentToRegistries := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			agent, ok := obj.(*agentsv1alpha1.AgentDeployment)
			if !ok {
				return nil
			}
			var list agentsv1alpha1.AgentRegistryList
			if err := mgr.GetClient().List(ctx, &list, client.InNamespace(agent.Namespace)); err != nil {
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.AgentRegistry{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&eventingv1.Broker{}).
		Owns(&servingv1.Service{}).
		Watches(&agentsv1alpha1.AgentDeployment{}, mapAgentToRegistries).
		Named("agentregistry").
		Complete(r)
}
