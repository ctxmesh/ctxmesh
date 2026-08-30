package bff

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
	"github.com/ctxmesh/agentry/internal/controlplane"
)

// managedMCPBundle builds the four objects the register flow creates for a server
// (all named <name>, plus the <name>-mcp-egress NetworkPolicy), labeled managed-by-MCP
// with the given scope/owner — so a delete test can assert the whole bundle is gone.
func managedMCPBundle(name, ns, scope, owner string) (*agentsv1alpha1.ToolRegistry, []client.Object) {
	labels := map[string]string{labelManagedBy: managedByMCP}
	if scope != "" {
		labels[labelMCPScope] = scope
	}
	if owner != "" {
		labels[labelMCPOwner] = owner
	}
	cp := func() map[string]string {
		m := map[string]string{}
		maps.Copy(m, labels)
		return m
	}
	// The ToolRegistry lives in the store (retired, ADR 0044); the rest are K8s
	// objects seeded into the fake client.
	tr := &agentsv1alpha1.ToolRegistry{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: cp()}}
	k8s := []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: cp()}},
		&agentsv1alpha1.SecretBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: cp()},
			Spec: agentsv1alpha1.SecretBindingSpec{
				Backend:   secretBackendKubernetes,
				SecretRef: agentsv1alpha1.SecretKeyRef{Name: name, Key: secretKeyOAuthAccessToken},
			},
		},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name + networkPolicyMCPSuffix, Namespace: ns, Labels: cp()}},
	}
	return tr, k8s
}

func deleteMCPServer(t *testing.T, s *Server, ns, name, userToken string) (*httptest.ResponseRecorder, DeleteMCPServerResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/mcpservers/"+ns+"/"+name, nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	s.Handler().ServeHTTP(rec, req)
	var resp DeleteMCPServerResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec, resp
}

func getMCPServerRefs(t *testing.T, s *Server, ns, name, userToken string) (*httptest.ResponseRecorder, MCPServerReferencesResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcpservers/"+ns+"/"+name+"/references", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	s.Handler().ServeHTTP(rec, req)
	var resp MCPServerReferencesResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec, resp
}

func gone(t *testing.T, c client.Client, obj client.Object, key client.ObjectKey) {
	t.Helper()
	err := c.Get(context.Background(), key, obj)
	assert.True(t, apierrors.IsNotFound(err), "expected %T %s to be deleted, got err=%v", obj, key.Name, err)
}

// TestDeleteMCPServerOwnerTearsDownBundle (m26.3): the owner of a personal server
// deletes it → the whole register bundle (ToolRegistry + Secret + SecretBinding +
// NetworkPolicy) is gone, and the response lists what was torn down.
func TestDeleteMCPServerOwnerTearsDownBundle(t *testing.T) {
	ctx := context.Background()
	const server, ns = "scalekit-mcp", "prod"
	owner := userGrantHash("user:alice-token") // the identity factory reports "user:<token>"
	tr, k8s := managedMCPBundle(server, ns, scopePersonal, owner)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(k8s...).Build()
	s, _ := newMCPServerWithIdentity(t, c)
	store := wireTRStore(t, s, nil, tr)

	rec, resp := deleteMCPServer(t, s, ns, server, "alice-token")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.ElementsMatch(t, []string{"ToolRegistry", "Secret", "SecretBinding", "NetworkPolicy"}, resp.Deleted)

	_, trErr := store.Get(ctx, ns, server)
	assert.ErrorIs(t, trErr, controlplane.ErrNotFound, "the store row is gone")
	gone(t, c, &corev1.Secret{}, client.ObjectKey{Namespace: ns, Name: server})
	gone(t, c, &agentsv1alpha1.SecretBinding{}, client.ObjectKey{Namespace: ns, Name: server})
	gone(t, c, &networkingv1.NetworkPolicy{}, client.ObjectKey{Namespace: ns, Name: server + networkPolicyMCPSuffix})
}

// TestDeleteMCPServerNonOwnerForbidden (m26.3): a non-owner cannot delete a personal
// server (ADR 0029 owner guard) — the bundle is untouched.
func TestDeleteMCPServerNonOwnerForbidden(t *testing.T) {
	// A non-"prod" namespace here also gives the shared helpers real ns variation.
	const server, ns = "alice-personal-mcp", "staging"
	owner := userGrantHash("user:alice-token")
	tr, k8s := managedMCPBundle(server, ns, scopePersonal, owner)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(k8s...).Build()
	s, _ := newMCPServerWithIdentity(t, c)
	store := wireTRStore(t, s, nil, tr)

	rec, _ := deleteMCPServer(t, s, ns, server, "bob-token") // not the owner
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// The server survives.
	_, err := store.Get(context.Background(), ns, server)
	require.NoError(t, err)
}

// TestDeleteMCPServerOrgScopeNoOwnerGuard (m26.3): an ORG server has no personal-owner
// guard — any caller allowed by RBAC (here, the fake client) can delete it. This is
// what makes the personal guard meaningful: it applies to personal servers only.
func TestDeleteMCPServerOrgScopeNoOwnerGuard(t *testing.T) {
	const server, ns = "shared-org-mcp", "prod"
	tr, k8s := managedMCPBundle(server, ns, scopeOrg, "")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(k8s...).Build()
	s, _ := newMCPServerWithIdentity(t, c)
	store := wireTRStore(t, s, nil, tr)

	rec, resp := deleteMCPServer(t, s, ns, server, "bob-token") // not an owner — org has none
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, resp.Deleted, toolRegistryKind)
	_, trErr := store.Get(context.Background(), ns, server)
	assert.ErrorIs(t, trErr, controlplane.ErrNotFound)
}

