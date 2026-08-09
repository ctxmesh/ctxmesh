"""Managed-loop contract tests (M14, ADR 0013).

These drive :func:`ctxmesh.run_managed_loop` against:

  * a **two-turn tool-call gateway stub** that reproduces the harness m14.2
    fixture (``harness/mock-provider/tool-call-mock.py``) turn shapes — turn 1
    returns an assistant ``tool_calls`` message with ``content: null`` (function
    ``echo_tool``); turn 2 (once a ``role: "tool"`` result is present) returns a
    final ``MOCK_TOOL_OK`` completion, and
  * a **discovery/MCP stub** advertising a matching ``echo_tool``.

The fixture constants (``TOOL_NAME``, ``TOOL_CALL_ID``, ``TOOL_ARGS``,
``FINAL_MARKER``) are MIRRORED here from the harness module so the assertions
track the same literals the round-trip proof asserts (the SDK repo cannot import
across into the brain harness). If those literals ever change in
``tool-call-mock.py``, this test's mirror must change with them.

Coverage:
  * ``ChatResponse.tool_calls`` parses the m14.2 turn-1 shape (content:null).
  * ``run_managed_loop`` drives the full two-turn sequence: it dispatches
    ``echo_tool`` via the discovery/MCP plane (:2999) and returns ``MOCK_TOOL_OK``.
  * the max-steps guard trips on a model that never stops calling tools.
  * the trace carries the step → tool → model span tree.
"""

from __future__ import annotations

import json
from typing import Any, Dict, List

import pytest
from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
    InMemorySpanExporter,
)

from ctxmesh import ManagedConfig, agent, mint_conversation_id, run_managed_loop
from ctxmesh.config import PlaneConfig, RunContext
from ctxmesh.errors import ConfigError
from ctxmesh.managed import _PERMISSIVE_PARAMETERS, _tool_schema
from ctxmesh.tools import Tool

from .launcher_stub import DiscoveryStub, MemoryStub, RecordedRequest, _BaseStub, _StubState

# ── Mirror of the harness m14.2 fixture contract (tool-call-mock.py) ───────────
# Kept in lockstep with harness/mock-provider/tool-call-mock.py module constants.
TOOL_NAME = "echo_tool"
TOOL_ARGS = {"text": "ping"}
TOOL_CALL_ID = "call_mock_0"
FINAL_MARKER = "MOCK_TOOL_OK"
USAGE = {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}


def _has_tool_result(messages: List[Dict[str, Any]]) -> bool:
    """True once the client has sent a tool result (mirrors the fixture logic)."""
    return any(isinstance(m, dict) and m.get("role") == "tool" for m in messages)


def _tool_result_text(messages: List[Dict[str, Any]]) -> str:
    for m in reversed(messages):
        if isinstance(m, dict) and m.get("role") == "tool":
            content = m.get("content")
            return content if isinstance(content, str) else json.dumps(content)
    return ""


def _turn1_tool_call_body() -> Dict[str, Any]:
    """Turn 1: the m14.2 assistant tool-call message (content:null)."""
    return {
        "id": "chatcmpl-mock-tool",
        "object": "chat.completion",
        "model": "tool-call-mock",
        "choices": [
            {
                "index": 0,
                "finish_reason": "tool_calls",
                "message": {
                    "role": "assistant",
                    "content": None,
                    "tool_calls": [
                        {
                            "id": TOOL_CALL_ID,
                            "type": "function",
                            "function": {
                                "name": TOOL_NAME,
                                "arguments": json.dumps(TOOL_ARGS),
                            },
                        }
                    ],
                },
            }
        ],
        "usage": USAGE,
    }


def _turn2_final_body(messages: List[Dict[str, Any]]) -> Dict[str, Any]:
    """Turn 2: the m14.2 final completion embedding the tool result."""
    result = _tool_result_text(messages)
    text = f"{FINAL_MARKER} the tool returned: {result}".rstrip()
    return {
        "id": "chatcmpl-mock-final",
        "object": "chat.completion",
        "model": "tool-call-mock",
        "choices": [
            {
                "index": 0,
                "finish_reason": "stop",
                "message": {"role": "assistant", "content": text},
            }
        ],
        "usage": USAGE,
    }


class ToolCallGatewayStub(_BaseStub):
    """A model gateway that reproduces the m14.2 two-turn tool-call contract.

    Stateless like the harness mock: the turn is decided purely from the request
    ``messages`` — a ``role: "tool"`` result present → the final answer, else →
    the tool call. ``always_tool_call=True`` makes it ALWAYS return a tool call
    (never a final answer), to exercise the loop's max-steps runaway guard.
    """

    def __init__(self, *, always_tool_call: bool = False) -> None:
        self.always_tool_call = always_tool_call
        super().__init__()

    def _install_routes(self) -> None:
        def completions(state: _StubState, req: RecordedRequest):
            body = json.loads(req.body) if req.body else {}
            messages = body.get("messages") or []
            if self.always_tool_call or not _has_tool_result(messages):
                resp = _turn1_tool_call_body()
            else:
                resp = _turn2_final_body(messages)
            return 200, {"Content-Type": "application/json"}, json.dumps(resp).encode()

        self.state.routes.update({"POST /chat/completions": completions})


@pytest.fixture
def tool_gateway():
    with ToolCallGatewayStub() as stub:
        yield stub


@pytest.fixture
def runaway_gateway():
    with ToolCallGatewayStub(always_tool_call=True) as stub:
        yield stub


class EchoDiscoveryStub(DiscoveryStub):
    """A DiscoveryStub whose tool is the m14.2 ``echo_tool`` (matching the mock)."""

    CATALOG_NAME = TOOL_NAME
    MCP_TOOL_NAME = TOOL_NAME

    def __init__(self) -> None:
        # The echo tool returns its input text, like the harness echo server.
        super().__init__(tool_result={"echo": TOOL_ARGS["text"]})


@pytest.fixture
def echo_discovery():
    with EchoDiscoveryStub() as stub:
        yield stub


def _plane(tool_gateway, echo_discovery) -> PlaneConfig:
    return PlaneConfig.for_test(
        discovery_base_url=echo_discovery.base_url,
        model_gateway_url=tool_gateway.base_url,
        run=RunContext(agent_name="managed-test"),
    )


# ── ChatResponse.tool_calls parses the m14.2 turn-1 shape ──────────────────────


def test_chat_response_parses_m14_2_tool_calls(tool_gateway):
    """model.chat on the turn-1 shape must NOT raise (the mandatory SDK fix)."""
    cfg = PlaneConfig.for_test(model_gateway_url=tool_gateway.base_url)
    client = agent.from_config(cfg)

    resp = client.model.chat("tool-mock", [{"role": "user", "content": "hi"}])

    # content:null → text is "" (not a raise), tool_calls carries the intent.
    assert resp.text == ""
    assert resp.has_tool_calls is True
    calls = resp.tool_calls
    assert len(calls) == 1
    assert calls[0]["id"] == TOOL_CALL_ID
    assert calls[0]["function"]["name"] == TOOL_NAME
    assert json.loads(calls[0]["function"]["arguments"]) == TOOL_ARGS
    # The raw assistant message is available for appending to history.
    assert resp.message.get("tool_calls")


def test_chat_response_no_tool_calls_on_plain_completion(gateway_stub):
    """A plain text completion has no tool_calls (and does not raise)."""
    cfg = PlaneConfig.for_test(model_gateway_url=gateway_stub.base_url)
    client = agent.from_config(cfg)
    resp = client.model.chat("m", [{"role": "user", "content": "q"}])
    assert resp.text == "the answer is 42"
    assert resp.has_tool_calls is False
    assert resp.tool_calls == []


# ── run_managed_loop drives the full two-turn fixture ──────────────────────────


def test_managed_loop_dispatches_tool_and_returns_final(tool_gateway, echo_discovery):
    client = agent.from_config(_plane(tool_gateway, echo_discovery))
    config = ManagedConfig(
        system_prompt="You are a helpful assistant.",
        model_route="tool-mock",
    )

    result = run_managed_loop(client, config, "please echo ping")

    # Two model turns: the tool call, then the final answer.
    assert result.steps == 2
    assert result.tools_called == [TOOL_NAME]
    # The final completion is the m14.2 marker.
    assert result.output.startswith(FINAL_MARKER)

    # The gateway saw the follow-up turn carry a role:"tool" result with the
    # matching tool_call_id (OpenAI's contract for tool results).
    last_req = tool_gateway.requests[-1]
    follow_up = json.loads(last_req.body)
    tool_msgs = [m for m in follow_up["messages"] if m.get("role") == "tool"]
    assert len(tool_msgs) == 1
    assert tool_msgs[0]["tool_call_id"] == TOOL_CALL_ID
    # The assistant tool-call message precedes the tool result (OpenAI ordering).
    roles = [m["role"] for m in follow_up["messages"]]
    assert roles == ["system", "user", "assistant", "tool"]

    # The system prompt from config is turn 1's system message (config→behavior).
    first_req = json.loads(tool_gateway.requests[0].body)
    assert first_req["messages"][0] == {
        "role": "system",
        "content": "You are a helpful assistant.",
    }
    # Turn 1 advertised the bound tool's schema to the gateway (tools passthrough).
    advertised = [t["function"]["name"] for t in first_req["tools"]]
    assert advertised == [TOOL_NAME]

    # The tool was actually dispatched over the MCP plane (:2999).
    assert len(echo_discovery.mcp_calls) == 1
    assert echo_discovery.mcp_calls[0]["name"] == TOOL_NAME
    assert echo_discovery.mcp_calls[0]["arguments"] == TOOL_ARGS


def test_managed_loop_max_steps_guard_trips_on_runaway(runaway_gateway, echo_discovery):
    """A model that never stops calling tools must hit the bound, not hang."""
    client = agent.from_config(_plane(runaway_gateway, echo_discovery))
    config = ManagedConfig(
        system_prompt="loop forever",
        model_route="tool-mock",
        max_steps=3,
    )
    with pytest.raises(ConfigError) as exc:
        run_managed_loop(client, config, "go")
    assert "max_steps=3" in str(exc.value)


