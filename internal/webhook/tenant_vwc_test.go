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

package webhook

import (
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildTenantLabelVWC_Guards(t *testing.T) {
	vwc := buildTenantLabelVWC("agentry", "webhook-service", []byte("CA"))
	require.Len(t, vwc.Webhooks, 1)
	w := vwc.Webhooks[0]

	// fail-closed.
	require.NotNil(t, w.FailurePolicy)
	assert.Equal(t, admissionregistrationv1.Fail, *w.FailurePolicy, "failurePolicy must be Fail (fail-closed)")

	// clientConfig → the webhook Service in the install namespace, caBundle set.
	require.NotNil(t, w.ClientConfig.Service)
	assert.Equal(t, "webhook-service", w.ClientConfig.Service.Name)
	assert.Equal(t, "agentry", w.ClientConfig.Service.Namespace)
	assert.Equal(t, []byte("CA"), w.ClientConfig.CABundle)

	// Blast-radius guard #1 — immutable-name exemption INCLUDING the install namespace.
	require.NotNil(t, w.NamespaceSelector)
	require.Len(t, w.NamespaceSelector.MatchExpressions, 1)
	exempt := w.NamespaceSelector.MatchExpressions[0]
	assert.Equal(t, "kubernetes.io/metadata.name", exempt.Key,
		"exemption must key on the API-server-managed IMMUTABLE name label, never a spoofable custom label")
	assert.Equal(t, metav1.LabelSelectorOpNotIn, exempt.Operator)
	assert.Contains(t, exempt.Values, "kube-system")
	assert.Contains(t, exempt.Values, "agentry", "the install namespace must be exempt (no self-wedge)")

	// Blast-radius guard #2 — matchConditions evaluating BOTH object + oldObject.
	require.Len(t, w.MatchConditions, 1)
	expr := w.MatchConditions[0].Expression
	assert.Contains(t, expr, "object.metadata.labels")
	assert.Contains(t, expr, "oldObject", "must evaluate oldObject too (closes the drop-the-label bypass)")
	assert.Contains(t, expr, "agents.ctxmesh.ai/tenant")

	// Rules — CREATE + UPDATE on namespaces (+ subresources), cluster scope.
	require.Len(t, w.Rules, 1)
	assert.ElementsMatch(t,
		[]admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
		w.Rules[0].Operations, "must include CREATE (a born-labeled namespace is a claimed tenant)")
	assert.Contains(t, w.Rules[0].Resources, "namespaces")
	assert.Contains(t, w.Rules[0].Resources, "namespaces/finalize")
}

func TestBuildTenantLabelVWC_ExemptionTracksInstallNamespace(t *testing.T) {
	// A non-default install namespace must be the one exempted (no hardcoded namespace).
	vwc := buildTenantLabelVWC("acme-ctrl", "webhook-service", nil)
	exempt := vwc.Webhooks[0].NamespaceSelector.MatchExpressions[0]
	assert.Contains(t, exempt.Values, "acme-ctrl")
	assert.NotContains(t, exempt.Values, "agentry")
}
