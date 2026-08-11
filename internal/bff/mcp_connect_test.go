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

// Tests for POST /api/mcp/connect (m73.6, ADR 0067 §3) — discover-then-materialize.
//
// What we cover:
//   (a) THE CRUX — materializing an OAuth origin creates a local copy with the
//       url/tools/oauthConfig + provenance labels + credentialSource=byo-oauth +
//       visibility=private, and NO Secret carrying token/key material is created for
//       the copy (the publisher's token never crosses the namespace boundary).
//   (b) the security gate — public materializes; org-in-tenant materializes; org
//       OUTSIDE the tenant → 404; private in another ns → 404; own-ns → allowed.
//   (c) idempotent re-connect — a second Connect of the same name → 200 already-connected.

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane"
	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

// callerConnectNS is the caller's own namespace in these tests (where copies land).
const callerConnectNS = "consumer-ns"

// newMCPConnectServer wires a BFF Server for the connect handler tests: mcpEnabled, an
// identity caller-client factory (so callerUsername resolves + the K8s creates in
// createMCPObjects run on a scheme-aware backing client), the toolRegistryStore, and
// an optional namespaceTenantStore. It returns the server and the store (seeded with
// the given origin registries), plus the backing client so tests can assert what
// K8s objects were (not) created.
func newMCPConnectServer(t *testing.T, nsStore namespacetenant.Store, origins ...*agentsv1alpha1.ToolRegistry) (*Server, toolregistry.Store, client.Client) {
	t.Helper()
	// The backing fake client is what createMCPObjects' K8s creates (Secret / NetworkPolicy)
	// land on — tests inspect it to assert NO token Secret was written for the copy. We stub
	// the DNS resolver so the egress NetworkPolicy build is deterministic (URLs are IP literals).
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	stubResolver(t, func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("192.0.2.10")}, nil })

	store := toolregistry.NewMemStore()
	for _, o := range origins {
		_, err := store.Upsert(context.Background(), crdToolRegistryToStore(o))
		require.NoError(t, err)
	}

	s := NewServer(Options{
		CallerClients:        &identityCallerFactory{backing: c},
		Scheme:               testScheme(t),
		Auth:                 AllowAll{},
		MCPEnabled:           true,
		ToolRegistryStore:    store,
		NamespaceTenantStore: nsStore,
		Version:              "test",
		Log:                  logr.Discard(),
	})
	// Permissive SSAR so createMCPObjects' caller-scoped store-create authz passes;
	// the discoverability gate is what these tests exercise, not RBAC.
	s.authorizer = &recordingAuthorizer{}
	return s, store, c
}

// connectReq builds a POST /api/mcp/connect request.
func connectReq(t *testing.T, body MCPConnectRequest) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/connect", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer alice")
	return req
}

// oauthOrigin builds a discoverable OAuth origin ToolRegistry with the given visibility,
// carrying a URL, one tool with an inputSchema, the OAuth auth-type annotation, the
// non-secret OAuth CLIENT config annotations, and — critically — NO secret reference of
// its own that connect would ever read (connect never touches a Secret regardless).
func oauthOrigin(ns, name, visibility string) *agentsv1alpha1.ToolRegistry {
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				labelManagedBy:           managedByMCP,
				labelMCPVisibility:       visibility,
				labelMCPCredentialSource: credSourceByoOAuth,
				labelMCPOwner:            userGrantHash("publisher@example.com"),
			},
			Annotations: map[string]string{
				annMCPURL:                "https://192.0.2.20/mcp",
				annMCPAuthType:           oauthAuthType,
				annMCPStatus:             agentsv1alpha1.ApprovalApproved,
				annMCPOAuthAuthEndpoint:  "https://auth.example.com/authorize",
				annMCPOAuthTokenEndpoint: "https://auth.example.com/token",
				annMCPOAuthClientID:      "publisher-client-id",
				annMCPOAuthScope:         "read:tools",
				annMCPOAuthRedirectURI:   "https://console.example/api/mcp/oauth/callback",
			},
		},
		Spec: agentsv1alpha1.ToolRegistrySpec{
			Tools: []agentsv1alpha1.ToolEntry{{
				Name:        "search",
				Description: "search the corpus",
				URL:         "https://192.0.2.20/mcp",
				InputSchema: rawExt(`{"type":"object"}`),
			}},
		},
	}
}

