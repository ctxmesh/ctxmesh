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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/toolmanifest"
	"github.com/ctxmesh/agent-engine/internal/toolpush"
)

// bindingFinalizer guards binding deletion so the agent's tool state converges
// BEFORE the object disappears: without it, the delete event reaches Reconcile
// after the object is gone and the agentRef is unrecoverable — the removed tool
// would linger in tools.json (which freshly-rolled pods cold-read) and live
// sidecars would never receive the shrink push. The deletion branch re-syncs
// the agent (CM + push, with the dying binding excluded), then releases the
// finalizer.
const bindingFinalizer = "agents.ctxmesh.ai/tools-sync"

// MCPToolBindingReconciler reconciles MCPToolBinding objects (specs/mcp-tools.md).
//
// It owns the COLD (durable) and HOT (push) sides of a binding, but NOT the pod
// template — the single-writer rule reserves the ksvc for the AgentDeployment
// reconciler. On any binding or registry event it:
//
//  1. resolves the triggering agent's FULL binding set (listed in the binding's
//     namespace and filtered by spec.agentRef client-side);
//  2. validates each binding against its ToolRegistry (membership + pin match)
//     and writes the Ready condition (Bound / UnregisteredTool / RegistryMismatch
//     / RegistryNotFound);
//  3. renders the manifest from the valid bindings and CreateOrUpdate's the
//     <agent>-tools ConfigMap (durable backing, owned by the AgentDeployment);
//  4. PUSHes the manifest to every ready pod of the agent (best-effort, non-fatal);
//  5. requeues the AgentDeployment so it re-renders the pod template (the
//     structural side of the change) — via an owner-less watch map, no annotations.
//
// Deletion is handled via bindingFinalizer: the terminating binding is excluded
// from the render set, the agent re-synced, and only then is the finalizer
// released.
type MCPToolBindingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Pusher pushes manifests to discovery sidecars. Nil → a default Pusher.
	Pusher *toolpush.Pusher
	// OBOEgress configures OBO egress-sidecar injection (ADR 0030). Disabled (default)
	// ⇒ no manifest rewrite — remote endpoints stay verbatim (no drift).
	OBOEgress OBOEgressConfig
	// Registry reads referenced ToolRegistries for binding validation (ADR 0043,
	// the RegistryReader seam). Nil ⇒ the CRD-backed default (K8s API) — the M42
	// authoritative path, and the envtest path (no DB). A production build wires
	// the Postgres reader only at the M43 read-switch, and MUST wire this AND the
	// AgentDeployment reconciler's Registry from the SAME reader instance: the two
	// reconcilers compute the pushed manifest and the pod template from the same
	// resolveAgentBindings logic, so different readers would silently drift them.
	Registry RegistryReader
	// RegistryChanges, when non-nil, feeds ToolRegistry-change events from the
	// Postgres poll source (registryPollSource, ADR 0044 §1) INSTEAD OF the CRD
	// watch — wired once the ToolRegistry CRD is retired (M45), because a deleted
	// CRD can no longer be watched. Nil ⇒ the CRD watch (pre-retirement, fully
	// reversible). Both feed the SAME mapRegistryToBindings map function, so the
	// fan-out semantics are identical; only the event source differs.
	RegistryChanges <-chan event.GenericEvent
}

// registryReader returns the configured RegistryReader. It is REQUIRED now that
// ToolRegistry is retired (ADR 0044) — there is no CRD to fall back to; main.go
// wires the Postgres reader and the envtests inject a memstore-backed one.
func (r *MCPToolBindingReconciler) registryReader() RegistryReader {
	return r.Registry
}

// pusher returns the configured pusher or a lazily-created default.
func (r *MCPToolBindingReconciler) pusher() *toolpush.Pusher {
	if r.Pusher == nil {
		r.Pusher = &toolpush.Pusher{}
	}
	return r.Pusher
}

