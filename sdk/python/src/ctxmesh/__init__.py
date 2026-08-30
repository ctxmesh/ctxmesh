"""ctxmesh — the agentry Python SDK.

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

    # knowledge-base retrieval (M68, ADR 0061 Fork 3):
    client.knowledge.search("what is X?")    # POST :2998/knowledge/search

    # multimodal content-parts helpers (M68, ADR 0061 Fork 5):
    from ctxmesh import text_part, image_url, content
    msgs = [{"role": "user", "content": content(
        text_part("What is in this image?"),
        image_url("https://example.com/photo.jpg"),
    )}]

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
from ctxmesh._multimodal import content, image_url, text_part
from ctxmesh.client import Client
from ctxmesh.config import PlaneConfig
from ctxmesh.errors import (
    ApprovalRequiredError,
    ConfigError,
    CtxmeshError,
    EndpointError,
    GuardrailBlockedError,
    NotInPodError,
)
from ctxmesh.knowledge import KnowledgeClient
from ctxmesh.managed import (
    DEFAULT_MAX_STEPS,
    ManagedConfig,
    ManagedResult,
    mint_conversation_id,
    run_managed_loop,
)
from ctxmesh.model import ChatResponse, ModelClient
from ctxmesh.runs import Run, RunEvent, RunsClient
from ctxmesh.serve import InvokeRequest, serve
from ctxmesh.trace import SpanHandle, TraceClient

__all__ = [
    "agent",
    "Client",
    "PlaneConfig",
    "CtxmeshError",
    "ConfigError",
    "NotInPodError",
    "EndpointError",
    "GuardrailBlockedError",
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
    "serve",
    "InvokeRequest",
    # M68: knowledge-base retrieval
    "KnowledgeClient",
    # M68: multimodal content-parts helpers (ADR 0061 Fork 5)
    "text_part",
    "image_url",
    "content",
]

__version__ = "0.1.0"
