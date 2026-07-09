"""Tools client — the discovery sidecar (:2999) + MCP tool invocation (M4).

``list()`` fetches the live manifest from the discovery sidecar
(``GET localhost:2999/tools``), falling back to the mounted ``tools.json``
(cold-start backing) when the sidecar is unreachable — the same precedence the
raw agent uses (mcp-tools.md "Agent consumption").

``call(name, **args)`` looks the tool up in the live manifest, then invokes it
over its MCP ``streamable-http`` endpoint (a JSON-RPC ``tools/call``). The
endpoint is taken verbatim from the manifest (it already ends in ``/mcp`` per
the m4.4 finding). We speak the MCP wire directly over stdlib rather than
pulling the heavyweight ``mcp`` package in as a runtime dep — the SDK stays lean
and 3.9-compatible.
"""

from __future__ import annotations

import json
from typing import Any, Dict, List, Optional

from ctxmesh import _http
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError, EndpointError

#: MCP protocol version the SDK negotiates (the version the M4 fixture speaks).
_MCP_PROTOCOL_VERSION = "2025-03-26"

#: Manifest fetch timeout — a cheap localhost GET, but generous for a waking pod.
_MANIFEST_TIMEOUT = 2.0

#: Tool-call timeout — a remote MCP round-trip may be slower than a localhost op.
_TOOL_CALL_TIMEOUT = 30.0


class Tool:
    """One entry of the discovery manifest (mcp-tools.md manifest shape)."""

    __slots__ = ("name", "mode", "endpoint", "transport")

    def __init__(self, name: str, mode: str, endpoint: str, transport: str):
        self.name = name
        self.mode = mode
        self.endpoint = endpoint
        self.transport = transport

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> Tool:
        return cls(
            name=d.get("name", ""),
            mode=d.get("mode", ""),
            endpoint=d.get("endpoint", ""),
            transport=d.get("transport", ""),
        )

    def __repr__(self) -> str:  # pragma: no cover - debug aid
        return f"Tool(name={self.name!r}, mode={self.mode!r}, endpoint={self.endpoint!r})"


class ToolsClient:
    """Tool discovery + invocation against the localhost plane."""

    def __init__(self, config: PlaneConfig):
        self._config = config

    # ── discovery ────────────────────────────────────────────────────────────
    def list(self) -> List[Tool]:
        """Return the live tool manifest (sidecar first, tools.json fallback)."""
        manifest = self._fetch_manifest()
        return [Tool.from_dict(t) for t in manifest.get("tools", [])]

    def _fetch_manifest(self) -> Dict[str, Any]:
        url = f"{self._config.discovery_base_url}/tools"
        try:
            resp = _http.request("GET", url, timeout=_MANIFEST_TIMEOUT, expect=(200,))
            data = resp.json()
            if isinstance(data, dict):
                return data
        except EndpointError:
            # Sidecar unreachable — fall through to the durable backing file.
            pass

        # Durable backing: the ConfigMap mount (cold-start only).
        try:
            with open(self._config.tools_json_path) as fh:
                data = json.load(fh)
            if isinstance(data, dict):
                return data
        except (OSError, json.JSONDecodeError):
            pass

        raise EndpointError(
            f"tool manifest unavailable: neither {url} nor "
            f"{self._config.tools_json_path} could be read"
        )

    def _find(self, name: str) -> Tool:
        for tool in self.list():
            if tool.name == name:
                return tool
        raise ConfigError(f"tool {name!r} is not in the current manifest")

    # ── invocation ───────────────────────────────────────────────────────────
    def call(self, name: str, **args: Any) -> Any:
        """Invoke a bound MCP tool by name; return its parsed result.

        Looks the tool up in the live manifest and performs an MCP
        ``tools/call`` over its ``streamable-http`` endpoint. The tool's text
        result is returned parsed as JSON when it is a JSON document, else as
        the raw string.
        """
        tool = self._find(name)
        if not tool.endpoint:
            raise ConfigError(f"tool {name!r} has no endpoint in the manifest")
        raw_text = _mcp_call_tool(tool.endpoint, name, args)
        try:
            return json.loads(raw_text)
        except (json.JSONDecodeError, TypeError):
            return raw_text


