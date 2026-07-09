"""Feedback client — the launcher's :2995 feedback-ingest hook (M9).

Wire contract (eval-prompts-feedback.md §3, cmd/launcher/feedback.go):

    POST /feedback  { "traceId": <id>, "name": <score-name>,
                      "value": <number>, "comment": <optional> }
                    -> 202 Accepted     (relayed to Langfuse)
                    -> 400              (missing traceId / malformed body)
                    -> 502              (Langfuse relay error)

The value is numeric. A 400 (bad request) and a 502 (upstream/Langfuse down)
both surface as an EndpointError carrying the status — the SDK never swallows a
rejected or failed score.
"""

from __future__ import annotations

from numbers import Real
from typing import Optional

from ctxmesh import _http
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError


class FeedbackClient:
    """Attach a numeric score to a trace via the launcher's :2995 hook."""

    def __init__(self, config: PlaneConfig):
        self._config = config

    def score(
        self,
        trace_id: str,
        name: str,
        value: Real,
        comment: Optional[str] = None,
    ) -> None:
        """POST a score for *trace_id*; returns on 202, raises otherwise.

        Raises ConfigError for arguments the endpoint would 400 on (empty
        traceId, non-numeric value) so the caller gets a precise client-side
        error, and EndpointError (with .status) for a server-side 400/502.
        """
        if not self._config.feedback_wired:
            raise ConfigError(
                "feedback is not wired for this agent: the launcher did not "
                "inject FEEDBACK_PORT/LANGFUSE_HOST. Wire feedback (LANGFUSE_HOST) "
                "to use client.feedback.score(...)"
            )
        if not trace_id:
            raise ConfigError("feedback.score requires a non-empty trace_id")
        # bool is a subclass of int/Real; the M9 hook coerces bool to 0/1, so we
        # accept it, but reject strings and other non-numeric values up front.
        if not isinstance(value, Real):
            raise ConfigError(f"feedback score value must be numeric, got {type(value).__name__}")

        payload = {
            "traceId": trace_id,
            "name": name,
            # bool -> 0/1 to match the hook's numeric coercion; everything else
            # is already a real number and serialises as JSON number.
            "value": int(value) if isinstance(value, bool) else value,
        }
        if comment is not None:
            payload["comment"] = comment

        _http.request(
            "POST",
            f"{self._config.feedback_base_url}/feedback",
            body=_http.json_body(payload),
            headers={"Content-Type": "application/json"},
            expect=(202,),
        )
