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
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/gateway"
)

// defaultPlatform is the platform a reconciler built without a namespace belongs to.
var defaultPlatform = newPlatformServices("")

// customPlatformNamespace is an install outside the chart's default namespace.
const customPlatformNamespace = "platform-x"

// platformHost matches a platform Service address and captures its namespace.
var platformHost = regexp.MustCompile(`ctxmesh-[a-z0-9-]+\.([a-z0-9-]+)\.svc`)

// The default install keeps exactly the addresses it had, so its agents' env and revision names do not
// move on upgrade.
func TestTheDefaultInstallKeepsItsAddresses(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "http://ctxmesh-gateway.ctxmesh.svc:4000", defaultPlatform.gatewayURL())
	assert.Equal(t, "http://ctxmesh-bff.ctxmesh.svc.cluster.local:9090", defaultPlatform.bffInternalURL())
	assert.Equal(t, "http://ctxmesh-bff.ctxmesh.svc.cluster.local:9090/api/internal/async/publish",
		defaultPlatform.asyncPublishURL())
	assert.Equal(t, "ctxmesh-statelayer.ctxmesh.svc:6379", defaultPlatform.statelayerAddr())
	assert.Equal(t, "ctxmesh-objectstore.ctxmesh.svc:9000", defaultPlatform.objectStoreAddr())
	assert.Equal(t, defaultPlatform, newPlatformServices(DefaultPlatformNamespace))
}

// platformAddressesIn returns, for every env var on any container of pod that holds a platform Service
// address, the namespace that address names.
func platformAddressesIn(pod podTemplate) map[string]string {
	out := map[string]string{}
	for _, c := range pod.containers {
		for _, e := range c.Env {
			if m := platformHost.FindStringSubmatch(e.Value); m != nil {
				out[c.Name+"/"+e.Name] = m[1]
			}
		}
	}
	return out
}

// An agent of a platform installed outside the default namespace is given that namespace's services,
// everywhere the controller writes one, and its revision name moves so a running pod gets them.
func TestAnAgentReachesThePlatformInItsOwnNamespace(t *testing.T) {
	t.Parallel()

	addressed := map[string]bool{}
	fixtures := map[string]platformEnvFixture{"plain": {}}
	maps.Copy(fixtures, platformEnvFixtures)
	for name, f := range fixtures {
		r, deploy := f.build(t, nil)
		if name == "plain" {
			deploy = &agentsv1beta1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "team-a"},
				Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest"},
			}
			r = identityReconciler(t, deploy, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}})
		}
		r.PlatformNamespace = customPlatformNamespace
		pod, err := r.buildPodTemplate(context.Background(), deploy)
		require.NoError(t, err, name)
		for v, ns := range platformAddressesIn(pod) {
			assert.Equal(t, customPlatformNamespace, ns, "%s: %s", name, v)
			addressed[v] = true
		}

		r.PlatformNamespace = ""
		defaultPod, err := r.buildPodTemplate(context.Background(), deploy)
		require.NoError(t, err, name)
		assert.NotEqual(t, defaultPod.digest, pod.digest, "%s: the addresses change, so the revision name moves", name)
		if name == "plain" {
			assert.Contains(t, pod.supersededDigests, defaultPod.digest,
				"the pod a default-namespace controller named is the same pod renamed")
		}
		for v, ns := range platformAddressesIn(defaultPod) {
			assert.Equal(t, DefaultPlatformNamespace, ns, "%s: %s", name, v)
		}
	}
	for _, v := range []string{
		"user-container/" + envModelGatewayURL, "user-container/" + envGatewayUpstreamURL,
		"user-container/" + envBFFInternalURL, "user-container/" + envAsyncPublishURL,
		"user-container/" + envMemoryBackendAddr, "user-container/" + envTenantQuotaAddr,
		"user-container/" + envObjectStoreAddr,
	} {
		assert.True(t, addressed[v], "no fixture gives %s a platform address, so this test does not cover it", v)
	}
}

// A registry's members may reach the platform's namespace, wherever it is installed.
func TestARegistryAdmitsThePlatformNamespace(t *testing.T) {
	t.Parallel()

	registry := &agentsv1beta1.AgentRegistry{
		ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "team-a"},
		Spec:       agentsv1beta1.AgentRegistrySpec{RegistryId: "mesh-id"},
	}
	c := fake.NewClientBuilder().WithScheme(podTemplateScheme(t)).WithObjects(registry).Build()
	r := &AgentRegistryReconciler{Client: c, Scheme: podTemplateScheme(t), PlatformNamespace: customPlatformNamespace}
	require.NoError(t, r.reconcileNetworkPolicy(context.Background(), registry))

	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "team-a", Name: "mesh" + networkPolicyNameSuffix}, &np))
	assert.Contains(t, egressNamespaces(np), customPlatformNamespace)
	assert.NotContains(t, egressNamespaces(np), DefaultPlatformNamespace)
}

func egressNamespaces(np networkingv1.NetworkPolicy) []string {
	var out []string
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil {
				if ns, ok := peer.NamespaceSelector.MatchLabels[namespaceNameLabel]; ok {
					out = append(out, ns)
				}
			}
		}
	}
	return out
}

