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
import uuid
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, Iterable, List, Optional

from ctxmesh._approval import approval_scope
from ctxmesh._capability import capability_scope
from ctxmesh.client import Client
from ctxmesh.errors import ApprovalRequiredError, ConfigError, ConsentRequiredError

#: A sane default bound: enough for a few tool round-trips, low enough that a
#: runaway (a model that keeps calling tools) trips it quickly. Overridable via
#: config (the image reads MAX_STEPS).
DEFAULT_MAX_STEPS = 8

#: The inbound header that scopes a run to a conversation thread — the same
#: X-Conversation-Id convention the launcher (cmd/launcher) and the memory/gateway/
#: A2A paths use. When present AND the agent is bound to memory, the loop replays the
#: recent turns so the stock agent is context-aware across a chat.
CONVERSATION_HEADER = "X-Conversation-Id"

#: X-Message-Id — the per-hop message id (ADR 0035, m33.4). The launcher sets it on an A2A-invoked
#: /invoke from the envelope; the loop relays it to memory writes so entries attribute to THIS hop.
MESSAGE_HEADER = "X-Message-Id"

#: The most recent conversation messages the loop replays as context on each turn.
#: Bounds the prompt so a long chat can't grow the context without limit — older turns
#: fall out of the window (the memory plane still retains the full history).
MAX_HISTORY_MESSAGES = 40


def _conversation_id_from_headers(headers: Optional[Dict[str, str]]) -> str:
    """Pull the conversation id out of inbound *headers* case-insensitively (HTTP header
    case is not guaranteed), returning "" when absent or blank."""
    return _header_value(headers, CONVERSATION_HEADER)


def _message_id_from_headers(headers: Optional[Dict[str, str]]) -> str:
    """Pull the per-hop message id (X-Message-Id, m33.4) out of inbound headers, "" when absent."""
    return _header_value(headers, MESSAGE_HEADER)


def mint_conversation_id() -> str:
    """Mint a fresh per-run conversation id for an autonomous run with no inbound session (m33.5,
    ADR 0035) — the run id doubles as the thread id, so each execution is its own thread/trace. A
    scheduled agent that must CONTINUE one long-lived thread supplies its own stable id instead (the
    ``conversation_id`` arg of :func:`run_managed_loop`)."""
    return "run-" + uuid.uuid4().hex


def _header_value(headers: Optional[Dict[str, str]], name: str) -> str:
    """Case-insensitive header lookup (HTTP header case is not guaranteed); "" when absent/blank."""
    if not headers:
        return ""
    target = name.lower()
    for key, value in headers.items():
        if key.lower() == target:
            return (value or "").strip()
    return ""


def _load_history(
    client: Client, conversation_id: str, max_messages: int = MAX_HISTORY_MESSAGES
) -> List[Dict[str, Any]]:
    """Return the recent ``{role, content}`` turns stored for this conversation, bounded to the last
    *max_messages* (the m33.6 window). Only well-formed user/assistant message dicts are replayed;
    any other JSON in the store is ignored (the memory plane is a general log)."""
    history: List[Dict[str, Any]] = []
    for entry in client.memory.get(conversation_id):
        if (
            isinstance(entry, dict)
            and entry.get("role") in ("user", "assistant")
            and isinstance(entry.get("content"), str)
        ):
            history.append({"role": entry["role"], "content": entry["content"]})
    # A non-positive bound would slice to empty/"all"; clamp to at least 1 turn of context.
    window = max_messages if max_messages > 0 else MAX_HISTORY_MESSAGES
    return history[-window:]


def _persist_turn(
    client: Client, conversation_id: str, user_input: str, answer: str, message_id: str = ""
) -> None:
    """Append this turn's user message and the assistant's final answer to the conversation
    so the next turn replays them. Intermediate tool-call scratchpad messages are NOT stored
    — only the clean user↔assistant exchange, which is what a later turn should see.

    message_id (m33.4) attributes both entries to the inbound A2A hop when this turn was reached
    via A2A, so the shared/private log records which hop each message belongs to."""
    mid = message_id or None
    client.memory.append({"role": "user", "content": user_input}, conversation_id, message_id=mid)
    client.memory.append({"role": "assistant", "content": answer}, conversation_id, message_id=mid)


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

    #: Bounded replay window (m33.6): the max number of recent conversation messages replayed as
    #: context on each turn, so a long chat can't grow the prompt without limit — older turns fall
    #: out of the window (the memory plane still retains the full history, itself capped + TTL'd on
    #: the store side). Defaults to :data:`MAX_HISTORY_MESSAGES`.
    max_history_messages: int = MAX_HISTORY_MESSAGES


