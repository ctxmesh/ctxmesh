"""Model client tests: OpenAI-compatible gateway call + the emitted LLM span.

Against the :class:`GatewayStub` (a fake ``POST /chat/completions``) these assert
the client (a) sends the right OpenAI-style body incl. ``**opts`` passthrough and
an Authorization header, (b) returns the completion text + usage, and (c) emits an
OpenInference ``LLM`` span with ``llm.model_name`` and the token counts from the
response ``usage``. Error statuses (a budget 402 / upstream 502) surface as an
EndpointError, never swallowed.
"""

from __future__ import annotations

import json

import pytest
from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
    InMemorySpanExporter,
)

from ctxmesh import agent
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError, EndpointError

from .launcher_stub import GatewayStub, RecordedRequest, _BaseStub, _StubState


def _client(gateway_stub: GatewayStub):
    cfg = PlaneConfig.for_test(model_gateway_url=gateway_stub.base_url)
    return agent.from_config(cfg)


def test_chat_posts_openai_body_and_returns_completion(gateway_stub: GatewayStub):
    client = _client(gateway_stub)
    resp = client.model.chat(
        "gpt-4o-mini",
        [{"role": "user", "content": "q"}],
        temperature=0.2,
    )

    # returned value
    assert resp.text == "the answer is 42"
    assert resp.usage == {"prompt_tokens": 11, "completion_tokens": 5, "total_tokens": 16}
    assert str(resp) == "the answer is 42"

    # request the gateway saw
    req = gateway_stub.requests[-1]
    assert req.method == "POST"
    assert req.path == "/chat/completions"
    assert req.headers.get("content-type") == "application/json"
    assert req.headers.get("authorization", "").startswith("Bearer ")
    body = json.loads(req.body)
    assert body["model"] == "gpt-4o-mini"
    assert body["messages"] == [{"role": "user", "content": "q"}]
    assert body["temperature"] == 0.2  # **opts passthrough


def test_chat_emits_llm_span_with_model_and_token_counts(
    gateway_stub: GatewayStub, span_exporter: InMemorySpanExporter
):
    client = _client(gateway_stub)
    client.model.chat("gpt-4o-mini", [{"role": "user", "content": "q"}])

    spans = [s for s in span_exporter.get_finished_spans() if s.name.startswith("chat ")]
    assert len(spans) == 1
    span = spans[0]
    assert span.attributes["openinference.span.kind"] == "LLM"
    assert span.attributes["llm.model_name"] == "gpt-4o-mini"
    assert span.attributes["llm.token_count.prompt"] == 11
    assert span.attributes["llm.token_count.completion"] == 5
    assert span.attributes["llm.token_count.total"] == 16
    assert span.attributes["output.value"] == "the answer is 42"


def test_unwired_gateway_raises_config_error():
    # No model_gateway_url (not in a pod): a clear ConfigError, never a silent
    # no-op or a call to a dead address.
    cfg = PlaneConfig.for_test(model_gateway_url="")
    client = agent.from_config(cfg)
    with pytest.raises(ConfigError):
        client.model.chat("m", [{"role": "user", "content": "q"}])


def test_gateway_502_surfaces_not_swallowed(gateway_stub: GatewayStub):
    gateway_stub.force_status = 502
    client = _client(gateway_stub)
    with pytest.raises(EndpointError) as exc:
        client.model.chat("m", [{"role": "user", "content": "q"}])
    assert exc.value.status == 502


def test_gateway_402_over_budget_surfaces(gateway_stub: GatewayStub):
    # The launcher budget proxy 402s when over cap — the client must surface it.
    gateway_stub.force_status = 402
    client = _client(gateway_stub)
    with pytest.raises(EndpointError) as exc:
        client.model.chat("m", [{"role": "user", "content": "q"}])
    assert exc.value.status == 402


def test_non_list_messages_rejected_client_side(gateway_stub: GatewayStub):
    client = _client(gateway_stub)
    with pytest.raises(ConfigError):
        client.model.chat("m", "not-a-list")  # type: ignore[arg-type]


