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
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/onlinescore"
)

// configStores runs one online-config assertion against the mem twin AND (when CONTROLPLANE_TEST_DSN points
// at a throwaway pg16) the real Postgres store — the same twin+pg conformance shape as the onlinescore store
// tests. This is what satisfies the board's real-Postgres DoD: point CONTROLPLANE_TEST_DSN at a live pg16
// and the controller's write path lands in an actual online_score_config row.
func configStores(t *testing.T, fn func(t *testing.T, s onlinescore.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, onlinescore.NewMemStore()) })

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres conformance run (the twin still ran)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE online_score_config`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, onlinescore.NewPostgresStore(db)) })
}

// mkOnlineSuite creates an EvalSuite with the given optional online block (m84.3). It carries the required
// scorer/threshold so the suite is well-formed; only the online block varies across the assertions.
func mkOnlineSuite(t *testing.T, name, namespace string, online *agentsv1alpha1.OnlineScoringSpec) *agentsv1alpha1.EvalSuite {
	t.Helper()
	es := &agentsv1alpha1.EvalSuite{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1alpha1.EvalSuiteSpec{
			Dataset:   agentsv1alpha1.DatasetRef{Ref: "golden-cases"},
			Scorers:   []agentsv1alpha1.ScorerSpec{{Name: "accuracy", Type: "mock", Weight: 1}},
			Threshold: "0.8",
			Online:    online,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, es))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, es) })
	return es
}

// TestOnlineConfig_ControllerPublishesAndClears is the m84.3 tier1 test: the CONTROLLER, reconciling an
// AgentDeployment whose evalSuiteRef → an EvalSuite with an `.online` block, writes the per-(ns, agent)
// config ROW to cpDB (via the real-Postgres harness when CONTROLPLANE_TEST_DSN is set); and CLEARS the row
// when the `.online` block (or the ref) is removed — the judge-OFF fail-safe. It exercises the SAME resolve
// path Reconcile invokes (Step 1c → reconcileOnlineScoreConfig), reading the EvalSuite via the envtest API
// server and writing to the store. No agent-CRD RBAC is added — the controller already holds evalsuites RBAC.
func TestOnlineConfig_ControllerPublishesAndClears(t *testing.T) {
	const (
		name      = "online-cfg-agent"
		namespace = "default"
		suiteName = "online-cfg-suite"
	)

	configStores(t, func(t *testing.T, store onlinescore.Store) {
		ctx := context.Background()

		// EvalSuite WITH an online block (judge ON: sampleRate + a daily cap + a window).
		mkOnlineSuite(t, suiteName, namespace, &agentsv1alpha1.OnlineScoringSpec{
			SampleRate:      "0.05",
			MaxScoredPerDay: 25,
			Window:          "24h",
			MinSamples:      10,
		})

		deploy := &agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: agentsv1alpha1.AgentDeploymentSpec{
				Image:        "ghcr.io/ctxmesh/example-agent:latest",
				EvalSuiteRef: suiteName,
			},
		}
		require.NoError(t, k8sClient.Create(testCtx, deploy))
		t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))

		r := newReconciler()
		r.OnlineConfig = store

		// (1) evalSuiteRef → online block ⇒ the controller UPSERTS the enabled config row.
		r.reconcileOnlineScoreConfig(testCtx, deploy)
		got, found, err := store.GetOnlineConfig(ctx, namespace, name)
		require.NoError(t, err)
		require.True(t, found, "an online-block EvalSuite ⇒ the controller writes the config row")
		require.True(t, got.Enabled, "the row is enabled (judge ON)")
		require.InDelta(t, 0.05, got.SampleRate, 1e-9)
		require.Equal(t, 25, got.MaxScoredPerDay)
		require.Equal(t, 24*time.Hour, got.Window)
		require.Equal(t, 10, got.MinSamples)

		// (2) Remove the `.online` block from the suite ⇒ the controller CLEARS the row (judge OFF).
		var es agentsv1alpha1.EvalSuite
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKey{Namespace: namespace, Name: suiteName}, &es))
		es.Spec.Online = nil
		require.NoError(t, k8sClient.Update(testCtx, &es))

		r.reconcileOnlineScoreConfig(testCtx, deploy)
		_, found, err = store.GetOnlineConfig(ctx, namespace, name)
		require.NoError(t, err)
		require.False(t, found, "removing the .online block clears the row (judge OFF, the fail-safe)")

		// (3) Re-add the online block, then drop the evalSuiteRef entirely ⇒ still cleared.
		require.NoError(t, k8sClient.Get(testCtx, client.ObjectKey{Namespace: namespace, Name: suiteName}, &es))
		es.Spec.Online = &agentsv1alpha1.OnlineScoringSpec{SampleRate: "0.1", MaxScoredPerDay: 5}
		require.NoError(t, k8sClient.Update(testCtx, &es))
		r.reconcileOnlineScoreConfig(testCtx, deploy)
		_, found, err = store.GetOnlineConfig(ctx, namespace, name)
		require.NoError(t, err)
		require.True(t, found, "re-adding the online block re-publishes the row")

		deploy.Spec.EvalSuiteRef = "" // no gate/policy at all
		r.reconcileOnlineScoreConfig(testCtx, deploy)
		_, found, err = store.GetOnlineConfig(ctx, namespace, name)
		require.NoError(t, err)
		require.False(t, found, "no evalSuiteRef ⇒ cleared (judge OFF)")
	})
}
