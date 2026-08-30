# Tenant-label ValidatingWebhook (audit P1-3, M128/Gate E) — enable-able (no cert-manager)

Closes the tenant-label spoof: only the Tenant controller's ServiceAccount may set/change/**remove** the
`agents.ctxmesh.ai/tenant` namespace label (ownership-not-entitlement, ADR 0102). A principal with
`namespace write` otherwise labels their own namespace into a victim tenant and is treated as intra-tenant
by the victim's NetworkPolicy selectors. Handler: `internal/webhook/tenant_label.go` (+ tests).

**Gate E (M128, [ADR 0102](../../../agent-brain/decisions/0102-ga-gate-e-platform-pki-tenant-webhook-and-egress-tls.md)) removed the cert-manager dependency.** When enabled, the manager's
in-process cert-controller (`open-policy-agent/cert-controller`) generates + rotates the serving cert and
the manager OWNS the ValidatingWebhookConfiguration — created programmatically only after the cert is ready
(`internal/webhook/tenant_vwc.go`: matchConditions-scoped to tenant-labeled writes, system-ns-exempt by the
immutable `kubernetes.io/metadata.name` label, `failurePolicy: Fail`, CREATE+UPDATE). No static VWC manifest.

## Enable it

1. Set `ENABLE_TENANT_LABEL_WEBHOOK=true` + `TENANT_WEBHOOK_CONTROLLER_SA=system:serviceaccount:<ns>:agentry-controller-manager`
   on the controller-manager Deployment (the manager then runs the cert-controller + registers
   `/validate-tenant-label` + creates the VWC after the cert is ready).
2. Wire `service.yaml` here into `config/default/kustomization.yaml` (the webhook Service; the VWC is
   manager-created, not a manifest).

**Default-ON** (enable by default + retire the `security.tenantLabelEnforcement` acknowledgment) is pending
the live blast-radius / cert-kill / uninstall-writable / no-wedge verification on a cluster — the M128
acceptance (accept-m128.sh Section D). See the M128 board.
