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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

func reconcileAgentForKsvc(t *testing.T, r *AgentDeploymentReconciler, name string) servingv1.Service {
	t.Helper()
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       agentsv1alpha1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest", ExecutionModel: "serving", Port: 8080},
	}
	require.NoError(t, k8sClient.Create(testCtx, deploy))
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, deploy) })
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(deploy), deploy))
	reconcileNN(t, r, name, "default")
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx, types.NamespacedName{Name: name, Namespace: "default"}, &ksvc))
	return ksvc
}

// C8 (ADR 0079): with LAUNCHER_IMAGE configured, the ksvc gets the launcher-inject initContainer + the shared
// emptyDir, and the user container's Command is overridden to exec the staged launcher.
func TestReconcile_LauncherInjection(t *testing.T) {
	const launcherImage = "ghcr.io/ctxmesh/launcher@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	r := newReconciler()
	r.LauncherImage = launcherImage

	ksvc := reconcileAgentForKsvc(t, r, "inject-agent")
	spec := ksvc.Spec.Template.Spec

	// initContainer: launcher-inject, the pinned image, the --install self-copy command, hardened + bounded.
	require.Len(t, spec.InitContainers, 1, "the launcher-inject initContainer must be injected")
	ic := spec.InitContainers[0]
	assert.Equal(t, "launcher-inject", ic.Name)
	assert.Equal(t, launcherImage, ic.Image, "the initContainer uses the pinned LAUNCHER_IMAGE")
	assert.Equal(t, []string{"/launcher", "--install", "/platform/launcher"}, ic.Command)
	require.NotNil(t, ic.SecurityContext)
	assert.True(t, *ic.SecurityContext.RunAsNonRoot, "hardened: runAsNonRoot")
	assert.True(t, *ic.SecurityContext.ReadOnlyRootFilesystem, "hardened: readOnlyRootFilesystem")
	require.NotNil(t, ic.SecurityContext.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, ic.SecurityContext.Capabilities.Drop, "hardened: drop ALL caps")
	assert.False(t, ic.Resources.Requests.Cpu().IsZero(), "resource-bounded so restricted/quota'd namespaces admit it")

	// the shared emptyDir volume, size-capped.
	vol := func() *corev1.Volume {
		for i := range spec.Volumes {
			if spec.Volumes[i].Name == "platform-launcher" {
				return &spec.Volumes[i]
			}
		}
		return nil
	}()
	require.NotNil(t, vol, "the platform-launcher emptyDir must be present")
	require.NotNil(t, vol.EmptyDir, "platform-launcher must be an emptyDir")
	require.NotNil(t, vol.EmptyDir.SizeLimit, "the emptyDir must be size-capped")

	// the user container: Command overridden to exec the staged launcher + a read-only /platform mount.
	uc, ok := containerByName(spec.Containers, "user-container")
	require.True(t, ok, "the user container must exist")
	assert.Equal(t, []string{"/platform/launcher"}, uc.Command, "the user container execs the staged launcher")
	mount := func() *corev1.VolumeMount {
		for i := range uc.VolumeMounts {
			if uc.VolumeMounts[i].Name == "platform-launcher" {
				return &uc.VolumeMounts[i]
			}
		}
		return nil
	}()
	require.NotNil(t, mount, "the user container must mount the staged launcher")
	assert.Equal(t, "/platform", mount.MountPath)
	assert.True(t, mount.ReadOnly, "the user container mounts the launcher read-only")

	// The collector sidecar's Command is NOT clobbered (we target the user container by name).
	if col, ok := containerByName(spec.Containers, "collector"); ok {
		assert.Nil(t, col.Command, "the collector sidecar Command must not be touched by injection")
	}
}

// Default (no LAUNCHER_IMAGE): NO injection — fully backward-compatible with baked-launcher images.
func TestReconcile_NoLauncherInjection_Default(t *testing.T) {
	r := newReconciler() // LauncherImage unset
	ksvc := reconcileAgentForKsvc(t, r, "no-inject-agent")
	spec := ksvc.Spec.Template.Spec

	assert.Empty(t, spec.InitContainers, "no initContainer without LAUNCHER_IMAGE")
	uc, ok := containerByName(spec.Containers, "user-container")
	require.True(t, ok)
	assert.Nil(t, uc.Command, "the user container runs on its image ENTRYPOINT (no Command override) by default")
	for _, v := range spec.Volumes {
		assert.NotEqual(t, "platform-launcher", v.Name, "no platform-launcher volume without LAUNCHER_IMAGE")
	}
}
