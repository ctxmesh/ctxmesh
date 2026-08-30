# MCP echo server — standalone fixture image (does NOT use the agentry
# base image; this is a plain MCP server, not an instrumented agent).
#
# Build from repo root:
#   docker build -t mcp-echo-server:latest \
#       -f examples/mcp-echo-server/Dockerfile \
#       examples/mcp-echo-server
#
# Or via the Makefile target:
#   make docker-build-mcp-echo-server
FROM python:3.12-slim

WORKDIR /app

COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt

COPY server.py /app/server.py

# PORT env controls the listen port (default 8080).
# TOOL_VERSION env is the hot-swap marker (default v1).
ENV PORT=8080 \
    TOOL_VERSION=v1

EXPOSE 8080

CMD ["python", "/app/server.py"]
