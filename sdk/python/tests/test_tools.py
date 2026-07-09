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


def test_call_follows_307_redirect_to_mcp_slash(client, discovery_stub: DiscoveryStub):
    """The SDK follows the 307 redirect from /mcp to /mcp/ on every POST.

    FastMCP/Starlette mounts at /mcp/ and 307-redirects /mcp (no trailing
    slash).  The discovery manifest carries /mcp verbatim (m4.4 convention).
    This test proves the SDK follows the redirect so tools.call succeeds.
    """
    result = client.tools.call("word-count", text="a b c")
    assert result == {"count": 3, "server_version": "v1"}

    # The MCP handshake requests land at /mcp/ (after the redirect), not /mcp.
    # Each of the 4 POSTs goes through: POST /mcp (307) → POST /mcp/ (200/202).
    redirected_reqs = [r for r in discovery_stub.requests if r.path == "/mcp/"]
    methods = [r.json().get("method") for r in redirected_reqs]
    assert methods == [
        "initialize",
        "notifications/initialized",
        "tools/list",
        "tools/call",
    ]

    # The redirect hops are recorded at /mcp (the no-slash path).
    redirect_reqs = [r for r in discovery_stub.requests if r.path == "/mcp"]
    assert len(redirect_reqs) == 4  # one redirect trigger per MCP POST


def test_call_full_mcp_handshake_and_parsed_result(client, discovery_stub: DiscoveryStub):
    result = client.tools.call("word-count", text="a b c")
    assert result == {"count": 3, "server_version": "v1"}

    # The MCP endpoint saw initialize + initialized + tools/list + tools/call.
    # After the 307 redirect, all MCP POSTs land at /mcp/ (trailing slash).
    methods = [
        r.json().get("method")
        for r in discovery_stub.requests
        if r.path == "/mcp/"
    ]
    assert methods == [
        "initialize",
        "notifications/initialized",
        "tools/list",
        "tools/call",
    ]

    # Discovery ran (tools/list was consulted to resolve the name).
    assert discovery_stub.list_calls == 1


def test_call_resolves_catalog_name_to_mcp_name(client, discovery_stub: DiscoveryStub):
    """The catalog name (`word-count`) resolves to the MCP name (`word_count`).

    This is the regression guard: the stub REJECTS a tools/call whose name is
    not the server's real MCP name, so a client that forwarded the catalog name
    verbatim would raise here instead of succeeding.
    """
    result = client.tools.call("word-count", text="a b c")
    assert result == {"count": 3, "server_version": "v1"}

    # The accepted tools/call carried the RESOLVED underscore name, not the
    # hyphenated catalog name.
    assert discovery_stub.mcp_calls == [
        {"name": "word_count", "arguments": {"text": "a b c"}}
    ]
    assert discovery_stub.mcp_calls[0]["name"] == DiscoveryStub.MCP_TOOL_NAME
    assert DiscoveryStub.MCP_TOOL_NAME != DiscoveryStub.CATALOG_NAME  # they differ


def test_call_unresolvable_name_raises_clear_error(client, discovery_stub: DiscoveryStub):
    """A catalog name that maps to no server tool raises a clear ConfigError.

    The server advertises multiple tools (none normalizing to the catalog name),
    so neither the exact/normalized match nor the sole-tool fallback applies.
    """
    discovery_stub._server_tools = lambda: [  # type: ignore[method-assign]
        {"name": "alpha", "inputSchema": {}},
        {"name": "beta", "inputSchema": {}},
    ]
    with pytest.raises(ConfigError) as exc:
        client.tools.call("word-count", text="x")
    # The error names the tools the server actually exposes.
    assert "alpha" in str(exc.value) and "beta" in str(exc.value)


def test_call_sole_tool_fallback(client, discovery_stub: DiscoveryStub):
    """When the server exposes exactly one tool, an odd catalog name still binds."""
    discovery_stub._server_tools = lambda: [  # type: ignore[method-assign]
        {"name": "only_thing", "inputSchema": {}}
    ]
    result = client.tools.call("word-count", text="x")
    assert result == {"count": 3, "server_version": "v1"}
    assert discovery_stub.mcp_calls[0]["name"] == "only_thing"


def test_call_propagates_session_id_header(client, discovery_stub: DiscoveryStub):
    client.tools.call("word-count", text="x")
    # initialize returns Mcp-Session-Id; subsequent calls must carry it back.
    # After the 307 redirect, all MCP POSTs land at /mcp/ (trailing slash).
    mcp_reqs = [r for r in discovery_stub.requests if r.path == "/mcp/"]
    initialized = mcp_reqs[1]
    assert initialized.headers.get("mcp-session-id") == "sess-1"


def test_redirect_cross_origin_refused(discovery_stub: DiscoveryStub):
    """A 307 redirect to a different host is refused — never re-POST body cross-origin.

    This is the security boundary: a malicious or misconfigured redirect that
    points to a different host must not cause the SDK to re-POST a request body
    (which may carry secrets/tokens) to an arbitrary destination.
    """
    # Patch the mcp_redirect route to return a cross-origin Location.
    original_mcp_redirect = discovery_stub.state.routes["POST /mcp"]

    def cross_origin_redirect(state, req):  # type: ignore[no-untyped-def]
        return 307, {"Location": "http://evil.example.com:9999/mcp/"}, b""

    discovery_stub.state.routes["POST /mcp"] = cross_origin_redirect

    cfg = PlaneConfig.for_test(discovery_base_url=discovery_stub.base_url)
    c = agent.from_config(cfg)
    with pytest.raises(EndpointError) as exc_info:
        c.tools.call("word-count", text="x")

    # The error message must be explicit about the cross-origin refusal.
    assert "cross-origin" in str(exc_info.value).lower()
    # Restore the original route so the stub is usable in subsequent tests.
    discovery_stub.state.routes["POST /mcp"] = original_mcp_redirect


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
