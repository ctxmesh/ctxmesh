# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Python SDK toolchain (sdk/python — the ctxmesh SDK, m10.2). ruff+pytest are
# wired into `make lint`/`make test` below so the harness tier0 covers Python.
# The toolchain is self-bootstrapping and deterministic: `make py-venv` creates
# a venv from the host python3 and installs PINNED ruff+pytest from
# sdk/python/requirements-dev.txt, so lint/test work on a clean host and in CI
# with no host-global installs. Host python3 is 3.9.x; the SDK targets 3.9+.
PYTHON ?= python3
SDK_DIR ?= sdk/python
SDK_VENV ?= $(SDK_DIR)/.venv
SDK_VENV_PY = $(SDK_VENV)/bin/python
SDK_REQS = $(SDK_DIR)/requirements-dev.txt
# Sentinel: touched after a successful install so the venv is only rebuilt when
# the pinned requirements change (make dependency tracking).
SDK_VENV_STAMP = $(SDK_VENV)/.deps-installed

# UI toolchain (ui/ — the Vite React SPA, m12.4). Node is a BUILD-TIME-ONLY
# dependency (ADR 0010: no Node runtime — the BFF serves the static build).
# Node is managed by nvm + a PINNED ui/.nvmrc (user requirement); pnpm + a
# committed pnpm-lock.yaml pin the deps. hack/ui-node.sh bootstraps the whole
# chain deterministically on a CLEAN host (installs nvm if absent, `nvm install`
# the .nvmrc version, corepack-activates pnpm, `pnpm install --frozen-lockfile`),
# then runs the requested pnpm script. eslint + vitest below are wired into
# `make lint`/`make test` so the harness tier0 covers the UI (Go + Python + TS);
# the Vite build feeds the image build. This is the M10 pinned-venv analogue via
# nvm.
UI_DIR ?= ui
UI_NODE ?= ./hack/ui-node.sh

# TypeScript SDK toolchain (sdk/typescript — the ctxmesh TS SDK, m77.1). A shipped
# LIBRARY vendored into the base-node agent image (ADR 0070), NOT part of the ui/
# pnpm workspace — its own lockfile, its own bootstrap. PINNED to the same Node
# story as ui/ (Node 22 via sdk/typescript/.nvmrc, pnpm 9.15.0). hack/sdk-ts.sh
# (the sibling of hack/ui-node.sh) bootstraps the whole chain deterministically on
# a CLEAN host + in CI, then runs the requested pnpm script. eslint + tsc + vitest
# below are wired into `make lint`/`make test` so the harness tier0 gates the TS SDK
# too (Go + Python + UI + the TS SDK).
SDK_TS_DIR ?= sdk/typescript
SDK_TS_NODE ?= ./hack/sdk-ts.sh

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	# internal/kedatypes provides wire-compatible KEDA types without importing the
	# upstream package (controller-runtime API version conflict). Exclude it from
	# CRD/webhook generation — the KEDA CRD comes from keda-crds.yaml, not here.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook \
		paths="{./api/...,./cmd/...,./internal/controller/...,./internal/gateway/...,./internal/telemetry/...,./internal/toolmanifest/...,./internal/toolpush/...}" \
		output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: gen-workflow-schema
gen-workflow-schema: manifests ## Regenerate the WorkflowSpec JSON-Schema (internal/bff/workflow_spec_schema.json) from the Workflow CRD. Run after any WorkflowSpec type change.
	go run ./hack/gen-workflow-schema \
		-crd config/crd/bases/agents.ctxmesh.ai_workflows.yaml \
		-out internal/bff/workflow_spec_schema.json

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet py-test ui-test sdk-ts-build sdk-ts-test ## Run unit tests (Go + Python SDK + UI vitest + TS SDK build+vitest). Envtest suites are tagged 'integration' and run via test-integration.
	go test ./... -coverprofile cover.out

.PHONY: test-integration
test-integration: manifests generate fmt vet setup-envtest ## Run envtest-backed integration tests (build tag 'integration').
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test -tags=integration $$(go list -tags=integration ./...) -coverprofile cover-integration.out

.PHONY: test-conformance
test-conformance: ## Run the credential-store backend conformance suite (ADR 0032). Hermetic by default; set CREDPOSTGRES_TEST_DSN + OPENBAO_TEST_ADDR/TOKEN for the full Postgres+OpenBao profile (incl crypto-shred).
	go test ./test/credconformance/... -v

