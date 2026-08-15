# images/base-node/Dockerfile — Node 22 base image with the m3.6 launcher baked in
# and the ctxmesh TypeScript SDK vendored so any Node agent can `import "ctxmesh"`
# (or `require`) with zero setup (ADR 0070 §3).
#
# Multi-stage build:
#   Stage 1 (go-builder): Compiles the launcher (cmd/launcher) from the repo's Go
#     module.  Built with CGO_ENABLED=0 so the binary is statically linked and runs
#     in any Linux image (identical to images/base-python/Dockerfile).
#   Stage 2 (sdk-builder): node:22-slim + pnpm 9.15.0 (via corepack); builds the
#     ctxmesh TS SDK to dist/ (ESM+CJS) from sdk/typescript/, then `npm pack`s it to
#     a tarball — so the runtime stage installs a clean, prod-only package with no
#     pnpm store / devDeps / build caches leaking into the image.
#   Stage 3 (runtime): node:22-slim.  Launcher binary is copied in; the packed
#     ctxmesh tarball is `npm install -g`'d so `require('ctxmesh')` / `import "ctxmesh"`
#     resolve from the global node_modules for every derived agent.
#
# Build from repo root:
#   docker build -t base-node:latest -f images/base-node/Dockerfile .
#
# ENTRYPOINT = /launcher (PID 1 — the m3.6 traced reverse proxy).
# Derived images MUST set:
#   ENV AGENT_ENTRYPOINT=/usr/local/bin/node
#   ENV AGENT_ENTRYPOINT_ARGS=<path-to-their-agent-entry.js>
# The launcher spawns $AGENT_ENTRYPOINT as a child process, owns $AGENT_PORT, and
# reverse-proxies to $AGENT_UPSTREAM_PORT (default 8081).

# ── Stage 1: Build the launcher ───────────────────────────────────────────────
FROM golang:1.26 AS go-builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Download deps in a separate layer so they are cached across code changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -a -o launcher ./cmd/launcher

# ── Stage 2: Build + pack the ctxmesh TS SDK ──────────────────────────────────
# A dedicated stage so the pnpm store, devDependencies, and tsc build caches never
# reach the runtime image — only the packed prod tarball crosses the stage boundary.
FROM node:22-slim AS sdk-builder

# Pin pnpm 9.15.0 through corepack (matches sdk/typescript/package.json
# "packageManager" + hack/sdk-ts.sh), so the in-image build is deterministic.
RUN corepack enable && corepack prepare pnpm@9.15.0 --activate

WORKDIR /tmp/ctxmesh-sdk
COPY sdk/typescript/ ./

# Install the FULL dep tree (devDeps included — tsc/typescript are devDeps) against the
# committed lockfile, build dist/ (ESM+CJS via `tsc -b`), then pack a prod tarball.
# `pnpm pack` honours package.json "files" (dist, src, README) so the tarball carries
# the built output + the runtime "dependencies" only (npm resolves those on install).
RUN pnpm install --frozen-lockfile --prod=false \
 && pnpm build \
 && pnpm pack --pack-destination /tmp

# ── Stage 3: Node runtime base image ──────────────────────────────────────────
FROM node:22-slim

# Copy the statically-linked launcher binary from the Go builder stage.
# File permissions are preserved; the +x bit is already set by the build.
COPY --from=go-builder /workspace/launcher /launcher

# Install the packed ctxmesh SDK globally so `require('ctxmesh')` / `import "ctxmesh"`
# resolve from the global node_modules for any derived agent, with ZERO setup. npm
# installs only the package's runtime "dependencies" (the OTel/OpenInference stack);
# devDeps (tsc, eslint, vitest) are excluded from the packed tarball. --omit=dev keeps
# it prod-only; the global prefix is on Node's default require/import search path.
COPY --from=sdk-builder /tmp/ctxmesh-*.tgz /tmp/ctxmesh.tgz
RUN npm install -g --omit=dev /tmp/ctxmesh.tgz \
 && rm -f /tmp/ctxmesh.tgz \
 && npm cache clean --force \
 # Make `ctxmesh` resolvable for BOTH module systems, zero-setup:
 #   - `require('ctxmesh')` (CJS) resolves via NODE_PATH → the global node_modules;
 #   - `import "ctxmesh"` (ESM) IGNORES NODE_PATH (it is CJS-only) and instead walks the
 #     importing file's directory chain up to the filesystem root. Symlinking the global
 #     install into the root-level /node_modules means an agent at ANY path (/app/agent.mjs,
 #     …) resolves the bare `ctxmesh` specifier via that upward walk. Its own deps stay
 #     nested under the global package's node_modules (npm self-contains them), so the
 #     single symlink is sufficient for the whole dep tree.
 && mkdir -p /node_modules \
 && ln -s /usr/local/lib/node_modules/ctxmesh /node_modules/ctxmesh

# NODE_PATH points at the global node_modules so a derived agent using CommonJS
# `require('ctxmesh')` resolves the global install with no local node_modules.
ENV NODE_PATH=/usr/local/lib/node_modules

# OTEL_EXPORTER_OTLP_ENDPOINT: gRPC address for the OTel collector sidecar.
# Default matches the controller-injected sidecar (localhost:4317 in the pod).
# Override in derived images or at runtime if the collector is elsewhere.
ENV OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317

# Launcher is PID 1.  Derived images set AGENT_ENTRYPOINT (node) + AGENT_ENTRYPOINT_ARGS
# (their agent entry .js) to point at their agent.
EXPOSE 8080
ENTRYPOINT ["/launcher"]
