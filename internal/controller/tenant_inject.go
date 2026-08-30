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
	"crypto/sha256"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
)

// tenantContext is the resolved tenant identity + model caps for an agent's namespace (ADR 0046, M47).
// The caps are injected into the agent pod (TENANT_*) and enforced by the launcher gateway proxy (m47.4).
type tenantContext struct {
	id            string
	budgetUSD     string
	rpm           int32
	maxConcurrent int32
}

// hasModelCaps reports whether the tenant carries any model cap the launcher gateway proxy must
// enforce (budget / rate / concurrency) — the gate for interposing the proxy + injecting TENANT_QUOTA_ADDR.
func (tc tenantContext) hasModelCaps() bool {
	return tc.budgetUSD != "" || tc.rpm > 0 || tc.maxConcurrent > 0
}

// resolveTenantForNamespace returns the Tenant that owns ns and its model caps, read from the
// AUTHORITATIVE namespace label the Tenant controller stamps (tenantLabel) — NOT re-derived from Tenant
// specs, so an agent can never be injected with a tenant the controller has not actually reconciled onto
// its namespace (avoids a race + a double-claim ambiguity). found=false when no tenant owns the namespace.
func resolveTenantForNamespace(ctx context.Context, c client.Client, ns string) (tenantContext, bool, error) {
	var namespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: ns}, &namespace); err != nil {
		return tenantContext{}, false, client.IgnoreNotFound(err)
	}
	name := namespace.Labels[tenantLabel]
	if name == "" {
		return tenantContext{}, false, nil
	}
	var t agentsv1alpha1.Tenant
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &t); err != nil {
		// The label points at a tenant that no longer exists (mid-prune) — treat as no tenant.
		return tenantContext{}, false, client.IgnoreNotFound(err)
	}
	tc := tenantContext{id: t.Name}
	if t.Spec.Model != nil {
		tc.budgetUSD = t.Spec.Model.BudgetUSD
		tc.rpm = t.Spec.Model.RPM
		tc.maxConcurrent = t.Spec.Model.MaxConcurrent
	}
	return tc, true, nil
}

// tenantDigest captures the injected tenant identity + model caps so a tenant assignment or a cap change
// rolls a new revision (the M8/M6 digest-component pattern). "" when the namespace has no tenant.
func tenantDigest(tc tenantContext, found bool) string {
	if !found {
		return ""
	}
	payload := fmt.Sprintf("id=%s;budget=%s;rpm=%d;conc=%d", tc.id, tc.budgetUSD, tc.rpm, tc.maxConcurrent)
	h := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", h[:])[:8]
}
