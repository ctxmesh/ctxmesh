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

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/prompt"
)

// A fast (no-envtest) unit test of the m40.3 compose-and-denormalize branch: resolvePrompt prefers the
// denormalized annotation and only falls back to the PromptVersion CRD when it's absent.
func promptTestReconciler(t *testing.T, resolver prompt.Resolver) *AgentDeploymentReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	return &AgentDeploymentReconciler{
		Client:         fake.NewClientBuilder().WithScheme(scheme).Build(),
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

// PromptVersion is retired to Postgres (ADR 0044) — there is no CRD fallback. An agent without the stamped
// annotation surfaces a user-facing PromptPointerMissing status error (the BFF stamps it on create/edit).
func TestResolvePrompt_NoAnnotation_UserError(t *testing.T) {
	r := promptTestReconciler(t, prompt.NewFixtureResolver())
	_, err := r.resolvePrompt(context.Background(), promptAgent("ghost", nil))
	pe, ok := asPromptResolveError(err)
	require.True(t, ok, "want a user-facing promptResolveError, got %v", err)
	assert.Equal(t, "PromptPointerMissing", pe.reason)
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

// The denormalized annotation is the sole prompt source (ADR 0044): it resolves to its pointer's content
// and yields a stable, non-empty digest (so a stamped agent rolls to a deterministic ksvc revision suffix).
func TestResolvePrompt_AnnotationResolves(t *testing.T) {
	ctx := context.Background()
	src := agentsv1alpha1.GitPromptSource{Repo: "https://git/x.git", Ref: "v9", Path: "p/s.txt"}
	resolver := prompt.NewFixtureResolver().Seed(src, "same content")
	r := promptTestReconciler(t, resolver)

	raw, err := json.Marshal(prompt.ResolvedPointer{Name: "pv", Repo: src.Repo, Ref: src.Ref, Path: src.Path})
	require.NoError(t, err)
	rp, err := r.resolvePrompt(ctx, promptAgent("pv", map[string]string{prompt.ResolvedPromptAnnotation: string(raw)}))
	require.NoError(t, err)
	assert.True(t, rp.hasPrompt)
	assert.Equal(t, "same content", rp.content)
	assert.NotEmpty(t, rp.digest)
}
