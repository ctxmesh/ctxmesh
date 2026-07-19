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

package credresolve

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testNS = "team-alpha"

var fixedNow = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(sch))
	return sch
}

// testServer / testUser are the server + granted user every backend test uses; bob is
// spelled out inline where a second (ungranted) user matters for isolation.
const (
	testServer = "weather"
	testUser   = "alice@example.com"
)

// grantSecret builds a legacy-mode (source-namespace) grant Secret for testUser on testServer.
func grantSecret(data map[string][]byte) *corev1.Secret {
	userHash := UserHash(nil, testUser)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(testServer, userHash, ""),
			Namespace: testNS,
			Labels:    SecretLabels(testServer, userHash, "", ""),
		},
		Data: data,
	}
}

// oauthData assembles grant-Secret token data with the given access token + expiry.
func oauthData(access, refresh string, expiry time.Time) map[string][]byte {
	toks := Tokens{AccessToken: access, RefreshToken: refresh, ExpiresAt: expiry}
	return SecretData(OAuthConfig{TokenEndpoint: "https://as.example/token", ClientID: "cid"}, toks)
}

// fakeExchanger is a counting, optionally-blocking TokenExchanger.
type fakeExchanger struct {
	mu           sync.Mutex
	refreshCalls int
	revokeCalls  int
	revokedTok   string
	next         Tokens
	err          error

	entered   chan struct{} // closed on the first Refresh entry (singleflight test)
	release   chan struct{} // Refresh blocks until closed (nil ⇒ no block)
	enterOnce sync.Once
}

func (f *fakeExchanger) Refresh(_ context.Context, _, _, _ string) (Tokens, error) {
	f.mu.Lock()
	f.refreshCalls++
	f.mu.Unlock()
	if f.entered != nil {
		f.enterOnce.Do(func() { close(f.entered) })
	}
	if f.release != nil {
		<-f.release
	}
	return f.next, f.err
}

func (f *fakeExchanger) Revoke(_ context.Context, _, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeCalls++
	f.revokedTok = token
	return nil
}

func (f *fakeExchanger) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls
}

// backendWith builds a legacy-mode K8sBackend over the given objects, with a fixed clock
// and an OAuth auth-type lookup, returning the backend + the underlying client.
func backendWith(t *testing.T, ex TokenExchanger, isOAuth bool, objs ...client.Object) (*K8sBackend, client.Client) {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	b := NewK8sBackend(K8sBackendConfig{
		Client:          cl,
		Exchanger:       ex,
		Now:             func() time.Time { return fixedNow },
		AuthTypeIsOAuth: func(context.Context, string, string) (bool, error) { return isOAuth, nil },
	})
	return b, cl
}

// TestResolveLegacyBoundaryFallback: the migration bridge (ADR 0033, m30.6) on the default K8s
// backend — a boundary-scoped resolve with no scoped grant falls back to the user's legacy
// unscoped grant, so connected accounts keep working before every write is boundary-scoped.
func TestResolveLegacyBoundaryFallback(t *testing.T) {
	ctx := context.Background()
	// Only a LEGACY (unscoped, boundary "") grant exists — as grantSecret builds.
	legacy := grantSecret(oauthData("LEGACY-AT", "LEGACY-RT", fixedNow.Add(time.Hour)))
	b, _ := backendWith(t, &fakeExchanger{}, true, legacy)

	// A registry-boundary-scoped resolve finds no scoped grant → falls back to the legacy one.
	got, err := b.Resolve(ctx, testNS, "r:reg-a", "weather", UserHash(nil, testUser))
	require.NoError(t, err)
	assert.Equal(t, "LEGACY-AT", got.Value)

	// A user with NO grant at all still gets consent-required under a boundary (no false fallback).
	_, err = b.Resolve(ctx, testNS, "r:reg-a", "weather", UserHash(nil, "nobody@example.com"))
	require.ErrorIs(t, err, ErrConsentRequired)
}

func TestResolvePerUserIsolation(t *testing.T) {
	ctx := context.Background()
	// Only alice has a grant (a still-valid token).
	alice := grantSecret(oauthData("ALICE-AT", "ALICE-RT", fixedNow.Add(time.Hour)))
	b, _ := backendWith(t, &fakeExchanger{}, true, alice)

	got, err := b.Resolve(ctx, testNS, "", "weather", UserHash(nil, testUser))
	require.NoError(t, err)
	assert.Equal(t, "ALICE-AT", got.Value)

	// Bob has no grant on an OAuth server → consent-required, and NEVER alice's token.
	_, err = b.Resolve(ctx, testNS, "", "weather", UserHash(nil, "bob@example.com"))
	require.ErrorIs(t, err, ErrConsentRequired)
}

func TestResolveOpenServerNoGrant(t *testing.T) {
	ctx := context.Background()
	b, _ := backendWith(t, &fakeExchanger{}, false /* not OAuth */)
	_, err := b.Resolve(ctx, testNS, "", "open-mcp", UserHash(nil, testUser))
	assert.ErrorIs(t, err, ErrNoCredential)
}