// RBAC — STANDALONE marker block. Markers inside a type's doc comment are
// silently ignored by controller-gen; they must be their own comment group
// immediately above a func for the generator to pick them up.
//
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=mcptoolbindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=mcptoolbindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=mcptoolbindings/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile resolves the agent referenced by the triggering binding and syncs
// the whole agent's tool state (validate → status → ConfigMap → push).
//
// Lifecycle:
//   - live binding → ensure bindingFinalizer, then sync the agent.
//   - terminating binding (deletionTimestamp set) → sync the agent FIRST (the
//     dying binding is excluded from the render set by listAgentBindings, so
//     the CM shrinks and live sidecars get the shrink push), THEN release the
//     finalizer. Sync errors keep the finalizer → the delete retries, so the
//     agent can never be left with stale tool state.
//   - NotFound → the finalizer flow already converged the agent before the
//     object vanished; nothing to key on.
func (r *MCPToolBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var binding agentsv1alpha1.MCPToolBinding
	if err := r.Get(ctx, req.NamespacedName, &binding); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching MCPToolBinding: %w", err)
	}

	if !binding.DeletionTimestamp.IsZero() {
		// Terminating: converge the agent without this binding, then let go.
		if res, err := r.syncAgent(ctx, binding.Namespace, binding.Spec.AgentRef); err != nil {
			return res, err // finalizer retained → deletion retries the sync
		}
		if controllerutil.ContainsFinalizer(&binding, bindingFinalizer) {
			controllerutil.RemoveFinalizer(&binding, bindingFinalizer)
			if err := r.Update(ctx, &binding); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing binding finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&binding, bindingFinalizer) {
		controllerutil.AddFinalizer(&binding, bindingFinalizer)
		if err := r.Update(ctx, &binding); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding binding finalizer: %w", err)
		}
	}

	// Adopt the binding under its AgentDeployment so deleting the agent cascades
	// (GC) to its bindings instead of leaving them as orphans.
	if err := r.ensureOwnedByAgent(ctx, &binding); err != nil {
		return ctrl.Result{}, err
	}

	return r.syncAgent(ctx, binding.Namespace, binding.Spec.AgentRef)
}

// ensureOwnedByAgent stamps a controller ownerReference from the binding's
// AgentDeployment onto the binding, so Kubernetes garbage-collects the binding
// when the agent is deleted. Without it a bound tool set is left dangling on
// agent delete (the ADR 0017 orphan-pruning gap) AND its deterministic name
// (<agent>-<tool>) collides with a 409 when the agent is recreated.
//
// It is a self-healing no-op in the common case: already-owned bindings skip the
// write, and when the AgentDeployment does not exist yet (a binding created
// before its agent) adoption is deferred — the AgentDeployment watch in
// SetupWithManager requeues the binding once the agent appears, and this stamps
// the ref then. Only bindings whose agent is permanently gone stay orphaned
// (nothing to own them); those are cleaned up out of band.
func (r *MCPToolBindingReconciler) ensureOwnedByAgent(
	ctx context.Context,
	binding *agentsv1alpha1.MCPToolBinding,
) error {
	for i := range binding.OwnerReferences {
		o := &binding.OwnerReferences[i]
		if o.Kind == "AgentDeployment" && o.Name == binding.Spec.AgentRef {
			return nil // already owned by this agent
		}
	}

	var agent agentsv1alpha1.AgentDeployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: binding.Spec.AgentRef}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // agent not present yet — adopt on a later reconcile
		}
		return fmt.Errorf("getting AgentDeployment %s for binding ownership: %w", binding.Spec.AgentRef, err)
	}

	if err := ctrl.SetControllerReference(&agent, binding, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on binding %s: %w", binding.Name, err)
	}
	if err := r.Update(ctx, binding); err != nil {
		return fmt.Errorf("adopting binding %s under agent %s: %w", binding.Name, binding.Spec.AgentRef, err)
	}
	return nil
}

