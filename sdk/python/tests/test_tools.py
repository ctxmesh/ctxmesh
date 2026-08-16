"""Tools client contract tests: discovery manifest + MCP tools/call."""

from __future__ import annotations

import json

import pytest

from ctxmesh import agent
from ctxmesh._capability import CAPABILITY_HEADER
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError, EndpointError
from ctxmesh.tools import Tool

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


def test_tool_from_dict_parses_input_schema():
    """A manifest tool carrying inputSchema exposes it verbatim on the Tool (m14.6b)."""
    schema = {
        "type": "object",
        "properties": {"text": {"type": "string"}},
        "required": ["text"],
    }
    tool = Tool.from_dict(
        {
            "name": "word-count",
            "mode": "remote",
            "endpoint": "http://wc.svc/mcp",
            "transport": "streamable-http",
            "inputSchema": schema,
        }
    )
    # Parsed verbatim — the exact object the manifest carried.
    assert tool.input_schema == schema


def test_tool_from_dict_absent_input_schema_is_none():
    """A manifest tool WITHOUT inputSchema (or a non-object one) → input_schema None."""
    base = {
        "name": "echo",
        "mode": "remote",
        "endpoint": "http://echo.svc/mcp",
        "transport": "streamable-http",
    }
    # Absent key.
    assert Tool.from_dict(base).input_schema is None
    # Explicit null.
    assert Tool.from_dict({**base, "inputSchema": None}).input_schema is None
    # A non-object (defensive): also treated as "no schema".
    assert Tool.from_dict({**base, "inputSchema": "not-an-object"}).input_schema is None


def test_tool_from_dict_parses_description():
    """A manifest tool carrying a description exposes it on the Tool (FUNC-10); absent or a
    non-string degrades to "" so the loop synthesises a generic one."""
    base = {
        "name": "word-count",
        "mode": "remote",
        "endpoint": "http://wc.svc/mcp",
        "transport": "streamable-http",
    }
    assert Tool.from_dict({**base, "description": "Count words."}).description == "Count words."
    assert Tool.from_dict(base).description == ""
    assert Tool.from_dict({**base, "description": None}).description == ""
    assert Tool.from_dict({**base, "description": 123}).description == ""


def test_list_carries_description_from_manifest(discovery_stub: DiscoveryStub):
    """tools.list() surfaces a manifest tool's description on the discovered Tool (FUNC-10)."""

    def manifest_with_desc():
        m = DiscoveryStub._manifest(discovery_stub)
        m["tools"][0]["description"] = "Count whitespace-separated words."
        return m

    discovery_stub._manifest = manifest_with_desc  # type: ignore[method-assign]
    cfg = PlaneConfig.for_test(discovery_base_url=discovery_stub.base_url)
    tools = agent.from_config(cfg).tools.list()
    assert tools[0].description == "Count whitespace-separated words."


def test_list_carries_input_schema_from_manifest(discovery_stub: DiscoveryStub):
    """tools.list() surfaces a manifest's inputSchema on the discovered Tool."""
    schema = {"type": "object", "properties": {"n": {"type": "integer"}}}

    def manifest_with_schema():
        m = DiscoveryStub._manifest(discovery_stub)
        m["tools"][0]["inputSchema"] = schema
        return m

    discovery_stub._manifest = manifest_with_schema  # type: ignore[method-assign]
    cfg = PlaneConfig.for_test(discovery_base_url=discovery_stub.base_url)
    tools = agent.from_config(cfg).tools.list()
    assert tools[0].input_schema == schema


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
    methods = [r.json().get("method") for r in discovery_stub.requests if r.path == "/mcp/"]
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
    assert discovery_stub.mcp_calls == [{"name": "word_count", "arguments": {"text": "a b c"}}]
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


def test_list_no_sidecar_no_tools_json_is_empty(tmp_path):
    # A managed agent with NO tools bound has neither a discovery sidecar
    # (localhost:2999) nor a tools.json ConfigMap mounted: the controller injects
    # both only when the agent has bindings. list() must return an empty manifest
    # (no tools), NOT raise — "no tools" is a first-class chat agent, not a broken
    # one (spec console-runs U-run-manifest). Without this, a zero-tools managed
    # agent 502s on every invoke with "tool manifest unavailable".
    missing = tmp_path / "no-such-dir" / "tools.json"  # a path that does not exist
    cfg = PlaneConfig.for_test(
        discovery_base_url="http://127.0.0.1:1",  # dead port → sidecar unreachable
        tools_json_path=str(missing),
    )
    c = agent.from_config(cfg)
    assert c.tools.list() == []


