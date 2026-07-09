"""Shared pytest fixtures: the launcher-plane stubs and a Client bound to them."""

from __future__ import annotations

from collections.abc import Iterator

import pytest
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
    InMemorySpanExporter,
)

from ctxmesh import _tracing, agent
from ctxmesh.client import Client
from ctxmesh.config import PlaneConfig, RunContext

from .launcher_stub import DiscoveryStub, FeedbackStub, GatewayStub, MemoryStub


@pytest.fixture
def memory_stub() -> Iterator[MemoryStub]:
    with MemoryStub() as stub:
        yield stub


@pytest.fixture
def discovery_stub() -> Iterator[DiscoveryStub]:
    with DiscoveryStub() as stub:
        yield stub


@pytest.fixture
def feedback_stub() -> Iterator[FeedbackStub]:
    with FeedbackStub() as stub:
        yield stub


@pytest.fixture
def gateway_stub() -> Iterator[GatewayStub]:
    with GatewayStub() as stub:
        yield stub


@pytest.fixture
def plane(
    memory_stub: MemoryStub,
    discovery_stub: DiscoveryStub,
    feedback_stub: FeedbackStub,
) -> PlaneConfig:
    """A fully-wired PlaneConfig pointing at all three stubs."""
    return PlaneConfig.for_test(
        memory_base_url=memory_stub.base_url,
        discovery_base_url=discovery_stub.base_url,
        feedback_base_url=feedback_stub.base_url,
        run=RunContext(agent_name="test-agent", conversation_id=""),
    )


@pytest.fixture
def client(plane: PlaneConfig) -> Client:
    return agent.from_config(plane)


@pytest.fixture
def span_exporter() -> Iterator[InMemorySpanExporter]:
    """An in-memory OTLP span capture.

    Resets the process-wide TracerProvider so this test installs a FRESH provider
    exporting into the returned exporter (SimpleSpanProcessor = synchronous, so a
    finished span is immediately available to assert on). Reset again on teardown
    so a later test that expects the offline no-op provider gets one.
    """
    _tracing.reset_provider()
    exporter = InMemorySpanExporter()
    # Prime the process-wide provider with this exporter before any client is
    # built; TraceClient then memoises onto it, so every client in the test
    # exports into the same capture.
    _tracing.get_provider(span_processor=SimpleSpanProcessor(exporter), force=True)
    yield exporter
    _tracing.reset_provider()


@pytest.fixture
def traced_client(plane: PlaneConfig, span_exporter: InMemorySpanExporter) -> Client:
    """A Client whose trace client exports into the in-memory ``span_exporter``.

    The ``span_exporter`` fixture already installed the capturing provider; the
    trace client memoises onto it (no second processor is registered).
    """
    return agent.from_config(plane)
