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

package credplane_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agentry/internal/credplane"
	"github.com/ctxmesh/agentry/internal/credresolve"
)

const (
	testNS     = "team-alpha"
	testServer = "weather"
)

// mockResolver is a credresolve.CredentialResolver test double capturing its inputs.
type mockResolver struct {
	cred                      credresolve.Credential
	err                       error
	calls                     int
	gotNS, gotServer, gotUser string
	revokeCalls               int
	revNS, revServer, revUser string
}

func (m *mockResolver) Resolve(_ context.Context, ns, boundary, server, userHash string) (credresolve.Credential, error) {
	m.calls++
	m.gotNS, m.gotServer, m.gotUser = ns, server, userHash
	return m.cred, m.err
}

func (m *mockResolver) Revoke(_ context.Context, ns, boundary, server, userHash string) error {
	m.revokeCalls++
	m.revNS, m.revServer, m.revUser = ns, server, userHash
	return nil
}

// newDelegation wires a credplane server (over the given resolver) behind an httptest
// server and returns a delegating client for it.
func newDelegation(t *testing.T, resolver credresolve.CredentialResolver) *credplane.Client {
	t.Helper()
	ts := httptest.NewServer(credplane.NewServer(resolver, logr.Discard()).Handler())
	t.Cleanup(ts.Close)
	return credplane.NewClient(ts.URL, ts.Client())
}

func TestDelegationResolveRoundTrip(t *testing.T) {
	ctx := context.Background()
	mock := &mockResolver{cred: credresolve.Credential{Kind: credresolve.KindBearer, Value: "USER-TOKEN"}}
	client := newDelegation(t, mock)

	got, err := client.Resolve(ctx, testNS, "", testServer, "u-alicehash")
	require.NoError(t, err)
	assert.Equal(t, credresolve.KindBearer, got.Kind)
	assert.Equal(t, "USER-TOKEN", got.Value)
	// The delegated call carried the exact coordinates.
	assert.Equal(t, testNS, mock.gotNS)
	assert.Equal(t, testServer, mock.gotServer)
	assert.Equal(t, "u-alicehash", mock.gotUser)
}

func TestDelegationMapsSentinels(t *testing.T) {
	ctx := context.Background()

	t.Run("consent_required", func(t *testing.T) {
		client := newDelegation(t, &mockResolver{err: credresolve.ErrConsentRequired})
		_, err := client.Resolve(ctx, testNS, "", testServer, "u-a")
		assert.ErrorIs(t, err, credresolve.ErrConsentRequired)
	})
	t.Run("no_credential", func(t *testing.T) {
		client := newDelegation(t, &mockResolver{err: credresolve.ErrNoCredential})
		_, err := client.Resolve(ctx, testNS, "", testServer, "u-a")
		assert.ErrorIs(t, err, credresolve.ErrNoCredential)
	})
	t.Run("internal error is generic (never leaks the cause)", func(t *testing.T) {
		client := newDelegation(t, &mockResolver{err: errors.New("etcd on fire: secret xyz")})
		_, err := client.Resolve(ctx, testNS, "", testServer, "u-a")
		require.Error(t, err)
		assert.NotErrorIs(t, err, credresolve.ErrConsentRequired)
		assert.NotContains(t, err.Error(), "etcd on fire", "the central cause must not cross the wire")
	})
}

func TestDelegationRevoke(t *testing.T) {
	mock := &mockResolver{}
	client := newDelegation(t, mock)
	require.NoError(t, client.Revoke(context.Background(), testNS, "", testServer, "u-alicehash"))
	assert.Equal(t, 1, mock.revokeCalls)
	assert.Equal(t, "u-alicehash", mock.revUser)
}

// blockingExchanger is a credresolve.TokenExchanger that counts refreshes and blocks the
// first one until released, so a concurrent herd can be forced to overlap.
type blockingExchanger struct {
	mu        sync.Mutex
	refreshes int
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	next      credresolve.Tokens
}

func (b *blockingExchanger) Refresh(context.Context, string, string, string) (credresolve.Tokens, error) {
	b.mu.Lock()
	b.refreshes++
	b.mu.Unlock()
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.next, nil
}

func (b *blockingExchanger) Revoke(context.Context, string, string) error { return nil }

func (b *blockingExchanger) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refreshes
}

// TestDelegationGlobalSingleflight is the scaling proof (ADR 0030 §1): many delegating
// sidecar calls for the SAME grant, arriving concurrently at the ONE central service,
// collapse to a SINGLE OAuth refresh — the herd is amortized globally, not per-pod.
func TestDelegationGlobalSingleflight(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	userHash := credresolve.UserHash(nil, "alice@example.com")

	// A near-expiry grant Secret (in-skew ⇒ the backend refreshes).
	grant := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credresolve.SecretName(testServer, userHash, ""),
			Namespace: testNS,
			Labels:    credresolve.SecretLabels(testServer, userHash, "", ""),
		},
		Data: credresolve.SecretData(
			credresolve.OAuthConfig{TokenEndpoint: "https://as/token", ClientID: "cid"},
			credresolve.Tokens{AccessToken: "OLD", RefreshToken: "RT", ExpiresAt: now.Add(10 * time.Second)},
		),
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(grant).Build()

	ex := &blockingExchanger{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		next:    credresolve.Tokens{AccessToken: "FRESH", ExpiresAt: now.Add(time.Hour)},
	}
	backend := credresolve.NewK8sBackend(credresolve.K8sBackendConfig{
		Client:    cl,
		Exchanger: ex,
		Now:       func() time.Time { return now },
	})

	ts := httptest.NewServer(credplane.NewServer(backend, logr.Discard()).Handler())
	t.Cleanup(ts.Close)

	// Many independent delegating clients (as N sidecars would be), firing concurrently.
	const n = 12
	results := make([]credresolve.Credential, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			client := credplane.NewClient(ts.URL, ts.Client())
			results[i], errs[i] = client.Resolve(ctx, testNS, "", testServer, userHash)
		}(i)
	}

	<-ex.entered
	time.Sleep(75 * time.Millisecond) // let the herd queue behind the one in-flight refresh
	close(ex.release)
	wg.Wait()

	assert.Equal(t, 1, ex.calls(), "the whole fleet's herd collapses to ONE refresh at the center")
	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, "FRESH", results[i].Value)
	}
}
