"""``ctxmesh.testing`` — offline fakes of the launcher localhost plane (DX-1).

Test a ctxmesh agent with **no cluster and no launcher**: these tiny ``http.server``
stubs stand in for the real localhost plane, so `pip install ctxmesh` alone is enough to
unit-test a handler or a custom loop. Pair them with
:meth:`ctxmesh.PlaneConfig.for_test` + :func:`ctxmesh.agent.from_config`:

    from ctxmesh import agent
    from ctxmesh.config import PlaneConfig, RunContext
    from ctxmesh.testing import MemoryStub, DiscoveryStub, GatewayStub

    with MemoryStub() as mem, DiscoveryStub() as disc, GatewayStub(content="hi") as gw:
        cfg = PlaneConfig.for_test(
            memory_base_url=mem.base_url,
            discovery_base_url=disc.base_url,
            model_gateway_url=gw.base_url,
            run=RunContext(agent_name="my-agent", conversation_id=""),
        )
        client = agent.from_config(cfg)
        assert client.model.chat("gpt-4o-mini", [{"role": "user", "content": "q"}]).text == "hi"

Each stub binds an ephemeral localhost port (``base_url``) and records every request it
received (method, path, body, headers) so a test can assert the client hit the right
endpoint with the right run context (conversationId header, agent identity). The plane the
stubs fake:

    MemoryStub    -> the :2998 M5 contract (get/put/append/search)
    DiscoveryStub -> the :2999 M4 contract (/tools) + a faithful inline MCP tool endpoint
    GatewayStub   -> the OpenAI-compatible model gateway (POST /chat/completions)
    FeedbackStub  -> the :2995 M9 contract (POST /feedback, 202/400/502)

This is the SAME module the SDK's own contract tests use (``tests/launcher_stub.py`` now
re-exports it) — shipping it means an external author gets the exact fakes the SDK is
validated against, not a re-implementation.
"""

from __future__ import annotations

import json
import threading
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, List, Optional
from urllib.parse import parse_qs, urlparse

__all__ = [
    "RecordedRequest",
    "MemoryStub",
    "DiscoveryStub",
    "GatewayStub",
    "FeedbackStub",
    "MeshStub",
]


@dataclass
class RecordedRequest:
    method: str
    path: str
    query: Dict[str, List[str]]
    headers: Dict[str, str]
    body: bytes

    def json(self) -> Any:
        return json.loads(self.body) if self.body else None


@dataclass
class _StubState:
    requests: List[RecordedRequest] = field(default_factory=list)
    # route -> handler(state, request) -> (status, headers, body-bytes)
    routes: Dict[str, Callable[["_StubState", RecordedRequest], Any]] = field(default_factory=dict)


class _BaseStub:
    """A threaded HTTP server on an ephemeral localhost port."""

    def __init__(self) -> None:
        self.state = _StubState()
        self._install_routes()
        handler_cls = self._make_handler()
        self._server = ThreadingHTTPServer(("127.0.0.1", 0), handler_cls)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    def _install_routes(self) -> None:  # overridden by subclasses
        raise NotImplementedError

    @property
    def base_url(self) -> str:
        host, port = self._server.server_address
        return f"http://{host}:{port}"

    @property
    def requests(self) -> List[RecordedRequest]:
        return self.state.requests

    def __enter__(self) -> "_BaseStub":
        self._thread.start()
        return self

    def __exit__(self, *exc: Any) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=2)

    def _make_handler(self):
        state = self.state

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_args: Any) -> None:  # silence test noise
                pass

            def _dispatch(self) -> None:
                length = int(self.headers.get("Content-Length", 0) or 0)
                body = self.rfile.read(length) if length else b""
                parsed = urlparse(self.path)
                req = RecordedRequest(
                    method=self.command,
                    path=parsed.path,
                    query=parse_qs(parsed.query),
                    headers={k.lower(): v for k, v in self.headers.items()},
                    body=body,
                )
                state.requests.append(req)

                # Route key is "METHOD <path-template>"; the memory paths carry a
                # conversationId segment, so match on a normalised template.
                route = _match_route(state.routes, req)
                if route is None:
                    self._respond(404, {}, b'{"error":"no stub route"}')
                    return
                status, headers, resp_body = route(state, req)
                self._respond(status, headers, resp_body)

            def _respond(self, status: int, headers: Dict[str, str], body: bytes) -> None:
                self.send_response(status)
                for key, value in headers.items():
                    self.send_header(key, value)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                if body:
                    self.wfile.write(body)

            do_GET = _dispatch
            do_PUT = _dispatch
            do_POST = _dispatch

        return Handler


