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

// Package webhook holds the engine's admission webhooks. The tenant-label validator (C14, audit P1-3) is the
// first: it makes the `agents.ctxmesh.ai/tenant` namespace label controller-managed, closing the label-spoof
// where a principal with `update namespaces` could label their own namespace into a victim tenant and be
// treated as intra-tenant by the victim's own NetworkPolicy selectors.
package webhook

import (
	"context"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// tenantLabel is the controller-managed namespace→tenant membership label (one source of truth with the
// Tenant controller + the state-layer tenant resolver).
const tenantLabel = agentsv1alpha1.TenantLabel

// TenantLabelValidator is a ValidatingWebhook on namespaces: it DENIES a create/update that changes the
// tenant label — set, change, OR remove — unless the request comes from the Tenant controller's
// ServiceAccount. **Ownership, not entitlement** (M128/Gate E, ADR 0102): tenant (re)assignment is
// controller-mediated ONLY — a user changes a namespace's tenant by editing the cluster-scoped, RBAC-guarded
// Tenant CR, never by editing the namespace label directly. Removal is denied too because it detaches the
// namespace from tenant quota scoping (a downgrade, not cleanup). This is the admission half of tenant
// isolation — the always-on system-namespace denylist (m90.3) is the other; together they stop a Tenant from
// fencing the cluster or a principal from joining/leaving a tenant out-of-band.
type TenantLabelValidator struct {
	decoder admission.Decoder
	// controllerUsername is the Tenant controller's SA username (system:serviceaccount:<ns>:<sa>) — the ONLY
	// principal allowed to set/change the tenant label.
	controllerUsername string
}

// NewTenantLabelValidator builds the validator. controllerUsername is the Tenant controller's SA username.
func NewTenantLabelValidator(decoder admission.Decoder, controllerUsername string) *TenantLabelValidator {
	return &TenantLabelValidator{decoder: decoder, controllerUsername: controllerUsername}
}

// Handle implements admission.Handler.
func (v *TenantLabelValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	// The Tenant controller owns the label — it may always set it.
	if v.controllerUsername != "" && req.UserInfo.Username == v.controllerUsername {
		return admission.Allowed("tenant controller manages the tenant label")
	}

	var newNs corev1.Namespace
	if err := v.decoder.Decode(req, &newNs); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	newVal := newNs.Labels[tenantLabel]

	var oldVal string
	if len(req.OldObject.Raw) > 0 {
		var oldNs corev1.Namespace
		if err := v.decoder.DecodeRaw(req.OldObject, &oldNs); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		oldVal = oldNs.Labels[tenantLabel]
	}

	// Deny ANY change to the tenant label by a non-controller — set, change, OR remove (ADR 0102:
	// ownership-not-entitlement; removal detaches quota scoping, a downgrade). A no-op (unchanged,
	// incl. a create/update that never touches the label) is allowed.
	if newVal != oldVal {
		verb := "set"
		switch {
		case oldVal != "" && newVal == "":
			verb = "remove"
		case oldVal != "" && newVal != "":
			verb = "change"
		}
		return admission.Denied(fmt.Sprintf(
			"the %q namespace label is controller-managed (audit P1-3, ADR 0102): only the Tenant controller may set/change/remove it — reassign the namespace via the Tenant resource. You tried to %s it (old=%q new=%q)",
			tenantLabel, verb, oldVal, newVal))
	}
	return admission.Allowed("")
}

// SetupTenantLabelWebhook registers the tenant-label validator on the manager's webhook server at
// /validate-tenant-label. It is inert until a ValidatingWebhookConfiguration (config/webhook) points the API
// server at it — which requires webhook serving certs (cert-manager or mounted), a user-gated deployment step.
func SetupTenantLabelWebhook(mgr ctrl.Manager, controllerUsername string) {
	v := NewTenantLabelValidator(admission.NewDecoder(mgr.GetScheme()), controllerUsername)
	mgr.GetWebhookServer().Register("/validate-tenant-label", &admission.Webhook{Handler: v})
}
