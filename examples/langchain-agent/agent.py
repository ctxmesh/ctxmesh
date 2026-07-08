"""LangChain example agent — SDK-free deep-trace demo (M3/M4).

This agent contains ZERO agent-engine SDK calls and ZERO manual OTel
instrumentation. The base-python image's OpenInference auto-instrumentation
(sitecustomize.py) emits the chain / llm / tool spans; the launcher emits the
/invoke boundary span. Together they form the trace tree the M3 milestone
demonstrates.

M4 addition (m4.6): on each /invoke, fetch the live tool manifest from the
discovery sidecar at localhost:2999/tools.  If the manifest lists a remote
"word-count" tool, delegate to the MCP echo server (streamable-http) wrapped
as a LangChain @tool so OpenInference still emits the TOOL span.  The /invoke
response gains two extra fields: ``tool_count`` and ``tool_version`` (the
hot-swap marker asserted by m4.7 e2e).

Fallback chain (all best-effort, never crash the agent):
  1. GET localhost:2999/tools  (sidecar)        <- live manifest
  2. read /etc/agent/tools.json                  <- cold-start / durable backing
  3. neither reachable -> M3 behaviour (local word_count, no extra fields)
"""

import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.request import urlopen
from urllib.error import URLError

from langchain_core.prompts import ChatPromptTemplate
from langchain_core.tools import tool
from langchain_openai import ChatOpenAI

# The launcher owns the external $AGENT_PORT and proxies to the user process
# on the upstream port; it passes that upstream port to us as AGENT_PORT.
PORT = int(os.environ.get("AGENT_PORT", "8081"))

# Gateway is OpenAI-compatible; the openai client appends /chat/completions to
# base_url. Default targets the in-cluster M2 gateway. api_key is never a real
# provider key — the gateway holds those (mock route needs none).
GATEWAY_URL = os.environ.get(
    "MODEL_GATEWAY_URL",
    "http://agent-engine-gateway.agent-engine-system.svc:4000",
)
MODEL_ROUTE = os.environ.get("MODEL_ROUTE", "default-model")

# Discovery sidecar manifest endpoint (localhost, always same pod).
SIDECAR_MANIFEST_URL = "http://localhost:2999/tools"
# Durable backing: ConfigMap volume mount (cold-start fallback only).
TOOLS_JSON_PATH = os.environ.get("TOOLS_JSON_PATH", "/etc/agent/tools.json")

# Manifest fetch timeout (seconds) — cheap localhost GET; generous enough for
# a waking pod but small enough not to stall the agent under invoke latency.
MANIFEST_TIMEOUT = 2


# ---------------------------------------------------------------------------
# Manifest discovery helpers
# ---------------------------------------------------------------------------

def _fetch_manifest() -> dict | None:
    """Return the tool manifest dict or None if unreachable.

    Priority:
      1. Discovery sidecar  (localhost:2999/tools)
      2. tools.json mount   (/etc/agent/tools.json or TOOLS_JSON_PATH)
    """
    # 1. Live sidecar manifest.
    try:
        with urlopen(SIDECAR_MANIFEST_URL, timeout=MANIFEST_TIMEOUT) as resp:  # noqa: S310
            return json.loads(resp.read())
    except (URLError, OSError, json.JSONDecodeError):
        pass

    # 2. Durable backing file (ConfigMap mount or TOOLS_JSON_PATH override).
    try:
        with open(TOOLS_JSON_PATH) as fh:
            return json.load(fh)
    except (OSError, json.JSONDecodeError):
        pass

    return None


def _find_remote_tool(manifest: dict, name: str) -> dict | None:
    """Return the first tool entry matching *name* in the manifest, or None."""
    for entry in manifest.get("tools", []):
        if entry.get("name") == name:
            return entry
    return None


# ---------------------------------------------------------------------------
# MCP call (streamable-http, no SDK)
# ---------------------------------------------------------------------------

def _call_mcp_word_count(endpoint: str, text: str) -> dict:
    """Call the remote word-count MCP tool via streamable-http.

    *endpoint* is taken verbatim from the manifest (already includes /mcp per
    m4.4 review finding — e.g. ``http://host/mcp``).

    Returns a dict with at least ``count`` (int) and ``server_version`` (str),
    parsed from the JSON-string tool result.

    Raises on any transport or protocol error so the caller can surface a
    ``tool_error`` gracefully.
    """
    from mcp import ClientSession  # noqa: PLC0415
    from mcp.client.streamable_http import streamablehttp_client  # noqa: PLC0415
    import anyio  # noqa: PLC0415

    async def _run() -> str:
        async with streamablehttp_client(endpoint) as (read, write, _):
            async with ClientSession(read, write) as session:
                await session.initialize()
                result = await session.call_tool("word_count", {"text": text})
                # MCP tool results: list of content items; first item is text.
                raw = result.content[0].text
                return raw

    raw_json = anyio.run(_run)
    return json.loads(raw_json)


# ---------------------------------------------------------------------------
# Local fallback tool (M3 behaviour, always present)
# ---------------------------------------------------------------------------

