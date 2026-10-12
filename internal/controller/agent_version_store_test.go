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
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentversion"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
)

// testVersionStore is the version history every envtest reconciler shares (newReconciler,
// regressionReconciler), as the controller's two reconcilers share one store in production.
var testVersionStore = agentversion.NewMemStore()

// recordVersionOf records a version of the stored deployment named name, as the reconciler would:
// the snapshot is the spec as the API server stores it (defaulted), with the versioned fields only.
func recordVersionOf(t *testing.T, deploy *agentsv1beta1.AgentDeployment, name string, snapshot agentsv1beta1.AgentDeploymentSpec) {
	t.Helper()
	var stored agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), &stored))
	_, err := testVersionStore.Record(testCtx, agentversion.Version{
		Namespace: stored.Namespace, Agent: stored.Name, AgentUID: string(stored.UID), Name: name,
		Snapshot: defaultedSnapshot(t, stored.Namespace, snapshot),
	})
	require.NoError(t, err)
}

// defaultedSnapshot is spec as the API server would store it: a dry-run create applies the CRD's
// defaults without persisting anything.
func defaultedSnapshot(t *testing.T, namespace string, spec agentsv1beta1.AgentDeploymentSpec) agentsv1beta1.AgentDeploymentSpec {
	t.Helper()
	probe := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "snapshot-", Namespace: namespace},
		Spec:       *spec.DeepCopy(),
	}
	require.NoError(t, k8sClient.Create(testCtx, probe, client.DryRunAll))
	return versionedSpec(probe.Spec)
}

// storedVersion returns a recorded version of the stored deployment.
func storedVersion(t *testing.T, store agentversion.Store, name, namespace, version string) (agentversion.Version, bool) {
	t.Helper()
	d := getDeploy(t, name, namespace)
	v, ok, err := store.Get(testCtx, namespace, string(d.UID), version)
	require.NoError(t, err)
	return v, ok
}

