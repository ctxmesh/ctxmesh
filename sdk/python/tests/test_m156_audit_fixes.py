"""Regression tests for the M156 SDK-audit findings, each verified against the code first.

Every test here pins a defect that was SILENT: the SDK returned a plausible answer while
doing the wrong thing, so nothing in the existing suite could have failed.
"""

from __future__ import annotations

import pytest

from ctxmesh import agent
from ctxmesh.config import PlaneConfig, RunContext
from ctxmesh.errors import ApprovalRequiredError, ConsentRequiredError, EndpointError
from ctxmesh.serve import process_invoke
from ctxmesh.testing import DiscoveryStub, GatewayStub, MemoryStub
from ctxmesh.tools import _first_text_content


def _client(mem, disc, gw):
    return agent.from_config(
        PlaneConfig.for_test(
            memory_base_url=mem.base_url,
            discovery_base_url=disc.base_url,
            model_gateway_url=gw.base_url,
            run=RunContext(agent_name="audit-agent", conversation_id=""),
        )
    )


def test_custom_handler_approval_becomes_an_approval_outcome_not_a_502():
    """A custom handler that pauses for approval must yield the HITL envelope.

    The managed loop already converted this; a hand-rolled handler's exception fell through
    to the HTTP layer and became `502 {"error": ...}` — a FAILED RUN where the product should
    show an approval prompt. TypeScript has handled it since it shipped, and its comment
    claimed to "mirror Python".
    """
    with MemoryStub() as mem, DiscoveryStub() as disc, GatewayStub() as gw:
        client = _client(mem, disc, gw)

        def handler(_req):
            raise ApprovalRequiredError("pause", key="refund:approve", summary="refund 500 USD")

        body = process_invoke(client, handler, "audit-agent", b'{"input":"x"}', {})

    assert body["approval_required"] == {"key": "refund:approve", "summary": "refund 500 USD"}
    assert "Awaiting approval" in body["output"]


def test_custom_handler_consent_becomes_a_consent_outcome():
    with MemoryStub() as mem, DiscoveryStub() as disc, GatewayStub() as gw:
        client = _client(mem, disc, gw)

        def handler(_req):
            raise ConsentRequiredError("connect", server="github")

        body = process_invoke(client, handler, "audit-agent", b'{"input":"x"}', {})

    assert body["consent_required"] == ["github"]


def test_mcp_is_error_is_a_failure_not_a_result():
    """`isError: true` is an execution failure the server reports IN BAND.

    Returning its text as an ordinary result told the model, the TOOL span and the author
    that a failed call had succeeded — and hid it from the managed loop's circuit breaker and
    retry gate, which only count exceptions. A tool failing every time never tripped either.
    """
    with pytest.raises(EndpointError) as exc:
        _first_text_content(
            {"isError": True, "content": [{"type": "text", "text": "database is down"}]},
            "http://tool.example/mcp",
        )
    assert "tool execution error" in str(exc.value)
    assert "database is down" in str(exc.value)


def test_a_successful_result_is_still_returned():
    assert (
        _first_text_content({"content": [{"type": "text", "text": "42"}]}, "http://t/mcp")
        == "42"
    )
    # isError present but false must NOT be treated as a failure.
    assert (
        _first_text_content(
            {"isError": False, "content": [{"type": "text", "text": "42"}]}, "http://t/mcp"
        )
        == "42"
    )


def test_a_tool_argument_named_timeout_reaches_the_tool():
    """The collision that made the old docstring false.

    `call(name, *, timeout=None, **args)` bound a model-produced `{"timeout": 30}` to the
    SOCKET timeout, and the tool never saw the argument. The dict path removes the ambiguity;
    this pins that the dict wins and the keyword still controls the socket.
    """
    import ctxmesh.tools as tools_mod

    seen = {}

    def fake_mcp_call(endpoint, name, args, timeout=None):
        seen["args"] = dict(args)
        seen["timeout"] = timeout
        return "ok"

    with MemoryStub() as mem, DiscoveryStub() as disc, GatewayStub() as gw:
        client = _client(mem, disc, gw)
        original = tools_mod._mcp_call_tool
        tools_mod._mcp_call_tool = fake_mcp_call
        try:
            client.tools.call(DiscoveryStub.CATALOG_NAME, {"timeout": 30}, timeout=5.0)
        finally:
            tools_mod._mcp_call_tool = original

    assert seen["args"] == {"timeout": 30}, "the tool must receive its own timeout argument"
    assert seen["timeout"] == 5.0, "the socket timeout must still come from the keyword"
