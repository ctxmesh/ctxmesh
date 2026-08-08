"""Re-export shim: the launcher-plane fakes now ship in the SDK as ``ctxmesh.testing`` (DX-1).

The stubs moved into the package (so `pip install ctxmesh` alone lets an external author test
their agent offline). This shim keeps the SDK's own tests importing ``.launcher_stub`` unchanged
— including the internal ``_BaseStub`` / ``_StubState`` some tests subclass — so there is ONE
definition of the fakes, exercised by both the SDK suite and downstream users.
"""

from __future__ import annotations

from ctxmesh.testing import (  # noqa: F401 - re-exported for the SDK's own tests
    DiscoveryStub,
    FeedbackStub,
    GatewayStub,
    MemoryStub,
    RecordedRequest,
    _BaseStub,
    _match_route,
    _normalise,
    _StubState,
)
