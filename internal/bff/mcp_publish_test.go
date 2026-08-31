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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// resourceActionKey identifies an authorizer action for the action-based authorizer.
type resourceActionKey struct {
	verb     string
	resource string
}

// resourceActionAuthorizer allows every (verb, resource) pair EXCEPT those in deny.
// This lets tests model a caller who can update toolregistries but NOT tenants, or
// vice versa — exercising the three publish tiers independently.
type resourceActionAuthorizer struct {
	deny map[resourceActionKey]bool
	last authz.Action
}

func (a *resourceActionAuthorizer) Authorize(_ context.Context, _ client.Client, action authz.Action) error {
	a.last = action
	if a.deny[resourceActionKey{verb: action.Verb, resource: action.Resource}] {
		return authz.ErrForbidden
	}
	return nil
}

// newPublishServer builds a Server+store wired for the publish handler tests.
// The authorizer controls which (verb, resource) pairs pass.
func newPublishServer(t *testing.T, auth authz.Authorizer, regs ...*agentsv1alpha1.ToolRegistry) (*Server, toolregistry.Store) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)
	store := wireTRStore(t, s, auth, regs...)
	return s, store
}

// servePublish issues POST /api/mcp/publish and returns the recorder.
func servePublish(t *testing.T, s *Server, req MCPPublishRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	hreq := httptest.NewRequest(http.MethodPost, "/api/mcp/publish", bytes.NewReader(b))
	hreq.Header.Set("Authorization", "Bearer tok")
	s.Handler().ServeHTTP(rec, hreq)
	return rec
}

// publishReq is a convenience builder for MCPPublishRequest in tests.
func publishReq(ns, name, visibility string) MCPPublishRequest {
	return MCPPublishRequest{Namespace: ns, Name: name, Visibility: visibility}
}

// newPrivateServer returns a private (personal) ToolRegistry for the given name+ns.
func newPrivateServer(name, ns string) *agentsv1alpha1.ToolRegistry {
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				labelManagedBy:           managedByMCP,
				labelMCPVisibility:       visibilityPrivate,
				labelMCPCredentialSource: credSourceByoOAuth,
				labelMCPScope:            scopePersonal,
			},
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{Name: name + "-tool"}}},
	}
}

// newOrgVisServer returns an org-visible ToolRegistry (already widened past team).
func newOrgVisServer(name, ns string) *agentsv1alpha1.ToolRegistry {
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				labelManagedBy:           managedByMCP,
				labelMCPVisibility:       visibilityOrg,
				labelMCPCredentialSource: credSourceNone,
				labelMCPScope:            scopeOrg,
			},
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{Name: name + "-tool"}}},
	}
}

// newSharedCredServer returns a team-visible server with a shared credential.
func newSharedCredServer(name, ns string) *agentsv1alpha1.ToolRegistry {
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				labelManagedBy:           managedByMCP,
				labelMCPVisibility:       visibilityTeam,
				labelMCPCredentialSource: credSourceShared,
				labelMCPScope:            scopeOrg,
			},
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{Name: name + "-tool"}}},
	}
}

// -----------------------------------------------------------------------
// Input validation
// -----------------------------------------------------------------------

// TestMCPPublishRejectsMissingFields verifies that missing name or visibility → 400.
func TestMCPPublishRejectsMissingFields(t *testing.T) {
	s, _ := newPublishServer(t, &recordingAuthorizer{})

	cases := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"missing name", `{"namespace":"prod","visibility":"team"}`, "required"},
		{"missing visibility", `{"namespace":"prod","name":"my-mcp"}`, "required"},
		{"empty body", `{}`, "required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/mcp/publish", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Authorization", "Bearer tok")
			s.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.wantMsg)
		})
	}
}

// TestMCPPublishRejectsPrivateTarget verifies that target visibility "private" → 400
// with an honest "unpublish is not supported" message.
func TestMCPPublishRejectsPrivateTarget(t *testing.T) {
	s, _ := newPublishServer(t, &recordingAuthorizer{})
	rec := servePublish(t, s, publishReq("prod", "any-server", visibilityPrivate))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "unpublish")
}

