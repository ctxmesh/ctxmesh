"""``ctxmesh.serve`` — the serving scaffold contract (DX-3).

Proves the one call a code-first agent makes encodes the launcher runtime contract correctly:
the ``/invoke`` body/​envelope shape, the ``/healthz``/``/readyz`` probes, the SSE token stream,
the autonomous conversation-id mint, and — the DX-2 fix it folds in — that the handler runs
inside ``request_scope`` so a custom loop keeps the invoking user's run capability.
"""

from __future__ import annotations

import http.client
import json
import logging
import threading
from http.server import ThreadingHTTPServer

import pytest

from ctxmesh._capability import CAPABILITY_HEADER, current_capability
from ctxmesh.managed import ManagedConfig, ManagedResult
from ctxmesh.serve import (
    InvokeRequest,
    _make_request_handler,
    _parse_body,
    process_invoke,
)


# ── the DX-2 fold-in: the handler runs inside request_scope ────────────────────────────
def test_process_invoke_binds_capability_for_the_handler(client):
    """A custom handler sees the invoking user's run capability bound (so its tool egress relays
    the OBO grant), and it is reset once the request completes — no bleed to the next request."""
    seen = {}

    def handler(req: InvokeRequest) -> str:
        seen["cap"] = current_capability()
        return f"echo:{req.input}"

    assert current_capability() is None
    body = process_invoke(
        client,
        handler,
        "agent-x",
        b'{"input":"hi"}',
        {CAPABILITY_HEADER: "cap-serve-1"},
    )
    assert seen["cap"] == "cap-serve-1"
    assert current_capability() is None  # reset on exit
    assert body["output"] == "echo:hi"


# ── the envelope ───────────────────────────────────────────────────────────────────────
def test_str_return_becomes_single_step_envelope(client):
    body = process_invoke(client, lambda req: "answer", "agent-x", b'{"input":"q"}', {})
    assert body == {
        "agent": "agent-x",
        "output": "answer",
        "steps": 1,
        "tools_called": [],
        "consent_required": [],
    }


def test_managed_result_carries_steps_tools_and_approval(client):
    result = ManagedResult(
        output="paused",
        steps=3,
        tools_called=["search", "echo"],
        consent_required=["scalekit"],
        approval_required={"key": "wire-money", "summary": "send $5"},
    )
    body = process_invoke(client, lambda req: result, "agent-x", b'{"input":"q"}', {})
    assert body["steps"] == 3
    assert body["tools_called"] == ["search", "echo"]
    assert body["consent_required"] == ["scalekit"]
    assert body["approval_required"] == {"key": "wire-money", "summary": "send $5"}


# ── streaming ──────────────────────────────────────────────────────────────────────────
def test_emit_token_streams_when_on_token_supplied(client):
    frames = []

    def handler(req: InvokeRequest) -> str:
        req.emit_token("hel")
        req.emit_token("lo")
        return "hello"

    body = process_invoke(client, handler, "agent-x", b'{"input":"q"}', {}, on_token=frames.append)
    assert frames == ["hel", "lo"]
    assert body["output"] == "hello"


def test_emit_token_is_a_noop_without_streaming(client):
    # No on_token → req.emit_token must be safe to call and do nothing (same handler, both modes).
    body = process_invoke(
        client, lambda req: (req.emit_token("x"), "done")[1], "agent-x", b'{"input":"q"}', {}
    )
    assert body["output"] == "done"


def test_emit_step_streams_when_on_step_supplied(client):
    # Step-visibility (M78, ADR 0071 §4): req.emit_step forwards a step metadata frame to on_step.
    frames = []

    def handler(req: InvokeRequest) -> str:
        req.emit_step({"step": 1, "kind": "model", "tokens": {"prompt": 3, "completion": 2}})
        return "ok"

    body = process_invoke(client, handler, "agent-x", b'{"input":"q"}', {}, on_step=frames.append)
    assert frames == [{"step": 1, "kind": "model", "tokens": {"prompt": 3, "completion": 2}}]
    assert body["output"] == "ok"


def test_emit_step_is_a_noop_without_streaming(client):
    # No on_step → req.emit_step must be safe to call and do nothing (same handler, both modes).
    body = process_invoke(
        client,
        lambda req: (req.emit_step({"step": 1, "kind": "model"}), "done")[1],
        "agent-x",
        b'{"input":"q"}',
        {},
    )
    assert body["output"] == "done"


# ── conversation-id resolution (m33.5) ─────────────────────────────────────────────────
def test_autonomous_run_mints_a_conversation_id(client):
    captured = {}
    process_invoke(
        client,
        lambda req: captured.setdefault("cid", req.conversation_id) or "",
        "agent-x",
        b'{"input":"q"}',
        {},
    )
    assert captured["cid"].startswith("run-")


