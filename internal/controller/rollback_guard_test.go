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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// The automatic path judges only the spec that is serving. A stored spec the ordinary reconcile has
// not served yet is either a revert the cache shows before its freeze, or a person's replacement for
// the regressed version; acting on either reverts a spec nobody has measured.
func TestShouldAutoRollback_OnlyJudgesTheServedSpec(t *testing.T) {
	r := &AgentDeploymentReconciler{}
	spec := agentsv1beta1.AgentDeploymentSpec{
		Image:   "ghcr.io/ctxmesh/example-agent:v2-regressed",
		Rollout: &agentsv1beta1.RolloutSpec{AutoRollback: &agentsv1beta1.AutoRollbackConfig{Enabled: true}},
	}
	hash, err := specHash(spec)
	require.NoError(t, err)
	served := &agentsv1beta1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Name: "guarded"}, Spec: spec}
	served.Status.LatestVersion = "guarded-" + hash
	apimeta.SetStatusCondition(&served.Status.Conditions, metav1.Condition{
		Type: conditionRegressionDetected, Status: metav1.ConditionTrue, Reason: reasonRegressionDetected,
	})
	assert.True(t, r.shouldAutoRollback(served), "the serving version regressed")

	edited := served.DeepCopy()
	edited.Spec.Image = "ghcr.io/ctxmesh/example-agent:v3-fixed"
	assert.False(t, r.shouldAutoRollback(edited), "a stored spec not yet served is not judged")

	frozen := served.DeepCopy()
	frozen.Status.Rollback = &agentsv1beta1.RollbackStatus{FrozenUntilAck: true}
	assert.False(t, r.shouldAutoRollback(frozen), "a frozen deployment is not judged")
}