def test_managed_loop_surfaces_approval_then_resumes(tool_gateway, echo_discovery, monkeypatch):
    """Human-in-the-loop (m32.4): a step that pauses for approval becomes an approval_required
    OUTCOME (not a crash); re-invoking with the approved key lets the same step proceed to a final
    answer — the resume contract, end to end through the loop."""
    from ctxmesh import pause_for_approval

    client = agent.from_config(_plane(tool_gateway, echo_discovery))
    config = ManagedConfig(system_prompt="You are a helpful assistant.", model_route="tool-mock")

    # Model the bound tool as a sensitive action gated on approval.
    def gated_call(name, **kwargs):
        pause_for_approval("echo-tool", "Run echo_tool with the given text?")
        return {"content": [{"type": "text", "text": "ok"}]}

    monkeypatch.setattr(client.tools, "call", gated_call)

    # First run: not approved → the loop surfaces requires_action(approval), never runs the tool.
    paused = run_managed_loop(client, config, "please echo ping")
    assert paused.approval_required == {
        "key": "echo-tool",
        "summary": "Run echo_tool with the given text?",
    }
    assert paused.tools_called == [], "the gated tool did not execute before approval"

    # Resume: re-invoke with the approved key → pause_for_approval proceeds, the tool runs, and the
    # loop reaches the mock final answer.
    resumed = run_managed_loop(client, config, "please echo ping", approvals=["echo-tool"])
    assert resumed.approval_required is None
    assert resumed.output.startswith(FINAL_MARKER)


def test_managed_loop_trace_has_step_tool_model_tree(
    tool_gateway, echo_discovery, span_exporter: InMemorySpanExporter
):
    client = agent.from_config(_plane(tool_gateway, echo_discovery))
    config = ManagedConfig(system_prompt="sys", model_route="tool-mock")

    run_managed_loop(client, config, "echo please")

    spans = span_exporter.get_finished_spans()
    kinds = {s.name: s.attributes.get("openinference.span.kind") for s in spans}

    # The AGENT loop root.
    assert kinds.get("managed-agent") == "AGENT"
    # Two CHAIN turns (tool-call turn + final answer turn).
    turn_spans = [s for s in spans if s.name.startswith("turn-")]
    assert len(turn_spans) == 2
    assert all(s.attributes.get("openinference.span.kind") == "CHAIN" for s in turn_spans)
    # A TOOL span for the dispatched echo_tool.
    tool_spans = [s for s in spans if s.attributes.get("openinference.span.kind") == "TOOL"]
    assert len(tool_spans) == 1
    assert tool_spans[0].attributes.get("tool.name") == TOOL_NAME
    # Two LLM spans (one per model.chat).
    llm_spans = [s for s in spans if s.attributes.get("openinference.span.kind") == "LLM"]
    assert len(llm_spans) == 2


def test_managed_loop_no_tools_bound_plain_chat(gateway_stub):
    """With no tools discovered, the loop is a plain one-turn chat agent."""

    # A discovery stub that advertises NO tools.
    class EmptyDiscovery(_BaseStub):
        def _install_routes(self) -> None:
            def tools(state: _StubState, req: RecordedRequest):
                body = json.dumps({"version": "v0", "tools": []}).encode()
                return 200, {"Content-Type": "application/json"}, body

            self.state.routes.update({"GET /tools": tools})

    with EmptyDiscovery() as disc:
        plane = PlaneConfig.for_test(
            discovery_base_url=disc.base_url,
            model_gateway_url=gateway_stub.base_url,
        )
        client = agent.from_config(plane)
        config = ManagedConfig(system_prompt="sys", model_route="m")
        result = run_managed_loop(client, config, "hello")

    assert result.steps == 1
    assert result.tools_called == []
    assert result.output == "the answer is 42"
    # No tools advertised → no `tools` field sent to the gateway.
    first_req = json.loads(gateway_stub.requests[0].body)
    assert "tools" not in first_req


# ── conversation threading (m29.6): the stock loop replays memory across turns ──


class _EmptyDiscovery(_BaseStub):
    """A discovery stub advertising NO tools — the loop is then a plain chat agent."""

    def _install_routes(self) -> None:
        def tools(state: _StubState, req: RecordedRequest):
            body = json.dumps({"version": "v0", "tools": []}).encode()
            return 200, {"Content-Type": "application/json"}, body

        self.state.routes.update({"GET /tools": tools})


def test_managed_loop_threads_conversation_memory(gateway_stub):
    """With a conversation id (X-Conversation-Id) AND memory wired, the loop persists the
    turn and replays it on the next turn — the stock agent is context-aware across a chat."""
    with MemoryStub() as mem, _EmptyDiscovery() as disc:
        plane = PlaneConfig.for_test(
            memory_base_url=mem.base_url,
            discovery_base_url=disc.base_url,
            model_gateway_url=gateway_stub.base_url,
        )
        client = agent.from_config(plane)
        config = ManagedConfig(system_prompt="sys", model_route="m")
        headers = {"X-Conversation-Id": "chat-1"}

        # Turn 1: no prior history → messages are [system, user]; the turn is persisted.
        run_managed_loop(client, config, "my name is Zed", headers=headers)
        turn1 = json.loads(gateway_stub.requests[0].body)["messages"]
        assert [m["role"] for m in turn1] == ["system", "user"]
        assert mem.store["chat-1"] == [
            {"role": "user", "content": "my name is Zed"},
            {"role": "assistant", "content": "the answer is 42"},
        ]

        # Turn 2: the prior user+assistant exchange is replayed BEFORE the new user turn.
        run_managed_loop(client, config, "what is my name", headers=headers)
        turn2 = json.loads(gateway_stub.requests[-1].body)["messages"]
        assert turn2 == [
            {"role": "system", "content": "sys"},
            {"role": "user", "content": "my name is Zed"},
            {"role": "assistant", "content": "the answer is 42"},
            {"role": "user", "content": "what is my name"},
        ]
        # Both turns are now stored (4 messages).
        assert len(mem.store["chat-1"]) == 4


def test_managed_loop_without_conversation_id_is_stateless(gateway_stub):
    """No conversation id ⇒ single-shot: memory is never touched and only [system, user]
    is sent — today's Playground behaviour is unchanged."""
    with MemoryStub() as mem, _EmptyDiscovery() as disc:
        plane = PlaneConfig.for_test(
            memory_base_url=mem.base_url,
            discovery_base_url=disc.base_url,
            model_gateway_url=gateway_stub.base_url,
        )
        client = agent.from_config(plane)
        config = ManagedConfig(system_prompt="sys", model_route="m")

        run_managed_loop(client, config, "hello")  # no headers → no conversation id

        assert mem.store == {}, "no conversation id ⇒ memory untouched"
        msgs = json.loads(gateway_stub.requests[0].body)["messages"]
        assert [m["role"] for m in msgs] == ["system", "user"]


def test_managed_loop_stable_conversation_id_threads_without_a_header(gateway_stub):
    """An autonomous agent with no inbound session supplies a STABLE conversation_id (m33.5) to
    continue ONE long-lived thread across runs — the loop persists + replays under it."""
    with MemoryStub() as mem, _EmptyDiscovery() as disc:
        plane = PlaneConfig.for_test(
            memory_base_url=mem.base_url,
            discovery_base_url=disc.base_url,
            model_gateway_url=gateway_stub.base_url,
        )
        client = agent.from_config(plane)
        config = ManagedConfig(system_prompt="sys", model_route="m")

        # No X-Conversation-Id header — the id comes from the explicit arg.
        run_managed_loop(client, config, "turn one", conversation_id="daily-digest")
        assert mem.store["daily-digest"] == [
            {"role": "user", "content": "turn one"},
            {"role": "assistant", "content": "the answer is 42"},
        ]

        # A second run with the SAME stable id replays the prior turn.
        run_managed_loop(client, config, "turn two", conversation_id="daily-digest")
        turn2 = json.loads(gateway_stub.requests[-1].body)["messages"]
        assert turn2[1:3] == [
            {"role": "user", "content": "turn one"},
            {"role": "assistant", "content": "the answer is 42"},
        ]
        assert len(mem.store["daily-digest"]) == 4


def test_mint_conversation_id_is_unique_and_prefixed():
    """The per-run minter (m33.5) yields a fresh, run-prefixed id each call."""
    a = mint_conversation_id()
    b = mint_conversation_id()
    assert a.startswith("run-") and b.startswith("run-")
    assert a != b, "each autonomous run gets its own thread id"


def test_managed_loop_bounds_replayed_history_window(gateway_stub):
    """The read-side window is bounded + configurable (m33.6): with max_history_messages=2, only the
    2 most-recent stored messages are replayed, even though the store retains the full history."""
    with MemoryStub() as mem, _EmptyDiscovery() as disc:
        # Seed a long history directly in the store.
        mem.store["long"] = [
            {"role": "user", "content": "m1"},
            {"role": "assistant", "content": "a1"},
            {"role": "user", "content": "m2"},
            {"role": "assistant", "content": "a2"},
        ]
        plane = PlaneConfig.for_test(
            memory_base_url=mem.base_url,
            discovery_base_url=disc.base_url,
            model_gateway_url=gateway_stub.base_url,
        )
        client = agent.from_config(plane)
        config = ManagedConfig(system_prompt="sys", model_route="m", max_history_messages=2)

        run_managed_loop(client, config, "now", conversation_id="long")

        msgs = json.loads(gateway_stub.requests[-1].body)["messages"]
        # system + only the LAST 2 history messages + the new user turn (older m1/a1 dropped).
        assert msgs == [
            {"role": "system", "content": "sys"},
            {"role": "user", "content": "m2"},
            {"role": "assistant", "content": "a2"},
            {"role": "user", "content": "now"},
        ]


# ── _tool_schema: discovered inputSchema verbatim, else permissive fallback ─────

# A schema requiring a real parameter — the whole point of m14.6b: the model must
# see THIS as the tool's `parameters`, not a permissive empty object.
SCHEMA_BEARING = {
    "type": "object",
    "properties": {"text": {"type": "string", "description": "text to echo"}},
    "required": ["text"],
    "additionalProperties": False,
}