// syncAgent resolves the agent's full binding set, validates + statuses every
// binding, writes the durable ConfigMap, and pushes to ready pods.
func (r *MCPToolBindingReconciler) syncAgent(
	ctx context.Context,
	namespace, agentName string,
) (ctrl.Result, error) {
	valid, validations, err := resolveAgentBindings(ctx, r.Client, r.registryReader(), namespace, agentName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving bindings for agent %s/%s: %w", namespace, agentName, err)
	}

	// ── Status: write the Ready condition on every binding of this agent ──────
	if err := r.writeBindingStatuses(ctx, namespace, agentName, validations); err != nil {
		return ctrl.Result{}, err
	}

	// Tool-call governance (M82.3, ADR 0074 §2): apply the SAME in-pod structural policy the
	// AgentDeployment reconciler applies to the pod template, so the advertised manifest (tools.json)
	// and the injected containers stay in lockstep — a denied (or require-approval) in-pod tool is not
	// deployed AND not advertised. Resolve the agent's spec.runtime.toolPolicy; if the agent is gone or
	// has no policy the set is unchanged (permissive). This drop is non-erroring here: the authoritative
	// require-approval REJECTION is the AgentDeployment's Ready=False (no pod), so no reader of this
	// manifest exists; the binding reconciler only reflects that reality. OBO/remote bindings pass
	// through verbatim (wire-enforced at the sidecar, m82.2).
	var tp resolvedToolPolicy
	var agent agentsv1alpha1.AgentDeployment
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: agentName}, &agent); getErr == nil {
		// Merge the agent's ApprovalPolicy (M139, ADR 0111) so this manifest drops the SAME tools the
		// pod does. A dangling approvalPolicyRef holds the AgentDeployment NotReady (no pod → this
		// manifest has no reader), so reflect the inline policy only rather than erroring the binding.
		ap, aerr := resolveApprovalPolicy(ctx, r.Client, &agent)
		if aerr != nil {
			if _, ok := asApprovalPolicyResolveError(aerr); !ok {
				return ctrl.Result{}, fmt.Errorf("resolving approval policy for agent %s/%s: %w", namespace, agentName, aerr)
			}
			ap = nil
		}
		if tp, err = resolveToolPolicy(&agent, ap); err != nil {
			return ctrl.Result{}, fmt.Errorf("resolving tool policy for agent %s/%s: %w", namespace, agentName, err)
		}
	} else if !apierrors.IsNotFound(getErr) {
		return ctrl.Result{}, fmt.Errorf("getting AgentDeployment %s/%s for tool policy: %w", namespace, agentName, getErr)
	}
	valid = dropUngovernableInPodTools(valid, tp)

	// ── Manifest render (valid bindings only) ────────────────────────────────
	manifest, _ := toolmanifest.Render(valid)
	// Tool-call governance (M82, ADR 0074 §1): front-all-tools is now the ONLY manifest mode —
	// always-on, no flag. EVERY tool is fronted through the per-pod egress sidecar
	// (RewriteAllForEgress), so the SDK-facing manifest matches the pod template the deployment
	// reconciler writes (both derive from RewriteAllForEgress — a split would silently drift the
	// pushed manifest from the injected sidecar's route table). This supersedes the M78 record-only
	// and ADR 0030 OBO-only rewrites (OBO tools keep their ServerName route + OAuth injection, so
	// the front-all table is a strict superset). RewriteAllForEgress leaves a no-tool manifest
	// unchanged, so a tool-less agent is byte-for-byte unchanged.
	manifest, _ = toolmanifest.RewriteAllForEgress(manifest, valid, egressSidecarBaseURL)

	// ── Durable backing: CreateOrUpdate <agent>-tools ConfigMap ──────────────
	if err := r.syncToolsConfigMap(ctx, namespace, agentName, manifest); err != nil {
		return ctrl.Result{}, err
	}

	// ── Hot path: push to every ready pod of the agent (best-effort) ─────────
	r.pushToReadyPods(ctx, namespace, agentName, manifest)

	return ctrl.Result{}, nil
}