# e2e/acceptance tests are black-box and live in the agent-brain harness
# (ADR 0004); this repo's pyramid stops at envtest (test-integration).

.PHONY: lint
lint: golangci-lint py-lint ui-lint sdk-ts-lint ## Run linters (Go golangci-lint + Python ruff + UI eslint + tsc + TS SDK eslint + tsc)
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

##@ Python SDK (sdk/python — ctxmesh, m10.2)

# The venv is created from the host python3 and gets EXACTLY the pinned deps.
# It depends on the requirements file so a pin change rebuilds it; the stamp
# lets a warm venv be reused without reinstalling on every make invocation.
$(SDK_VENV_STAMP): $(SDK_REQS) $(SDK_DIR)/pyproject.toml
	@echo "Bootstrapping SDK dev venv ($(SDK_VENV)) from $$($(PYTHON) --version 2>&1)…"
	"$(PYTHON)" -m venv "$(SDK_VENV)"
	"$(SDK_VENV_PY)" -m pip install --upgrade --quiet pip
	"$(SDK_VENV_PY)" -m pip install --quiet -r "$(SDK_REQS)"
	# Install the SDK itself so `import ctxmesh` resolves regardless of cwd.
	"$(SDK_VENV_PY)" -m pip install --quiet -e "$(SDK_DIR)"
	@touch "$(SDK_VENV_STAMP)"

.PHONY: py-venv
py-venv: $(SDK_VENV_STAMP) ## Create/refresh the SDK dev venv with pinned ruff+pytest.

.PHONY: py-lint
py-lint: py-venv ## Lint the Python SDK with ruff (fails the target on any finding).
	"$(SDK_VENV)/bin/ruff" check "$(SDK_DIR)"

.PHONY: py-lint-fix
py-lint-fix: py-venv ## ruff --fix the Python SDK.
	"$(SDK_VENV)/bin/ruff" check --fix "$(SDK_DIR)"

.PHONY: py-test
py-test: py-venv ## Run the Python SDK unit + contract tests with pytest.
	cd "$(SDK_DIR)" && "$(abspath $(SDK_VENV))/bin/pytest"

.PHONY: py-clean
py-clean: ## Remove the SDK dev venv and Python caches.
	rm -rf "$(SDK_VENV)"
	find "$(SDK_DIR)" -type d -name __pycache__ -prune -exec rm -rf {} + 2>/dev/null || true
	rm -rf "$(SDK_DIR)/.pytest_cache" "$(SDK_DIR)/.ruff_cache"

##@ UI (ui/ — Vite React SPA; nvm + pnpm, build-time only, m12.4)

.PHONY: ui-deps
ui-deps: ## Bootstrap the UI toolchain on a clean host (nvm+node from .nvmrc, pnpm, frozen install).
	$(UI_NODE) install

.PHONY: ui-lint
ui-lint: ## Lint the UI (eslint) + typecheck (tsc). Bootstraps the toolchain first. Wired into `make lint`.
	$(UI_NODE) install
	$(UI_NODE) run lint
	$(UI_NODE) run typecheck

.PHONY: ui-test
ui-test: ## Run the UI unit/component tests (vitest run). Bootstraps the toolchain first. Wired into `make test`.
	$(UI_NODE) install
	$(UI_NODE) run test

.PHONY: ui-build
ui-build: ## Build the SPA to static assets (ui/dist). The BFF/image serves this output.
	$(UI_NODE) install
	$(UI_NODE) run build
	./hack/ui-no-internal-routes.sh ui/dist

.PHONY: ui-visual
ui-visual: ## Render every route x 4 widths x 2 themes; assert nothing overflows; write screenshots (M151).
	$(UI_NODE) install
	$(UI_NODE) run build
	$(UI_NODE) run visual

.PHONY: ui-visual-baseline
ui-visual-baseline: ## Same sweep, but RECORD failures instead of failing — the honest before-picture.
	$(UI_NODE) install
	$(UI_NODE) run build
	$(UI_NODE) run visual:baseline

.PHONY: ui-colour-check
ui-colour-check: ## Fail if any colour literal appears outside ui/src/styles/tokens.css (M151).
	./hack/ui-colour-literals.sh

