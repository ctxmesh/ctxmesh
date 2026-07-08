"""MCP echo server — fixture for M4 e2e tests.

Exposes ONE tool: ``word_count(text: str)`` that returns a deterministic
JSON string containing the word count and a version marker (``TOOL_VERSION``
env var, default ``v1``).  The version marker lets the hot-swap assertion in
the M4 e2e confirm that the agent sees the new server version after a binding
update, without a pod restart.

Transport: streamable-http (MCP spec, HTTP+SSE).
Port:      ``PORT`` env var, default 8080.

Health check:
  ``GET /healthz`` → 200 ``{"status": "ok"}`` via FastMCP's
  ``custom_route`` (a plain Starlette route on the same app/port, outside
  the MCP protocol) — usable directly by Knative HTTP readiness probes.
"""

import json
import os

from mcp.server.fastmcp import FastMCP
from starlette.requests import Request
from starlette.responses import JSONResponse

PORT = int(os.environ.get("PORT", "8080"))
TOOL_VERSION = os.environ.get("TOOL_VERSION", "v1")

# host and port are FastMCP settings kwargs (not run() kwargs).
# The streamable-http path defaults to /mcp.
mcp = FastMCP("mcp-echo-server", host="0.0.0.0", port=PORT)


@mcp.tool()
def word_count(text: str) -> str:
    """Count whitespace-separated words in *text*.

    Returns a JSON string with ``count`` (int) and ``server_version`` (str)
    so the M4 e2e can assert the hot-swap version flip without parsing a
    human-readable sentence.

    The return type is ``str`` (not ``dict``) because the MCP protocol
    serialises tool results as text content; using str avoids any
    SDK-version-dependent dict→content wrapping behaviour.
    """
    count = len(text.split()) if text.strip() else 0
    return json.dumps({"count": count, "server_version": TOOL_VERSION})


@mcp.custom_route("/healthz", methods=["GET"])
async def healthz(_request: Request) -> JSONResponse:
    """Plain HTTP health endpoint (outside the MCP protocol) for probes."""
    return JSONResponse({"status": "ok", "server_version": TOOL_VERSION})


if __name__ == "__main__":
    print(
        f"mcp-echo-server starting on :{PORT} "
        f"(TOOL_VERSION={TOOL_VERSION})",
        flush=True,
    )
    mcp.run(transport="streamable-http")