// writeBindingStatuses sets the Ready condition on each of the agent's bindings
// from the validation map. Conflicts are logged, not fatal (optimistic locking
// re-converges on the next event).
func (r *MCPToolBindingReconciler) writeBindingStatuses(
	ctx context.Context,
	namespace, agentName string,
	validations map[string]bindingValidation,
) error {
	bindings, err := listAgentBindings(ctx, r.Client, namespace, agentName)
	if err != nil {
		return fmt.Errorf("listing bindings for status write: %w", err)
	}

	for i := range bindings {
		b := &bindings[i]
		v, ok := validations[b.Name]
		if !ok {
			continue
		}
		status := metav1.ConditionTrue
		if !v.Valid {
			status = metav1.ConditionFalse
		}
		// Only hit the API when the condition actually changed — SetStatusCondition
		// reports it; an unconditional Status().Update per binding per reconcile
		// would churn resourceVersions (and watch events) for no reason.
		changed := apimeta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
			Type:               conditionReady,
			Status:             status,
			Reason:             v.Reason,
			Message:            v.Message,
			ObservedGeneration: b.Generation,
		})
		if b.Status.ObservedGeneration != b.Generation {
			b.Status.ObservedGeneration = b.Generation
			changed = true
		}
		if !changed {
			continue
		}
		if err := r.Status().Update(ctx, b); err != nil {
			// Return the error so the reconcile REQUEUES (audit FUNC-6) rather than leaving
			// status stale until an unrelated event (the old "will requeue" log never did).
			return fmt.Errorf("updating MCPToolBinding status %s: %w", b.Name, err)
		}
	}
	return nil
}

// syncToolsConfigMap converges the <agent>-tools ConfigMap holding tools.json.
// Ownership is always the AgentDeployment (not the binding): the CM is the
// agent's durable backing and must survive individual binding deletes.
//
// When the AgentDeployment does NOT exist the CM is DELETED, never created
// (m4.5 review r2): no agent → no pods → nothing to durably back. Creating
// here has two failure faces — in a terminating namespace the create is
// rejected with 403 NamespaceTerminating, which (combined with the
// sync-error-retains-finalizer rule) would wedge the binding finalizer and the
// namespace forever; in a live namespace it would resurrect an ownerless CM
// after the agent's GC already collected the owned one, leaking it. The
// binding-created-before-agent ordering still converges: the AgentDeployment
// watch in SetupWithManager requeues the bindings when the agent appears, and
// this function then creates the CM WITH its owner ref.
//
// A CreateOrUpdate failure caused by the namespace terminating is treated as
// converged (the whole namespace is going away; nothing to back) so the
// finalizer can release and namespace deletion completes.
func (r *MCPToolBindingReconciler) syncToolsConfigMap(
	ctx context.Context,
	namespace, agentName string,
	manifest toolmanifest.Manifest,
) error {
	var agent agentsv1alpha1.AgentDeployment
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: agentName}, &agent); getErr != nil {
		if !apierrors.IsNotFound(getErr) {
			return fmt.Errorf("getting AgentDeployment %s for CM ownership: %w", agentName, getErr)
		}
		// Agent gone → remove any leftover CM. Deletes are admitted in
		// terminating namespaces (NamespaceLifecycle only blocks creates);
		// NotFound means it is already gone — either way, converged.
		leftover := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: toolsConfigMapName(agentName), Namespace: namespace},
		}
		if delErr := r.Delete(ctx, leftover); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("deleting orphaned %s ConfigMap: %w", toolsConfigMapName(agentName), delErr)
		}
		return nil
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshalling tools.json: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: toolsConfigMapName(agentName), Namespace: namespace},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[toolsConfigMapKey] = string(data)
		return ctrl.SetControllerReference(&agent, cm, r.Scheme)
	}); err != nil {
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return nil // namespace is being deleted — converged by definition
		}
		return fmt.Errorf("upserting %s ConfigMap: %w", toolsConfigMapName(agentName), err)
	}
	return nil
}