def test_timeout_opt_does_not_leak_into_request_body(gateway_stub: GatewayStub):
    # `timeout` is a client-side transport concern: it must set the HTTP timeout
    # and NOT be sent to the gateway as an OpenAI body field.
    client = _client(gateway_stub)
    client.model.chat("m", [{"role": "user", "content": "q"}], timeout=1.5, temperature=0.1)
    body = json.loads(gateway_stub.requests[-1].body)
    assert "timeout" not in body
    assert body["temperature"] == 0.1  # other opts still pass through


# ── run-capability on the model call (m66.7, per-user OBO limits) ──────────────
#
# The model call carries the invoking user's run capability (the SAME one tools.py
# attaches to every MCP egress) so the launcher gateway can VERIFY the user and
# enforce per-user rate/abuse limits. In scope ⇒ the header is attached; out of
# scope (unattended/offline) ⇒ it is omitted, leaving Content-Type/Authorization
# untouched.


def test_chat_attaches_run_capability_when_in_scope(gateway_stub: GatewayStub):
    from ctxmesh._capability import CAPABILITY_HEADER, capability_scope

    client = _client(gateway_stub)
    with capability_scope({CAPABILITY_HEADER: "cap-token-123"}):
        client.model.chat("m", [{"role": "user", "content": "q"}])
    req = gateway_stub.requests[-1]
    # RecordedRequest.headers are lower-cased.
    assert req.headers.get(CAPABILITY_HEADER.lower()) == "cap-token-123"
    # The pre-existing contract is untouched.
    assert req.headers.get("content-type") == "application/json"
    assert req.headers.get("authorization", "").startswith("Bearer ")


def test_chat_omits_run_capability_when_not_in_scope(gateway_stub: GatewayStub):
    from ctxmesh._capability import CAPABILITY_HEADER

    client = _client(gateway_stub)
    # No capability_scope active ⇒ an unattended/offline run.
    client.model.chat("m", [{"role": "user", "content": "q"}])
    req = gateway_stub.requests[-1]
    assert CAPABILITY_HEADER.lower() not in req.headers
    # Content-Type/Authorization are still present.
    assert req.headers.get("content-type") == "application/json"
    assert req.headers.get("authorization", "").startswith("Bearer ")


# ── the empty-response raise (m14.3 review C1) ─────────────────────────────────
#
# The m14.3 SDK fix made a tool_calls turn (content:null + tool_calls) NOT raise.
# The guard these tests protect: a response with NEITHER content NOR tool_calls
# (or an EMPTY tool_calls:[] + content:null) is still a MALFORMED body and MUST
# raise — so a future refactor can't silently start swallowing broken responses.


class _RawBodyGatewayStub(_BaseStub):
    """A gateway stub returning an ARBITRARY raw JSON body (to forge bad shapes)."""

    def __init__(self, body: dict) -> None:
        self._body = body
        super().__init__()

    def _install_routes(self) -> None:
        def completions(state: _StubState, req: RecordedRequest):
            payload = json.dumps(self._body).encode()
            return 200, {"Content-Type": "application/json"}, payload

        self.state.routes.update({"POST /chat/completions": completions})


def _client_for_body(body: dict):
    stub = _RawBodyGatewayStub(body)
    stub.__enter__()
    cfg = PlaneConfig.for_test(model_gateway_url=stub.base_url)
    return stub, agent.from_config(cfg)


def test_content_null_no_tool_calls_raises():
    # content:null and NO tool_calls → not a tool turn, no text: a malformed body.
    body = {
        "id": "x",
        "model": "m",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": None}}],
    }
    stub, client = _client_for_body(body)
    try:
        with pytest.raises(EndpointError):
            client.model.chat("m", [{"role": "user", "content": "q"}])
    finally:
        stub.__exit__(None, None, None)


def test_content_null_empty_tool_calls_raises():
    # content:null AND tool_calls:[] (an EMPTY array) → still no callable intent
    # and no text: a malformed body that must raise, not return "".
    body = {
        "id": "x",
        "model": "m",
        "choices": [
            {"index": 0, "message": {"role": "assistant", "content": None, "tool_calls": []}}
        ],
    }
    stub, client = _client_for_body(body)
    try:
        with pytest.raises(EndpointError):
            client.model.chat("m", [{"role": "user", "content": "q"}])
    finally:
        stub.__exit__(None, None, None)



