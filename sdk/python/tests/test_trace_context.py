"""THE crux test: SDK step spans root under the launcher's ``agent.invoke`` span.

The launcher proxy (``cmd/launcher/proxy.go``) starts an ``agent.invoke`` SERVER
span and injects its W3C ``traceparent`` on the inbound ``/invoke`` request. The
SDK MUST extract that context and make its step/tool/llm spans **children of
``agent.invoke``** — same trace id, correct parent span id — NOT a detached new
trace. A detached trace is a FAIL for the M10 invariant (the m10.5 e2e asserts
the tree is rooted there).

We synthesize the launcher's behaviour: start an ``agent.invoke`` server span,
inject its ``traceparent`` into a headers dict, then bind that via
``client.trace.request_context(headers)`` and assert the resulting spans' trace
id + parent span id point at ``agent.invoke``.
"""

from __future__ import annotations

from opentelemetry import trace as otel_trace
from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
    InMemorySpanExporter,
)
from opentelemetry.trace.propagation.tracecontext import (
    TraceContextTextMapPropagator,
)

from ctxmesh import _tracing
from ctxmesh.client import Client


def _synthesize_agent_invoke() -> tuple:
    """Mimic the launcher: an agent.invoke SERVER span + its injected traceparent.

    Returns (invoke_span_context, headers) where ``headers`` carries the W3C
    ``traceparent`` exactly as the proxy forwards it to the user process.
    """
    tracer = _tracing.get_tracer()
    with tracer.start_as_current_span(
        "agent.invoke", kind=otel_trace.SpanKind.SERVER
    ) as invoke:
        ctx = invoke.get_span_context()
        headers: dict = {}
        TraceContextTextMapPropagator().inject(headers)
    return ctx, headers


def test_step_spans_root_under_agent_invoke(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    invoke_ctx, headers = _synthesize_agent_invoke()
    assert "traceparent" in headers  # the launcher injected it

    # The SDK binds the inbound request context, then traces a custom loop.
    with traced_client.trace.request_context(headers):
        with traced_client.trace.step("plan") as step:
            step.set_input("q")
            with traced_client.trace.tool("search", {}) as t:
                t.set_output("r")
            step.set_output("done")

    spans = {s.name: s for s in span_exporter.get_finished_spans()}
    plan = spans["plan"]

    # 1. Same trace as agent.invoke — NOT a detached new trace.
    assert plan.context.trace_id == invoke_ctx.trace_id
    # 2. plan's parent is exactly the agent.invoke span.
    assert plan.parent is not None
    assert plan.parent.span_id == invoke_ctx.span_id
    # 3. the whole subtree (tool under step) inherits the same trace id.
    assert spans["search"].context.trace_id == invoke_ctx.trace_id
    assert spans["search"].parent.span_id == plan.context.span_id


def test_loop_headers_arg_also_roots_under_agent_invoke(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    # The convenience path: client.trace.loop(name, headers=...) binds the request
    # context and opens an AGENT root — that root must also parent to agent.invoke.
    invoke_ctx, headers = _synthesize_agent_invoke()

    with traced_client.trace.loop("research", headers=headers) as root:
        root.set_input("go")
        with traced_client.trace.step("plan"):
            pass

    spans = {s.name: s for s in span_exporter.get_finished_spans()}
    research = spans["research"]
    assert research.context.trace_id == invoke_ctx.trace_id
    assert research.parent is not None
    assert research.parent.span_id == invoke_ctx.span_id
    # the AGENT root's child step stays in the same trace, under the root.
    assert spans["plan"].context.trace_id == invoke_ctx.trace_id
    assert spans["plan"].parent.span_id == research.context.span_id


def test_absent_traceparent_starts_fresh_root(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    # A locally-run loop with no inbound traceparent must NOT error — it simply
    # starts a fresh root trace (there is no agent.invoke to parent to).
    with traced_client.trace.request_context({}):
        with traced_client.trace.step("plan") as step:
            step.set_output("ok")

    plan = {s.name: s for s in span_exporter.get_finished_spans()}["plan"]
    assert plan.parent is None  # a fresh root


def test_garbage_traceparent_does_not_crash(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    # A malformed header must degrade to a fresh root, never raise.
    with traced_client.trace.request_context({"traceparent": "not-a-valid-header"}):
        with traced_client.trace.step("plan"):
            pass

    assert "plan" in {s.name: s for s in span_exporter.get_finished_spans()}


def test_model_llm_span_nests_under_step_and_roots_under_invoke(
    traced_client: Client, span_exporter: InMemorySpanExporter, monkeypatch
):
    # A model.chat call inside a step must emit an LLM span that is (a) a child of
    # the step and (b) in the same trace as agent.invoke. We stub the gateway HTTP
    # so no real call is made — the point is the span topology.
    import ctxmesh.model as model_mod

    class _FakeResp:
        status = 200

        def json(self):
            return {
                "model": "m",
                "choices": [{"message": {"content": "42"}}],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            }

        def text(self):
            return ""

    monkeypatch.setattr(model_mod._http, "request", lambda *a, **k: _FakeResp())

    invoke_ctx, headers = _synthesize_agent_invoke()
    with traced_client.trace.request_context(headers):
        with traced_client.trace.step("plan"):
            traced_client.model.chat("m", [{"role": "user", "content": "q"}])

    spans = {s.name: s for s in span_exporter.get_finished_spans()}
    plan = spans["plan"]
    llm = spans["chat m"]  # model.chat names the LLM span "chat <model>"
    assert llm.attributes["openinference.span.kind"] == "LLM"
    assert llm.parent.span_id == plan.context.span_id  # nested under the step
    assert llm.context.trace_id == invoke_ctx.trace_id  # rooted under agent.invoke
