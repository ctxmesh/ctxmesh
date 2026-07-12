package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
)

// TestPreferredAgentURL pins the invoke-reachability fix (M20 W0): AgentDeployment
// status.url must prefer the ksvc CLUSTER-LOCAL address (status.address.url) over the
// external route URL, so the in-cluster BFF Playground invoke can actually reach the
// agent. The external ingress URL is not hairpin-routable from inside the cluster (on
// kind it points at the calling pod's own localhost), so dispatching there 502s.
func TestPreferredAgentURL(t *testing.T) {
	clusterLocal := apis.HTTP("echo.default.svc.cluster.local")
	external := apis.HTTP("echo.default.127.0.0.1.sslip.io")

	t.Run("prefers the cluster-local address over the external route", func(t *testing.T) {
		ksvc := &servingv1.Service{}
		ksvc.Status.URL = external
		ksvc.Status.Address = &duckv1.Addressable{URL: clusterLocal}
		assert.Equal(t, "http://echo.default.svc.cluster.local", preferredAgentURL(ksvc))
	})

	t.Run("falls back to the route URL when no cluster-local address is set", func(t *testing.T) {
		ksvc := &servingv1.Service{}
		ksvc.Status.URL = external
		assert.Equal(t, "http://echo.default.127.0.0.1.sslip.io", preferredAgentURL(ksvc))
	})

	t.Run("empty when neither is set so the caller keeps any existing url", func(t *testing.T) {
		assert.Equal(t, "", preferredAgentURL(&servingv1.Service{}))
	})
}