.PHONY: ui-versions
ui-versions: ## Print the pinned node+pnpm the UI toolchain resolves (from .nvmrc).
	$(UI_NODE) print-versions

.PHONY: ui-clean
ui-clean: ## Remove UI build output and installed node_modules.
	rm -rf "$(UI_DIR)/dist" "$(UI_DIR)/node_modules"

##@ TypeScript SDK (sdk/typescript — ctxmesh TS SDK; nvm + pnpm, m77.1)

.PHONY: sdk-ts-deps
sdk-ts-deps: ## Bootstrap the TS SDK toolchain on a clean host (nvm+node from .nvmrc, pnpm, frozen install).
	$(SDK_TS_NODE) install

.PHONY: sdk-ts-lint
sdk-ts-lint: ## Lint the TS SDK (eslint) + typecheck (tsc --noEmit). Bootstraps the toolchain first. Wired into `make lint` (tier0).
	$(SDK_TS_NODE) install
	$(SDK_TS_NODE) run lint
	$(SDK_TS_NODE) run typecheck

.PHONY: sdk-ts-build
sdk-ts-build: ## Build the TS SDK library to dist/ (tsc -b → ESM + CJS + declarations). Bootstraps the toolchain first. Wired into `make test` (tier0).
	$(SDK_TS_NODE) install
	$(SDK_TS_NODE) run build

.PHONY: sdk-ts-test
sdk-ts-test: ## Run the TS SDK unit tests (vitest run). Bootstraps the toolchain first. Wired into `make test` (tier0).
	$(SDK_TS_NODE) install
	$(SDK_TS_NODE) run test

.PHONY: sdk-ts-versions
sdk-ts-versions: ## Print the pinned node+pnpm the TS SDK toolchain resolves (from .nvmrc).
	$(SDK_TS_NODE) print-versions

.PHONY: sdk-ts-clean
sdk-ts-clean: ## Remove TS SDK build output and installed node_modules.
	rm -rf "$(SDK_TS_DIR)/dist" "$(SDK_TS_DIR)/node_modules"

.PHONY: build-bff
build-bff: fmt vet ## Build the BFF binary (bin/bff) — serves the static UI + /api behind M11 auth.
	go build -o bin/bff ./cmd/bff

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: build-cli
build-cli: fmt vet ## Build ctxmesh CLI binary (bin/ctxmesh).
	go build -o bin/ctxmesh ./cmd/ctxmesh

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-build-launcher
docker-build-launcher: ## Build the launcher image (launcher:latest) from Dockerfile.launcher.
	$(CONTAINER_TOOL) build -t launcher:latest -f Dockerfile.launcher .

.PHONY: docker-build-egress-sidecar
docker-build-egress-sidecar: ## Build the egress-sidecar image (egress-sidecar:latest) from Dockerfile.egress-sidecar (ADR 0030 §1).
	$(CONTAINER_TOOL) build -t egress-sidecar:latest -f Dockerfile.egress-sidecar .

.PHONY: docker-build-egress-init
docker-build-egress-init: ## Build the egress-redirect initContainer image (egress-init:latest) from Dockerfile.egress-init (M142.4, ADR 0123).
	$(CONTAINER_TOOL) build -t egress-init:latest -f Dockerfile.egress-init .

# REPLAY_TAG is the version the replay-serve image is tagged AND stamped with. It must match the
# CLI's devVersion (cmd/ctxmesh) so `dev --replay`'s /replay/version parity check passes; the
# default "m78-smoke" agrees with the CLI's built-in default out of the box (ADR 0071 §3a).
REPLAY_TAG ?= m78-smoke
.PHONY: docker-build-replay
docker-build-replay: ## Build the replay-serve image (ctxmesh-replay:$(REPLAY_TAG)) from Dockerfile.replay (ADR 0071 §3a).
	$(CONTAINER_TOOL) build --build-arg REPLAY_VERSION=$(REPLAY_TAG) -t ctxmesh-replay:$(REPLAY_TAG) -f Dockerfile.replay .

.PHONY: docker-build-token-service
docker-build-token-service: ## Build the token-service image (token-service:latest) from Dockerfile.token-service (ADR 0030 §1 central service).
	$(CONTAINER_TOOL) build -t token-service:latest -f Dockerfile.token-service .

