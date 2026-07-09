"""Step-tracing helpers — the M10 core (the 🧪).

A custom, no-framework agent loop has no inferable step boundaries, so its deep
``step → tool → model`` trace tree must be emitted explicitly. These helpers do
that, producing a tree **structurally identical** to what OpenInference
auto-instrumentation gives a framework agent (same span kinds + attribute keys —
see :mod:`ctxmesh._semconv`), exported over the same OTLP/gRPC path to the
collector (:mod:`ctxmesh._tracing`).

Usage (spec "Step-tracing helpers")::

    # bind the inbound request so the whole tree roots under agent.invoke
    with client.trace.request_context(request.headers):
        with client.trace.step("plan") as step:            # CHAIN span
            step.set_input(user_prompt)
            plan = client.model.chat(model, messages)       # nested LLM span
            with client.trace.tool("web_search", args) as t:  # TOOL span (child)
                results = client.tools.call("web_search", **args)
                t.set_output(results)
            step.set_output(plan)

**The invariant (must root under ``agent.invoke``).** The launcher proxy starts
the ``agent.invoke`` server span and injects its W3C ``traceparent`` on the
inbound ``/invoke`` request (``cmd/launcher/proxy.go``). ``request_context`` (or
``loop(..., headers=...)``) extracts that context and activates it, so every
step/tool/llm span created inside is a **child of ``agent.invoke``** — same trace
id, correct parent span id — not a detached root. This is the crux the m10.5 e2e
asserts.

Nesting is by ordinary OTel context: entering a ``step`` activates its span, so a
``tool``/``llm`` opened inside becomes its child; leaving restores the parent.
"""

from __future__ import annotations

import json
from contextlib import contextmanager
from types import TracebackType
from typing import Any, ContextManager, Dict, Iterator, Optional, Type

from opentelemetry import context as otel_context
from opentelemetry import trace as otel_trace
from opentelemetry.sdk.trace import SpanProcessor
from opentelemetry.trace import Span, Status, StatusCode, Tracer

from ctxmesh import _semconv, _tracing
from ctxmesh.config import PlaneConfig


def _to_value(value: Any) -> str:
    """Render an input/output payload for an ``input.value``/``output.value`` attr.

    OpenInference stores these as a single string attribute. A str passes through
    verbatim (so a plain prompt/answer reads naturally in Langfuse); anything else
    is compact-JSON-encoded, falling back to ``str()`` for a non-serialisable
    object so a rich payload never crashes the trace.
    """
    if isinstance(value, str):
        return value
    try:
        return json.dumps(value, separators=(",", ":"), default=str)
    except (TypeError, ValueError):
        return str(value)


class SpanHandle:
    """A thin, safe handle over an active span with ``set_input``/``set_output``.

    Setting an attribute on an ended or non-recording span is a no-op (never an
    error) so a telemetry blip cannot crash the loop.
    """

    __slots__ = ("_span",)

    def __init__(self, span: Span):
        self._span = span

    @property
    def span(self) -> Span:
        return self._span

    def set_input(self, value: Any) -> SpanHandle:
        """Set the OpenInference ``input.value`` for this span."""
        if self._span.is_recording():
            self._span.set_attribute(_semconv.INPUT_VALUE, _to_value(value))
        return self

    def set_output(self, value: Any) -> SpanHandle:
        """Set the OpenInference ``output.value`` for this span."""
        if self._span.is_recording():
            self._span.set_attribute(_semconv.OUTPUT_VALUE, _to_value(value))
        return self

    def set_attribute(self, key: str, value: Any) -> SpanHandle:
        """Set an arbitrary attribute (escape hatch for extra OpenInference keys)."""
        if self._span.is_recording():
            self._span.set_attribute(key, value)
        return self


class _RequestContext:
    """Context manager that activates the inbound request's OTel context.

    On ``__enter__`` it attaches the context extracted from the request headers
    (carrying the launcher's ``agent.invoke`` ``traceparent``); on ``__exit__`` it
    detaches, restoring the prior context. Spans opened inside therefore parent
    to ``agent.invoke``.
    """

    __slots__ = ("_ctx", "_token")

    def __init__(self, ctx: otel_context.Context):
        self._ctx = ctx
        self._token: Optional[object] = None

    def __enter__(self) -> _RequestContext:
        self._token = otel_context.attach(self._ctx)
        return self

    def __exit__(
        self,
        exc_type: Optional[Type[BaseException]],
        exc: Optional[BaseException],
        tb: Optional[TracebackType],
    ) -> None:
        if self._token is not None:
            otel_context.detach(self._token)
            self._token = None


