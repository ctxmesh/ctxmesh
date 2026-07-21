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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/promptversion"
)

func promptVersionCRD(ns, name, repo, ref, path string) *agentsv1alpha1.PromptVersion {
	pv := &agentsv1alpha1.PromptVersion{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	pv.Spec.Git.Repo = repo
	pv.Spec.Git.Ref = ref
	pv.Spec.Git.Path = path
	return pv
}

func newPromptVersionSync(t *testing.T, store promptversion.Store, objs ...runtime.Object) *PromptVersionSyncReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	return &PromptVersionSyncReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build(),
		Store:  store,
	}
}

// Reconcile of a live CRD upserts the store row, carrying the git pointer.
func TestPromptVersionSync_Upsert(t *testing.T) {
	ctx := context.Background()
	store := promptversion.NewMemStore()
	crd := promptVersionCRD("default", "pv1", "github.com/acme/prompts", "v1.0.0", "greet.md")
	r := newPromptVersionSync(t, store, crd)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pv1"}})
	require.NoError(t, err)

	got, err := store.Get(ctx, "default", "pv1")
	require.NoError(t, err)
	assert.Equal(t, "github.com/acme/prompts", got.Repo)
	assert.Equal(t, "v1.0.0", got.Ref)
	assert.Equal(t, "greet.md", got.Path)
}

// Reconcile of a vanished CRD deletes the projected row (idempotent).
func TestPromptVersionSync_DeleteOnNotFound(t *testing.T) {
	ctx := context.Background()
	store := promptversion.NewMemStore()
	_, err := store.Upsert(ctx, promptversion.PromptVersion{Namespace: "default", Name: "pv1"})
	require.NoError(t, err)

	r := newPromptVersionSync(t, store)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pv1"}}
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	_, err = store.Get(ctx, "default", "pv1")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)

	_, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)
}

// pruneOrphans deletes store rows with no CRD, keeps rows that have one.
func TestPromptVersionSync_PruneOrphans(t *testing.T) {
	ctx := context.Background()
	store := promptversion.NewMemStore()
	_, err := store.Upsert(ctx, promptversion.PromptVersion{Namespace: "default", Name: "pv-live"})
	require.NoError(t, err)
	_, err = store.Upsert(ctx, promptversion.PromptVersion{Namespace: "default", Name: "pv-orphan"})
	require.NoError(t, err)

	r := newPromptVersionSync(t, store, promptVersionCRD("default", "pv-live", "r", "ref", "p"))
	require.NoError(t, r.pruneOrphans(ctx))

	_, err = store.Get(ctx, "default", "pv-live")
	assert.NoError(t, err)
	_, err = store.Get(ctx, "default", "pv-orphan")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)
}
