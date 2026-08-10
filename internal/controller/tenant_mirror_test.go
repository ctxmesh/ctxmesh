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

	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
)

// TestTenant_MembershipMirror_ConvergesAndDeletes drives the reconcile-time membership-mirror sync
// (m73.3, ADR 0067 §6) against the in-memory store — no envtest / Postgres, so it runs under tier0.
// It exercises the exact calls Reconcile makes: syncMembershipMirror on converge (upsert + prune)
// and the store's DeleteTenant on the tenant-delete path. It also proves nil-safety (the envtest
// path with no control-plane DB must not panic).
func TestTenant_MembershipMirror_ConvergesAndDeletes(t *testing.T) {
	ctx := context.Background()
	mem := namespacetenant.NewMemStore()
	r := &TenantReconciler{NamespaceTenant: mem}

	// Members {a, b} → the mirror has a→T and b→T.
	r.syncMembershipMirror(ctx, "T", []string{"ns-a", "ns-b"})
	members, err := mem.MembersOf(ctx, "T")
	require.NoError(t, err)
	assert.Equal(t, []string{"ns-a", "ns-b"}, members)

	// Removing b from the owned set → the next sync prunes b, keeps a.
	r.syncMembershipMirror(ctx, "T", []string{"ns-a"})
	members, err = mem.MembersOf(ctx, "T")
	require.NoError(t, err)
	assert.Equal(t, []string{"ns-a"}, members)
	_, ok, err := mem.TenantOf(ctx, "ns-b")
	require.NoError(t, err)
	assert.False(t, ok, "dropped namespace must be pruned from the mirror")

	// Deleting the tenant → DeleteTenant clears all its rows (the reconcile delete path calls this).
	require.NoError(t, mem.DeleteTenant(ctx, "T"))
	members, err = mem.MembersOf(ctx, "T")
	require.NoError(t, err)
	assert.Empty(t, members)
}

// TestTenant_MembershipMirror_NilStoreIsNoOp: an unconfigured store (envtest without a control-plane
// DB) must be a silent no-op, never a panic.
func TestTenant_MembershipMirror_NilStoreIsNoOp(t *testing.T) {
	r := &TenantReconciler{} // NamespaceTenant == nil
	assert.NotPanics(t, func() {
		r.syncMembershipMirror(context.Background(), "T", []string{"ns-a"})
	})
}
