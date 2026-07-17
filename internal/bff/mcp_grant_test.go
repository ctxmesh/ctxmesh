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
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// --- test doubles -----------------------------------------------------------

// identityCallerFactory is a CallerClientFactory whose per-request client answers
// SelfSubjectReview with "user:<token>" — so the consent/revoke handlers resolve a
// stable per-user identity that differs by token. Every client shares the SAME
// backing store (the fake client) so writes/reads are consistent across requests,
// and the SSR interceptor is bound to THIS request's token. extra layers any
// additional interceptors (e.g. a forbidden Delete) the test needs.
type identityCallerFactory struct {
	backing client.WithWatch
	extra   interceptor.Funcs
}

func (f *identityCallerFactory) ForRequest(r *http.Request) (client.Client, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, errUnauthenticated
	}
	funcs := f.extra
	// The identity create wins for SelfSubjectReview; other creates fall through to
	// the test-supplied create (if any) or the backing store.
	base := f.extra.Create
	funcs.Create = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if ssr, ok := obj.(*authnv1.SelfSubjectReview); ok {
			ssr.Status = authnv1.SelfSubjectReviewStatus{UserInfo: authnv1.UserInfo{Username: "user:" + token}}
			return nil
		}
		if base != nil {
			return base(ctx, c, obj, opts...)
		}
		return c.Create(ctx, obj, opts...)
	}
	return interceptor.NewClient(f.backing, funcs), nil
}

func (f *identityCallerFactory) PodLogsForRequest(r *http.Request) (PodLogAccessor, error) {
	return nil, nil
}

// newMCPServerWithIdentity builds an MCP-enabled Server whose caller factory maps a
// bearer token to the username "user:<token>" via SelfSubjectReview, over the given
// backing fake client. Returns the server + a captured log buffer (for leak/audit
// scans). Any extra interceptors are layered onto every per-request client.
func newMCPServerWithIdentity(t *testing.T, c client.WithWatch, extra ...interceptor.Funcs) (*Server, *logBuffer) {
	t.Helper()
	var ex interceptor.Funcs
	if len(extra) > 0 {
		ex = extra[0]
	}
	lb := &logBuffer{}
	log := funcr.New(func(prefix, args string) { lb.write(prefix, args) }, funcr.Options{})
	s := NewServer(Options{
		CallerClients: &identityCallerFactory{backing: c, extra: ex},
		Scheme:        testScheme(t),
		Auth:          AllowAll{},
		MCPEnabled:    true,
		ProviderHTTP:  &http.Client{},
		Version:       "test",
		Log:           log,
	})
	return s, lb
}

// grantNS is the namespace every grant test uses.
const grantNS = "prod"

// seedGrant builds a (user, server)-labeled grant Secret carrying the given OAuth
// tokens, mirroring what the consent callback stores. The tokens live ONLY in Data.
func seedGrant(server, username, access, refresh, tokenEndpoint string, expiry time.Time) *corev1.Secret {
	ns := grantNS
	userHash := userGrantHash(username)
	data := map[string][]byte{
		secretKeyOAuthAccessToken:   []byte(access),
		secretKeyOAuthTokenEndpoint: []byte(tokenEndpoint),
		secretKeyOAuthClientID:      []byte(theOAuthClientID),
	}
	if refresh != "" {
		data[secretKeyOAuthRefreshToken] = []byte(refresh)
	}
	if !expiry.IsZero() {
		data[secretKeyOAuthExpiry] = []byte(expiry.UTC().Format(time.RFC3339))
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      grantSecretName(server, userHash),
			Namespace: ns,
			Labels:    grantSecretLabels(server, userHash, ""),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// oauthToolRegistry is a register-managed ToolRegistry marked as an OAuth server, so
// the resolver's serverIsOAuth check + the consent-begin confirmation pass.
func oauthToolRegistry(server, url string) *agentsv1alpha1.ToolRegistry {
	return &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:        server,
			Namespace:   grantNS,
			Labels:      map[string]string{labelManagedBy: managedByMCP},
			Annotations: map[string]string{annMCPAuthType: oauthAuthType, annMCPURL: url},
		},
	}
}

