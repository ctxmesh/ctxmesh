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

//go:build integration

package integration

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestEnvtestBootstrap proves the envtest toolchain works end to end: the
// control plane (kube-apiserver + etcd) starts from KUBEBUILDER_ASSETS, a
// controller-runtime client can talk to it, and objects round-trip. Milestone
// M1 replaces this bootstrap with real reconciler suites.
func TestEnvtestBootstrap(t *testing.T) {
	env := &envtest.Environment{}

	cfg, err := env.Start()
	require.NoError(t, err, "envtest control plane failed to start — run 'make setup-envtest'")
	t.Cleanup(func() {
		require.NoError(t, env.Stop())
	})

	c, err := client.New(cfg, client.Options{})
	require.NoError(t, err)

	ctx := t.Context()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "envtest-bootstrap"}}
	require.NoError(t, c.Create(ctx, ns))

	got := &corev1.Namespace{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "envtest-bootstrap"}, got))
	require.Equal(t, corev1.NamespaceActive, got.Status.Phase)
}
