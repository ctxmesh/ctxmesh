# platform-edge — Gateway API edge (ADR 0038, M36)

A small, values-driven Helm chart: **one host-routed edge** in front of the whole platform, so the
console / Langfuse / agents are reached by **domain** instead of per-service `kubectl port-forward`.
Non-destructive — Knative/Kourier is untouched: agent traffic is routed *through* Kourier by Host, so
Knative keeps its own routing.

**Same manifests local→prod; only values differ** (`baseDomain`, `tls.enabled`, `dns.externalDns`) —
config differs, not shape (ADR 0038 — the code never branches on environment).

## Routes (from `values.yaml`, host = `<subdomain>.<baseDomain>`)

| Host (local default) | Path | Backend |
|----------------------|------|---------|
| `console.127.0.0.1.sslip.io` | all | `agent-engine-bff` (BFF) |
| `langfuse.127.0.0.1.sslip.io` | all | `langfuse-web` |
| `*.default.127.0.0.1.sslip.io` | `/invoke` (Exact) | `kourier-internal` → Knative routes by Host to the agent (ext-auth guarded) |
| `*.default.127.0.0.1.sslip.io` | everything else | `agent-engine-bff` (BFF) — the per-agent **chatbox** SPA + `/api/*` |

The agent host is **path-split** across two `HTTPRoute`s (m37.3, `agentEdge` values): the machine
`/invoke` endpoint → the agent (behind ext-auth), and everything else → the BFF, which serves a
chatbox pinned to that agent (see below). Each `HTTPRoute` renders in its backend's namespace
(route+backend same ns ⇒ no `ReferenceGrant`), attached to the shared `Gateway`
(`envoy-gateway-system/platform-edge`, `allowedRoutes: from All`).

## Per-agent chatbox at the agent host (m37.3)

A browser hitting an agent's own hostname (`<agent>.default.<baseDomain>/`) gets a **chrome-less,
single-agent chatbox** — the same chat as the console, pinned to that one agent, with its own login.
Mechanics: the `agents-app` route sets `X-Ctxmesh-Agent-Chatbox` so the BFF injects
`<meta name="agent-pin" content="ns/name">` into the SPA shell (a meta, not a script — the CSP forbids
inline scripts); the SPA reads it and mounts a **chatbox-only** app (the operator-console router is
never mounted, so the console isn't reachable at agent origins). Auth is the SPA's own bearer login —
`sessionStorage` is per-origin, so the agent origin logs in independently (no cross-origin token
sharing). The MCP-consent OAuth callback resolves at the agent origin too (`/api/*` → BFF).

> The SPA is chatbox-only, but `/api/*` is reachable at agent origins under the same bearer token +
> caller RBAC (no privilege escalation — a token can only reach what it already could). Restricting the
> API surface at agent origins to just the chatbox's calls is a hardening follow-on.

## Authenticated agent edge (ext-auth, ADR 0039)

`extAuth.enabled` (default **on**) puts an Envoy `SecurityPolicy` in front of the **agents** route: Envoy
calls the BFF (`/api/extauth`) for every request to an agent hostname. The BFF verifies the caller's
bearer token (missing/invalid ⇒ **401**, Envoy denies), mints the **run capability** for that user + the
agent's trust boundary, and returns it in `X-Ctxmesh-Run-Capability`, which Envoy injects **upstream**.
So `curl -H "Authorization: Bearer <token>" <agent>.<ns>.<domain>/invoke` gets the **same OBO** as the
console's `POST /api/invoke` — the agent URL is a first-class authenticated endpoint. The agent still only
**relays** the capability (never forges it), so the ADR 0033 model holds end to end; one place mints (the
BFF), and the signing key never leaves it.

Because the agents route is in `kourier-system` and the BFF in `agent-engine-system`, the chart also
renders the cross-namespace `ReferenceGrant` (SecurityPolicy → BFF Service). Set `extAuth.enabled: false`
to leave agent URLs unauthenticated (no OBO — the pre-ADR-0039 behaviour).

> **Envoy `path_prefix` gotcha:** Envoy's HTTP ext_authz *prepends* the configured `path` to the original
> request path, so a client `POST /invoke` reaches the BFF as `POST /api/extauth/invoke`. The BFF matches
> the whole `/api/extauth/` subtree, not just the exact path — verified live on kind.

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

**Agent exposure:** external *invocation* can go through the authenticated BFF (`POST /api/invoke`) **or**
the agent's own hostname — both now enforce the same auth. The direct per-agent hostnames (the escape
hatch for the standalone chatbox, cross-cluster A2A, webhooks) are guarded by the **ext-auth edge** above,
which reuses the capability/OBO model (ADR 0033/0039). Agent ksvcs stay cluster-internal; the edge is the
trust boundary.