// TestDeleteMCPServerReportsOrphanedBindings (m26.3): deleting a server reports the
// dependent bindings (RegistryRef == server) it leaves RegistryNotFound — the bindings
// themselves are NOT cascaded (that's the agent's lifecycle, not the server's).
func TestDeleteMCPServerReportsOrphanedBindings(t *testing.T) {
	const server, ns = "dep-mcp", "prod"
	owner := userGrantHash("user:alice-token")
	tr, objs := managedMCPBundle(server, ns, scopePersonal, owner)
	objs = append(objs, &agentsv1alpha1.MCPToolBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-x-tool", Namespace: ns},
		Spec:       agentsv1alpha1.MCPToolBindingSpec{RegistryRef: server, AgentRef: "agent-x", ToolName: "t"},
	})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s, _ := newMCPServerWithIdentity(t, c)
	wireTRStore(t, s, nil, tr)

	rec, resp := deleteMCPServer(t, s, ns, server, "alice-token")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, []string{"agent-x-tool"}, resp.OrphanedBindings)

	// The binding is left in place (now RegistryNotFound), not deleted by the server delete.
	var b agentsv1alpha1.MCPToolBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "agent-x-tool"}, &b))
}

// TestMCPServerReferencesListsDependentBindings (m26.3): the delete-impact preview
// lists the dependent bindings without deleting anything.
func TestMCPServerReferencesListsDependentBindings(t *testing.T) {
	const server, ns = "ref-mcp", "prod"
	tr, objs := managedMCPBundle(server, ns, scopePersonal, userGrantHash("user:alice-token"))
	objs = append(objs,
		&agentsv1alpha1.MCPToolBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "a1-tool", Namespace: ns},
			Spec:       agentsv1alpha1.MCPToolBindingSpec{RegistryRef: server, AgentRef: "a1", ToolName: "t"},
		},
		&agentsv1alpha1.MCPToolBinding{ // a binding on a DIFFERENT registry — not a dependent
			ObjectMeta: metav1.ObjectMeta{Name: "a2-other", Namespace: ns},
			Spec:       agentsv1alpha1.MCPToolBindingSpec{RegistryRef: "some-other", AgentRef: "a2", ToolName: "t"},
		},
	)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	s, _ := newMCPServerWithIdentity(t, c)
	store := wireTRStore(t, s, nil, tr)

	rec, resp := getMCPServerRefs(t, s, ns, server, "alice-token")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, 1, resp.BindingCount)
	assert.Equal(t, "a1-tool", resp.References[0].Name)
	assert.Equal(t, "a1", resp.References[0].AgentRef)

	// Preview does not delete.
	_, err := store.Get(context.Background(), ns, server)
	require.NoError(t, err)
}

// TestMCPServerSummaryScope (m26.5): the server list projects the scope label so the UI
// can show it + gate the org-credential action; an absent label grandfathers to org
// (ADR 0029, visibility only).
func TestMCPServerSummaryScope(t *testing.T) {
	mk := func(scope string) *agentsv1alpha1.ToolRegistry {
		labels := map[string]string{labelManagedBy: managedByMCP}
		if scope != "" {
			labels[labelMCPScope] = scope
		}
		return &agentsv1alpha1.ToolRegistry{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "prod", Labels: labels}}
	}
	assert.Equal(t, scopeOrg, mcpServerSummaryFromRegistry(mk(scopeOrg)).Scope)
	assert.Equal(t, scopePersonal, mcpServerSummaryFromRegistry(mk(scopePersonal)).Scope)
	assert.Equal(t, scopePublic, mcpServerSummaryFromRegistry(mk(scopePublic)).Scope)
	assert.Equal(t, scopeOrg, mcpServerSummaryFromRegistry(mk("")).Scope, "absent label grandfathers to org")
}

// TestDeleteMCPServerUnknownIs404 (m26.3): deleting an unregistered server is a 404.
func TestDeleteMCPServerUnknownIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _ := newMCPServerWithIdentity(t, c)
	wireTRStore(t, s, nil) // empty store ⇒ unknown server
	rec, _ := deleteMCPServer(t, s, "prod", "ghost-mcp", "alice-token")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestDeleteMCPServerNonMCPRegistryIs404 (m26.3): a ToolRegistry that is NOT
// register-managed (no managed-by label) is not a deletable MCP server → 404, so this
// endpoint never deletes an arbitrary/platform ToolRegistry.
func TestDeleteMCPServerNonMCPRegistryIs404(t *testing.T) {
	const server, ns = "platform-registry", "prod"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _ := newMCPServerWithIdentity(t, c)
	// A ToolRegistry with NO managed-by label — not a deletable MCP server.
	store := wireTRStore(t, s, nil, &agentsv1alpha1.ToolRegistry{ObjectMeta: metav1.ObjectMeta{Name: server, Namespace: ns}})

	rec, _ := deleteMCPServer(t, s, ns, server, "alice-token")
	assert.Equal(t, http.StatusNotFound, rec.Code)

	_, err := store.Get(context.Background(), ns, server)
	require.NoError(t, err, "the non-MCP registry must be untouched")
}
