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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentversion"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
)

// legacyAgentVersionCRD is the retired AgentVersion CRD as an upgraded cluster still has it (Helm
// keeps CRDs), reduced to what the import reads.
func legacyAgentVersionCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "agentversions.agents.ctxmesh.ai"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "agents.ctxmesh.ai",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind: "AgentVersion", ListKind: "AgentVersionList", Plural: "agentversions", Singular: "agentversion",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1beta1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
					},
				}},
			}},
		},
	}
}

// freshClient has its own REST mapper, so it sees the API as it is now.
func freshClient(t *testing.T) client.Client {
	t.Helper()
	c, err := client.New(testEnv.Config, client.Options{Scheme: k8sClient.Scheme()})
	require.NoError(t, err)
	return c
}

// mkLegacyVersion creates an AgentVersion object as an older controller wrote it.
func mkLegacyVersion(t *testing.T, c client.Client, name, namespace, owner string, ownerUID types.UID, image string) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("agents.ctxmesh.ai/v1beta1")
	obj.SetKind("AgentVersion")
	obj.SetName(name)
	obj.SetNamespace(namespace)
	if owner != "" {
		obj.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "agents.ctxmesh.ai/v1beta1", Kind: "AgentDeployment", Name: owner, UID: ownerUID,
			Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
		}})
	}
	obj.Object["spec"] = map[string]any{
		"deploymentName": owner,
		"snapshot": map[string]any{
			"image": image,
			// A field a later release removed: dropped on import, not an error.
			"approvalPolicyRef": "retired",
		},
	}
	require.NoError(t, c.Create(testCtx, obj))
}

// An upgrade keeps its history: the AgentVersion objects of live agents are copied in, oldest first,
// once; objects of deleted or recreated agents, and objects with no owner, are not. A rollback then
// reverts to an imported snapshot.
func TestImportAgentVersions(t *testing.T) {
	const namespace = "default"

	// A fresh install has no AgentVersion kind: nothing to import, and the import is done.
	fresh := agentversion.NewMemStore()
	res, err := ImportAgentVersions(testCtx, freshClient(t), fresh)
	require.NoError(t, err)
	assert.Equal(t, agentversion.ImportResult{Ran: true}, res)

	_, err = envtest.InstallCRDs(testEnv.Config, envtest.CRDInstallOptions{
		CRDs: []*apiextensionsv1.CustomResourceDefinition{legacyAgentVersionCRD()},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = envtest.UninstallCRDs(testEnv.Config, envtest.CRDInstallOptions{
			CRDs: []*apiextensionsv1.CustomResourceDefinition{legacyAgentVersionCRD()},
		})
	})

	// The fresh install's import completed, so a CRD that appears later is never imported into it:
	// its versions would be ordered after the ones recorded since.
	res, err = ImportAgentVersions(testCtx, freshClient(t), fresh)
	require.NoError(t, err)
	assert.False(t, res.Ran)

	const name = "imp-agent"
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:v2"},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	uid := getDeploy(t, name, namespace).UID

	c := freshClient(t)
	mkLegacyVersion(t, c, name+"-a1", namespace, name, uid, "ghcr.io/ctxmesh/example-agent:v1")
	mkLegacyVersion(t, c, name+"-b2", namespace, name, uid, "ghcr.io/ctxmesh/example-agent:v2")
	mkLegacyVersion(t, c, name+"-old", namespace, name, "an-earlier-uid", "ghcr.io/ctxmesh/example-agent:v0")
	mkLegacyVersion(t, c, "gone-agent-v1", namespace, "gone-agent", "gone-uid", "ghcr.io/ctxmesh/example-agent:gone")
	mkLegacyVersion(t, c, "loose-v1", namespace, "", "", "ghcr.io/ctxmesh/example-agent:loose")

	store := agentversion.NewMemStore()
	res, err = ImportAgentVersions(testCtx, c, store)
	require.NoError(t, err)
	assert.Equal(t, agentversion.ImportResult{Ran: true, Imported: 2, Skipped: 3}, res)
	assert.Equal(t, []string{name + "-b2", name + "-a1"}, versionNames(t, store, name, namespace))
	v, ok := storedVersion(t, store, name, namespace, name+"-a1")
	require.True(t, ok)
	assert.Equal(t, "ghcr.io/ctxmesh/example-agent:v1", v.Snapshot.Image)
	assert.Equal(t, agentversion.SourceImport, v.Source)
	owners, err := store.Owners(testCtx, namespace, "gone-agent-v1")
	require.NoError(t, err)
	assert.Empty(t, owners, "a version of a deleted agent is not imported")

	// Running again changes nothing.
	mkLegacyVersion(t, c, name+"-c3", namespace, name, uid, "ghcr.io/ctxmesh/example-agent:v3")
	res, err = ImportAgentVersions(testCtx, c, store)
	require.NoError(t, err)
	assert.False(t, res.Ran)
	assert.Equal(t, []string{name + "-b2", name + "-a1"}, versionNames(t, store, name, namespace))

	// The controller's own record of the serving spec comes after the imported history, and a
	// rollback reverts to an imported snapshot.
	r := rollbackReconciler(onlinescore.NewMemStore())
	r.Versions = store
	reconcileNN(t, r, name, namespace)
	serving := latestVersionOf(t, name, namespace)
	assert.Equal(t, []string{serving, name + "-b2", name + "-a1"}, versionNames(t, store, name, namespace))

	annotateRollback(t, name, namespace, name+"-a1")
	reconcileNN(t, r, name, namespace)
	got := getDeploy(t, name, namespace)
	assert.Equal(t, "ghcr.io/ctxmesh/example-agent:v1", got.Spec.Image, "the spec reverts to the imported snapshot")
	cond := rolledBackCond(t, name, namespace)
	require.NotNil(t, cond)
	assert.Equal(t, reasonRolledBack, cond.Reason)
}
