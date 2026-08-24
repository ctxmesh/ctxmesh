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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// This file holds the per-agent default-token hardening tests (m79.4), the
// name-length CEL guard, and the shared env-assertion helpers. They previously
// lived in memorybinding_controller_test.go, which was removed when the
// MemoryBinding CRD was retired (M127, ADR 0101); the memory-attached agents they
// need now express memory via the folded AgentDeployment.spec.sessionMemory field
// (setAgentSessionMemory), not a MemoryBinding object.

// statelayerTokenVolumeMounted reports whether the ksvc pod template carries the
// audience-scoped PROJECTED state-layer proxy token volume — the dedicated auth path
// that must remain UNAFFECTED by the m79.4 default-token hardening.
func statelayerTokenVolumeMounted(ksvc *servingv1.Service) bool {
	for _, v := range ksvc.Spec.Template.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.ServiceAccountToken != nil && src.ServiceAccountToken.Audience == statelayerPodAudience {
				return true
			}
		}
	}
	return false
}

// TestMountServiceAccountToken_HardenedByDefault (m79.4, m52 C10): a proxy-attached
// agent's per-agent identity SA has AutomountServiceAccountToken=false by DEFAULT
// (spec.mountServiceAccountToken unset), stripping the unused default kube-API token
// from the pod. The dedicated projected proxy-token volume is unaffected.
func TestMountServiceAccountToken_HardenedByDefault(t *testing.T) {
	const (
		namespace = "default"
		agentName = "msat-default-agent"
		proxyURL  = "http://statelayer-proxy.agent-engine-system.svc:8080"
	)
	mkAgent(t, agentName, namespace)
	setAgentSessionMemory(t, namespace, agentName, "session", "")

	r := newReconciler()
	r.StatelayerProxyURL = proxyURL
	reconcileNN(t, r, agentName, namespace)

	// The identity SA has automount explicitly disabled — the hardened default.
	wantSA := agentIdentitySAName(agentName)
	var sa corev1.ServiceAccount
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: wantSA, Namespace: namespace}, &sa),
		"the per-agent identity ServiceAccount is created")
	require.NotNil(t, sa.AutomountServiceAccountToken,
		"AutomountServiceAccountToken must be explicitly SET (not nil/API-default) so the default token is stripped")
	assert.False(t, *sa.AutomountServiceAccountToken,
		"default (spec.mountServiceAccountToken unset) ⇒ AutomountServiceAccountToken=false (hardened)")

	// The DEDICATED projected proxy-token volume is unaffected — memory-auth still works.
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc))
	assert.True(t, statelayerTokenVolumeMounted(&ksvc),
		"the audience-scoped projected proxy-token volume must remain mounted (memory-auth path unaffected)")
	// And the ksvc PodSpec itself carries NO automount setting — the mechanism is the SA,
	// NOT the Knative-restricted RevisionSpec.PodSpec (the m5.7 landmine class).
	assert.Nil(t, ksvc.Spec.Template.Spec.AutomountServiceAccountToken,
		"automount must NOT be set on the ksvc PodSpec (Knative strips/rejects it) — it lives on the SA")
}

// TestMountServiceAccountToken_OptIn (m79.4): with spec.mountServiceAccountToken=true,
// the identity SA's AutomountServiceAccountToken is true — the escape hatch for an agent
// that legitimately builds an in-cluster kube config. The projected proxy-token volume is
// still mounted (the two token paths are independent).
func TestMountServiceAccountToken_OptIn(t *testing.T) {
	const (
		namespace = "default"
		agentName = "msat-optin-agent"
		proxyURL  = "http://statelayer-proxy.agent-engine-system.svc:8080"
	)
	a := mkAgent(t, agentName, namespace)
	a.Spec.MountServiceAccountToken = ptr.To(true)
	require.NoError(t, k8sClient.Update(testCtx, a))
	setAgentSessionMemory(t, namespace, agentName, "session", "")

	r := newReconciler()
	r.StatelayerProxyURL = proxyURL
	reconcileNN(t, r, agentName, namespace)

	wantSA := agentIdentitySAName(agentName)
	var sa corev1.ServiceAccount
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: wantSA, Namespace: namespace}, &sa))
	require.NotNil(t, sa.AutomountServiceAccountToken)
	assert.True(t, *sa.AutomountServiceAccountToken,
		"spec.mountServiceAccountToken=true ⇒ AutomountServiceAccountToken=true (opt-in escape hatch)")

	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc))
	assert.True(t, statelayerTokenVolumeMounted(&ksvc),
		"the projected proxy-token volume is independent of the default-token opt-in and stays mounted")
}

