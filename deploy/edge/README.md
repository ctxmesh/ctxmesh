# Edge gateway (Gateway API / Envoy Gateway) — ADR 0038, M36

One host-routed edge in front of the whole platform, so console / Langfuse / agents are reached by
**domain** instead of per-service `kubectl port-forward`. Non-destructive: Knative/Kourier is untouched
— agent traffic is routed *through* Kourier by Host, so Knative keeps its own routing.

Base domain here is `127.0.0.1.sslip.io` (resolves to loopback everywhere, no `/etc/hosts`). In
production this is a values-driven `baseDomain` (e.g. `agents.example.com`) and the gateway Service is a
real `LoadBalancer`; the manifests are otherwise identical (ADR 0038 — config differs, not the shape).

## Routes (`gateway.yaml`)

| Host | Backend |
|------|---------|
| `console.127.0.0.1.sslip.io` | `agent-engine-bff` (BFF) |
| `langfuse.127.0.0.1.sslip.io` | `langfuse-web` |
| `*.default.127.0.0.1.sslip.io` | `kourier-internal` → Knative routes by Host to the agent |

Each `HTTPRoute` lives in its backend's namespace (no `ReferenceGrant`); the `Gateway`
(`envoy-gateway-system/platform-edge`) allows routes `from: All`.

## Install (local kind)

```sh
# 1. Envoy Gateway (brings the Gateway API CRDs). Do NOT pre-install the CRDs separately —
#    EG owns them; a kubectl pre-apply causes a field-manager conflict.
helm install eg oci://docker.io/envoyproxy/gateway-helm --version v1.2.4 \
  -n envoy-gateway-system --create-namespace
kubectl -n envoy-gateway-system rollout status deploy/envoy-gateway

# 2. GatewayClass + Gateway + HTTPRoutes
kubectl apply -f deploy/edge/gateway.yaml

# 3. Publish the edge on the host. kind has no LoadBalancer provider, so the Gateway shows
#    Programmed=False (AddressNotAssigned) — harmless; the data-plane Envoy is Running. Reach it via:
SVC=$(kubectl get svc -n envoy-gateway-system -o name | grep platform-edge)
kubectl port-forward -n envoy-gateway-system "$SVC" 8888:80
#    → http://console.127.0.0.1.sslip.io:8888/ , http://langfuse.127.0.0.1.sslip.io:8888/ ,
#      http://<agent>.default.127.0.0.1.sslip.io:8888/
#    For clean :80 URLs (no port): `sudo kubectl port-forward … 80:80`, or run cloud-provider-kind
#    so the LoadBalancer gets a real IP. For a pristine :80 that matches prod, recreate the kind
#    cluster with extraPortMappings (destructive — wipes cluster state).
```

Wire the BFF's Langfuse link-out at the edge domain so "view full trace" resolves in a browser:
`LANGFUSE_UI_URL=http://langfuse.127.0.0.1.sslip.io:8888` (ADR 0038 internal/external URL split, m36.1).

## Follow-ons (M36 board)
- Templatize the Gateway + HTTPRoutes as **values-gated chart templates** (`gateway.enabled`,
  `baseDomain`) with `make helm-verify` no-drift.
- Prod seams (m36.4): TLS via cert-manager (wildcard), DNS via external-dns, `Gateway.service.type:
  LoadBalancer`. External agent invocation goes through the authenticated BFF; direct per-agent hosts
  are opt-in + capability/mTLS auth (ADR 0033).