def _normalise(path: str) -> str:
    """Collapse a concrete memory path to its route template.

    /memory/conv-42          -> /memory/{id}
    /memory/conv-42/append   -> /memory/{id}/append
    /memory/conv-42/search   -> /memory/{id}/search
    """
    parts = path.strip("/").split("/")
    if len(parts) >= 2 and parts[0] == "memory":
        tail = "/" + "/".join(parts[2:]) if len(parts) > 2 else ""
        return "/memory/{id}" + tail
    # /a2a/research -> /a2a/{target} (the mesh listener, M156)
    if len(parts) == 2 and parts[0] == "a2a":
        return "/a2a/{target}"
    return path


def _match_route(routes: Dict[str, Any], req: RecordedRequest):
    key = f"{req.method} {_normalise(req.path)}"
    return routes.get(key)


# ── memory (:2998) ─────────────────────────────────────────────────────────────
class MemoryStub(_BaseStub):
    """In-memory fake of the M5 :2998 contract (per-conversationId list store)."""

    def __init__(self) -> None:
        self.store: Dict[str, List[Any]] = {}
        super().__init__()

    def _conv_id(self, req: RecordedRequest) -> str:
        return req.path.strip("/").split("/")[1]

    def _install_routes(self) -> None:
        def get(state: _StubState, req: RecordedRequest):
            entries = self.store.get(self._conv_id(req), [])
            return 200, {"Content-Type": "application/json"}, json.dumps(entries).encode()

        def put(state: _StubState, req: RecordedRequest):
            self.store[self._conv_id(req)] = json.loads(req.body)
            return 204, {}, b""

        def append(state: _StubState, req: RecordedRequest):
            self.store.setdefault(self._conv_id(req), []).append(json.loads(req.body))
            return 204, {}, b""

        def search(state: _StubState, req: RecordedRequest):
            q = req.query.get("q", [""])[0]
            entries = self.store.get(self._conv_id(req), [])
            matches = [e for e in entries if q == "" or q in json.dumps(e)]
            return 200, {"Content-Type": "application/json"}, json.dumps(matches).encode()

        self.state.routes.update(
            {
                "GET /memory/{id}": get,
                "PUT /memory/{id}": put,
                "POST /memory/{id}/append": append,
                "GET /memory/{id}/search": search,
            }
        )


