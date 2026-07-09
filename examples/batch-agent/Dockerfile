# batch-agent — run-once example for the `job` execution model (M7).
#
# This image is NOT an HTTP server. When the launcher spawns it, the
# agent runs to completion and exits. The launcher propagates the exit code
# to the K8s Job / CronJob controller.
#
# Build from repo root (base image first):
#   docker build -t base-python:latest -f images/base-python/Dockerfile .
#   docker build -t batch-agent:latest -f examples/batch-agent/Dockerfile .
FROM base-python:latest

WORKDIR /app

# No third-party deps: stdlib-only agent — nothing to pip-install.
COPY examples/batch-agent/agent.py /app/agent.py

# Tell the launcher to exec this script as the child process.
# The launcher (PID 1) spawns it, waits for it to exit, then exits with
# the same code — satisfying the K8s Job / CronJob completion contract.
ENV AGENT_ENTRYPOINT=/usr/local/bin/python \
    AGENT_ENTRYPOINT_ARGS=/app/agent.py

# Input: set BATCH_TASK (or BATCH_TASK_FILE / CLI arg) at runtime.
# Failure injection: BATCH_FAIL=1 causes exit 2.
