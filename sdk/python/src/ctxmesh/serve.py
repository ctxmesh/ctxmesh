"""``ctxmesh.serve`` — the agent serving scaffold (DX-3).

Every code-first agent used to hand-copy ~100 lines of ``BaseHTTPRequestHandler`` and,
worse, had to *know* the launcher runtime contract by heart: the ``POST /invoke`` body
shape, the ``/healthz``/``/readyz`` probes, the ``$AGENT_PORT`` the launcher proxies to,
the ``traceparent`` capture that roots the trace under ``agent.invoke``, the autonomous
conversation-id mint, and the SSE token envelope. Miss any one and the agent is subtly
broken — most dangerously, a hand-rolled loop that forgets :meth:`Client.request_scope`
relays NO run capability, silently downgrading every tool egress to org/public creds
(the DX-2 bug).

:func:`serve` encodes all of that ONCE:

    import ctxmesh

    def handle(req: ctxmesh.InvokeRequest) -> str:
        with req.client.trace.loop("my-agent", headers=req.headers) as root:
            answer = req.client.model.chat(route="gpt-4o-mini", messages=[
                {"role": "user", "content": req.input},
            ])
        return answer.text

    ctxmesh.serve(handle)          # binds request_scope + trace, serves the contract

* ``serve(handler)`` runs a CUSTOM loop: your ``handler(req)`` returns a ``str`` or a
  :class:`~ctxmesh.managed.ManagedResult`. ``serve`` parses the request, binds
  :meth:`Client.request_scope` (capability + granted approvals — the DX-2 fix) and roots
  the trace under the launcher's ``agent.invoke`` span, then renders the response envelope.
* ``serve()`` (no handler) runs the STOCK managed loop — the same behaviour the
  managed-agent image ships — reading its config from the environment. The image's
  entrypoint is now just ``ctxmesh.serve()``.

Streaming: when the caller sends ``Accept: text/event-stream`` the response is SSE and
``req.emit_token(text)`` streams a ``token`` frame; when it does not, ``emit_token`` is a
no-op and the JSON envelope is returned. Either way your handler code is identical — pass
``req.emit_token`` wherever your loop produces content deltas (the managed handler wires it
to ``on_token``).
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, Mapping, Optional, Union

from ctxmesh import agent as _agent_module
from ctxmesh.client import Client
from ctxmesh.managed import (
    ManagedConfig,
    ManagedResult,
    mint_conversation_id,
    run_managed_loop,
)

#: What a handler may return: a bare answer string, or a full ManagedResult (to carry
#: steps / tools_called / consent_required / approval_required into the envelope).
HandlerResult = Union[str, ManagedResult]

#: A serve handler: given the parsed :class:`InvokeRequest`, produce a :data:`HandlerResult`.
Handler = Callable[["InvokeRequest"], HandlerResult]


@dataclass
class InvokeRequest:
    """One parsed ``POST /invoke`` request handed to a :data:`Handler`.

    Everything a custom loop needs, already unpacked and bound — so the handler is pure
    business logic, never HTTP plumbing.
    """

    #: The user prompt (the ``"input"`` field of the request body).
    input: str
    #: The inbound request headers (the launcher injected ``traceparent`` +
    #: ``X-Conversation-Id``). Pass to ``client.trace.loop(headers=...)`` for a nice AGENT root.
    headers: Mapping[str, str]
    #: Approval keys the caller granted for this run (human-in-the-loop resume, m32.4);
    #: already bound via ``request_scope`` so ``pause_for_approval`` proceeds instead of pausing.
    approvals: list = field(default_factory=list)
    #: The L7 resume envelope (ADR 0091), present only when the platform re-invokes a supervisor that
    #: SUSPENDED on a delegate: the stock managed loop verifies it and continues from the checkpoint
    #: instead of starting fresh. ``None`` on every ordinary run. A custom handler may ignore it.
    checkpoint: Optional[Any] = None
    #: The conversation/thread id for this run: the inbound ``X-Conversation-Id`` (a console
    #: chat / A2A hop) or, for an autonomous run with no session, a freshly minted per-run id.
    conversation_id: Optional[str] = None
    #: The SDK client — its capability + approvals are already bound for the life of the handler
    #: call (``request_scope``), so ``client.tools.call``/``client.memory`` relay the user's grant.
    client: Optional[Client] = None
    #: Stream a content delta as an SSE ``token`` frame. A no-op when the caller did not request
    #: streaming, so the same handler works for both — wire it to your loop's on-token hook.
    emit_token: Callable[[str], None] = lambda _text: None
    #: Stream a ``step`` metadata frame (M78, ADR 0071 §4/§C3) — lightweight live step-visibility
    #: (step N, kind, tool, token counts, a fixture ref). A no-op when the caller did not request
    #: streaming, so the same handler works for both — wire it to your loop's on-step hook.
    emit_step: Callable[[dict], None] = lambda _frame: None


def _parse_body(raw: bytes) -> tuple:
    """Parse the /invoke body into ``(input, approvals, checkpoint)``. Tolerant like the reference
    entrypoint: non-JSON is treated as the raw prompt text (never a 500). *checkpoint* is the
    platform-owned L7 resume envelope (ADR 0091) the worker injects on a suspended supervisor's
    re-invoke — ``None`` on a fresh run; the managed loop re-verifies it before trusting it."""
    approvals: list = []
    try:
        body = json.loads(raw or b"{}")
    except json.JSONDecodeError:
        return raw.decode(errors="replace"), approvals, None
    if not isinstance(body, dict):
        return str(body), approvals, None
    raw_approvals = body.get("approvals")
    if isinstance(raw_approvals, list):
        approvals = [str(a) for a in raw_approvals]
    return str(body.get("input", "")), approvals, body.get("checkpoint")


def _autonomous_conversation_id(headers: Mapping[str, str]) -> Optional[str]:
    """Return a minted per-run conversation id (m33.5) when the caller supplied NO session, else
    None — so an inbound ``X-Conversation-Id`` (console chat / A2A hop) takes precedence. Each
    autonomous run is thus its own thread/trace. Case-insensitive."""
    for key, value in headers.items():
        if key.lower() == "x-conversation-id" and (value or "").strip():
            return None
    return mint_conversation_id()


def _envelope(agent_name: str, result: HandlerResult) -> Dict[str, Any]:
    """The ``/invoke`` response envelope (identical shape to every stock agent). A bare ``str``
    return is wrapped as a single-step answer; ``consent_required`` drives the "Connect your
    account" CTA and ``approval_required`` the human-in-the-loop prompt."""
    if isinstance(result, str):
        result = ManagedResult(output=result, steps=1, tools_called=[])
    body: Dict[str, Any] = {
        "agent": agent_name,
        "output": result.output,
        "steps": result.steps,
        "tools_called": list(result.tools_called),
        "consent_required": list(result.consent_required),
    }
    if result.approval_required:
        body["approval_required"] = result.approval_required
    if result.handoff:
        # The agent TRANSFERRED the conversation (M67, ADR 0060 §5). The BFF handoff edge already
        # terminated this run + created the target's; surface the marker so the BFF's executeRun
        # does not append an empty answer over the recorded handoff outcome (and the console can
        # render the transfer).
        body["handoff"] = result.handoff
    if result.delegate_waiting:
        # The supervisor SUSPENDED on a delegate (L7, ADR 0091): carry the loop checkpoint + the
        # delegate intents so the BFF worker enacts child-create + parent→waiting in one transaction.
        body["delegate_waiting"] = result.delegate_waiting
    return body