@dataclass
class ManagedResult:
    """The outcome of a managed run: the final text + how it got there."""

    #: The final assistant completion once the model stopped calling tools.
    output: str
    #: The number of model turns taken (1 = answered without any tool call).
    steps: int
    #: The catalog names of the tools dispatched, in call order.
    tools_called: List[str]
    #: MCP servers a tool call hit that the invoking user has not connected an account to
    #: (ADR 0029 §2 / m25.9). Non-empty ⇒ the run needs a "Connect your account" CTA; the
    #: model was told to report + stop rather than retry.
    consent_required: List[str] = field(default_factory=list)
    #: When a step called ``pause_for_approval`` for a not-yet-approved key (human-in-the-loop,
    #: m32.4), ``{"key": ..., "summary": ...}`` describing what needs approving. ``None`` ⇒ the run
    #: did not pause for approval. Non-None ⇒ the BFF surfaces a ``requires_action`` (approval).
    approval_required: Optional[Dict[str, str]] = None


#: The permissive parameters schema advertised when a tool has no discovered
#: inputSchema (curated/legacy entries, the m14.2 echo mock). It accepts any
#: object so the gateway can relay ``tools`` and the model can at least name the
#: tool; the concrete arguments pass through to ``tools.call`` verbatim.
_PERMISSIVE_PARAMETERS: Dict[str, Any] = {
    "type": "object",
    "properties": {},
    "additionalProperties": True,
}


