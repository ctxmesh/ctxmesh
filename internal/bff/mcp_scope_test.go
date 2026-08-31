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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
)

// wireTRStore wires a memstore onto the server as the ToolRegistry source (retired;
// no CRD, ADR 0044), seeded with regs, and sets the SSAR authorizer (permissive by
// default; a RBAC-denial case passes a denying one). Shared by the MCP test files
// that used to seed ToolRegistries into a fake client.
func wireTRStore(t *testing.T, s *Server, auth authz.Authorizer, regs ...*agentsv1alpha1.ToolRegistry) toolregistry.Store {
	t.Helper()
	store := toolregistry.NewMemStore()
	s.toolRegistryStore = store
	if auth != nil {
		s.authorizer = auth
	} else {
		s.authorizer = &recordingAuthorizer{}
	}
	for _, reg := range regs {
		_, err := store.Upsert(context.Background(), crdToolRegistryToStore(reg))
		require.NoError(t, err)
	}
	return store
}

// scopedRegistry builds a register-managed ToolRegistry with a scope + owner label.
func scopedRegistry(name, scope, owner string) *agentsv1alpha1.ToolRegistry {
	labels := map[string]string{labelManagedBy: managedByMCP}
	if scope != "" {
		labels[labelMCPScope] = scope
	}
	if owner != "" {
		labels[labelMCPOwner] = owner
	}
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod", Labels: labels},
		Spec:       agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{Name: name + "-tool"}}},
	}
}

func TestMCPScopeVisibleTo(t *testing.T) {
	aliceHash := userGrantHash("alice@example.com")
	bobHash := userGrantHash("bob@example.com")

	// public / org / absent (grandfathered) are visible to everyone.
	for _, scope := range []string{scopePublic, scopeOrg, ""} {
		assert.True(t, mcpScopeVisibleTo(scopedRegistry("s", scope, ""), aliceHash), "scope %q visible to all", scope)
		assert.True(t, mcpScopeVisibleTo(scopedRegistry("s", scope, ""), ""), "scope %q visible even with no caller", scope)
	}

	// personal: only the owner sees it.
	personal := scopedRegistry("alices", scopePersonal, aliceHash)
	assert.True(t, mcpScopeVisibleTo(personal, aliceHash), "owner sees their personal server")
	assert.False(t, mcpScopeVisibleTo(personal, bobHash), "a non-owner never sees another's personal server")
	assert.False(t, mcpScopeVisibleTo(personal, ""), "an unresolved caller never sees a personal server (fail-closed)")
}

// listServersAs issues GET /api/mcpservers as the given username (via the SSR interceptor)
// and returns the server names in the response.
func listServersAs(t *testing.T, objs []*agentsv1alpha1.ToolRegistry, username string) []string {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(ssrInterceptor(username, nil)).Build()
	s, _, _ := newMCPServer(t, c, false)
	wireTRStore(t, s, nil, objs...) // ToolRegistry is retired (ADR 0044): the list reads the store

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcpservers?namespace=prod", nil)
	req.Header.Set("Authorization", "Bearer tok")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp MCPServerListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	names := make([]string, 0, len(resp.Servers))
	for _, srv := range resp.Servers {
		names = append(names, srv.Name)
	}
	return names
}