// TestMCPPublishRejectsInvalidVisibility verifies that an unknown visibility → 400.
func TestMCPPublishRejectsInvalidVisibility(t *testing.T) {
	s, _ := newPublishServer(t, &recordingAuthorizer{})
	rec := servePublish(t, s, publishReq("prod", "any-server", "bogus"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// -----------------------------------------------------------------------
// Team tier
// -----------------------------------------------------------------------

// TestMCPPublishTeamHappyPath verifies that a caller with update toolregistries in
// the server's namespace can widen to team visibility.
func TestMCPPublishTeamHappyPath(t *testing.T) {
	ctx := context.Background()
	auth := &resourceActionAuthorizer{} // allow all by default
	reg := newPrivateServer("alpha-server", "prod")
	s, store := newPublishServer(t, auth, reg)

	rec := servePublish(t, s, publishReq("prod", "alpha-server", visibilityTeam))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Verify store was updated.
	after, err := store.Get(ctx, "prod", "alpha-server")
	require.NoError(t, err)
	assert.Equal(t, visibilityTeam, after.Labels[labelMCPVisibility])
	assert.Equal(t, credSourceByoOAuth, after.Labels[labelMCPCredentialSource], "credential source unchanged")
	assert.Equal(t, scopeOrg, after.Labels[labelMCPScope], "legacy scope set to org")

	// The SSAR was for toolregistries update in ns.
	assert.Equal(t, authz.VerbUpdate, auth.last.Verb)
	assert.Equal(t, resourceToolRegistries, auth.last.Resource)
	assert.Equal(t, "prod", auth.last.Namespace)
}

// TestMCPPublishTeamDenied verifies that a caller without update toolregistries → 403.
func TestMCPPublishTeamDenied(t *testing.T) {
	auth := &resourceActionAuthorizer{
		deny: map[resourceActionKey]bool{
			{authz.VerbUpdate, resourceToolRegistries}: true,
		},
	}
	reg := newPrivateServer("beta-server", "prod")
	s, store := newPublishServer(t, auth, reg)

	rec := servePublish(t, s, publishReq("prod", "beta-server", visibilityTeam))
	require.Equal(t, http.StatusForbidden, rec.Code)

	// Store must be unchanged.
	ctx := context.Background()
	after, err := store.Get(ctx, "prod", "beta-server")
	require.NoError(t, err)
	assert.Equal(t, visibilityPrivate, after.Labels[labelMCPVisibility], "denied publish does NOT change visibility")
}

// -----------------------------------------------------------------------
// Org tier
// -----------------------------------------------------------------------

// TestMCPPublishOrgHappyPath verifies that a caller with update tenants (cluster-scoped)
// can widen to org visibility.
func TestMCPPublishOrgHappyPath(t *testing.T) {
	ctx := context.Background()
	auth := &resourceActionAuthorizer{} // allow all
	reg := newPrivateServer("gamma-server", "prod")
	s, store := newPublishServer(t, auth, reg)

	rec := servePublish(t, s, publishReq("prod", "gamma-server", visibilityOrg))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := store.Get(ctx, "prod", "gamma-server")
	require.NoError(t, err)
	assert.Equal(t, visibilityOrg, after.Labels[labelMCPVisibility])

	// The SSAR was for tenants update (cluster-scoped).
	assert.Equal(t, authz.VerbUpdate, auth.last.Verb)
	assert.Equal(t, resourceTenants, auth.last.Resource)
	assert.Equal(t, "", auth.last.Namespace, "cluster-scoped: no namespace")
}

// TestMCPPublishOrgDenied verifies that a caller without update tenants → 403
// and the store is unchanged.
func TestMCPPublishOrgDenied(t *testing.T) {
	ctx := context.Background()
	auth := &resourceActionAuthorizer{
		deny: map[resourceActionKey]bool{
			{authz.VerbUpdate, resourceTenants}: true,
		},
	}
	reg := newPrivateServer("delta-server", "prod")
	s, store := newPublishServer(t, auth, reg)

	rec := servePublish(t, s, publishReq("prod", "delta-server", visibilityOrg))
	require.Equal(t, http.StatusForbidden, rec.Code)

	after, err := store.Get(ctx, "prod", "delta-server")
	require.NoError(t, err)
	assert.Equal(t, visibilityPrivate, after.Labels[labelMCPVisibility], "denied publish does NOT change visibility")
}

// -----------------------------------------------------------------------
// Public tier
// -----------------------------------------------------------------------

// TestMCPPublishPublicHappyPath verifies that a platform operator (update tenants,
// cluster-wide) can widen to public visibility.
func TestMCPPublishPublicHappyPath(t *testing.T) {
	ctx := context.Background()
	auth := &resourceActionAuthorizer{} // allow all (platform operator)
	reg := newPrivateServer("epsilon-server", "prod")
	s, store := newPublishServer(t, auth, reg)

	rec := servePublish(t, s, publishReq("prod", "epsilon-server", visibilityPublic))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := store.Get(ctx, "prod", "epsilon-server")
	require.NoError(t, err)
	assert.Equal(t, visibilityPublic, after.Labels[labelMCPVisibility])

	// Public SSAR: update tenants, cluster-scoped, no resource name.
	assert.Equal(t, authz.VerbUpdate, auth.last.Verb)
	assert.Equal(t, resourceTenants, auth.last.Resource)
	assert.Equal(t, "", auth.last.Namespace)
	assert.Equal(t, "", auth.last.Name, "public tier: no resource name (can edit ALL tenants)")
}

// TestMCPPublishPublicDenied verifies that a non-platform-admin → 403.
func TestMCPPublishPublicDenied(t *testing.T) {
	auth := &resourceActionAuthorizer{
		deny: map[resourceActionKey]bool{
			{authz.VerbUpdate, resourceTenants}: true,
		},
	}
	reg := newPrivateServer("zeta-server", "staging")
	s, _ := newPublishServer(t, auth, reg)

	rec := servePublish(t, s, publishReq("staging", "zeta-server", visibilityPublic))
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// -----------------------------------------------------------------------
// No-egress invariant
// -----------------------------------------------------------------------

// TestMCPPublishOpensNoEgress asserts that a successful publish does NOT create or
// modify any NetworkPolicy (the m14.6 B1 invariant — only handleApproveMCP opens
// egress). We verify this by confirming no NetworkPolicy is created via the fake
// client after a successful publish.
func TestMCPPublishOpensNoEgress(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _, _ := newMCPServer(t, c, false)
	auth := &resourceActionAuthorizer{} // allow all
	wireTRStore(t, s, auth, newPrivateServer("eta-server", "prod"))

	rec := servePublish(t, s, publishReq("prod", "eta-server", visibilityTeam))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Verify no NetworkPolicy was created via the fake client.
	var npl networkingv1.NetworkPolicyList
	require.NoError(t, c.List(context.Background(), &npl))
	assert.Empty(t, npl.Items, "publish MUST NOT create a NetworkPolicy — egress is only opened by handleApproveMCP")
}

// -----------------------------------------------------------------------
// validateMCPCells integration: shared-credential server → public forbidden
// -----------------------------------------------------------------------

// TestMCPPublishSharedCredToPublicForbidden verifies that publishing a shared-credential
// server to public visibility → 400 (validateMCPCells rejects public×shared).
func TestMCPPublishSharedCredToPublicForbidden(t *testing.T) {
	auth := &resourceActionAuthorizer{} // platform operator — all SSAR pass
	reg := newSharedCredServer("shared-mcp", "prod")
	s, _ := newPublishServer(t, auth, reg)

	rec := servePublish(t, s, publishReq("prod", "shared-mcp", visibilityPublic))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "shared", "error should mention the forbidden cell")
}

// -----------------------------------------------------------------------
// Not-found case
// -----------------------------------------------------------------------

// TestMCPPublishServerNotFound verifies that publishing a non-existent server → 404.
func TestMCPPublishServerNotFound(t *testing.T) {
	s, _ := newPublishServer(t, &recordingAuthorizer{}) // empty store
	rec := servePublish(t, s, publishReq("prod", "unknown-server", visibilityTeam))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// -----------------------------------------------------------------------
// handleSetOrgCredential no-downgrade refinement
// -----------------------------------------------------------------------

// TestSetOrgCredentialDoesNotDowngradeOrgVisibility verifies the m73.5 refinement:
// setting a shared credential on an already org-visible server MUST NOT downgrade
// its visibility to team.
func TestSetOrgCredentialDoesNotDowngradeOrgVisibility(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	auth := &verbAuthorizer{} // allow all verbs
	s, store := retireOrgServer(t, c, auth)

	// Seed an org-visible server into the store (retireOrgServer seeds "scalekit" as personal).
	_, err := store.Upsert(ctx, crdToolRegistryToStore(newOrgVisServer("my-org-server", "prod")))
	require.NoError(t, err)

	body, _ := json.Marshal(SetOrgCredentialRequest{Server: "my-org-server", Namespace: "prod", Credential: "SHARED"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/org-credential", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-tok")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := store.Get(ctx, "prod", "my-org-server")
	require.NoError(t, err)
	assert.Equal(t, visibilityOrg, after.Labels[labelMCPVisibility],
		"setting a shared credential on an org-visible server MUST NOT downgrade visibility to team")
	assert.Equal(t, credSourceShared, after.Labels[labelMCPCredentialSource])
}

// TestSetOrgCredentialPrivateElevatedToTeam verifies the baseline behavior:
// a private server is elevated to team when a shared credential is set.
func TestSetOrgCredentialPrivateElevatedToTeam(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	auth := &verbAuthorizer{}
	s, store := retireOrgServer(t, c, auth)
	// retireOrgServer seeds "scalekit" as personal / private.

	body, _ := json.Marshal(SetOrgCredentialRequest{Server: "scalekit", Namespace: "prod", Credential: "KEY"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/org-credential", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-tok")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := store.Get(ctx, "prod", "scalekit")
	require.NoError(t, err)
	assert.Equal(t, visibilityTeam, after.Labels[labelMCPVisibility],
		"private server is elevated to team when shared credential is set")
	assert.Equal(t, credSourceShared, after.Labels[labelMCPCredentialSource])
}
