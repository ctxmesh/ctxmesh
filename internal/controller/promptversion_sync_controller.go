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
	"github.com/ctxmesh/agent-engine/internal/controlplane/promptversion"
)

// PromptVersionSyncReconciler converges the control-plane store to the
// PromptVersion CRD set (ADR 0042 Amendment 4) — the authoritative write path for
// the Postgres read-switch. Registered only when the store is wired
// (CONTROLPLANE_DSN set). Parallel to ToolRegistrySyncReconciler.
type PromptVersionSyncReconciler struct {
	client.Client
	Store promptversion.Store
}

// +kubebuilder:rbac:groups=agents.ctxmesh.ai,resources=promptversions,verbs=get;list;watch

// Reconcile projects one PromptVersion into the store: upsert when it exists,
// delete when it's gone (idempotent).
func (r *PromptVersionSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pv agentsv1alpha1.PromptVersion
	if err := r.Get(ctx, req.NamespacedName, &pv); err != nil {
		if apierrors.IsNotFound(err) {
			if derr := r.Store.Delete(ctx, req.Namespace, req.Name); derr != nil && !errors.Is(derr, controlplane.ErrNotFound) {
				recordSync(entityPromptVersion, syncResultError)
				return ctrl.Result{}, fmt.Errorf("deleting projected PromptVersion %s/%s: %w", req.Namespace, req.Name, derr)
			}
			recordSync(entityPromptVersion, syncResultOK)
			return ctrl.Result{}, nil
		}
		recordSync(entityPromptVersion, syncResultError)
		return ctrl.Result{}, fmt.Errorf("fetching PromptVersion: %w", err)
	}

	if _, err := r.Store.Upsert(ctx, promptVersionToStore(&pv)); err != nil {
		recordSync(entityPromptVersion, syncResultError)
		return ctrl.Result{}, fmt.Errorf("upserting projected PromptVersion %s/%s: %w", pv.Namespace, pv.Name, err)
	}
	recordSync(entityPromptVersion, syncResultOK)
	return ctrl.Result{RequeueAfter: syncHealthInterval}, nil
}

// promptVersionToStore maps a PromptVersion CRD to its store row — kept
// byte-parallel with the BFF's mirrorPromptVersion.
func promptVersionToStore(pv *agentsv1alpha1.PromptVersion) promptversion.PromptVersion {
	return promptversion.PromptVersion{
		Namespace: pv.Namespace,
		Name:      pv.Name,
		Repo:      pv.Spec.Git.Repo,
		Ref:       pv.Spec.Git.Ref,
		Path:      pv.Spec.Git.Path,
		Labels:    pv.Labels,
	}
}

// pruneOrphans deletes store rows whose PromptVersion CRD no longer exists
// (collect-then-delete; never deletes a row with a live CRD). A PromptVersion
// created between the CRD List and the store scan may be transiently pruned as a
// false orphan, but the For() watch re-upserts it (and the 30m health-requeue
// backstops), so the projection self-heals.
func (r *PromptVersionSyncReconciler) pruneOrphans(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("promptversion-orphan-prune")

	var list agentsv1alpha1.PromptVersionList
	if err := r.List(ctx, &list); err != nil {
		return fmt.Errorf("listing PromptVersions for orphan prune: %w", err)
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
			return fmt.Errorf("listing projected PromptVersions for orphan prune: %w", err)
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
			log.Error(err, "pruning orphaned projected PromptVersion", "namespace", o.ns, "name", o.name)
			continue
		}
		log.Info("pruned orphaned projected PromptVersion (no CRD)", "namespace", o.ns, "name", o.name)
	}
	return nil
}

// SetupWithManager registers the reconciler + the leader-elected orphan-prune
// startup pass.
func (r *PromptVersionSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.Add(&storeOrphanPruner{prune: r.pruneOrphans}); err != nil {
		return fmt.Errorf("registering promptversion orphan pruner: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.PromptVersion{}).
		Named("promptversion-sync").
		Complete(r)
}
