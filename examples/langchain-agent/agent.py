"""LangChain example agent — SDK-free deep-trace demo (M3).

This agent contains ZERO agent-engine SDK calls and ZERO manual OTel
instrumentation. The base-python image's OpenInference auto-instrumentation
(sitecustomize.py) emits the chain / llm / tool spans; the launcher emits the
/invoke boundary span. Together they form the trace tree the M3 milestone
demonstrates.

On POST /invoke it runs a minimal LangChain flow: a tool call (word_count)
followed by a model call through the in-cluster gateway (LiteLLM, OpenAI-
compatible). Health endpoints are gateway-independent so the container is
Ready before any model traffic.
"""

import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

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


@tool
def word_count(text: str) -> int:
    """Count the number of whitespace-separated words in text."""
    return len(text.split())


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


def run_agent(user_input: str) -> str:
    """Tool step then model step — each auto-traced as a span."""
    wc = word_count.invoke({"text": user_input})
    chain = _prompt | _llm
    result = chain.invoke({"input": user_input, "wc": wc})
    return result.content


def _run_with_incoming_context(headers: dict, user_input: str) -> str:
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
            output = _run_with_incoming_context(dict(self.headers), user_input)
            self._send(200, {"agent": "langchain-agent", "output": output})
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
