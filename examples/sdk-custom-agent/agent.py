"""sdk-custom-agent — no-framework loop using the ctxmesh SDK (M10 example).

This is the milestone's 🧪 example: a hand-written loop with NO LangChain /
LlamaIndex / OpenAI SDK.  The trace tree (AGENT → step/CHAIN → tool/TOOL →
model/LLM) is emitted *explicitly* via ``client.trace.*`` rather than by
auto-instrumentation — that is the entire point of the SDK's step-tracing
helpers.

Trace structure per /invoke:

    agent.invoke               (launcher — the boundary span)
    └─ sdk-custom-agent (AGENT)  ← client.trace.loop — rooted under agent.invoke
       ├─ plan (CHAIN)           ← client.trace.step
       │  └─ chat gpt-4o-mini (LLM)  ← client.model.chat (auto)
       └─ word-count (TOOL)      ← client.trace.tool
          └─ chat gpt-4o-mini (LLM)  ← client.model.chat (auto)

The launcher injects the W3C ``traceparent`` on every proxied /invoke request.
``client.trace.loop(name, headers=request.headers)`` extracts it so every span
opened inside is a *child* of the launcher's ``agent.invoke`` span — same trace
id, correct parent span id — rather than a detached new trace.  This is the
invariant m10.5 asserts.

Runtime contract (mirrors echo-agent):
  - POST /invoke   — body: {"input": "<prompt>"}
                     response: {"agent":"sdk-custom-agent","output":"<text>",
                                "word_count":<int>}
  - GET  /healthz  — 200 ok
  - GET  /readyz   — 200 ok

Environment variables (injected by the launcher in-pod):
  MODEL_GATEWAY_URL  — model gateway base URL
  MODEL_ROUTE        — gateway route alias (e.g. "gpt-4o-mini")
  AGENT_PORT         — port the launcher assigned for our upstream listener
  AGENT_NAME         — agent identity (read by from_env)
  OTEL_EXPORTER_OTLP_ENDPOINT — OTLP/gRPC collector (:4317)
  (MEMORY_PORT, FEEDBACK_PORT, etc. — forwarded by from_env; unused here)
"""

from __future__ import annotations

import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict

from ctxmesh import agent

# ---------------------------------------------------------------------------
# Initialise the SDK client once (reads the launcher-injected env).
# from_env() fails fast with NotInPodError when no launcher env is present,
# which surfaces immediately at start-up — never silently at first request.
# ---------------------------------------------------------------------------
_client = agent.from_env()

# ---------------------------------------------------------------------------
# Runtime constants
# ---------------------------------------------------------------------------

# The launcher owns the external $AGENT_PORT and proxies to our upstream port.
PORT = int(os.environ.get("AGENT_PORT", "8081"))

# Gateway route alias — matches the ToolRegistry / LiteLLM route config.
MODEL_ROUTE = os.environ.get("MODEL_ROUTE", "gpt-4o-mini")

# The catalog name for the word-count tool (matches MCPToolBinding name in the
# agent's ToolRegistry entry, which `client.tools.call` resolves to the MCP
# tool name automatically via tools/list handshake).
WORD_COUNT_TOOL = "word-count"

# Deterministic system prompt — the mock gateway is configured to reply to this
# exact phrasing with a fixed response (MOCK_OK), keeping the e2e reproducible.
_SYSTEM_PROMPT = "You are a concise assistant. Reply in one sentence."


# ---------------------------------------------------------------------------
# Core agent logic: step → tool → model trace tree
# ---------------------------------------------------------------------------