def test_list_broken_tools_json_raises(tmp_path):
    # A tools.json that EXISTS but is malformed is a genuinely broken manifest —
    # surface it loudly (EndpointError), do NOT silently degrade to no-tools. This
    # is the distinction from an absent file (which is a valid zero-tools agent).
    tools_json = tmp_path / "tools.json"
    tools_json.write_text("{ this is not valid json")
    cfg = PlaneConfig.for_test(
        discovery_base_url="http://127.0.0.1:1",  # dead port → sidecar unreachable
        tools_json_path=str(tools_json),
    )
    c = agent.from_config(cfg)
    with pytest.raises(EndpointError):
        c.tools.list()


# ── delegate_to synthetic tool (M64, ADR 0057) ────────────────────────────────


def test_delegate_tool_present_when_enabled(client, discovery_stub: DiscoveryStub, monkeypatch):
    """A team supervisor (DELEGATE_ENABLED) gets the synthetic delegate_to tool next to its MCP
    tools, with the roster driving the schema enum + the description."""
    monkeypatch.setenv("DELEGATE_ENABLED", "true")
    monkeypatch.setenv(
        "DELEGATE_ROSTER",
        json.dumps(
            [
                {"name": "researcher", "description": "searches the web"},
                {"name": "coder", "description": "writes code"},
            ]
        ),
    )
    tools = client.tools.list()
    names = [t.name for t in tools]
    assert "delegate_to" in names, "the supervisor sees delegate_to next to its MCP tools"
    assert "word-count" in names, "the MCP tools are still present"

    dt = next(t for t in tools if t.name == "delegate_to")
    assert dt.mode == "delegate"
    assert dt.input_schema["properties"]["sub_agent"]["enum"] == ["researcher", "coder"]
    assert dt.input_schema["required"] == ["sub_agent", "task"]
    assert "researcher: searches the web" in dt.description


def test_delegate_tool_absent_when_disabled(client, discovery_stub: DiscoveryStub, monkeypatch):
    """A plain agent (no DELEGATE_ENABLED) never sees delegate_to."""
    monkeypatch.delenv("DELEGATE_ENABLED", raising=False)
    assert "delegate_to" not in [t.name for t in client.tools.list()]


def test_delegate_posts_and_relays_capability(client, monkeypatch):
    """delegate() POSTs the spawn body to the launcher-local endpoint, relays the run capability,
    and returns the launcher's {ok, answer} verbatim."""
    captured = {}

    class _Resp:
        def json(self):
            return {"ok": True, "answer": "sub-answer", "subRun": "sub-1"}

    def fake_request(method, url, *, body=None, headers=None, timeout=None, expect=None):
        captured.update(
            method=method, url=url, body=json.loads(body), headers=headers, expect=expect
        )
        return _Resp()

    monkeypatch.setattr("ctxmesh.tools._http.request", fake_request)
    monkeypatch.setattr("ctxmesh.tools.current_capability", lambda: "cap-token")

    out = client.tools.delegate(sub_agent="researcher", task="find it", step="2", call_id="c9")

    assert out == {"ok": True, "answer": "sub-answer", "subRun": "sub-1"}
    assert captured["method"] == "POST"
    assert captured["url"].endswith(":2994/delegate")
    assert captured["expect"] == (200,)
    assert captured["body"] == {
        "subAgent": "researcher",
        "input": "find it",
        "step": "2",
        "callId": "c9",
    }
    assert (
        captured["headers"][CAPABILITY_HEADER] == "cap-token"
    ), "the parent capability is relayed (OBO)"


# ── handoff_to (M67, ADR 0060 §5) ──────────────────────────────────────────────


