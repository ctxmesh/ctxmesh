# sdk-custom-agent — no-framework loop using the ctxmesh SDK (M10 example).
# Built on the base-python image which already bundles the ctxmesh SDK (m10.4),
# so `import ctxmesh` is available with zero extra installs here.
#
# Build from repo root (base image first):
#   docker build -t base-python:latest -f images/base-python/Dockerfile .
#   docker build -t sdk-custom-agent:latest -f examples/sdk-custom-agent/Dockerfile .
#
# Or via the Makefile target (once added):
#   make docker-build-sdk-custom-agent
FROM base-python:latest

WORKDIR /app

# No extra requirements: ctxmesh and its deps (opentelemetry-sdk, opentelemetry-
# exporter-otlp-proto-grpc, openinference-semantic-conventions) are already
# installed in base-python.  The agent itself is pure stdlib + ctxmesh.

COPY examples/sdk-custom-agent/agent.py /app/agent.py

# The launcher spawns this as the child process (listening on the upstream port
# the launcher assigns via AGENT_PORT) and reverse-proxies $AGENT_PORT to it.
ENV AGENT_ENTRYPOINT=/usr/local/bin/python \
    AGENT_ENTRYPOINT_ARGS=/app/agent.py

EXPOSE 8080