// TestServerGrantRoutingModes pins the m25.1b mode switch: the write/read/delete paths
// route to the locked credential namespace via the privileged client only when BOTH a
// namespace and a client are wired, and stay on the legacy caller-scoped per-namespace
// path otherwise (fail-safe: a namespace without a client does NOT enter locked mode).
func TestServerGrantRoutingModes(t *testing.T) {
	caller := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	cred := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	locked := &Server{credentialNamespace: "ae-credentials", credentialClient: cred}
	require.True(t, locked.lockedCredentials())
	ns, name := locked.grantCoordinates("prod", "gh", "u-abc")
	assert.Equal(t, "ae-credentials", ns, "locked: grant lands in the credential namespace")
	assert.NotEqual(t, grantSecretName("gh", "u-abc"), name, "locked: name folds the source ns")
	assert.True(t, locked.grantClient(caller) == cred, "locked: writes go through the privileged client")
	assert.Equal(t, "prod", locked.grantSourceNSLabel("prod"))

	legacy := &Server{}
	require.False(t, legacy.lockedCredentials())
	lns, lname := legacy.grantCoordinates("prod", "gh", "u-abc")
	assert.Equal(t, "prod", lns, "legacy: grant stays in the source namespace")
	assert.Equal(t, grantSecretName("gh", "u-abc"), lname, "legacy: original name")
	assert.True(t, legacy.grantClient(caller) == caller, "legacy: caller-scoped write")
	assert.Empty(t, legacy.grantSourceNSLabel("prod"))

	half := &Server{credentialNamespace: "ae-credentials"}
	assert.False(t, half.lockedCredentials(), "a namespace without a privileged client must stay legacy (fail-safe)")
}

// TestGrantTokensNeverInLabelsOrDTO is the leak scan: the stored grant carries the
// tokens ONLY in Data — never in a label, an annotation, or the resolved
// credential's non-token fields.
func TestGrantTokensNeverInLabelsOrDTO(t *testing.T) {
	const server, ns = "leak-mcp", "prod"
	grant := seedGrant(server, "frank@example.com", theOAuthAccessToken, theOAuthRefreshToken, "https://x/token", time.Now().Add(time.Hour))

	for k, v := range grant.Labels {
		assert.NotContains(t, v, theOAuthAccessToken, "label %s must not carry the access token", k)
		assert.NotContains(t, v, theOAuthRefreshToken, "label %s must not carry the refresh token", k)
	}
	for k, v := range grant.Annotations {
		assert.NotContains(t, v, theOAuthAccessToken, "annotation %s must not carry the access token", k)
		assert.NotContains(t, v, theOAuthRefreshToken, "annotation %s must not carry the refresh token", k)
	}
	// The user label is the HASH — never the raw username.
	assert.NotContains(t, grant.Labels[labelMCPGrantUser], "frank@example.com", "the user label must be a hash, not the raw username")
	assert.Equal(t, userGrantHash("frank@example.com"), grant.Labels[labelMCPGrantUser])
}

// --- consent flow (stores a (user, server) grant) ---------------------------

// grantAuth builds the OAuth client config for a consent request against the fake.
func grantAuth(o *fakeOAuthServer) *MCPAuthRequest { return authFor(o) }

// beginGrant posts a consent-begin for the given server/user token and returns the
// pending response.
func beginGrant(t *testing.T, s *Server, server, userToken string, auth *MCPAuthRequest) (*httptest.ResponseRecorder, OAuthPendingResponse) {
	t.Helper()
	body, err := json.Marshal(MCPGrantConsentRequest{Server: server, Namespace: "prod", Auth: auth})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/oauth/grant", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+userToken)
	s.Handler().ServeHTTP(rec, req)
	var pending OAuthPendingResponse
	if rec.Code == http.StatusAccepted {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pending))
	}
	return rec, pending
}

// newGrantServer builds a Server whose caller factory answers SelfSubjectReview with
// a username derived from the bearer token (so consent/revoke resolve a per-user
// identity), backed by the given fake client + a captured log buffer.
func newGrantServer(t *testing.T, c client.WithWatch, extra ...interceptor.Funcs) (*Server, *logBuffer) {
	t.Helper()
	return newMCPServerWithIdentity(t, c, extra...)
}