# ── discovery (:2999) + fake MCP tool endpoint ────────────────────────────────
class DiscoveryStub(_BaseStub):
    """Fake of the M4 :2999 /tools manifest and an inline MCP tool endpoint.

    ``mcp_endpoint`` serves a faithful streamable-http MCP server: it answers
    ``initialize`` (with an Mcp-Session-Id header), the ``initialized``
    notification, ``tools/list`` (advertising the server's REAL tool names), and
    ``tools/call`` — which, like the real FastMCP echo server, VALIDATES
    ``params.name`` against the advertised tools and returns a JSON-RPC error for
    an unknown name. This is the guard against a false-green: the discovery
    catalog name (``word-count``, hyphen) deliberately differs from the MCP tool
    name (``word_count``, underscore), so a client that fails to resolve the real
    name gets rejected here, exactly as it would in the real deployment.
    """

    #: The catalog (ToolRegistry) key advertised in the discovery manifest.
    CATALOG_NAME = "word-count"
    #: The name the MCP server actually exposes the tool under (FastMCP fn name).
    MCP_TOOL_NAME = "word_count"

    def __init__(
        self,
        tool_result: Optional[Dict[str, Any]] = None,
        manifest_input_schema: Optional[Dict[str, Any]] = None,
    ) -> None:
        default_result = {"count": 3, "server_version": "v1"}
        self.tool_result = tool_result if tool_result is not None else default_result
        #: When set, the discovery manifest advertises this inputSchema on the
        #: tool entry (the m14.6b propagation the managed loop consumes). None →
        #: the manifest omits inputSchema (the schema-less / permissive path).
        self.manifest_input_schema = manifest_input_schema
        #: params of every ACCEPTED tools/call (unknown names are rejected).
        self.mcp_calls: List[Dict[str, Any]] = []
        #: names of tools/list responses served (proof the client discovered).
        self.list_calls = 0
        self._session_counter = 0
        super().__init__()

    @property
    def mcp_endpoint(self) -> str:
        return f"{self.base_url}/mcp"

    def _manifest(self) -> Dict[str, Any]:
        tool: Dict[str, Any] = {
            "name": self.CATALOG_NAME,
            "mode": "remote",
            "endpoint": self.mcp_endpoint,
            "transport": "streamable-http",
        }
        if self.manifest_input_schema is not None:
            tool["inputSchema"] = self.manifest_input_schema
        return {"version": "stub0001", "tools": [tool]}

    def _server_tools(self) -> List[Dict[str, Any]]:
        """The MCP server's advertised tools (underscore name, unlike the catalog)."""
        return [
            {
                "name": self.MCP_TOOL_NAME,
                "description": "Count whitespace-separated words.",
                "inputSchema": {
                    "type": "object",
                    "properties": {"text": {"type": "string"}},
                    "required": ["text"],
                },
            }
        ]

    def _install_routes(self) -> None:
        def tools(state: _StubState, req: RecordedRequest):
            return 200, {"Content-Type": "application/json"}, json.dumps(self._manifest()).encode()

        def mcp_redirect(state: _StubState, req: RecordedRequest):
            """Mirrors FastMCP/Starlette: POST /mcp (no trailing slash) → 307 /mcp/."""
            location = f"{self.base_url}/mcp/"
            return 307, {"Location": location}, b""

        def mcp(state: _StubState, req: RecordedRequest):
            msg = json.loads(req.body)
            method = msg.get("method")
            if method == "initialize":
                self._session_counter += 1
                sid = f"sess-{self._session_counter}"
                result = {
                    "jsonrpc": "2.0",
                    "id": msg.get("id"),
                    "result": {"protocolVersion": "2025-03-26", "capabilities": {}},
                }
                return (
                    200,
                    {"Content-Type": "application/json", "Mcp-Session-Id": sid},
                    json.dumps(result).encode(),
                )
            if method == "notifications/initialized":
                return 202, {}, b""
            if method == "tools/list":
                self.list_calls += 1
                result = {
                    "jsonrpc": "2.0",
                    "id": msg.get("id"),
                    "result": {"tools": self._server_tools()},
                }
                return 200, {"Content-Type": "application/json"}, json.dumps(result).encode()
            if method == "tools/call":
                params = msg["params"]
                # Validate the name like the real server: an unknown tool is a
                # JSON-RPC error, NOT a silent success. This is what makes a
                # client that sends the wrong (catalog) name fail the test. We
                # check against the tools this server currently advertises (so a
                # test that overrides _server_tools stays consistent).
                advertised = {t["name"] for t in self._server_tools()}
                if params.get("name") not in advertised:
                    err = {
                        "jsonrpc": "2.0",
                        "id": msg.get("id"),
                        "error": {
                            "code": -32602,
                            "message": f"Unknown tool: {params.get('name')!r}",
                        },
                    }
                    return 200, {"Content-Type": "application/json"}, json.dumps(err).encode()
                self.mcp_calls.append(params)
                result = {
                    "jsonrpc": "2.0",
                    "id": msg.get("id"),
                    "result": {"content": [{"type": "text", "text": json.dumps(self.tool_result)}]},
                }
                # Exercise the SSE branch too: reply as text/event-stream.
                sse = f"event: message\ndata: {json.dumps(result)}\n\n"
                return 200, {"Content-Type": "text/event-stream"}, sse.encode()
            return 400, {}, b'{"error":"unknown method"}'

        self.state.routes.update(
            {
                "GET /tools": tools,
                # POST /mcp (no trailing slash) → 307 /mcp/, mirroring FastMCP/Starlette.
                # The manifest carries the endpoint WITHOUT the trailing slash
                # (as discovered in m4.4), so the SDK's first MCP POST always
                # hits this redirect; the fix in _http.py must follow it.
                "POST /mcp": mcp_redirect,
                # The real MCP handshake lives at /mcp/ (with trailing slash).
                "POST /mcp/": mcp,
            }
        )


