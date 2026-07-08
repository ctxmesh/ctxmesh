# LangChain example agent — built on the OpenInference-instrumented base image.
# The base image's ENTRYPOINT is the launcher (PID 1, traced proxy); this image
# only tells the launcher what to spawn via AGENT_ENTRYPOINT(_ARGS). It must NOT
# override ENTRYPOINT.
#
# Build from repo root (base image first):
#   docker build -t base-python:latest -f images/base-python/Dockerfile .
#   docker build -t langchain-agent:latest -f examples/langchain-agent/Dockerfile .
FROM base-python:latest

WORKDIR /app

COPY examples/langchain-agent/requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt

COPY examples/langchain-agent/agent.py /app/agent.py

# The launcher spawns this as the child process (listening on the upstream port
# the launcher assigns via AGENT_PORT) and reverse-proxies $AGENT_PORT to it.
ENV AGENT_ENTRYPOINT=/usr/local/bin/python \
    AGENT_ENTRYPOINT_ARGS=/app/agent.py

EXPOSE 8080