@tool
def word_count(text: str) -> int:
    """Count the number of whitespace-separated words in text."""
    return len(text.split())


# ---------------------------------------------------------------------------
# LangChain @tool wrapper for the MCP remote tool
# ---------------------------------------------------------------------------

def _make_mcp_word_count_tool(endpoint: str):
    """Return a LangChain @tool that calls the remote MCP word-count tool.

    Wrapping as a @tool ensures OpenInference emits a TOOL span with
    ``tool.name = mcp_word_count``, satisfying the m4 trace assertion.

    The closure captures *endpoint* so we can rebuild the tool on each invoke
    if the manifest changes (hot-swap) — the function is cheap to construct.
    """

    @tool
    def mcp_word_count(text: str) -> str:
        """Count words via the remote MCP word-count tool server."""
        raw = _call_mcp_word_count(endpoint, text)
        return json.dumps(raw)

    return mcp_word_count


# ---------------------------------------------------------------------------
# LLM and prompt (shared, stateless)
# ---------------------------------------------------------------------------

_llm = ChatOpenAI(
    base_url=GATEWAY_URL,
    model=MODEL_ROUTE,
    api_key="dummy",  # noqa: S106 — gateway holds real keys; this is never used
    timeout=30,
    max_retries=0,
)

_prompt = ChatPromptTemplate.from_messages(
    [
        ("system", "You are a concise assistant."),
        ("human", "User said: {input} (word count: {wc}). Reply in one sentence."),
    ]
)


# ---------------------------------------------------------------------------
# Core agent logic
# ---------------------------------------------------------------------------

def run_agent(user_input: str) -> dict:
    """Tool step then model step.

    Returns a dict with at least ``output``; may also contain ``tool_count``,
    ``tool_version``, and ``tool_error``.
    """
    extra: dict = {}

    # --- Manifest discovery ---
    manifest = _fetch_manifest()

    if manifest is not None:
        wc_entry = _find_remote_tool(manifest, "word-count")
    else:
        wc_entry = None

    # --- Tool call ---
    if wc_entry is not None:
        endpoint = wc_entry["endpoint"]
        mcp_wc_tool = _make_mcp_word_count_tool(endpoint)
        try:
            raw_result = mcp_wc_tool.invoke({"text": user_input})
            parsed = json.loads(raw_result)
            wc = parsed.get("count", len(user_input.split()))
            extra["tool_count"] = parsed["count"]
            extra["tool_version"] = parsed["server_version"]
        except Exception as exc:  # noqa: BLE001 — best-effort, never crash agent
            # Tool failure: surface the error, fall back to local word count.
            extra["tool_error"] = str(exc)
            wc = word_count.invoke({"text": user_input})
    else:
        # M3 / fallback: local @tool word_count.
        wc = word_count.invoke({"text": user_input})

    # --- Model call ---
    chain = _prompt | _llm
    result = chain.invoke({"input": user_input, "wc": wc})
    extra["output"] = result.content
    return extra


def _run_with_incoming_context(headers: dict, user_input: str) -> dict:
    """Run the agent under the caller's W3C trace context.

    The launcher injects ``traceparent`` into the proxied request; stdlib
    http.server has no auto-instrumentation, so without an explicit extract
    the LangChain spans would each start their own ROOT trace and the
    reasoning tree fragments (observed 2026-07-08: six sibling traces in
    Langfuse instead of one). Best-effort: if OTel isn't available, run
    without a parent context.
    """
    try:
        from opentelemetry import context as otel_context  # noqa: PLC0415
        from opentelemetry.propagate import extract  # noqa: PLC0415

        token = otel_context.attach(extract(headers))
        try:
            return run_agent(user_input)
        finally:
            otel_context.detach(token)
    except ImportError:
        return run_agent(user_input)


# ---------------------------------------------------------------------------
# HTTP server
# ---------------------------------------------------------------------------

class Handler(BaseHTTPRequestHandler):
    def _send(self, code: int, body: dict) -> None:
        payload = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self) -> None:  # noqa: N802 — BaseHTTPRequestHandler API
        if self.path in ("/healthz", "/readyz"):
            self._send(200, {"status": "ok"})
        else:
            self._send(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802 — BaseHTTPRequestHandler API
        if self.path != "/invoke":
            self._send(404, {"error": "not found"})
            return
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length) if length else b"{}"
        try:
            user_input = json.loads(raw or b"{}").get("input", "")
        except json.JSONDecodeError:
            user_input = raw.decode(errors="replace")
        try:
            result = _run_with_incoming_context(dict(self.headers), user_input)
            self._send(200, {"agent": "langchain-agent", **result})
        except Exception as exc:  # noqa: BLE001 — surface upstream errors to caller
            self._send(502, {"agent": "langchain-agent", "error": str(exc)})

    def log_message(self, *_args) -> None:  # quiet default access logging
        pass


def main() -> None:
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)  # noqa: S104
    print(f"langchain-agent listening on :{PORT}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