def test_inbound_conversation_id_takes_precedence(client):
    captured = {}
    process_invoke(
        client,
        lambda req: captured.setdefault("cid", req.conversation_id) or "",
        "agent-x",
        b'{"input":"q"}',
        {"X-Conversation-Id": "conv-42"},
    )
    # None ⇒ the loop uses the inbound X-Conversation-Id (not a minted per-run id).
    assert captured["cid"] is None


# ── body parsing tolerance ─────────────────────────────────────────────────────────────
def test_parse_body_variants():
    # (input, approvals, checkpoint) — checkpoint is None unless the platform injected one (L7).
    assert _parse_body(b'{"input":"hello","approvals":["k1","k2"]}') == (
        "hello",
        ["k1", "k2"],
        None,
    )
    assert _parse_body(b"") == ("", [], None)
    # Non-JSON is treated as the raw prompt (never a 500).
    assert _parse_body(b"raw text") == ("raw text", [], None)
    # A JSON non-object degrades to its string form.
    assert _parse_body(b'"just a string"') == ("just a string", [], None)
    # An L7 resume envelope rides alongside input (the managed loop verifies it before trusting).
    env = {"version": 1, "kind": "supervisor-loop", "sha256": "x", "payload": "{}"}
    assert _parse_body(json.dumps({"input": "go", "checkpoint": env}).encode()) == ("go", [], env)


# ── the full HTTP contract (real socket) ───────────────────────────────────────────────
@pytest.fixture
def running_agent(client):
    """A live ctxmesh.serve HTTP server on an ephemeral port, echoing the input."""
    handler_cls = _make_request_handler(client, lambda req: f"echo:{req.input}", "srv-agent")
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler_cls)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server.server_address
    finally:
        server.shutdown()
        thread.join(timeout=5)


def _get(addr, path):
    conn = http.client.HTTPConnection(addr[0], addr[1], timeout=5)
    conn.request("GET", path)
    resp = conn.getresponse()
    out = (resp.status, resp.read())
    conn.close()
    return out


def _post(addr, path, body, headers=None):
    conn = http.client.HTTPConnection(addr[0], addr[1], timeout=5)
    conn.request("POST", path, body=body, headers=headers or {})
    resp = conn.getresponse()
    out = (resp.status, resp.read())
    conn.close()
    return out


def test_health_probes_return_ok(running_agent):
    for path in ("/healthz", "/readyz"):
        status, raw = _get(running_agent, path)
        assert status == 200
        assert json.loads(raw) == {"status": "ok"}


def test_unknown_routes_are_404(running_agent):
    assert _get(running_agent, "/nope")[0] == 404
    assert _post(running_agent, "/nope", b"{}")[0] == 404


def test_invoke_returns_the_envelope(running_agent):
    status, raw = _post(running_agent, "/invoke", b'{"input":"ping"}')
    assert status == 200
    body = json.loads(raw)
    assert body["agent"] == "srv-agent"
    assert body["output"] == "echo:ping"


def test_invoke_streams_sse_when_accepted(client):
    step_meta = {"step": 1, "kind": "model", "tokens": {"prompt": 3, "completion": 2}, "ref": None}

    def handler(req: InvokeRequest) -> str:
        req.emit_token("a")
        req.emit_step(step_meta)
        req.emit_token("b")
        return "ab"

    handler_cls = _make_request_handler(client, handler, "srv-agent")
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler_cls)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        conn = http.client.HTTPConnection(*server.server_address, timeout=5)
        conn.request(
            "POST", "/invoke", body=b'{"input":"q"}', headers={"Accept": "text/event-stream"}
        )
        resp = conn.getresponse()
        assert resp.getheader("Content-Type") == "text/event-stream"
        text = resp.read().decode()
        conn.close()
    finally:
        server.shutdown()
        thread.join(timeout=5)

    frames = [
        json.loads(line[len("data: ") :]) for line in text.splitlines() if line.startswith("data: ")
    ]
    assert [f for f in frames if f["type"] == "token"] == [
        {"type": "token", "text": "a"},
        {"type": "token", "text": "b"},
    ]
    # The step frame streamed as an SSE `step` event (M78, ADR 0071 §4 — live step-visibility).
    step_frames = [f for f in frames if f["type"] == "step"]
    assert step_frames == [{"type": "step", **step_meta}]
    done = [f for f in frames if f["type"] == "done"]
    assert done and done[0]["output"] == "ab"


