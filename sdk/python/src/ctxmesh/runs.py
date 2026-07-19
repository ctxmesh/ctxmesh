"""Client for the run-oriented execution contract (ADR 0034, m32.9).

A :class:`RunsClient` drives the BFF run API — create a durable run, poll it, stream its events,
resume it (consent connected / approval granted-or-denied), or cancel it. It is the programmatic
counterpart to the console chat: an app, CLI, or another service creates and follows runs the same
way the UI does.

Runs are caller-authenticated (ADR 0011): the client sends a bearer token, exactly like the
console. This is deliberately NOT the in-pod launcher plane (memory/tools/model) — those are
localhost and unauthenticated; a run is a control-plane resource.

    runs = RunsClient("https://console.example", token=my_token)
    run = runs.create(agent="researcher", input={"input": "summarise Q3"})
    for event in runs.stream(run.id):
        if event.kind == "token":
            print(event.data, end="")
    final = runs.get(run.id)
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any, Dict, Iterator, List, Optional

from ctxmesh import _http
from ctxmesh.errors import EndpointError

# Terminal run statuses (mirror internal/run.Status): no further transition, so run() stops polling.
_TERMINAL = frozenset({"succeeded", "failed", "cancelled", "expired"})


@dataclass
class Run:
    """A run's current state (a subset of the BFF run DTO the client needs)."""

    id: str
    status: str
    #: The assistant turns so far ([{role, content}]); the final answer is the last one on success.
    messages: List[Dict[str, str]] = field(default_factory=list)
    #: When status == requires_action, what the run is waiting on: {kind, servers?, key?, message?}.
    requires_action: Optional[Dict[str, Any]] = None
    trace_id: str = ""
    error: str = ""

    @property
    def is_terminal(self) -> bool:
        return self.status in _TERMINAL

    @classmethod
    def _from_json(cls, obj: Dict[str, Any]) -> "Run":
        return cls(
            id=str(obj.get("id", "")),
            status=str(obj.get("status", "")),
            messages=list(obj.get("messages") or []),
            requires_action=obj.get("requiresAction"),
            trace_id=str(obj.get("traceId", "")),
            error=str(obj.get("error", "")),
        )


@dataclass
class RunEvent:
    """One event off a run's SSE stream: a monotonic seq, a kind, and its data payload."""

    seq: int
    kind: str
    data: str


class RunsClient:
    """A thin, dependency-free client over the BFF run API (ADR 0034)."""

    def __init__(self, base_url: str, *, token: Optional[str] = None):
        if not base_url:
            raise ValueError("RunsClient requires a base_url (the console/BFF origin)")
        self._base = base_url.rstrip("/")
        self._token = token

    # -- lifecycle ---------------------------------------------------------------------------

    def create(
        self,
        *,
        agent: str,
        input: Any,
        namespace: Optional[str] = None,
        conversation_id: Optional[str] = None,
    ) -> Run:
        """Create + start a run (POST /api/runs → 202). Returns the run id + initial status."""
        body: Dict[str, Any] = {"agent": agent, "input": input}
        if namespace:
            body["namespace"] = namespace
        if conversation_id:
            body["conversationId"] = conversation_id
        resp = _http.request(
            "POST", self._url("/api/runs"),
            body=_http.json_body(body), headers=self._headers(), expect=(200, 202),
        )
        return Run._from_json(resp.json())

    def get(self, run_id: str) -> Run:
        """Fetch a run's current state (GET /api/runs/{id})."""
        resp = _http.request(
            "GET", self._url(f"/api/runs/{run_id}"), headers=self._headers(), expect=(200,)
        )
        return Run._from_json(resp.json())

    def resume(self, run_id: str, *, decision: Optional[str] = None) -> Run:
        """Resume a run paused in requires_action (POST /api/runs/{id}/resume).

        ``decision`` is for human-in-the-loop approval (m32.4): ``"approve"`` (the default effect)
        or ``"deny"`` (→ cancelled). The consent path needs no decision — the user connected their
        account, so resume just re-invokes.
        """
        body = _http.json_body({"decision": decision}) if decision else None
        resp = _http.request(
            "POST", self._url(f"/api/runs/{run_id}/resume"),
            body=body, headers=self._headers(), expect=(200, 202),
        )
        return Run._from_json(resp.json())

    def cancel(self, run_id: str) -> Run:
        """Cancel a non-terminal run (POST /api/runs/{id}/cancel → cancelled)."""
        resp = _http.request(
            "POST", self._url(f"/api/runs/{run_id}/cancel"), headers=self._headers(), expect=(200,)
        )
        return Run._from_json(resp.json())

    # -- streaming ---------------------------------------------------------------------------

    def stream(self, run_id: str, *, from_seq: int = 0) -> Iterator[RunEvent]:
        """Stream a run's events over SSE (GET /api/runs/{id}/events), resumable from a cursor.

        Yields :class:`RunEvent` (``state`` / ``message`` / ``token`` / ``step``) as they arrive;
        the generator ends when the stream closes (the run reached a terminal state). Pass
        ``from_seq`` (a Last-Event-ID cursor) to replay only events after a reconnect.
        """
        headers = self._headers()
        headers["Accept"] = "text/event-stream"
        if from_seq:
            headers["Last-Event-ID"] = str(from_seq)
        seq, kind, data = 0, "", None
        for line in _http.stream("GET", self._url(f"/api/runs/{run_id}/events"), headers=headers):
            if line == "":
                # Blank line terminates an SSE event — emit what we accumulated.
                if data is not None:
                    yield RunEvent(seq=seq, kind=kind or "message", data=data)
                seq, kind, data = 0, "", None
                continue
            field_name, _, value = line.partition(":")
            value = value[1:] if value.startswith(" ") else value
            if field_name == "id":
                seq = _int_or(value, 0)
            elif field_name == "event":
                kind = value
            elif field_name == "data":
                data = value if data is None else data + "\n" + value

    # -- sync sugar --------------------------------------------------------------------------

    def run(
        self,
        *,
        agent: str,
        input: Any,
        namespace: Optional[str] = None,
        conversation_id: Optional[str] = None,
        poll_interval: float = 0.25,
        timeout: float = 120.0,
    ) -> Run:
        """Create a run and BLOCK until it reaches a terminal state (or requires_action), then
        return it — the synchronous sugar over create + poll. A ``requires_action`` run is returned
        so the caller can resume it. Raises :class:`EndpointError` on timeout."""
        run = self.create(
            agent=agent, input=input, namespace=namespace, conversation_id=conversation_id
        )
        deadline = time.monotonic() + timeout
        while True:
            run = self.get(run.id)
            if run.is_terminal or run.status == "requires_action":
                return run
            if time.monotonic() >= deadline:
                raise EndpointError(f"run {run.id} did not finish within {timeout}s", status=None)
            time.sleep(poll_interval)

    # -- internals ---------------------------------------------------------------------------

    def _url(self, path: str) -> str:
        return self._base + path

    def _headers(self) -> Dict[str, str]:
        headers = {"Content-Type": "application/json"}
        if self._token:
            headers["Authorization"] = f"Bearer {self._token}"
        return headers


def _int_or(value: str, default: int) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return default
