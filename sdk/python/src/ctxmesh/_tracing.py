"""OpenTelemetry wiring for the step-tracing helpers (the M10 core).

This module owns the SDK's :class:`~opentelemetry.sdk.trace.TracerProvider` and
the W3C ``traceparent`` propagator. It deliberately mirrors the base-image
``images/base-python/sitecustomize.py`` bootstrap so a custom-loop tree emitted
by the SDK exports the same way an auto-instrumented framework tree does:

* **OTLP/gRPC to ``$OTEL_EXPORTER_OTLP_ENDPOINT``** (``:4317``) — the exact path
  auto-instrumentation uses, so SDK spans land in Langfuse alongside them.
* **W3C TraceContext propagator** — so the launcher-injected ``traceparent`` on
  the inbound ``/invoke`` request can be *extracted* and the SDK's step spans
  become children of the launcher's ``agent.invoke`` root (the M10 invariant),
  not a detached new trace.
* **Best-effort, non-blocking.** A telemetry blip must never crash the loop
  (spec "Edge cases"): the gRPC channel is lazy, the BatchSpanProcessor drops
  spans asynchronously when the collector is down, and *setup* failure degrades
  to a no-op provider rather than raising into agent code. This is distinct from
  the plane clients, which surface endpoint errors — a rejected memory write is
  a real error; a dropped span is not.

**Offline / test mode:** when ``OTEL_EXPORTER_OTLP_ENDPOINT`` is unset the SDK
does not open a gRPC exporter at all — it installs a no-op (console-free) or a
caller-supplied in-memory processor. Tests pass an in-memory exporter to capture
and assert the emitted spans without a live collector.
"""

from __future__ import annotations

import logging
import os
import threading
from typing import Dict, Optional

from opentelemetry import propagate
from opentelemetry.context import Context
from opentelemetry.propagators.textmap import CarrierT
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import SpanProcessor, TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.trace import Tracer
from opentelemetry.trace.propagation.tracecontext import (
    TraceContextTextMapPropagator,
)

_LOG = logging.getLogger("ctxmesh.tracing")

#: The tracer/instrumentation-scope name. Distinct from the launcher's
#: ``agentry/launcher`` scope so a trace shows which producer emitted a span,
#: while both still share the trace id via propagation.
TRACER_NAME = "ctxmesh"

#: Env var naming the OTLP/gRPC collector (spec table; base-python default).
OTLP_ENDPOINT_ENV = "OTEL_EXPORTER_OTLP_ENDPOINT"

# One provider per process. Building a TracerProvider is cheap but installing a
# BatchSpanProcessor spins a background thread, so we memoise and guard it.
_LOCK = threading.Lock()
_PROVIDER: Optional[TracerProvider] = None
_PROPAGATOR = TraceContextTextMapPropagator()


def _resource() -> Resource:
    """Service identity, mirroring the base-image bootstrap's resource attrs."""
    return Resource.create(
        {
            "service.name": os.environ.get("AGENT_NAME", "python-agent"),
            "service.version": os.environ.get("AGENT_VERSION", "unknown"),
        }
    )


def _build_provider(
    *,
    endpoint: Optional[str],
    span_processor: Optional[SpanProcessor],
) -> TracerProvider:
    """Construct a TracerProvider.

    Precedence for where spans go:
      1. an explicit *span_processor* (tests inject an in-memory exporter);
      2. else an OTLP/gRPC BatchSpanProcessor to *endpoint* if one is set;
      3. else no processor at all — spans are created (so context propagation
         and nesting still work and can be asserted) but exported nowhere. This
         is the documented offline/no-op mode.
    """
    provider = TracerProvider(resource=_resource())

    if span_processor is not None:
        provider.add_span_processor(span_processor)
        return provider

    if endpoint:
        try:
            # Import lazily so an environment without the gRPC exporter still
            # yields a working no-op provider instead of an import crash.
            from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import (  # noqa: PLC0415
                OTLPSpanExporter,
            )

            # insecure=True: plaintext gRPC to the in-pod collector sidecar,
            # exactly as sitecustomize.py does. The channel is lazy (no blocking
            # dial); a down collector drops spans asynchronously, never blocking
            # or crashing the agent loop.
            exporter = OTLPSpanExporter(endpoint=endpoint, insecure=True)
            provider.add_span_processor(BatchSpanProcessor(exporter))
        except Exception as exc:  # noqa: BLE001 - telemetry setup must never crash the loop
            _LOG.warning(
                "ctxmesh tracing: OTLP exporter to %s could not be created; "
                "step spans will not export (tracing disabled, agent continues): %s",
                endpoint,
                exc,
            )

    return provider


