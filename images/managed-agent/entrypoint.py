"""managed-agent entrypoint — the stock, config-driven agent runtime (M14, ADR 0013).

This is the thin entrypoint for the **managed-agent image**: a first-class agent whose
behaviour is supplied by its ``AgentDeployment``, not baked into code. Since DX-3 the entire
runtime — the ``POST /invoke`` contract, the ``/healthz``/``/readyz`` probes, the ``$AGENT_PORT``
the launcher proxies to, the ``traceparent`` capture, the run-capability binding, the autonomous
conversation-id mint, and the SSE token stream — lives in ``ctxmesh.serve``. Called with no
handler it runs the stock tool-calling loop (:func:`ctxmesh.run_managed_loop`) with its config
resolved from the environment (:meth:`ctxmesh.ManagedConfig.from_env`), so this file is a
one-liner and every agent (managed or hand-rolled) shares ONE serving contract.

Where the behaviour comes from (config → behaviour; nothing agent-specific is hardcoded here):

  * **System prompt** — ``SYSTEM_PROMPT`` env, or, when absent, the M9 launcher-served prompt
    file (``PROMPT_FILE`` — a ConfigMap the controller materialises from the agent's promptRef).
  * **Model route** — ``MODEL_ROUTE`` env (the agent.yaml ``model.route``).
  * **Tools** — discovered live from the discovery plane (:2999): the bound MCPToolBindings the
    controller rendered from expand's ``tools: [...]``.
  * **Max steps** — ``MAX_STEPS`` env; defaults to the SDK's ``DEFAULT_MAX_STEPS`` (ADR 0013).

Runtime contract (mirrors every other agent — echo-agent / sdk-custom-agent):

  * POST /invoke   — body: {"input": "<prompt>"} → {"agent", "output", "steps", "tools_called"}
                     (SSE token stream when the caller sends ``Accept: text/event-stream``)
  * GET  /healthz  — 200 ok
  * GET  /readyz   — 200 ok
"""

from __future__ import annotations

import ctxmesh


def main() -> None:
    # No handler ⇒ the stock managed loop, config from the environment. serve() reads
    # $AGENT_NAME / $AGENT_PORT and builds the client via ctxmesh.agent.from_env() (which
    # fails fast with NotInPodError at start-up if the launcher env is absent).
    ctxmesh.serve()


if __name__ == "__main__":
    main()
