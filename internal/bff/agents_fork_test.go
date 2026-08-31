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

// Tests for POST /api/agents/{ns}/{name}/fork (m74.3, ADR 0068 §4/§6) — fork =
// install-from-template = ONE create path.
//
// What we cover:
//   (a) duplicate-in-place — forking your OWN agent reads its live source-spec,
//       creates the copy through createAgentFromYAML, and stamps fork-origin provenance.
//   (b) cross-namespace fork — reads the PUBLISHED snapshot (not the live foreign agent)
//       and gates discoverability: public forks; org-in-tenant forks; org-outside → 404;
//       private → 404 (a private origin isn't even publishable, so its snapshot is absent).
//   (c) provenance-matched idempotency — a same-origin re-fork → 200 already-forked;
//       a name collision with a DIFFERENT / absent origin → 409 (never a silent 200-lie).
//   (d) the edit-preservation test — a PUT edit of a forked agent PRESERVES its
//       fork-origin labels (the future staleness feature depends on this — ADR 0068 §6).

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/promptversion"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/publishedartifact"
	"github.com/ctxmesh/ctxmesh/internal/expand"
)

// forkCallerNS is the caller's own namespace in these tests (where forks land).
const forkCallerNS = "consumer-ns"

// forkSourceSpec is a minimal console source-spec (canonical JSON) an agent is forked from.
// The `name` is overwritten by the fork with the local name; every other field rides through.
func forkSourceSpec(name string) string {
	return `{"name":"` + name + `","image":"ghcr.io/ctxmesh/echo:v1"}`
}

// newForkServer wires a BFF Server for the fork handler tests: an identity caller factory
// (so callerUsername resolves + createAgentFromYAML runs on a scheme-aware backing client),
// the promptStore + expand adapter (the create path), and the published-artifact + namespace-
// tenant stores (the cross-namespace read + discoverability gate). It returns the server and
// the backing client so tests can assert what landed.
func newForkServer(t *testing.T, artStore publishedartifact.Store, nsStore namespacetenant.Store, seed ...client.Object) (*Server, client.WithWatch) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(seed...).Build()
	s := NewServer(Options{
		CallerClients:          &identityCallerFactory{backing: c},
		Scheme:                 testScheme(t),
		Auth:                   AllowAll{},
		Adapters:               Adapters{Expand: NewExpandAdapter()},
		PublishedArtifactStore: artStore,
		NamespaceTenantStore:   nsStore,
		Version:                "test",
		Log:                    logr.Discard(),
	})
	// PromptVersion is Postgres-only (ADR 0044) — a create needs the store wired even
	// though these specs carry no inline prompt.
	s.promptStore = promptversion.NewMemStore()
	// Permissive SSAR so the caller-scoped create's authz passes (the discoverability gate
	// is what these tests exercise, not RBAC).
	s.authorizer = &recordingAuthorizer{}
	return s, c
}

// ownAgent builds a console-authored AgentDeployment (carrying the source-spec annotation) in
// the given namespace — the duplicate-in-place origin.
func ownAgent(name, ns string) *agentsv1alpha1.AgentDeployment {
	return &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: map[string]string{expand.AnnotationSourceSpec: forkSourceSpec(name)},
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/echo:v1"},
	}
}

// doFork serves a POST /api/agents/{originNS}/{originName}/fork request with the given body.
func doFork(t *testing.T, s *Server, originNS, originName string, body ForkAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+originNS+"/"+originName+"/fork", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer alice")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// -----------------------------------------------------------------------
// (a) duplicate-in-place
// -----------------------------------------------------------------------

// TestFork_DuplicateInPlace forks the caller's OWN agent (origin ns == caller ns): the copy is
// created via the ONE create path from the live source-spec, under the requested local name,
// carrying the fork-origin provenance labels (version empty — no published snapshot).
func TestFork_DuplicateInPlace(t *testing.T) {
	ctx := context.Background()
	origin := ownAgent("assistant", forkCallerNS)
	s, c := newForkServer(t, publishedartifact.NewMemStore(), namespacetenant.NewMemStore(), origin)

	rec := doFork(t, s, forkCallerNS, "assistant", ForkAgentRequest{Name: "my-assistant", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "forked", resp.Status)
	assert.NotNil(t, resp.NeedsRebinding, "needsRebinding must be [] not null")
	assert.Empty(t, resp.NeedsRebinding, "m74.3 copies the source-spec as-is")
	assert.NotNil(t, resp.UnresolvedRefs)
	assert.Empty(t, resp.UnresolvedRefs)
	// P1-1 fix: the response must expose the FORK's namespace+name (the caller's ns),
	// not the origin's, so the UI navigates to the caller's copy, not the publisher's.
	assert.Equal(t, "my-assistant", resp.Agent.Name, "Agent.Name must be the fork's local name")
	assert.Equal(t, forkCallerNS, resp.Agent.Namespace, "Agent.Namespace must be the CALLER's namespace, not the origin's")

	// The forked AgentDeployment landed under the local name, with provenance.
	var forked agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: forkCallerNS, Name: "my-assistant"}, &forked))
	assert.Equal(t, forkCallerNS, forked.Labels[labelForkOriginNamespace])
	assert.Equal(t, "assistant", forked.Labels[labelForkOriginName])
	assert.Empty(t, forked.Labels[labelForkOriginVersion], "duplicate-in-place has no published version")
	// The forked copy carries its own source-spec (renamed) so it round-trips on edit.
	assert.Contains(t, forked.Annotations[expand.AnnotationSourceSpec], `"my-assistant"`)
}

