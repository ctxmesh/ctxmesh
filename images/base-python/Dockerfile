# images/base-python/Dockerfile — Python base image with OTel + OpenInference
# auto-instrumentation and the m3.6 launcher baked in.
#
# Multi-stage build:
#   Stage 1 (go-builder): Compiles the launcher (cmd/launcher) from the repo's
#     Go module.  Built with CGO_ENABLED=0 so the resulting binary is
#     statically linked and runs in any Linux image.
#   Stage 2 (python-base): python:3.12-slim.  Launcher binary is copied in,
#     pinned OpenInference + OTel packages are installed from requirements.txt,
#     and sitecustomize.py is placed in site-packages so the Python interpreter
#     automatically loads auto-instrumentation before any user code.
#
# Build from repo root:
#   docker build -t base-python:latest -f images/base-python/Dockerfile .
#
# ENTRYPOINT = /launcher (PID 1 — the m3.6 traced reverse proxy).
# Derived images MUST set:
#   ENV AGENT_ENTRYPOINT=<path-to-their-python-interpreter-or-binary>
#   ENV AGENT_ENTRYPOINT_ARGS=<optional-extra-args>
# The launcher spawns $AGENT_ENTRYPOINT as a child process, owns $AGENT_PORT,
# and reverse-proxies to $AGENT_UPSTREAM_PORT (default 8081).

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

# ── Stage 2: Python base image ────────────────────────────────────────────────
FROM python:3.12-slim

# Copy the statically-linked launcher binary from the Go builder stage.
# File permissions are preserved; the +x bit is already set by the build.
COPY --from=go-builder /workspace/launcher /launcher

# Install pinned OpenInference + OTel packages.
# Upgrade pip first so wheel metadata is handled correctly.
COPY images/base-python/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --upgrade pip \
 && pip install --no-cache-dir -r /tmp/requirements.txt \
 && rm /tmp/requirements.txt

# Auto-instrumentation bootstrap: the site module imports sitecustomize.py
# automatically before any user code.  Placement in site-packages ensures
# it is found regardless of PYTHONPATH overrides.
COPY images/base-python/sitecustomize.py \
     /usr/local/lib/python3.12/site-packages/sitecustomize.py

# OTEL_EXPORTER_OTLP_ENDPOINT: gRPC address for the OTel collector sidecar.
# Default matches the controller-injected sidecar (localhost:4317 in the pod).
# Override in derived images or at runtime if the collector is elsewhere.
ENV OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317

# Launcher is PID 1.  Derived images set AGENT_ENTRYPOINT (and optionally
# AGENT_ENTRYPOINT_ARGS) to point to their agent binary.
EXPOSE 8080
ENTRYPOINT ["/launcher"]
