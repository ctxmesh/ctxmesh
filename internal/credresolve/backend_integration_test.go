//go:build integration

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

// Envtest-backed proof of the credresolve backend against a REAL API server (not the
// fake client): per-user grant isolation and one-refresh-under-herd with genuine
// resourceVersion optimistic concurrency. Runs only under `make test-integration`
// (build tag integration).
package credresolve

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	itEnv    *envtest.Environment
	itCfg    *rest.Config
	itScheme *runtime.Scheme
)

// TestMain bootstraps a bare envtest control plane (core Secrets only — no CRDs; the
// auth-type seam keeps this package CRD-free) so grant reads/writes hit a real API server.
func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	itScheme = runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(itScheme); err != nil {
		panic("add client-go scheme: " + err.Error())
	}

	itEnv = &envtest.Environment{}
	cfg, err := itEnv.Start()
	if err != nil {
		panic("start envtest: " + err.Error())
	}
	itCfg = cfg

	code := m.Run()
	_ = itEnv.Stop()
	os.Exit(code)
}

// itClient builds a real API-server-backed client + a unique namespace for a test.
func itClient(t *testing.T, ns string) client.Client {
	t.Helper()
	cl, err := client.New(itCfg, client.Options{Scheme: itScheme})
	require.NoError(t, err)
	require.NoError(t, cl.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	return cl
}

func itGrant(ns, server, user string, data map[string][]byte) *corev1.Secret {
	userHash := UserHash(nil, user)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(server, userHash),
			Namespace: ns,
			Labels:    SecretLabels(server, userHash, ""),
		},
		Data: data,
	}
}

// TestIntegrationPerUserIsolation proves against a real API server that a grant written
// for user A is resolvable by A and never by B (who gets consent-required, not A's token).
func TestIntegrationPerUserIsolation(t *testing.T) {
	ctx := context.Background()
	const ns = "it-isolation"
	cl := itClient(t, ns)
	now := time.Now().UTC()

	alice := itGrant(ns, "weather", "alice@example.com", SecretData(
		OAuthConfig{TokenEndpoint: "https://as/token", ClientID: "cid"},
		Tokens{AccessToken: "ALICE-AT", RefreshToken: "ALICE-RT", ExpiresAt: now.Add(time.Hour)},
	))
	require.NoError(t, cl.Create(ctx, alice))

	b := NewK8sBackend(K8sBackendConfig{
		Client:          cl,
		AuthTypeIsOAuth: func(context.Context, string, string) (bool, error) { return true, nil },
	})

	got, err := b.Resolve(ctx, ns, "weather", UserHash(nil, "alice@example.com"))
	require.NoError(t, err)
	assert.Equal(t, "ALICE-AT", got.Value)

	_, err = b.Resolve(ctx, ns, "weather", UserHash(nil, "bob@example.com"))
	assert.ErrorIs(t, err, ErrConsentRequired)
}

// TestIntegrationOneRefreshUnderHerd proves that a concurrent herd of resolves against a
// near-expiry grant collapses to a SINGLE token-endpoint refresh (in-process singleflight)
// and that the rotated token is written back once, on a real API server.
func TestIntegrationOneRefreshUnderHerd(t *testing.T) {
	ctx := context.Background()
	const ns = "it-herd"
	cl := itClient(t, ns)
	now := time.Now().UTC()

	grant := itGrant(ns, "weather", "alice@example.com", SecretData(
		OAuthConfig{TokenEndpoint: "https://as/token", ClientID: "cid"},
		Tokens{AccessToken: "OLD-AT", RefreshToken: "OLD-RT", ExpiresAt: now.Add(10 * time.Second)},
	))
	require.NoError(t, cl.Create(ctx, grant))

	ex := &fakeExchanger{
		next:    Tokens{AccessToken: "NEW-AT", RefreshToken: "NEW-RT", ExpiresAt: now.Add(time.Hour)},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	b := NewK8sBackend(K8sBackendConfig{
		Client:          cl,
		Exchanger:       ex,
		AuthTypeIsOAuth: func(context.Context, string, string) (bool, error) { return true, nil },
	})
	userHash := UserHash(nil, "alice@example.com")

	const n = 10
	results := make([]Credential, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = b.Resolve(ctx, ns, "weather", userHash)
		}(i)
	}
	<-ex.entered
	time.Sleep(75 * time.Millisecond)
	close(ex.release)
	wg.Wait()

	assert.Equal(t, 1, ex.calls(), "the herd collapses to one refresh")
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		assert.Equal(t, "NEW-AT", results[i].Value)
	}

	var after corev1.Secret
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: grant.Name}, &after))
	assert.Equal(t, "NEW-AT", string(after.Data[KeyAccessToken]))
}