def get_provider(
    *,
    span_processor: Optional[SpanProcessor] = None,
    endpoint: Optional[str] = None,
    force: bool = False,
) -> TracerProvider:
    """Return the process-wide SDK TracerProvider, building it once.

    *endpoint* defaults to ``$OTEL_EXPORTER_OTLP_ENDPOINT``; pass ``span_processor``
    (tests) to capture spans in memory instead of exporting. ``force=True`` rebuilds
    (tests only) so a fresh in-memory exporter can be installed per test.
    """
    global _PROVIDER
    with _LOCK:
        if _PROVIDER is not None and not force:
            return _PROVIDER
        resolved_endpoint = endpoint if endpoint is not None else os.environ.get(OTLP_ENDPOINT_ENV)
        _PROVIDER = _build_provider(endpoint=resolved_endpoint, span_processor=span_processor)
        return _PROVIDER


def reset_provider() -> None:
    """Drop the memoised provider (tests only — lets each test install its own)."""
    global _PROVIDER
    with _LOCK:
        _PROVIDER = None


def get_tracer(
    *,
    span_processor: Optional[SpanProcessor] = None,
    endpoint: Optional[str] = None,
) -> Tracer:
    """A :class:`Tracer` from the SDK provider (never the global no-op provider).

    Using the SDK provider directly (not ``trace.get_tracer``) keeps the SDK's
    spans working even if agent code never installed a global provider — the
    step helpers are self-contained.
    """
    return get_provider(span_processor=span_processor, endpoint=endpoint).get_tracer(TRACER_NAME)


def extract_context(headers: Optional[Dict[str, str]]) -> Context:
    """Extract an OTel :class:`Context` from inbound request headers.

    Reads the W3C ``traceparent``/``tracestate`` the launcher proxy injects on
    every ``/invoke`` (``cmd/launcher/proxy.go``: ``prop.Inject`` after starting
    the ``agent.invoke`` server span). Binding this context under the step-tracing
    root is what roots the SDK's tree beneath ``agent.invoke`` rather than
    starting a detached trace. Header lookup is case-insensitive (the W3C
    propagator lower-cases keys); an absent/empty ``traceparent`` yields the
    current (empty) context, so the loop simply starts a new root — never an
    error.
    """
    carrier: CarrierT = {}
    for key, value in (headers or {}).items():
        carrier[key.lower()] = value
    return _PROPAGATOR.extract(carrier=carrier, context=Context())


def install_propagator() -> None:
    """Install the W3C TraceContext propagator globally (idempotent).

    Mirrors the base-image bootstrap so ``propagate.extract/inject`` and any
    downstream auto-instrumentation share the same wire format. Called once when
    a :class:`~ctxmesh.trace.TraceClient` is constructed.
    """
    propagate.set_global_textmap(TraceContextTextMapPropagator())


def current_traceparent() -> Optional[str]:
    """The active span's W3C ``traceparent`` (for correlating out-of-band calls).

    Returns ``None`` when there is no recording span in context. Used by the
    model client so a gateway call it makes can be correlated, and by callers
    that need the trace id for feedback scoring.
    """
    carrier: Dict[str, str] = {}
    _PROPAGATOR.inject(carrier)
    return carrier.get("traceparent")
