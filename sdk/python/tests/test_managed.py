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

from ctxmesh import ManagedConfig, agent, run_managed_loop
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
        plane = PlaneConfig.for_test(discovery_base_url=disc.base_url, model_gateway_url="http://gw")
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