// publicOrigin builds a public no-auth origin.
func publicOrigin(ns, name string) *agentsv1alpha1.ToolRegistry {
	o := oauthOrigin(ns, name, visibilityPublic)
	o.Labels[labelMCPCredentialSource] = credSourceNone
	delete(o.Annotations, annMCPAuthType)
	return o
}

// doConnect serves the connect request and returns the recorder.
func doConnect(t *testing.T, s *Server, body MCPConnectRequest) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, connectReq(t, body))
	return rec
}

// -----------------------------------------------------------------------
// (a) THE CRUX — no credential crosses the boundary
// -----------------------------------------------------------------------

// TestMCPConnect_OAuthOrigin_CopyHasNoCredential is the credential-safety crux: an
// OAuth origin materializes a local copy carrying the url/tools/oauthConfig +
// provenance + credentialSource=byo-oauth + visibility=private, and NO Secret with
// token/key data is created for the copy.
func TestMCPConnect_OAuthOrigin_CopyHasNoCredential(t *testing.T) {
	ctx := context.Background()
	// The publisher's server is org-visible in a sibling namespace within the caller's tenant.
	nsStore := namespacetenant.NewMemStore()
	require.NoError(t, nsStore.SetMembers(ctx, "acme", []string{callerConnectNS, "publisher-ns"}))
	origin := oauthOrigin("publisher-ns", "vendor-mcp", visibilityOrg)
	s, store, back := newMCPConnectServer(t, nsStore, origin)

	rec := doConnect(t, s, MCPConnectRequest{
		OriginNamespace: "publisher-ns",
		OriginName:      "vendor-mcp",
		Namespace:       callerConnectNS,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp MCPConnectResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "connected", resp.Status)
	assert.Equal(t, callerConnectNS, resp.Server.Namespace)

	// The copy exists in the caller's namespace with the origin's NON-secret definition.
	copyRec, err := store.Get(ctx, callerConnectNS, "vendor-mcp")
	require.NoError(t, err)
	copy := storeToolRegistryToCRD(copyRec)
	assert.Equal(t, "https://192.0.2.20/mcp", copy.Annotations[annMCPURL], "url copied")
	assert.Equal(t, oauthAuthType, copy.Annotations[annMCPAuthType], "authType copied")
	require.Len(t, copy.Spec.Tools, 1, "tools copied")
	assert.Equal(t, "search", copy.Spec.Tools[0].Name)
	require.NotNil(t, copy.Spec.Tools[0].InputSchema, "tool inputSchema copied verbatim")

	// The OAuth CLIENT config (non-secret) is carried so the consumer can begin their own grant.
	assert.Equal(t, "https://auth.example.com/authorize", copy.Annotations[annMCPOAuthAuthEndpoint])
	assert.Equal(t, "https://auth.example.com/token", copy.Annotations[annMCPOAuthTokenEndpoint])
	assert.Equal(t, "publisher-client-id", copy.Annotations[annMCPOAuthClientID])
	assert.Equal(t, "read:tools", copy.Annotations[annMCPOAuthScope])

	// The two-axis labels: the copy is the consumer's OWN private byo-oauth server.
	assert.Equal(t, visibilityPrivate, copy.Labels[labelMCPVisibility], "copy is private")
	assert.Equal(t, credSourceByoOAuth, copy.Labels[labelMCPCredentialSource], "oauth ⇒ byo-oauth")

	// Provenance labels stamp where it came from (frozen one-time copy).
	assert.Equal(t, "publisher-ns", copy.Labels[labelMCPOriginNamespace])
	assert.Equal(t, "vendor-mcp", copy.Labels[labelMCPOriginName])

	// The owner is the CALLER (their token-derived hash), not the publisher.
	assert.Equal(t, userGrantHash("user:alice"), copy.Labels[labelMCPOwner], "owner is the caller, not the publisher")
	assert.NotEqual(t, userGrantHash("publisher@example.com"), copy.Labels[labelMCPOwner])

	// THE CRUX ASSERTION: no Secret carrying token/key material was created for the copy.
	// The copy carries no annMCPSecret reference, and no Secret named after the copy exists.
	assert.Empty(t, copy.Annotations[annMCPSecret], "copy references NO Secret — no credential materialized")
	var sec corev1.Secret
	getErr := back.Get(ctx, client.ObjectKey{Namespace: callerConnectNS, Name: "vendor-mcp"}, &sec)
	assert.True(t, apierrors.IsNotFound(getErr), "NO Secret must exist for the copy — the publisher's token never crosses the boundary; got err=%v", getErr)

	// And no Secret at all was created by this flow — connect never writes credential material.
	var secList corev1.SecretList
	require.NoError(t, back.List(ctx, &secList))
	assert.Empty(t, secList.Items, "connect must create NO Secret — the crux (no credential crosses the namespace boundary)")
}

// -----------------------------------------------------------------------
// (b) the security gate
// -----------------------------------------------------------------------

// TestMCPConnect_PublicOrigin_Materializes: a public origin in an unrelated namespace
// is discoverable and materializes.
func TestMCPConnect_PublicOrigin_Materializes(t *testing.T) {
	origin := publicOrigin("unrelated-ns", "open-mcp")
	s, store, _ := newMCPConnectServer(t, namespacetenant.NewMemStore(), origin)

	rec := doConnect(t, s, MCPConnectRequest{OriginNamespace: "unrelated-ns", OriginName: "open-mcp", Namespace: callerConnectNS})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	copyRec, err := store.Get(context.Background(), callerConnectNS, "open-mcp")
	require.NoError(t, err)
	assert.Equal(t, visibilityPrivate, copyRec.Labels[labelMCPVisibility])
	assert.Equal(t, credSourceNone, copyRec.Labels[labelMCPCredentialSource], "no-auth origin ⇒ none")
}

// TestMCPConnect_OrgInTenant_Materializes: an org origin in a SIBLING namespace within
// the caller's tenant is discoverable and materializes.
func TestMCPConnect_OrgInTenant_Materializes(t *testing.T) {
	ctx := context.Background()
	nsStore := namespacetenant.NewMemStore()
	require.NoError(t, nsStore.SetMembers(ctx, "acme", []string{callerConnectNS, "sibling-ns"}))
	origin := oauthOrigin("sibling-ns", "org-mcp", visibilityOrg)
	s, store, _ := newMCPConnectServer(t, nsStore, origin)

	rec := doConnect(t, s, MCPConnectRequest{OriginNamespace: "sibling-ns", OriginName: "org-mcp", Namespace: callerConnectNS})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err := store.Get(ctx, callerConnectNS, "org-mcp")
	require.NoError(t, err)
}

// TestMCPConnect_OrgOutsideTenant_404: an org origin OUTSIDE the caller's tenant is NOT
// discoverable → 404 (never 403 — do not confirm existence).
func TestMCPConnect_OrgOutsideTenant_404(t *testing.T) {
	ctx := context.Background()
	nsStore := namespacetenant.NewMemStore()
	// The caller's tenant does NOT include foreign-ns.
	require.NoError(t, nsStore.SetMembers(ctx, "acme", []string{callerConnectNS}))
	origin := oauthOrigin("foreign-ns", "far-mcp", visibilityOrg)
	s, store, _ := newMCPConnectServer(t, nsStore, origin)

	rec := doConnect(t, s, MCPConnectRequest{OriginNamespace: "foreign-ns", OriginName: "far-mcp", Namespace: callerConnectNS})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	// Nothing materialized.
	_, err := store.Get(ctx, callerConnectNS, "far-mcp")
	assert.Error(t, err, "an undiscoverable origin must not materialize")
}

// TestMCPConnect_PrivateOtherNS_404: a private origin in another namespace is never
// discoverable → 404 (even guessing the exact ns+name must not leak it).
func TestMCPConnect_PrivateOtherNS_404(t *testing.T) {
	ctx := context.Background()
	nsStore := namespacetenant.NewMemStore()
	require.NoError(t, nsStore.SetMembers(ctx, "acme", []string{callerConnectNS, "publisher-ns"}))
	origin := oauthOrigin("publisher-ns", "secret-mcp", visibilityPrivate)
	s, _, _ := newMCPConnectServer(t, nsStore, origin)

	rec := doConnect(t, s, MCPConnectRequest{OriginNamespace: "publisher-ns", OriginName: "secret-mcp", Namespace: callerConnectNS})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// TestMCPConnect_OwnNamespace_Materializes: a server in the caller's OWN namespace is
// always discoverable (even private), so re-materializing under a new name is allowed.
func TestMCPConnect_OwnNamespace_Materializes(t *testing.T) {
	ctx := context.Background()
	origin := oauthOrigin(callerConnectNS, "mine", visibilityPrivate)
	s, store, _ := newMCPConnectServer(t, namespacetenant.NewMemStore(), origin)

	rec := doConnect(t, s, MCPConnectRequest{OriginNamespace: callerConnectNS, OriginName: "mine", Name: "mine-copy", Namespace: callerConnectNS})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err := store.Get(ctx, callerConnectNS, "mine-copy")
	require.NoError(t, err)
}

// TestMCPConnect_UnknownOrigin_404: a non-existent origin → 404 (folded into the
// undiscoverable case; existence is never confirmed).
func TestMCPConnect_UnknownOrigin_404(t *testing.T) {
	s, _, _ := newMCPConnectServer(t, namespacetenant.NewMemStore())
	rec := doConnect(t, s, MCPConnectRequest{OriginNamespace: "nowhere", OriginName: "ghost", Namespace: callerConnectNS})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestMCPConnect_MissingFields_400: missing originNamespace/originName → 400.
func TestMCPConnect_MissingFields_400(t *testing.T) {
	s, _, _ := newMCPConnectServer(t, namespacetenant.NewMemStore())
	rec := doConnect(t, s, MCPConnectRequest{OriginName: "x", Namespace: callerConnectNS})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = doConnect(t, s, MCPConnectRequest{OriginNamespace: "ns", Namespace: callerConnectNS})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// -----------------------------------------------------------------------
// (c) idempotent re-connect
// -----------------------------------------------------------------------

// TestMCPConnect_Idempotent: connecting the same origin twice → the second call returns
// 200 "already-connected" with the existing summary (not a 500, not a scary error).
func TestMCPConnect_Idempotent(t *testing.T) {
	origin := publicOrigin("unrelated-ns", "open-mcp")
	s, store, _ := newMCPConnectServer(t, namespacetenant.NewMemStore(), origin)

	body := MCPConnectRequest{OriginNamespace: "unrelated-ns", OriginName: "open-mcp", Namespace: callerConnectNS}
	rec1 := doConnect(t, s, body)
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())

	rec2 := doConnect(t, s, body)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	var resp MCPConnectResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	assert.Equal(t, "already-connected", resp.Status)
	assert.Equal(t, "open-mcp", resp.Server.Name)

	// Exactly one copy exists (re-connect did not create a duplicate).
	page, err := store.List(context.Background(), controlplane.ListOptions{Namespace: callerConnectNS})
	require.NoError(t, err)
	count := 0
	for i := range page.Items {
		if page.Items[i].Name == "open-mcp" {
			count++
		}
	}
	assert.Equal(t, 1, count, "re-connect must not duplicate the copy")
}
