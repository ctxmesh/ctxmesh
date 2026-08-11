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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/publishedartifact"
	"github.com/ctxmesh/agent-engine/internal/expand"
)

// newTemplatesServer builds a Server whose CRD routes run through the fake caller factory (backed by the
// given fake client) AND carries a fresh in-memory publishedartifact store, so the publish/unpublish
// handlers exercise the real store contract. Mirrors newCallerServer + the store injection.
func newTemplatesServer(t *testing.T, factory CallerClientFactory, store publishedartifact.Store) *Server {
	t.Helper()
	return NewServer(Options{
		CallerClients:          factory,
		Scheme:                 testScheme(t),
		Auth:                   AllowAll{},
		Adapters:               Adapters{Expand: NewExpandAdapter()},
		Version:                "test",
		PublishedArtifactStore: store,
		Log:                    logr.Discard(),
	})
}

// agentWithSourceSpec builds an AgentDeployment carrying the source-spec annotation (a console-authored
// agent — the only publishable kind).
//
//nolint:unparam // name/ns are fixed across these tests but kept as params for clarity + future reuse.
func agentWithSourceSpec(name, ns, spec string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: map[string]string{expand.AnnotationSourceSpec: spec},
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{Image: "img:live"},
	}
}

// postPublishTemplate drives POST /api/templates with the given JSON body.
func postPublishTemplate(t *testing.T, s *Server, body PublishTemplateRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/templates", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestPublishTemplateSnapshotsSourceSpec: a console-authored agent publishes to v1, carrying its source-spec
// snapshot + a content hash; the caller-scoped GET is the authorization.
func TestPublishTemplateSnapshotsSourceSpec(t *testing.T) {
	spec := `{"name":"assistant","model":"gpt-4"}`
	ad := agentWithSourceSpec("assistant", detailNS, spec)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	store := publishedartifact.NewMemStore()
	s := newTemplatesServer(t, &fakeCallerClientFactory{client: c}, store)

	rec := postPublishTemplate(t, s, PublishTemplateRequest{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "assistant", Visibility: "team",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp PublishTemplateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Version, "first publish is v1")
	assert.Equal(t, "team", resp.Visibility)
	assert.NotEmpty(t, resp.ContentHash, "content_hash must be computed")

	// The store holds the snapshot verbatim at v1.
	latest, ok, err := store.GetLatest(context.Background(), kindAgent, detailNS, "assistant")
	require.NoError(t, err)
	require.True(t, ok)
	assert.JSONEq(t, spec, string(latest.SpecJSON))
	assert.Equal(t, resp.ContentHash, latest.ContentHash)

	// A re-publish cuts v2.
	rec2 := postPublishTemplate(t, s, PublishTemplateRequest{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "assistant", Visibility: "org",
	})
	require.Equal(t, http.StatusCreated, rec2.Code)
	var resp2 PublishTemplateResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, 2, resp2.Version, "re-publish cuts v2")
}

// TestPublishTemplateMissingSourceSpecIs400: an agent authored outside the console (no source-spec) is not
// publishable — an honest 400, not a fabricated spec.
func TestPublishTemplateMissingSourceSpecIs400(t *testing.T) {
	ad := publishedAgent("kubectl-agent", detailNS) // no source-spec annotation
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newTemplatesServer(t, &fakeCallerClientFactory{client: c}, publishedartifact.NewMemStore())

	rec := postPublishTemplate(t, s, PublishTemplateRequest{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "kubectl-agent", Visibility: "team",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "no source-spec")
}

// TestPublishTemplateRejectsPrivateVisibility: publishing "private" is meaningless → 400.
func TestPublishTemplateRejectsPrivateVisibility(t *testing.T) {
	ad := agentWithSourceSpec("assistant", detailNS, `{"name":"assistant"}`)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newTemplatesServer(t, &fakeCallerClientFactory{client: c}, publishedartifact.NewMemStore())

	rec := postPublishTemplate(t, s, PublishTemplateRequest{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "assistant", Visibility: "private",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "private")
}

// TestPublishTemplateRejectsBadVisibility: an unknown visibility → 400.
func TestPublishTemplateRejectsBadVisibility(t *testing.T) {
	ad := agentWithSourceSpec("assistant", detailNS, `{"name":"assistant"}`)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newTemplatesServer(t, &fakeCallerClientFactory{client: c}, publishedartifact.NewMemStore())

	rec := postPublishTemplate(t, s, PublishTemplateRequest{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "assistant", Visibility: "everyone",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "visibility must be one of")
}

// TestPublishTemplateRejectsNonAgentKind: v1 only publishes agents → 400 for any other kind.
func TestPublishTemplateRejectsNonAgentKind(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newTemplatesServer(t, &fakeCallerClientFactory{client: c}, publishedartifact.NewMemStore())

	rec := postPublishTemplate(t, s, PublishTemplateRequest{
		Kind: "team", OriginNamespace: detailNS, OriginName: "squad", Visibility: "team",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "kind must be")
}

// TestPublishTemplateNotFoundIs404: publishing a missing agent → 404 (the caller-scoped GET fails).
func TestPublishTemplateNotFoundIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newTemplatesServer(t, &fakeCallerClientFactory{client: c}, publishedartifact.NewMemStore())

	rec := postPublishTemplate(t, s, PublishTemplateRequest{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "ghost", Visibility: "team",
	})
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestPublishTemplateNilStoreIs501: a BFF without the store serves 501, never a panic.
func TestPublishTemplateNilStoreIs501(t *testing.T) {
	ad := agentWithSourceSpec("assistant", detailNS, `{"name":"assistant"}`)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newTemplatesServer(t, &fakeCallerClientFactory{client: c}, nil)

	rec := postPublishTemplate(t, s, PublishTemplateRequest{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "assistant", Visibility: "team",
	})
	require.Equal(t, http.StatusNotImplemented, rec.Code)
}

// TestPublishTemplateWithoutTokenIs401: no bearer token → 401 before any K8s call.
func TestPublishTemplateWithoutTokenIs401(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newTemplatesServer(t, &fakeCallerClientFactory{client: c, requireToken: true}, publishedartifact.NewMemStore())

	raw, _ := json.Marshal(PublishTemplateRequest{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "assistant", Visibility: "team",
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/templates", bytes.NewReader(raw)))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// --- Unpublish (DELETE /api/templates/{kind}/{ns}/{name}) ---------------------

// deleteTemplate drives DELETE /api/templates/{kind}/{ns}/{name}.
//
//nolint:unparam // kind is always the agent kind in v1 but kept as a param for the future team/eval kinds.
func deleteTemplate(t *testing.T, s *Server, kind, ns, name string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/templates/"+kind+"/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestUnpublishTemplateTombstones: unpublish tombstones the artifact so GetLatest reports not-found.
func TestUnpublishTemplateTombstones(t *testing.T) {
	ad := agentWithSourceSpec("assistant", detailNS, `{"name":"assistant"}`)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	store := publishedartifact.NewMemStore()
	s := newTemplatesServer(t, &fakeCallerClientFactory{client: c}, store)

	// Publish first, then unpublish.
	require.Equal(t, http.StatusCreated, postPublishTemplate(t, s, PublishTemplateRequest{
		Kind: kindAgent, OriginNamespace: detailNS, OriginName: "assistant", Visibility: "team",
	}).Code)

	rec := deleteTemplate(t, s, kindAgent, detailNS, "assistant")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	_, ok, err := store.GetLatest(context.Background(), kindAgent, detailNS, "assistant")
	require.NoError(t, err)
	assert.False(t, ok, "a tombstoned artifact must be hidden from GetLatest")
}

// TestUnpublishTemplateIdempotent: unpublishing an artifact that was never published (but whose agent
// exists) is a 200 no-op, not a 500.
func TestUnpublishTemplateIdempotent(t *testing.T) {
	ad := agentWithSourceSpec("assistant", detailNS, `{"name":"assistant"}`)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(ad).Build()
	s := newTemplatesServer(t, &fakeCallerClientFactory{client: c}, publishedartifact.NewMemStore())

	rec := deleteTemplate(t, s, kindAgent, detailNS, "assistant")
	require.Equal(t, http.StatusOK, rec.Code, "unpublishing an unpublished artifact must be a 200 no-op")

	// A second DELETE is still a no-op success.
	rec2 := deleteTemplate(t, s, kindAgent, detailNS, "assistant")
	require.Equal(t, http.StatusOK, rec2.Code)
}

// TestUnpublishTemplateAuthorizesViaAgentGet: a stranger who cannot read the origin agent cannot tombstone
// its template — the caller-scoped GET 404s (fake client: missing agent → not found).
func TestUnpublishTemplateAuthorizesViaAgentGet(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build() // no agent exists
	s := newTemplatesServer(t, &fakeCallerClientFactory{client: c}, publishedartifact.NewMemStore())

	rec := deleteTemplate(t, s, kindAgent, detailNS, "ghost")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}
