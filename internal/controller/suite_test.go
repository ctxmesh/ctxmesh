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

// Package controller contains envtest-backed integration tests for the
// AgentDeployment controller. Tests run only with the 'integration' build tag
// via: make test-integration
package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	agentsv1alpha1 "github.com/ctx-mesh/agent-engine/api/v1alpha1"
)

var (
	k8sClient  client.Client
	testEnv    *envtest.Environment
	testCtx    context.Context
	testCancel context.CancelFunc
)

// TestMain bootstraps the envtest environment for all integration tests in this
// package. It mirrors the Kubebuilder scaffold pattern but uses stdlib testing
// rather than Ginkgo (ADR 0003: stdlib-only test pyramid).
//
// CRD directories loaded:
//   - config/crd/bases — our AgentDeployment and AgentVersion CRDs
//   - test/integration/testdata/crds — Knative Serving CRDs (serving-crds.yaml,
//     pinned to knative-v1.22.1 / knative.dev/serving v0.49.1)
func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	testCtx, testCancel = context.WithCancel(context.Background())
	defer testCancel()

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "test", "integration", "testdata", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}

	if dir := firstEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic("failed to start envtest environment: " + err.Error())
	}

	testScheme := k8sruntime.NewScheme()
	if err = scheme.AddToScheme(testScheme); err != nil {
		panic("failed to add client-go scheme: " + err.Error())
	}
	if err = agentsv1alpha1.AddToScheme(testScheme); err != nil {
		panic("failed to add agents/v1alpha1 scheme: " + err.Error())
	}
	if err = servingv1.AddToScheme(testScheme); err != nil {
		panic("failed to add Knative serving/v1 scheme: " + err.Error())
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		panic("failed to create envtest client: " + err.Error())
	}

	// Ensure the agent-engine-system namespace exists. It is used by the
	// ModelRoute controller tests for the gateway ConfigMap and Deployment.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agent-engine-system"}}
	if nsErr := k8sClient.Create(testCtx, ns); nsErr != nil {
		if !apierrors.IsAlreadyExists(nsErr) {
			panic("failed to create agent-engine-system namespace: " + nsErr.Error())
		}
	}

	code := m.Run()

	if stopErr := testEnv.Stop(); stopErr != nil {
		logf.Log.Error(stopErr, "envtest environment stopped with error")
	}

	os.Exit(code)
}

// firstEnvTestBinaryDir returns the first subdirectory inside bin/k8s so tests
// run from an IDE (without KUBEBUILDER_ASSETS set) can still find the binaries
// downloaded by 'make setup-envtest'.
func firstEnvTestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}