def test_tool_schema_uses_discovered_input_schema_verbatim():
    """When the discovered Tool carries an inputSchema, it becomes `parameters` verbatim."""
    tool = Tool(
        name="echo_tool",
        mode="remote",
        endpoint="http://x/mcp",
        transport="streamable-http",
        input_schema=SCHEMA_BEARING,
    )
    fn = _tool_schema(tool)["function"]
    assert fn["name"] == "echo_tool"
    # The exact discovered schema, not a re-derived/permissive one.
    assert fn["parameters"] == SCHEMA_BEARING
    assert fn["parameters"]["required"] == ["text"]


def test_tool_schema_advertises_real_description():
    """FUNC-10: a Tool carrying a description advertises it as the model function
    `description`; absent, the loop synthesises a generic name-derived one (never empty)."""
    described = Tool(
        name="word_count",
        mode="remote",
        endpoint="http://x/mcp",
        transport="streamable-http",
        description="Count whitespace-separated words.",
    )
    assert _tool_schema(described)["function"]["description"] == "Count whitespace-separated words."

    plain = Tool(
        name="word_count", mode="remote", endpoint="http://x/mcp", transport="streamable-http"
    )
    desc = _tool_schema(plain)["function"]["description"]
    assert desc == "The word_count tool bound to this agent."  # generic fallback, not empty


def test_tool_schema_falls_back_to_permissive_when_absent():
    """A Tool without an inputSchema → the permissive empty-object fallback."""
    tool = Tool(
        name="echo_tool", mode="remote", endpoint="http://x/mcp", transport="streamable-http"
    )
    fn = _tool_schema(tool)["function"]
    assert fn["parameters"] == _PERMISSIVE_PARAMETERS
    # Empty/degenerate schemas also take the fallback (never advertise an
    # unusable empty-properties-required schema as-is).
    empty_schema_tool = Tool(
        name="echo_tool",
        mode="remote",
        endpoint="http://x/mcp",
        transport="streamable-http",
        input_schema={},
    )
    assert _tool_schema(empty_schema_tool)["function"]["parameters"] == _PERMISSIVE_PARAMETERS


# ── end-to-end: a schema-bearing tool rides the model's tools[].parameters ─────


class SchemaEchoDiscoveryStub(EchoDiscoveryStub):
    """The echo tool, but the discovery manifest now carries a real inputSchema.

    Proves the m14.6b chain end-to-end at the SDK boundary: the schema stored on
    the manifest (what the controller renders from the ToolRegistry entry) rides
    through ``tools.list()`` → ``_tool_schema`` → the model.chat request's
    ``tools[].function.parameters``, verbatim.
    """

    def __init__(self) -> None:
        super().__init__()
        self.manifest_input_schema = SCHEMA_BEARING


@pytest.fixture
def schema_echo_discovery():
    with SchemaEchoDiscoveryStub() as stub:
        yield stub


def test_managed_loop_advertises_schema_bearing_tool_parameters(
    tool_gateway, schema_echo_discovery
):
    """The discovered inputSchema is the tool's `parameters` in the chat request."""
    client = agent.from_config(_plane(tool_gateway, schema_echo_discovery))
    config = ManagedConfig(system_prompt="sys", model_route="tool-mock")

    result = run_managed_loop(client, config, "please echo ping")

    # The loop still completes the two-turn round-trip and dispatches the tool.
    assert result.steps == 2
    assert result.tools_called == [TOOL_NAME]
    assert result.output.startswith(FINAL_MARKER)

    # The load-bearing assertion: turn 1's request advertised the REAL schema as
    # the tool's parameters — the model gets exact parameters, not permissive.
    first_req = json.loads(tool_gateway.requests[0].body)
    advertised = first_req["tools"]
    assert len(advertised) == 1
    fn = advertised[0]["function"]
    assert fn["name"] == TOOL_NAME
    assert fn["parameters"] == SCHEMA_BEARING
    # Concretely: a required param the permissive fallback would never carry.
    assert fn["parameters"]["required"] == ["text"]
    assert fn["parameters"]["additionalProperties"] is False


def test_managed_loop_echo_fallback_still_permissive(tool_gateway, echo_discovery):
    """The schema-less echo mock still works — turn 1 advertises the permissive schema."""
    client = agent.from_config(_plane(tool_gateway, echo_discovery))
    config = ManagedConfig(system_prompt="sys", model_route="tool-mock")

    result = run_managed_loop(client, config, "please echo ping")
    assert result.output.startswith(FINAL_MARKER)

    # No inputSchema on the manifest → the permissive fallback rides the request.
    first_req = json.loads(tool_gateway.requests[0].body)
    fn = first_req["tools"][0]["function"]
    assert fn["name"] == TOOL_NAME
    assert fn["parameters"] == _PERMISSIVE_PARAMETERS


# ── m32.7: the managed loop streams the answer via on_token ────────────────────

from ctxmesh.model import ChatResponse  # noqa: E402


def test_managed_loop_streams_the_answer(monkeypatch):
    """With on_token wired, the loop streams the final answer's content deltas AND returns the
    assembled result — the streaming /invoke's token source (m32.7)."""
    with _EmptyDiscovery() as disc:
        plane = PlaneConfig.for_test(
            discovery_base_url=disc.base_url, model_gateway_url="http://gw"
        )
        client = agent.from_config(plane)

        def fake_stream(route, messages, **opts):
            for tok in ["Hel", "lo", " there"]:
                yield tok
            return ChatResponse(
                text="Hello there",
                usage={},
                model=route,
                raw={"choices": [{"message": {"role": "assistant", "content": "Hello there"}}]},
            )

        monkeypatch.setattr(client.model, "stream_completion", fake_stream)

        got: list = []
        config = ManagedConfig(system_prompt="sys", model_route="m")
        result = run_managed_loop(client, config, "hi", on_token=got.append)

        assert got == ["Hel", "lo", " there"], "each content delta streamed to on_token"
        assert result.output == "Hello there"
        assert result.steps == 1
        assert result.tools_called == []


# ── m66.14: guarded agents downgrade to buffered chat (ADR 0059 §4) ────────────


def test_guarded_agent_uses_buffered_even_with_on_token(monkeypatch):
    """When GUARDRAIL_POLICY is set AND on_token is provided, the loop must call
    client.model.chat (buffered) and must NOT call stream_completion.

    This prevents the 422 guardrail_streaming_unsupported that the proxy returns
    for stream:true requests when guardrails are active (m66.6, ADR 0059 §4).
    """
    monkeypatch.setenv("GUARDRAIL_POLICY", "default")
    with _EmptyDiscovery() as disc:
        plane = PlaneConfig.for_test(
            discovery_base_url=disc.base_url, model_gateway_url="http://gw"
        )
        client = agent.from_config(plane)

        chat_called = []

        def fake_chat(route, messages, **opts):
            chat_called.append(True)
            return ChatResponse(
                text="buffered answer",
                usage={},
                model=route,
                raw={"choices": [{"message": {"role": "assistant", "content": "buffered answer"}}]},
            )

        stream_called = []

        def fake_stream(route, messages, **opts):
            stream_called.append(True)
            # should never be reached when guarded
            raise AssertionError("stream_completion must not be called when guarded")
            yield  # makes this a generator so the type matches stream_completion's signature

        monkeypatch.setattr(client.model, "chat", fake_chat)
        monkeypatch.setattr(client.model, "stream_completion", fake_stream)

        got: list = []
        config = ManagedConfig(system_prompt="sys", model_route="m")
        result = run_managed_loop(client, config, "hi", on_token=got.append)

        assert stream_called == [], "stream_completion must not be called when guarded"
        assert chat_called == [True], "buffered chat must be called when guarded"
        assert result.output == "buffered answer"
        assert got == [], "no token deltas when guarded — message arrives as one block"


def test_unguarded_agent_streams_when_on_token_provided(monkeypatch):
    """Without GUARDRAIL_POLICY, the loop streams as before (m32.7 regression guard).

    This confirms the unguarded path is unchanged: stream_completion IS called
    and on_token receives the deltas.
    """
    monkeypatch.delenv("GUARDRAIL_POLICY", raising=False)
    with _EmptyDiscovery() as disc:
        plane = PlaneConfig.for_test(
            discovery_base_url=disc.base_url, model_gateway_url="http://gw"
        )
        client = agent.from_config(plane)

        def fake_stream(route, messages, **opts):
            for tok in ["un", "guard", "ed"]:
                yield tok
            return ChatResponse(
                text="unguarded",
                usage={},
                model=route,
                raw={"choices": [{"message": {"role": "assistant", "content": "unguarded"}}]},
            )

        stream_spy = []
        original_fake = fake_stream

        def spied_stream(route, messages, **opts):
            stream_spy.append(True)
            return original_fake(route, messages, **opts)

        monkeypatch.setattr(client.model, "stream_completion", spied_stream)

        got: list = []
        config = ManagedConfig(system_prompt="sys", model_route="m")
        result = run_managed_loop(client, config, "hi", on_token=got.append)

        assert stream_spy == [True], "stream_completion must be called when NOT guarded"
        assert got == ["un", "guard", "ed"], "token deltas must flow to on_token when unguarded"
        assert result.output == "unguarded"


def test_guarded_agent_no_on_token_uses_buffered(monkeypatch):
    """When GUARDRAIL_POLICY is set and no on_token is provided, the loop stays
    buffered (unchanged behavior — this is a sanity guard)."""
    monkeypatch.setenv("GUARDRAIL_POLICY", "strict")
    with _EmptyDiscovery() as disc:
        plane = PlaneConfig.for_test(
            discovery_base_url=disc.base_url, model_gateway_url="http://gw"
        )
        client = agent.from_config(plane)

        def fake_chat(route, messages, **opts):
            return ChatResponse(
                text="still buffered",
                usage={},
                model=route,
                raw={
                    "choices": [{"message": {"role": "assistant", "content": "still buffered"}}]
                },
            )

        stream_called = []

        def fake_stream(route, messages, **opts):
            stream_called.append(True)
            raise AssertionError("stream_completion must not be called when guarded")
            yield  # makes this a generator so the type matches stream_completion's signature

        monkeypatch.setattr(client.model, "chat", fake_chat)
        monkeypatch.setattr(client.model, "stream_completion", fake_stream)

        config = ManagedConfig(system_prompt="sys", model_route="m")
        result = run_managed_loop(client, config, "hi")  # no on_token

        assert stream_called == [], "stream_completion must not be called when guarded"
        assert result.output == "still buffered"