def test_handoff_tool_present_when_enabled(client, discovery_stub: DiscoveryStub, monkeypatch):
    """A roster-bearing agent (DELEGATE_ENABLED) gets the synthetic handoff_to tool next to
    delegate_to + its MCP tools, with the roster driving the target_agent enum + the description."""
    monkeypatch.setenv("DELEGATE_ENABLED", "true")
    monkeypatch.setenv(
        "DELEGATE_ROSTER",
        json.dumps(
            [
                {"name": "billing", "description": "handles billing"},
                {"name": "research", "description": "does research"},
            ]
        ),
    )
    tools = client.tools.list()
    names = [t.name for t in tools]
    assert "handoff_to" in names, "the roster-bearing agent sees handoff_to"
    assert "delegate_to" in names, "delegate_to is still present (both are offered)"

    ht = next(t for t in tools if t.name == "handoff_to")
    assert ht.mode == "handoff"
    assert ht.endpoint.endswith(":2994/handoff")
    assert ht.input_schema["properties"]["target_agent"]["enum"] == ["billing", "research"]
    assert ht.input_schema["required"] == ["target_agent"], "only target_agent is required"
    # include_history (m83.6) is exposed to the model as an optional boolean (default true).
    assert ht.input_schema["properties"]["include_history"]["type"] == "boolean"
    assert "billing: handles billing" in ht.description


def test_handoff_tool_absent_when_disabled(client, discovery_stub: DiscoveryStub, monkeypatch):
    """A plain agent (no roster) never sees handoff_to."""
    monkeypatch.delenv("DELEGATE_ENABLED", raising=False)
    assert "handoff_to" not in [t.name for t in client.tools.list()]


def test_handoff_posts_and_relays_capability(client, monkeypatch):
    """handoff() POSTs the transfer body to the launcher-local :2994/handoff endpoint, relays the
    run capability (OBO for the conversation owner), and returns the launcher's outcome verbatim."""
    captured = {}

    class _Resp:
        def json(self):
            return {"ok": True, "runId": "hand-1", "sourceRun": "A-1", "handedOffTo": "billing"}

    def fake_request(method, url, *, body=None, headers=None, timeout=None, expect=None):
        captured.update(
            method=method, url=url, body=json.loads(body), headers=headers, expect=expect
        )
        return _Resp()

    monkeypatch.setattr("ctxmesh.tools._http.request", fake_request)
    monkeypatch.setattr("ctxmesh.tools.current_capability", lambda: "cap-token")

    out = client.tools.handoff(target_agent="billing", message="refund needed")

    assert out == {"ok": True, "runId": "hand-1", "sourceRun": "A-1", "handedOffTo": "billing"}
    assert captured["method"] == "POST"
    assert captured["url"].endswith(":2994/handoff")
    assert captured["expect"] == (200,)
    # include_history defaults True (replay B's full history — today's behavior, m83.6).
    assert captured["body"] == {
        "targetAgent": "billing",
        "message": "refund needed",
        "includeHistory": True,
    }
    assert captured["headers"][CAPABILITY_HEADER] == "cap-token", "the capability is relayed (OBO)"


def test_handoff_relays_include_history_false(client, monkeypatch):
    """handoff(include_history=False) (m83.6) posts includeHistory=false so the BFF records the
    transfer-turn history skip and B starts from `message` as a SUMMARY."""
    captured = {}

    class _Resp:
        def json(self):
            return {"ok": True, "runId": "hand-9"}

    def fake_request(method, url, *, body=None, headers=None, timeout=None, expect=None):
        captured.update(body=json.loads(body))
        return _Resp()

    monkeypatch.setattr("ctxmesh.tools._http.request", fake_request)
    monkeypatch.setattr("ctxmesh.tools.current_capability", lambda: None)

    client.tools.handoff(target_agent="billing", message="summary…", include_history=False)
    assert captured["body"] == {
        "targetAgent": "billing",
        "message": "summary…",
        "includeHistory": False,
    }


# ── record-mode relay on tool-call egress (M78, ADR 0071 §1/C1) ────────────────


def test_mcp_headers_relays_record_when_recorded():
    """A recorded run relays X-Ctxmesh-Record on every MCP tool-call egress so the egress sidecar
    captures the tool I/O into the run's replay fixture (TOOL channel)."""
    from ctxmesh._record import RECORD_HEADER, record_scope
    from ctxmesh.tools import _mcp_headers

    with record_scope({RECORD_HEADER: "run-tool-rec"}):
        headers = _mcp_headers(session_id=None)
    assert headers[RECORD_HEADER] == "run-tool-rec"


def test_mcp_headers_omits_record_when_not_recorded():
    """A non-recorded run relays NO X-Ctxmesh-Record header ⇒ the sidecar captures nothing."""
    from ctxmesh._record import RECORD_HEADER
    from ctxmesh.tools import _mcp_headers

    headers = _mcp_headers(session_id=None)  # no record_scope active
    assert RECORD_HEADER not in headers
