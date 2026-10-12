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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// A controller installed outside the default namespace serves its agents, through the real API server,
// with the platform's services in its own namespace: every platform address in the Knative Service names
// it, and none names the default.
func TestAnAgentOfACustomNamespaceInstallIsServedItsPlatform(t *testing.T) {
	const name, namespace = "pns-agent", "default"
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:         "ghcr.io/ctxmesh/example-agent:latest",
			SessionMemory: &agentsv1beta1.SessionMemorySpec{},
			Record:        true,
		},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	r := newReconciler()
	r.PlatformNamespace = customPlatformNamespace
	reconcileNN(t, r, name, namespace)

	ksvc := getKsvc(t, name, namespace)
	addressed := map[string]string{}
	for _, c := range ksvc.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if m := platformHost.FindStringSubmatch(e.Value); m != nil {
				addressed[e.Name] = m[1]
			}
		}
	}
	for _, v := range []string{envGatewayUpstreamURL, envBFFInternalURL, envMemoryBackendAddr, envObjectStoreAddr} {
		assert.Equal(t, customPlatformNamespace, addressed[v], "%s", v)
	}
	for v, ns := range addressed {
		assert.Equal(t, customPlatformNamespace, ns, "%s", v)
	}
	env := envByName(ksvc.Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, newPlatformServices(customPlatformNamespace).gatewayURL(), env[envGatewayUpstreamURL])
}
