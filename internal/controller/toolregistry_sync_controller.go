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
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

// ToolRegistrySyncReconciler converges the control-plane store to the ToolRegistry
// CRD set (ADR 0042 Amendment 4) — the authoritative write path that makes
// Postgres trustworthy as the read source. It is registered only when the store
// is wired (CONTROLPLANE_DSN set); reads fall back to the CRD otherwise.
type ToolRegistrySyncReconciler struct {
	client.Client
	Store toolregistry.Store
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=toolregistries,verbs=get;list;watch

// Reconcile projects one ToolRegistry into the store: upsert when it exists,
// delete when it's gone (idempotent — a missing row is already converged). The
// CRD stays the RBAC-gated source of truth; this only mirrors it authoritatively.
func (r *ToolRegistrySyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var reg agentsv1alpha1.ToolRegistry
	if err := r.Get(ctx, req.NamespacedName, &reg); err != nil {
		if apierrors.IsNotFound(err) {
			if derr := r.Store.Delete(ctx, req.Namespace, req.Name); derr != nil && !errors.Is(derr, controlplane.ErrNotFound) {
				recordSync(entityToolRegistry, syncResultError)
				return ctrl.Result{}, fmt.Errorf("deleting projected ToolRegistry %s/%s: %w", req.Namespace, req.Name, derr)
			}
			recordSync(entityToolRegistry, syncResultOK)
			return ctrl.Result{}, nil
		}
		recordSync(entityToolRegistry, syncResultError)
		return ctrl.Result{}, fmt.Errorf("fetching ToolRegistry: %w", err)
	}

	if _, err := r.Store.Upsert(ctx, toolRegistryToStore(&reg)); err != nil {
		recordSync(entityToolRegistry, syncResultError)
		return ctrl.Result{}, fmt.Errorf("upserting projected ToolRegistry %s/%s: %w", reg.Namespace, reg.Name, err)
	}
	recordSync(entityToolRegistry, syncResultOK)
	return ctrl.Result{RequeueAfter: syncHealthInterval}, nil
}

// toolRegistryToStore maps a ToolRegistry CRD to its store row — the forward
// projection (storeRegistryToCRD is the inverse). Kept byte-parallel with the
// BFF's mirrorToolRegistry (internal/bff/toolregistries.go) so the sync
// reconciler and the best-effort BFF mirror write identical rows.
func toolRegistryToStore(reg *agentsv1alpha1.ToolRegistry) toolregistry.ToolRegistry {
	tools := make([]toolregistry.ToolEntry, len(reg.Spec.Tools))
	for i := range reg.Spec.Tools {
		e := reg.Spec.Tools[i]
		var schema []byte
		if e.InputSchema != nil {
			schema = e.InputSchema.Raw
		}
		tools[i] = toolregistry.ToolEntry{
			Name: e.Name, Image: e.Image, URL: e.URL, Description: e.Description,
			InputSchema: schema, Source: e.Source, ApprovalStatus: e.ApprovalStatus,
		}
	}
	return toolregistry.ToolRegistry{
		Namespace: reg.Namespace, Name: reg.Name,
		Tools: tools, Annotations: reg.Annotations, Labels: reg.Labels,
	}
}

// pruneOrphans deletes store rows whose ToolRegistry CRD no longer exists. It
// collects the orphan set fully before deleting (offset pagination would skip
// rows if the table were mutated mid-scan), and never deletes a row that has a
// live CRD. A ToolRegistry created between the CRD List and the store scan may be
// transiently pruned as a false orphan, but the For() watch re-upserts it (and
// the 30m health-requeue backstops), so the projection self-heals — the window is
// a one-shot startup pass against a synced cache.
func (r *ToolRegistrySyncReconciler) pruneOrphans(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("toolregistry-orphan-prune")

	var list agentsv1alpha1.ToolRegistryList
	if err := r.List(ctx, &list); err != nil {
		return fmt.Errorf("listing ToolRegistries for orphan prune: %w", err)
	}
	live := make(map[string]struct{}, len(list.Items))
	for i := range list.Items {
		live[list.Items[i].Namespace+"/"+list.Items[i].Name] = struct{}{}
	}

	var orphans []struct{ ns, name string }
	token := ""
	for {
		page, err := r.Store.List(ctx, controlplane.ListOptions{PageSize: controlplane.MaxPageSize, PageToken: token})
		if err != nil {
			return fmt.Errorf("listing projected ToolRegistries for orphan prune: %w", err)
		}
		for i := range page.Items {
			row := &page.Items[i]
			if _, ok := live[row.Namespace+"/"+row.Name]; !ok {
				orphans = append(orphans, struct{ ns, name string }{row.Namespace, row.Name})
			}
		}
		if page.NextPage == "" {
			break
		}
		token = page.NextPage
	}

	for _, o := range orphans {
		if err := r.Store.Delete(ctx, o.ns, o.name); err != nil && !errors.Is(err, controlplane.ErrNotFound) {
			log.Error(err, "pruning orphaned projected ToolRegistry", "namespace", o.ns, "name", o.name)
			continue
		}
		log.Info("pruned orphaned projected ToolRegistry (no CRD)", "namespace", o.ns, "name", o.name)
	}
	return nil
}

// SetupWithManager registers the reconciler + the leader-elected orphan-prune
// startup pass. The For(ToolRegistry) watch's initial list-sync backfills every
// existing object through Reconcile.
func (r *ToolRegistrySyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.Add(&storeOrphanPruner{prune: r.pruneOrphans}); err != nil {
		return fmt.Errorf("registering toolregistry orphan pruner: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.ToolRegistry{}).
		Named("toolregistry-sync").
		Complete(r)
}