def process_invoke(
    client: Client,
    handler: Handler,
    agent_name: str,
    raw_body: bytes,
    headers: Mapping[str, str],
    *,
    on_token: Optional[Callable[[str], None]] = None,
    on_step: Optional[Callable[[Dict[str, Any]], None]] = None,
) -> Dict[str, Any]:
    """Parse one /invoke, bind the run scope + trace, run *handler*, and return the envelope.

    This is the pure core of :func:`serve` (no sockets), so the contract is unit-testable. It
    binds :meth:`Client.request_scope` (capability + approvals — DX-2) AND roots the trace under
    ``agent.invoke`` (``trace.request_context``) around the handler call; both are idempotent with
    the managed loop, which re-binds them. Errors from the handler propagate (the HTTP layer maps
    them to a 502 / SSE ``error`` frame) — never swallowed.
    """
    user_input, approvals, checkpoint = _parse_body(raw_body)
    conversation_id = _autonomous_conversation_id(headers)
    req = InvokeRequest(
        input=user_input,
        headers=dict(headers),
        approvals=approvals,
        checkpoint=checkpoint,
        conversation_id=conversation_id,
        client=client,
        emit_token=on_token or (lambda _text: None),
        emit_step=on_step or (lambda _frame: None),
    )
    with client.trace.request_context(headers), client.request_scope(
        headers, approvals=approvals
    ):
        result = handler(req)
    return _envelope(agent_name, result)