# ── delegate_to dispatch in the managed loop (M64, ADR 0057) ───────────────────


class _DelSpan:
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def set_output(self, v):
        self.out = v


class _DelTrace:
    def tool(self, name, input=None):
        return _DelSpan()


class _DelTools:
    def __init__(self, result):
        self._result = result
        self.calls = []

    def delegate(self, sub_agent, task, step, call_id):
        self.calls.append({"sub_agent": sub_agent, "task": task, "step": step, "call_id": call_id})
        return self._result


class _DelClient:
    def __init__(self, result):
        self.trace = _DelTrace()
        self.tools = _DelTools(result)


def test_dispatch_delegate_threads_the_answer():
    """A successful delegation returns the sub-run's answer as the tool result, records the call,
    and forwards the idempotency key (step + call_id)."""
    from ctxmesh.managed import _dispatch_delegate

    client = _DelClient({"ok": True, "answer": "the sub-run answer"})
    tools_called = []
    out = _dispatch_delegate(
        client, {"sub_agent": "researcher", "task": "find it"}, "3", "call-9", tools_called
    )
    assert out == "the sub-run answer"
    assert tools_called == ["delegate_to"]
    assert client.tools.calls == [
        {"sub_agent": "researcher", "task": "find it", "step": "3", "call_id": "call-9"}
    ]


def test_dispatch_delegate_failure_is_tool_text():
    """A guard denial / sub-run failure comes back as text the model can act on, not an error."""
    from ctxmesh.managed import _dispatch_delegate

    client = _DelClient({"ok": False, "error": "spawn_budget_exceeded"})
    tools_called = []
    out = _dispatch_delegate(client, {"sub_agent": "coder", "task": "x"}, "1", "c1", tools_called)
    assert "did not succeed" in out and "spawn_budget_exceeded" in out
    assert tools_called == ["delegate_to"], "even a refused delegation is recorded as attempted"


def test_dispatch_delegate_requires_sub_agent():
    """A delegate_to call missing sub_agent is refused before any spawn."""
    from ctxmesh.managed import _dispatch_delegate

    client = _DelClient({"ok": True, "answer": "unused"})
    out = _dispatch_delegate(client, {"task": "x"}, "1", "c1", [])
    assert "requires a 'sub_agent'" in out
    assert client.tools.calls == [], "no delegation is attempted without a sub_agent"


# ── v1b bounded-parallel fan-out (M64, ADR 0057) ───────────────────────────────


def test_delegate_batch_runs_concurrently_and_propagates_capability():
    """A turn's delegate_to calls run CONCURRENTLY, all results return keyed by call_id, and each
    worker thread sees the invoking user's run capability (OBO propagated via the copied ctx)."""
    import threading

    from ctxmesh._capability import capability_scope, current_capability
    from ctxmesh.managed import _dispatch_delegate_batch

    seen_caps = []
    seen_threads = set()

    class _BatchTools:
        def delegate(self, sub_agent, task, step, call_id):
            seen_caps.append(current_capability())
            seen_threads.add(threading.get_ident())
            return {"ok": True, "answer": f"{sub_agent}:done"}

    class _BatchClient:
        trace = _DelTrace()
        tools = _BatchTools()

    calls = [("c1", "researcher", "t1"), ("c2", "coder", "t2"), ("c3", "writer", "t3")]
    with capability_scope({"X-Ctxmesh-Run-Capability": "cap-token"}):
        results = _dispatch_delegate_batch(_BatchClient(), calls, "1")

    assert results == {"c1": "researcher:done", "c2": "coder:done", "c3": "writer:done"}
    assert (
        seen_caps == ["cap-token"] * 3
    ), "every sub-run acts on-behalf-of the same user (OBO to threads)"
    assert (
        len(seen_threads) >= 2
    ), "the delegations ran on multiple worker threads (concurrent, not serial)"


def test_delegate_batch_single_call_no_pool():
    """A single delegate call takes the direct path (no thread pool) and returns its result."""
    from ctxmesh.managed import _dispatch_delegate_batch

    client = _DelClient({"ok": True, "answer": "solo"})
    out = _dispatch_delegate_batch(client, [("c1", "researcher", "t")], "1")
    assert out == {"c1": "solo"}


# ── handoff_to dispatch + terminal loop (M67, ADR 0060 §5) ─────────────────────


class _HandoffTools:
    def __init__(self, result):
        self._result = result
        self.calls = []

    def handoff(self, target_agent, message=""):
        self.calls.append({"target_agent": target_agent, "message": message})
        return self._result


class _HandoffClient:
    def __init__(self, result):
        self.trace = _DelTrace()
        self.tools = _HandoffTools(result)


def _handoff_call(target_agent="billing", message="please take over"):
    """An OpenAI handoff_to tool-call object (the shape the model emits)."""
    return {
        "id": "call-h1",
        "type": "function",
        "function": {
            "name": "handoff_to",
            "arguments": json.dumps({"target_agent": target_agent, "message": message}),
        },
    }


def test_dispatch_handoff_records_the_transfer():
    """A successful handoff returns a structured transfer result (targetAgent + ok + runId), records
    the call, and forwards the target + message. It is NOT awaited — the outcome is the transfer."""
    from ctxmesh.managed import _dispatch_handoff

    client = _HandoffClient(
        {"ok": True, "runId": "hand-1", "sourceRun": "A-1", "handedOffTo": "billing"}
    )
    out = _dispatch_handoff(client, _handoff_call("billing", "refund"))
    assert out == {"ok": "true", "targetAgent": "billing", "runId": "hand-1", "sourceRun": "A-1"}
    assert client.tools.calls == [{"target_agent": "billing", "message": "refund"}]


def test_dispatch_handoff_refusal_is_recorded():
    """A refused handoff (non-member target / missing cap) comes back as ok=false + the error —
    recorded, never raised (the turn ends on a handoff regardless)."""
    from ctxmesh.managed import _dispatch_handoff

    client = _HandoffClient({"ok": False, "error": "not a member of this team's roster"})
    out = _dispatch_handoff(client, _handoff_call("attacker"))
    assert out["ok"] == "false"
    assert out["targetAgent"] == "attacker"
    assert "roster" in out["error"]


def test_dispatch_handoff_requires_target():
    """A handoff_to call missing target_agent is refused before any transfer."""
    from ctxmesh.managed import _dispatch_handoff

    client = _HandoffClient({"ok": True})
    out = _dispatch_handoff(client, _handoff_call(target_agent=""))
    assert out["ok"] == "false"
    assert "requires a 'target_agent'" in out["error"]
    assert client.tools.calls == [], "no transfer is attempted without a target"


def test_managed_loop_handoff_is_terminal(monkeypatch):
    """A handoff_to tool call ENDS the managed loop with a handoff ManagedResult and NO further
    answer: the loop does not take another model turn, produces no output, and the model is never
    re-asked (a handoff is a transfer — the OPPOSITE of a delegate call, which threads a result)."""
    import ctxmesh.managed as managed
    from ctxmesh import ManagedConfig
    from ctxmesh.model import ChatResponse

    config = ManagedConfig(system_prompt="you are a router", model_route="tool-mock")

    # A client whose tools.list() offers handoff_to and whose handoff() records the transfer.
    handoff_result = {"ok": True, "runId": "hand-9", "sourceRun": "A-9", "handedOffTo": "billing"}

    class _Tool:
        name = "handoff_to"
        input_schema = {"type": "object", "properties": {"target_agent": {"type": "string"}}}
        description = "hand off"

    class _Tools(_HandoffTools):
        def list(self):
            return [_Tool()]

    class _Loop:
        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def set_input(self, v):
            pass

        def set_output(self, v):
            pass

    class _LoopTrace(_DelTrace):
        def loop(self, name, headers=None):
            return _Loop()

        def step(self, name):
            return _Loop()

    class _Cfg:
        memory_wired = False
        longterm_wired = False

    class _Client:
        def __init__(self):
            self.trace = _LoopTrace()
            self.tools = _Tools(handoff_result)
            self.config = _Cfg()

    client = _Client()

    # The model's single turn asks to hand off (content:null + a handoff_to tool call).
    calls = {"n": 0}

    def fake_chat(*args, **kwargs):
        calls["n"] += 1
        raw = {
            "choices": [
                {"message": {"role": "assistant", "content": None, "tool_calls": [_handoff_call()]}}
            ]
        }
        return ChatResponse(text="", usage={}, model="tool-mock", raw=raw)

    monkeypatch.setattr(managed, "_chat_with_resilience", fake_chat)

    result = managed.run_managed_loop(client, config, "I need billing help")

    assert result.handoff is not None, "the loop returns a handoff outcome"
    assert result.handoff["targetAgent"] == "billing"
    assert result.handoff["ok"] == "true"
    assert result.output == "", "a handoff produces NO further answer (the target continues)"
    assert result.tools_called == ["handoff_to"]
    assert calls["n"] == 1, "the loop took exactly ONE model turn — no re-ask after the handoff"


def test_managed_loop_refused_handoff_is_recoverable(monkeypatch):
    """A REFUSED handoff (ok=false — non-member target / launcher down) is NOT terminal: the loop
    does NOT return a handoff marker, threads the refusal back as a tool result, and lets the model
    recover with a normal answer — so the BFF terminates the still-running source run (the fix)."""
    import ctxmesh.managed as managed
    from ctxmesh import ManagedConfig
    from ctxmesh.model import ChatResponse

    config = ManagedConfig(system_prompt="you are a router", model_route="tool-mock")

    class _Tool:
        name = "handoff_to"
        input_schema = {"type": "object", "properties": {"target_agent": {"type": "string"}}}
        description = "hand off"

    class _Tools(_HandoffTools):
        def list(self):
            return [_Tool()]

    class _Loop:
        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def set_input(self, v):
            pass

        def set_output(self, v):
            pass

    class _LoopTrace(_DelTrace):
        def loop(self, name, headers=None):
            return _Loop()

        def step(self, name):
            return _Loop()

    class _Cfg:
        memory_wired = False
        longterm_wired = False

    class _Client:
        def __init__(self):
            self.trace = _LoopTrace()
            # handoff() refuses (non-member target).
            self.tools = _Tools({"ok": False, "error": "not a member of this team's roster"})
            self.config = _Cfg()

    client = _Client()

    # Turn 1: the model tries handoff_to (refused). Turn 2: it answers the user itself.
    calls = {"n": 0}

    def fake_chat(*args, **kwargs):
        calls["n"] += 1
        if calls["n"] == 1:
            raw = {
                "choices": [
                    {"message": {"role": "assistant", "content": None,
                                 "tool_calls": [_handoff_call("attacker")]}}
                ]
            }
            return ChatResponse(text="", usage={}, model="tool-mock", raw=raw)
        answer = "I can help you directly."
        raw = {"choices": [{"message": {"role": "assistant", "content": answer}}]}
        return ChatResponse(text=answer, usage={}, model="tool-mock", raw=raw)

    monkeypatch.setattr(managed, "_chat_with_resilience", fake_chat)

    result = managed.run_managed_loop(client, config, "I need help")

    assert result.handoff is None, "a REFUSED handoff sets NO marker (the run was not transferred)"
    assert result.output == "I can help you directly.", "the model recovered and answered normally"
    assert calls["n"] == 2, "the loop CONTINUED past the refused handoff (it is not terminal)"
    assert "handoff_to" in result.tools_called


