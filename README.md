# ctxmesh

ctxmesh is a Kubernetes operator for deploying and operating LLM agents. It is built with
[Kubebuilder](https://book.kubebuilder.io/) and manages the full lifecycle of agent workloads —
from a short agent description to a running, observable, tool-using agent on a cluster.

There are three ways to work with it:

- **The web console** — connect a model provider, describe an agent, run it, and inspect its
  traces from a browser (the primary surface for most users).
- **The `ctxmesh` CLI** — expand a simplified `agent.yaml` into CRDs and apply it, or run an
  agent locally with `ctxmesh dev`.
- **The `ctxmesh` Python SDK** — author a code-first agent against the in-pod platform plane
  (memory / tools / model / feedback / tracing).

## Install on a cluster (Helm)

ctxmesh ships as a Helm chart in [`deploy/helm/ctxmesh`](deploy/helm/ctxmesh). It installs the
control plane — controller-manager + CRDs, the LiteLLM model gateway, RBAC personas
(`ctxmesh-{operator,developer,viewer}`), the operator UI + Go BFF, and (for dev/trial) a bundled
data plane: PostgreSQL with pgvector, Valkey, MinIO and NATS JetStream.

**Prerequisites.** A Kubernetes cluster (≥ 1.29) with **Knative Serving + Eventing** and **KEDA**
already installed — the controller reconciles their CRDs and agents won't come up without them
(ctxmesh does *not* bundle them) — plus Helm 3.

```sh
# from a clone of this repo — a dev/trial install, everything bundled, single replica
helm install ctxmesh ./deploy/helm/ctxmesh --namespace ctxmesh --create-namespace
```

That is the whole install. There is **no Secret to create by hand and no environment variable to
export** — the chart provisions the database it needs and points the control plane at it. (Until
M148 it did not: `bff-adapters` was consumed by four templates and created by none, and nothing
deployed PostgreSQL at all, so this command produced a CrashLoopBackOff. `harness/scripts/accept-m148.sh`
now asserts that everything the chart consumes, the chart creates.)

Wait for it to come up, then open the console:

```sh
kubectl -n ctxmesh rollout status deploy/ctxmesh-controller-manager
kubectl -n ctxmesh port-forward svc/ctxmesh-bff 9090:9090   # → http://localhost:9090/
```

**What you get, and what you have to add.** The install brings up a working control plane: the
console, the CRDs, the model gateway, the databases and the credential plane. It ships **no models** —
the gateway starts with an empty model list — so the first thing to do is connect a provider in the
console (or create a `SecretBinding` + `ModelRoute`), which is what lets an agent answer anything.

Knowledge bases need one more piece than agents do: an **embedding** `ModelRoute`, named in the
KnowledgeBase's `spec.embeddingRoute`. It can point at a hosted embedding model through the provider
you just connected, or at a self-hosted one — ctxmesh embeds through the gateway either way, so
retrieval works with no external service if you run your own embedder. Once that route exists,
uploading documents and searching returns ranked chunks **with citations** (`documentRef` +
`chunkIndex`), which is what an agent grounds its answers on.

**Use your own images.** The chart defaults to the `:latest` tags built by `make docker-build-*`
(side-loaded into a kind cluster). For a real cluster, push the images to a registry and override
the repository + tag:

```sh
helm install ctxmesh ./deploy/helm/ctxmesh -n ctxmesh --create-namespace \
  --set controllerManager.image.repository=ghcr.io/my-org/ctxmesh-controller \
  --set controllerManager.image.tag=v0.1.0
  # …likewise bff.image.* and gateway.image.* (see values.yaml)
```

### Production

Add `--set profile=production` to make the HA invariants hard — multiple replicas + leader
election, PodDisruptionBudgets, an external data plane, and signed images. See
[`values-production.yaml`](deploy/helm/ctxmesh/values-production.yaml).

A production install **must** supply its own PostgreSQL (with the `pgvector` extension — knowledge
chunks are searched by vector similarity):

```sh
helm install ctxmesh ./deploy/helm/ctxmesh -n ctxmesh --create-namespace \
  --set profile=production \
  -f deploy/helm/ctxmesh/values-production.yaml \
  --set postgres.externalDsn='postgres://user:pass@db.example:5432/ctxmesh?sslmode=require'
```

The bundled data plane uses deterministic, obviously-fake dev credentials and is not rendered under
`profile=production`. The render **fails** if `postgres.externalDsn` is missing rather than
installing a control plane that cannot start — the alternative is a CrashLoopBackOff whose cause is
three layers from its symptom.

Uninstall with `helm uninstall ctxmesh -n ctxmesh`.

## The console

The console (a React app served by the operator's BFF) is the golden path: **connect a provider →
describe an agent → run it → follow the trace**, plus fleet CRUD, tenants, cost, and MCP-tool
management. Run it locally with no cluster:

```sh
ctxmesh dev --ui        # serves the console + a local agent runtime (no cluster, no login)
```

## CLI

The `ctxmesh` CLI converts a simplified agent description into CRD manifests.

```sh
make build-cli                                            # build bin/ctxmesh
bin/ctxmesh expand agent.yaml                        # → AgentDeployment (+ related CRDs) on stdout
bin/ctxmesh expand agent.yaml | kubectl apply -f -   # apply directly
```

**agent.yaml** — a **managed** agent needs no image or code; its behaviour is its configuration
(a system prompt + a model + optional tools/memory). The operator runs it on the stock
managed-agent image:

```yaml
name: research-assistant
runtime: managed                 # stock runtime — no user image/code
systemPrompt: |
  You are a concise research assistant. Cite sources.
model:
  route: default-model           # a connected model (ModelRoute alias)
tools: [web-search]              # bound MCP tools (optional; each → an MCPToolBinding)
executionModel: serving          # serving | eventing | job (default: serving)
scaling: { min: 1, max: 5 }
```

A **custom-image** agent supplies its own container instead of `runtime: managed`:

```yaml
name: my-agent
image: ghcr.io/my-org/my-agent:latest
model: { route: default-model }
```

`prompt`, `tools`, `budget`, `eval`, and `role` are all first-class fields (session memory and
registry membership are attached via their own resources — a MemoryBinding and an AgentRegistry);
only genuinely unknown fields are rejected (with an informative error).

## SDK (`ctxmesh`)

A code-first agent uses the [`ctxmesh` Python SDK](sdk/python) — optional, typed sugar over the
launcher's localhost platform plane (memory, tools, the model gateway, feedback, and step-tracing).
`ctxmesh.serve(handler)` encodes the whole runtime contract (the `/invoke` + health endpoints,
trace propagation, streaming, and the per-request capability scope) so a handler is just business
logic; `ctxmesh.testing` lets you exercise it offline with no cluster. See the
[SDK README](sdk/python/README.md).

## Examples

Runnable agents live in [`examples/`](examples): `echo-agent` (minimal), `sdk-custom-agent` (a
no-framework loop using the SDK's tracing helpers), `langchain-agent`, `batch-agent`, and
`mcp-echo-server` (a tool server). Most run locally via `ctxmesh dev -f examples/<name>/agent.yaml`.

## Development

```sh
make build                 # compile the operator binary
make build-cli             # compile the ctxmesh CLI (bin/ctxmesh)
make test                  # go unit + envtest, the Python SDK tests, and the UI tests
make lint                  # go + python + ui linters
make run                   # run the operator locally against the current kubeconfig context
make docker-build-managed  # build the managed-agent runtime image
make docker-build-launcher # build the launcher image
```
