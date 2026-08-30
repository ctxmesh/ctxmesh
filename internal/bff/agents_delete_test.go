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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/controlplane/publishedartifact"
)

// deleteAgent drives DELETE /api/agents/team-a/{name} with a caller token and
// returns the recorder. A bearer token is always attached so the caller-scoped
// seam is exercised.
func deleteAgent(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/agents/"+detailNS+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// getReferences drives GET /api/agents/team-a/{name}/references with a caller
// token and returns the recorder.
func getReferences(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/agents/"+detailNS+"/"+name+"/references", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// ownedToolBinding builds an MCPToolBinding whose agentRef names agent and that
// carries an ownerReference to the AgentDeployment (Kind AgentDeployment, Name
// agent). This simulates an expand-emitted child that Kubernetes will GC on
// delete.
func ownedToolBinding(name, agent, ns string) *agentsv1alpha1.MCPToolBinding {
	return &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: agentDeploymentOwnerKind, Name: agent, APIVersion: "agents.ctxmesh.ai/v1alpha1"},
			},
		},
		Spec: agentsv1alpha1.MCPToolBindingSpec{
			AgentRef:    agent,
			ToolName:    "search",
			RegistryRef: "reg",
			Mode:        "remote",
			Server:      agentsv1alpha1.ToolServer{URL: "http://x"},
		},
	}
}

// independentScalingPolicy builds an AgentScalingPolicy whose agentRef names
// agent but that carries NO ownerReference to the AgentDeployment. This
// simulates an independently-created policy that would orphan on delete.
func independentScalingPolicy(name, agent, ns string) *agentsv1alpha1.AgentScalingPolicy {
	return &agentsv1alpha1.AgentScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentsv1alpha1.AgentScalingPolicySpec{
			AgentRef: agent,
			Trigger:  "request-rate",
			Min:      0,
			Max:      5,
		},
	}
}

// --- DELETE /api/agents/{ns}/{name} ------------------------------------------

// TestDeleteAgentRemovesObject proves a DELETE succeeds (204) and the
// AgentDeployment is gone from the fake cluster afterwards.
func TestDeleteAgentRemovesObject(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteAgent(t, s, "echo")
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	// The AgentDeployment must no longer exist in the fake cluster.
	var got agentsv1alpha1.AgentDeployment
	err := c.Get(context.Background(), client.ObjectKey{Name: "echo", Namespace: detailNS}, &got)
	require.True(t, apierrors.IsNotFound(err), "AgentDeployment must be gone after a successful DELETE")
}

// deleteAgentUnpublish drives DELETE /api/agents/{ns}/{name}?unpublish=true (U4 opt-in auto-unpublish).
func deleteAgentUnpublish(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/agents/"+detailNS+"/"+name+"?unpublish=true", nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestDeleteAgent_UnpublishTrueTombstones: ?unpublish=true tombstones the agent's published template
// AFTER the delete (U4).
func TestDeleteAgent_UnpublishTrueTombstones(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	store := publishedartifact.NewMemStore()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "echo",
		SpecJSON: []byte(`{"name":"echo"}`), Visibility: "org", ContentHash: "h1",
	})
	require.NoError(t, err)
	s.publishedArtifactStore = store

	rec := deleteAgentUnpublish(t, s, "echo")
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	// The published template is now tombstoned → GetLatest reports no live release.
	_, ok, gErr := store.GetLatest(context.Background(), kindAgent, detailNS, "echo")
	require.NoError(t, gErr)
	assert.False(t, ok, "?unpublish=true must tombstone the published template")
}

// TestDeleteAgent_BareDeleteKeepsPublished: a bare DELETE (no ?unpublish) keeps the published template
// live — ADR 0068 registry semantics (U4 is opt-in).
func TestDeleteAgent_BareDeleteKeepsPublished(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	store := publishedartifact.NewMemStore()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "echo",
		SpecJSON: []byte(`{"name":"echo"}`), Visibility: "org", ContentHash: "h1",
	})
	require.NoError(t, err)
	s.publishedArtifactStore = store

	rec := deleteAgent(t, s, "echo")
	require.Equal(t, http.StatusNoContent, rec.Code)

	// The published template survives the origin's deletion (the intentional default).
	_, ok, gErr := store.GetLatest(context.Background(), kindAgent, detailNS, "echo")
	require.NoError(t, gErr)
	assert.True(t, ok, "a bare DELETE must keep the published template live (ADR 0068)")
}

// TestDeleteAgent_UnpublishNoStoreStillDeletes: ?unpublish=true with NO published store still deletes
// (best-effort — the tombstone is a no-op, never fails the delete).
func TestDeleteAgent_UnpublishNoStoreStillDeletes(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c}) // no publishedArtifactStore

	rec := deleteAgentUnpublish(t, s, "echo")
	assert.Equal(t, http.StatusNoContent, rec.Code, "the delete must succeed even with no published store")
}

