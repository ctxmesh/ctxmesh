# Upgrading ctxmesh

ctxmesh installs and upgrades as a Helm chart published as an OCI artifact. The chart's
`appVersion` is the product version; every first-party image carries the same tag, so an
install is reproducible and an upgrade is a single version change.

## Check what you are running

```sh
helm -n ctxmesh list
```

The `CHART` column is the chart version (`ctxmesh-0.1.0-beta.1`); `APP VERSION` is the
product version the images carry (`v0.1.0-beta.1`).

## Upgrade

```sh
helm upgrade ctxmesh oci://ghcr.io/ctxmesh/charts/ctxmesh \
  --version 0.1.0-beta.2 \
  --namespace ctxmesh \
  --wait --timeout 8m
```

`--wait` matters: it holds the command open until the rolled workloads are ready and the
post-upgrade preflight has run, so a failed upgrade reports as a failed command rather
than as a healthy-looking release with a crash-looping pod behind it.

Re-pass any `--set` flags your install uses. Helm carries stored values forward, but a
flag you passed at install time and omit here still applies — being explicit is what
makes the upgrade reproducible, and `helm get values ctxmesh -n ctxmesh` prints what the
current release is actually using.

## What is preserved

- **Your data.** The bundled Postgres keeps its PersistentVolumeClaim across upgrades;
  the chart never deletes it. ToolRegistries, MCP registrations, run history and the
  knowledge store are Postgres-backed and survive.
- **Your custom resources.** AgentDeployments, ModelRoutes, SecretBindings and the rest
  live in etcd and are untouched by a chart upgrade.
- **Your provider credentials.** The Secrets holding API keys are created by the connect
  flow, not by the chart, so an upgrade does not touch them.

The upgrade path is exercised on every release by `e2e-upgrade.sh`, which installs the
previous version from its published artifacts, writes real data through the product's own
API, runs **the command above**, and reads the data back. An upgrade on an empty cluster
proves nothing, so it is never tested that way.

## What to check afterwards

```sh
kubectl -n ctxmesh get pods
helm -n ctxmesh list
```

Every pod `Running`, and `APP VERSION` showing the version you upgraded to. If the
preflight Job failed, read it — it names the specific dependency it could not reach:

```sh
kubectl -n ctxmesh logs job/ctxmesh-preflight
```

## Rolling back

```sh
helm -n ctxmesh rollback ctxmesh
```

This returns the workloads to the previous release. It does **not** roll back data:
anything written since the upgrade stays written, and a rollback across a future release
that migrates the schema is not supported — check the release notes for that release
before upgrading, not after.

## Version skew

Upgrade one minor version at a time. Skipping versions is untested, and a schema
migration that assumes the immediately previous shape has no way to tell you it was
skipped.

## M162 — the agent container is hardened by default (BREAKING for root images)

**What changed.** The agent's own container now gets a `securityContext`: `runAsNonRoot`, no
privilege escalation, all capabilities dropped, `RuntimeDefault` seccomp. Before this release it
had **none** — the controller applied one only to the launcher-inject initContainer, so every
agent image ran with whatever the container runtime allowed.

**What breaks.** An image whose process runs as **root** will not start. The kubelet refuses it
with:

```
Error: container has runAsNonRoot and image will run as root
```

That failure is deliberate and loud. The alternative — silently leaving the container
unhardened — is the shape of bug this platform has spent several releases removing: a control
that appears configured and protects nothing.

**Two ways forward.**

Rebuild the image to run as a non-root user (preferred). Any non-root uid works; the platform
enforces "not root" without dictating which:

```dockerfile
USER 1000:1000
```

Or declare the exception on the agent, per agent, so the decision is visible in the spec and in
review:

```yaml
spec:
  unconfined: true   # this image genuinely needs root
```

**What is deliberately NOT enforced.** A read-only root filesystem and a pinned uid. Both break a
large fraction of real images — anything writing to `/tmp`, anything with a baked-in uid — and a
default that breaks most images is one operators disable wholesale, leaving them with less
protection than a narrower default they keep.

