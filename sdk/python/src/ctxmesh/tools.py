"""Tools client — the discovery sidecar (:2999) + MCP tool invocation (M4).

``list()`` fetches the live manifest from the discovery sidecar
(``GET localhost:2999/tools``), falling back to the mounted ``tools.json``
(cold-start backing) when the sidecar is unreachable — the same precedence the
raw agent uses (mcp-tools.md "Agent consumption").

``call(name, **args)`` looks the tool up in the live manifest, then invokes it
over its MCP ``streamable-http`` endpoint. The endpoint is taken verbatim from
the manifest (it already ends in ``/mcp`` per the m4.4 finding). We speak the
MCP wire directly over stdlib rather than pulling the heavyweight ``mcp``
package in as a runtime dep — the SDK stays lean and 3.9-compatible.

**Catalog name vs MCP tool name.** The discovery manifest name is the
*ToolRegistry catalog key* (e.g. ``word-count``, hyphen), which is NOT
necessarily the name the MCP server exposes the tool under (e.g. ``word_count``,
underscore — a FastMCP function name). ``toolmanifest.Tool`` carries only the
catalog name/endpoint, so ``call`` must discover the real MCP name from the
server: it does the handshake, runs ``tools/list``, resolves the catalog name to
a server tool name (exact → hyphen/underscore-normalized → sole-tool fallback),
and only then calls with the *resolved* name. (Carrying the MCP name in the
manifest is a phase-2 M4 item; the SDK resolves it at call time for now.)
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
    """One entry of the discovery manifest (mcp-tools.md manifest shape).

    ``input_schema`` is the tool's argument JSON Schema as the manifest carries it
    (the ``inputSchema`` key), captured from the MCP server's ``tools/list`` and
    stored on the ToolRegistry entry (m14.6, plumbed through in m14.6b). It is the
    parsed JSON object *verbatim* — the managed loop advertises it to the model as
    the tool's ``parameters`` so the model produces correct ``arguments``. It is
    ``None`` when the manifest omits it (a curated/legacy entry with no captured
    schema); the loop then falls back to a permissive object-parameters schema.
    """

    __slots__ = ("name", "mode", "endpoint", "transport", "input_schema")

    def __init__(
        self,
        name: str,
        mode: str,
        endpoint: str,
        transport: str,
        input_schema: Optional[Dict[str, Any]] = None,
    ):
        self.name = name
        self.mode = mode
        self.endpoint = endpoint
        self.transport = transport
        self.input_schema = input_schema

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> Tool:
        # The manifest carries inputSchema verbatim as a JSON object. Anything
        # that is not a JSON object (absent, null, or a malformed non-object) is
        # treated as "no schema" so the loop takes the permissive fallback rather
        # than handing the model a schema it can't use.
        raw_schema = d.get("inputSchema")
        input_schema = raw_schema if isinstance(raw_schema, dict) else None
        return cls(
            name=d.get("name", ""),
            mode=d.get("mode", ""),
            endpoint=d.get("endpoint", ""),
            transport=d.get("transport", ""),
            input_schema=input_schema,
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
        """Invoke a bound MCP tool by its *catalog* name; return its result.

        *name* is the discovery-manifest catalog key. The client resolves the
        endpoint from the manifest, then resolves the real MCP tool name via the
        server's ``tools/list`` (catalog names may differ from MCP names — e.g.
        ``word-count`` vs ``word_count``) before issuing ``tools/call``. The
        tool's text result is returned parsed as JSON when it is a JSON document,
        else as the raw string.
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
# SSE frame ("data: <json>"). The sequence is:
#   1. POST initialize        -> result + an Mcp-Session-Id response header
#   2. POST notifications/initialized (notification; no id, no response body)
#   3. POST tools/list        -> the server's real tool names (to resolve the
#                                catalog name -> the MCP name)
#   4. POST tools/call        -> the tool result (with the resolved MCP name)
# We keep this deliberately small — just enough to discover + invoke a tool.


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


def _mcp_call_tool(endpoint: str, catalog_name: str, arguments: Dict[str, Any]) -> str:
    """Full MCP session: handshake -> tools/list -> resolve name -> tools/call.

    *catalog_name* is the discovery-manifest key; the actual MCP tool name is
    discovered from the server and may differ (see the module docstring). Returns
    the first text content item of the tool result.
    """
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

    # 3. tools/list -> discover the server's real tool names.
    list_result, _ = _mcp_post(
        endpoint,
        {"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}},
        session_id=session_id,
        expect_body=True,
    )
    server_names = _server_tool_names(list_result, endpoint)
    mcp_name = _resolve_tool_name(catalog_name, server_names, endpoint)

    # 4. tools/call with the RESOLVED MCP name.
    result, _ = _mcp_post(
        endpoint,
        {
            "jsonrpc": "2.0",
            "id": 3,
            "method": "tools/call",
            "params": {"name": mcp_name, "arguments": arguments},
        },
        session_id=session_id,
        expect_body=True,
    )

    return _first_text_content(result, endpoint)


def _server_tool_names(list_result: Optional[Dict[str, Any]], endpoint: str) -> List[str]:
    """Extract the tool-name list from an MCP tools/list result."""
    if not isinstance(list_result, dict):
        raise EndpointError(f"MCP tools/list at {endpoint} returned no result object")
    tools = list_result.get("tools")
    if not isinstance(tools, list):
        raise EndpointError(f"MCP tools/list at {endpoint} returned no tools array")
    names = [t.get("name", "") for t in tools if isinstance(t, dict) and t.get("name")]
    if not names:
        raise EndpointError(f"MCP server at {endpoint} advertises no tools")
    return names


def _normalize(name: str) -> str:
    """Fold hyphen/underscore so catalog `word-count` matches MCP `word_count`."""
    return name.replace("-", "_")


def _resolve_tool_name(catalog_name: str, server_names: List[str], endpoint: str) -> str:
    """Map a catalog name to a server MCP tool name.

    Precedence: exact match -> hyphen/underscore-normalized match -> if the
    server advertises exactly one tool, use it -> otherwise raise a clear error
    listing what the server actually exposes.
    """
    # 1. Exact.
    if catalog_name in server_names:
        return catalog_name
    # 2. Normalized (hyphen<->underscore). Only accept an unambiguous match.
    target = _normalize(catalog_name)
    normalized_matches = [n for n in server_names if _normalize(n) == target]
    if len(normalized_matches) == 1:
        return normalized_matches[0]
    # 3. Sole-tool fallback.
    if len(server_names) == 1:
        return server_names[0]
    # 4. Give up with an actionable error.
    raise ConfigError(
        f"tool {catalog_name!r} could not be resolved to an MCP tool at "
        f"{endpoint}: the server exposes {server_names!r}. "
        f"(The discovery-catalog name may differ from the MCP tool name; a "
        f"normalized match was ambiguous or absent.)"
    )


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