# ── m32.7: streaming token source ─────────────────────────────────────────────

from ctxmesh import _http  # noqa: E402
from ctxmesh.model import _accumulate_tool_calls, _sse_delta, _sse_obj  # noqa: E402


def _lines(*frames):
    """Split SSE frames into the lines _http.stream would yield (data lines + blank separators)."""
    out = []
    for f in frames:
        out.extend(f.split("\n"))
    return out


def _patch_stream(monkeypatch, lines):
    monkeypatch.setattr(_http, "stream", lambda *a, **k: iter(lines))


def test_sse_delta_parses_deltas_and_ignores_noise():
    assert _sse_delta('data: {"choices":[{"delta":{"content":"hi"}}]}') == "hi"
    assert _sse_delta("data: [DONE]") == ""
    assert _sse_delta(": a comment line") == ""
    assert _sse_delta("event: foo") == ""
    assert _sse_delta("data: not-json") == ""
    assert _sse_delta('data: {"choices":[{"delta":{}}]}') == ""
    assert _sse_delta('data: {"choices":[]}') == ""
    assert _sse_obj("data: [DONE]") is None
    assert _sse_obj('data: {"a":1}') == {"a": 1}


def test_model_stream_yields_token_deltas(monkeypatch):
    _patch_stream(
        monkeypatch,
        _lines(
            'data: {"choices":[{"delta":{"content":"Hel"}}]}',
            'data: {"choices":[{"delta":{"content":"lo"}}]}',
            "data: [DONE]",
        ),
    )
    client = agent.from_config(PlaneConfig.for_test(model_gateway_url="http://gw"))
    assert list(client.model.stream("m", [{"role": "user", "content": "hi"}])) == ["Hel", "lo"]


def _drain(gen):
    tokens = []
    try:
        while True:
            tokens.append(next(gen))
    except StopIteration as done:
        return tokens, done.value


def test_stream_completion_yields_tokens_and_returns_text(monkeypatch):
    _patch_stream(
        monkeypatch,
        _lines(
            'data: {"choices":[{"delta":{"content":"Hel"}}]}',
            'data: {"choices":[{"delta":{"content":"lo"}}]}',
            "data: [DONE]",
        ),
    )
    client = agent.from_config(PlaneConfig.for_test(model_gateway_url="http://gw"))
    tokens, resp = _drain(client.model.stream_completion("m", [{"role": "user", "content": "hi"}]))
    assert tokens == ["Hel", "lo"]
    assert resp.text == "Hello"
    assert resp.tool_calls == []
    assert resp.has_tool_calls is False


def test_stream_completion_assembles_streamed_tool_calls(monkeypatch):
    # The name + id arrive in the first chunk; function.arguments is streamed across chunks.
    _patch_stream(
        monkeypatch,
        _lines(
            'data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"echo_tool","arguments":"{\\"te"}}]}}]}',  # noqa: E501
            'data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"xt\\":\\"hi\\"}"}}]}}]}',  # noqa: E501
            "data: [DONE]",
        ),
    )
    client = agent.from_config(PlaneConfig.for_test(model_gateway_url="http://gw"))
    tokens, resp = _drain(client.model.stream_completion("m", [{"role": "user", "content": "hi"}]))
    assert tokens == [], "a tool-calling turn streams no text"
    assert resp.has_tool_calls is True
    calls = resp.tool_calls
    assert len(calls) == 1
    assert calls[0]["id"] == "call_1"
    assert calls[0]["function"]["name"] == "echo_tool"
    assert json.loads(calls[0]["function"]["arguments"]) == {"text": "hi"}


def test_accumulate_tool_calls_ignores_malformed():
    acc = {}
    _accumulate_tool_calls(acc, None)
    _accumulate_tool_calls(acc, "not-a-list")
    _accumulate_tool_calls(acc, [{"index": 0, "function": {"arguments": "x"}}])
    assert acc[0]["function"]["arguments"] == "x"