def _tool_schema(tool: Any) -> Dict[str, Any]:
    """Build an OpenAI ``tools[]`` function schema for a discovered tool.

    *tool* is a :class:`ctxmesh.tools.Tool` from ``tools.list()``. When the
    discovery manifest carried the tool's ``inputSchema`` (m14.6 stored it on the
    ToolRegistry entry; m14.6b plumbs it through), it is used VERBATIM as the
    OpenAI function ``parameters`` — so a real model sees the tool's exact
    argument schema and produces correct ``arguments``. When the manifest omits
    it (a curated/legacy entry or the schema-less echo mock), the loop falls back
    to :data:`_PERMISSIVE_PARAMETERS` — enough for the gateway to relay ``tools``
    and for the model to name the tool it wants.
    """
    schema = getattr(tool, "input_schema", None)
    parameters = schema if isinstance(schema, dict) and schema else _PERMISSIVE_PARAMETERS
    return {
        "type": "function",
        "function": {
            "name": tool.name,
            "description": f"The {tool.name} tool bound to this agent.",
            "parameters": parameters,
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


def _stream_turn(
    client: Client,
    route: str,
    messages: List[Dict[str, Any]],
    chat_opts: Dict[str, Any],
    on_token: Callable[[str], None],
) -> Any:
    """Stream one model turn: push each content delta to *on_token*, and return the assembled
    ChatResponse (content + tool_calls) so the loop dispatches tools / detects the final answer
    exactly as the non-streaming path (m32.7)."""
    gen = client.model.stream_completion(route, messages, **chat_opts)
    try:
        while True:
            on_token(next(gen))
    except StopIteration as done:
        return done.value


def run_managed_loop(
    client: Client,
    config: ManagedConfig,
    user_input: str,
    *,
    headers: Optional[Dict[str, str]] = None,
    on_token: Optional[Callable[[str], None]] = None,
    approvals: Optional[Iterable[str]] = None,
    conversation_id: Optional[str] = None,
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
    tool_schemas = [_tool_schema(t) for t in tools]

    # Conversation threading (m29.6): when the caller supplied a conversation id — the
    # console chat sends one stable id per session via X-Conversation-Id — AND this agent
    # is bound to memory, replay the recent turns so the stock loop is context-aware across
    # the chat. No id, or no memory binding ⇒ a single-shot run: messages are just
    # [system, user] and nothing is persisted (today's Playground behaviour, unchanged).
    # Conversation id resolution (m33.5, ADR 0035). Precedence: the inbound session id (a console
    # chat's X-Conversation-Id) > an agent-supplied conversation_id — the STABLE-key opt-in for a
    # scheduled/autonomous agent that continues one long-lived thread across runs. When neither is
    # present the loop is single-shot (unchanged). The "mint a fresh per-run id for an autonomous
    # run" default is applied at the deployment boundary (the managed-agent entrypoint), which
    # passes a minted id here — keeping this library call free of ambient I/O.
    conversation_id = _conversation_id_from_headers(headers) or conversation_id or ""
    # Per-hop message id (m33.4): when this turn was reached via A2A, the launcher stamped the hop's
    # messageId onto the inbound headers; relay it so persisted turns attribute to this hop.
    message_id = _message_id_from_headers(headers)
    threaded = bool(conversation_id) and client.config.memory_wired
    history = (
        _load_history(client, conversation_id, config.max_history_messages) if threaded else []
    )

    messages: List[Dict[str, Any]] = [
        {"role": "system", "content": config.system_prompt},
        *history,
        {"role": "user", "content": user_input},
    ]
    tools_called: List[str] = []
    consent_required: List[str] = []

    # Bind the invoking user's run capability (ADR 0030 §3) from the inbound headers for
    # the whole turn, so every MCP tool call this loop dispatches relays it to the egress
    # sidecar. approval_scope binds the human-in-the-loop approvals GRANTED for this run
    # (m32.4) — on a resume the re-invoke carries the approved key so pause_for_approval
    # proceeds. Both are request-scoped ContextVars — no cross-user bleed between runs.
    step = 0
    with capability_scope(headers), approval_scope(approvals), client.trace.loop(
        "managed-agent", headers=headers
    ) as root:
        root.set_input(user_input)

        try:
            return _drive_loop(
                client, config, root, messages, tool_schemas, tool_names,
                tools_called, consent_required, on_token, conversation_id, threaded, user_input,
                message_id,
            )
        except ApprovalRequiredError as exc:
            # A step gated on human approval (pause_for_approval). Surface it as a
            # requires_action (approval) OUTCOME — the console renders approve/deny and the
            # run resumes with the key granted — rather than crashing the run.
            root.set_output(f"approval required: {exc.summary}")
            return ManagedResult(
                output=f"Awaiting approval: {exc.summary}",
                steps=step,
                tools_called=tools_called,
                consent_required=consent_required,
                approval_required={"key": exc.key, "summary": exc.summary},
            )


def _drive_loop(
    client: Client,
    config: ManagedConfig,
    root: Any,
    messages: List[Dict[str, Any]],
    tool_schemas: List[Dict[str, Any]],
    tool_names: set,
    tools_called: List[str],
    consent_required: List[str],
    on_token: Optional[Callable[[str], None]],
    conversation_id: str,
    threaded: bool,
    user_input: str,
    message_id: str = "",
) -> ManagedResult:
    """The tool-calling loop body (extracted so run_managed_loop can wrap it in the
    capability/approval scopes + catch ApprovalRequiredError as a requires_action outcome)."""
    for step in range(1, config.max_steps + 1):
        with client.trace.step(f"turn-{step}") as turn:
            # model.chat emits its own LLM span nested under this step.
            chat_opts: Dict[str, Any] = dict(config.model_opts)
            if tool_schemas:
                chat_opts["tools"] = tool_schemas
            # When a token sink is wired (the streaming /invoke, m32.7), stream this turn:
            # push content deltas to on_token as they arrive, then take the assembled response
            # (with any tool_calls) to drive the loop exactly as the non-streaming path does.
            if on_token is not None:
                resp = _stream_turn(client, config.model_route, messages, chat_opts, on_token)
            else:
                resp = client.model.chat(config.model_route, messages, **chat_opts)

            if not resp.has_tool_calls:
                # The model stopped calling tools → this is the final answer.
                turn.set_output(resp.text)
                root.set_output(resp.text)
                # Persist the clean user↔assistant exchange so the next turn in this
                # conversation replays it. Only on a completed answer (an error/runaway
                # path stores nothing — there is no answer to thread).
                if threaded:
                    _persist_turn(client, conversation_id, user_input, resp.text, message_id)
                return ManagedResult(
                    output=resp.text,
                    steps=step,
                    tools_called=tools_called,
                    consent_required=consent_required,
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
                    try:
                        with client.trace.tool(name, input=args) as tool_span:
                            result = client.tools.call(name, **args)
                            tool_span.set_output(result)
                        content = _tool_result_content(result)
                        tools_called.append(name)
                    except ConsentRequiredError as exc:
                        # The invoking user has not connected their account to this MCP
                        # server (ADR 0029 §2). Record it for the run's "Connect your
                        # account" CTA and tell the model to report + stop, not retry.
                        if exc.server and exc.server not in consent_required:
                            consent_required.append(exc.server)
                        content = (
                            f"consent_required: the user must connect their account for "
                            f"the {exc.server!r} MCP server before this tool can run. "
                            f"Report this to the user and stop — do not retry."
                        )

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
