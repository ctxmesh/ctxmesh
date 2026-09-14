//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controller"
)

// TestControllerStartsWithoutKnativeEventing is the M179 proof for ADR 0141.
//
// executionModel is serving (the default) | eventing | job, and only one of the three needs Knative
// Eventing. The controller nonetheless owned Trigger and Broker unconditionally, and
// controller-runtime starts an informer per owned kind — so on a cluster without those CRDs the
// manager could not start AT ALL. A user deploying nothing but serving agents, which is the entire
// quickstart, had to install a second Knative component before the product would run.
//
// The fixture is the point: this environment loads our CRDs and Knative SERVING, and deliberately
// omits eventing-crds.yaml. The package's other suite loads all of them, so between the two the
// conditional branch is exercised in both shapes — a branch tested in one configuration is untested.
//
// Without the fix this fails at mgr.Start with `no matches for kind "Trigger"`.
func TestControllerStartsWithoutKnativeEventing(t *testing.T) {
	crds := t.TempDir()
	for _, f := range []string{"serving-crds.yaml", "keda-crds.yaml"} {
		b, err := os.ReadFile(filepath.Join("testdata", "crds", f))
		require.NoError(t, err, "fixture %s must exist", f)
		require.NoError(t, os.WriteFile(filepath.Join(crds, f), b, 0o600))
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases"), crds},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err, "envtest control plane failed to start — run 'make setup-envtest'")
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	require.NoError(t, agentsv1beta1.AddToScheme(scheme))
	require.NoError(t, servingv1.AddToScheme(scheme))
	// The eventing TYPES stay registered in the scheme even though the cluster serves no eventing
	// CRDs. That is the realistic shape: the binary always knows the types; the cluster is what
	// varies. Registering them also keeps this test honest — the manager only avoids the watch
	// because of the capability check, not because the type is unknown to it.
	require.NoError(t, eventingv1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)

	available := controller.EventingAvailable(mgr)
	require.False(t, available,
		"this fixture deliberately omits the eventing CRDs; if detection says they are present the test proves nothing")

	require.NoError(t, (&controller.AgentDeploymentReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		EventingAvailable: available,
	}).SetupWithManager(mgr), "the deployment controller must register without Knative Eventing")

	require.NoError(t, (&controller.AgentRegistryReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		EventingAvailable: available,
	}).SetupWithManager(mgr), "the registry controller must register without Knative Eventing")

	// Starting is the real assertion. SetupWithManager only queues the watches; controller-runtime
	// resolves each owned kind against the API when the manager runs, which is where the old
	// unconditional Owns(&Trigger{}) died.
	runCtx, stop := context.WithCancel(t.Context())
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(runCtx) }()

	select {
	case err := <-errCh:
		require.NoError(t, err, "the manager must START on a cluster with no Knative Eventing")
	case <-time.After(30 * time.Second):
		// Still running after 30s is the pass: every informer resolved against the API and the
		// manager is serving. The old unconditional Owns(&Trigger{}) returned here immediately with
		// `no matches for kind "Trigger"`.
	}
}
