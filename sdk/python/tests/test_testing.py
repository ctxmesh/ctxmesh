"""``ctxmesh.testing`` — the shipped offline-testing surface (DX-1).

Proves the fakes import from the PUBLIC package path (not the tests dir) and let an author
run an agent handler end-to-end with no cluster/launcher — the whole point of shipping them
in the wheel. This imports from ``ctxmesh.testing`` DIRECTLY (not the conftest fixtures) so a
regression that fails to package the module is caught here.
"""

from __future__ import annotations

from ctxmesh import InvokeRequest, agent
from ctxmesh.config import PlaneConfig, RunContext
from ctxmesh.serve import process_invoke
from ctxmesh.testing import DiscoveryStub, GatewayStub, MemoryStub


def _offline_client(mem, disc, gw):
    cfg = PlaneConfig.for_test(
        memory_base_url=mem.base_url,
        discovery_base_url=disc.base_url,
        model_gateway_url=gw.base_url,
        run=RunContext(agent_name="offline-agent", conversation_id=""),
    )
    return agent.from_config(cfg)


def test_public_import_path_runs_an_agent_offline():
    """A custom handler served via ctxmesh.serve, driven entirely against ctxmesh.testing fakes."""
    with (
        MemoryStub() as mem,
        DiscoveryStub() as disc,
        GatewayStub(content="hello from offline") as gw,
    ):
        client = _offline_client(mem, disc, gw)

        def handler(req: InvokeRequest) -> str:
            return req.client.model.chat(
                "gpt-4o-mini", [{"role": "user", "content": req.input}]
            ).text

        body = process_invoke(client, handler, "offline-agent", b'{"input":"hi"}', {})
        assert body["output"] == "hello from offline"
        # The gateway stub recorded the round-trip — the plane was really exercised.
        assert gw.requests and gw.requests[-1].path == "/chat/completions"


def test_memory_stub_round_trips_through_the_client():
    with MemoryStub() as mem, DiscoveryStub() as disc, GatewayStub() as gw:
        client = _offline_client(mem, disc, gw).with_conversation("conv-1")
        client.memory.append({"role": "user", "content": "remember me"})
        got = client.memory.get()
        assert got == [{"role": "user", "content": "remember me"}]
