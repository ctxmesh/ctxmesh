"""Memory client contract tests against the :2998 stub."""

from __future__ import annotations

import json

import pytest

from ctxmesh import agent
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError, EndpointError

from .launcher_stub import MemoryStub


def test_get_hits_the_right_path_and_returns_entries(client, memory_stub: MemoryStub):
    memory_stub.store["conv-42"] = [{"role": "user", "content": "hi"}]
    entries = client.memory.get("conv-42")
    assert entries == [{"role": "user", "content": "hi"}]

    req = memory_stub.requests[-1]
    assert req.method == "GET"
    assert req.path == "/memory/conv-42"


def test_get_empty_conversation_returns_empty_list(client):
    assert client.memory.get("never-seen") == []


def test_put_replaces_with_json_array_body(client, memory_stub: MemoryStub):
    entries = [{"role": "user", "content": "a"}, {"role": "assistant", "content": "b"}]
    client.memory.put(entries, conversation_id="conv-1")

    req = memory_stub.requests[-1]
    assert req.method == "PUT"
    assert req.path == "/memory/conv-1"
    # Body is a compact JSON array (the M5 contract).
    assert json.loads(req.body) == entries
    assert b" " not in req.body  # compacted
    assert memory_stub.store["conv-1"] == entries


def test_append_posts_single_value(client, memory_stub: MemoryStub):
    client.memory.append({"role": "user", "content": "x"}, conversation_id="conv-9")
    req = memory_stub.requests[-1]
    assert req.method == "POST"
    assert req.path == "/memory/conv-9/append"
    assert json.loads(req.body) == {"role": "user", "content": "x"}
    # No message id → no X-Message-Id header (the launcher endpoint mints one).
    assert "x-message-id" not in req.headers


def test_append_relays_message_id_header(client, memory_stub: MemoryStub):
    # The per-hop messageId (m33.4) rides X-Message-Id so the launcher attributes the entry to it.
    client.memory.append(
        {"role": "user", "content": "x"}, conversation_id="c", message_id="m-hop-7"
    )
    req = memory_stub.requests[-1]
    assert req.headers.get("x-message-id") == "m-hop-7"


def test_search_passes_query_param(client, memory_stub: MemoryStub):
    memory_stub.store["conv-s"] = [{"content": "needle"}, {"content": "hay"}]
    matches = client.memory.search("needle", conversation_id="conv-s")
    assert matches == [{"content": "needle"}]

    req = memory_stub.requests[-1]
    assert req.path == "/memory/conv-s/search"
    assert req.query.get("q") == ["needle"]


def test_session_forwards_run_capability(client, memory_stub: MemoryStub):
    # M98 (EU1a, ADR 0080): the session methods forward the run capability (like long-term memory)
    # so the launcher can user-scope per-user session memory. Every verb relays it when bound.
    from ctxmesh._capability import CAPABILITY_HEADER, capability_scope

    memory_stub.store["c1"] = [{"content": "hi"}]
    with capability_scope({CAPABILITY_HEADER: "cap-token-abc"}):
        client.memory.get("c1")
        client.memory.put([{"role": "user", "content": "a"}], conversation_id="c1")
        client.memory.append({"role": "user", "content": "b"}, conversation_id="c1")
        client.memory.search("hi", conversation_id="c1")

    relayed = [r.headers.get("x-ctxmesh-run-capability") for r in memory_stub.requests[-4:]]
    assert relayed == ["cap-token-abc"] * 4, "get/put/append/search all relay the run capability"


def test_session_omits_capability_when_absent(client, memory_stub: MemoryStub):
    # No active capability (e.g. an async/eventing turn) ⇒ no header; the launcher falls back to the
    # agent-wide bucket rather than 401ing (ADR 0052 async-safety).
    client.memory.append({"role": "user", "content": "x"}, conversation_id="c2")
    assert "x-ctxmesh-run-capability" not in memory_stub.requests[-1].headers


def test_with_conversation_binds_default_id(client, memory_stub: MemoryStub):
    memory_stub.store["bound"] = [{"content": "z"}]
    turn = client.with_conversation("bound")
    assert turn.memory.get() == [{"content": "z"}]
    assert memory_stub.requests[-1].path == "/memory/bound"


def test_invalid_conversation_id_is_rejected_client_side(client):
    # A slash-containing id would mangle the URL path — rejected before any HTTP.
    with pytest.raises(ConfigError):
        client.memory.get("bad/id")
    with pytest.raises(ConfigError):
        client.memory.get("has space")
    with pytest.raises(ConfigError):
        client.memory.get("")


def test_unwired_memory_raises_config_error(feedback_stub, discovery_stub):
    # A config where memory was never wired (no MEMORY_PORT/backend).
    cfg = agent.from_env(environ={"AGENT_NAME": "a"}).config
    unwired = agent.from_config(cfg)
    with pytest.raises(ConfigError):
        unwired.memory.get("conv-1")


def test_backend_error_surfaces_not_swallowed(client, memory_stub: MemoryStub):
    # Point the client at a dead port → connection refused → EndpointError.
    dead = PlaneConfig.for_test(memory_base_url="http://127.0.0.1:1")
    c = agent.from_config(dead)
    with pytest.raises(EndpointError):
        c.memory.get("conv-1")