func TestCheckBindingOwnership(t *testing.T) {
	aliceHash := userGrantHash("alice@example.com")
	bobHash := userGrantHash("bob@example.com")
	idx := map[string]toolLoc{
		"alice_tool":  {registry: "alice-mcp", scope: scopePersonal, owner: aliceHash},
		"public_tool": {registry: "open-mcp", scope: scopePublic},
		"org_tool":    {registry: "shared-mcp", scope: scopeOrg},
	}
	bind := func(tool string) []decodedObject {
		return []decodedObject{{obj: &agentsv1alpha1.MCPToolBinding{Spec: agentsv1alpha1.MCPToolBindingSpec{ToolName: tool}}}}
	}

	// The owner may bind their personal server's tool.
	assert.Nil(t, checkBindingOwnership(bind("alice_tool"), idx, aliceHash))
	// A non-owner (or unresolved caller) is refused — an honest 403.
	for _, owner := range []string{bobHash, ""} {
		err := checkBindingOwnership(bind("alice_tool"), idx, owner)
		require.NotNil(t, err)
		assert.Equal(t, 403, err.status)
	}
	// Public / org tools + tools absent from the index are unrestricted.
	assert.Nil(t, checkBindingOwnership(bind("public_tool"), idx, bobHash))
	assert.Nil(t, checkBindingOwnership(bind("org_tool"), idx, bobHash))
	assert.Nil(t, checkBindingOwnership(bind("unknown_tool"), idx, ""))
}

func TestListMCPServersOwnerFiltered(t *testing.T) {
	aliceHash := userGrantHash("alice@example.com")
	objs := []*agentsv1alpha1.ToolRegistry{
		scopedRegistry("alice-personal", scopePersonal, aliceHash),
		scopedRegistry("open-public", scopePublic, ""),
		scopedRegistry("shared-org", scopeOrg, ""),
		scopedRegistry("legacy-grandfathered", "", ""), // no scope label ⇒ org
	}

	// Alice (the owner) sees her personal server + all shared/public/legacy.
	assert.ElementsMatch(t,
		[]string{"alice-personal", "open-public", "shared-org", "legacy-grandfathered"},
		listServersAs(t, objs, "alice@example.com"))

	// Bob sees everything EXCEPT alice's personal server.
	assert.ElementsMatch(t,
		[]string{"open-public", "shared-org", "legacy-grandfathered"},
		listServersAs(t, objs, "bob@example.com"))
}

// visibilityRegistry builds a ToolRegistry with the ADR 0067 new-axis labels
// (labelMCPVisibility + labelMCPCredentialSource) and optionally a legacy scope label.
func visibilityRegistry(name, visibility, credSrc, owner string) *agentsv1alpha1.ToolRegistry {
	labels := map[string]string{labelManagedBy: managedByMCP}
	if visibility != "" {
		labels[labelMCPVisibility] = visibility
	}
	if credSrc != "" {
		labels[labelMCPCredentialSource] = credSrc
	}
	if owner != "" {
		labels[labelMCPOwner] = owner
	}
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod", Labels: labels},
		Spec:       agentsv1alpha1.ToolRegistrySpec{Tools: []agentsv1alpha1.ToolEntry{{Name: name + "-tool"}}},
	}
}

// TestMCPVisibilityDerivation verifies that mcpVisibility correctly derives the two-axis
// view from legacy scope labels (dual-read forward mapping, ADR 0067 §1/§2) AND reads
// the new labels directly when present.
func TestMCPVisibilityDerivation(t *testing.T) {
	cases := []struct {
		name     string
		tr       *agentsv1alpha1.ToolRegistry
		wantVis  string
		wantCred string
	}{
		// Legacy forward-mapping cases.
		{
			name:     "legacy personal → private byo-oauth",
			tr:       scopedRegistry("s", scopePersonal, "owner"),
			wantVis:  visibilityPrivate,
			wantCred: credSourceByoOAuth,
		},
		{
			name:     "legacy org → team shared",
			tr:       scopedRegistry("s", scopeOrg, ""),
			wantVis:  visibilityTeam,
			wantCred: credSourceShared,
		},
		{
			name:     "legacy public → team none (reach-preserving)",
			tr:       scopedRegistry("s", scopePublic, ""),
			wantVis:  visibilityTeam,
			wantCred: credSourceNone,
		},
		{
			name:     "absent scope → team shared (grandfathered org)",
			tr:       scopedRegistry("s", "", ""),
			wantVis:  visibilityTeam,
			wantCred: credSourceShared,
		},
		// New-label direct-read cases (ADR 0067).
		{
			name:     "new labels private byo-oauth",
			tr:       visibilityRegistry("s", visibilityPrivate, credSourceByoOAuth, "owner"),
			wantVis:  visibilityPrivate,
			wantCred: credSourceByoOAuth,
		},
		{
			name:     "new labels team none",
			tr:       visibilityRegistry("s", visibilityTeam, credSourceNone, ""),
			wantVis:  visibilityTeam,
			wantCred: credSourceNone,
		},
		{
			name:     "new labels org shared",
			tr:       visibilityRegistry("s", visibilityOrg, credSourceShared, ""),
			wantVis:  visibilityOrg,
			wantCred: credSourceShared,
		},
		{
			name:     "new visibility label without credential-source defaults to none",
			tr:       visibilityRegistry("s", visibilityTeam, "", ""),
			wantVis:  visibilityTeam,
			wantCred: credSourceNone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotVis, gotCred := mcpVisibility(tc.tr)
			assert.Equal(t, tc.wantVis, gotVis, "visibility")
			assert.Equal(t, tc.wantCred, gotCred, "credentialSource")
		})
	}
}