# ── minimal MCP streamable-http client (stdlib only) ───────────────────────────
#
# streamable-http transport (mcp-tools.md): the client POSTs JSON-RPC to the
# endpoint. The server responds either application/json or a text/event-stream
# SSE frame ("data: <json>"). The handshake is:
#   1. POST initialize        -> result + an Mcp-Session-Id response header
#   2. POST notifications/initialized (notification; no id, no response body)
#   3. POST tools/call        -> the tool result
# We keep this deliberately small — just enough to invoke a bound tool.


def _mcp_headers(session_id: Optional[str]) -> Dict[str, str]:
    headers = {
        "Content-Type": "application/json",
        # A streamable-http server may reply with either representation.
        "Accept": "application/json, text/event-stream",
    }
    if session_id:
        headers["Mcp-Session-Id"] = session_id
    return headers


def _mcp_post(
    endpoint: str,
    payload: Dict[str, Any],
    session_id: Optional[str],
    *,
    expect_body: bool,
) -> tuple[Optional[Dict[str, Any]], Optional[str]]:
    """POST one JSON-RPC message; return (parsed-result, session-id-header)."""
    resp = _http.request(
        "POST",
        endpoint,
        body=_http.json_body(payload),
        headers=_mcp_headers(session_id),
        timeout=_TOOL_CALL_TIMEOUT,
        expect=(200, 202),
    )
    new_session = resp.headers.get("mcp-session-id")
    if not expect_body:
        return None, new_session
    message = _parse_jsonrpc(resp)
    if "error" in message:
        err = message["error"]
        raise EndpointError(
            f"MCP error from {endpoint}: {err.get('message', err)}",
            status=resp.status,
        )
    return message.get("result"), new_session


def _parse_jsonrpc(resp: _http.Response) -> Dict[str, Any]:
    """Extract the JSON-RPC message from a JSON or SSE (text/event-stream) body."""
    content_type = resp.headers.get("content-type", "")
    text = resp.text()
    if "text/event-stream" in content_type:
        # SSE frames: lines beginning "data:"; take the last data payload.
        data_line = None
        for line in text.splitlines():
            if line.startswith("data:"):
                data_line = line[len("data:"):].strip()
        if data_line is None:
            raise EndpointError("MCP SSE response contained no data frame")
        text = data_line
    try:
        return json.loads(text)
    except json.JSONDecodeError as exc:
        raise EndpointError(f"MCP response was not valid JSON: {exc}") from exc


def _mcp_call_tool(endpoint: str, tool_name: str, arguments: Dict[str, Any]) -> str:
    """Full MCP handshake + tools/call; return the first text content item."""
    # 1. initialize.
    init_result, session_id = _mcp_post(
        endpoint,
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": _MCP_PROTOCOL_VERSION,
                "capabilities": {},
                "clientInfo": {"name": "ctxmesh", "version": "0.1.0"},
            },
        },
        session_id=None,
        expect_body=True,
    )
    _ = init_result  # negotiated capabilities unused for a single tools/call

    # 2. notifications/initialized (a notification: no id, no response expected).
    _mcp_post(
        endpoint,
        {"jsonrpc": "2.0", "method": "notifications/initialized"},
        session_id=session_id,
        expect_body=False,
    )

    # 3. tools/call.
    result, _ = _mcp_post(
        endpoint,
        {
            "jsonrpc": "2.0",
            "id": 2,
            "method": "tools/call",
            "params": {"name": tool_name, "arguments": arguments},
        },
        session_id=session_id,
        expect_body=True,
    )

    return _first_text_content(result, endpoint)


def _first_text_content(result: Optional[Dict[str, Any]], endpoint: str) -> str:
    """Pull the first text content item out of an MCP CallToolResult."""
    if not isinstance(result, dict):
        raise EndpointError(f"MCP tools/call at {endpoint} returned no result object")
    content = result.get("content")
    if not isinstance(content, list) or not content:
        raise EndpointError(f"MCP tools/call at {endpoint} returned empty content")
    first = content[0]
    if isinstance(first, dict) and "text" in first:
        return first["text"]
    raise EndpointError(f"MCP tools/call at {endpoint} returned non-text content")
