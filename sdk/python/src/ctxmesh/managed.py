"""The managed-agent loop — a generic, config-driven tool-calling agent (M14, ADR 0013).

The managed runtime (``images/managed-agent/``) is a stock image whose behaviour
is supplied by the ``AgentDeployment``, not baked into code: a system prompt + a
set of tools. This module is the reusable *substance* behind that image — a
no-framework tool-calling loop so the image is a thin entrypoint (the M10 pattern:
SDK = substance, image = packaging).

**The loop** (ADR 0013):

    system prompt
      → model.chat(messages, tools=<schemas from tools.list()>)
      → if the assistant returned tool_calls: dispatch each via tools.call(),
        append a role:"tool" result for each, and loop
      → otherwise return the final completion text.

It is **bounded** by a max-steps guard (:class:`ManagedConfig.max_steps`) so a
model that loops forever on tool calls cannot hang the pod — the guard is a hard
stop that raises :class:`~ctxmesh.errors.ConfigError`.

It is **fully traced** (M3/M10): the whole run is one ``AGENT`` span rooted under
the launcher's ``agent.invoke`` (when request headers are passed), each iteration
is a ``CHAIN`` step, each tool dispatch a ``TOOL`` span, and ``model.chat`` emits
its own ``LLM`` span — the same ``step → tool → model`` tree any custom agent
emits, so a managed agent is indistinguishable from a hand-written one in Langfuse.

Behaviour comes entirely from :class:`ManagedConfig`; nothing agent-specific is
hardcoded here. The image's entrypoint builds a config from its environment (the
system prompt, the model route) and hands it to :func:`run_managed_loop`.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional

from ctxmesh.client import Client
from ctxmesh.errors import ConfigError

#: A sane default bound: enough for a few tool round-trips, low enough that a
#: runaway (a model that keeps calling tools) trips it quickly. Overridable via
#: config (the image reads MAX_STEPS).
DEFAULT_MAX_STEPS = 8


@dataclass
class ManagedConfig:
    """The behaviour of one managed-agent run — everything comes from here.

    Nothing about a specific agent is hardcoded in the loop or the image: the
    system prompt and the model route are config, the tool set is discovered
    live from the plane (the bound MCPToolBindings the controller rendered), and
    the loop is bounded by ``max_steps``.
    """

    #: The system prompt that shapes the agent's persona/behaviour. Supplied by
    #: the AgentDeployment (expand's ``systemPrompt``), served to the image as an
    #: env var or the launcher-served prompt file.
    system_prompt: str

    #: The gateway route alias for model.chat (the ``MODEL_ROUTE`` env / the
    #: agent.yaml ``model.route``). The gateway resolves it to a real provider.
    model_route: str

    #: Hard bound on loop iterations (model turns). When exceeded the loop raises
    #: rather than hanging — mandatory per ADR 0013.
    max_steps: int = DEFAULT_MAX_STEPS

    #: Optional extra ``model.chat`` body opts (temperature, max_tokens, …).
    model_opts: Dict[str, Any] = field(default_factory=dict)


@dataclass
class ManagedResult:
    """The outcome of a managed run: the final text + how it got there."""

    #: The final assistant completion once the model stopped calling tools.
    output: str
    #: The number of model turns taken (1 = answered without any tool call).
    steps: int
    #: The catalog names of the tools dispatched, in call order.
    tools_called: List[str]


def _tool_schema(name: str) -> Dict[str, Any]:
    """Build an OpenAI ``tools[]`` function schema for a discovered tool.

    The discovery manifest carries only the catalog name + endpoint (the MCP
    input schema is resolved at call time, tools.py) — not a JSON schema. So the
    loop advertises each bound tool with a permissive object-parameters schema:
    enough for the gateway to relay ``tools`` and for the model to name the tool
    it wants; the concrete arguments the model produces are passed through to
    ``tools.call`` verbatim.
    """
    return {
        "type": "function",
        "function": {
            "name": name,
            "description": f"The {name} tool bound to this agent.",
            "parameters": {"type": "object", "properties": {}, "additionalProperties": True},
        },
    }


def _parse_arguments(raw: Any) -> Dict[str, Any]:
    """Parse a tool call's ``arguments`` (OpenAI serialises them as a JSON string).

    Returns ``{}`` for empty/absent arguments; a non-object JSON (e.g. a bare
    string) is wrapped so ``tools.call(**args)`` never fails on a scalar.
    """
    if raw is None or raw == "":
        return {}
    if isinstance(raw, dict):
        return raw
    if isinstance(raw, str):
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError:
            return {}
        return parsed if isinstance(parsed, dict) else {}
    return {}


def _tool_result_content(result: Any) -> str:
    """Render a tools.call result as the string content of a role:"tool" message."""
    if isinstance(result, str):
        return result
    try:
        return json.dumps(result, separators=(",", ":"), default=str)
    except (TypeError, ValueError):
        return str(result)


def run_managed_loop(
    client: Client,
    config: ManagedConfig,
    user_input: str,
    *,
    headers: Optional[Dict[str, str]] = None,
) -> ManagedResult:
    """Run the config-driven tool-calling loop for one user turn.

    *client* is a :class:`~ctxmesh.client.Client` (``agent.from_env()`` in-pod).
    *config* supplies the system prompt, model route, and the ``max_steps`` bound.
    *user_input* is the inbound request text. *headers* (the launcher-injected
    request headers) bind the trace so the whole tree roots under ``agent.invoke``.

    Returns a :class:`ManagedResult` with the final completion, the step count,
    and the tools dispatched. Raises :class:`~ctxmesh.errors.ConfigError` if the
    ``max_steps`` bound is hit before the model stops calling tools (the runaway
    guard) — errors from the model/tool planes surface unchanged (never swallowed).
    """
    # Discover the bound tools once per run: the manifest the controller rendered
    # from this agent's MCPToolBindings (:2999). Absent/empty is fine — the loop
    # then behaves like a plain chat agent (no tools advertised).
    tools = client.tools.list()
    tool_names = {t.name for t in tools}
    tool_schemas = [_tool_schema(t.name) for t in tools]

    messages: List[Dict[str, Any]] = [
        {"role": "system", "content": config.system_prompt},
        {"role": "user", "content": user_input},
    ]
    tools_called: List[str] = []

    with client.trace.loop("managed-agent", headers=headers) as root:
        root.set_input(user_input)

        for step in range(1, config.max_steps + 1):
            with client.trace.step(f"turn-{step}") as turn:
                # model.chat emits its own LLM span nested under this step.
                chat_opts: Dict[str, Any] = dict(config.model_opts)
                if tool_schemas:
                    chat_opts["tools"] = tool_schemas
                resp = client.model.chat(config.model_route, messages, **chat_opts)

                if not resp.has_tool_calls:
                    # The model stopped calling tools → this is the final answer.
                    turn.set_output(resp.text)
                    root.set_output(resp.text)
                    return ManagedResult(
                        output=resp.text, steps=step, tools_called=tools_called
                    )

                # A tool-calling turn: append the assistant message verbatim
                # (OpenAI requires it to precede the tool results), then dispatch
                # each call and append a role:"tool" result.
                messages.append(_assistant_message_for_history(resp))
                turn.set_output({"tool_calls": [_call_name(c) for c in resp.tool_calls]})

                for call in resp.tool_calls:
                    name = _call_name(call)
                    args = _parse_arguments(_call_arguments(call))
                    call_id = call.get("id", "")

                    if name not in tool_names:
                        # A tool the agent is not bound to — surface it as the
                        # tool result so the model can recover, rather than
                        # crashing the run on a hallucinated tool name.
                        content = f"error: tool {name!r} is not bound to this agent"
                    else:
                        with client.trace.tool(name, input=args) as tool_span:
                            result = client.tools.call(name, **args)
                            tool_span.set_output(result)
                        content = _tool_result_content(result)
                        tools_called.append(name)

                    messages.append(
                        {
                            "role": "tool",
                            "tool_call_id": call_id,
                            "name": name,
                            "content": content,
                        }
                    )

        # Bound exceeded: the model kept calling tools past max_steps. Hard stop
        # rather than hang the pod (the mandatory runaway guard, ADR 0013).
        raise ConfigError(
            f"managed loop exceeded max_steps={config.max_steps} without a final "
            f"completion (the model kept calling tools). Tools called so far: "
            f"{tools_called!r}."
        )


def _call_name(call: Dict[str, Any]) -> str:
    fn = call.get("function")
    if isinstance(fn, dict):
        name = fn.get("name")
        if isinstance(name, str):
            return name
    return ""


def _call_arguments(call: Dict[str, Any]) -> Any:
    fn = call.get("function")
    if isinstance(fn, dict):
        return fn.get("arguments")
    return None


def _assistant_message_for_history(resp: Any) -> Dict[str, Any]:
    """The assistant message to append to history on a tool-calling turn.

    Prefer the raw message object off the response (it carries the ``tool_calls``
    array in the exact shape the follow-up request needs); fall back to a
    reconstructed message if the body is unusual.
    """
    message = resp.message
    if isinstance(message, dict) and message.get("tool_calls"):
        return message
    return {"role": "assistant", "content": resp.text or None, "tool_calls": resp.tool_calls}
