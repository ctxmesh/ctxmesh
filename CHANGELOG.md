# Changelog

## v0.1.0-beta.6 — the release reports itself

The first release verified by a gate instead of by hand.

**One command now answers "what actually got published?"** `hack/release-truth-sdks.sh` asks all six
registries plus the chart and fails on anything absent. [ADR 0139](https://github.com/ctxmesh/ctxmesh/blob/main/decisions)
asked for this after beta.2 shipped 8 of 9 artifacts and it was never built; establishing the truth
took six manual queries as recently as beta.5. Two things made it harder than a loop over six URLs:
every ecosystem spells the version differently (`0.1.0b6` on PyPI, `0.1.0.pre.beta.6` on RubyGems),
and a registry can answer 200 and still be wrong, so each probe asserts the **version**, not the
package.

**CI stops failing on someone else's outage.** `proxy.golang.org` drops HTTP/2 streams under load and
took down two different required jobs 24 minutes apart during beta.5, neither related to the change
under test. All 11 Dockerfiles now retry `go mod download`, and the `go install` paths get a
workflow-level `GOPROXY`. The cost was never the re-runs — a required check that goes red for
unrelated reasons teaches you to dismiss red.

**Maven no longer blocks on Central's queue.** The publish job waited for full publication, which took
14 minutes for beta.4 and was still waiting at 25 for beta.5, where the job timeout killed it *after*
the bundle had uploaded successfully. It now waits only for validation — which still fails on a bad
signature, a missing javadoc or a malformed POM — and Central publishes on its own schedule.

**npm silently truncates a description at 255 characters.** The TypeScript description was 342, so it
published cut mid-word, losing the repository URL at the end. Every local check passed because the
file was correct; only the registry knew.

**The four plane-client SDKs have documentation.** `/sdk/go/`, `/sdk/rust/`, `/sdk/ruby/` and
`/sdk/java/` were all 404 — four SDKs on public registries with no guide. Each page is written
against that SDK's real exported surface and states plainly what the plane-client tier does not
include.

**The public site can no longer drift unnoticed.** A new harness gate asserts every version named in
an install command is installable — it runs at *release*, not on the docs deploy path, so a registry
hiccup cannot block a docs deploy. It immediately found the install pages pinning a chart a release
behind.

Also: `npm install ctxmesh` still resolves to an older build than other ecosystems, and that is now a
recorded decision rather than an oversight — npm's OIDC trusted publishing cannot move a dist-tag,
and the token it would need cannot be scoped to that one operation. See ADR 0140. It self-heals at
the first stable release.

Upgrading from beta.5 needs no action — release tooling and documentation only, no runtime change.

## v0.1.0-beta.5 — the description says what ctxmesh is, and where it lives

beta.4 fixed the SDK READMEs. That was the wrong surface for two of the six registries, and this
release finishes the job.

**rubygems.org and Maven Central render no README at all.** Their page body is the gem
`description` / POM `<description>`, so the footer added in beta.4 was invisible there — measured on
the published beta.4 gem, `rubygems.org/gems/ctxmesh` showed none of it. A reader saw "typed clients
for agents running on ctxmesh", with no way to learn what ctxmesh *is* or where the project lives.
The repository link existed only in a sidebar list.

**None of the six descriptions mentioned the repository, and none said what ctxmesh is.** Every one
now opens with "SDK for ctxmesh, the Kubernetes-native control plane for AI agents" and ends with
`Source, docs and issues: https://github.com/ctxmesh/ctxmesh`. Go's package doc — what pkg.go.dev
renders — says the same.

**The gate was blind in the same place.** `hack/sdk-readme-truth.sh` checked READMEs, which is why
beta.4 passed it while two registry pages still told a visitor nothing. It now also asserts every
package's *description* references the repository, and was proven by removing the reference and
watching it fail.

Upgrading from beta.4 needs no action — metadata only, no code changed.

## v0.1.0-beta.4 — the registry pages tell the truth

beta.3 published six SDKs. This release fixes what those six pages actually said, because a
package nobody can orient themselves around is not really published.

**Four SDKs never linked the project.** Go, Rust, Ruby and Java were built from a shared template
that dropped the footer Python and TypeScript carry, so someone landing on crates.io or Maven
Central found a package and no route back — no repository, no docs, no issue tracker. All six now
end the same way: SDK overview, compatibility, source and issues, and a link into the SDK's own
directory.

**Java's install snippet did not work.** It pinned `0.1.0-beta.1`, which 404s on Maven Central, so
the copy-paste install failed outright. Rust pinned the same version; Cargo resolves it through
caret semantics, so it *worked* while telling people the wrong version — the quieter failure, and
the one that survives longer. Both now pin the shipped version.

**The cause was structural, and is closed.** [ADR 0135](https://github.com/ctxmesh/ctxmesh/blob/main/decisions)
stamps Python and TypeScript at release; the other four ship verbatim, whatever is in the tree, and
nothing wrote them. `hack/bump-version.sh` is now the single writer for those four manifests *and*
the README pins, with `--check` in CI. `hack/sdk-readme-truth.sh` additionally asserts every README
links the repository, pins a version that **actually resolves on that SDK's own registry**, and
offers no documentation link that 404s — a live check, because Java's pin was perfectly well-formed
and still a 404.

**The docs site named two SDKs while six shipped.** Go, Rust, Ruby and Java appeared nowhere on
ctxmesh.github.io. The SDK index now carries all six with their registries and explains the two
tiers, and the compatibility table has six rows. Both Helm install pages had also drifted two
releases behind and now pin the current chart.

**Pre-release installs are documented because they are not uniform.** `pip install ctxmesh` takes a
pre-release when no stable exists; `gem install ctxmesh` refuses and needs `--pre`, though a
Gemfile's `gem "ctxmesh"` resolves it without help; npm uses the `beta` dist-tag; Maven and Cargo
take an exact version. RubyGems publishes `0.1.0-beta.4` as `0.1.0.pre.beta.4`.

Upgrading from beta.3 needs no action — no runtime behaviour changed.

## v0.1.0-beta.3 — the install stops needing a flag

beta.2 made a cold install *possible*. This makes it work without the user knowing to ask.

**The control plane waits for its store instead of exiting.** `controller`, `bff` and
`token-service` fail-fast when `CONTROLPLANE_DSN` is unreachable and rely on Kubernetes to
restart them. That stance is preserved — a store that dies while a process is serving still
takes it down — but it was never about start-up *order*. CrashLoopBackOff is exponential to
a 300s cap, so on a cold cluster where PostgreSQL is still pulling its image, its dependents
burn restarts and are asleep in a five-minute backoff by the time the database is healthy.

Only the start-up connection retries, backoff is capped at 15s (an *uncapped* exponential is
the shape that caused this), and `CONTROLPLANE_STARTUP_TIMEOUT=0` restores the previous
behaviour exactly.

**`--timeout` is still required on a cold cluster, and this does not change that.** The
retry removes the *backoff* — the part that made a first install a coin flip decided by
which image finished pulling first. It does not remove the *pulling*: the PostgreSQL image
alone took 4m31s in a measured run and a full cold install took 8m05s, both past Helm's
five-minute default. What changes is that the install now fails or succeeds for an honest
reason instead of a race. Keep `--timeout 20m` on a first install; a chart cannot set that
default on your behalf, because Helm has no chart-side equivalent of the client flag.

**A release gate that runs.** `make release-verify VERSION=v<x.y.z>` installs a *published*
release onto a cold cluster from `oci://ghcr.io`, pulling the chart logged-out. The guard
that caught beta.1 being uninstallable existed but nothing ran it, so it would not have
caught the next one.

**CI stopped lying.** Three test packages dropped a shared `knowledge_chunks` parent in the
one database twenty-four test files share, and `go test` runs packages in parallel — so one
package's setup deleted another's partitions mid-test. It presented as flakiness: the same
commit passed `unit` on a PR and failed it on `main`, which is what makes a green check stop
meaning anything. Each package now gets its own Postgres schema.

Upgrading from beta.2 needs no action.

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
