"""ctxmesh — the agent-engine Python SDK.

Typed, optional sugar over the launcher's language-agnostic localhost platform
plane (ADR 0002). Every capability the SDK exposes is *also* a raw launcher
endpoint; the SDK never adds a capability the plane does not have — it only
removes the raw-HTTP boilerplate and applies the run context.

m10.2 surface (this package):

    from ctxmesh import agent

    client = agent.from_env()                 # reads the launcher-injected env

    client.memory.get(); client.memory.put(entries)
    client.memory.append(entry); client.memory.search(query)

    client.tools.list()                       # live discovery manifest (:2999)
    client.tools.call(name, **args)           # invoke a bound MCP tool

    client.feedback.score(trace_id, name, value, comment=None)   # :2995

The ``model`` client and the ``trace.*`` step-tracing helpers are m10.3 and are
deliberately NOT part of this package yet.
"""

from ctxmesh import agent
from ctxmesh.client import Client
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import (
    ConfigError,
    CtxmeshError,
    EndpointError,
    NotInPodError,
)

__all__ = [
    "agent",
    "Client",
    "PlaneConfig",
    "CtxmeshError",
    "ConfigError",
    "NotInPodError",
    "EndpointError",
]

__version__ = "0.1.0"
