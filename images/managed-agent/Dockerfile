# images/managed-agent/Dockerfile — the managed-agent runtime image (M14, ADR 0013).
#
# A stock, platform-owned agent whose behaviour is supplied by config (a system
# prompt + a set of bound tools), NOT by user code — so a persona/assistant agent
# needs no user Docker build. This is the on-ramp for "describe an agent → a
# running agent" (§30); custom-image agents remain first-class and unchanged.
#
# Built on base-python, which already bundles the launcher (PID 1, the full
# runtime contract) + the ctxmesh SDK (so `import ctxmesh` — and the reusable
# run_managed_loop — is available with zero extra installs). The image is a THIN
# entrypoint over the SDK loop (the M10 pattern: SDK = substance, image = package).
#
# Build from repo root (base image first):
#   docker build -t base-python:latest -f images/base-python/Dockerfile .
#   docker build -t managed-agent:latest -f images/managed-agent/Dockerfile .
#
# Or via the Makefile target:
#   make docker-build-managed
#
# Configuration (read by the entrypoint at start-up — config → behaviour):
#   SYSTEM_PROMPT  — the agent's system prompt (expand's systemPrompt); when
#                    absent, the M9 launcher-served PROMPT_FILE is used.
#   MODEL_ROUTE    — the gateway route alias (agent.yaml model.route).
#   MAX_STEPS      — bound on tool-call iterations (default: SDK DEFAULT_MAX_STEPS).
# The bound tools come from the discovery plane (:2999) — the MCPToolBindings the
# controller renders from expand's `tools: [...]`.
#
# BASE_IMAGE lets the release build compose FROM the PUBLISHED, multi-arch
# base-python (ghcr.io/ctxmesh/agent-engine-base-python:<version>@digest, m50/ADR
# 0048) instead of the local `base-python:latest` — so `managed` is a multi-arch
# manifest too. Defaults to the local dev image so `make docker-build-managed`
# (which builds base-python:latest first) is unchanged.
ARG BASE_IMAGE=base-python:latest
FROM ${BASE_IMAGE}

WORKDIR /app

# No extra requirements: ctxmesh and its deps are already installed in
# base-python. The entrypoint is pure stdlib + ctxmesh.
COPY images/managed-agent/entrypoint.py /app/entrypoint.py

# The launcher spawns this as the child process (listening on the upstream port
# the launcher assigns via AGENT_PORT) and reverse-proxies $AGENT_PORT to it.
ENV AGENT_ENTRYPOINT=/usr/local/bin/python \
    AGENT_ENTRYPOINT_ARGS=/app/entrypoint.py

EXPOSE 8080