# ── m65.5: structured outputs — response_format + in-loop schema repair ─────────


# A simple JSON Schema the tests drive against.
_OUTPUT_SCHEMA = {
    "type": "object",
    "properties": {"answer": {"type": "string"}},
    "required": ["answer"],
    "additionalProperties": False,
}

# A response text that conforms to _OUTPUT_SCHEMA.
_CONFORMING_ANSWER = '{"answer": "hello"}'
# A response text that is valid JSON but violates _OUTPUT_SCHEMA (missing required key).
_NONCONFORMING_ANSWER = '{"wrong_key": 42}'
# A response text that is not JSON at all.
_NOT_JSON = "this is plain text, not JSON"


def _plain_final_body(text: str) -> Dict[str, Any]:
    """A one-turn final-answer gateway response body for the given text."""
    return {
        "id": "chatcmpl-so",
        "object": "chat.completion",
        "model": "test",
        "choices": [
            {
                "index": 0,
                "finish_reason": "stop",
                "message": {"role": "assistant", "content": text},
            }
        ],
        "usage": USAGE,
    }


class _SequentialGatewayStub(_BaseStub):
    """A model gateway stub that returns a fixed sequence of responses, one per POST."""

    def __init__(self, responses: List[str]) -> None:
        self._responses = list(responses)
        self._idx = 0
        super().__init__()

    def _install_routes(self) -> None:
        def completions(state: _StubState, req: RecordedRequest):
            text = self._responses[min(self._idx, len(self._responses) - 1)]
            self._idx += 1
            return (
                200,
                {"Content-Type": "application/json"},
                json.dumps(_plain_final_body(text)).encode(),
            )

        self.state.routes.update({"POST /chat/completions": completions})


class _EmptyDiscoveryStub(_BaseStub):
    """Discovery stub advertising NO tools — the loop is a plain chat agent."""

    def _install_routes(self) -> None:
        def tools(state: _StubState, req: RecordedRequest):
            body = json.dumps({"version": "v0", "tools": []}).encode()
            return 200, {"Content-Type": "application/json"}, body

        self.state.routes.update({"GET /tools": tools})


def _schema_plane(gw_stub) -> PlaneConfig:
    with _EmptyDiscoveryStub() as disc:
        return PlaneConfig.for_test(
            discovery_base_url=disc.base_url,
            model_gateway_url=gw_stub.base_url,
        )


def test_structured_output_conforming_no_repair():
    """output_schema set, the model returns schema-conforming JSON on the first final turn →
    run_managed_loop returns that output with NO repair turn (model called exactly once)."""
    with _SequentialGatewayStub([_CONFORMING_ANSWER]) as gw, _EmptyDiscoveryStub() as disc:
        plane = PlaneConfig.for_test(
            discovery_base_url=disc.base_url, model_gateway_url=gw.base_url
        )
        client = agent.from_config(plane)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            output_schema=_OUTPUT_SCHEMA,
        )

        result = run_managed_loop(client, config, "go")

    assert result.output == _CONFORMING_ANSWER
    # The model was called exactly ONCE — no repair turn.
    assert len(gw.requests) == 1, f"expected 1 model call, got {len(gw.requests)}"
    # response_format was injected into the chat opts on that call.
    body = json.loads(gw.requests[0].body)
    assert "response_format" in body
    assert body["response_format"]["type"] == "json_schema"
    assert body["response_format"]["json_schema"]["schema"] == _OUTPUT_SCHEMA
    assert body["response_format"]["json_schema"]["strict"] is False


def test_structured_output_repair_then_success():
    """output_schema set; model returns non-conforming JSON first, then conforming →
    exactly ONE repair turn happens; the final output is the conforming answer."""
    with _SequentialGatewayStub([_NONCONFORMING_ANSWER, _CONFORMING_ANSWER]) as gw, \
            _EmptyDiscoveryStub() as disc:
        plane = PlaneConfig.for_test(
            discovery_base_url=disc.base_url, model_gateway_url=gw.base_url
        )
        client = agent.from_config(plane)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            output_schema=_OUTPUT_SCHEMA,
            # Give plenty of steps so the repair doesn't hit the max_steps guard.
            max_steps=10,
        )

        result = run_managed_loop(client, config, "go")

    assert result.output == _CONFORMING_ANSWER
    # Two model calls: the original final answer + ONE repair turn.
    assert len(gw.requests) == 2, f"expected 2 model calls, got {len(gw.requests)}"
    # The second call carried a corrective user message.
    second_body = json.loads(gw.requests[1].body)
    messages = second_body["messages"]
    user_msgs = [m for m in messages if m["role"] == "user"]
    # The last user message is the corrective repair prompt.
    last_user = user_msgs[-1]["content"]
    assert "not valid per the required JSON schema" in last_user


def test_structured_output_give_up_after_max_repairs():
    """output_schema set; model ALWAYS returns non-conforming JSON →
    the loop stops after exactly _MAX_SCHEMA_REPAIR repair attempts and returns the last
    (non-conforming) answer without raising."""
    from ctxmesh.managed import _MAX_SCHEMA_REPAIR

    # Always bad — more entries than _MAX_SCHEMA_REPAIR + 1 so the stub never runs out.
    bad_responses = [_NONCONFORMING_ANSWER] * (_MAX_SCHEMA_REPAIR + 5)
    with _SequentialGatewayStub(bad_responses) as gw, _EmptyDiscoveryStub() as disc:
        plane = PlaneConfig.for_test(
            discovery_base_url=disc.base_url, model_gateway_url=gw.base_url
        )
        client = agent.from_config(plane)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            output_schema=_OUTPUT_SCHEMA,
            # Give plenty of steps so we're testing the schema-repair bound, not max_steps.
            max_steps=20,
        )

        result = run_managed_loop(client, config, "go")

    # Must return the last (non-conforming) answer — must NOT raise.
    assert result.output == _NONCONFORMING_ANSWER
    # Exactly 1 (original) + _MAX_SCHEMA_REPAIR (repair turns) calls — no more.
    expected_calls = 1 + _MAX_SCHEMA_REPAIR
    assert len(gw.requests) == expected_calls, (
        f"expected {expected_calls} model calls (1 original + {_MAX_SCHEMA_REPAIR} repairs), "
        f"got {len(gw.requests)}"
    )


def test_structured_output_no_schema_unchanged_behavior():
    """output_schema=None (the default) → no response_format in the chat opts,
    no validation, the loop behaves byte-for-byte as before (regression guard)."""
    with _SequentialGatewayStub([_NOT_JSON]) as gw, _EmptyDiscoveryStub() as disc:
        plane = PlaneConfig.for_test(
            discovery_base_url=disc.base_url, model_gateway_url=gw.base_url
        )
        client = agent.from_config(plane)
        # No output_schema — the default.
        config = ManagedConfig(system_prompt="sys", model_route="m")

        result = run_managed_loop(client, config, "go")

    # The plain-text answer is returned unchanged with exactly one model call.
    assert result.output == _NOT_JSON
    assert len(gw.requests) == 1
    # No response_format was injected.
    body = json.loads(gw.requests[0].body)
    assert "response_format" not in body


def test_validate_against_schema_unit():
    """Unit coverage for _validate_against_schema: conforming → None,
    schema violation → error string, not-JSON → error string."""
    from ctxmesh.managed import _validate_against_schema

    # Conforming.
    assert _validate_against_schema('{"answer": "hi"}', _OUTPUT_SCHEMA) is None
    # Schema violation (missing required key).
    err = _validate_against_schema('{"wrong": 1}', _OUTPUT_SCHEMA)
    assert err is not None and isinstance(err, str)
    # Not JSON.
    err2 = _validate_against_schema("not json at all", _OUTPUT_SCHEMA)
    assert err2 is not None and "not valid JSON" in err2


# ── m65.6: tool-use policy in the managed loop (ADR 0058) ───────────────────────
#
# A configurable gateway that, on any turn WITHOUT a role:"tool" result present,
# returns the given tool_calls; once a tool result appears, it returns a final
# answer. That lets a test drive N tool calls in one turn and then let the loop
# finish. Every request body is recorded (self.requests) so a test can assert the
# `tool_choice` the loop sent. Discovery advertises the same tool names so they
# resolve into tool_names; client.tools.call is monkeypatched to record dispatch.


def _tool_call_obj(call_id: str, name: str, args: Dict[str, Any]) -> Dict[str, Any]:
    return {
        "id": call_id,
        "type": "function",
        "function": {"name": name, "arguments": json.dumps(args)},
    }


class _PolicyGatewayStub(_BaseStub):
    """Returns a fixed list of tool_calls on the first turn, then a final answer."""

    def __init__(self, tool_calls: List[Dict[str, Any]]) -> None:
        self._tool_calls = tool_calls
        super().__init__()

    def _install_routes(self) -> None:
        def completions(state: _StubState, req: RecordedRequest):
            body = json.loads(req.body) if req.body else {}
            messages = body.get("messages") or []
            if _has_tool_result(messages):
                resp = _plain_final_body("POLICY_FINAL done")
            else:
                resp = {
                    "id": "chatcmpl-policy",
                    "object": "chat.completion",
                    "model": "policy-mock",
                    "choices": [
                        {
                            "index": 0,
                            "finish_reason": "tool_calls",
                            "message": {
                                "role": "assistant",
                                "content": None,
                                "tool_calls": self._tool_calls,
                            },
                        }
                    ],
                    "usage": USAGE,
                }
            return 200, {"Content-Type": "application/json"}, json.dumps(resp).encode()

        self.state.routes.update({"POST /chat/completions": completions})