// TestFork_DefaultName forks with no name in the body → the copy reuses the ORIGIN name in the
// caller's namespace (exercised cross-namespace via a published snapshot so the default-named
// copy does not collide with the origin).
func TestFork_DefaultName(t *testing.T) {
	art := publishedartifact.NewMemStore()
	publishArt(t, art, "publisher-ns", "assistant", visibilityPublic, "hash-1")
	s, c := newForkServer(t, art, namespacetenant.NewMemStore())

	rec := doFork(t, s, "publisher-ns", "assistant", ForkAgentRequest{Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var forked agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: forkCallerNS, Name: "assistant"}, &forked))
	assert.Equal(t, "publisher-ns", forked.Labels[labelForkOriginNamespace])
}

// TestFork_NotForkable_400: an origin with no source-spec (kubectl-authored) is not forkable.
func TestFork_NotForkable_400(t *testing.T) {
	kubectlAgent := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "raw", Namespace: forkCallerNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:1"},
	}
	s, _ := newForkServer(t, publishedartifact.NewMemStore(), namespacetenant.NewMemStore(), kubectlAgent)

	rec := doFork(t, s, forkCallerNS, "raw", ForkAgentRequest{Namespace: forkCallerNS})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "no source-spec")
}

// TestFork_OwnNamespaceMissing_404: forking a missing own-ns agent → 404 (the caller-scoped GET fails).
func TestFork_OwnNamespaceMissing_404(t *testing.T) {
	s, _ := newForkServer(t, publishedartifact.NewMemStore(), namespacetenant.NewMemStore())
	rec := doFork(t, s, forkCallerNS, "ghost", ForkAgentRequest{Namespace: forkCallerNS})
	require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
}

// -----------------------------------------------------------------------
// (b) cross-namespace fork — published snapshot + discoverability gate
// -----------------------------------------------------------------------

// publishArt seeds a published artifact snapshot for origin ns/name at the given visibility.
func publishArt(t *testing.T, store publishedartifact.Store, ns, name, visibility, hash string) int {
	t.Helper()
	v, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: ns, OriginName: name,
		SpecJSON: json.RawMessage(forkSourceSpec(name)), Visibility: visibility, ContentHash: hash,
	})
	require.NoError(t, err)
	return v
}

// TestFork_CrossNS_Public forks a PUBLIC published agent from a foreign namespace: the snapshot
// is read (not the live foreign agent), discoverability passes, and the copy lands with the
// pinned origin-version + content-hash.
func TestFork_CrossNS_Public(t *testing.T) {
	ctx := context.Background()
	art := publishedartifact.NewMemStore()
	publishArt(t, art, "publisher-ns", "assistant", visibilityPublic, "hash-abc")
	// NB: no live agent in publisher-ns is seeded — the caller reads ONLY the snapshot (ADR 0011).
	s, c := newForkServer(t, art, namespacetenant.NewMemStore())

	rec := doFork(t, s, "publisher-ns", "assistant", ForkAgentRequest{Name: "local-copy", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var forked agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: forkCallerNS, Name: "local-copy"}, &forked))
	assert.Equal(t, "publisher-ns", forked.Labels[labelForkOriginNamespace])
	assert.Equal(t, "assistant", forked.Labels[labelForkOriginName])
	assert.Equal(t, "1", forked.Labels[labelForkOriginVersion], "the pinned published version")
	assert.Equal(t, "hash-abc", forked.Labels[labelForkContentHash])
}

