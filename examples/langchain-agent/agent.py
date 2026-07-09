"""LangChain example agent — SDK-free deep-trace demo (M3/M4/M5).

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

M5 addition (m5.6): session memory via the launcher's :2998 endpoint.
When the /invoke request carries "conversation_id":
  1. GET localhost:2998/memory/{id}  (2s timeout) → prior turns as JSON array
     of {"role","content"} entries.  An empty array (first turn) is fine.
  2. Prior turns are prepended to the LangChain prompt as a system message.
  3. After answering, POST .../append the user turn then POST .../append the
     assistant turn (two appends, 2s timeouts each).
  4. Response gains "turns": <total entries after this exchange> (retention
     proof: turn1 → 2, turn2 on cold pod → 4).  "turns" is only reported
     when the GET *and both appends* succeeded — never for unpersisted state.
  5. Memory is best-effort: any GET/append failure → response carries
     "memory": "unavailable" and the agent still answers normally.
  6. conversation_id is validated client-side against the :2998 contract
     (non-empty, ≤128 chars, no '/', ':' or whitespace); an invalid id skips
     memory entirely and the response carries "memory":
     "invalid_conversation_id" (documented skip, not a phantom outage).
When "conversation_id" is absent the agent behaves exactly as before.

Fallback chain (all best-effort, never crash the agent):
  1. GET localhost:2999/tools  (sidecar)        <- live manifest
  2. read /etc/agent/tools.json                  <- cold-start / durable backing
  3. neither reachable -> M3 behaviour (local word_count, no extra fields)
"""

import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import quote
from urllib.request import urlopen, Request
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

# Launcher memory endpoint (m5.6).  Only active when MEMORY_BACKEND_ADDR is
# injected by the controller (MemoryBinding).  Agents that have no binding
# will get connection-refused, handled in the best-effort memory helpers below.
# Override via MEMORY_BASE_URL env var for test/local validation.
MEMORY_BASE_URL = os.environ.get("MEMORY_BASE_URL", "http://localhost:2998")
# Per-op timeout (seconds) — same 2s bound as the launcher's own Valkey ops.
MEMORY_TIMEOUT = 2


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
# Session memory helpers (m5.6) — all best-effort, never raise
# ---------------------------------------------------------------------------

def _valid_conversation_id(conv_id: str) -> bool:
    """Client-side mirror of the :2998 contract's conversationId constraints.

    Valid: non-empty, ≤128 chars, and contains no '/', ':' or whitespace.
    Mirroring the server-side rules here means a bad id surfaces as a clear
    client-side skip ("memory": "invalid_conversation_id") instead of a
    phantom outage (e.g. a slash-containing id 404ing on a mangled URL path).
    """
    if not conv_id or len(conv_id) > 128:
        return False
    return not any(c in "/:" or c.isspace() for c in conv_id)


def _memory_get(conv_id: str) -> list | None:
    """GET /memory/{conv_id} → list of {"role","content"} entries, or None.

    Returns None on any error (connection refused, timeout, bad JSON, non-200).
    An empty list from the endpoint means first turn — that is a valid result.
    """
    # quote(..., safe="") is defence-in-depth on top of _valid_conversation_id:
    # no validated id needs escaping, but the URL must never be corruptible.
    url = f"{MEMORY_BASE_URL}/memory/{quote(conv_id, safe='')}"
    try:
        with urlopen(url, timeout=MEMORY_TIMEOUT) as resp:  # noqa: S310
            if resp.status != 200:
                return None
            data = json.loads(resp.read())
            if isinstance(data, list):
                return data
            return None
    except Exception:  # noqa: BLE001 — best-effort
        return None


def _memory_append(conv_id: str, entry: dict) -> bool:
    """POST /memory/{conv_id}/append with one {"role","content"} entry.

    Returns True only when the endpoint acknowledged the append with a 2xx;
    False on any error (connection refused, timeout, non-2xx). Never raises.
    Uses compacted JSON (no extra whitespace) per the m5.4 contract.
    """
    url = f"{MEMORY_BASE_URL}/memory/{quote(conv_id, safe='')}/append"
    payload = json.dumps(entry, separators=(",", ":")).encode()
    req = Request(url, data=payload, headers={"Content-Type": "application/json"}, method="POST")  # noqa: S310
    try:
        with urlopen(req, timeout=MEMORY_TIMEOUT) as resp:  # noqa: S310
            return 200 <= resp.status < 300
    except Exception:  # noqa: BLE001 — best-effort
        return False


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

# Base prompt (no prior context).
_prompt = ChatPromptTemplate.from_messages(
    [
        ("system", "You are a concise assistant."),
        ("human", "User said: {input} (word count: {wc}). Reply in one sentence."),
    ]
)

