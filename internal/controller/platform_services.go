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

// DefaultPlatformNamespace is the chart's default install namespace (values.namespace). It is only a
// fallback: the controller reads its own namespace from POD_NAMESPACE, and every platform service runs
// in that same namespace, because the chart installs every platform component into values.namespace.
const DefaultPlatformNamespace = "ctxmesh"

// platformServices are the in-cluster addresses of the platform services agents reach, and the namespace
// agents' network policies admit them from. Each is derived from the namespace the platform is installed
// into; a namespace written into an address points every agent of any other install at nothing.
type platformServices struct {
	namespace string
}

// newPlatformServices returns the services of the platform installed into namespace, or into
// DefaultPlatformNamespace when namespace is empty (a reconciler built without one, as in tests).
func newPlatformServices(namespace string) platformServices {
	if namespace == "" {
		namespace = DefaultPlatformNamespace
	}
	return platformServices{namespace: namespace}
}

// gatewayURL is the LiteLLM model gateway: MODEL_GATEWAY_URL for an agent whose calls need no in-pod
// proxy, else the proxy's GATEWAY_UPSTREAM_URL.
func (p platformServices) gatewayURL() string {
	return "http://ctxmesh-gateway." + p.namespace + ".svc:4000"
}

// bffInternalURL is the BFF's in-cluster listener (BFF_INTERNAL_URL): run-capability binding, guardrail
// audits, delegation and async publish.
func (p platformServices) bffInternalURL() string {
	return "http://ctxmesh-bff." + p.namespace + ".svc.cluster.local:9090"
}

// asyncPublishURL is where a registry member hands a fire-and-forget hop to become durable.
func (p platformServices) asyncPublishURL() string {
	return p.bffInternalURL() + "/api/internal/async/publish"
}

// statelayerAddr is the in-cluster Valkey: the default session-memory backend and, on an install without
// the state-layer proxy, the tenant-quota and spawn-guard store.
func (p platformServices) statelayerAddr() string {
	return "ctxmesh-statelayer." + p.namespace + ".svc:6379"
}

// objectStoreAddr is the dev data plane's object store, for blob offload and record fixtures.
func (p platformServices) objectStoreAddr() string {
	return "ctxmesh-objectstore." + p.namespace + ".svc:9000"
}
