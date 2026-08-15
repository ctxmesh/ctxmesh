"""The Client facade: one object holding the localhost-plane clients.

Constructed by :func:`ctxmesh.agent.from_env` (in-pod) or
:func:`ctxmesh.agent.from_config` (tests/offline). Each attribute wraps one raw
launcher endpoint and applies the run context:

    client.memory     -> :2998               (M5)
    client.knowledge  -> :2998               (M68) — knowledge-base retrieval
    client.tools      -> :2999               (M4)
    client.feedback   -> :2995               (M9)
    client.model      -> $MODEL_GATEWAY_URL   (M2/M8) — emits an LLM span
    client.trace      -> $OTEL_EXPORTER_OTLP_ENDPOINT (:4317) — step-tracing helpers

``client.trace`` emits the custom-loop ``step → tool → model`` trace tree and,
via ``client.trace.request_context(headers)``, roots it under the launcher's
``agent.invoke`` span (the M10 invariant). ``client.model.chat`` emits the ``LLM``
node in that tree.
"""

from __future__ import annotations

from contextlib import contextmanager
from typing import Iterable, Iterator, Mapping, Optional

from opentelemetry.sdk.trace import SpanProcessor

from ctxmesh._approval import approval_scope, voucher_scope
from ctxmesh._capability import capability_scope
from ctxmesh._record import record_scope
from ctxmesh.config import PlaneConfig, RunContext
from ctxmesh.feedback import FeedbackClient
from ctxmesh.knowledge import KnowledgeClient
from ctxmesh.memory import MemoryClient
from ctxmesh.model import ModelClient
from ctxmesh.tools import ToolsClient
from ctxmesh.trace import TraceClient


class Client:
    """Typed entry point to the launcher localhost plane."""

    def __init__(
        self,
        config: PlaneConfig,
        *,
        span_processor: Optional[SpanProcessor] = None,
    ):
        self._config = config
        self.memory = MemoryClient(config)
        self.knowledge = KnowledgeClient(config)
        self.tools = ToolsClient(config)
        self.feedback = FeedbackClient(config)
        # trace must exist before model: model.chat wraps its round-trip in an
        # LLM span emitted through the trace client. The OTLP endpoint comes from
        # config ($OTEL_EXPORTER_OTLP_ENDPOINT); tests pass an in-memory
        # span_processor to capture spans instead of exporting over gRPC.
        self.trace = TraceClient(
            config,
            span_processor=span_processor,
            endpoint=config.otlp_endpoint or None,
        )
        self.model = ModelClient(config, self.trace)

    @contextmanager
    def request_scope(
        self,
        headers: Optional[Mapping[str, str]] = None,
        *,
        approvals: Optional[Iterable[str]] = None,
    ) -> Iterator[None]:
        """Bind the invoking user's run capability (+ any granted approvals) from the
        inbound /invoke headers for the duration of the block (DX-2).

        A CUSTOM agent loop MUST enter this — otherwise the ContextVar is unset, so every
        tool-call egress relays NO run capability and silently resolves ORG/PUBLIC creds
        instead of the user's OBO grant (a silent auth downgrade), and ``pause_for_approval``
        can never resume (its resume-binding was private). The managed loop
        (``run_managed_loop`` / ``ctxmesh.serve``) enters it for you; this makes the same
        binding public for hand-rolled loops. Pair it with
        ``client.trace.request_context(headers)`` (or use ``ctxmesh.serve``, which does both)
        to also root the trace under the launcher's agent.invoke span.

            with client.request_scope(request.headers):
                client.tools.call("search", {...})   # relays the user's capability
        """
        with (
            capability_scope(headers),
            approval_scope(approvals),
            voucher_scope(headers),
            record_scope(headers),
        ):
            yield

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
        id argument through the turn. The trace/model/tools/feedback clients are
        shared (they are not conversation-scoped).
        """
        bound = Client(self._config)
        bound.memory = MemoryClient(self._config, conversation_id=conversation_id)
        bound.trace = self.trace
        bound.model = self.model
        return bound
