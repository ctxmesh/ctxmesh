"""from_env() plane resolution + fail-fast-outside-a-pod contract."""

from __future__ import annotations

import pytest

from ctxmesh import agent
from ctxmesh.config import DEFAULT_FEEDBACK_PORT, DEFAULT_MEMORY_PORT, PlaneConfig
from ctxmesh.errors import NotInPodError


def test_from_env_fails_fast_without_launcher():
    """No launcher env → NotInPodError, never a silent no-op (spec edge case)."""
    with pytest.raises(NotInPodError) as exc:
        agent.from_env(environ={})
    # The message must be actionable: mention it needs a launcher pod.
    assert "launcher" in str(exc.value).lower()


def test_from_env_reads_injected_ports_and_context():
    env = {
        "AGENT_NAME": "billing-agent",
        "AGENT_VERSION": "1.2.3",
        "AGENT_ROLE": "assistant",
        "MEMORY_PORT": "2998",
        "MEMORY_BACKEND_ADDR": "valkey:6379",
        "FEEDBACK_PORT": "2995",
        "LANGFUSE_HOST": "http://langfuse",
    }
    client = agent.from_env(environ=env)
    cfg = client.config
    assert cfg.memory_base_url == "http://localhost:2998"
    assert cfg.discovery_base_url == "http://localhost:2999"
    assert cfg.feedback_base_url == "http://localhost:2995"
    assert cfg.memory_wired is True
    assert cfg.feedback_wired is True
    assert cfg.run.agent_name == "billing-agent"
    assert cfg.run.agent_version == "1.2.3"


def test_from_env_defaults_ports_when_unset_but_marker_present():
    # Only AGENT_NAME present: still a launcher pod; ports fall back to defaults.
    client = agent.from_env(environ={"AGENT_NAME": "a"})
    assert client.config.memory_base_url == f"http://localhost:{DEFAULT_MEMORY_PORT}"
    assert client.config.feedback_base_url == f"http://localhost:{DEFAULT_FEEDBACK_PORT}"
    # Neither memory nor feedback was wired (no ports/backends injected).
    assert client.config.memory_wired is False
    assert client.config.feedback_wired is False


def test_from_env_rejects_bad_port():
    with pytest.raises(NotInPodError):
        agent.from_env(environ={"AGENT_NAME": "a", "MEMORY_PORT": "not-a-port"})
    with pytest.raises(NotInPodError):
        agent.from_env(environ={"AGENT_NAME": "a", "FEEDBACK_PORT": "70000"})


def test_for_test_builds_fully_wired_config():
    cfg = PlaneConfig.for_test()
    assert cfg.memory_wired and cfg.feedback_wired
    assert cfg.discovery_base_url.startswith("http://localhost:2999")
