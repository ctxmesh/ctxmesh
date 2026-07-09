"""The ``agent`` entry module: build a :class:`~ctxmesh.client.Client`.

    from ctxmesh import agent
    client = agent.from_env()          # in-pod: reads the launcher plane

``from_env()`` fails fast with :class:`~ctxmesh.errors.NotInPodError` when the
launcher env is absent — it never silently no-ops (spec edge case). Tests and
offline callers use :func:`from_config` with an explicit
:class:`~ctxmesh.config.PlaneConfig` (e.g. one pointing at the launcher stub).
"""

from __future__ import annotations

from typing import Dict, Optional

from opentelemetry.sdk.trace import SpanProcessor

from ctxmesh.client import Client
from ctxmesh.config import PlaneConfig


def from_env(environ: Optional[Dict[str, str]] = None) -> Client:
    """Build a Client from the launcher-injected env (the in-pod entry point).

    Reads MEMORY_PORT / FEEDBACK_PORT / MODEL_GATEWAY_URL /
    OTEL_EXPORTER_OTLP_ENDPOINT / AGENT_NAME / … and the fixed discovery port
    :2999, resolving the localhost plane's base URLs and the run context. Raises
    NotInPodError when no launcher env is present.
    """
    config = PlaneConfig.from_env(environ, require_launcher=True)
    return Client(config)


def from_config(
    config: PlaneConfig,
    *,
    span_processor: Optional[SpanProcessor] = None,
) -> Client:
    """Build a Client from an explicit PlaneConfig (tests / offline mode).

    ``span_processor`` (tests) installs an in-memory span exporter on the trace
    client so the emitted step/tool/llm spans can be captured and asserted
    without a live collector.
    """
    return Client(config, span_processor=span_processor)