.PHONY: docker-build-bff
docker-build-bff: ## Build the BFF image (bff:latest) — builds the Vite SPA (build-time Node) + Go BFF; serves static assets, NO Node runtime.
	$(CONTAINER_TOOL) build -t bff:latest -f Dockerfile.bff .

.PHONY: docker-build-statelayer-proxy
docker-build-statelayer-proxy: ## Build the state-layer proxy image (statelayer-proxy:latest) — server-side tenant isolation (M51, ADR 0050).
	$(CONTAINER_TOOL) build -t statelayer-proxy:latest -f Dockerfile.statelayer-proxy .

.PHONY: docker-build-discovery
docker-build-discovery: ## Build the tool-discovery sidecar image (dev.local/agent-discovery:0.1.0) from Dockerfile.discovery.
	$(CONTAINER_TOOL) build -t dev.local/agent-discovery:0.1.0 -f Dockerfile.discovery .

.PHONY: docker-build-example
docker-build-example: ## Build the echo-agent example image (echo-agent:latest) from examples/echo-agent/Dockerfile.
	$(CONTAINER_TOOL) build -t echo-agent:latest -f examples/echo-agent/Dockerfile .

.PHONY: docker-build-base-python
docker-build-base-python: ## Build the Python base image (base-python:latest) — launcher + OpenInference/OTel auto-instrumentation.
	$(CONTAINER_TOOL) build -t base-python:latest -f images/base-python/Dockerfile .

.PHONY: docker-build-base-node
docker-build-base-node: ## Build the Node 22 base image (base-node:latest) — launcher (PID 1) + the vendored ctxmesh TS SDK (ADR 0070 §3).
	$(CONTAINER_TOOL) build -t base-node:latest -f images/base-node/Dockerfile .

.PHONY: docker-build-langchain-example
docker-build-langchain-example: docker-build-base-python ## Build the LangChain example agent image (langchain-agent:latest); depends on base-python:latest.
	$(CONTAINER_TOOL) build -t langchain-agent:latest -f examples/langchain-agent/Dockerfile .

.PHONY: docker-build-batch-example
docker-build-batch-example: docker-build-base-python ## Build the batch-agent example image (batch-agent:latest — job/CronJob model); depends on base-python:latest.
	$(CONTAINER_TOOL) build -t batch-agent:latest -f examples/batch-agent/Dockerfile .

.PHONY: docker-build-sdk-custom-agent
docker-build-sdk-custom-agent: docker-build-base-python ## Build the M10 no-framework SDK example (sdk-custom-agent:latest); depends on base-python:latest (which bundles ctxmesh).
	$(CONTAINER_TOOL) build -t sdk-custom-agent:latest -f examples/sdk-custom-agent/Dockerfile .

.PHONY: docker-build-managed
docker-build-managed: docker-build-base-python ## Build the M14 managed-agent runtime image (managed-agent:latest — config-driven, no user Docker build; ADR 0013); depends on base-python:latest (which bundles the launcher + ctxmesh).
	$(CONTAINER_TOOL) build -t managed-agent:latest -f images/managed-agent/Dockerfile .

.PHONY: docker-build-collector
docker-build-collector: ## Build the project OTel Collector image (dev.local/agent-otel-collector:0.116.0) — contrib collector (M11 redaction transform) on a glibc base.
	$(CONTAINER_TOOL) build -t dev.local/agent-otel-collector:0.116.0 -f images/otel-collector/Dockerfile .

.PHONY: docker-build-mcp-echo-server
docker-build-mcp-echo-server: ## Build the M4 fixture MCP echo server (dev.local/mcp-echo-server:e2e — matches the ToolRegistry pin).
	$(CONTAINER_TOOL) build -t dev.local/mcp-echo-server:e2e examples/mcp-echo-server

.PHONY: docker-build-real-embedder
docker-build-real-embedder: ## Build the offline semantic embedder (dev.local/real-embedder:m117 — fastembed/MiniLM, model baked in; M140). Bakes the model at build time so the pod runs offline.
	$(CONTAINER_TOOL) build -t dev.local/real-embedder:m117 examples/real-embedder

