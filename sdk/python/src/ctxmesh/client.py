"""The Client facade: one object holding the three localhost-plane clients.

Constructed by :func:`ctxmesh.agent.from_env` (in-pod) or
:func:`ctxmesh.agent.from_config` (tests/offline). Each attribute wraps one raw
launcher endpoint and applies the run context:

    client.memory     -> :2998   (M5)
    client.tools      -> :2999   (M4)
    client.feedback   -> :2995   (M9)

The ``model`` client and the ``trace.*`` step helpers are m10.3 and are not
attributes here yet.
"""

from __future__ import annotations

from ctxmesh.config import PlaneConfig, RunContext
from ctxmesh.feedback import FeedbackClient
from ctxmesh.memory import MemoryClient
from ctxmesh.tools import ToolsClient


class Client:
    """Typed entry point to the launcher localhost plane."""

    def __init__(self, config: PlaneConfig):
        self._config = config
        self.memory = MemoryClient(config)
        self.tools = ToolsClient(config)
        self.feedback = FeedbackClient(config)

    @property
    def config(self) -> PlaneConfig:
        return self._config

    @property
    def run(self) -> RunContext:
        """The run context (agent identity + conversationId) the SDK stamps."""
        return self._config.run

    def with_conversation(self, conversation_id: str) -> Client:
        """Return a client whose memory ops default to *conversation_id*.

        conversationId is per-request (the agent reads it from its inbound
        payload). Bind it once here so ``client.memory.get()`` needs no repeated
        id argument through the turn.
        """
        bound = Client(self._config)
        bound.memory = MemoryClient(self._config, conversation_id=conversation_id)
        return bound