// TestFork_CrossNS_OrgInTenant forks an ORG published agent from a sibling namespace IN the
// caller's tenant → allowed.
func TestFork_CrossNS_OrgInTenant(t *testing.T) {
	ctx := context.Background()
	nsStore := namespacetenant.NewMemStore()
	require.NoError(t, nsStore.SetMembers(ctx, "acme", []string{forkCallerNS, "sibling-ns"}))
	art := publishedartifact.NewMemStore()
	publishArt(t, art, "sibling-ns", "org-agent", visibilityOrg, "hash-org")
	s, c := newForkServer(t, art, nsStore)

	rec := doFork(t, s, "sibling-ns", "org-agent", ForkAgentRequest{Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var forked agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: forkCallerNS, Name: "org-agent"}, &forked))
	assert.Equal(t, "sibling-ns", forked.Labels[labelForkOriginNamespace])
}

// TestFork_CrossNS_OrgOutsideTenant_404: an ORG published agent OUTSIDE the caller's tenant is
// not discoverable → 404 (never 403 — do not confirm existence). Nothing is created.
func TestFork_CrossNS_OrgOutsideTenant_404(t *testing.T) {
	ctx := context.Background()
	nsStore := namespacetenant.NewMemStore()
	require.NoError(t, nsStore.SetMembers(ctx, "acme", []string{forkCallerNS})) // tenant excludes foreign-ns
	art := publishedartifact.NewMemStore()
	publishArt(t, art, "foreign-ns", "org-agent", visibilityOrg, "hash-far")
	s, c := newForkServer(t, art, nsStore)

	rec := doFork(t, s, "foreign-ns", "org-agent", ForkAgentRequest{Namespace: forkCallerNS})
	require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
	var forked agentsv1alpha1.AgentDeployment
	assert.Error(t, c.Get(ctx, client.ObjectKey{Namespace: forkCallerNS, Name: "org-agent"}, &forked),
		"an undiscoverable origin must not fork anything")
}

// TestFork_CrossNS_Team_404: a TEAM published agent in a FOREIGN namespace is never discoverable
// cross-namespace → 404 (team is own-namespace-only). This stands in for the "private → 404" case:
// a private agent is not even publishable, so its snapshot is absent → 404 identically.
func TestFork_CrossNS_Team_404(t *testing.T) {
	nsStore := namespacetenant.NewMemStore()
	require.NoError(t, nsStore.SetMembers(context.Background(), "acme", []string{forkCallerNS, "publisher-ns"}))
	art := publishedartifact.NewMemStore()
	publishArt(t, art, "publisher-ns", "team-agent", visibilityTeam, "hash-team")
	s, _ := newForkServer(t, art, nsStore)

	rec := doFork(t, s, "publisher-ns", "team-agent", ForkAgentRequest{Namespace: forkCallerNS})
	require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
}

// TestFork_CrossNS_NoSnapshot_404: a foreign origin with NO published snapshot (never published /
// tombstoned) → 404, indistinguishable from an undiscoverable one (never confirm existence).
func TestFork_CrossNS_NoSnapshot_404(t *testing.T) {
	s, _ := newForkServer(t, publishedartifact.NewMemStore(), namespacetenant.NewMemStore())
	rec := doFork(t, s, "publisher-ns", "unpublished", ForkAgentRequest{Namespace: forkCallerNS})
	require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
}

// -----------------------------------------------------------------------
// (c) provenance-matched idempotency
// -----------------------------------------------------------------------

// TestFork_Idempotent_SameOrigin: re-forking the SAME origin into an existing local copy → 200
// already-forked with the existing summary (a double-click is not a scary error).
func TestFork_Idempotent_SameOrigin(t *testing.T) {
	art := publishedartifact.NewMemStore()
	publishArt(t, art, "publisher-ns", "assistant", visibilityPublic, "hash-1")
	s, c := newForkServer(t, art, namespacetenant.NewMemStore())

	body := ForkAgentRequest{Name: "local", Namespace: forkCallerNS}
	rec1 := doFork(t, s, "publisher-ns", "assistant", body)
	require.Equal(t, http.StatusCreated, rec1.Code, "body: %s", rec1.Body.String())

	rec2 := doFork(t, s, "publisher-ns", "assistant", body)
	require.Equal(t, http.StatusOK, rec2.Code, "a same-origin re-fork must be 200; body: %s", rec2.Body.String())
	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	assert.Equal(t, "already-forked", resp.Status)
	assert.Equal(t, "local", resp.Agent.Name)

	// Exactly one copy exists (the re-fork created no duplicate) — verified by name still present.
	var got agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: forkCallerNS, Name: "local"}, &got))
	assert.Equal(t, "publisher-ns", got.Labels[labelForkOriginNamespace])
}