**Every agent takes one new revision** on upgrade, because the hardening is folded into the
revision digest. It has to be: a pod-spec change that does not move the revision name is dropped
by the reconciler's update guard, so a securityContext that did not roll the revision would never
reach a running pod.

## M162 — the launcher plane is bound to loopback

The five pod-internal launcher listeners (`:2994` delegate, `:2995` feedback, `:2996` budget /
guardrail proxy, `:2997` A2A, `:2998` memory / knowledge) now bind `127.0.0.1` instead of every
interface. They were always documented as localhost listeners; binding `:port` exposed them on
the **pod IP**, where any same-tenant pod could reach them — and the launcher attaches the pod's
own projected ServiceAccount token to whatever arrives, spending the victim's identity for the
caller.

No action is required unless something outside the agent pod was calling those ports, which was
never a supported arrangement. The agent's own serving port is unchanged: the kubelet dials the
pod IP for the readiness and liveness probes.

## M162 — egress: a dedicated sidecar uid, and UDP is denied

The L4 egress redirect exempted uid **65532** so the sidecar's forwarded calls would not loop back
into itself. That is also the standard distroless `nonroot` uid, so an agent image using that
convention was exempt from egress control entirely. The sidecar now runs as **64535**.

UDP was not redirected at all — every rule was `-p tcp`. QUIC (HTTP/3 on UDP 443) therefore
bypassed the redirect completely, and arbitrary UDP was an open channel. UDP is now dropped except
DNS to the cluster resolver. **If an agent relies on HTTP/3 or on non-DNS UDP, it will now fail**;
route it over TCP.

## M164 — the inter-agent call surface is renamed to AMP (BREAKING for saved trace queries)

The surface agents use to call each other was called **A2A**. That name predates Google's
Agent2Agent specification by years and now collides with it, while meaning something
materially different: Google's A2A is *interop* between agents run by different parties;
ctxmesh's is *mediation* between agents the platform already owns — an agent never dials
another agent, it asks its own launcher, which stamps identity, enforces registry isolation
and trips the guards. It is now called **AMP**.

**Almost nothing about this is breaking.** The wire and the API keep working:

| | Status |
|---|---|
| `client.mesh.call(...)` — the SDK API you write | **unchanged** |
| `POST /a2a/{target}` — the launcher's loopback endpoint | still served, alongside `/amp/{target}` |
| `X-A2A-Envelope` — the launcher↔launcher header | still sent **and** accepted, alongside `X-AMP-Envelope` |
| Agent specs, CRDs, `A2A_*` env vars | **unchanged** |

**The one thing that breaks: trace and metric names.** Spans, span events and attributes move
from the `a2a.*` namespace to `amp.*`:

```
a2a.call                    →  amp.call
a2a.guard                   →  amp.guard
a2a.guard_tripped           →  amp.guard_tripped
a2a.cross_registry_denied   →  amp.cross_registry_denied
a2a.conversation.id         →  amp.conversation.id
a2a.caller.registry.id      →  amp.caller.registry.id
a2a.caller_denied           →  amp.caller_denied
a2a.budget_remaining        →  amp.budget_remaining
a2a.async, a2a.async.*      →  amp.async, amp.async.*
```

A span name cannot be emitted under two names at once, so this is a single deliberate cut
rather than a deprecation window. **Any saved dashboard, alert or trace query keyed on `a2a.*`
will silently return nothing** — it will not error, it will just go quiet, which is the worst
way for a rename to reach you. Grep your Grafana/Langfuse/Honeycomb saved queries for `a2a.`
before upgrading.

**What is deliberately NOT renamed**, because renaming it would break running installs with no
user-visible benefit: the `A2A_MAX_DEPTH` / `A2A_HOP_BUDGET` / `A2A_PORT` pod env vars (running
pods keep their env until rolled, so renaming the read side would quietly revert every agent's
guard limits to defaults), the JetStream stream `CTXMESH_A2A` and subject `ctxmesh.a2a`
(in-flight durable hops would be stranded), and the `a2a:seen:` dedup keyspace and its Valkey
ACL. See [ADR 0138](https://github.com/ctxmesh/ctxmesh) for the reasoning.
