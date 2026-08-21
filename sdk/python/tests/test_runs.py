"""RunsClient contract tests (ADR 0034, m32.9).

Drive :class:`ctxmesh.RunsClient` against a monkeypatched HTTP layer (the same approach the token
streaming tests use), asserting it speaks the BFF run API: create → 202, get, resume with a
decision, cancel, SSE stream parsing, and the sync run() sugar's create+poll.
"""

from __future__ import annotations

import json
from typing import List, Tuple

import pytest

from ctxmesh import RunEvent, RunsClient, _http


class _Recorder:
    """Records requests and replays canned Responses (FIFO)."""

    def __init__(self, responses: List[_http.Response]):
        self.calls: List[Tuple[str, str, bytes, dict]] = []
        self._responses = list(responses)

    def request(self, method, url, *, body=None, headers=None, timeout=30.0, expect=None):
        self.calls.append((method, url, body, headers or {}))
        return self._responses.pop(0)


def _resp(status: int, obj: dict) -> _http.Response:
    return _http.Response(status, {"content-type": "application/json"}, json.dumps(obj).encode())


def test_create_posts_run_and_parses_id(monkeypatch):
    rec = _Recorder([_resp(202, {"id": "run-1", "status": "queued"})])
    monkeypatch.setattr(_http, "request", rec.request)

    runs = RunsClient("https://console.example/", token="tok-abc")
    run = runs.create(agent="researcher", input={"input": "hi"}, conversation_id="c1")

    assert run.id == "run-1"
    assert run.status == "queued"
    method, url, body, headers = rec.calls[0]
    assert (method, url) == ("POST", "https://console.example/api/runs")
    assert headers["Authorization"] == "Bearer tok-abc"
    sent = json.loads(body)
    assert sent == {"agent": "researcher", "input": {"input": "hi"}, "conversationId": "c1"}


def test_get_parses_messages_and_requires_action(monkeypatch):
    rec = _Recorder(
        [
            _resp(
                200,
                {
                    "id": "run-1",
                    "status": "requires_action",
                    "traceId": "tr-1",
                    "messages": [{"role": "assistant", "content": "awaiting"}],
                    "requiresAction": {
                        "kind": "approval",
                        "key": "send-email",
                        "message": "Send it?",
                    },
                },
            )
        ]
    )
    monkeypatch.setattr(_http, "request", rec.request)

    run = RunsClient("https://c").get("run-1")
    assert run.status == "requires_action"
    assert run.trace_id == "tr-1"
    assert run.requires_action["kind"] == "approval"
    assert run.requires_action["key"] == "send-email"
    assert run.messages[0]["content"] == "awaiting"
    assert not run.is_terminal


def test_resume_sends_decision(monkeypatch):
    rec = _Recorder([_resp(200, {"id": "run-1", "status": "cancelled"})])
    monkeypatch.setattr(_http, "request", rec.request)

    run = RunsClient("https://c").resume("run-1", decision="deny")
    assert run.status == "cancelled"
    method, url, body, _ = rec.calls[0]
    assert (method, url) == ("POST", "https://c/api/runs/run-1/resume")
    assert json.loads(body) == {"decision": "deny"}


def test_cancel_posts(monkeypatch):
    rec = _Recorder([_resp(200, {"id": "run-1", "status": "cancelled"})])
    monkeypatch.setattr(_http, "request", rec.request)

    run = RunsClient("https://c").cancel("run-1")
    assert run.status == "cancelled"
    assert rec.calls[0][:2] == ("POST", "https://c/api/runs/run-1/cancel")


def test_stream_parses_sse_frames(monkeypatch):
    # A realistic BFF SSE stream: two token frames, a message, then the terminal state.
    lines = [
        "id:1",
        "event:token",
        "data:Hel",
        "",
        "id:2",
        "event:token",
        "data:lo",
        "",
        "id:3",
        "event:message",
        "data:Hello",
        "",
        "id:4",
        "event:state",
        "data:succeeded",
        "",
    ]

    def fake_stream(method, url, *, body=None, headers=None, timeout=30.0):
        assert headers["Accept"] == "text/event-stream"
        assert "Last-Event-ID" not in headers  # from_seq defaults to 0 → no cursor header
        yield from lines

    monkeypatch.setattr(_http, "stream", fake_stream)

    events = list(RunsClient("https://c").stream("run-1"))
    assert events == [
        RunEvent(1, "token", "Hel"),
        RunEvent(2, "token", "lo"),
        RunEvent(3, "message", "Hello"),
        RunEvent(4, "state", "succeeded"),
    ]


def test_run_sugar_creates_then_polls_to_terminal(monkeypatch):
    # create → queued; then get → running, then succeeded (poll stops on terminal).
    rec = _Recorder(
        [
            _resp(202, {"id": "run-1", "status": "queued"}),
            _resp(200, {"id": "run-1", "status": "running"}),
            _resp(
                200,
                {
                    "id": "run-1",
                    "status": "succeeded",
                    "messages": [{"role": "assistant", "content": "done"}],
                },
            ),
        ]
    )
    monkeypatch.setattr(_http, "request", rec.request)

    final = RunsClient("https://c").run(agent="a", input={"input": "x"}, poll_interval=0.0)
    assert final.status == "succeeded"
    assert final.messages[-1]["content"] == "done"
    # 1 create + 2 gets.
    assert [c[0] for c in rec.calls] == ["POST", "GET", "GET"]


def test_run_sugar_returns_on_requires_action(monkeypatch):
    rec = _Recorder(
        [
            _resp(202, {"id": "run-1", "status": "queued"}),
            _resp(
                200,
                {
                    "id": "run-1",
                    "status": "requires_action",
                    "requiresAction": {"kind": "consent_required", "servers": ["gh"]},
                },
            ),
        ]
    )
    monkeypatch.setattr(_http, "request", rec.request)

    run = RunsClient("https://c").run(agent="a", input={}, poll_interval=0.0)
    assert run.status == "requires_action"
    assert run.requires_action["servers"] == ["gh"]


def test_requires_base_url():
    with pytest.raises(ValueError):
        RunsClient("")


def test_stream_wraps_midstream_error_as_endpoint_error(monkeypatch):
    """FUNC-7: a mid-stream read error (e.g. a socket TimeoutError between SSE events, on the
    5s default) must surface as EndpointError — never a raw TimeoutError — so a caller sees
    the SDK's typed error instead of an unhandled exception mid-run."""
    import urllib.request

    class FakeResp:
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, *_a):
            return False

        def __iter__(self):
            yield b"data: one\n"
            raise TimeoutError("read timed out")  # the FUNC-7 stall, between events

    monkeypatch.setattr(urllib.request, "urlopen", lambda *_a, **_k: FakeResp())
    gen = _http.stream("GET", "https://c/events")
    assert next(gen) == "data: one"
    with pytest.raises(_http.EndpointError):
        next(gen)


def test_runs_stream_uses_long_read_timeout(monkeypatch):
    """FUNC-7: runs.stream must pass a LONG read timeout so a >5s idle gap (a slow model turn
    or a run parked at requires_action) doesn't kill the stream on the 5s default."""
    seen = {}

    def fake_stream(method, url, *, headers=None, timeout=None, **_kw):
        seen["timeout"] = timeout
        return iter(["event: run.completed", "data: {}", ""])

    monkeypatch.setattr(_http, "stream", fake_stream)
    list(RunsClient("https://c").stream("run-1"))
    assert seen["timeout"] == _http.STREAM_READ_TIMEOUT
    assert seen["timeout"] > _http.DEFAULT_TIMEOUT