// TestMountServiceAccountToken_ToggleRollsRevision (m79.4): toggling
// spec.mountServiceAccountToken changes the pod-template structural digest, so a NEW
// Knative revision name is produced — the SA-automount change actually reaches a running
// pod (the M4 silent-loss landmine: a pod-spec change that doesn't move the revision name
// is dropped by the CreateOrUpdate name-guard). It also re-converges the identity SA.
func TestMountServiceAccountToken_ToggleRollsRevision(t *testing.T) {
	const (
		namespace = "default"
		agentName = "msat-toggle-agent"
		proxyURL  = "http://statelayer-proxy.agent-engine-system.svc:8080"
	)
	a := mkAgent(t, agentName, namespace)
	setAgentSessionMemory(t, namespace, agentName, "session", "")

	r := newReconciler()
	r.StatelayerProxyURL = proxyURL

	// Reconcile hardened-default → capture the revision name + SA automount.
	reconcileNN(t, r, agentName, namespace)
	var ksvc servingv1.Service
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc))
	revBefore := ksvc.Spec.Template.Name
	require.NotEmpty(t, revBefore, "the revision has a stable structural name")

	var saBefore corev1.ServiceAccount
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentIdentitySAName(agentName), Namespace: namespace}, &saBefore))
	require.NotNil(t, saBefore.AutomountServiceAccountToken)
	assert.False(t, *saBefore.AutomountServiceAccountToken, "starts hardened (false)")

	// Toggle the opt-in ON and re-reconcile.
	require.NoError(t, k8sClient.Get(testCtx, client.ObjectKeyFromObject(a), a))
	a.Spec.MountServiceAccountToken = ptr.To(true)
	require.NoError(t, k8sClient.Update(testCtx, a))
	reconcileNN(t, r, agentName, namespace)

	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentName, Namespace: namespace}, &ksvc))
	revAfter := ksvc.Spec.Template.Name
	assert.NotEqual(t, revBefore, revAfter,
		"toggling spec.mountServiceAccountToken must roll a NEW revision (structural digest changed)")

	var saAfter corev1.ServiceAccount
	require.NoError(t, k8sClient.Get(testCtx,
		types.NamespacedName{Name: agentIdentitySAName(agentName), Namespace: namespace}, &saAfter))
	require.NotNil(t, saAfter.AutomountServiceAccountToken)
	assert.True(t, *saAfter.AutomountServiceAccountToken,
		"the identity SA re-converges to automount=true after the opt-in toggle")

	// The projected proxy-token volume (memory-auth path) is unaffected across the toggle.
	assert.True(t, statelayerTokenVolumeMounted(&ksvc),
		"the projected proxy-token volume stays mounted across a default-token opt-in toggle")
}

// TestAgentDeployment_NameLengthCELGuard pins the CRD's name-length CEL guard: a
// 45-char name is rejected at admission with the revision-name-budget message, and
// a 44-char name is admitted.
func TestAgentDeployment_NameLengthCELGuard(t *testing.T) {
	const namespace = "default"

	mk := func(name string) *agentsv1alpha1.AgentDeployment {
		return &agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: agentsv1alpha1.AgentDeploymentSpec{
				Image:          "ghcr.io/ctxmesh/example-agent:latest",
				ExecutionModel: "serving",
				Port:           8080,
			},
		}
	}

	// 45 chars → rejected with the budget message.
	longName := strings.Repeat("a", 45)
	err := k8sClient.Create(testCtx, mk(longName))
	require.Error(t, err, "45-char name must be rejected at admission")
	assert.Contains(t, err.Error(), "44 characters",
		"rejection must carry the revision-name budget message")

	// 44 chars → admitted.
	okName := strings.Repeat("a", 44)
	okDeploy := mk(okName)
	require.NoError(t, k8sClient.Create(testCtx, okDeploy),
		"44-char name must be admitted")
	t.Cleanup(func() { _ = k8sClient.Delete(testCtx, okDeploy) })
}

// envByName converts a container env slice to a map of name → value for assertions.
// Only plain-value entries are included; ValueFrom entries are keyed by name with
// a sentinel so the caller can assert presence or specific type. Handles every
// valueFrom shape (fieldRef/secretKeyRef/configMapKeyRef/resourceFieldRef)
// without assuming a specific one is set — agent ksvcs carry no valueFrom today,
// but a future non-fieldRef source must not nil-panic this helper.
func envByName(env []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		switch {
		case e.ValueFrom == nil:
			m[e.Name] = e.Value
		case e.ValueFrom.FieldRef != nil:
			m[e.Name] = fmt.Sprintf("<fieldRef:%s>", e.ValueFrom.FieldRef.FieldPath)
		default:
			m[e.Name] = "<valueFrom>"
		}
	}
	return m
}

// countEnv returns how many times name appears in the env slice — used to assert
// an env var (e.g. AGENT_NAME, which both the memory and registry paths may
// inject) lands EXACTLY once (no double-injection).
func countEnv(env []corev1.EnvVar, name string) int {
	n := 0
	for _, e := range env {
		if e.Name == name {
			n++
		}
	}
	return n
}
