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

from .launcher_stub import GatewayStub


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