class _MultiToolDiscoveryStub(_BaseStub):
    """Discovery advertising the named tools (permissive schema) so they resolve."""

    def __init__(self, names: List[str]) -> None:
        self._names = names
        super().__init__()

    def _install_routes(self) -> None:
        def tools(state: _StubState, req: RecordedRequest):
            manifest = {"version": "v0", "tools": [{"name": n} for n in self._names]}
            return 200, {"Content-Type": "application/json"}, json.dumps(manifest).encode()

        self.state.routes.update({"GET /tools": tools})


def _policy_client(gw, disc, monkeypatch):
    """Build a client on the given gateway/discovery, recording client.tools.call dispatches."""
    plane = PlaneConfig.for_test(discovery_base_url=disc.base_url, model_gateway_url=gw.base_url)
    client = agent.from_config(plane)
    dispatched: List[str] = []

    def fake_call(name, **kwargs):
        dispatched.append(name)
        return {"content": [{"type": "text", "text": f"{name} ran"}]}

    monkeypatch.setattr(client.tools, "call", fake_call)
    return client, dispatched


def test_tool_policy_deny_not_dispatched_and_model_told(monkeypatch):
    """A tool whose rule is deny (via the default) is never dispatched; the model receives an
    honest denial tool message so it can adapt."""
    calls = [_tool_call_obj("c1", "search", {"q": "x"})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["search"]) as disc:
        client, dispatched = _policy_client(gw, disc, monkeypatch)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={"default": "deny"},
        )
        result = run_managed_loop(client, config, "go")

    # The tool was NOT dispatched, and tools_called does not record it as executed.
    assert dispatched == []
    assert result.tools_called == []
    # The follow-up turn carried the honest denial as the tool result.
    follow_up = json.loads(gw.requests[-1].body)
    tool_msgs = [m for m in follow_up["messages"] if m.get("role") == "tool"]
    assert len(tool_msgs) == 1
    assert "not permitted by policy" in tool_msgs[0]["content"]
    assert tool_msgs[0]["tool_call_id"] == "c1"


def test_tool_policy_deny_via_override(monkeypatch):
    """An override naming the tool wins over an allow default: that one tool is denied."""
    calls = [_tool_call_obj("c1", "danger", {})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["danger"]) as disc:
        client, dispatched = _policy_client(gw, disc, monkeypatch)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={
                "default": "allow",
                "overrides": [{"name": "danger", "rule": "deny"}],
            },
        )
        result = run_managed_loop(client, config, "go")

    assert dispatched == []
    assert result.tools_called == []
    follow_up = json.loads(gw.requests[-1].body)
    tool_msgs = [m for m in follow_up["messages"] if m.get("role") == "tool"]
    assert "not permitted by policy" in tool_msgs[0]["content"]


def test_tool_policy_allow_default_unchanged(monkeypatch):
    """default=allow → the tool dispatches normally (today's behaviour)."""
    calls = [_tool_call_obj("c1", "search", {"q": "x"})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["search"]) as disc:
        client, dispatched = _policy_client(gw, disc, monkeypatch)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={"default": "allow"},
        )
        result = run_managed_loop(client, config, "go")

    assert dispatched == ["search"]
    assert result.tools_called == ["search"]
    assert result.output.startswith("POLICY_FINAL")


def test_tool_policy_require_approval_top_level_not_granted(monkeypatch):
    """require-approval, top-level (no spawn depth), not granted → the loop surfaces
    approval_required with key tool:<name> and the tool never dispatches."""
    calls = [_tool_call_obj("c1", "send_email", {"to": "a@b.c"})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["send_email"]) as disc:
        client, dispatched = _policy_client(gw, disc, monkeypatch)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={"default": "require-approval"},
        )
        result = run_managed_loop(client, config, "go")

    assert result.approval_required is not None
    assert result.approval_required["key"] == "tool:send_email"
    assert "send_email" in result.approval_required["summary"]
    assert dispatched == [], "the gated tool did not execute before approval"
    assert result.tools_called == []


def test_tool_policy_require_approval_top_level_granted(monkeypatch):
    """require-approval, top-level, approval GRANTED (via approvals=) → the tool dispatches."""
    calls = [_tool_call_obj("c1", "send_email", {"to": "a@b.c"})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["send_email"]) as disc:
        client, dispatched = _policy_client(gw, disc, monkeypatch)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={"default": "require-approval"},
        )
        result = run_managed_loop(client, config, "go", approvals=["tool:send_email"])

    assert result.approval_required is None
    assert dispatched == ["send_email"]
    assert result.tools_called == ["send_email"]
    assert result.output.startswith("POLICY_FINAL")


def test_tool_policy_require_approval_in_sub_run_fails_closed(monkeypatch):
    """require-approval INSIDE a delegated sub-run (X-Ctxmesh-Spawn-Depth: 1) → FAIL-CLOSED:
    pause_for_approval is NEVER called (proven by a spy), the tool is NOT dispatched, and the
    model receives the sub-run-denial message. This is the ADR 0058 fifth-issue rule: pausing a
    sub-run hangs the supervisor's synchronous await, so the loop must deny honestly instead."""
    import ctxmesh.managed as managed_mod

    # Spy on pause_for_approval AS THE LOOP CALLS IT: the loop resolves the name from the
    # ctxmesh.managed module namespace, so patch it there and assert it is never invoked.
    pause_calls: List[tuple] = []
    real_pause = managed_mod.pause_for_approval

    def spy_pause(key, summary):
        pause_calls.append((key, summary))
        return real_pause(key, summary)

    monkeypatch.setattr(managed_mod, "pause_for_approval", spy_pause)

    calls = [_tool_call_obj("c1", "send_email", {"to": "a@b.c"})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["send_email"]) as disc:
        client, dispatched = _policy_client(gw, disc, monkeypatch)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={"default": "require-approval"},
        )
        result = run_managed_loop(
            client, config, "go", headers={"X-Ctxmesh-Spawn-Depth": "1"}
        )

    # THE load-bearing assertion: pause_for_approval was NOT called at all inside the sub-run.
    assert pause_calls == [], "a sub-run must NOT pause for approval (fail-closed)"
    # The run did NOT become an approval_required outcome, and the tool never dispatched.
    assert result.approval_required is None
    assert dispatched == []
    assert result.tools_called == []
    # The model got the honest sub-run denial as the tool result.
    follow_up = json.loads(gw.requests[-1].body)
    tool_msgs = [m for m in follow_up["messages"] if m.get("role") == "tool"]
    assert len(tool_msgs) == 1
    assert "cannot be used inside a delegated sub-run" in tool_msgs[0]["content"]


@pytest.mark.parametrize(
    "forced, expected",
    [
        ("", None),  # unset → no tool_choice (provider auto)
        ("auto", "auto"),
        ("required", "required"),
        ("search", {"type": "function", "function": {"name": "search"}}),
    ],
)
def test_tool_policy_forced_choice_sets_tool_choice(monkeypatch, forced, expected):
    """forcedChoice → the right tool_choice on the model call. "" leaves it unset."""
    # A conforming default (allow) so the single call dispatches and the loop finishes cleanly.
    calls = [_tool_call_obj("c1", "search", {"q": "x"})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["search"]) as disc:
        client, _ = _policy_client(gw, disc, monkeypatch)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={"default": "allow", "forcedChoice": forced},
        )
        run_managed_loop(client, config, "go")

    # Assert on the FIRST model request (turn 1), where the loop applies forcedChoice.
    first_body = json.loads(gw.requests[0].body)
    if expected is None:
        assert "tool_choice" not in first_body
    else:
        assert first_body["tool_choice"] == expected


def test_tool_policy_parallel_limit_caps_dispatch(monkeypatch):
    """parallelLimit=1 with 3 tool calls in a turn → exactly the FIRST is dispatched; the other
    two come back with the honest skip message so the model can re-request them next turn."""
    calls = [
        _tool_call_obj("c1", "t1", {}),
        _tool_call_obj("c2", "t2", {}),
        _tool_call_obj("c3", "t3", {}),
    ]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["t1", "t2", "t3"]) as disc:
        client, dispatched = _policy_client(gw, disc, monkeypatch)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={"default": "allow", "parallelLimit": 1},
        )
        result = run_managed_loop(client, config, "go")

    # Exactly one tool ran; the first in the model's order.
    assert dispatched == ["t1"]
    assert result.tools_called == ["t1"]
    # The follow-up turn carried the two skip messages (honest, re-requestable).
    follow_up = json.loads(gw.requests[-1].body)
    tool_msgs = {m["tool_call_id"]: m["content"] for m in follow_up["messages"]
                 if m.get("role") == "tool"}
    assert "exceeds the tool parallel-limit of 1" in tool_msgs["c2"]
    assert "exceeds the tool parallel-limit of 1" in tool_msgs["c3"]


def test_tool_policy_none_is_unchanged(monkeypatch):
    """tool_policy=None (the default) → all tools dispatch, no tool_choice, no limit."""
    calls = [
        _tool_call_obj("c1", "t1", {}),
        _tool_call_obj("c2", "t2", {}),
    ]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["t1", "t2"]) as disc:
        client, dispatched = _policy_client(gw, disc, monkeypatch)
        config = ManagedConfig(system_prompt="sys", model_route="m")  # no tool_policy
        result = run_managed_loop(client, config, "go")

    assert dispatched == ["t1", "t2"]
    assert result.tools_called == ["t1", "t2"]
    first_body = json.loads(gw.requests[0].body)
    assert "tool_choice" not in first_body


