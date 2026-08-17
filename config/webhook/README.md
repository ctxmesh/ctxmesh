# Tenant-label ValidatingWebhook (C14) — opt-in

Closes audit P1-3's label-spoof: only the Tenant controller's ServiceAccount may set or change the
`agents.ctxmesh.ai/tenant` namespace label (a principal with `update namespaces` otherwise labels their own
namespace into a victim tenant and is treated as intra-tenant by the victim's NetworkPolicy selectors).

The **logic** ships + is unit-tested (`internal/webhook/tenant_label.go`, `..._test.go`). Activation is a
**user-gated deploy step** — the base install wires no webhook cert infra:

1. Install cert-manager (or mount serving certs) + a `Certificate` for the webhook Service.
2. Set `ENABLE_TENANT_LABEL_WEBHOOK=true` + `TENANT_WEBHOOK_CONTROLLER_SA=<controller SA username>` on the
   controller-manager Deployment.
3. Wire this dir into `config/default/kustomization.yaml` (with the cert-manager caBundle injection).

Carded: m52.C14.
