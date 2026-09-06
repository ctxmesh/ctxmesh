# Changelog

## v0.1.0-beta.1 — the first beta

The first release you can install without cloning the repository.

Everything before this was verified from a working tree — `kustomize build config/`,
images built locally and loaded into kind. That is the right thing for a dev loop, and it
is why a set of defects survived until a release forced the question: the chart's
first-party images were bare names (`docker.io/library/controller:latest`), `appVersion`
was `latest`, the release workflow published one of five images and no chart at all, and
the chart's post-install preflight was denied by the datastore NetworkPolicy on every
cluster that enforces one. All fixed here (ADR 0133).

### Install

```sh
helm install ctxmesh oci://ghcr.io/ctxmesh/charts/ctxmesh \
  --version 0.1.0-beta.1 --namespace ctxmesh --create-namespace --wait
```

Upgrades: [docs/upgrading.md](docs/upgrading.md).

### What a beta means here

The platform runs agents on Kubernetes: an `AgentDeployment` CRD, a managed runtime that
needs no Dockerfile, a model gateway, per-tenant isolation, RAG over a bundled pgvector
store, evals, guardrails, cost attribution, and a console that goes from an idea to a
running agent without touching YAML.

**Beta means the API is `v1beta1` on purpose** (ADR 0100) — the shape is settled enough
to build on and not yet frozen. Breaking changes will come with a documented migration.

### Highlights since the last milestone arc

**Authoring.** The console's create path is proven end to end by a single test that walks
it: connect a provider, author an agent in form fields, watch it reach Ready, find it
again — no YAML anywhere. That walk found four defects no per-screen test could see,
including a connect wizard offering providers the API rejected, and a permission check
that refused *after* the user's API key had been submitted.

**Providers.** Anthropic, OpenAI, and any OpenAI-compatible endpoint (`custom`, with a
required base URL). Gemini connects through the custom provider with base
`https://generativelanguage.googleapis.com/v1beta/openai`.

**Knowledge.** A second modality (scanned PDFs via OCR), a golden-embedding check that
catches a silently swapped embedding model, and concurrent ingests refused rather than
allowed to destroy each other's chunks.

**Security.** Datastore NetworkPolicies, PSA labels on the install namespace, RBAC
narrowed to namespace-scoped Secret writes, and a least-privilege gate that fails the
build on verb wildcards or cluster-scoped Secret writes.

**Install.** Postgres and NATS are bundled and chart-owned; an install-truth gate fails
the build when the chart consumes something it never creates.

### Breaking change: the inter-agent telemetry namespace

The inter-agent call surface is now called **AMP**, renamed from A2A — which predated Google's
Agent2Agent by years and had come to collide with it while meaning the opposite thing (theirs is
interop between agents run by different parties; ours is mediation between agents one platform
already owns).

Almost nothing about the rename is breaking. `client.mesh.call()` is unchanged, the launcher still
serves `POST /a2a/{target}` alongside `/amp/{target}`, and it still sends **and** accepts
`X-A2A-Envelope` alongside `X-AMP-Envelope`, so an older SDK keeps working against a newer launcher.

**What does break: spans, span events and attributes move from `a2a.*` to `amp.*`** — `amp.call`,
`amp.guard`, `amp.guard_tripped`, `amp.cross_registry_denied`, `amp.conversation.id`, and the
`amp.async.*` family. A span cannot be emitted under two names at once, so this is one deliberate
cut. **A saved dashboard, alert or trace query keyed on `a2a.*` returns nothing rather than
erroring** — grep your saved queries before upgrading. Full mapping in
[docs/upgrading.md](docs/upgrading.md).

### Prerequisite clarified: Knative Eventing is required

The controller watches Knative Eventing `Trigger` resources at startup, so on a cluster without
Eventing it cannot start — **even if you only ever use the serving execution model**. `Chart.yaml`
has always listed it; the install docs did not, and now do. Install Knative Serving *and* Eventing
before the chart.

### Also in this release

- **Home is a work queue.** The console's landing page leads with one ranked list of everything
  blocked on a person — stops, approvals, failing agents, critical alerts — over a fleet bar whose
  every stage opens the list it counts. Counts that used to read "not yet known" above 200 agents
  are now real, from a new census endpoint.
- **Counts that were wrong are fixed.** The alerts feed answered a cluster-wide read with zero
  (both stores filtered on namespace equality while the console sent none), and a capped fetch was
  printed as a total. Any count that could be the size of a page now renders as a bound.
- **Zero reachable vulnerabilities.** Seven standard-library CVEs and one in gRPC, all reachable
  from the call graph, closed before this tag.
- **The durable stores are tested against a real database in CI**, not only against their in-memory
  test doubles.

### Known limitations

- **Connecting a provider needs a permission no shipped role grants.** `secrets: create`
  is deliberately withheld from `ctxmesh-operator` for least privilege, so a cluster
  admin must grant it before anyone can connect a provider in the console. The console
  tells you this up front rather than failing after your key is submitted. A
  provider-admin role is the next release's work.
- **Audio and vision ingestion** are implemented but unproven end to end — they need
  models the test clusters do not carry.
- **The BFF's memory grows across ingestions.** Large corpora may need a raised memory
  limit; the leak is tracked, not papered over with a bigger default.
- **Contributions open with this release** under the DCO (ADR 0134) — `git commit -s`.