def test_resolve_tool_rule_unit():
    """Unit coverage for _resolve_tool_rule: override wins, else default, else allow."""
    from ctxmesh.managed import _resolve_tool_rule

    policy = {
        "default": "require-approval",
        "overrides": [{"name": "safe", "rule": "allow"}, {"name": "bad", "rule": "deny"}],
    }
    assert _resolve_tool_rule(policy, "safe") == "allow"
    assert _resolve_tool_rule(policy, "bad") == "deny"
    # Not named by an override → the default.
    assert _resolve_tool_rule(policy, "other") == "require-approval"
    # No default → allow.
    assert _resolve_tool_rule({}, "x") == "allow"
    # A malformed/unrecognised override rule falls through to the default (never a silent widen).
    assert _resolve_tool_rule(
        {"default": "deny", "overrides": [{"name": "x", "rule": "bogus"}]}, "x"
    ) == "deny"


def test_forced_and_limit_unit():
    """Unit coverage for _forced_tool_choice and _parallel_limit edge cases."""
    from ctxmesh.managed import _forced_tool_choice, _parallel_limit

    assert _forced_tool_choice({}) is None
    assert _forced_tool_choice({"forcedChoice": ""}) is None
    assert _forced_tool_choice({"forcedChoice": "auto"}) == "auto"
    assert _forced_tool_choice({"forcedChoice": "required"}) == "required"
    assert _forced_tool_choice({"forcedChoice": "mytool"}) == {
        "type": "function",
        "function": {"name": "mytool"},
    }
    # parallel-limit: positive int caps; non-positive / non-int / bool → 0 (unlimited).
    assert _parallel_limit({"parallelLimit": 3}) == 3
    assert _parallel_limit({"parallelLimit": 0}) == 0
    assert _parallel_limit({"parallelLimit": -1}) == 0
    assert _parallel_limit({}) == 0
    assert _parallel_limit({"parallelLimit": True}) == 0


# ── Per-turn resilience (m65.7, ADR 0058) ──────────────────────────────────────
#
# These drive the model-call / tool-call resilience the managed loop applies when
# ManagedConfig.resilience is set: timeouts, idempotency-aware retries, and the
# per-run circuit breaker. Backoff sleeps are monkeypatched to zero so the suite
# stays fast; failures are simulated by wrapping client.model.chat / client.tools.call.


@pytest.fixture(autouse=True)
def _no_backoff_sleep(monkeypatch):
    """Neutralise the resilience backoff sleep for THIS test module so injected retries
    don't add real wall-clock time (the anti-stall requirement). Only patches the sleep
    the managed module calls; unrelated timing is untouched."""
    import ctxmesh.managed as managed_mod

    monkeypatch.setattr(managed_mod.time, "sleep", lambda _s: None)


class _NToolTurnsGatewayStub(_BaseStub):
    """A gateway that returns the SAME single tool call for the first ``turns`` model
    requests, then a final answer — regardless of what tool results came back. Lets a test
    drive the same tool N times in one run (to exercise the circuit breaker across turns)."""

    def __init__(self, call: Dict[str, Any], turns: int) -> None:
        self._call = call
        self._turns = turns
        self._seen = 0
        super().__init__()

    def _install_routes(self) -> None:
        def completions(state: _StubState, req: RecordedRequest):
            self._seen += 1
            if self._seen <= self._turns:
                resp = {
                    "id": "chatcmpl-nturns",
                    "object": "chat.completion",
                    "model": "nturns-mock",
                    "choices": [
                        {
                            "index": 0,
                            "finish_reason": "tool_calls",
                            "message": {
                                "role": "assistant",
                                "content": None,
                                "tool_calls": [self._call],
                            },
                        }
                    ],
                    "usage": USAGE,
                }
            else:
                resp = _plain_final_body("NTURNS_FINAL done")
            return 200, {"Content-Type": "application/json"}, json.dumps(resp).encode()

        self.state.routes.update({"POST /chat/completions": completions})


def _resilience_client(gw, disc):
    """A client on the given gateway/discovery (no tool monkeypatch — tests install their own)."""
    plane = PlaneConfig.for_test(discovery_base_url=disc.base_url, model_gateway_url=gw.base_url)
    return agent.from_config(plane)


# ── model-call resilience ───────────────────────────────────────────────────────


def test_model_retry_on_transient_failure_then_success(monkeypatch):
    """resilience.modelCall.maxRetries=2: chat raises EndpointError once then succeeds →
    the loop retries and returns the eventual final answer. Proves the retry happened."""
    from ctxmesh.errors import EndpointError

    with _SequentialGatewayStub(["MODEL_FINAL done"]) as gw, _EmptyDiscoveryStub() as disc:
        client = _resilience_client(gw, disc)
        real_chat = client.model.chat
        attempts = {"n": 0}

        def flaky_chat(route, messages, **opts):
            attempts["n"] += 1
            if attempts["n"] == 1:
                raise EndpointError("transient gateway blip", status=502)
            return real_chat(route, messages, **opts)

        monkeypatch.setattr(client.model, "chat", flaky_chat)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            resilience={"modelCall": {"timeoutSeconds": 0, "maxRetries": 2}},
        )
        result = run_managed_loop(client, config, "go")

    assert attempts["n"] == 2, "chat was retried exactly once after the transient failure"
    assert result.output.startswith("MODEL_FINAL")


def test_model_timeout_is_passed_into_chat_opts(monkeypatch):
    """resilience.modelCall.timeoutSeconds>0 → the loop sets chat_opts['timeout'] on the call."""
    with _SequentialGatewayStub(["MODEL_FINAL done"]) as gw, _EmptyDiscoveryStub() as disc:
        client = _resilience_client(gw, disc)
        real_chat = client.model.chat
        seen_timeout = {"value": "unset"}

        def capturing_chat(route, messages, **opts):
            seen_timeout["value"] = opts.get("timeout", "unset")
            return real_chat(route, messages, **opts)

        monkeypatch.setattr(client.model, "chat", capturing_chat)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            resilience={"modelCall": {"timeoutSeconds": 17, "maxRetries": 0}},
        )
        run_managed_loop(client, config, "go")

    assert seen_timeout["value"] == 17


# ── tool-call resilience: THE idempotency-critical tests ────────────────────────


def test_tool_not_retried_by_default_safety(monkeypatch):
    """THE safety test. A NON-retryable tool (no retryable:true override) that raises on dispatch
    is attempted EXACTLY ONCE — it is NOT retried — even with toolCall.maxRetries=3. Proven by
    counting the fake tool's invocations: exactly 1. Blind retry of a non-idempotent tool would
    double-execute its side effect, so a tool without an explicit retryable marker is never retried.

    The gateway serves a SINGLE tool-calling turn (via _PolicyGatewayStub, which flips to a final
    answer the moment a tool result appears). So the fake tool is offered exactly one dispatch
    opportunity — any count above 1 could ONLY come from a retry. The failure is threaded to the
    model as an honest tool-error result (resilience.toolCall is configured), the loop finishes,
    and the count is asserted to be exactly 1: no retry happened."""
    from ctxmesh.errors import EndpointError

    calls = [_tool_call_obj("c1", "send_email", {"to": "a@b.c"})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["send_email"]) as disc:
        client = _resilience_client(gw, disc)
        attempts = {"n": 0}

        def always_fail(name, **kwargs):
            attempts["n"] += 1
            raise EndpointError("tool boom", status=500)

        monkeypatch.setattr(client.tools, "call", always_fail)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            # maxRetries is generous, but send_email is NOT marked retryable → single attempt.
            tool_policy={"default": "allow"},
            resilience={"toolCall": {"timeoutSeconds": 0, "maxRetries": 3}},
        )
        result = run_managed_loop(client, config, "go")

    # THE load-bearing assertion: the non-retryable tool was dispatched EXACTLY ONCE (no retry),
    # despite maxRetries=3. A second call could only be a retry — there is exactly one turn.
    assert attempts["n"] == 1, "a non-retryable tool must be attempted EXACTLY ONCE (no retry)"
    # The failure surfaced honestly to the model as the tool result (not swallowed, not retried).
    follow_up = json.loads(gw.requests[-1].body)
    tool_msgs = [m for m in follow_up["messages"] if m.get("role") == "tool"]
    assert "send_email" in tool_msgs[-1]["content"] and "failed" in tool_msgs[-1]["content"]
    assert result.tools_called == []  # a failed dispatch is not recorded as executed


def test_tool_retried_when_explicitly_retryable(monkeypatch):
    """A tool whose override sets retryable:true raises once then succeeds → the loop retries
    (exactly TWO attempts) and the run completes. Opt-in retry is only for declared-idempotent
    tools."""
    from ctxmesh.errors import EndpointError

    calls = [_tool_call_obj("c1", "read_doc", {"id": "x"})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["read_doc"]) as disc:
        client = _resilience_client(gw, disc)
        attempts = {"n": 0}

        def fail_once(name, **kwargs):
            attempts["n"] += 1
            if attempts["n"] == 1:
                raise EndpointError("transient tool blip", status=503)
            return {"content": [{"type": "text", "text": "read_doc ok"}]}

        monkeypatch.setattr(client.tools, "call", fail_once)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={
                "default": "allow",
                "overrides": [{"name": "read_doc", "rule": "allow", "retryable": True}],
            },
            resilience={"toolCall": {"timeoutSeconds": 0, "maxRetries": 2}},
        )
        result = run_managed_loop(client, config, "go")

    assert attempts["n"] == 2, "a retryable tool is retried once after the transient failure"
    assert result.tools_called == ["read_doc"]
    assert result.output.startswith("POLICY_FINAL")


def test_tool_timeout_is_passed_into_tools_call(monkeypatch):
    """resilience.toolCall.timeoutSeconds>0 → the loop passes timeout=<n> into client.tools.call."""
    calls = [_tool_call_obj("c1", "search", {"q": "x"})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["search"]) as disc:
        client = _resilience_client(gw, disc)
        seen = {"timeout": "unset"}

        def capturing_call(name, **kwargs):
            seen["timeout"] = kwargs.get("timeout", "unset")
            return {"content": [{"type": "text", "text": "search ran"}]}

        monkeypatch.setattr(client.tools, "call", capturing_call)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={"default": "allow"},
            resilience={"toolCall": {"timeoutSeconds": 9, "maxRetries": 0}},
        )
        run_managed_loop(client, config, "go")

    assert seen["timeout"] == 9


# ── circuit breaker ─────────────────────────────────────────────────────────────