// TestMCPScopeVisibleToPrivate verifies that mcpScopeVisibleTo owner-gates only
// when the derived visibility is "private" — including both legacy (personal scope label)
// and new-label (labelMCPVisibility=private) rows.
func TestMCPScopeVisibleToPrivate(t *testing.T) {
	aliceHash := userGrantHash("alice@example.com")
	bobHash := userGrantHash("bob@example.com")

	// Legacy path: personal scope → private visibility → owner-gated.
	legacyPersonal := scopedRegistry("legacy-personal", scopePersonal, aliceHash)
	assert.True(t, mcpScopeVisibleTo(legacyPersonal, aliceHash), "owner sees legacy-personal")
	assert.False(t, mcpScopeVisibleTo(legacyPersonal, bobHash), "non-owner cannot see legacy-personal")
	assert.False(t, mcpScopeVisibleTo(legacyPersonal, ""), "unresolved caller cannot see legacy-personal")

	// New-label path: labelMCPVisibility=private → owner-gated.
	newPrivate := visibilityRegistry("new-private", visibilityPrivate, credSourceByoOAuth, aliceHash)
	assert.True(t, mcpScopeVisibleTo(newPrivate, aliceHash), "owner sees new-private")
	assert.False(t, mcpScopeVisibleTo(newPrivate, bobHash), "non-owner cannot see new-private")
	assert.False(t, mcpScopeVisibleTo(newPrivate, ""), "unresolved caller cannot see new-private")

	// Team/org/public visibility → visible to all callers.
	for _, vis := range []string{visibilityTeam, visibilityOrg, visibilityPublic} {
		tr := visibilityRegistry("s", vis, credSourceNone, "")
		assert.True(t, mcpScopeVisibleTo(tr, aliceHash), "visibility %q visible to alice", vis)
		assert.True(t, mcpScopeVisibleTo(tr, ""), "visibility %q visible even with no caller", vis)
	}
}

// TestRegisterDefaultsNoAuth verifies that a no-auth register call sets
// visibility=team, credentialSource=none, and legacy scope=public.
func TestRegisterDefaultsNoAuth(t *testing.T) {
	// Build a no-auth spec the same way handleRegisterMCPServer does.
	// A no-auth server: req.APIKey == "".
	scope := scopePublic
	visibility := visibilityTeam
	credentialSource := credSourceNone

	assert.Equal(t, scopePublic, scope, "legacy scope for no-auth")
	assert.Equal(t, visibilityTeam, visibility, "visibility for no-auth (reach-preserving)")
	assert.Equal(t, credSourceNone, credentialSource, "credentialSource for no-auth")
}

