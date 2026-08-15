/**
 * OpenInference semantic-convention keys the step-tracing helpers emit.
 * Parity with `sdk/python/src/ctxmesh/_semconv.py`.
 *
 * We import the constants from `@arizeai/openinference-semantic-conventions` (the JS
 * OpenInference vocabulary) rather than hardcoding string keys, so the SDK's spans are the
 * same shape the base-image OpenInference auto-instrumentation emits — a custom-loop tree
 * is then structurally identical to a framework tree (the M10 invariant). The Python SDK
 * does the same via `openinference-semantic-conventions`; both SDKs therefore emit the same
 * attribute keys + span-kind values.
 *
 * Feasibility-gate finding (m77.4, ADR 0070 §2): the JS package's `SemanticConventions`
 * object carries EVERY key we need (`openinference.span.kind`, `input.value`, `output.value`,
 * `tool.name`, `llm.model_name`, `llm.token_count.*`) and the `OpenInferenceSpanKind` enum
 * carries AGENT/CHAIN/TOOL/LLM with the exact string values Python uses — so NO literal-key
 * fallback was needed. The keys/values below are verified byte-for-byte equal to the Python
 * `_semconv.py` constants (see the m77.4 report).
 *
 *     span.kind      openinference.span.kind   (CHAIN / AGENT / TOOL / LLM)
 *     input.value    the step/tool input (JSON-serialised)
 *     output.value   the step/tool/llm output
 *     tool.name      the tool's name (TOOL spans)
 *     llm.model_name + llm.token_count.{prompt,completion,total}  (LLM spans)
 */

import {
  OpenInferenceSpanKind,
  SemanticConventions,
} from "@arizeai/openinference-semantic-conventions";

// ── attribute keys ─────────────────────────────────────────────────────────────
/** `"openinference.span.kind"` — the OpenInference span-kind attribute. */
export const SPAN_KIND: string = SemanticConventions.OPENINFERENCE_SPAN_KIND;
/** `"input.value"` — the step/tool/llm input payload. */
export const INPUT_VALUE: string = SemanticConventions.INPUT_VALUE;
/** `"output.value"` — the step/tool/llm output payload. */
export const OUTPUT_VALUE: string = SemanticConventions.OUTPUT_VALUE;
/** `"tool.name"` — the tool's name (TOOL spans). */
export const TOOL_NAME: string = SemanticConventions.TOOL_NAME;
/** `"llm.model_name"` — the model route (LLM spans). */
export const LLM_MODEL_NAME: string = SemanticConventions.LLM_MODEL_NAME;
/** `"llm.token_count.prompt"` — prompt tokens (LLM spans). */
export const LLM_TOKEN_COUNT_PROMPT: string = SemanticConventions.LLM_TOKEN_COUNT_PROMPT;
/** `"llm.token_count.completion"` — completion tokens (LLM spans). */
export const LLM_TOKEN_COUNT_COMPLETION: string =
  SemanticConventions.LLM_TOKEN_COUNT_COMPLETION;
/** `"llm.token_count.total"` — total tokens (LLM spans). */
export const LLM_TOKEN_COUNT_TOTAL: string = SemanticConventions.LLM_TOKEN_COUNT_TOTAL;

// ── span-kind values ───────────────────────────────────────────────────────────
/** `"CHAIN"` — a reasoning step. */
export const KIND_CHAIN: string = OpenInferenceSpanKind.CHAIN;
/** `"AGENT"` — the loop root. */
export const KIND_AGENT: string = OpenInferenceSpanKind.AGENT;
/** `"TOOL"` — a tool call. */
export const KIND_TOOL: string = OpenInferenceSpanKind.TOOL;
/** `"LLM"` — a model call. */
export const KIND_LLM: string = OpenInferenceSpanKind.LLM;
