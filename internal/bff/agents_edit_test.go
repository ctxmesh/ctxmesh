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

package bff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/expand"
)

// putAgent drives PUT /api/agents/team-a/{name} with the given edited simplified
// spec (as the JSON body the SPA sends) and returns the recorder. A caller token
// is attached so the caller-scoped seam is exercised.
func putAgent(t *testing.T, s *Server, name, editedYAML string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(UpdateAgentRequest{AgentYAML: editedYAML})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/agents/"+detailNS+"/"+name, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// consoleAgent builds an AgentDeployment carrying the source-spec annotation for
// the given simplified spec — a console-managed agent (m15.2 stamp), the Mode-A
// population. The stored annotation is canonical JSON of srcYAML.
func consoleAgent(t *testing.T, name, srcYAML string, spec agentsv1alpha1.AgentDeploymentSpec) *agentsv1alpha1.AgentDeployment {
	t.Helper()
	canonical, cErr := canonicalizeSourceSpec([]byte(srcYAML))
	require.Nil(t, cErr)
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   detailNS,
			Annotations: map[string]string{expand.AnnotationSourceSpec: canonical},
		},
		Spec: spec,
	}
}

// --- Mode A: console-managed full round-trip ---------------------------------

// TestEditRoundTripAppliesAndReStamps proves an edit of a console-created agent
// re-expands the edited spec, SSA-applies the change (the new image lands), and
// updates the source-spec annotation to the NEW spec so the next edit round-trips.
func TestEditRoundTripAppliesAndReStamps(t *testing.T) {
	const original = "name: echo\nimage: img:1\n"
	const edited = "name: echo\nimage: img:2\nscaling:\n  min: 2\n  max: 4\n"

	ad := consoleAgent(t, "echo", original, agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := putAgent(t, s, "echo", edited)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp UpdateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, editModeRoundTrip, resp.Mode)

	// The changed field landed on the live object via SSA.
	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "echo", Namespace: detailNS}, &got))
	assert.Equal(t, "img:2", got.Spec.Image, "edited image must land")
	require.NotNil(t, got.Spec.Scaling)
	assert.Equal(t, int32(2), got.Spec.Scaling.Min)
	assert.Equal(t, int32(4), got.Spec.Scaling.Max)

	// The source-spec annotation is re-stamped to the NEW edited spec.
	stored, ok := got.Annotations[expand.AnnotationSourceSpec]
	require.True(t, ok, "AgentDeployment must still carry the source-spec annotation")
	var fromAnnotation, fromEdited any
	require.NoError(t, json.Unmarshal([]byte(stored), &fromAnnotation))
	editedCanonical, cErr := canonicalizeSourceSpec([]byte(edited))
	require.Nil(t, cErr)
	require.NoError(t, json.Unmarshal([]byte(editedCanonical), &fromEdited))
	assert.Equal(t, fromEdited, fromAnnotation, "source-spec must be re-stamped to the edited spec")
}

// TestEditConcurrentEditGuard proves the m71.3 optimistic-concurrency guard: a PUT
// carrying a STALE resourceVersion is refused with 409 (the live object untouched),
// while a matching resourceVersion applies. Empty (unset) is unaffected — the other
// edit tests exercise that path.
func TestEditConcurrentEditGuard(t *testing.T) {
	const original = "name: echo\nimage: img:1\n"
	const edited = "name: echo\nimage: img:2\n"

	ad := consoleAgent(t, "echo", original, agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	putRV := func(rv string) *httptest.ResponseRecorder {
		body, err := json.Marshal(UpdateAgentRequest{AgentYAML: edited, ResourceVersion: rv})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/agents/"+detailNS+"/echo", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer caller-token")
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	// A stale resourceVersion → 409; the live object is NOT clobbered.
	rec := putRV("stale-does-not-match")
	require.Equal(t, http.StatusConflict, rec.Code, "a stale resourceVersion must 409; body: %s", rec.Body.String())
	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "echo", Namespace: detailNS}, &got))
	assert.Equal(t, "img:1", got.Spec.Image, "a 409 must not clobber the live object")

	// The matching (live) resourceVersion → applies.
	rec2 := putRV(got.ResourceVersion)
	require.Equal(t, http.StatusOK, rec2.Code, "a matching resourceVersion must apply; body: %s", rec2.Body.String())
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "echo", Namespace: detailNS}, &got))
	assert.Equal(t, "img:2", got.Spec.Image, "the edit must land when the resourceVersion matches")
}

// TestEditRoundTripInlineSecretRejected proves Mode A runs the create guard: an
// edited spec carrying inline credential material is a teaching 400 and the live
// object is untouched.
func TestEditRoundTripInlineSecretRejected(t *testing.T) {
	const original = "name: echo\nimage: img:1\n"
	const leaky = "name: echo\nimage: img:1\napiKey: sk-leak\n"

	ad := consoleAgent(t, "echo", original, agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := putAgent(t, s, "echo", leaky)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "inline secrets are not allowed")

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "echo", Namespace: detailNS}, &got))
	assert.Equal(t, "img:1", got.Spec.Image, "the live object must be untouched on a rejected edit")
}

