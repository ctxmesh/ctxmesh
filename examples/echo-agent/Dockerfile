# examples/echo-agent/Dockerfile — reference agent image.
#
# Multi-stage build (single Dockerfile, built from repo root context):
#   Stage 1 (builder): compiles both binaries — launcher and echo-agent — from
#     the same Go module, keeping a single go mod download layer.
#   Stage 2 (final): distroless/static — minimal, no shell, no package manager.
#     Launcher is ENTRYPOINT (PID 1); AGENT_ENTRYPOINT points to echo-agent.
#     The launcher exec's echo-agent, which inherits PID 1 and all signals.
#
# Build from repo root:
#   docker build -t echo-agent:latest -f examples/echo-agent/Dockerfile .

FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o launcher ./cmd/launcher && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o echo-agent ./examples/echo-agent

# Final minimal image — distroless/static has no shell and no libc,
# which is appropriate because both binaries are statically linked.
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/launcher /launcher
COPY --from=builder /workspace/echo-agent /echo-agent

# Launcher reads $AGENT_ENTRYPOINT and exec's it.
# Health/readiness probes target the user process directly (per M1 contract).
ENV AGENT_ENTRYPOINT=/echo-agent
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/launcher"]
