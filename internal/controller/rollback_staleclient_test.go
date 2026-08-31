//go:build integration

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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
)

// staleCacheClient wraps the direct envtest client to reproduce the ONE property a real
// controller-runtime manager client has that the envtest client does NOT: its Reader (Get)
// is CACHE-backed and lags the apiserver by up to one write, while its Writer (Update) goes
// straight to the apiserver. The refuseRollback / actuateRollback bug (m52.N7) is invisible in
// plain envtest precisely because newReconciler wires the direct client, whose Get always
// returns the current resourceVersion so the status write never 409s.
//
// It simulates the lag deterministically: the FIRST Update of a given AgentDeployment snapshots
// that object's (pre-update) resourceVersion — the "cache view" — and does the real Update
// (which advances the apiserver's resourceVersion). Every subsequent Get of that object then
// returns the current spec/status/annotations but with the FROZEN, stale resourceVersion —
// exactly what an informer cache serves one write behind. A status Update carrying that stale
// resourceVersion 409s against the apiserver (the bug); a status merge-PATCH does not carry a
// resourceVersion and applies regardless (the fix).
type staleCacheClient struct {
	client.Client
	// staleRV maps an AgentDeployment key → the resourceVersion the "cache" is pinned to after
	// its first Update. Absent ⇒ Get passes straight through (no lag yet).
	staleRV map[types.NamespacedName]string
}

func newStaleCacheClient(c client.Client) *staleCacheClient {
	return &staleCacheClient{Client: c, staleRV: map[types.NamespacedName]string{}}
}

func (s *staleCacheClient) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if err := s.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	// Pin the resourceVersion of an AgentDeployment to its cached (stale) value once a first
	// Update has frozen the cache view; leave everything else (spec/status/annotations) current.
	if dep, ok := obj.(*agentsv1alpha1.AgentDeployment); ok {
		if rv, pinned := s.staleRV[key]; pinned {
			dep.ResourceVersion = rv
		}
	}
	return nil
}

func (s *staleCacheClient) Update(
	ctx context.Context, obj client.Object, opts ...client.UpdateOption,
) error {
	// On the FIRST Update of an AgentDeployment, freeze the cache view to the resourceVersion the
	// object carried going IN (the value the "cache" still holds) before the apiserver advances
	// it. This reproduces the informer cache lagging one write behind the annotation-clear Update.
	if dep, ok := obj.(*agentsv1alpha1.AgentDeployment); ok {
		key := client.ObjectKeyFromObject(obj)
		if _, already := s.staleRV[key]; !already {
			s.staleRV[key] = dep.ResourceVersion
		}
	}
	return s.Client.Update(ctx, obj, opts...)
}

// staleCacheReconciler builds an AgentDeployment reconciler whose embedded client lags Get
// behind Update like a real manager cache, so the m52.N7 persist-after-refuse bug is actually
// exercised (unlike the direct-client newReconciler used by the other rollback tests).
func staleCacheReconciler(s onlinescore.Store) *AgentDeploymentReconciler {
	r := newReconciler()
	r.Client = newStaleCacheClient(k8sClient)
	r.OnlineScore = s
	return r
}

// TestRollback_Refused_ReasonPersists_UnderCacheLag drives a REFUSED (cooldown) rollback through
// a reconciler whose Get is cache-lagged behind Update — the production shape m52.N7 dropped the
// refusal reason under. It asserts the RolledBack=False condition WITH the RollbackCooldown
// reason IS persisted to status after the reconcile, and that the fire-once annotation-clear is
// preserved. Before the status-Patch fix (when refuseRollback used Status().Update), the stale
// resourceVersion this client serves makes that Update 409 — the reason is dropped, the
// annotation is already cleared, and this test fails (no RolledBack condition). It is the
// red→green witness for the fix.
func TestRollback_Refused_ReasonPersists_UnderCacheLag(t *testing.T) {
	const (
		name       = "rb-cachelag-refuse-agent"
		namespace  = "default"
		targetV    = "rb-cachelag-target000"
		currentImg = "ghcr.io/ctxmesh/example-agent:v2"
	)
	mkVersionSnap(t, name, targetV, namespace, "ghcr.io/ctxmesh/example-agent:v1")

	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: currentImg},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })

	// Seed a recent successful rollback so the cooldown guard REFUSES this attempt.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	now := metav1.Now()
	deploy.Status.Rollback = &agentsv1alpha1.RollbackStatus{
		RolledBackTo:   "some-earlier-version",
		LastRollbackAt: &now,
	}
	require.NoError(t, k8sClient.Status().Update(testCtx, deploy))

	annotateRollback(t, name, namespace, targetV)

	// Reconcile through the cache-lagged client: refuseRollback clears the annotation (a first
	// Update that pins the stale cache view), then must persist RolledBack=False despite the Get
	// for the status write now returning the stale resourceVersion.
	reconcileNN(t, staleCacheReconciler(onlinescore.NewMemStore()), name, namespace)

	// Read the GROUND TRUTH from the direct apiserver client (not the lagged one).
	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &got))

	assert.Equal(t, currentImg, got.Spec.Image, "a refused rollback must leave spec unchanged")
	assert.NotContains(t, got.Annotations, rollbackAnnotation,
		"the annotation must be cleared so the refusal fires once")

	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond,
		"the refusal reason MUST persist to status even under a cache-lagged Get (m52.N7): "+
			"a Status().Update carrying the stale resourceVersion would 409 and drop it")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonRollbackCooldown, cond.Reason,
		"the persisted refusal reason must name the cooldown guard")
}
