"""managed-agent entrypoint — the stock, config-driven agent runtime (M14, ADR 0013).

This is the thin entrypoint for the **managed-agent image**: a first-class agent
whose behaviour is supplied by its ``AgentDeployment``, not baked into code. It
reads its configuration from the environment, then hands it to the reusable
``ctxmesh.run_managed_loop`` — the substance lives in the SDK (the M10 pattern:
SDK = substance, image = packaging), so the loop is testable in isolation and
this file stays tiny.

Where the behaviour comes from (config → behaviour; nothing agent-specific is
hardcoded here):

  * **System prompt** — ``SYSTEM_PROMPT`` env, or, when absent, the M9
    launcher-served prompt file (``PROMPT_FILE`` — a ConfigMap the controller
    materialises from the agent's promptRef; see internal/controller/prompt_inject.go).
    The expand ``systemPrompt`` field is delivered to the pod as the ``SYSTEM_PROMPT``
    env; the ``PROMPT_FILE`` fallback keeps it aligned with how every other agent
    gets its prompt, so a managed agent can also carry a git-backed PromptVersion.
  * **Model route** — ``MODEL_ROUTE`` env (the agent.yaml ``model.route``); the
    gateway resolves it to a real provider.
  * **Tools** — discovered live from the discovery plane (:2999): the bound
    MCPToolBindings the controller rendered from expand's ``tools: [...]``. The
    loop advertises their schemas to the model and dispatches via tools.call().
  * **Max steps** — ``MAX_STEPS`` env (a bound on tool-call iterations); defaults
    to the SDK's ``DEFAULT_MAX_STEPS``. Mandatory runaway guard (ADR 0013).

Runtime contract (mirrors the other agents — echo-agent / sdk-custom-agent):

  * POST /invoke   — body: {"input": "<prompt>"}
                     response: {"agent": <name>, "output": "<text>",
                                "steps": <int>, "tools_called": [<name>...]}
  * GET  /healthz  — 200 ok
  * GET  /readyz   — 200 ok

The launcher (PID 1) owns $AGENT_PORT and reverse-proxies to this listener; it
injects the W3C ``traceparent`` on every /invoke request, which the loop binds so
the whole step → tool → model tree roots under the launcher's ``agent.invoke``
span (the M10 invariant).
"""

from __future__ import annotations

import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict

from ctxmesh import DEFAULT_MAX_STEPS, ManagedConfig, agent, run_managed_loop

# ---------------------------------------------------------------------------
# Initialise the SDK client once (reads the launcher-injected env). from_env()
# fails fast with NotInPodError when no launcher env is present, surfacing at
# start-up rather than silently at first request.
# ---------------------------------------------------------------------------
_client = agent.from_env()

# The launcher owns the external $AGENT_PORT and proxies to our upstream port.
PORT = int(os.environ.get("AGENT_PORT", "8081"))

# The agent's display name (for the response envelope + trace).
AGENT_NAME = os.environ.get("AGENT_NAME", "managed-agent")


def _load_system_prompt() -> str:
    """Resolve the system prompt: SYSTEM_PROMPT env, else the M9 PROMPT_FILE.

    ``SYSTEM_PROMPT`` (expand's ``systemPrompt`` delivered as env) wins. When it
    is absent, fall back to the launcher-served prompt file (``PROMPT_FILE`` — the
    per-agent prompt ConfigMap the controller materialises from a promptRef, M9),
    so a managed agent can also source a git-backed PromptVersion. When neither is
    present, a minimal default keeps the agent functional rather than empty.
    """
    inline = os.environ.get("SYSTEM_PROMPT")
    if inline is not None and inline != "":
        return inline

    prompt_file = os.environ.get("PROMPT_FILE")
    if prompt_file:
        try:
            with open(prompt_file, encoding="utf-8") as fh:
                content = fh.read().strip()
            if content:
                return content
        except OSError:
            # A missing/unreadable prompt file is not fatal — fall through to the
            # default so the agent still serves (and surfaces its own errors on
            # the model plane rather than crashing at start-up).
            pass

    return "You are a helpful assistant."


def _load_config() -> ManagedConfig:
    """Build the ManagedConfig from the environment (config → behaviour)."""
    raw_max = os.environ.get("MAX_STEPS", "")
    try:
        max_steps = int(raw_max) if raw_max else DEFAULT_MAX_STEPS
    except ValueError:
        max_steps = DEFAULT_MAX_STEPS
    if max_steps < 1:
        max_steps = DEFAULT_MAX_STEPS

    return ManagedConfig(
        system_prompt=_load_system_prompt(),
        # MODEL_ROUTE mirrors the other agents; empty is a config error surfaced
        # by the gateway call, not silently swallowed.
        model_route=os.environ.get("MODEL_ROUTE", ""),
        max_steps=max_steps,
    )


# Resolve config once at start-up (behaviour is fixed for the pod's lifetime).
_config = _load_config()


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

        # Capture request headers for OTel context extraction — the launcher
        # injects ``traceparent`` so the loop roots under agent.invoke.
        req_headers = dict(self.headers)

        try:
            result = run_managed_loop(
                _client, _config, user_input, headers=req_headers
            )
            self._send(
                200,
                {
                    "agent": AGENT_NAME,
                    "output": result.output,
                    "steps": result.steps,
                    "tools_called": result.tools_called,
                    # MCP servers the user must connect an account to (ADR 0029 §2 / m25.9) —
                    # non-empty drives the console "Connect your account" CTA.
                    "consent_required": result.consent_required,
                },
            )
        except Exception as exc:  # noqa: BLE001
            self._send(502, {"agent": AGENT_NAME, "error": str(exc)})

    def log_message(self, *_args: Any) -> None:  # quiet default access logging
        pass


def main() -> None:
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)  # noqa: S104
    print(f"managed-agent {AGENT_NAME!r} listening on :{PORT}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
