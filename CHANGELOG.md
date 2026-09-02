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
