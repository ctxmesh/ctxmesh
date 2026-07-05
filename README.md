# agent-engine

agent-engine is a Kubernetes operator for deploying and operating LLM agents. It is built with [Kubebuilder](https://book.kubebuilder.io/) and manages the full lifecycle of agent workloads on Kubernetes clusters.

## Development

```sh
make build   # compile the operator binary
make test    # run unit and integration tests
make run     # run the operator locally against the current kubeconfig context
```