// TestDeleteAgentNotFoundIs404 proves deleting a missing agent surfaces as 404.
func TestDeleteAgentNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := deleteAgent(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestDeleteAgentForbiddenIs403 proves a caller whose DELETE is Forbidden by the
// API server gets an honest 403 — the BFF never pre-empts the decision or falls
// back to its own SA (ADR 0011). The same fake-interceptor pattern as
// TestEditForbiddenApplyIs403.
func TestDeleteAgentForbiddenIs403(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "agentdeployments"},
					obj.GetName(), errors.New("viewer cannot delete"))
			},
		}).Build()
	factory := &fakeCallerClientFactory{client: c}
	s := newCallerServer(t, factory)

	rec := deleteAgent(t, s, "echo")
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "forbidden")
	// Confirm the CALLER'S token was what reached the factory.
	assert.Equal(t, "caller-token", factory.gotToken)
}

// TestDeleteAgentWithoutTokenIs401 proves a token-less DELETE is rejected 401 by
// the factory BEFORE any K8s call — the caller-scoped seam never falls back to
// the BFF SA.
func TestDeleteAgentWithoutTokenIs401(t *testing.T) {
	deleteCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				deleteCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	rec := httptest.NewRecorder()
	// No Authorization header.
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/agents/"+detailNS+"/echo", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, deleteCalled, "no K8s delete must run for a token-less request")
}

// --- GET /api/agents/{ns}/{name}/references ----------------------------------

// TestAgentReferencesClassifiesOwnedVsOrphan is the core classification test.
// It seeds:
//   - an owned MCPToolBinding (ownerRef → AgentDeployment "echo") → GC'd
//   - an independent AgentScalingPolicy (no ownerRef)              → orphan
//
// Expects: GCCount=1, OrphanCount=1, two entries with the right ownedByAgent flags.
func TestAgentReferencesClassifiesOwnedVsOrphan(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	// Owned MCPToolBinding (expand-emitted child with an ownerRef).
	owned := ownedToolBinding("echo-search", "echo", detailNS)
	// Independent AgentScalingPolicy (no ownerRef — user created separately).
	orphan := independentScalingPolicy("echo-scale", "echo", detailNS)
	// A decoy tool binding for a DIFFERENT agent — must NOT appear.
	decoy := ownedToolBinding("other-search", "other", detailNS)

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(ad, owned, orphan, decoy).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getReferences(t, s, "echo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var got AgentReferencesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	// Summary counts.
	assert.Equal(t, 1, got.GCCount, "one owned MCPToolBinding must be counted as GC'd")
	assert.Equal(t, 1, got.OrphanCount, "one independent AgentScalingPolicy must be counted as orphan")
	require.Len(t, got.References, 2, "exactly two references for echo (not the decoy)")

	// Resolve by name for order-independent assertion.
	byName := map[string]AgentReferenceEntry{}
	for _, ref := range got.References {
		byName[ref.Name] = ref
	}

	require.Contains(t, byName, "echo-search")
	assert.Equal(t, "MCPToolBinding", byName["echo-search"].Kind)
	assert.True(t, byName["echo-search"].OwnedByAgent, "echo-search carries an ownerRef → must be classified as GC'd")

	require.Contains(t, byName, "echo-scale")
	assert.Equal(t, "AgentScalingPolicy", byName["echo-scale"].Kind)
	assert.False(t, byName["echo-scale"].OwnedByAgent, "echo-scale has no ownerRef → must be classified as orphan")

	// The decoy must not appear.
	assert.NotContains(t, byName, "other-search")
}

// TestAgentReferencesEmptyWhenNone proves the endpoint returns an empty (not
// null) References slice and zero counts when no objects reference the agent.
func TestAgentReferencesEmptyWhenNone(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getReferences(t, s, "solo")
	require.Equal(t, http.StatusOK, rec.Code)

	var got AgentReferencesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.NotNil(t, got.References, "References must be [] not null when there are no references")
	assert.Empty(t, got.References)
	assert.Equal(t, 0, got.GCCount)
	assert.Equal(t, 0, got.OrphanCount)
}

// TestAgentReferencesNotFoundIs404 proves a references request for a missing
// agent surfaces as 404 (the agent existence check gates the list).
func TestAgentReferencesNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getReferences(t, s, "ghost")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestAgentReferencesWithoutTokenIs401 proves a token-less references request is
// rejected 401 before any K8s call.
func TestAgentReferencesWithoutTokenIs401(t *testing.T) {
	listCalled := false
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				listCalled = true
				return nil
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c, requireToken: true})

	rec := httptest.NewRecorder()
	// No Authorization header.
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents/"+detailNS+"/echo/references", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, listCalled, "no K8s list must run for a token-less request")
}

// TestAgentReferencesForbiddenListIs403 proves a caller whose RBAC forbids
// listing the reference resources sees an honest 403 — not a silent empty [].
func TestAgentReferencesForbiddenListIs403(t *testing.T) {
	ad := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: detailNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	// Agent Get succeeds; the reference Lists are forbidden.
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "agents.ctxmesh.ai", Resource: "mcptoolbindings"},
					"", errors.New("viewer denied"))
			},
		}).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})

	rec := getReferences(t, s, "echo")
	require.Equal(t, http.StatusForbidden, rec.Code, "a forbidden list must surface as 403, not an empty []")
	assert.Contains(t, rec.Body.String(), "forbidden")
}

// (TestAgentReferencesIncludesMemoryBinding removed — MemoryBinding retired in
// M127/ADR 0101; session memory is now the folded AgentDeployment.spec.sessionMemory
// field, deleted with the agent, not an independent delete-preview reference.)
