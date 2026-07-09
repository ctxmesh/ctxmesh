"""Tools client contract tests: discovery manifest + MCP tools/call."""

from __future__ import annotations

import pytest

from ctxmesh import agent
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError, EndpointError

from .launcher_stub import DiscoveryStub


def test_list_returns_manifest_tools(client, discovery_stub: DiscoveryStub):
    tools = client.tools.list()
    assert [t.name for t in tools] == ["word-count"]
    assert tools[0].mode == "remote"
    assert tools[0].transport == "streamable-http"
    assert tools[0].endpoint.endswith("/mcp")

    # It hit the discovery /tools endpoint.
    assert discovery_stub.requests[0].method == "GET"
    assert discovery_stub.requests[0].path == "/tools"


def test_call_full_mcp_handshake_and_parsed_result(client, discovery_stub: DiscoveryStub):
    result = client.tools.call("word-count", text="a b c")
    assert result == {"count": 3, "server_version": "v1"}

    # The MCP endpoint saw initialize + initialized + tools/call in order.
    methods = [
        r.json().get("method")
        for r in discovery_stub.requests
        if r.path == "/mcp"
    ]
    assert methods == ["initialize", "notifications/initialized", "tools/call"]

    # The tool was called with the right name + arguments.
    assert discovery_stub.mcp_calls == [{"name": "word-count", "arguments": {"text": "a b c"}}]


def test_call_propagates_session_id_header(client, discovery_stub: DiscoveryStub):
    client.tools.call("word-count", text="x")
    # initialize returns Mcp-Session-Id; subsequent calls must carry it back.
    mcp_reqs = [r for r in discovery_stub.requests if r.path == "/mcp"]
    initialized = mcp_reqs[1]
    assert initialized.headers.get("mcp-session-id") == "sess-1"


def test_call_unknown_tool_raises(client):
    with pytest.raises(ConfigError):
        client.tools.call("does-not-exist")


def test_call_mcp_error_surfaces(discovery_stub: DiscoveryStub):
    # A tool endpoint that returns an MCP error should surface, not swallow.
    # Point the manifest tool at a dead MCP port via a hand-built config.
    cfg = PlaneConfig.for_test(discovery_base_url=discovery_stub.base_url)
    # Monkeypatch the manifest to a dead endpoint by overriding tool_result path:
    # simplest: call a tool whose endpoint is unreachable.
    dead_tool = {
        "name": "dead",
        "mode": "remote",
        "endpoint": "http://127.0.0.1:1/mcp",
        "transport": "streamable-http",
    }
    discovery_stub._manifest = lambda: {  # type: ignore[method-assign]
        "version": "x",
        "tools": [dead_tool],
    }
    c = agent.from_config(cfg)
    with pytest.raises(EndpointError):
        c.tools.call("dead", foo="bar")


def test_list_falls_back_to_tools_json(tmp_path):
    # Sidecar unreachable (dead port) but tools.json present → fallback works.
    tools_json = tmp_path / "tools.json"
    tools_json.write_text(
        '{"version":"f","tools":[{"name":"local","mode":"remote","endpoint":"http://x/mcp","transport":"streamable-http"}]}'
    )
    cfg = PlaneConfig.for_test(
        discovery_base_url="http://127.0.0.1:1",
        tools_json_path=str(tools_json),
    )
    c = agent.from_config(cfg)
    tools = c.tools.list()
    assert [t.name for t in tools] == ["local"]
