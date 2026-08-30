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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1beta1 "github.com/ctxmesh/agentry/api/v1beta1"
)

// reconcileFeedbackStore reconciles the named FeedbackStore against the envtest API server.
func reconcileFeedbackStore(t *testing.T, name, ns string) {
	t.Helper()
	r := &FeedbackStoreReconciler{Client: k8sClient}
	_, err := r.Reconcile(testCtx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	require.NoError(t, err, "reconcile must not error")
}

func feedbackStoreValidated(t *testing.T, name, ns string) *metav1.Condition {
	t.Helper()
	var fs agentsv1beta1.FeedbackStore
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, &fs))
	return apimeta.FindStatusCondition(fs.Status.Conditions, conditionFeedbackValidated)
}

// TestFeedbackStore_ReconcileValidates drives the CRD through the real envtest schema (ADR 0112): a
// coherent store reconciles to Validated=True; a duplicate score name across sources → Validated=False.
func TestFeedbackStore_ReconcileValidates(t *testing.T) {
	const ns = "default"

	// (a) A coherent store (human + external, unique names) → Validated=True.
	good := &agentsv1beta1.FeedbackStore{
		ObjectMeta: metav1.ObjectMeta{Name: "fs-good", Namespace: ns},
		Spec: agentsv1beta1.FeedbackStoreSpec{
			Mode:  agentsv1beta1.FeedbackEnforce,
			Human: &agentsv1beta1.HumanSource{Scores: []agentsv1beta1.ScoreDecl{{Name: "thumbs", DataType: agentsv1beta1.ScoreBoolean}}},
			External: []agentsv1beta1.ExternalSource{
				{Name: "csat-webhook", Score: agentsv1beta1.ScoreDecl{Name: "csat", DataType: agentsv1beta1.ScoreNumeric}},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, good))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, good) })
	reconcileFeedbackStore(t, "fs-good", ns)
	cond := feedbackStoreValidated(t, "fs-good", ns)
	require.NotNil(t, cond, "the reconciler must set a Validated condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a coherent store is Validated=True")

	// (b) A duplicate score name across human + external → Validated=False.
	bad := &agentsv1beta1.FeedbackStore{
		ObjectMeta: metav1.ObjectMeta{Name: "fs-dup", Namespace: ns},
		Spec: agentsv1beta1.FeedbackStoreSpec{
			Human: &agentsv1beta1.HumanSource{Scores: []agentsv1beta1.ScoreDecl{{Name: "shared"}}},
			External: []agentsv1beta1.ExternalSource{
				{Name: "ch", Score: agentsv1beta1.ScoreDecl{Name: "shared"}},
			},
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, bad))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, bad) })
	reconcileFeedbackStore(t, "fs-dup", ns)
	badCond := feedbackStoreValidated(t, "fs-dup", ns)
	require.NotNil(t, badCond)
	assert.Equal(t, metav1.ConditionFalse, badCond.Status, "a duplicate score name is Validated=False")
	assert.Equal(t, reasonFeedbackInvalid, badCond.Reason)
}
