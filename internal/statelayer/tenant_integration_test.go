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

// Envtest-backed proof of the tenant resolver against a REAL API server (not the
// fake client): StartNamespaceResolver builds a live Namespace-only cache, syncs,
// and resolves namespace → tenant via the authoritative label — including a label
// applied AFTER sync (proving the informer watch, not a one-shot list). Runs only
// under `make test-integration` (build tag integration).
package statelayer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestIntegrationNamespaceResolver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl, err := client.New(itCfg, client.Options{Scheme: itScheme})
	require.NoError(t, err)

	// A tenant-labeled namespace and an unlabeled one, both present BEFORE the cache
	// starts (the initial list path).
	require.NoError(t, cl.Create(ctx, nsObj("it-alpha", map[string]string{tenantLabel: "tenant-alpha"})))
	require.NoError(t, cl.Create(ctx, nsObj("it-plain", nil)))

	resolver, err := StartNamespaceResolver(ctx, itCfg, 30*time.Second)
	require.NoError(t, err)

	id, err := resolver.TenantID(ctx, "it-alpha")
	require.NoError(t, err)
	assert.Equal(t, "tenant-alpha", id, "labeled namespace resolves to its tenant")

	id, err = resolver.TenantID(ctx, "it-plain")
	require.NoError(t, err)
	assert.Empty(t, id, "unlabeled namespace is untenanted")

	id, err = resolver.TenantID(ctx, "it-nonexistent")
	require.NoError(t, err)
	assert.Empty(t, id, "unknown namespace is untenanted, not an error")

	// A namespace labeled AFTER the cache synced must become resolvable via the
	// informer's watch (not just the initial list).
	require.NoError(t, cl.Create(ctx, nsObj("it-beta", map[string]string{tenantLabel: "tenant-beta"})))
	require.Eventually(t, func() bool {
		got, e := resolver.TenantID(ctx, "it-beta")
		return e == nil && got == "tenant-beta"
	}, 10*time.Second, 100*time.Millisecond, "the watch must reflect a namespace labeled after sync")
}
