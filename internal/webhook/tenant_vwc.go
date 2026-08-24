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
	"context"
	"fmt"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TenantLabelVWCName is the manager-owned ValidatingWebhookConfiguration for the tenant-label
// validator. The manager creates it (M128/Gate E, ADR 0102 §2 — "manager-owned VWC, created only
// after the webhook server is ready") rather than shipping a static manifest, so the namespace +
// exemption list are correct for ANY install namespace with no chart templating, and there is no
// uncertified-VWC window (it is applied only after the cert-controller has the CA).
const TenantLabelVWCName = "tenant-label-validator"

// buildTenantLabelVWC returns the desired VWC for the given install namespace + CA bundle.
// It is pure (no I/O) so the exemption list, matchConditions, rules, and failurePolicy are
// unit-testable. See ADR 0102 §2 for the reasoning behind each guard.
func buildTenantLabelVWC(namespace string, webhookServiceName string, caBundle []byte) *admissionregistrationv1.ValidatingWebhookConfiguration {
	fail := admissionregistrationv1.Fail
	sideEffectsNone := admissionregistrationv1.SideEffectClassNone
	scope := admissionregistrationv1.ClusterScope
	path := "/validate-tenant-label"

	return &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: TenantLabelVWCName,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "agent-engine",
				"app.kubernetes.io/managed-by": "agent-engine-controller-manager",
			},
		},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name:                    "tenant-label.agents.ctxmesh.ai",
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &sideEffectsNone,
			// fail-closed: an unreachable/uncertified webhook DENIES the (narrowly-matched) write.
			FailurePolicy: &fail,
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				CABundle: caBundle,
				Service: &admissionregistrationv1.ServiceReference{
					Name:      webhookServiceName,
					Namespace: namespace,
					Path:      &path,
				},
			},
			// Blast-radius guard #1 — exempt system/control-plane namespaces by the API-server-managed
			// IMMUTABLE name label (ADR 0102: NEVER a spoofable custom exempt label). A cert/webhook
			// outage can then never wedge a write to these namespaces.
			NamespaceSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "kubernetes.io/metadata.name",
					Operator: metav1.LabelSelectorOpNotIn,
					Values:   []string{"kube-system", "kube-node-lease", "kube-public", namespace},
				}},
			},
			// Blast-radius guard #2 (matchConditions, CEL) — invoke ONLY for writes that TOUCH the tenant
			// label (present in object OR oldObject). A dead cert cannot block any ordinary namespace
			// write; evaluating BOTH object + oldObject closes the "drop the label in the same update"
			// bypass. (GA @1.30, beta-on @1.28 — below that floor the API server ignores this field and
			// the namespaceSelector still bounds the radius to non-system namespace writes.)
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "tenant-label-present",
				Expression: "(has(object.metadata.labels) && 'agents.ctxmesh.ai/tenant' in object.metadata.labels) || " +
					"(oldObject != null && has(oldObject.metadata.labels) && 'agents.ctxmesh.ai/tenant' in oldObject.metadata.labels)",
			}},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{
					admissionregistrationv1.Create, admissionregistrationv1.Update,
				},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					// status/finalize too — a label set via either subresource would otherwise bypass.
					Resources: []string{"namespaces", "namespaces/status", "namespaces/finalize"},
					Scope:     &scope,
				},
			}},
		}},
	}
}

// ApplyTenantLabelVWC creates-or-reconciles the manager-owned tenant-label VWC. Call it AFTER the
// cert-controller signals ready (ADR 0102 §2 — no uncertified-VWC window). The caBundle is OWNED by
// the cert-controller (its Webhooks-field injection keeps it current on rotation), so this applier
// PRESERVES the existing caBundle on update and never clobbers it — it reconciles only the manager's
// spec (rules, matchConditions, exemption, failurePolicy). On first create the caBundle is whatever
// the caller passes (nil ⇒ the rotator fills it on its next sync; the matchConditions bound the brief
// empty-caBundle window to non-system tenant-labeled writes, which self-heal on the controller retry).
func ApplyTenantLabelVWC(ctx context.Context, c client.Client, namespace, webhookServiceName string, caBundle []byte) error {
	desired := buildTenantLabelVWC(namespace, webhookServiceName, caBundle)
	var existing admissionregistrationv1.ValidatingWebhookConfiguration
	err := c.Get(ctx, client.ObjectKey{Name: TenantLabelVWCName}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		if cErr := c.Create(ctx, desired); cErr != nil {
			return fmt.Errorf("create tenant-label VWC: %w", cErr)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get tenant-label VWC: %w", err)
	default:
		// Preserve the cert-controller's injected caBundle; reconcile the rest of the spec.
		for i := range desired.Webhooks {
			if i < len(existing.Webhooks) {
				desired.Webhooks[i].ClientConfig.CABundle = existing.Webhooks[i].ClientConfig.CABundle
			}
		}
		existing.Webhooks = desired.Webhooks
		existing.Labels = desired.Labels
		if uErr := c.Update(ctx, &existing); uErr != nil {
			return fmt.Errorf("update tenant-label VWC: %w", uErr)
		}
		return nil
	}
}
