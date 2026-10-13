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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

func feedbackAgentSpec(fb *agentsv1beta1.FeedbackSpec) agentsv1beta1.AgentDeploymentSpec {
	return agentsv1beta1.AgentDeploymentSpec{
		Image: "ghcr.io/ctxmesh/example-agent:latest", ExecutionModel: "serving", Port: 8080,
		Feedback: fb,
	}
}

// TestAgentDeployment_FeedbackValidatedAtAdmission proves the two invariants the BFF depends on are
// enforced by the API server on spec.feedback (ADR 0152 §3): at least one source is declared, and a
// score name appears once across every source, because the name is the source-attribution key.
func TestAgentDeployment_FeedbackValidatedAtAdmission(t *testing.T) {
	const ns = "default"
	human := func(names ...string) *agentsv1beta1.HumanSource {
		s := &agentsv1beta1.HumanSource{Scores: []agentsv1beta1.ScoreDecl{}}
		for _, n := range names {
			s.Scores = append(s.Scores, agentsv1beta1.ScoreDecl{Name: n})
		}
		return s
	}
	ext := func(channel, score string) agentsv1beta1.ExternalSource {
		return agentsv1beta1.ExternalSource{Name: channel, Score: agentsv1beta1.ScoreDecl{Name: score}}
	}

	cases := []struct {
		name     string
		spec     agentsv1beta1.FeedbackSpec
		wantErr  bool
		errMatch string
	}{
		{"no sources at all", agentsv1beta1.FeedbackSpec{}, true, "at least one source"},
		{"human with zero scores and no external", agentsv1beta1.FeedbackSpec{Human: human()}, true, "scores"},
		{"coherent human only", agentsv1beta1.FeedbackSpec{Human: human("thumbs", "accuracy")}, false, ""},
		{"coherent external only", agentsv1beta1.FeedbackSpec{
			External: []agentsv1beta1.ExternalSource{ext("csat-webhook", "csat")},
		}, false, ""},
		{"coherent human and external", agentsv1beta1.FeedbackSpec{
			Human: human("thumbs"), External: []agentsv1beta1.ExternalSource{ext("csat-webhook", "csat")},
		}, false, ""},
		{"duplicate name within human", agentsv1beta1.FeedbackSpec{Human: human("dup", "dup")}, true,
			"human score names must be unique"},
		{"duplicate name across human and external", agentsv1beta1.FeedbackSpec{
			Human: human("shared"), External: []agentsv1beta1.ExternalSource{ext("ch", "shared")},
		}, true, "unique across all sources"},
		{"duplicate name across two external channels", agentsv1beta1.FeedbackSpec{
			External: []agentsv1beta1.ExternalSource{ext("ch1", "same"), ext("ch2", "same")},
		}, true, "distinct score name"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			agent := &agentsv1beta1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("fb-cel-%d", i), Namespace: ns},
				Spec:       feedbackAgentSpec(&spec),
			}
			err := k8sClient.Create(testCtx, agent)
			if tc.wantErr {
				require.Error(t, err, "an incoherent spec.feedback must be rejected at admission")
				assert.True(t, apierrors.IsInvalid(err), "rejection must be a validation error, got %v", err)
				assert.Contains(t, err.Error(), tc.errMatch)
				return
			}
			require.NoError(t, err, "a coherent spec.feedback must be admitted")
			t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })
		})
	}
}

// TestAgentDeployment_FeedbackModeDefaultsToEnforce proves an admitted declaration that omits mode is
// stored as Enforce, so the BFF gates by default.
func TestAgentDeployment_FeedbackModeDefaultsToEnforce(t *testing.T) {
	const ns = "default"
	agent := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "fb-default-mode", Namespace: ns},
		Spec: feedbackAgentSpec(&agentsv1beta1.FeedbackSpec{
			Human: &agentsv1beta1.HumanSource{Scores: []agentsv1beta1.ScoreDecl{{Name: "thumbs"}}},
		}),
	}
	require.NoError(t, k8sClient.Create(testCtx, agent))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, agent) })

	var got agentsv1beta1.AgentDeployment
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: agent.Name, Namespace: ns}, &got))
	require.NotNil(t, got.Spec.Feedback)
	assert.Equal(t, agentsv1beta1.FeedbackEnforce, got.Spec.Feedback.Mode)
	assert.Equal(t, agentsv1beta1.ScoreNumeric, got.Spec.Feedback.Human.Scores[0].DataType)
}

// TestAgentDeployment_FeedbackValidatedInEveryServedVersion proves the feedback rules hold in every
// version the API server serves, read from the installed CRD rather than assumed: conversion is None,
// so a served version lacking a rule would admit an incoherent declaration the others reject.
func TestAgentDeployment_FeedbackValidatedInEveryServedVersion(t *testing.T) {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition",
	})
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: "agentdeployments.agents.ctxmesh.ai"}, crd))
	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	require.NoError(t, err)
	require.True(t, found, "the AgentDeployment CRD lists no versions")

	var served []string
	for _, v := range versions {
		m, ok := v.(map[string]any)
		require.True(t, ok, "spec.versions entry is not an object")
		if s, _ := m["served"].(bool); s {
			name, _ := m["name"].(string)
			served = append(served, name)
		}
	}
	require.NotEmpty(t, served, "the AgentDeployment CRD serves no version")

	agent := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "fb-cel-served", Namespace: "default"},
		Spec: feedbackAgentSpec(&agentsv1beta1.FeedbackSpec{
			Human:    &agentsv1beta1.HumanSource{Scores: []agentsv1beta1.ScoreDecl{{Name: "shared"}}},
			External: []agentsv1beta1.ExternalSource{{Name: "ch", Score: agentsv1beta1.ScoreDecl{Name: "shared"}}},
		}),
	}
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(agent)
	require.NoError(t, err)
	for _, version := range served {
		t.Run(version, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(content)}
			obj.SetAPIVersion("agents.ctxmesh.ai/" + version)
			obj.SetKind("AgentDeployment")
			err := k8sClient.Create(testCtx, obj)
			require.Error(t, err, "%s must reject a duplicate score name", version)
			assert.True(t, apierrors.IsInvalid(err), "rejection must be a validation error, got %v", err)
		})
	}
}
