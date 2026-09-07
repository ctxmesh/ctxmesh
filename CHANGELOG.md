# Changelog

## v0.1.0-beta.2 — the first install actually works

v0.1.0-beta.1 published correctly and could not be installed. This release fixes that, and
it is the first version whose install was tested from the published artifacts *before*
shipping rather than after.

**The command on the release page could not succeed.** It was `--wait` with no `--timeout`,
which is Helm's 5-minute default. A first install pulls nine images into an empty cache and
the PostgreSQL image alone took 4m31s in a measured cold run. That is not bad luck, it is
arithmetic — the command budgeted five minutes for work that needs fifteen, then reported a
failed install while the cluster was still pulling. The release notes and the install docs
now pass `--timeout 20m` and explain that it is a ceiling, not an expected wait.

**Two Helm hooks were timing the image pull, not their own work.** `activeDeadlineSeconds` is
counted by Kubernetes from Job *creation*, so the capability-keygen hook's 120s covered the
pull of the image it had not yet started. On a cold node it lost that race and
`backoffLimit: 0` made the loss terminal. `backoffLimit: 0` is kept — its reasoning, that a
keygen which *ran* and failed must not retry into an "already provisioned" no-op, is sound and
was simply never about a container that failed to start. Both hooks now allow 900s and are
configurable for a slow registry.

**The control plane slept through its own dependency recovering.** `controller`, `bff` and
`token-service` exit when the control-plane store is unreachable and rely on Kubernetes to
restart them, but CrashLoopBackOff is exponential to a 300s cap. While PostgreSQL pulled, its
dependents accumulated backoff and were still asleep after it was healthy — observed directly
as `postgres 1/1 Running` beside `controller CrashLoopBackOff` — tripping the 600s
`progressDeadlineSeconds` default with nothing actually broken. That bound is now 1800s, which
hands the limit back to the user's `--timeout`.

**Why this was invisible.** Every tier installs from a working tree or a local registry that
serves images in about a second, so every deadline was met with room to spare. A new harness
slice installs from `oci://ghcr.io/ctxmesh/charts/ctxmesh` onto a cold cluster, pulling the
chart *logged out* — an authenticated pull succeeds against a private package, which is how
beta.1 was called verified while five of six packages were unreachable. It asserts what a
pods-are-Running check waves through: Helm status `deployed` at revision 1, and the capability
keypair Secret existing.

Upgrading from beta.1 needs no action beyond the new `--timeout`. If a beta.1 install is
sitting at `STATUS: failed`, re-running `helm upgrade` provisions the keypair it never created.

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