func TestResolveOrgCredentialWhenNoPersonalGrant(t *testing.T) {
	ctx := context.Background()
	orgCalls := 0
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build() // no personal grant
	b := NewK8sBackend(K8sBackendConfig{
		Client:          cl,
		Now:             func() time.Time { return fixedNow },
		AuthTypeIsOAuth: func(context.Context, string, string) (bool, error) { return true, nil },
		OrgCredential: func(context.Context, string, string) (Credential, error) {
			orgCalls++
			return Credential{Kind: KindBearer, Value: "ORG-SHARED"}, nil
		},
	})
	got, err := b.Resolve(ctx, testNS, "", testServer, UserHash(nil, "bob@example.com"))
	require.NoError(t, err)
	assert.Equal(t, "ORG-SHARED", got.Value, "an org-scoped server resolves the shared org credential")
	assert.Equal(t, 1, orgCalls)
}

func TestResolvePersonalBeforeOrg(t *testing.T) {
	ctx := context.Background()
	orgCalls := 0
	grant := grantSecret(oauthData("ALICE-AT", "RT", fixedNow.Add(time.Hour)))
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(grant).Build()
	b := NewK8sBackend(K8sBackendConfig{
		Client:          cl,
		Now:             func() time.Time { return fixedNow },
		AuthTypeIsOAuth: func(context.Context, string, string) (bool, error) { return true, nil },
		OrgCredential: func(context.Context, string, string) (Credential, error) {
			orgCalls++
			return Credential{Value: "ORG"}, nil
		},
	})
	got, err := b.Resolve(ctx, testNS, "", testServer, UserHash(nil, testUser))
	require.NoError(t, err)
	assert.Equal(t, "ALICE-AT", got.Value, "a personal grant overrides the org credential")
	assert.Equal(t, 0, orgCalls, "the org credential is not consulted when a personal grant exists")
}

func TestResolveOrgNoCredentialFallsThroughToConsent(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	b := NewK8sBackend(K8sBackendConfig{
		Client:          cl,
		Now:             func() time.Time { return fixedNow },
		AuthTypeIsOAuth: func(context.Context, string, string) (bool, error) { return true, nil },
		OrgCredential: func(context.Context, string, string) (Credential, error) {
			return Credential{}, ErrNoCredential // not org-scoped / no org credential
		},
	})
	_, err := b.Resolve(ctx, testNS, "", testServer, UserHash(nil, "bob@example.com"))
	assert.ErrorIs(t, err, ErrConsentRequired, "no org credential + OAuth server → consent")
}

func TestResolveStillValidSkipsRefresh(t *testing.T) {
	ctx := context.Background()
	ex := &fakeExchanger{}
	grant := grantSecret(oauthData("AT", "RT", fixedNow.Add(time.Hour)))
	b, _ := backendWith(t, ex, true, grant)

	got, err := b.Resolve(ctx, testNS, "", "weather", UserHash(nil, testUser))
	require.NoError(t, err)
	assert.Equal(t, "AT", got.Value)
	assert.Equal(t, 0, ex.calls(), "a still-valid token must not hit the token endpoint")
}

func TestResolveRefreshesNearExpiryAndWritesBack(t *testing.T) {
	ctx := context.Background()
	ex := &fakeExchanger{next: Tokens{AccessToken: "NEW-AT", RefreshToken: "NEW-RT", ExpiresAt: fixedNow.Add(time.Hour)}}
	grant := grantSecret(oauthData("OLD-AT", "OLD-RT", fixedNow.Add(10*time.Second)))
	b, cl := backendWith(t, ex, true, grant)

	got, err := b.Resolve(ctx, testNS, "", "weather", UserHash(nil, testUser))
	require.NoError(t, err)
	assert.Equal(t, "NEW-AT", got.Value)
	assert.Equal(t, 1, ex.calls())

	// The rotated tokens are written back to the SAME Secret, server-side.
	var after corev1.Secret
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: testNS, Name: grant.Name}, &after))
	assert.Equal(t, "NEW-AT", string(after.Data[KeyAccessToken]))
	assert.Equal(t, "NEW-RT", string(after.Data[KeyRefreshToken]))
}

func TestResolveNearExpiryNoRefreshTokenNeedsConsent(t *testing.T) {
	ctx := context.Background()
	ex := &fakeExchanger{}
	// Near expiry, and NO refresh token to rotate with → re-consent.
	grant := grantSecret(oauthData("OLD-AT", "", fixedNow.Add(10*time.Second)))
	b, _ := backendWith(t, ex, true, grant)

	_, err := b.Resolve(ctx, testNS, "", "weather", UserHash(nil, testUser))
	assert.ErrorIs(t, err, ErrConsentRequired)
	assert.Equal(t, 0, ex.calls(), "no refresh token ⇒ no token-endpoint call")
}