// versionNames lists the stored deployment's versions, newest first.
func versionNames(t *testing.T, store agentversion.Store, name, namespace string) []string {
	t.Helper()
	d := getDeploy(t, name, namespace)
	entries, err := store.List(testCtx, namespace, string(d.UID))
	require.NoError(t, err)
	out := []string{}
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

// failingStore is a version store whose every call fails while down is set.
type failingStore struct {
	agentversion.Store
	down atomic.Bool
}

var errStoreDown = errors.New("control-plane store unavailable")

func (f *failingStore) Record(ctx context.Context, v agentversion.Version) (bool, error) {
	if f.down.Load() {
		return false, errStoreDown
	}
	return f.Store.Record(ctx, v)
}

func (f *failingStore) Get(ctx context.Context, ns, uid, name string) (agentversion.Version, bool, error) {
	if f.down.Load() {
		return agentversion.Version{}, false, errStoreDown
	}
	return f.Store.Get(ctx, ns, uid, name)
}

func (f *failingStore) Prior(ctx context.Context, ns, uid, name string) (string, bool, error) {
	if f.down.Load() {
		return "", false, errStoreDown
	}
	return f.Store.Prior(ctx, ns, uid, name)
}

func (f *failingStore) List(ctx context.Context, ns, uid string) ([]agentversion.Entry, error) {
	if f.down.Load() {
		return nil, errStoreDown
	}
	return f.Store.List(ctx, ns, uid)
}

func condition(t *testing.T, name, namespace, kind string) *metav1.Condition {
	t.Helper()
	return apimeta.FindStatusCondition(getDeploy(t, name, namespace).Status.Conditions, kind)
}

// The reconciler records the spec's version, under the agent's UID, with the versioned snapshot, and
// says so on status.
func TestVersion_RecordedOnReconcile(t *testing.T) {
	const name, namespace = "ver-recorded", "default"
	mkPlainAgent(t, name)
	reconcileNN(t, newReconciler(), name, namespace)

	d := getDeploy(t, name, namespace)
	v, ok := storedVersion(t, testVersionStore, name, namespace, d.Status.LatestVersion)
	require.True(t, ok, "the serving version is recorded")
	assert.Equal(t, versionedSpec(d.Spec), v.Snapshot)
	assert.Equal(t, agentversion.SourceController, v.Source)
	cond := condition(t, name, namespace, conditionVersionRecorded)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

// A store outage never stops a deploy: the spec still rolls, the failure is on status and in the
// metric, and the reconcile comes back soon. Once the store returns, the version is recorded.
func TestVersion_RecordFailsOpen(t *testing.T) {
	const name, namespace = "ver-failopen", "default"
	mkPlainAgent(t, name)
	store := &failingStore{Store: agentversion.NewMemStore()}
	store.down.Store(true)
	r := newReconciler()
	r.Versions = store

	before := testutil.ToFloat64(versionRecordFailures)
	res := reconcileNN(t, r, name, namespace)
	assert.Equal(t, versionStoreRetry, res.RequeueAfter, "an unrecorded version is retried soon")
	assert.Equal(t, before+1, testutil.ToFloat64(versionRecordFailures))

	d := getDeploy(t, name, namespace)
	require.NotEmpty(t, d.Status.LatestVersion, "the ordinary reconcile ran")
	assert.Equal(t, d.Status.LatestVersion, servedAgentVersion(t, name), "the spec rolled without the store")
	cond := condition(t, name, namespace, conditionVersionRecorded)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonVersionNotRecorded, cond.Reason)
	assert.Contains(t, cond.Message, d.Status.LatestVersion)

	store.down.Store(false)
	res = reconcileNN(t, r, name, namespace)
	assert.NotEqual(t, versionStoreRetry, res.RequeueAfter)
	_, ok := storedVersion(t, store, name, namespace, d.Status.LatestVersion)
	assert.True(t, ok, "the version is recorded once the store is back")
	assert.Equal(t, metav1.ConditionTrue, condition(t, name, namespace, conditionVersionRecorded).Status)
}

// A new version prunes the agent's history to the newest versionHistoryKeep, keeping the serving
// version's prior and every version the rollback history names, however old.
func TestVersion_PruneKeepsNewestAndPinned(t *testing.T) {
	const name, namespace = "ver-prune", "default"
	mkPlainAgent(t, name)
	deploy := getDeploy(t, name, namespace)
	r := newReconciler()
	reconcileNN(t, r, name, namespace)
	serving := getDeploy(t, name, namespace).Status.LatestVersion

	// Older history than the serving version is unusual but possible after an import; seed newer
	// versions instead, so the serving one and its prior are among the oldest.
	var seeded []string
	for i := range versionHistoryKeep + 5 {
		v := fmt.Sprintf("%s-seed%02d", name, i)
		recordVersionOf(t, deploy, v, agentsv1beta1.AgentDeploymentSpec{Image: "img"})
		seeded = append(seeded, v)
	}
	flapped := seeded[1]
	d := getDeploy(t, name, namespace)
	d.Status.Rollback = &agentsv1beta1.RollbackStatus{History: []agentsv1beta1.RollbackEvent{
		{ToVersion: serving, FromVersion: flapped, At: metav1.Now()},
	}}
	require.NoError(t, k8sClient.Status().Update(testCtx, d))

	upd := getDeploy(t, name, namespace)
	upd.Spec.Port = 8090
	require.NoError(t, k8sClient.Update(testCtx, upd))
	reconcileNN(t, r, name, namespace)
	current := getDeploy(t, name, namespace).Status.LatestVersion

	got := versionNames(t, testVersionStore, name, namespace)
	assert.Contains(t, got, current)
	assert.Contains(t, got, serving, "the version serving when the new one was recorded is kept")
	assert.Contains(t, got, flapped, "a version the flap guard reads is kept")
	assert.NotContains(t, got, seeded[0], "an old unpinned version is pruned")
	assert.Len(t, got, versionHistoryKeep+2, "the newest %d plus the two pinned beyond them", versionHistoryKeep)
}

// A deleted agent's history goes with it.
func TestVersion_DeletedAgentLosesHistory(t *testing.T) {
	const name, namespace = "ver-deleted", "default"
	mkPlainAgent(t, name)
	r := newReconciler()
	reconcileNN(t, r, name, namespace)
	d := getDeploy(t, name, namespace)
	require.NotEmpty(t, versionNames(t, testVersionStore, name, namespace))

	require.NoError(t, k8sClient.Delete(testCtx, d))
	reconcileNN(t, r, name, namespace)
	entries, err := testVersionStore.List(testCtx, namespace, string(d.UID))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// A version of another agent is refused as a mismatch; a version of an earlier agent of the same name
// is not found. Neither changes the spec.
func TestRollback_TargetOfAnotherAgent(t *testing.T) {
	const namespace = "default"
	other := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rb-mismatch-other", Namespace: namespace},
		Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:other"},
	}
	require.NoError(t, k8sClient.Create(testCtx, other))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, other) })
	recordVersionOf(t, other, "rb-mismatch-other-v1", agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:v1"})

	const name = "rb-mismatch-agent"
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:v2"},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	_, err := testVersionStore.Record(testCtx, agentversion.Version{
		Namespace: namespace, Agent: name, AgentUID: "an-earlier-uid", Name: name + "-old",
		Snapshot: agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:v0"},
	})
	require.NoError(t, err)

	for _, tc := range []struct{ target, reason, says string }{
		{"rb-mismatch-other-v1", reasonRollbackTargetMismatch, `a version of deployment "rb-mismatch-other"`},
		{name + "-old", reasonRollbackTargetNotFound, "an earlier"},
	} {
		annotateRollback(t, name, namespace, tc.target)
		reconcileNN(t, rollbackReconciler(onlinescore.NewMemStore()), name, namespace)

		got := getDeploy(t, name, namespace)
		assert.Equal(t, "ghcr.io/ctxmesh/example-agent:v2", got.Spec.Image, "%s: the spec is unchanged", tc.target)
		assert.NotContains(t, got.Annotations, rollbackAnnotation)
		cond := rolledBackCond(t, name, namespace)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, tc.reason, cond.Reason)
		assert.Contains(t, cond.Message, tc.says)
	}
}