class TraceClient:
    """Emits the custom-loop ``step → tool → model`` tree (``client.trace``).

    Construction installs the W3C propagator and resolves the SDK tracer. Tests
    pass a ``span_processor`` (an in-memory exporter) so the emitted spans can be
    captured and asserted; in-pod the tracer exports over OTLP/gRPC to
    ``$OTEL_EXPORTER_OTLP_ENDPOINT``.
    """

    def __init__(
        self,
        config: PlaneConfig,
        *,
        span_processor: Optional[SpanProcessor] = None,
        endpoint: Optional[str] = None,
    ):
        self._config = config
        _tracing.install_propagator()
        self._tracer: Tracer = _tracing.get_tracer(
            span_processor=span_processor, endpoint=endpoint
        )

    # ── binding the inbound request (the invariant) ───────────────────────────
    def request_context(self, headers: Optional[Dict[str, str]]) -> _RequestContext:
        """Bind the inbound ``/invoke`` request so the tree roots under ``agent.invoke``.

        Pass the request's headers (the launcher injected the W3C ``traceparent``);
        everything traced inside the ``with`` block becomes a child of the
        launcher's ``agent.invoke`` span. Without this bind, the SDK's spans would
        start a new, detached trace — a FAIL for the M10 invariant. An absent
        ``traceparent`` (e.g. a locally-run loop) is fine: the block simply starts
        a fresh root trace.
        """
        return _RequestContext(_tracing.extract_context(headers))

    # ── span-emitting context managers ────────────────────────────────────────
    @contextmanager
    def loop(
        self, name: str = "agent", *, headers: Optional[Dict[str, str]] = None
    ) -> Iterator[SpanHandle]:
        """The loop-root span — an OpenInference ``AGENT`` span.

        Convenience over ``request_context`` + a top ``AGENT`` span: passing
        *headers* binds the inbound request context (rooting under ``agent.invoke``)
        for the duration of the loop, so a custom loop can write::

            with client.trace.loop("research", headers=req.headers) as root:
                ...  # steps/tools/llms nest under this AGENT span
        """
        if headers is not None:
            with self.request_context(headers), self._span(
                name, _semconv.KIND_AGENT
            ) as handle:
                yield handle
        else:
            with self._span(name, _semconv.KIND_AGENT) as handle:
                yield handle

    def step(self, name: str) -> "_SpanCM":
        """A reasoning step — an OpenInference ``CHAIN`` span.

        The unit of a custom loop's plan. ``tool``/``llm`` spans opened inside it
        nest beneath it via OTel context.
        """
        return _SpanCM(self, name, _semconv.KIND_CHAIN)

    def tool(self, name: str, input: Any = None) -> "_SpanCM":  # noqa: A002 - matches spec API
        """A tool call — an OpenInference ``TOOL`` span with ``tool.name`` + input.

        Wraps a ``client.tools.call`` (or any tool invocation) so it appears as a
        ``TOOL`` node under the current step, exactly like an auto-instrumented
        framework tool span.
        """
        return _SpanCM(
            self,
            name,
            _semconv.KIND_TOOL,
            attributes={_semconv.TOOL_NAME: name},
            input_value=input,
        )

    def llm(
        self,
        name: str = "llm",
        *,
        model: Optional[str] = None,
        input: Any = None,  # noqa: A002 - matches spec API
    ) -> "_SpanCM":
        """A model call — an OpenInference ``LLM`` span.

        Custom loops that call the gateway directly (not via ``client.model.chat``,
        which already emits an ``LLM`` span) can wrap it here to get the same node.
        ``model`` sets ``llm.model_name``; token counts can be added on the handle
        via ``set_attribute`` after the call.
        """
        attributes: Dict[str, Any] = {}
        if model:
            attributes[_semconv.LLM_MODEL_NAME] = model
        return _SpanCM(
            self, name, _semconv.KIND_LLM, attributes=attributes, input_value=input
        )

    # ── internal span factory ─────────────────────────────────────────────────
    @contextmanager
    def _span(
        self,
        name: str,
        kind: str,
        *,
        attributes: Optional[Dict[str, Any]] = None,
        input_value: Any = None,
    ) -> Iterator[SpanHandle]:
        """Start a recording OpenInference span of *kind*, active for the block.

        The span is started with ``start_as_current_span`` so it becomes the
        active span — nesting (a tool/llm under a step) is automatic. On an
        exception the span is marked ERROR and the exception recorded, then
        re-raised (errors are never swallowed); the span is always ended.
        """
        attrs: Dict[str, Any] = {_semconv.SPAN_KIND: kind}
        if attributes:
            attrs.update(attributes)
        if input_value is not None:
            attrs[_semconv.INPUT_VALUE] = _to_value(input_value)

        with self._tracer.start_as_current_span(
            name,
            kind=otel_trace.SpanKind.INTERNAL,
            attributes=attrs,
        ) as span:
            handle = SpanHandle(span)
            try:
                yield handle
            except Exception as exc:
                # Record the failure on the span, but never swallow it.
                span.set_status(Status(StatusCode.ERROR, str(exc)))
                span.record_exception(exc)
                raise


class _SpanCM:
    """A re-usable ``with`` wrapper around :meth:`TraceClient._span`.

    Returned by ``step``/``tool``/``llm`` so those read as plain factory calls
    while still being context managers.
    """

    __slots__ = ("_client", "_name", "_kind", "_attributes", "_input", "_cm")

    def __init__(
        self,
        client: TraceClient,
        name: str,
        kind: str,
        *,
        attributes: Optional[Dict[str, Any]] = None,
        input_value: Any = None,
    ):
        self._client = client
        self._name = name
        self._kind = kind
        self._attributes = attributes
        self._input = input_value
        self._cm: Optional[ContextManager[SpanHandle]] = None

    def __enter__(self) -> SpanHandle:
        self._cm = self._client._span(
            self._name,
            self._kind,
            attributes=self._attributes,
            input_value=self._input,
        )
        return self._cm.__enter__()

    def __exit__(
        self,
        exc_type: Optional[Type[BaseException]],
        exc: Optional[BaseException],
        tb: Optional[TracebackType],
    ) -> Optional[bool]:
        assert self._cm is not None
        return self._cm.__exit__(exc_type, exc, tb)
