"""OpenInference semantic-convention keys the step-tracing helpers emit.

We import the constants from the ``openinference-semantic-conventions`` package
rather than hardcoding string keys, so the SDK's spans are byte-for-byte the
shape the base-image OpenInference auto-instrumentation emits — a custom-loop
tree is then structurally identical to a framework tree (the m10.5 e2e compares
the two). That package is *already* pinned in ``images/base-python`` (semconv
``0.1.30``), so depending on it here adds **zero** net footprint to base-python.

Exposing the keys/values we use behind thin module constants keeps the tracing
code readable and gives one place to see exactly which OpenInference attributes
the SDK sets:

    span.kind      openinference.span.kind   (CHAIN / AGENT / TOOL / LLM)
    input.value    the step/tool input (JSON-serialised)
    output.value   the step/tool/llm output
    tool.name      the tool's name (TOOL spans)
    llm.model_name + llm.token_count.{prompt,completion,total}  (LLM spans)
"""

from __future__ import annotations

from openinference.semconv.trace import (
    OpenInferenceSpanKindValues,
    SpanAttributes,
)

# ── attribute keys ─────────────────────────────────────────────────────────────
SPAN_KIND = SpanAttributes.OPENINFERENCE_SPAN_KIND  # "openinference.span.kind"
INPUT_VALUE = SpanAttributes.INPUT_VALUE  # "input.value"
OUTPUT_VALUE = SpanAttributes.OUTPUT_VALUE  # "output.value"
TOOL_NAME = SpanAttributes.TOOL_NAME  # "tool.name"
LLM_MODEL_NAME = SpanAttributes.LLM_MODEL_NAME  # "llm.model_name"
LLM_TOKEN_COUNT_PROMPT = SpanAttributes.LLM_TOKEN_COUNT_PROMPT
LLM_TOKEN_COUNT_COMPLETION = SpanAttributes.LLM_TOKEN_COUNT_COMPLETION
LLM_TOKEN_COUNT_TOTAL = SpanAttributes.LLM_TOKEN_COUNT_TOTAL

# ── span-kind values ───────────────────────────────────────────────────────────
KIND_CHAIN = OpenInferenceSpanKindValues.CHAIN.value  # "CHAIN"  — a reasoning step
KIND_AGENT = OpenInferenceSpanKindValues.AGENT.value  # "AGENT"  — the loop root
KIND_TOOL = OpenInferenceSpanKindValues.TOOL.value  # "TOOL"   — a tool call
KIND_LLM = OpenInferenceSpanKindValues.LLM.value  # "LLM"    — a model call