// TestMCPGrantConsentStoresPerUserGrant drives the full consent flow: begin →
// authorization URL + state, callback with a valid code → a (user, server) grant
// Secret with the token in Data, labeled by the hashed user — the token in NO
// label/DTO/log.
func TestMCPGrantConsentStoresPerUserGrant(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	const server, ns = "grant-mcp", "prod"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(oauthToolRegistry(server, "http://grant/mcp")).Build()
	s, lb := newGrantServer(t, c)

	rec, pending := beginGrant(t, s, server, "alice-token", grantAuth(oauth))
	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "authorization_required", pending.Status)
	require.NotEmpty(t, pending.State)
	oauth.expectChallenge = "" // pass PKCE; assert grant storage

	// The callback is browser-facing → 303 back to the console catalog, not JSON.
	crec := callback(t, s, oauth.validCode, pending.State)
	q := assertCallbackRedirect(t, crec, "/tools/catalog")
	assert.Equal(t, server, q.Get("mcp_connected"), "the redirect names the consented server")

	// The grant Secret exists, labeled (user, server), with the token ONLY in Data.
	var grant corev1.Secret
	name := grantSecretName(server, userGrantHash("user:alice-token"))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, &grant))
	assert.Equal(t, theOAuthAccessToken, string(grant.Data[secretKeyOAuthAccessToken]))
	assert.Equal(t, theOAuthRefreshToken, string(grant.Data[secretKeyOAuthRefreshToken]))
	assert.Equal(t, userGrantHash("user:alice-token"), grant.Labels[labelMCPGrantUser])
	assert.Equal(t, server, grant.Labels[labelMCPGrantServer])

	// Leak scan: token in NO label/annotation, the DTO, or any log line.
	for k, v := range grant.Labels {
		assert.NotContains(t, v, theOAuthAccessToken, "label %s", k)
		assert.NotContains(t, v, theOAuthRefreshToken, "label %s", k)
	}
	for k, v := range grant.Annotations {
		assert.NotContains(t, v, theOAuthAccessToken, "annotation %s", k)
	}
	assertNoOAuthSecretsInBody(t, crec.Body.String())
	assert.NotContains(t, lb.String(), theOAuthAccessToken, "no token in any log line")
	assert.NotContains(t, lb.String(), theOAuthRefreshToken, "no refresh token in any log line")
	// The audit log records the create WITHOUT the token.
	assert.Contains(t, lb.String(), string(grantActionCreate), "grant.create must be audited")
	assert.Contains(t, lb.String(), userGrantHash("user:alice-token"), "the audit carries the hashed user")
}