// While version history cannot be read, a requested rollback is neither taken nor refused: the
// annotation stays, RolledBack is Unknown, and the ordinary reconcile still rolls the spec as
// applied, so the way out of a bad deploy does not depend on the store. When the store returns,
// the rollback acts.
func TestRollback_HumanWaitsForTheStore(t *testing.T) {
	const name, namespace = "rb-storedown-agent", "default"
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:v2"},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	store := &failingStore{Store: agentversion.NewMemStore()}
	_, err := store.Record(testCtx, agentversion.Version{
		Namespace: namespace, Agent: name, AgentUID: string(getDeploy(t, name, namespace).UID), Name: name + "-v1",
		Snapshot: agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:v1"},
	})
	require.NoError(t, err)
	r := newReconciler()
	r.Versions = store

	annotateRollback(t, name, namespace, name+"-v1")
	store.down.Store(true)
	res := reconcileNN(t, r, name, namespace)
	assert.Equal(t, versionStoreRetry, res.RequeueAfter)

	got := getDeploy(t, name, namespace)
	assert.Equal(t, name+"-v1", got.Annotations[rollbackAnnotation], "the request is kept")
	assert.Equal(t, "ghcr.io/ctxmesh/example-agent:v2", got.Spec.Image)
	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status)
	assert.Equal(t, reasonVersionStoreUnavailable, cond.Reason)
	hash, err := specHash(got.Spec)
	require.NoError(t, err)
	assert.Equal(t, name+"-"+hash, got.Status.LatestVersion, "the ordinary reconcile rolled the applied spec")

	store.down.Store(false)
	reconcileNN(t, r, name, namespace)
	got = getDeploy(t, name, namespace)
	assert.Equal(t, "ghcr.io/ctxmesh/example-agent:v1", got.Spec.Image, "the rollback acts once the store is back")
	assert.NotContains(t, got.Annotations, rollbackAnnotation)
	assert.Equal(t, metav1.ConditionTrue, rolledBackCond(t, name, namespace).Status)
}

// The automatic path does the same: no store, no automatic rollback, and the ordinary reconcile runs.
func TestAutoRollback_WaitsForTheStore(t *testing.T) {
	const name, namespace = "ar-storedown-agent", "default"
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       autoRollbackSpec("ghcr.io/ctxmesh/example-agent:v2-regressed", true),
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	store := &failingStore{Store: agentversion.NewMemStore()}
	r := newReconciler()
	r.Versions = store
	reconcileNN(t, r, name, namespace)
	servingV := latestVersionOf(t, name, namespace)
	setRegressionDetected(t, name, namespace, servingV)

	store.down.Store(true)
	res := reconcileNN(t, r, name, namespace)
	assert.Equal(t, versionStoreRetry, res.RequeueAfter)
	got := getDeploy(t, name, namespace)
	assert.Equal(t, "ghcr.io/ctxmesh/example-agent:v2-regressed", got.Spec.Image)
	assert.Nil(t, got.Status.Rollback, "nothing was rolled back, refused or frozen")
	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status)
	assert.Equal(t, reasonVersionStoreUnavailable, cond.Reason)
	assert.Equal(t, servingV, got.Status.LatestVersion, "the ordinary reconcile ran")
}