def _run_invoke(user_input: str, headers: Dict[str, str]) -> Dict[str, Any]:
    """Execute the no-framework loop under the launcher's trace context.

    The ``with client.trace.loop(...)`` binds the inbound W3C ``traceparent``
    (which the launcher injected into the /invoke request headers) so the whole
    step→tool→model tree roots under the launcher's ``agent.invoke`` span.
    Without this the SDK spans would start a detached trace — a hard FAIL for
    the m10.5 invariant.

    Step→tool→model structure:

    1. **plan step (CHAIN):** call the model to produce a terse plan/summary of
       the user prompt.  The model.chat call auto-emits an LLM child span.

    2. **word-count tool (TOOL):** invoke the word-count MCP tool via
       client.tools.call.  The SDK emits the TOOL span; the actual MCP
       round-trip is handled entirely by the ToolsClient (handshake + tools/list
       + tools/call).

    3. **answer step (CHAIN):** synthesise the final answer from the plan and
       the word count.  client.model.chat emits another LLM child span.

    Returns a dict with ``output`` (the final answer) and ``word_count`` (int).
    """
    with _client.trace.loop("sdk-custom-agent", headers=headers) as root:
        root.set_input(user_input)

        # ── step 1: plan ─────────────────────────────────────────────────────
        with _client.trace.step("plan") as plan_step:
            plan_step.set_input(user_input)
            plan_messages = [
                {"role": "system", "content": _SYSTEM_PROMPT},
                {"role": "user", "content": f"Summarise in one sentence: {user_input}"},
            ]
            plan_resp = _client.model.chat(MODEL_ROUTE, plan_messages)
            plan_step.set_output(plan_resp.text)

        # ── step 2: word-count tool call ──────────────────────────────────────
        word_count = 0
        with _client.trace.tool(WORD_COUNT_TOOL, input={"text": user_input}) as tool_span:
            tool_result = _client.tools.call(WORD_COUNT_TOOL, text=user_input)
            # tool_result is the JSON-parsed MCP response: {"count":<int>, ...}
            if isinstance(tool_result, dict):
                word_count = int(tool_result.get("count", len(user_input.split())))
            else:
                word_count = len(user_input.split())
            tool_span.set_output({"word_count": word_count})

        # ── step 3: final answer ──────────────────────────────────────────────
        with _client.trace.step("answer") as answer_step:
            answer_messages = [
                {"role": "system", "content": _SYSTEM_PROMPT},
                {
                    "role": "user",
                    "content": (
                        f"User said: {user_input!r} ({word_count} words). "
                        f"Plan summary: {plan_resp.text!r}. Reply in one sentence."
                    ),
                },
            ]
            answer_resp = _client.model.chat(MODEL_ROUTE, answer_messages)
            answer_step.set_input(answer_messages[-1]["content"])
            answer_step.set_output(answer_resp.text)

        root.set_output(answer_resp.text)

    return {"output": answer_resp.text, "word_count": word_count}


# ---------------------------------------------------------------------------
# HTTP server (mirrors echo-agent's contract and langchain-agent's shape)
# ---------------------------------------------------------------------------


class Handler(BaseHTTPRequestHandler):
    def _send(self, code: int, body: dict) -> None:
        payload = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self) -> None:  # noqa: N802
        if self.path in ("/healthz", "/readyz"):
            self._send(200, {"status": "ok"})
        else:
            self._send(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path != "/invoke":
            self._send(404, {"error": "not found"})
            return

        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length) if length else b"{}"
        try:
            body = json.loads(raw or b"{}")
            user_input = str(body.get("input", ""))
        except json.JSONDecodeError:
            user_input = raw.decode(errors="replace")

        # Capture all request headers for OTel context extraction.
        # The launcher injects ``traceparent`` here so loop() can root under
        # agent.invoke.  dict() normalises BaseHTTP's case-sensitive mapping.
        req_headers = dict(self.headers)

        try:
            result = _run_invoke(user_input, req_headers)
            self._send(200, {"agent": "sdk-custom-agent", **result})
        except Exception as exc:  # noqa: BLE001
            self._send(502, {"agent": "sdk-custom-agent", "error": str(exc)})

    def log_message(self, *_args: Any) -> None:  # quiet default access logging
        pass


def main() -> None:
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)  # noqa: S104
    print(f"sdk-custom-agent listening on :{PORT}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