// pushToReadyPods lists the agent's ready pods (by the Knative service label)
// and POSTs the manifest to each sidecar. Best-effort: every failure is logged
// and swallowed — the ConfigMap backing guarantees eventual consistency and a
// waking pod reads it (specs/mcp-tools.md — "Push fails"). The push is NOT
// envtest-verifiable (no kubelet schedules pods, no pod IPs) → the m4.7 e2e
// covers the live propagation path.
func (r *MCPToolBindingReconciler) pushToReadyPods(
	ctx context.Context,
	namespace, agentName string,
	manifest toolmanifest.Manifest,
) {
	log := logf.FromContext(ctx)
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{knativeServiceLabel: agentName},
	); err != nil {
		log.Error(err, "listing agent pods for manifest push; relying on ConfigMap backing", "agent", agentName)
		return
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if !podReady(pod) || pod.Status.PodIP == "" {
			continue
		}
		url := toolpush.PushURL(pod.Status.PodIP)
		if err := r.pusher().Push(ctx, url, manifest); err != nil {
			// Non-fatal: log and continue; CM backing converges regardless.
			log.Info("manifest push failed (non-fatal; ConfigMap backing converges)",
				"pod", pod.Name, "podIP", pod.Status.PodIP, "err", err.Error())
			continue
		}
		log.Info("pushed manifest to discovery sidecar", "pod", pod.Name, "version", manifest.Version)
	}
}

// podReady reports whether a pod has a True PodReady condition.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// SetupWithManager registers the reconciler and the secondary watches. No
// field index is involved anywhere in this feature: watch map functions read
// spec fields straight off the event object, and reconcile-time binding
// lookups are namespace Lists filtered client-side (listAgentBindings) so the
// same code path serves the cached manager client and the raw envtest client
// alike.
//
// Watch mapping (annotation-free requeue of the AgentDeployment): a binding
// event enqueues that binding (delete events included — the finalizer keeps
// the object readable until the agent has converged); a ToolRegistry event
// enqueues every binding that references it; an AgentDeployment event enqueues
// every binding that targets it — required by the delete-when-no-agent CM
// rule: a binding reconciled BEFORE its agent exists writes no CM, so the
// agent's creation must re-trigger the bindings for the CM to materialize
// (with its owner ref). All funnel through Reconcile, which re-syncs the whole
// agent. The pod template is separately requeued by the binding watch in the
// AgentDeployment reconciler's setup.
func (r *MCPToolBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// ToolRegistry → requeue every binding that references it (a catalog change
	// can flip bindings valid/invalid).
	// The event carries only the changed registry's namespace/name (a lightweight
	// metav1.PartialObjectMetadata from the poll source — ToolRegistry is no longer
	// a CRD object), so read them off the client.Object rather than type-asserting.
	mapRegistryToBindings := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			regNS, regName := obj.GetNamespace(), obj.GetName()
			if regName == "" {
				return nil
			}
			var list agentsv1alpha1.MCPToolBindingList
			if err := mgr.GetClient().List(ctx, &list, client.InNamespace(regNS)); err != nil {
				return nil
			}
			var reqs []reconcile.Request
			for i := range list.Items {
				if list.Items[i].Spec.RegistryRef == regName {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
					})
				}
			}
			return reqs
		},
	)

	// AgentDeployment → requeue every binding targeting it (create: the CM can
	// now be written with its owner ref; delete: the CM must be removed).
	mapAgentToBindings := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			agent, ok := obj.(*agentsv1alpha1.AgentDeployment)
			if !ok {
				return nil
			}
			var list agentsv1alpha1.MCPToolBindingList
			if err := mgr.GetClient().List(ctx, &list, client.InNamespace(agent.Namespace)); err != nil {
				return nil
			}
			var reqs []reconcile.Request
			for i := range list.Items {
				if list.Items[i].Spec.AgentRef == agent.Name {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
					})
				}
			}
			return reqs
		},
	)

	b := ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.MCPToolBinding{}).
		Watches(&agentsv1alpha1.AgentDeployment{}, mapAgentToBindings).
		Named("mcptoolbinding")

	// ToolRegistry change trigger: the leader-elected Postgres poll source
	// (registryPollSource, ADR 0044 §1) feeds ToolRegistry-change events through the
	// channel — the CRD is retired, so there is no CRD to watch. Tests that don't
	// exercise catalog changes leave RegistryChanges nil (no registry watch).
	if r.RegistryChanges != nil {
		b = b.WatchesRawSource(source.Channel(r.RegistryChanges, mapRegistryToBindings))
	}

	return b.Complete(r)
}