# ── model gateway ($MODEL_GATEWAY_URL) ─────────────────────────────────────────
class GatewayStub(_BaseStub):
    """Fake of the OpenAI-compatible model gateway (POST /chat/completions).

    Returns a canned OpenAI-style completion with a ``usage`` block so the model
    client can extract the completion text and stamp token counts on the LLM
    span. ``force_status`` drives the error paths (a budget 402 / upstream 502).
    Records each request so a test can assert the body ({model, messages, ...opts})
    and the Authorization header.
    """

    def __init__(
        self,
        *,
        content: str = "the answer is 42",
        usage: Optional[Dict[str, int]] = None,
        model: str = "gpt-4o-mini",
        force_status: Optional[int] = None,
    ) -> None:
        self.content = content
        self.usage = (
            usage
            if usage is not None
            else {
                "prompt_tokens": 11,
                "completion_tokens": 5,
                "total_tokens": 16,
            }
        )
        self.model = model
        self.force_status = force_status
        super().__init__()

    def _install_routes(self) -> None:
        def completions(state: _StubState, req: RecordedRequest):
            if self.force_status is not None:
                return self.force_status, {}, b"gateway error\n"
            body = {
                "id": "chatcmpl-stub",
                "model": self.model,
                "choices": [
                    {"index": 0, "message": {"role": "assistant", "content": self.content}}
                ],
                "usage": self.usage,
            }
            return 200, {"Content-Type": "application/json"}, json.dumps(body).encode()

        self.state.routes.update({"POST /chat/completions": completions})


# ── feedback (:2995) ───────────────────────────────────────────────────────────
class FeedbackStub(_BaseStub):
    """Fake of the M9 :2995 feedback hook (202 on valid, 400/502 configurable)."""

    def __init__(self, *, force_status: Optional[int] = None) -> None:
        #: When set, every POST returns this status (to exercise the 502 path).
        self.force_status = force_status
        super().__init__()

    def _install_routes(self) -> None:
        def feedback(state: _StubState, req: RecordedRequest):
            if self.force_status is not None:
                return self.force_status, {}, b"forced error\n"
            try:
                payload = json.loads(req.body)
            except json.JSONDecodeError:
                return 400, {}, b"malformed JSON\n"
            if not payload.get("traceId"):
                return 400, {}, b"traceId is required\n"
            return 202, {}, b""

        self.state.routes.update({"POST /feedback": feedback})


class MeshStub(_BaseStub):
    """Fake of the launcher A2A listener (``POST /a2a/{targetAgent}``, :2997).

    The real launcher stamps the platform envelope, resolves the target over DNS and
    forwards. A stub imitates none of that and should not pretend to. What it CAN stand in
    for is the contract the SDK sees: a target that resolves returns the peer's JSON, and a
    target the launcher refuses comes back as a typed status.

    ``deny`` drives the refusal paths, which are the interesting ones — a mediated mesh
    exists in order to say no, so an agent that never exercises its refusal handling has not
    exercised the mesh at all.
    """

    def __init__(
        self,
        *,
        response: Optional[Dict[str, Any]] = None,
        deny: Optional[Dict[str, int]] = None,
    ) -> None:
        self.response = (
            response if response is not None else {"ok": True, "answer": "from the peer"}
        )
        #: target -> the status the launcher would answer with: 403 caller_not_allowed
        #: or cross_registry_denied, 404 unknown_target, 502 blocked.
        self.deny = deny or {}
        super().__init__()

    def _install_routes(self) -> None:
        def call(_state: "_StubState", req: RecordedRequest):
            target = req.path.rstrip("/").rsplit("/", 1)[-1]
            status = self.deny.get(target)
            if status:
                reason = {403: "caller_not_allowed", 404: "unknown_target"}.get(
                    status, "upstream_failure"
                )
                body = json.dumps({"error": reason}).encode()
                return status, {"Content-Type": "application/json"}, body
            return 200, {"Content-Type": "application/json"}, json.dumps(self.response).encode()

        self.state.routes.update({"POST /a2a/{target}": call})
