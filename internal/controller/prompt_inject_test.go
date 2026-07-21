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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/prompt"
)

// A fast (no-envtest) unit test of the m40.3 compose-and-denormalize branch: resolvePrompt prefers the
// denormalized annotation and only falls back to the PromptVersion CRD when it's absent.
func promptTestReconciler(t *testing.T, resolver prompt.Resolver, objs ...runtime.Object) *AgentDeploymentReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	return &AgentDeploymentReconciler{
		Client:         fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build(),
		PromptResolver: resolver,
	}
}

func promptAgent(promptRef string, ann map[string]string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "a", Annotations: ann},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{PromptRef: promptRef},
	}
}

func TestResolvePrompt_AnnotationFirst_NoCRDNeeded(t *testing.T) {
	src := agentsv1alpha1.GitPromptSource{Repo: "https://git/x.git", Ref: "v1", Path: "p/s.txt"}
	r := promptTestReconciler(t, prompt.NewFixtureResolver().Seed(src, "hello prompt")) // NO PromptVersion in the store

	raw, err := json.Marshal(prompt.ResolvedPointer{Name: "greeter", Repo: src.Repo, Ref: src.Ref, Path: src.Path})
	require.NoError(t, err)
	deploy := promptAgent("greeter", map[string]string{prompt.ResolvedPromptAnnotation: string(raw)})

	rp, err := r.resolvePrompt(context.Background(), deploy)
	require.NoError(t, err)
	assert.True(t, rp.hasPrompt)
	assert.Equal(t, "hello prompt", rp.content)
	assert.Equal(t, promptDigest(src, rp.version), rp.digest) // digest over the annotation pointer
	assert.NotEmpty(t, rp.digest)
}

func TestResolvePrompt_FallsBackToCRD_WhenNoAnnotation(t *testing.T) {
	src := agentsv1alpha1.GitPromptSource{Repo: "https://git/y.git", Ref: "v2", Path: "p/s.txt"}
	pv := &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "legacy"},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: src},
	}
	r := promptTestReconciler(t, prompt.NewFixtureResolver().Seed(src, "legacy prompt"), pv)

	deploy := promptAgent("legacy", nil) // no annotation → fetch the CRD
	rp, err := r.resolvePrompt(context.Background(), deploy)
	require.NoError(t, err)
	assert.True(t, rp.hasPrompt)
	assert.Equal(t, "legacy prompt", rp.content)
}

func TestResolvePrompt_NoAnnotationNoCRD_UserError(t *testing.T) {
	r := promptTestReconciler(t, prompt.NewFixtureResolver())
	_, err := r.resolvePrompt(context.Background(), promptAgent("ghost", nil))
	pe, ok := asPromptResolveError(err)
	require.True(t, ok, "want a user-facing promptResolveError, got %v", err)
	assert.Equal(t, "PromptVersionNotFound", pe.reason)
}

func TestResolvePrompt_MalformedAnnotation_UserError(t *testing.T) {
	r := promptTestReconciler(t, prompt.NewFixtureResolver())
	deploy := promptAgent("greeter", map[string]string{prompt.ResolvedPromptAnnotation: "{not json"})
	_, err := r.resolvePrompt(context.Background(), deploy)
	pe, ok := asPromptResolveError(err)
	require.True(t, ok, "a corrupt stamp is surfaced on status, not masked, got %v", err)
	assert.Equal(t, "PromptPointerInvalid", pe.reason)
}

func TestResolvePrompt_NoPromptRef_NoOp(t *testing.T) {
	r := promptTestReconciler(t, prompt.NewFixtureResolver())
	rp, err := r.resolvePrompt(context.Background(), promptAgent("", nil))
	require.NoError(t, err)
	assert.False(t, rp.hasPrompt)
}

// The annotation path and the CRD path must produce the SAME digest/content for the same pointer (so a
// stamped agent rolls to the SAME ksvc revision suffix), AND the annotation must win over a divergent
// PromptVersion in the store (self-contained reconcile). Locks the m40.3 equivalence contract.
func TestResolvePrompt_AnnotationEquivalentToCRD_AndWins(t *testing.T) {
	ctx := context.Background()
	src := agentsv1alpha1.GitPromptSource{Repo: "https://git/x.git", Ref: "v9", Path: "p/s.txt"}
	resolver := prompt.NewFixtureResolver().Seed(src, "same content")

	// CRD path: no annotation, the matching PromptVersion in the store.
	crdR := promptTestReconciler(t, resolver, &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pv"},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: src},
	})
	crdRP, err := crdR.resolvePrompt(ctx, promptAgent("pv", nil))
	require.NoError(t, err)

	// Annotation path: same pointer via the annotation, but a DIVERGENT PromptVersion of the same name in
	// the store — if the annotation is (wrongly) ignored, this resolves to an unseeded pointer and errors.
	annR := promptTestReconciler(t, resolver, &agentsv1alpha1.PromptVersion{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pv"},
		Spec:       agentsv1alpha1.PromptVersionSpec{Git: agentsv1alpha1.GitPromptSource{Repo: "https://git/OTHER.git", Ref: "OTHER", Path: "OTHER"}},
	})
	raw, err := json.Marshal(prompt.ResolvedPointer{Name: "pv", Repo: src.Repo, Ref: src.Ref, Path: src.Path})
	require.NoError(t, err)
	annRP, err := annR.resolvePrompt(ctx, promptAgent("pv", map[string]string{prompt.ResolvedPromptAnnotation: string(raw)}))
	require.NoError(t, err)

	assert.Equal(t, crdRP.digest, annRP.digest, "same pointer → identical digest (same ksvc revision suffix)")
	assert.Equal(t, crdRP.content, annRP.content)
	assert.Equal(t, "same content", annRP.content, "the annotation must win over the divergent CRD")
}