def _managed_handler(config: ManagedConfig) -> Handler:
    """The default handler: the stock config-driven tool-calling loop (:func:`run_managed_loop`).
    Wiring ``on_token`` to ``emit_token`` gives the managed agent its token stream for free."""

    def handle(req: InvokeRequest) -> ManagedResult:
        return run_managed_loop(
            req.client,
            config,
            req.input,
            headers=req.headers,
            approvals=req.approvals,
            conversation_id=req.conversation_id,
            on_token=req.emit_token,
            on_step=req.emit_step,
            checkpoint=req.checkpoint,
        )

    return handle


def _make_request_handler(client: Client, handler: Handler, agent_name: str):
    """Build the BaseHTTPRequestHandler subclass bound to *handler* — the thin HTTP adapter over
    :func:`process_invoke`. Kept local so the handler closes over the client/handler/name."""

    class _Handler(BaseHTTPRequestHandler):
        def _send_json(self, code: int, body: Dict[str, Any]) -> None:
            payload = json.dumps(body).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def do_GET(self) -> None:  # noqa: N802 - stdlib dispatch name
            if self.path in ("/healthz", "/readyz"):
                self._send_json(200, {"status": "ok"})
            else:
                self._send_json(404, {"error": "not found"})

        def do_POST(self) -> None:  # noqa: N802 - stdlib dispatch name
            if self.path != "/invoke":
                self._send_json(404, {"error": "not found"})
                return
            length = int(self.headers.get("Content-Length", "0"))
            raw = self.rfile.read(length) if length else b"{}"
            if "text/event-stream" in (self.headers.get("Accept") or ""):
                self._stream(raw)
                return
            try:
                body = process_invoke(client, handler, agent_name, raw, dict(self.headers))
                self._send_json(200, body)
            except Exception as exc:  # noqa: BLE001 - report, never crash the server
                self._send_json(502, {"agent": agent_name, "error": str(exc)})

        def _stream(self, raw: bytes) -> None:
            """Stream token frames then a terminal ``done``/``error`` frame (m32.7): the source the
            BFF republishes into the run event stream."""
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()

            def emit(obj: Dict[str, Any]) -> None:
                self.wfile.write(f"data: {json.dumps(obj)}\n\n".encode())
                self.wfile.flush()

            try:
                body = process_invoke(
                    client,
                    handler,
                    agent_name,
                    raw,
                    dict(self.headers),
                    on_token=lambda text: emit({"type": "token", "text": text}),
                    # Step-visibility (M78, ADR 0071 §4/§C3): each `step` metadata frame the
                    # managed loop emits becomes an SSE `step` frame the BFF republishes onto the
                    # run stream.
                    on_step=lambda frame: emit({"type": "step", **frame}),
                )
                body["type"] = "done"
                emit(body)
            except Exception as exc:  # noqa: BLE001 - report as an error frame, never crash
                emit({"type": "error", "agent": agent_name, "error": str(exc)})

        def log_message(self, *_args: Any) -> None:  # quiet default access logging
            pass

    return _Handler


def serve(
    handler: Optional[Handler] = None,
    *,
    client: Optional[Client] = None,
    agent_name: Optional[str] = None,
    port: Optional[int] = None,
    host: str = "0.0.0.0",  # noqa: S104 - bind all: the pod's only listener, fronted by the launcher
) -> None:
    """Serve an agent over the launcher runtime contract — the one call a code-first agent needs.

    *handler* is your loop (``handler(req) -> str | ManagedResult``); omit it to run the stock
    managed loop (config from the environment). *client* defaults to :func:`ctxmesh.agent.from_env`
    (fails fast with ``NotInPodError`` if the launcher env is absent — a start-up error, not a
    silent first-request one). *agent_name* / *port* default to ``$AGENT_NAME`` / ``$AGENT_PORT``
    (the port the launcher proxies to). Blocks serving forever.

    Endpoints: ``POST /invoke`` (``{"input", "approvals"?}`` → the response envelope, or SSE when
    ``Accept: text/event-stream``), ``GET /healthz``/``/readyz`` → 200.
    """
    if client is None:
        client = _agent_module.from_env()
    if handler is None:
        handler = _managed_handler(ManagedConfig.from_env())
    if agent_name is None:
        agent_name = os.environ.get("AGENT_NAME", "agent")
    if port is None:
        port = int(os.environ.get("AGENT_PORT", "8081"))

    request_handler = _make_request_handler(client, handler, agent_name)
    server = ThreadingHTTPServer((host, port), request_handler)
    print(f"ctxmesh agent {agent_name!r} listening on :{port}", flush=True)
    server.serve_forever()