def test_handler_error_becomes_502_not_a_crash(client):
    def boom(req: InvokeRequest) -> str:
        raise RuntimeError("kaboom")

    handler_cls = _make_request_handler(client, boom, "srv-agent")
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler_cls)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        status, raw = _post(server.server_address, "/invoke", b'{"input":"q"}')
    finally:
        server.shutdown()
        thread.join(timeout=5)
    assert status == 502
    assert "kaboom" in json.loads(raw)["error"]


# ── ManagedConfig.from_env (the moved-into-SDK env resolution) ──────────────────────────
def test_managed_config_from_env_reads_the_environment(monkeypatch):
    monkeypatch.setenv("SYSTEM_PROMPT", "be terse")
    monkeypatch.setenv("MODEL_ROUTE", "gpt-4o-mini")
    monkeypatch.setenv("MAX_STEPS", "5")
    cfg = ManagedConfig.from_env()
    assert cfg.system_prompt == "be terse"
    assert cfg.model_route == "gpt-4o-mini"
    assert cfg.max_steps == 5


def test_managed_config_from_env_defaults_are_safe(monkeypatch):
    for var in ("SYSTEM_PROMPT", "MODEL_ROUTE", "MAX_STEPS", "PROMPT_FILE"):
        monkeypatch.delenv(var, raising=False)
    cfg = ManagedConfig.from_env()
    assert cfg.system_prompt == "You are a helpful assistant."  # minimal default, never empty
    assert cfg.model_route == ""  # a config error the gateway surfaces, not swallowed here
    # A garbage MAX_STEPS falls back to the runaway-guard default, never 0/negative.
    monkeypatch.setenv("MAX_STEPS", "not-a-number")
    assert ManagedConfig.from_env().max_steps >= 1
    monkeypatch.setenv("MAX_STEPS", "0")
    assert ManagedConfig.from_env().max_steps >= 1


def test_managed_config_from_env_reads_prompt_file(monkeypatch, tmp_path):
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    prompt = tmp_path / "prompt.txt"
    prompt.write_text("  from the file  ")
    monkeypatch.setenv("PROMPT_FILE", str(prompt))
    assert ManagedConfig.from_env().system_prompt == "from the file"


def test_bad_max_steps_warns_and_uses_default(monkeypatch, caplog):
    """OTH-3: a misconfig degrades SAFELY but is no longer SILENT — it warns to stderr."""
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    monkeypatch.setenv("MAX_STEPS", "not-a-number")
    with caplog.at_level(logging.WARNING, logger="ctxmesh"):
        cfg = ManagedConfig.from_env()
    assert cfg.max_steps >= 1  # safe default
    assert any("MAX_STEPS" in r.message and "not-a-number" in r.message for r in caplog.records)


def test_unreadable_prompt_file_warns_and_uses_default(monkeypatch, caplog, tmp_path):
    """OTH-3: a set-but-unreadable PROMPT_FILE warns instead of silently serving the default."""
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    monkeypatch.setenv("PROMPT_FILE", str(tmp_path / "does-not-exist.txt"))
    with caplog.at_level(logging.WARNING, logger="ctxmesh"):
        cfg = ManagedConfig.from_env()
    assert cfg.system_prompt == "You are a helpful assistant."  # safe default, still serves
    assert any("PROMPT_FILE" in r.message for r in caplog.records)


# ── ManagedConfig.from_env: AGENT_RUNTIME / output_schema (m65.5, ADR 0058) ─────


def test_managed_config_from_env_output_schema_from_agent_runtime(monkeypatch):
    """AGENT_RUNTIME='{"outputSchema":{...}}' → from_env().output_schema equals that schema."""
    schema = {
        "type": "object",
        "properties": {"answer": {"type": "string"}},
        "required": ["answer"],
    }
    monkeypatch.setenv("AGENT_RUNTIME", json.dumps({"outputSchema": schema}))
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    cfg = ManagedConfig.from_env()
    assert cfg.output_schema == schema


def test_managed_config_from_env_output_schema_absent_when_no_agent_runtime(monkeypatch):
    """AGENT_RUNTIME unset → from_env().output_schema is None."""
    monkeypatch.delenv("AGENT_RUNTIME", raising=False)
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    cfg = ManagedConfig.from_env()
    assert cfg.output_schema is None


def test_managed_config_from_env_malformed_agent_runtime_no_crash(monkeypatch, caplog):
    """AGENT_RUNTIME='not json' → output_schema is None + a WARNING is logged (never a crash)."""
    monkeypatch.setenv("AGENT_RUNTIME", "not valid json {{")
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    with caplog.at_level(logging.WARNING, logger="ctxmesh"):
        cfg = ManagedConfig.from_env()
    assert cfg.output_schema is None
    assert any("AGENT_RUNTIME" in r.message for r in caplog.records)


