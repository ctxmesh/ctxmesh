"""
sitecustomize.py — OpenInference auto-instrumentation bootstrap.

Automatically imported by the Python interpreter at start-up via the site
module mechanism (placed in site-packages by the base-python Dockerfile).
Runs before any user code; registers OpenInference instrumentors and wires
an OTLP/gRPC trace exporter so framework spans are emitted without any SDK
calls in agent code.

Design contract
---------------
* **Best-effort only.** Any failure — collector unreachable, package missing,
  import error — is logged as a warning; execution continues normally.  The
  agent MUST NOT crash because of instrumentation.
* **W3C TraceContext propagation** is installed globally so that the launcher's
  injected ``traceparent`` header is continued by child spans (LangChain chain,
  LLM, tool), forming the full trace tree defined in specs/observability.md.
* **OTLP/gRPC, non-blocking.** The gRPC channel is lazy (no blocking dial at
  startup).  The BatchSpanProcessor drops spans asynchronously when the
  collector is unreachable — requests are never slowed down.
* Service identity comes from ``AGENT_NAME`` / ``AGENT_VERSION`` env vars
  (injected by the controller); defaults to ``python-agent`` / ``unknown``.

Environment variables read
--------------------------
OTEL_EXPORTER_OTLP_ENDPOINT  gRPC target, default ``localhost:4317``
                              (controller injects the OTel collector sidecar
                              at that address — specs/observability.md).
AGENT_NAME                    Sets ``service.name`` OTel resource attribute.
AGENT_VERSION                 Sets ``service.version`` OTel resource attribute.
"""

from __future__ import annotations

import logging
import os

_LOG = logging.getLogger("openinference.bootstrap")


def _setup() -> None:
    """Entry point — called once at interpreter start-up."""
    try:
        _configure_otel()
    except Exception as exc:  # noqa: BLE001
        _LOG.warning(
            "openinference bootstrap: OTel setup failed (tracing disabled): %s", exc
        )
        return

    # Instrumentors are registered independently — a missing framework package
    # (e.g. Anthropic not installed) must not block the others.
    for _fn in (_instrument_langchain, _instrument_openai, _instrument_anthropic):
        try:
            _fn()
        except Exception as exc:  # noqa: BLE001
            _LOG.debug("openinference bootstrap: %s skipped: %s", _fn.__name__, exc)


def _configure_otel() -> None:
    """Initialise the TracerProvider + OTLP/gRPC batch exporter."""
    # Import here so a missing package surfaces as a warning, not a top-level
    # ImportError that would prevent the module from loading at all.
    from opentelemetry import trace  # noqa: PLC0415
    from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import (  # noqa: PLC0415
        OTLPSpanExporter,
    )
    from opentelemetry.propagate import set_global_textmap  # noqa: PLC0415
    from opentelemetry.propagators.composite import (  # noqa: PLC0415
        CompositePropagator,
    )
    from opentelemetry.sdk.resources import Resource  # noqa: PLC0415
    from opentelemetry.sdk.trace import TracerProvider  # noqa: PLC0415
    from opentelemetry.sdk.trace.export import BatchSpanProcessor  # noqa: PLC0415
    from opentelemetry.trace.propagation.tracecontext import (  # noqa: PLC0415
        TraceContextTextMapPropagator,
    )

    endpoint = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")

    resource = Resource(
        attributes={
            "service.name": os.environ.get("AGENT_NAME", "python-agent"),
            "service.version": os.environ.get("AGENT_VERSION", "unknown"),
        }
    )

    # insecure=True: plaintext gRPC — appropriate for the in-pod sidecar.
    # The connection is non-blocking (lazy gRPC channel); collector-down
    # situations cause silent span drops, not request failures.
    exporter = OTLPSpanExporter(endpoint=endpoint, insecure=True)
    provider = TracerProvider(resource=resource)
    provider.add_span_processor(BatchSpanProcessor(exporter))
    trace.set_tracer_provider(provider)

    # W3C TraceContext: continues the launcher's ``traceparent`` so child spans
    # (LangChain chain, LLM, tool) nest beneath the agent.invoke boundary span.
    set_global_textmap(CompositePropagator([TraceContextTextMapPropagator()]))

    _LOG.debug(
        "openinference bootstrap: OTel configured (endpoint=%s, service=%s)",
        endpoint,
        resource.attributes.get("service.name"),
    )


def _instrument_langchain() -> None:
    from openinference.instrumentation.langchain import LangChainInstrumentor  # noqa: PLC0415

    LangChainInstrumentor().instrument()
    _LOG.debug("openinference bootstrap: LangChainInstrumentor registered")


def _instrument_openai() -> None:
    from openinference.instrumentation.openai import OpenAIInstrumentor  # noqa: PLC0415

    OpenAIInstrumentor().instrument()
    _LOG.debug("openinference bootstrap: OpenAIInstrumentor registered")


def _instrument_anthropic() -> None:
    from openinference.instrumentation.anthropic import AnthropicInstrumentor  # noqa: PLC0415

    AnthropicInstrumentor().instrument()
    _LOG.debug("openinference bootstrap: AnthropicInstrumentor registered")


_setup()
