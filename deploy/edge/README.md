# platform-edge — Gateway API edge (ADR 0038, M36)

A small, values-driven Helm chart: **one host-routed edge** in front of the whole platform, so the
console / Langfuse / agents are reached by **domain** instead of per-service `kubectl port-forward`.
Non-destructive — Knative/Kourier is untouched: agent traffic is routed *through* Kourier by Host, so
Knative keeps its own routing.

**Same manifests local→prod; only values differ** (`baseDomain`, `tls.enabled`, `dns.externalDns`) —
config differs, not shape (ADR 0038 — the code never branches on environment).

## Routes (from `values.yaml`, host = `<subdomain>.<baseDomain>`)

| Host (local default) | Backend |
|----------------------|---------|
| `console.127.0.0.1.sslip.io` | `agent-engine-bff` (BFF) |
| `langfuse.127.0.0.1.sslip.io` | `langfuse-web` |
| `*.default.127.0.0.1.sslip.io` | `kourier-internal` → Knative routes by Host to the agent |

Each `HTTPRoute` renders in its backend's namespace (route+backend same ns ⇒ no `ReferenceGrant`),
attached to the shared `Gateway` (`envoy-gateway-system/platform-edge`, `allowedRoutes: from All`).

## Install (local kind)

```sh
# 1. A Gateway API implementation. Envoy Gateway brings the Gateway API CRDs — do NOT pre-apply the
#    CRDs separately (a kubectl pre-apply causes a field-manager conflict with EG's helm).
helm install eg oci://docker.io/envoyproxy/gateway-helm --version v1.2.4 \
  -n envoy-gateway-system --create-namespace
kubectl -n envoy-gateway-system rollout status deploy/envoy-gateway

# 2. The edge (this chart). Defaults = local (sslip.io, HTTP, no DNS controller).
helm install platform-edge deploy/edge

# 3. Publish the edge on the host. kind has no LoadBalancer provider, so the Gateway shows
#    Programmed=False (AddressNotAssigned) — harmless; the data-plane Envoy is Running. Reach it via:
SVC=$(kubectl get svc -n envoy-gateway-system -o name | grep platform-edge)
kubectl port-forward -n envoy-gateway-system "$SVC" 8888:80
#    → http://console.127.0.0.1.sslip.io:8888/ , http://langfuse.127.0.0.1.sslip.io:8888/ ,
#      http://<agent>.default.127.0.0.1.sslip.io:8888/invoke
#    Clean :80 (no port): `sudo kubectl port-forward … 80:80`, or run cloud-provider-kind for a real
#    LoadBalancer IP, or (destructive, later) recreate kind with extraPortMappings.
```

Wire the BFF's Langfuse link-out at the edge domain so "view full trace" resolves in a browser
(ADR 0038 internal/external split, m36.1):
`LANGFUSE_UI_URL=http://langfuse.127.0.0.1.sslip.io:8888`.

## Production

```sh
helm install platform-edge deploy/edge \
  --set baseDomain=agents.example.com \
  --set tls.enabled=true --set tls.issuerRef.name=letsencrypt-prod \
  --set dns.externalDns=true
```

This turns on the HTTPS listener + a cert-manager wildcard `Certificate`, and annotates the `Gateway`
for `external-dns` — both are **cluster controllers installed out of band** (the chart references them,
it does not install them; documented seams like state-layer HA / signed images in
[specs/deployment.md](../../../agent-brain/specs/deployment.md)). The Gateway Service is a real
`LoadBalancer` fronted by a cloud LB; DNS points `*.agents.example.com` at it. The manifests are
otherwise identical to local.

**Agent exposure:** external *invocation* goes through the authenticated BFF (`POST /api/invoke`) —
agent ksvcs stay cluster-internal. The direct per-agent hostnames above are the escape hatch for
cross-cluster A2A / webhooks and MUST enforce mutual auth (reuse the capability/OBO model, ADR 0033).
