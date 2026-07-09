"""Shared pytest fixtures: the launcher-plane stubs and a Client bound to them."""

from __future__ import annotations

from collections.abc import Iterator

import pytest

from ctxmesh import agent
from ctxmesh.client import Client
from ctxmesh.config import PlaneConfig, RunContext

from .launcher_stub import DiscoveryStub, FeedbackStub, MemoryStub


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
