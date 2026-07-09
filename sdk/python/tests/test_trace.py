"""Step-tracing helper tests: OpenInference span kinds/attrs, nesting, errors.

These assert against an in-memory OTLP span exporter (the same shape a real
collector would receive) that the SDK emits the OpenInference ``CHAIN``/``TOOL``/
``LLM`` span kinds with the right attribute keys, and that ``tool``/``llm`` spans
nest as children of the enclosing ``step``. Context propagation (the root-under-
``agent.invoke`` invariant) is covered separately in ``test_trace_context.py``.
"""

from __future__ import annotations

import pytest
from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
    InMemorySpanExporter,
)
from opentelemetry.trace import StatusCode

from ctxmesh.client import Client


def _by_name(exporter: InMemorySpanExporter):
    return {s.name: s for s in exporter.get_finished_spans()}


def test_step_emits_chain_span_with_io(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    with traced_client.trace.step("plan") as step:
        step.set_input("do research")
        step.set_output("a plan")

    spans = _by_name(span_exporter)
    assert "plan" in spans
    plan = spans["plan"]
    assert plan.attributes["openinference.span.kind"] == "CHAIN"
    assert plan.attributes["input.value"] == "do research"
    assert plan.attributes["output.value"] == "a plan"


def test_loop_root_emits_agent_span(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    with traced_client.trace.loop("research") as root:
        root.set_output("done")

    root = _by_name(span_exporter)["research"]
    assert root.attributes["openinference.span.kind"] == "AGENT"
    assert root.attributes["output.value"] == "done"


def test_tool_emits_tool_span_with_name_and_io(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    with traced_client.trace.tool("web_search", {"q": "otel"}) as t:
        t.set_output({"hits": 3})

    span = _by_name(span_exporter)["web_search"]
    assert span.attributes["openinference.span.kind"] == "TOOL"
    assert span.attributes["tool.name"] == "web_search"
    # non-str input/output are JSON-encoded onto the single OpenInference attr.
    assert span.attributes["input.value"] == '{"q":"otel"}'
    assert span.attributes["output.value"] == '{"hits":3}'


def test_llm_emits_llm_span_with_model(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    messages = [{"role": "user", "content": "hi"}]
    with traced_client.trace.llm(model="gpt-4o-mini", input=messages) as span_h:
        span_h.set_output("hello")

    span = _by_name(span_exporter)["llm"]
    assert span.attributes["openinference.span.kind"] == "LLM"
    assert span.attributes["llm.model_name"] == "gpt-4o-mini"
    assert span.attributes["output.value"] == "hello"


def test_tool_and_llm_nest_under_step(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    with traced_client.trace.step("plan") as step:
        step.set_input("q")
        with traced_client.trace.tool("search", {"q": "x"}) as t:
            t.set_output("r")
        with traced_client.trace.llm(model="m") as ll:
            ll.set_output("a")
        step.set_output("done")

    spans = _by_name(span_exporter)
    plan_id = spans["plan"].context.span_id
    # Both child spans parent to the step, and share its trace.
    assert spans["search"].parent is not None
    assert spans["search"].parent.span_id == plan_id
    assert spans["llm"].parent is not None
    assert spans["llm"].parent.span_id == plan_id
    assert spans["search"].context.trace_id == spans["plan"].context.trace_id
    assert spans["llm"].context.trace_id == spans["plan"].context.trace_id


def test_nested_steps_form_a_tree(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    with traced_client.trace.loop("root"):
        with traced_client.trace.step("outer"):
            with traced_client.trace.step("inner"):
                pass

    spans = _by_name(span_exporter)
    root_id = spans["root"].context.span_id
    outer_id = spans["outer"].context.span_id
    assert spans["outer"].parent.span_id == root_id
    assert spans["inner"].parent.span_id == outer_id


def test_exception_in_step_marks_error_and_reraises(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    # An error inside a step is recorded on the span AND re-raised (never
    # swallowed) — the span still exports, marked ERROR.
    with pytest.raises(ValueError, match="boom"):
        with traced_client.trace.step("plan"):
            raise ValueError("boom")

    span = _by_name(span_exporter)["plan"]
    assert span.status.status_code == StatusCode.ERROR
    assert any(e.name == "exception" for e in span.events)


def test_set_input_output_on_ended_span_is_safe(
    traced_client: Client, span_exporter: InMemorySpanExporter
):
    # Holding the handle past the with-block and setting attrs must not crash
    # (a telemetry blip never breaks the loop) — it is simply a no-op.
    with traced_client.trace.step("plan") as step:
        pass
    step.set_output("late")  # span already ended; no error, no effect
    span = _by_name(span_exporter)["plan"]
    assert "output.value" not in span.attributes


def test_offline_no_op_export_does_not_crash():
    # No span_processor and no OTLP endpoint: the trace client runs in no-op
    # export mode. Tracing a full tree must succeed and export nowhere.
    from ctxmesh import _tracing, agent
    from ctxmesh.config import PlaneConfig

    _tracing.reset_provider()
    try:
        client = agent.from_config(PlaneConfig.for_test())  # otlp_endpoint=""
        with client.trace.loop("root") as root:
            root.set_input("x")
            with client.trace.step("s") as step:
                with client.trace.tool("t", {}) as tool:
                    tool.set_output("r")
                step.set_output("y")
    finally:
        _tracing.reset_provider()