// TestMCPGrantConsentNonOAuthServerIs4xx proves consent for a server that is not an
// OAuth server is rejected — no grant is started for a server that cannot use one.
func TestMCPGrantConsentNonOAuthServerIs4xx(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	const server, ns = "plain-mcp", "prod"
	tr := &agentsv1alpha1.ToolRegistry{
		ObjectMeta: metav1.ObjectMeta{
			Name:        server,
			Namespace:   ns,
			Annotations: map[string]string{annMCPURL: "http://plain/mcp"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(tr).Build()
	s, _ := newGrantServer(t, c)

	rec, _ := beginGrant(t, s, server, "alice-token", grantAuth(oauth))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestMCPGrantConsentUnknownServerIs404 proves consent for an unregistered server is
// a 404.
func TestMCPGrantConsentUnknownServerIs404(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s, _ := newGrantServer(t, c)

	rec, _ := beginGrant(t, s, "ghost-mcp", "alice-token", grantAuth(oauth))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// oauthToolRegistryWithConfig builds an OAuth ToolRegistry carrying the persisted OAuth
// client config annotations (ADR 0031), so a grant can be begun from just {server, ns}.
func oauthToolRegistryWithConfig(server, url string, o *fakeOAuthServer) *agentsv1alpha1.ToolRegistry {
	tr := oauthToolRegistry(server, url)
	tr.Annotations[annMCPOAuthAuthEndpoint] = o.authorizeURL()
	tr.Annotations[annMCPOAuthTokenEndpoint] = o.tokenURL()
	tr.Annotations[annMCPOAuthClientID] = "recovered-client-id"
	tr.Annotations[annMCPOAuthScope] = "read"
	tr.Annotations[annMCPOAuthRedirectURI] = "https://console.example/api/mcp/oauth/callback"
	return tr
}

// TestMCPGrantConsentRecoversConfigFromRegistration (m26.1, ADR 0031): a caller begins
// consent with NO auth block — the BFF recovers the server's OAuth client config from the
// register-time annotations, so the Playground never has to supply OAuth config. The
// authorization URL is built from the RECOVERED endpoint + client id.
func TestMCPGrantConsentRecoversConfigFromRegistration(t *testing.T) {
	oauth := newFakeOAuthServer(t)
	const server = "recover-mcp"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(oauthToolRegistryWithConfig(server, "http://recover/mcp", oauth)).Build()
	s, _ := newGrantServer(t, c)

	rec, pending := beginGrant(t, s, server, "bob-token", nil) // no auth block — recovery only
	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "authorization_required", pending.Status)
	require.NotEmpty(t, pending.State)
	assert.Contains(t, pending.AuthorizationURL, oauth.authorizeURL(), "authorize URL from the recovered endpoint")
	assert.Contains(t, pending.AuthorizationURL, "recovered-client-id", "authorize URL carries the recovered client id")
}

// TestMCPGrantConsentLegacyServerNeedsConfig (m26.1): a legacy OAuth server registered
// before config persistence has no recoverable config; beginning consent with no auth
// block fails validation honestly rather than starting a broken flow. Supplying the config
// (overlay) still works — covered by TestMCPGrantConsentStoresPerUserGrant.
func TestMCPGrantConsentLegacyServerNeedsConfig(t *testing.T) {
	const server = "legacy-oauth-mcp"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(oauthToolRegistry(server, "http://legacy/mcp")).Build() // no OAuth config annotations
	s, _ := newGrantServer(t, c)

	rec, _ := beginGrant(t, s, server, "alice-token", nil) // nothing to recover, nothing supplied
	assert.Equal(t, http.StatusBadRequest, rec.Code, "no recoverable + no supplied config → honest 400")
}

// TestCreateMCPObjectsPersistsOAuthConfig (m26.1, ADR 0031): an OAuth registration stamps
// the discovered OAuth client config as NON-SECRET annotations on the ToolRegistry (so a
// per-user grant can later be begun from {server, ns}) — and no token material leaks into
// any annotation.
func TestCreateMCPObjectsPersistsOAuthConfig(t *testing.T) {
	const server, ns = "cfg-mcp", "prod"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	_, cErr := createMCPObjects(context.Background(), c, testScheme(t), mcpCreateSpec{
		name: server, namespace: ns, url: "http://10.0.0.5:8080/mcp", status: "approved",
		authType:        oauthAuthType,
		oauthSecretData: map[string][]byte{secretKeyOAuthAccessToken: []byte(theOAuthAccessToken)},
		oauthConfig: mcpOAuthConfig{
			AuthorizationEndpoint: "https://as.example/authorize",
			TokenEndpoint:         "https://as.example/token",
			ClientID:              "cfg-client",
			Scope:                 "read write",
			RedirectURI:           "https://console.example/api/mcp/oauth/callback",
		},
	})
	require.Nil(t, cErr)

	var tr agentsv1alpha1.ToolRegistry
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: server, Namespace: ns}, &tr))
	assert.Equal(t, "https://as.example/authorize", tr.Annotations[annMCPOAuthAuthEndpoint])
	assert.Equal(t, "https://as.example/token", tr.Annotations[annMCPOAuthTokenEndpoint])
	assert.Equal(t, "cfg-client", tr.Annotations[annMCPOAuthClientID])
	assert.Equal(t, "read write", tr.Annotations[annMCPOAuthScope])
	assert.Equal(t, "https://console.example/api/mcp/oauth/callback", tr.Annotations[annMCPOAuthRedirectURI])
	// The access token lands ONLY in the Secret, never an annotation.
	for k, v := range tr.Annotations {
		assert.NotContains(t, v, theOAuthAccessToken, "annotation %s must not carry token material", k)
	}
}

// --- revocation -------------------------------------------------------------

// revokeGrant issues DELETE /api/mcp/oauth/grant/{server} as the given user token.
func revokeGrant(t *testing.T, s *Server, server, userToken string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/mcp/oauth/grant/"+server+"?namespace=prod", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestMCPGrantRevokeThenConsentRequired proves a user revoking their grant makes the
// next resolve for that (user, server) return consent-required (honest re-consent).
func TestMCPGrantRevokeThenConsentRequired(t *testing.T) {
	const server = "revoke-mcp"
	// "user:alice-token" is what the identity factory reports for "alice-token".
	grant := seedGrant(server, "user:alice-token", theOAuthAccessToken, theOAuthRefreshToken, "https://x/token", time.Now().Add(time.Hour))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(oauthToolRegistry(server, "http://revoke/mcp"), grant).Build()
	s, lb := newGrantServer(t, c)

	// Alice revokes HER grant.
	rec := revokeGrant(t, s, server, "alice-token")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, lb.String(), string(grantActionRevoke), "grant.revoke must be audited")

	// The grant Secret is gone → the next OBO resolve for that (user, server) is
	// consent-required (re-consent). Resolution itself now lives in internal/credresolve.
	var gone corev1.Secret
	getErr := c.Get(context.Background(), client.ObjectKeyFromObject(grant), &gone)
	assert.True(t, apierrors.IsNotFound(getErr), "the grant Secret must be deleted")
}

// TestMCPGrantRevokeOnlyOwnGrant proves a user cannot revoke ANOTHER user's grant:
// Bob's DELETE targets HIS (user, server) name — never Alice's — so Alice's grant
// survives and Bob gets a 404 (he has none).
func TestMCPGrantRevokeOnlyOwnGrant(t *testing.T) {
	const server, ns = "shared-server", "prod"
	aliceGrant := seedGrant(server, "user:alice-token", "ALICE-token", "a-refresh", "https://x/token", time.Now().Add(time.Hour))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(oauthToolRegistry(server, "http://shared/mcp"), aliceGrant).Build()
	s, _ := newGrantServer(t, c)

	// Bob (who has NO grant) tries to revoke — his DELETE names HIS own grant object
	// (which does not exist), so it is a 404 and Alice's grant is untouched.
	rec := revokeGrant(t, s, server, "bob-token")
	assert.Equal(t, http.StatusNotFound, rec.Code, "bob has no grant of his own to revoke")

	// Alice's grant is intact — Bob could not name it.
	var stillThere corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(aliceGrant), &stillThere))
	assert.Equal(t, "ALICE-token", string(stillThere.Data[secretKeyOAuthAccessToken]))
}

