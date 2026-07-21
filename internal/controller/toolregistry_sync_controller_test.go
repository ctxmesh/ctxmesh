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
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

func toolRegCRD(ns, name string, tools ...agentsv1alpha1.ToolEntry) *agentsv1alpha1.ToolRegistry {
	reg := &agentsv1alpha1.ToolRegistry{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	reg.Spec.Tools = tools
	return reg
}

func newToolRegSync(t *testing.T, store toolregistry.Store, objs ...runtime.Object) *ToolRegistrySyncReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	return &ToolRegistrySyncReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build(),
		Store:  store,
	}
}

// Reconcile of a live CRD upserts the store row (the projection), carrying the
// tool set verbatim — this is also the backfill path (initial informer add).
func TestToolRegistrySync_Upsert(t *testing.T) {
	ctx := context.Background()
	store := toolregistry.NewMemStore()
	schema := []byte(`{"type":"object"}`)
	crd := toolRegCRD("default", "reg1", agentsv1alpha1.ToolEntry{
		Name: "t1", URL: "https://mcp.example", InputSchema: &runtime.RawExtension{Raw: schema},
	})
	r := newToolRegSync(t, store, crd)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "reg1"}})
	require.NoError(t, err)

	got, err := store.Get(ctx, "default", "reg1")
	require.NoError(t, err)
	require.Len(t, got.Tools, 1)
	assert.Equal(t, "t1", got.Tools[0].Name)
	assert.JSONEq(t, string(schema), string(got.Tools[0].InputSchema))
}

// Reconcile of a vanished CRD deletes the projected row — and is idempotent
// (a second reconcile with the row already gone is a clean no-op).
func TestToolRegistrySync_DeleteOnNotFound(t *testing.T) {
	ctx := context.Background()
	store := toolregistry.NewMemStore()
	_, err := store.Upsert(ctx, toolregistry.ToolRegistry{Namespace: "default", Name: "reg1"})
	require.NoError(t, err)

	r := newToolRegSync(t, store) // no CRD in the client → Get returns NotFound

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "reg1"}}
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	_, err = store.Get(ctx, "default", "reg1")
	assert.ErrorIs(t, err, controlplane.ErrNotFound)

	// Idempotent: reconcile again, row already gone.
	_, err = r.Reconcile(ctx, req)
	assert.NoError(t, err)
}

// pruneOrphans deletes store rows with no CRD and keeps rows that have one — the
// self-heal for a delete missed while the reconciler was down.
func TestToolRegistrySync_PruneOrphans(t *testing.T) {
	ctx := context.Background()
	store := toolregistry.NewMemStore()
	_, err := store.Upsert(ctx, toolregistry.ToolRegistry{Namespace: "default", Name: "reg-live"})
	require.NoError(t, err)
	_, err = store.Upsert(ctx, toolregistry.ToolRegistry{Namespace: "default", Name: "reg-orphan"})
	require.NoError(t, err)
	_, err = store.Upsert(ctx, toolregistry.ToolRegistry{Namespace: "other", Name: "reg-orphan2"})
	require.NoError(t, err)

	// Only reg-live has a CRD.
	r := newToolRegSync(t, store, toolRegCRD("default", "reg-live"))

	require.NoError(t, r.pruneOrphans(ctx))

	_, err = store.Get(ctx, "default", "reg-live")
	assert.NoError(t, err, "live row must survive")
	_, err = store.Get(ctx, "default", "reg-orphan")
	assert.ErrorIs(t, err, controlplane.ErrNotFound, "orphan must be pruned")
	_, err = store.Get(ctx, "other", "reg-orphan2")
	assert.ErrorIs(t, err, controlplane.ErrNotFound, "cross-namespace orphan must be pruned")
}