// TestEditRoundTripRenameRejected proves a PUT cannot rename/re-target the agent:
// a spec whose name differs from the URL is a 400 (the URL name is authoritative).
func TestEditRoundTripRenameRejected(t *testing.T) {
	const original = "name: echo\nimage: img:1\n"
	const renamed = "name: renamed\nimage: img:2\n"

	ad := consoleAgent(t, "echo", original, agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := putAgent(t, s, "echo", renamed)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "rename is not supported")
}

// --- Mode B: degraded safe-field patch ---------------------------------------

// TestEditDegradedPatchesSafeFieldOnly proves a degraded edit (annotation-less
// agent) patches ONLY a safe field (image) via SSA and leaves the hand-set
// non-safe env var intact (Mode B never re-expands, never drops hand-set state).
func TestEditDegradedPatchesSafeFieldOnly(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "kubectl-agent", Namespace: detailNS},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image: "img:1",
			// A hand-set env var the console doesn't model — must survive the patch.
			Env: []corev1.EnvVar{
				{Name: "CUSTOM_FLAG", Value: "keep-me"},
				{Name: "MODEL_ROUTE", Value: "old-route"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// Edit only the image + the model route (both safe).
	const edited = "name: kubectl-agent\nimage: img:2\nmodel:\n  route: new-route\n"
	rec := putAgent(t, s, "kubectl-agent", edited)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp UpdateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, editModeDegraded, resp.Mode)

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kubectl-agent", Namespace: detailNS}, &got))
	assert.Equal(t, "img:2", got.Spec.Image, "safe image edit must land")

	env := map[string]string{}
	for _, ev := range got.Spec.Env {
		env[ev.Name] = ev.Value
	}
	assert.Equal(t, "keep-me", env["CUSTOM_FLAG"], "hand-set non-safe env var must be preserved")
	assert.Equal(t, "new-route", env["MODEL_ROUTE"], "safe MODEL_ROUTE edit must land")
}

// TestEditDegradedNonSafeChangeRejected proves a degraded edit that changes a
// NON-safe field (executionModel) → a teaching 400 and the live object is
// untouched.
func TestEditDegradedNonSafeChangeRejected(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "kubectl-agent", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1", ExecutionModel: "serving"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// executionModel changed serving → eventing: a non-safe modeled change.
	const edited = "name: kubectl-agent\nimage: img:2\nexecutionModel: eventing\n"
	rec := putAgent(t, s, "kubectl-agent", edited)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "managed outside the UI")
	assert.Contains(t, rec.Body.String(), "executionModel")

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kubectl-agent", Namespace: detailNS}, &got))
	assert.Equal(t, "img:1", got.Spec.Image, "a rejected degraded edit must not partially apply")
}

// TestEditDegradedEchoedNonSafeFieldTolerated proves the rule does NOT reject a
// spec that merely echoes a non-safe field UNCHANGED — only a real differing
// change trips it. executionModel echoed at the live value is fine even while a
// safe field (image) changes.
func TestEditDegradedEchoedNonSafeFieldTolerated(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "kubectl-agent", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1", ExecutionModel: "eventing"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	// executionModel echoes the live value (eventing) while image changes.
	const edited = "name: kubectl-agent\nimage: img:2\nexecutionModel: eventing\n"
	rec := putAgent(t, s, "kubectl-agent", edited)
	require.Equal(t, http.StatusOK, rec.Code, "an echoed-unchanged non-safe field must be tolerated; body: %s", rec.Body.String())

	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "kubectl-agent", Namespace: detailNS}, &got))
	assert.Equal(t, "img:2", got.Spec.Image)
}

// --- Detail DTO flags: managedOutsideUI + drift ------------------------------

// TestDetailManagedOutsideUITrueForAnnotationLess proves the detail DTO flags
// managedOutsideUI=true for a kubectl-created (annotation-less) agent and false
// for a console-created one.
func TestDetailManagedOutsideUITrueForAnnotationLess(t *testing.T) {
	kubectlAD := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "kubectl-agent", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(kubectlAD).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "kubectl-agent")
	require.Equal(t, http.StatusOK, rec.Code)
	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.ManagedOutsideUI, "annotation-less agent is managed outside the UI")
	assert.False(t, got.Drift, "drift is meaningless for an annotation-less agent")
}

func TestDetailManagedOutsideUIFalseForConsoleAgent(t *testing.T) {
	const src = "name: console-echo\nimage: img:1\n"
	ad := consoleAgent(t, "console-echo", src, agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "console-echo")
	require.Equal(t, http.StatusOK, rec.Code)
	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.False(t, got.ManagedOutsideUI, "console-created agent is NOT managed outside the UI")
	assert.False(t, got.Drift, "a console agent matching its source-spec has no drift")
}