// TestMCPGrantRevokeForbiddenIs403 proves a caller whose RBAC denies deleting the
// grant Secret surfaces a 403 (caller-scoped, ADR 0011) — not a swallowed success.
func TestMCPGrantRevokeForbiddenIs403(t *testing.T) {
	const server, ns = "rbac-mcp", "prod"
	grant := seedGrant(server, "user:viewer-token", theOAuthAccessToken, "", "", time.Now().Add(time.Hour))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(oauthToolRegistry(server, "http://rbac/mcp"), grant).Build()
	// SelfSubjectReview still succeeds (the factory handles it); only the grant
	// Secret DELETE is denied — the caller-scoped 403 must surface.
	s, _ := newGrantServer(t, c, interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, grant.Name, assert.AnError)
		},
	})

	rec := revokeGrant(t, s, server, "viewer-token")
	assert.Equal(t, http.StatusForbidden, rec.Code, "a denied delete must surface a 403")
}

// TestUserGrantHashHMAC pins the m25.1 security property (ADR 0029 §7, advisor R5):
// with a per-cluster key the user-identity hash is HMAC-SHA256 — salted by the key so
// it cannot be confirmed offline against a low-entropy email/username — while staying
// deterministic for lookup; without a key it degrades to the legacy unsalted SHA-256
// so already-hardened callers and dev clusters both keep working. The hash is one-way
// and fixed-format regardless, so a raw username never lands in cluster metadata.
func TestUserGrantHashHMAC(t *testing.T) {
	// grantHMACKey is package-global; restore the default (nil ⇒ legacy) for every
	// other test in the package, whatever this one leaves it at.
	defer setGrantHMACKey(nil)

	const user = "alice@example.com"

	setGrantHMACKey(nil)
	legacy := userGrantHash(user)
	assert.Equal(t, legacy, userGrantHash(user), "no key: deterministic")
	assert.Regexp(t, `^u-[0-9a-f]{40}$`, legacy, "fixed one-way format, no raw username")

	setGrantHMACKey([]byte("cluster-key-one"))
	k1 := userGrantHash(user)
	assert.Regexp(t, `^u-[0-9a-f]{40}$`, k1)
	assert.NotEqual(t, legacy, k1, "a key must salt the hash away from the unsalted digest")
	assert.Equal(t, k1, userGrantHash(user), "same key + user: deterministic (lookup stable)")

	setGrantHMACKey([]byte("cluster-key-two"))
	assert.NotEqual(t, k1, userGrantHash(user), "a different cluster key must yield a different hash")

	setGrantHMACKey([]byte("cluster-key-one"))
	assert.Equal(t, k1, userGrantHash(user), "the same key reproduces the same hash (re-key = re-consent, not drift)")
	assert.NotEqual(t, k1, userGrantHash("bob@example.com"), "distinct users stay distinct under one key")
}

