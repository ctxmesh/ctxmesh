# agent-engine

agent-engine is a Kubernetes operator for deploying and operating LLM agents. It is built with [Kubebuilder](https://book.kubebuilder.io/) and manages the full lifecycle of agent workloads on Kubernetes clusters.

## CLI

The `agent-engine` CLI converts simplified agent descriptions into CRD manifests.

```sh
make build-cli                         # build bin/agent-engine
bin/agent-engine expand agent.yaml     # expand agent.yaml → AgentDeployment YAML on stdout
bin/agent-engine expand agent.yaml | kubectl apply -f -   # apply directly
```

**agent.yaml** (M2 supported fields):

```yaml
name: my-agent                         # required
image: ghcr.io/my-org/my-agent:latest  # required
executionModel: serving                # optional; default: serving
resources:
  cpu: "500m"
  memory: "256Mi"
scaling:
  min: 1
  max: 5
model:
  route: default-model                 # ModelRoute alias → MODEL_ROUTE env in pod
```

Unknown fields and fields arriving in later milestones (`prompt`, `tools`, `memory`, `budget`, `registry`) are rejected with an informative error.

## Development

```sh
make build                 # compile the operator binary
make build-cli             # compile the agent-engine CLI (bin/agent-engine)
make test                  # run unit and integration tests
make run                   # run the operator locally against the current kubeconfig context
make docker-build-launcher # build the launcher image (launcher:latest)
make docker-build-example  # build the echo-agent example image (echo-agent:latest)
```