// A Tenant can never claim the platform's namespace, and its members reach it, wherever it is installed.
// The chart's default namespace is an ordinary one when the platform runs elsewhere.
func TestATenantProtectsAndAdmitsThePlatformNamespace(t *testing.T) {
	t.Parallel()

	r := &TenantReconciler{PlatformNamespace: customPlatformNamespace}
	assert.True(t, r.protectedNamespace(customPlatformNamespace))
	assert.False(t, r.protectedNamespace(DefaultPlatformNamespace))
	assert.True(t, r.protectedNamespace(kubeSystemNamespace))
	assert.True(t, (&TenantReconciler{}).protectedNamespace(DefaultPlatformNamespace))

	tenant := &agentsv1beta1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	c := fake.NewClientBuilder().WithScheme(podTemplateScheme(t)).WithObjects(tenant).Build()
	r.Client = c
	require.NoError(t, r.reconcileNetworkPolicy(context.Background(), tenant, "team-a"))
	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "team-a", Name: tenantNetworkPolicyName}, &np))
	assert.Contains(t, egressNamespaces(np), customPlatformNamespace)
	assert.NotContains(t, egressNamespaces(np), DefaultPlatformNamespace)
}

// The gateway's config, its Deployment and the provider Secret mirrors live in the platform's namespace.
func TestTheGatewayIsConfiguredInThePlatformNamespace(t *testing.T) {
	t.Parallel()

	ns := customPlatformNamespace
	replicas := int32(1)
	objs := []client.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: gateway.GatewayDeploymentName, Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gateway"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "gateway"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "litellm", Image: "litellm"}}},
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "provider-key", Namespace: "team-a"},
			Data:       map[string][]byte{"api-key": []byte("k")},
		},
		&agentsv1beta1.SecretBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: "team-a"},
			Spec: agentsv1beta1.SecretBindingSpec{
				Backend: "kubernetes", SecretRef: agentsv1beta1.SecretKeyRef{Name: "provider-key", Key: "api-key"},
			},
		},
		&agentsv1beta1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "team-a"},
			Spec: agentsv1beta1.ModelRouteSpec{Providers: []agentsv1beta1.ProviderRef{{
				Provider: "openai", Model: "gpt-4o-mini", Priority: 1, SecretBindingRef: "openai",
			}}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(podTemplateScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&agentsv1beta1.ModelRoute{}).Build()
	r := &ModelRouteReconciler{Client: c, PlatformNamespace: ns}
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "chat"},
	})
	require.NoError(t, err)

	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: gateway.GatewayConfigMapName}, &cm))
	assert.Contains(t, cm.Data["config.yaml"], "gpt-4o-mini")
	var mirror corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: gateway.MirrorSecretName(ns, "team-a", "provider-key"),
	}, &mirror), "the provider Secret is mirrored beside the gateway")
	var gw appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: gateway.GatewayDeploymentName}, &gw))
	assert.NotEmpty(t, gw.Spec.Template.Annotations, "the gateway in the platform namespace is rolled to the new config")
}

// fixedPlatformNamespaceSources are the controller's sources: every package that gives agents, their
// policies or the gateway a platform address, and the binary that tells them where the platform is.
var fixedPlatformNamespaceSources = []string{"*.go", "../gateway/*.go", "../../cmd/main.go"}

// TestNoPlatformAddressNamesANamespace is the census: no string in the controller's sources names a
// platform Service in a fixed namespace, or is the default namespace itself, except the one constant the
// derivation falls back to. A literal namespace in an address is how every agent of an install outside the
// default namespace was pointed at Services that do not exist.
func TestNoPlatformAddressNamesANamespace(t *testing.T) {
	t.Parallel()

	fixedAddress := regexp.MustCompile(`ctxmesh-[a-z0-9-]+\.[a-z0-9-]+\.svc|\.` + regexp.QuoteMeta(DefaultPlatformNamespace) + `\.svc`)
	var offenders []string
	scanned := 0
	for _, pattern := range fixedPlatformNamespaceSources {
		files, err := filepath.Glob(pattern)
		require.NoError(t, err)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			scanned++
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, f, nil, 0)
			require.NoError(t, err)
			allowed := allowedNamespaceLiterals(file)
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || allowed[lit] {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				if s == DefaultPlatformNamespace || fixedAddress.MatchString(s) {
					offenders = append(offenders, fset.Position(lit.Pos()).String()+": "+lit.Value)
				}
				return true
			})
		}
	}
	require.Greater(t, scanned, 50, "the census must read the controller's sources")
	assert.Empty(t, offenders, "derive these from the platform namespace (platform_services.go)")
}

// allowedNamespaceLiterals are the strings in file that may say "ctxmesh": the declaration of
// DefaultPlatformNamespace, which the derivation falls back to, and the organization name on the
// webhook CA, which is not a namespace.
func allowedNamespaceLiterals(file *ast.File) map[*ast.BasicLit]bool {
	allowed := map[*ast.BasicLit]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ValueSpec:
			for i, name := range v.Names {
				if name.Name == "DefaultPlatformNamespace" && i < len(v.Values) {
					if lit, ok := v.Values[i].(*ast.BasicLit); ok {
						allowed[lit] = true
					}
				}
			}
		case *ast.KeyValueExpr:
			if key, ok := v.Key.(*ast.Ident); ok && key.Name == "CAOrganization" {
				if lit, ok := v.Value.(*ast.BasicLit); ok {
					allowed[lit] = true
				}
			}
		}
		return true
	})
	return allowed
}