def test_managed_config_from_env_agent_runtime_no_output_schema_key(monkeypatch):
    """AGENT_RUNTIME set but no 'outputSchema' key → output_schema is None (extensible parse)."""
    monkeypatch.setenv("AGENT_RUNTIME", json.dumps({"toolPolicy": {"default": "allow"}}))
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    cfg = ManagedConfig.from_env()
    assert cfg.output_schema is None


# ── ManagedConfig.from_env: AGENT_RUNTIME / tool_policy (m65.6, ADR 0058) ───────


def test_managed_config_from_env_tool_policy_from_agent_runtime(monkeypatch):
    """AGENT_RUNTIME='{"toolPolicy":{...}}' → from_env().tool_policy equals that policy."""
    policy = {
        "default": "require-approval",
        "overrides": [{"name": "search", "rule": "allow", "retryable": True}],
        "forcedChoice": "auto",
        "parallelLimit": 2,
    }
    monkeypatch.setenv("AGENT_RUNTIME", json.dumps({"toolPolicy": policy}))
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    cfg = ManagedConfig.from_env()
    assert cfg.tool_policy == policy


def test_managed_config_from_env_tool_policy_absent_when_no_agent_runtime(monkeypatch):
    """AGENT_RUNTIME unset → from_env().tool_policy is None."""
    monkeypatch.delenv("AGENT_RUNTIME", raising=False)
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    cfg = ManagedConfig.from_env()
    assert cfg.tool_policy is None


def test_managed_config_from_env_tool_policy_no_key(monkeypatch):
    """AGENT_RUNTIME set but no 'toolPolicy' key → tool_policy is None."""
    monkeypatch.setenv("AGENT_RUNTIME", json.dumps({"outputSchema": {"type": "object"}}))
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    cfg = ManagedConfig.from_env()
    assert cfg.tool_policy is None


def test_managed_config_from_env_tool_policy_non_dict_ignored(monkeypatch, caplog):
    """A non-dict toolPolicy is a misconfig → tool_policy is None + a WARNING (never a crash)."""
    monkeypatch.setenv("AGENT_RUNTIME", json.dumps({"toolPolicy": ["not", "a", "dict"]}))
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    with caplog.at_level(logging.WARNING, logger="ctxmesh"):
        cfg = ManagedConfig.from_env()
    assert cfg.tool_policy is None
    assert any("toolPolicy" in r.message for r in caplog.records)


# ── ManagedConfig.from_env: AGENT_RUNTIME / resilience (m65.7, ADR 0058) ────────


def test_managed_config_from_env_resilience_from_agent_runtime(monkeypatch):
    """AGENT_RUNTIME='{"resilience":{...}}' → from_env().resilience equals that object."""
    resilience = {
        "modelCall": {"timeoutSeconds": 20, "maxRetries": 2},
        "toolCall": {
            "timeoutSeconds": 15,
            "maxRetries": 3,
            "circuitBreaker": {"failureThreshold": 5, "cooldownSeconds": 30},
        },
    }
    monkeypatch.setenv("AGENT_RUNTIME", json.dumps({"resilience": resilience}))
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    cfg = ManagedConfig.from_env()
    assert cfg.resilience == resilience


def test_managed_config_from_env_resilience_absent_when_no_agent_runtime(monkeypatch):
    """AGENT_RUNTIME unset → from_env().resilience is None."""
    monkeypatch.delenv("AGENT_RUNTIME", raising=False)
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    cfg = ManagedConfig.from_env()
    assert cfg.resilience is None


def test_managed_config_from_env_resilience_no_key(monkeypatch):
    """AGENT_RUNTIME set but no 'resilience' key → resilience is None."""
    monkeypatch.setenv("AGENT_RUNTIME", json.dumps({"toolPolicy": {"default": "allow"}}))
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    cfg = ManagedConfig.from_env()
    assert cfg.resilience is None


def test_managed_config_from_env_resilience_non_dict_ignored(monkeypatch, caplog):
    """A non-dict resilience is a misconfig → resilience is None + a WARNING (never a crash)."""
    monkeypatch.setenv("AGENT_RUNTIME", json.dumps({"resilience": ["not", "a", "dict"]}))
    monkeypatch.delenv("SYSTEM_PROMPT", raising=False)
    with caplog.at_level(logging.WARNING, logger="ctxmesh"):
        cfg = ManagedConfig.from_env()
    assert cfg.resilience is None
    assert any("resilience" in r.message for r in caplog.records)
