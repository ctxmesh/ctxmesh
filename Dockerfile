# Build the manager binary
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
# Retry: proxy.golang.org drops HTTP/2 streams under load, and it failed two different required
# CI jobs 24 minutes apart on 2026-09-11 (sum.golang.org in lint, proxy.golang.org here), neither
# related to the change under test. `exit 0` on success and an explicit `exit 1` after exhaustion —
# a retry loop that falls through to success is the version of this fix that hides the failure.
RUN set -e; \
    for i in 1 2 3 4 5; do \
      if go mod download; then exit 0; fi; \
      echo "go mod download failed (attempt $i/5) — retrying" >&2; \
      sleep $((i * 5)); \
    done; \
    echo "go mod download failed after 5 attempts" >&2; \
    exit 1

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