// TestFork_NameCollision_DifferentOrigin_409: an agent of the target name already exists but was
// forked from a DIFFERENT origin (or is not a fork at all) → 409 (a 200 there would silently lie).
func TestFork_NameCollision_DifferentOrigin_409(t *testing.T) {
	// A pre-existing local agent named "local" that is NOT a fork of publisher-ns/assistant
	// (it carries a different origin's provenance).
	collider := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "local",
			Namespace: forkCallerNS,
			Labels: map[string]string{
				labelForkOriginNamespace: "some-other-ns",
				labelForkOriginName:      "some-other-agent",
			},
		},
		Spec: agentsv1alpha1.AgentDeploymentSpec{Image: "img:x"},
	}
	art := publishedartifact.NewMemStore()
	publishArt(t, art, "publisher-ns", "assistant", visibilityPublic, "hash-1")
	s, _ := newForkServer(t, art, namespacetenant.NewMemStore(), collider)

	rec := doFork(t, s, "publisher-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusConflict, rec.Code, "a different-origin name collision must 409; body: %s", rec.Body.String())
}

// TestFork_NameCollision_NoForkLabels_409: a target-name agent with NO fork labels (a plain
// hand-authored agent) → 409 (it is not a fork of anything, so a re-fork must not 200-lie).
func TestFork_NameCollision_NoForkLabels_409(t *testing.T) {
	plain := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: forkCallerNS},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "img:plain"},
	}
	art := publishedartifact.NewMemStore()
	publishArt(t, art, "publisher-ns", "assistant", visibilityPublic, "hash-1")
	s, _ := newForkServer(t, art, namespacetenant.NewMemStore(), plain)

	rec := doFork(t, s, "publisher-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusConflict, rec.Code, "a no-fork-labels name collision must 409; body: %s", rec.Body.String())
}

// -----------------------------------------------------------------------
// (d) the edit-preservation test (ADR 0068 §6 — the Q4 trap)
// -----------------------------------------------------------------------

// TestFork_EditPreservesProvenanceLabels: a forked agent carries fork-origin provenance labels;
// a PUT round-trip edit (which re-expands from the source-spec, metadata the source-spec does
// NOT carry) MUST preserve those labels — the future "update available" staleness feature (ADR
// 0068 §6) is dead on arrival if the edit path drops them. This asserts the edit path already
// preserves foreign labels via SSA's granular label merge (a console apply that omits the fork
// labels never touches them).
func TestFork_EditPreservesProvenanceLabels(t *testing.T) {
	ctx := context.Background()
	art := publishedartifact.NewMemStore()
	publishArt(t, art, "publisher-ns", "assistant", visibilityPublic, "hash-1")
	s, c := newForkServer(t, art, namespacetenant.NewMemStore())

	// Author a fork (with provenance labels) via the real fork path.
	rec := doFork(t, s, "publisher-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var before agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: forkCallerNS, Name: "local"}, &before))
	require.Equal(t, "publisher-ns", before.Labels[labelForkOriginNamespace], "sanity: the fork stamped provenance")

	// PUT-edit the fork's source-spec (change the image) through the console edit round-trip.
	const edited = "name: local\nimage: ghcr.io/ctxmesh/echo:v2\n"
	body, err := json.Marshal(UpdateAgentRequest{AgentYAML: edited})
	require.NoError(t, err)
	putRec := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/api/agents/"+forkCallerNS+"/local", bytes.NewReader(body))
	putReq.Header.Set("Authorization", "Bearer alice")
	s.Handler().ServeHTTP(putRec, putReq)
	require.Equal(t, http.StatusOK, putRec.Code, "the edit must apply; body: %s", putRec.Body.String())

	var after agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: forkCallerNS, Name: "local"}, &after))
	// The edit landed AND the provenance labels survived.
	assert.Equal(t, "ghcr.io/ctxmesh/echo:v2", after.Spec.Image, "the edit must land")
	assert.Equal(t, "publisher-ns", after.Labels[labelForkOriginNamespace],
		"the edit path MUST preserve fork-origin provenance labels (ADR 0068 §6 staleness)")
	assert.Equal(t, "assistant", after.Labels[labelForkOriginName])
	assert.Equal(t, "1", after.Labels[labelForkOriginVersion])
	assert.Equal(t, "hash-1", after.Labels[labelForkContentHash])
}

// TestFork_NilStore_CrossNS_501: a BFF without the published-artifact store cannot resolve a
// cross-namespace origin → 501 (honest, never a panic). Own-ns forks still work (no store needed).
func TestFork_NilStore_CrossNS_501(t *testing.T) {
	s, _ := newForkServer(t, nil, namespacetenant.NewMemStore())
	rec := doFork(t, s, "publisher-ns", "assistant", ForkAgentRequest{Namespace: forkCallerNS})
	require.Equal(t, http.StatusNotImplemented, rec.Code, "body: %s", rec.Body.String())
}