// TestDetailDriftTrueWhenLiveMutated proves drift=true when a console-created
// agent's live spec is mutated away from what its stored source-spec expands to
// (someone kubectl-patched the image).
func TestDetailDriftTrueWhenLiveMutated(t *testing.T) {
	const src = "name: echo\nimage: img:1\n"
	// Stored source-spec says img:1, but the live object was mutated to img:hacked.
	ad := consoleAgent(t, "echo", src, agentsv1alpha1.AgentDeploymentSpec{Image: "img:hacked"})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getDetail(t, s, "echo")
	require.Equal(t, http.StatusOK, rec.Code)
	var got AgentDetailResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.False(t, got.ManagedOutsideUI)
	assert.True(t, got.Drift, "a live object mutated away from its source-spec must flag drift")
}

// --- Caller-scoping (ADR 0011) -----------------------------------------------

// TestEditRoutesCallerTokenToClient proves the CALLER'S token — not the BFF SA —
// is what the factory is asked to scope the PUT by.
func TestEditRoutesCallerTokenToClient(t *testing.T) {
	const src = "name: echo\nimage: img:1\n"
	ad := consoleAgent(t, "echo", src, agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"})
	factory := &fakeCallerClientFactory{client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()}
	s := newCallerServer(t, factory)

	body, _ := json.Marshal(UpdateAgentRequest{AgentYAML: "name: echo\nimage: img:2\n"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/agents/"+detailNS+"/echo", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "developer-persona-token", factory.gotToken,
		"the caller's token must be what the factory scopes the edit by")
}

// TestEditForbiddenApplyIs403 proves a caller whose SSA apply is Forbidden by the
// API server (a viewer persona, passed through the caller-scoped client) gets an
// honest 403 — the BFF never pre-empts the decision or falls back to its SA.
func TestEditForbiddenApplyIs403(t *testing.T) {
	const src = "name: echo\nimage: img:1\n"
	ad := consoleAgent(t, "echo", src, agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"},
					obj.GetName(), errors.New("viewer cannot update"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := putAgent(t, s, "echo", "name: echo\nimage: img:2\n")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestEditNotFoundIs404 proves a PUT to a missing agent is a 404 (the live read
// fails before any apply).
func TestEditNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := putAgent(t, s, "ghost", "name: ghost\nimage: img:1\n")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestEditWithoutTokenIs401 proves a token-less PUT is rejected 401 by the factory
// BEFORE any K8s read/apply — the caller-scoped seam never falls back to the SA.
func TestEditWithoutTokenIs401(t *testing.T) {
	patchCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				patchCalled = true
				return cl.Patch(ctx, obj, p, opts...)
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	body, _ := json.Marshal(UpdateAgentRequest{AgentYAML: "name: echo\nimage: img:2\n"})
	rec := httptest.NewRecorder()
	// No Authorization header.
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/agents/"+detailNS+"/echo", bytes.NewReader(body)))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, patchCalled, "no K8s apply must run for a token-less request")
}

// TestEditMissingBodyIs400 proves an edit with neither agentYAML nor any field is a
// teaching 400.
func TestEditMissingBodyIs400(t *testing.T) {
	const src = "name: echo\nimage: img:1\n"
	ad := consoleAgent(t, "echo", src, agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := putAgent(t, s, "echo", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "no edit provided")
}

// TestMergeEditOntoSourceSpecPreservesTools pins the m25 S5 fix at the merge seam: a
// field-only edit (image/prompt/scaling) overlays onto the stored source spec WITHOUT
// dropping fields the edit form doesn't model — crucially `tools`.
func TestMergeEditOntoSourceSpecPreservesTools(t *testing.T) {
	stored := `{"image":"img:1","name":"echo","scaling":{"max":3,"min":1},"tools":["search","fetch"]}`
	img, prompt := "img:2", "be helpful"
	merged, mErr := mergeEditOntoSourceSpec(stored, UpdateAgentRequest{
		Image:        &img,
		SystemPrompt: &prompt,
		Scaling:      &editScalingSpec{Min: intPtr(2), Max: intPtr(5)},
	})
	require.Nil(t, mErr)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(merged), &got))
	assert.Equal(t, "img:2", got["image"], "image overlaid")
	assert.Equal(t, "be helpful", got["systemPrompt"], "prompt overlaid")
	assert.Equal(t, []any{"search", "fetch"}, got["tools"], "tools MUST survive a field-only edit")
	sc, _ := got["scaling"].(map[string]any)
	assert.EqualValues(t, 2, sc["min"])
	assert.EqualValues(t, 5, sc["max"])
}

// TestEditFieldOnlySucceeds proves the console edit form's shape (structured fields,
// no agentYAML) is now accepted end-to-end: it merges onto the source spec, re-expands,
// and applies — the "agentYAML is required" bug is closed.
func TestEditFieldOnlySucceeds(t *testing.T) {
	src := "name: echo\nruntime: managed\nimage: img:1\ntools:\n  - search\n"
	ad := consoleAgent(t, "echo", src, agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	img := "img:2"
	body, err := json.Marshal(UpdateAgentRequest{Image: &img})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/agents/"+detailNS+"/echo", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code, "a field-only edit must be accepted; body: %s", rec.Body.String())
}

func intPtr(i int) *int { return &i }
