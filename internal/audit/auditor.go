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

package audit

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// auditedTypes is one typed object per mutating-audited agent.ctxmesh.ai CRD.
// Listing them explicitly — rather than reflecting the whole scheme — makes the
// audited set an intentional, reviewable allow-list: adding a new agent CRD is
// a deliberate line here. This MUST stay in sync with the platform-persona RBAC
// roles in config/rbac — a CRD missing here is an audit gap (its mutations go
// unrecorded). The human-readable Kind is resolved from the scheme at Start.
func auditedTypes() []client.Object {
	return []client.Object{
		&agentsv1alpha1.AgentDeployment{},
		&agentsv1alpha1.AgentVersion{},
		&agentsv1alpha1.ModelRoute{},
		&agentsv1alpha1.SecretBinding{},
		&agentsv1alpha1.MCPToolBinding{},
		&agentsv1alpha1.AgentRegistry{},
		&agentsv1alpha1.AgentScalingPolicy{},
		&agentsv1alpha1.EvalSuite{},
		// PromptVersion + ToolRegistry retired to Postgres (ADR 0044); MemoryBinding
		// folded into AgentDeployment.spec.sessionMemory + retired (ADR 0101) — no
		// longer CRDs, so not watched here.
	}
}

// Auditor is a manager.Runnable that watches every agent CRD via the manager's
// cache and emits a structured AuditEntry to the Sink on each mutating action
// (Add/Update/Delete). It captures Delete — the reconciler alone cannot,
// without a finalizer — which is the point of using an informer here.
type Auditor struct {
	cache  cache.Cache
	scheme *runtime.Scheme
	sink   Sink
	// now is the clock; overridable in tests for deterministic timestamps.
	now func() time.Time
	// types is the audited CRD set; overridable in tests.
	types []client.Object
}

// NewAuditor builds an Auditor over the given cache, resolving Kinds via scheme
// and emitting to sink. The cache/scheme are typically the manager's shared
// cache (mgr.GetCache()) and scheme (mgr.GetScheme()).
func NewAuditor(c cache.Cache, scheme *runtime.Scheme, sink Sink) *Auditor {
	return &Auditor{
		cache:  c,
		scheme: scheme,
		sink:   sink,
		now:    time.Now,
		types:  auditedTypes(),
	}
}

// NeedLeaderElection returns false: audit must observe mutations on every
// manager replica, not only the elected leader — otherwise a standby replica's
// view (and a leader handover) would leave gaps in the trail.
func (a *Auditor) NeedLeaderElection() bool { return false }

// SetupWithManager registers the Auditor as a manager Runnable so it starts
// with (and stops with) the manager, using the manager's shared cache.
func (a *Auditor) SetupWithManager(mgr manager.Manager) error {
	return mgr.Add(a)
}

// Start registers Add/Update/Delete handlers on the cache informer for each
// audited CRD, then blocks until the context is cancelled. It returns once all
// handlers are registered (registration is synchronous); handler callbacks run
// on the informers' event loops for the manager's lifetime.
func (a *Auditor) Start(ctx context.Context) error {
	for _, obj := range a.types {
		gvk, err := apiutil.GVKForObject(obj, a.scheme)
		if err != nil {
			return fmt.Errorf("resolving GVK for %T: %w", obj, err)
		}
		kind := gvk.Kind

		informer, err := a.cache.GetInformer(ctx, obj)
		if err != nil {
			return fmt.Errorf("getting informer for %s: %w", kind, err)
		}
		if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				a.emit(VerbCreate, kind, obj)
			},
			UpdateFunc: func(oldObj, newObj any) {
				// Skip informer resyncs / no-op relists: a genuine update
				// changes the resourceVersion. This keeps periodic cache
				// resyncs out of the audit trail without dropping real edits.
				if sameResourceVersion(oldObj, newObj) {
					return
				}
				a.emit(VerbUpdate, kind, newObj)
			},
			DeleteFunc: func(obj any) {
				a.emit(VerbDelete, kind, resolveDeleted(obj))
			},
		}); err != nil {
			return fmt.Errorf("registering audit handler for %s: %w", kind, err)
		}
	}

	<-ctx.Done()
	return nil
}

// emit builds and records one audit entry for a mutating action on obj.
func (a *Auditor) emit(verb Verb, kind string, obj any) {
	meta, ok := obj.(metav1.Object)
	if !ok {
		// Not a Kubernetes object (should not happen for typed informers);
		// skip rather than record a malformed entry.
		return
	}
	a.sink.Record(AuditEntry{
		Timestamp: a.now(),
		Verb:      verb,
		Kind:      kind,
		Name:      meta.GetName(),
		Namespace: meta.GetNamespace(),
		Subject:   subjectFromObject(meta),
		// The mutated object's resourceVersion — stable per mutation, and it survives a delete (the
		// informer tombstone carries it). A persistent sink folds it into a DETERMINISTIC dedup key so
		// the same mutation observed on every manager replica (NeedLeaderElection=false) collapses to
		// one stored row (ADR 0056 §3). Empty in the log sink's view; only the Postgres sink reads it.
		ResourceVersion: meta.GetResourceVersion(),
	})
}

// sameResourceVersion reports whether old and new carry the same
// resourceVersion — the signal for an informer resync (no real change).
func sameResourceVersion(oldObj, newObj any) bool {
	oldMeta, ok1 := oldObj.(metav1.Object)
	newMeta, ok2 := newObj.(metav1.Object)
	if !ok1 || !ok2 {
		return false
	}
	return oldMeta.GetResourceVersion() == newMeta.GetResourceVersion()
}

// resolveDeleted unwraps a DeletedFinalStateUnknown tombstone, which the
// informer delivers to DeleteFunc when it missed the delete watch event and
// only knows the object was removed. The wrapped object still carries the
// name/namespace/managedFields needed for the audit entry.
func resolveDeleted(obj any) any {
	if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		return tombstone.Obj
	}
	return obj
}