.PHONY: docker-build-real-reranker
docker-build-real-reranker: ## Build the offline cross-encoder reranker (dev.local/real-reranker:m140 — fastembed TextCrossEncoder/ms-marco-MiniLM, model baked in; M140.2, ADR 0117).
	$(CONTAINER_TOOL) build -t dev.local/real-reranker:m140 examples/real-reranker

.PHONY: docker-build-ocr-service
docker-build-ocr-service: ## Build the offline OCR service (dev.local/ocr-service:m140 — tesseract + poppler baked in; scanned-PDF text, M140.5).
	$(CONTAINER_TOOL) build -t dev.local/ocr-service:m140 examples/ocr-service

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name ctxmesh-builder
	$(CONTAINER_TOOL) buildx use ctxmesh-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm ctxmesh-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -
	# M134 (ADR 0102): the tenant-label VWC is created at RUNTIME (not in config/), so `kustomize delete`
	# won't remove it — delete it explicitly so a dev cluster doesn't accumulate an orphaned fail-closed webhook.
	-"$(KUBECTL)" delete validatingwebhookconfiguration tenant-label-validator --ignore-not-found

##@ Helm chart (deploy/helm/ctxmesh — GENERATED from config/, m12.2)

# The Helm chart's control-plane + CRD templates are GENERATED from
# `kustomize build config/default` — NEVER hand-maintained — so `helm install`
# cannot drift from `make deploy`. `helm-generate` regenerates them;
# `helm-verify` fails if the committed chart drifts from config/ OR if
# `helm template` (default values) stops matching `kustomize build config/default`.
HELM ?= helm
HELM_CHART ?= deploy/helm/ctxmesh

.PHONY: helm-generate
helm-generate: manifests kustomize ## Regenerate the Helm chart templates from config/default. Run after any config/ change.
	KUSTOMIZE="$(KUSTOMIZE)" ./hack/gen-helm-chart.sh

.PHONY: crd-version-parity
crd-version-parity: manifests ## Guard: multi-version CRDs keep matching top-level CEL validations (conversion is None; audit FUNC-8).
	./hack/check-crd-version-parity.sh config/crd/bases

.PHONY: rbac-least-privilege
rbac-least-privilege: manifests ## Assert the SHIPPED roles grant no verb wildcards and no cluster-scoped Secret writes (M149).
	./hack/rbac-least-privilege.sh config/rbac

.PHONY: release-truth
release-truth: ## Assert the release publishes every artifact an install needs, at a version (M154). Static, no cluster.
	./hack/release-truth.sh

.PHONY: provider-parity
provider-parity: ## Assert the console offers exactly the providers the BFF supports (M153). Static, no cluster.
	./hack/provider-parity.sh

.PHONY: install-truth
install-truth: ## Assert the chart provisions what it consumes (M148). Render-only, no cluster.
	./hack/install-truth.sh

.PHONY: helm-verify
helm-verify: manifests kustomize ## Prove the Helm chart does not drift from `kustomize build config/default` (no-drift gate).
	@set -e; \
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	echo ">> regenerating chart templates into a scratch dir and diffing vs committed"; \
	mkdir -p "$$tmp/gen"; \
	KUSTOMIZE="$(KUSTOMIZE)" ./hack/gen-helm-chart.sh "$$tmp/gen" >/dev/null; \
	for f in crds.yaml control-plane.yaml dev-data-plane.yaml bff.yaml; do \
	  diff -u "$(HELM_CHART)/templates/$$f" "$$tmp/gen/$$f" \
	    || { echo "DRIFT: $(HELM_CHART)/templates/$$f is stale — run 'make helm-generate'"; exit 1; }; \
	done; \
	echo ">> checking helm template (+ hack/kustomize-parity.values.yaml) == kustomize build config/default"; \
	"$(KUSTOMIZE)" build config/default > "$$tmp/kustomize.yaml"; \
	"$(HELM)" template ctxmesh "$(HELM_CHART)" -f hack/kustomize-parity.values.yaml > "$$tmp/helm.yaml"; \
	python3 ./hack/helm_nodrift_diff.py "$$tmp/kustomize.yaml" "$$tmp/helm.yaml"

.PHONY: helm-lint
helm-lint: ## helm lint the chart.
	"$(HELM)" lint "$(HELM_CHART)"

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.21.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.12.2
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
