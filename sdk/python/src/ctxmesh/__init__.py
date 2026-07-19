"""ctxmesh — the agent-engine Python SDK.

Typed, optional sugar over the launcher's language-agnostic localhost platform
plane (ADR 0002). Every capability the SDK exposes is *also* a raw launcher
endpoint; the SDK never adds a capability the plane does not have — it only
removes the raw-HTTP boilerplate and applies the run context.

Surface:

    from ctxmesh import agent

    client = agent.from_env()                 # reads the launcher-injected env

    client.memory.get(); client.memory.put(entries)
    client.memory.append(entry); client.memory.search(query)

    client.tools.list()                       # live discovery manifest (:2999)
    client.tools.call(name, **args)           # invoke a bound MCP tool

    client.feedback.score(trace_id, name, value, comment=None)   # :2995

    client.model.chat(model, messages, **opts)  # $MODEL_GATEWAY_URL; emits LLM span

    # step-tracing helpers for a custom loop (the M10 core). Bind the inbound
    # request so the tree roots under the launcher's agent.invoke span:
    with client.trace.request_context(request.headers):
        with client.trace.step("plan") as step:            # CHAIN span
            plan = client.model.chat(model, messages)       # nested LLM span
            with client.trace.tool("search", args) as t:    # TOOL span (child)
                t.set_output(client.tools.call("search", **args))
            step.set_output(plan)
"""

from ctxmesh import agent
from ctxmesh._approval import pause_for_approval
from ctxmesh.client import Client
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import (
    ApprovalRequiredError,
    ConfigError,
    CtxmeshError,
    EndpointError,
    NotInPodError,
)
from ctxmesh.managed import (
    DEFAULT_MAX_STEPS,
    ManagedConfig,
    ManagedResult,
    mint_conversation_id,
    run_managed_loop,
)
from ctxmesh.model import ChatResponse, ModelClient
from ctxmesh.runs import Run, RunEvent, RunsClient
from ctxmesh.trace import SpanHandle, TraceClient

__all__ = [
    "agent",
    "Client",
    "PlaneConfig",
    "CtxmeshError",
    "ConfigError",
    "NotInPodError",
    "EndpointError",
    "ModelClient",
    "ChatResponse",
    "TraceClient",
    "SpanHandle",
    "run_managed_loop",
    "mint_conversation_id",
    "ManagedConfig",
    "ManagedResult",
    "DEFAULT_MAX_STEPS",
    "pause_for_approval",
    "ApprovalRequiredError",
    "RunsClient",
    "Run",
    "RunEvent",
]

__version__ = "0.1.0"