// TestGrantSecretCoordinates pins the m25.1a key shape (ADR 0029 §7): legacy mode is
// byte-for-byte the pre-m25.1 (namespace, name) so nothing migrates until a locked
// credential namespace is configured; locked mode consolidates every grant into that
// one namespace while folding the source namespace into the object name so grants
// from different namespaces never collide there.
func TestGrantSecretCoordinates(t *testing.T) {
	const server, hash = "gh", "u-abcdef0123456789"

	// Legacy (no credential namespace): unchanged (source ns, original name).
	ns, name := grantSecretCoordinates("", "team-a", server, hash)
	assert.Equal(t, "team-a", ns, "legacy: grant stays in its source namespace")
	assert.Equal(t, grantSecretName(server, hash), name, "legacy: original name, no migration")

	// Locked: the credential namespace, source ns folded into the name.
	lockedNS, lockedName := grantSecretCoordinates("ae-credentials", "team-a", server, hash)
	assert.Equal(t, "ae-credentials", lockedNS, "locked: all grants land in the credential namespace")
	assert.True(t, strings.HasPrefix(lockedName, grantSecretName(server, hash)+"-"),
		"locked: name extends the legacy base with the source-ns hash")
	assert.LessOrEqual(t, len(lockedName), 253, "object name stays within the k8s limit")

	// Same (server, user) from a DIFFERENT namespace → a distinct object (no collision).
	_, otherName := grantSecretCoordinates("ae-credentials", "team-b", server, hash)
	assert.NotEqual(t, lockedName, otherName, "locked: different source namespaces never collide")

	// A different server in the same namespace is also a distinct object.
	_, otherServer := grantSecretCoordinates("ae-credentials", "team-a", "jira", hash)
	assert.NotEqual(t, lockedName, otherServer, "locked: different servers never collide")

	// A different user (hash) for the same server + namespace is a distinct object too.
	_, otherUser := grantSecretCoordinates("ae-credentials", "team-a", server, "u-999888777666")
	assert.NotEqual(t, lockedName, otherUser, "locked: different users never collide")

	// Deterministic (lookup must be stable across write/read/refresh).
	_, again := grantSecretCoordinates("ae-credentials", "team-a", server, hash)
	assert.Equal(t, lockedName, again, "locked: coordinates are deterministic")

	// The source namespace is the authoritative label match key in locked mode.
	assert.Equal(t, "team-a", grantSecretLabels(server, hash, "team-a")[labelMCPGrantSourceNS])
	_, hasNS := grantSecretLabels(server, hash, "")[labelMCPGrantSourceNS]
	assert.False(t, hasNS, "legacy labels carry no source-namespace")
}