# Prompt used when prior conversation context is available (m5.6).
# {prior_context} is a plain-text summary of previous turns injected as a
# second system message — deterministic, no extra parsing, not lossy.
_prompt_with_context = ChatPromptTemplate.from_messages(
    [
        ("system", "You are a concise assistant."),
        ("system", "Prior conversation context:\n{prior_context}"),
        ("human", "User said: {input} (word count: {wc}). Reply in one sentence."),
    ]
)


# ---------------------------------------------------------------------------
# Core agent logic
# ---------------------------------------------------------------------------

def run_agent(user_input: str, conv_id: str | None = None) -> dict:
    """Tool step then model step, optionally with session memory.

    When *conv_id* is provided (m5.6):
      - Prior turns are fetched from the launcher's :2998 memory endpoint.
      - The model is called with a prior-context system message.
      - User and assistant turns are appended after the response.
      - The response gains "turns": <total entries after this exchange>,
        reported ONLY when the GET and both appends all succeeded.
      - Memory failures are non-fatal: response gains "memory": "unavailable"
        but the agent still answers using current input only.
      - An invalid conv_id (empty, >128 chars, or containing '/', ':' or
        whitespace) skips memory entirely: "memory": "invalid_conversation_id".
    When *conv_id* is absent the behaviour is identical to M3/M4.

    Returns a dict with at least ``output``; may also contain ``tool_count``,
    ``tool_version``, ``tool_error``, ``turns``, and ``memory``.
    """
    extra: dict = {}

    # --- Session memory: validate id + fetch prior context (m5.6) ---
    prior_turns: list | None = None
    memory_ok = False
    if conv_id is not None and not _valid_conversation_id(conv_id):
        # Documented client-side skip (see _valid_conversation_id) — memory is
        # never contacted with an id the contract would reject.
        extra["memory"] = "invalid_conversation_id"
        conv_id = None
    if conv_id is not None:
        prior_turns = _memory_get(conv_id)
        memory_ok = prior_turns is not None  # None == fetch failed

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
    if conv_id is not None and memory_ok and prior_turns:
        # Build a deterministic plain-text summary of prior turns.
        # Format: "role: content" lines, one per turn.
        # v1: the injected prior-context is deliberately unbounded — no
        # windowing/truncation; context-window management is deferred.
        prior_lines = "\n".join(
            f"{t.get('role', 'unknown')}: {t.get('content', '')}"
            for t in prior_turns
        )
        chain = _prompt_with_context | _llm
        result = chain.invoke({"input": user_input, "wc": wc, "prior_context": prior_lines})
    else:
        chain = _prompt | _llm
        result = chain.invoke({"input": user_input, "wc": wc})

    extra["output"] = result.content

    # --- Session memory: append turns (m5.6) ---
    if conv_id is not None:
        if memory_ok:
            # The user/assistant append pair is NOT atomic: if the first lands
            # and the second fails, the partial pair is visible on the next
            # GET by design — surfaced as "unavailable" here, never masked.
            user_ok = _memory_append(conv_id, {"role": "user", "content": user_input})
            asst_ok = _memory_append(conv_id, {"role": "assistant", "content": result.content})
            if user_ok and asst_ok:
                # "turns" = prior entries + 2 new ones (deterministic retention
                # proof) — only reported for state actually persisted.
                prior_count = len(prior_turns) if prior_turns else 0
                extra["turns"] = prior_count + 2
            else:
                extra["memory"] = "unavailable"
        else:
            extra["memory"] = "unavailable"

    return extra


def _run_with_incoming_context(headers: dict, user_input: str, conv_id: str | None = None) -> dict:
    """Run the agent under the caller's W3C trace context.

    The launcher injects ``traceparent`` into the proxied request; stdlib
    http.server has no auto-instrumentation, so without an explicit extract
    the LangChain spans would each start their own ROOT trace and the
    reasoning tree fragments (observed 2026-07-08: six sibling traces in
    Langfuse instead of one). Best-effort: if OTel isn't available, run
    without a parent context.

    *conv_id* is forwarded to run_agent for session memory (m5.6).
    """
    try:
        from opentelemetry import context as otel_context  # noqa: PLC0415
        from opentelemetry.propagate import extract  # noqa: PLC0415

        token = otel_context.attach(extract(headers))
        try:
            return run_agent(user_input, conv_id=conv_id)
        finally:
            otel_context.detach(token)
    except ImportError:
        return run_agent(user_input, conv_id=conv_id)


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
            body = json.loads(raw or b"{}")
            user_input = body.get("input", "")
            # m5.6: extract conversation_id for session memory (optional).
            conv_id_raw = body.get("conversation_id")
            conv_id: str | None = str(conv_id_raw) if conv_id_raw is not None else None
        except json.JSONDecodeError:
            user_input = raw.decode(errors="replace")
            conv_id = None
        try:
            result = _run_with_incoming_context(dict(self.headers), user_input, conv_id=conv_id)
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