def test_circuit_breaker_opens_and_short_circuits(monkeypatch):
    """A tool failing failureThreshold times → the breaker OPENS; the next call short-circuits
    WITHOUT dispatching and the model gets the honest 'circuit open' message. Proven by the
    dispatch count: exactly `failureThreshold` real dispatches, then no more."""
    from ctxmesh.errors import EndpointError

    call = _tool_call_obj("c1", "flaky", {})
    # 4 tool-calling turns: 3 to trip the threshold, a 4th that must short-circuit.
    with _NToolTurnsGatewayStub(call, turns=4) as gw, _MultiToolDiscoveryStub(["flaky"]) as disc:
        client = _resilience_client(gw, disc)
        dispatches = {"n": 0}

        def always_fail(name, **kwargs):
            dispatches["n"] += 1
            raise EndpointError("flaky down", status=500)

        monkeypatch.setattr(client.tools, "call", always_fail)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            # flaky is NOT retryable → one dispatch per turn; threshold 3 opens after 3 turns.
            tool_policy={"default": "allow"},
            resilience={
                "toolCall": {
                    "timeoutSeconds": 0,
                    "maxRetries": 0,
                    "circuitBreaker": {"failureThreshold": 3, "cooldownSeconds": 60},
                }
            },
        )
        result = run_managed_loop(client, config, "go")

    # Exactly 3 real dispatches (the threshold); the 4th turn short-circuited — no 4th dispatch.
    assert dispatches["n"] == 3, "breaker opened on the 3rd failure; the 4th call did not dispatch"
    # The run still completed (the short-circuit is not a crash) and returned the final answer.
    assert result.output.startswith("NTURNS_FINAL")
    # The 4th turn's tool result carried the honest circuit-open message.
    follow_up = json.loads(gw.requests[-1].body)
    tool_msgs = [m for m in follow_up["messages"] if m.get("role") == "tool"]
    assert "circuit open for tool 'flaky'" in tool_msgs[-1]["content"]


def test_circuit_breaker_half_open_recovers(monkeypatch):
    """After cooldownSeconds elapse, the open breaker allows ONE probe; a success closes it.
    The monotonic clock is monkeypatched so no real time passes."""
    from ctxmesh.errors import EndpointError

    call = _tool_call_obj("c1", "svc", {})
    # 3 turns: 2 fail to trip threshold=2 (open), turn 3 is the half-open probe (succeeds).
    with _NToolTurnsGatewayStub(call, turns=3) as gw, _MultiToolDiscoveryStub(["svc"]) as disc:
        client = _resilience_client(gw, disc)
        import ctxmesh.managed as managed_mod

        # A monotonic clock that JUMPS forward on every read (by more than the cooldown). So the
        # allow-check on turn 3 (after the breaker opened on turn 2's failure) always sees the
        # cooldown elapsed → the half-open probe is admitted. Turns 1-2 still fail and open it,
        # because record_failure stamps open_until = now + cooldown at the LATEST clock read.
        ticks = {"t": 1000.0}

        def jumping_monotonic():
            ticks["t"] += 1000.0  # >> cooldownSeconds (30) so any open breaker is always cooled
            return ticks["t"]

        monkeypatch.setattr(managed_mod.time, "monotonic", jumping_monotonic)

        dispatches = {"n": 0}

        def two_fail_then_ok(name, **kwargs):
            dispatches["n"] += 1
            if dispatches["n"] <= 2:
                raise EndpointError("svc down", status=500)
            return {"content": [{"type": "text", "text": "svc ok"}]}

        monkeypatch.setattr(client.tools, "call", two_fail_then_ok)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={"default": "allow"},
            resilience={
                "toolCall": {
                    "timeoutSeconds": 0,
                    "maxRetries": 0,
                    "circuitBreaker": {"failureThreshold": 2, "cooldownSeconds": 30},
                }
            },
        )
        result = run_managed_loop(client, config, "go")

    # 2 failures opened it; the probe (dispatch #3) was admitted and succeeded → run finished.
    assert dispatches["n"] == 3, "the half-open probe was admitted after the cooldown"
    assert result.tools_called == ["svc"], "the successful probe recorded a real tool call"
    assert result.output.startswith("NTURNS_FINAL")


# ── sub-run retry bounding ──────────────────────────────────────────────────────


def test_sub_run_caps_tool_retries(monkeypatch):
    """Inside a delegated sub-run (X-Ctxmesh-Spawn-Depth: 1), a retryable tool that ALWAYS fails
    with maxRetries=3 is attempted AT MOST twice (1 retry), not four times — a retry storm inside a
    synchronously-awaited sub-run amplifies M64's parked-worker limit and multiplies spend."""
    from ctxmesh.errors import EndpointError

    calls = [_tool_call_obj("c1", "read_doc", {"id": "x"})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["read_doc"]) as disc:
        client = _resilience_client(gw, disc)
        attempts = {"n": 0}

        def always_fail(name, **kwargs):
            attempts["n"] += 1
            raise EndpointError("read_doc down", status=500)

        monkeypatch.setattr(client.tools, "call", always_fail)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            tool_policy={
                "default": "allow",
                "overrides": [{"name": "read_doc", "rule": "allow", "retryable": True}],
            },
            resilience={"toolCall": {"timeoutSeconds": 0, "maxRetries": 3}},
        )
        # One tool turn (the stub flips to a final answer once a tool result appears); the retries
        # all happen inside the single dispatch, then the failure threads and the loop finishes.
        run_managed_loop(client, config, "go", headers={"X-Ctxmesh-Spawn-Depth": "1"})

    assert attempts["n"] == 2, "a sub-run caps retries to 1 (2 attempts), not the configured 3"


def test_sub_run_caps_model_retries(monkeypatch):
    """Inside a sub-run, model-call retries are also capped to 1: chat that ALWAYS fails with
    maxRetries=3 is attempted at most twice (1 retry) before the failure surfaces."""
    from ctxmesh.errors import EndpointError

    with _PolicyGatewayStub([]) as gw, _MultiToolDiscoveryStub([]) as disc:
        client = _resilience_client(gw, disc)
        attempts = {"n": 0}

        def always_fail(route, messages, **opts):
            attempts["n"] += 1
            raise EndpointError("gateway down", status=502)

        monkeypatch.setattr(client.model, "chat", always_fail)
        config = ManagedConfig(
            system_prompt="sys",
            model_route="m",
            resilience={"modelCall": {"timeoutSeconds": 0, "maxRetries": 3}},
        )
        with pytest.raises(EndpointError):
            run_managed_loop(client, config, "go", headers={"X-Ctxmesh-Spawn-Depth": "1"})

    assert attempts["n"] == 2, "a sub-run caps model retries to 1 (2 attempts), not configured 3"


# ── resilience None → unchanged (regression) ────────────────────────────────────


def test_resilience_none_is_unchanged(monkeypatch):
    """resilience=None (the default) → no timeout override on chat OR tool, no retry, no breaker:
    a single chat call, a single tool dispatch, and no 'timeout' kwarg on either."""
    calls = [_tool_call_obj("c1", "search", {"q": "x"})]
    with _PolicyGatewayStub(calls) as gw, _MultiToolDiscoveryStub(["search"]) as disc:
        client = _resilience_client(gw, disc)
        chat_opts_seen: List[Dict[str, Any]] = []
        real_chat = client.model.chat

        def capturing_chat(route, messages, **opts):
            chat_opts_seen.append(dict(opts))
            return real_chat(route, messages, **opts)

        tool_kwargs_seen: List[Dict[str, Any]] = []

        def capturing_call(name, **kwargs):
            tool_kwargs_seen.append(dict(kwargs))
            return {"content": [{"type": "text", "text": "search ran"}]}

        monkeypatch.setattr(client.model, "chat", capturing_chat)
        monkeypatch.setattr(client.tools, "call", capturing_call)
        config = ManagedConfig(system_prompt="sys", model_route="m")  # resilience=None
        result = run_managed_loop(client, config, "go")

    # No timeout override was injected on any chat call, and the tool call carried no timeout.
    assert all("timeout" not in o for o in chat_opts_seen)
    assert all("timeout" not in k for k in tool_kwargs_seen)
    # Exactly one tool dispatch (no retry, no breaker interference).
    assert len(tool_kwargs_seen) == 1
    assert result.tools_called == ["search"]
    assert result.output.startswith("POLICY_FINAL")


# ── resilience helper units ─────────────────────────────────────────────────────


def test_resilience_helpers_unit():
    """Unit coverage for the m65.7 helpers: retryable gate, sub-run cap, breaker build/state."""
    from ctxmesh.managed import (
        _CircuitBreaker,
        _effective_retries,
        _make_breaker,
        _tool_retryable,
    )

    # retryable gate: only an explicit True on a matching override; everything else False.
    policy = {"overrides": [{"name": "safe", "retryable": True}, {"name": "no", "rule": "allow"}]}
    assert _tool_retryable(policy, "safe") is True
    assert _tool_retryable(policy, "no") is False  # override present but no retryable:true
    assert _tool_retryable(policy, "absent") is False  # no override at all
    assert _tool_retryable(None, "safe") is False  # no policy
    assert _tool_retryable({"overrides": [{"name": "x", "retryable": "true"}]}, "x") is False  # str

    # sub-run cap: min(configured, 1) when spawn_depth>0; configured at top level.
    assert _effective_retries(3, 0) == 3
    assert _effective_retries(3, 1) == 1
    assert _effective_retries(0, 1) == 0

    # breaker build: absent circuitBreaker / non-positive threshold → None.
    assert _make_breaker(None) is None
    assert _make_breaker({"toolCall": {}}) is None
    assert _make_breaker({"toolCall": {"circuitBreaker": {"failureThreshold": 0}}}) is None
    b = _make_breaker(
        {"toolCall": {"circuitBreaker": {"failureThreshold": 2, "cooldownSeconds": 5}}}
    )
    assert isinstance(b, _CircuitBreaker)

    # breaker state machine: closed → open at threshold; a success resets the count.
    cb = _CircuitBreaker(failure_threshold=2, cooldown_seconds=1000.0)
    assert cb.allow("t") is True
    cb.record_failure("t")
    assert cb.allow("t") is True  # 1 failure < threshold → still closed
    cb.record_success("t")  # reset
    cb.record_failure("t")
    cb.record_failure("t")  # 2 consecutive → open
    assert cb.allow("t") is False  # open, cooling down → short-circuit