func TestResolveCacheFastPath(t *testing.T) {
	ctx := context.Background()
	ex := &fakeExchanger{}
	grant := grantSecret(oauthData("AT", "RT", fixedNow.Add(time.Hour)))
	b, cl := backendWith(t, ex, true, grant)
	userHash := UserHash(nil, testUser)

	first, err := b.Resolve(ctx, testNS, "", "weather", userHash)
	require.NoError(t, err)
	assert.Equal(t, "AT", first.Value)

	// Delete the backing Secret; a cached resolve must still succeed (served locally).
	require.NoError(t, cl.Delete(ctx, grant))
	second, err := b.Resolve(ctx, testNS, "", "weather", userHash)
	require.NoError(t, err, "the cache fast-path serves without re-reading the Secret")
	assert.Equal(t, "AT", second.Value)
}

func TestResolveSingleflightCollapsesHerd(t *testing.T) {
	ctx := context.Background()
	ex := &fakeExchanger{
		next:    Tokens{AccessToken: "NEW-AT", ExpiresAt: fixedNow.Add(time.Hour)},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	grant := grantSecret(oauthData("OLD-AT", "OLD-RT", fixedNow.Add(10*time.Second)))
	b, _ := backendWith(t, ex, true, grant)
	userHash := UserHash(nil, testUser)

	const n = 8
	results := make([]Credential, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = b.Resolve(ctx, testNS, "", "weather", userHash)
		}(i)
	}

	// Once the first refresh is in flight, give the herd time to queue behind singleflight,
	// then release. All N share the one refresh result.
	<-ex.entered
	time.Sleep(50 * time.Millisecond)
	close(ex.release)
	wg.Wait()

	assert.Equal(t, 1, ex.calls(), "concurrent resolves of the same grant do ONE refresh")
	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, "NEW-AT", results[i].Value)
	}
}

func TestResolveOptimisticConcurrencyAdoptsWinner(t *testing.T) {
	ctx := context.Background()
	userHash := UserHash(nil, testUser)
	grant := grantSecret(oauthData("OLD-AT", "OLD-RT", fixedNow.Add(10*time.Second)))

	var conflicted bool
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(grant).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if !conflicted {
					conflicted = true
					// Simulate ANOTHER writer (a different pod) refreshing first: write a
					// fresh, valid token, then reject our stale update with a conflict.
					var winner corev1.Secret
					if err := c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: grant.Name}, &winner); err != nil {
						return err
					}
					winner.Data[KeyAccessToken] = []byte("WINNER-AT")
					winner.Data[KeyExpiry] = []byte(fixedNow.Add(time.Hour).UTC().Format(time.RFC3339))
					if err := c.Update(ctx, &winner); err != nil {
						return err
					}
					return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), assert.AnError)
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()

	ex := &fakeExchanger{next: Tokens{AccessToken: "OUR-AT", ExpiresAt: fixedNow.Add(time.Hour)}}
	b := NewK8sBackend(K8sBackendConfig{
		Client:          cl,
		Exchanger:       ex,
		Now:             func() time.Time { return fixedNow },
		AuthTypeIsOAuth: func(context.Context, string, string) (bool, error) { return true, nil },
	})

	got, err := b.Resolve(ctx, testNS, "", "weather", userHash)
	require.NoError(t, err)
	// We adopt the WINNER's token (re-read after conflict), NOT our own rotation, and we do
	// NOT re-call the authorization server for the winner's already-fresh token.
	assert.Equal(t, "WINNER-AT", got.Value)
	assert.Equal(t, 1, ex.calls(), "the conflict loser adopts the winner without a second AS call")
}

func TestRevokeForgetsAndBestEffortRevokes(t *testing.T) {
	ctx := context.Background()
	ex := &fakeExchanger{}
	data := oauthData("AT", "RT", fixedNow.Add(time.Hour))
	data[KeyRevocationEndpoint] = []byte("https://as.example/revoke")
	grant := grantSecret(data)
	b, cl := backendWith(t, ex, true, grant)
	userHash := UserHash(nil, testUser)

	require.NoError(t, b.Revoke(ctx, testNS, "", "weather", userHash))
	assert.Equal(t, 1, ex.revokeCalls, "a stored revocation endpoint ⇒ best-effort RFC 7009 revoke")
	assert.Equal(t, "RT", ex.revokedTok, "revoke uses the refresh token when present")

	// The grant Secret is forgotten (deleted).
	var after corev1.Secret
	err := cl.Get(ctx, client.ObjectKey{Namespace: testNS, Name: grant.Name}, &after)
	assert.True(t, apierrors.IsNotFound(err), "revoke deletes the grant Secret")

	// A revoke of a missing grant is a no-op.
	assert.NoError(t, b.Revoke(ctx, testNS, "", "weather", userHash))
}
