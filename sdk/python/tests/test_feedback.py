"""Feedback client contract tests against the :2995 stub (202 / 400 / 502)."""

from __future__ import annotations

import json

import pytest

from ctxmesh import agent
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import ConfigError, EndpointError

from .launcher_stub import FeedbackStub


def test_score_posts_expected_body_and_accepts_202(client, feedback_stub: FeedbackStub):
    client.feedback.score("trace-abc", "thumbs-up", 1, comment="great")

    req = feedback_stub.requests[-1]
    assert req.method == "POST"
    assert req.path == "/feedback"
    assert json.loads(req.body) == {
        "traceId": "trace-abc",
        "name": "thumbs-up",
        "value": 1,
        "comment": "great",
    }


def test_score_omits_comment_when_none(client, feedback_stub: FeedbackStub):
    client.feedback.score("t", "accuracy", 0.75)
    body = json.loads(feedback_stub.requests[-1].body)
    assert body == {"traceId": "t", "name": "accuracy", "value": 0.75}
    assert "comment" not in body


def test_score_coerces_bool_to_number(client, feedback_stub: FeedbackStub):
    client.feedback.score("t", "flag", True)
    assert json.loads(feedback_stub.requests[-1].body)["value"] == 1


def test_missing_trace_id_rejected_client_side(client):
    with pytest.raises(ConfigError):
        client.feedback.score("", "name", 1)


def test_non_numeric_value_rejected_client_side(client):
    with pytest.raises(ConfigError):
        client.feedback.score("t", "name", "not-a-number")  # type: ignore[arg-type]


def test_server_400_surfaces_with_status(feedback_stub: FeedbackStub):
    # The stub 400s on an empty traceId; but client-side validation catches that,
    # so drive the 400 by forcing the stub to reject a well-formed request.
    feedback_stub.force_status = 400
    cfg = PlaneConfig.for_test(feedback_base_url=feedback_stub.base_url)
    c = agent.from_config(cfg)
    with pytest.raises(EndpointError) as exc:
        c.feedback.score("t", "name", 1)
    assert exc.value.status == 400


def test_server_502_surfaces_not_swallowed(feedback_stub: FeedbackStub):
    feedback_stub.force_status = 502
    cfg = PlaneConfig.for_test(feedback_base_url=feedback_stub.base_url)
    c = agent.from_config(cfg)
    with pytest.raises(EndpointError) as exc:
        c.feedback.score("t", "name", 1)
    assert exc.value.status == 502


def test_unwired_feedback_raises_config_error():
    cfg = agent.from_env(environ={"AGENT_NAME": "a"}).config
    c = agent.from_config(cfg)
    with pytest.raises(ConfigError):
        c.feedback.score("t", "name", 1)