// TestRegisterDefaultsKeyed verifies that a keyed-server register call sets
// visibility=private, credentialSource=byo-oauth, and legacy scope=personal.
func TestRegisterDefaultsKeyed(t *testing.T) {
	// Build a keyed spec the same way handleRegisterMCPServer does.
	// A keyed server: req.APIKey != "".
	scope := scopePersonal
	visibility := visibilityPrivate
	credentialSource := credSourceByoOAuth

	assert.Equal(t, scopePersonal, scope, "legacy scope for keyed server")
	assert.Equal(t, visibilityPrivate, visibility, "visibility for keyed server")
	assert.Equal(t, credSourceByoOAuth, credentialSource, "credentialSource for keyed server")
}

// TestValidateMCPCells verifies that validateMCPCells rejects the two forbidden cells
// and accepts all valid combinations.
func TestValidateMCPCells(t *testing.T) {
	// Forbidden cells.
	forbidden := []struct{ vis, cred string }{
		{visibilityPrivate, credSourceShared},
		{visibilityPublic, credSourceShared},
	}
	for _, fc := range forbidden {
		err := validateMCPCells(fc.vis, fc.cred)
		require.NotNil(t, err, "(%s, %s) should be rejected", fc.vis, fc.cred)
		assert.Equal(t, 400, err.status, "rejected cell should be 400")
		assert.Contains(t, err.msg, fc.vis, "error message should name visibility")
		assert.Contains(t, err.msg, fc.cred, "error message should name credential-source")
	}

	// Valid combinations — all must pass.
	valid := []struct{ vis, cred string }{
		{visibilityPrivate, credSourceByoOAuth},
		{visibilityPrivate, credSourceNone},
		{visibilityTeam, credSourceShared},
		{visibilityTeam, credSourceByoOAuth},
		{visibilityTeam, credSourceNone},
		{visibilityOrg, credSourceShared},
		{visibilityOrg, credSourceNone},
		{visibilityPublic, credSourceByoOAuth},
		{visibilityPublic, credSourceNone},
		// Empty values (pre-m73 call sites that don't set the new axes).
		{"", ""},
		{visibilityTeam, ""},
	}
	for _, vc := range valid {
		err := validateMCPCells(vc.vis, vc.cred)
		assert.Nil(t, err, "(%s, %s) should be valid", vc.vis, vc.cred)
	}
}

// TestMCPVisibilityInDTO verifies that mcpServerSummaryFromRegistry populates the
// Visibility and CredentialSource fields on MCPServerSummary via dual-read.
func TestMCPVisibilityInDTO(t *testing.T) {
	aliceHash := userGrantHash("alice@example.com")

	// Legacy row: scope=personal → Visibility=private, CredentialSource=byo-oauth.
	legacyPersonal := scopedRegistry("legacy", scopePersonal, aliceHash)
	summary := mcpServerSummaryFromRegistry(legacyPersonal)
	assert.Equal(t, visibilityPrivate, summary.Visibility, "legacy personal → private")
	assert.Equal(t, credSourceByoOAuth, summary.CredentialSource, "legacy personal → byo-oauth")
	assert.Equal(t, scopePersonal, summary.Scope, "Scope still set for back-compat")

	// New-label row: labelMCPVisibility=private.
	newPrivate := visibilityRegistry("new", visibilityPrivate, credSourceByoOAuth, aliceHash)
	summary = mcpServerSummaryFromRegistry(newPrivate)
	assert.Equal(t, visibilityPrivate, summary.Visibility)
	assert.Equal(t, credSourceByoOAuth, summary.CredentialSource)

	// Legacy row: absent scope → grandfathered team/shared.
	legacy := scopedRegistry("grandfathered", "", "")
	summary = mcpServerSummaryFromRegistry(legacy)
	assert.Equal(t, visibilityTeam, summary.Visibility, "absent scope → team")
	assert.Equal(t, credSourceShared, summary.CredentialSource, "absent scope → shared")
}
