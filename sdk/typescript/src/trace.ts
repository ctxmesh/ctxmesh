/**
 * Step-tracing helpers — the M10 core (the feasibility gate).
 * Parity with `sdk/python/src/ctxmesh/trace.py`.
 *
 * A custom, no-framework agent loop has no inferable step boundaries, so its deep
 * `step → tool → model` trace tree must be emitted explicitly. These helpers do that,
 * producing a tree **structurally identical** to what OpenInference auto-instrumentation
 * gives a framework agent (same span kinds + attribute keys — see `_semconv.ts`), exported
 * over the same OTLP/gRPC path to the collector (`_tracing.ts`).
 *
 * Usage (mirrors the Python context-manager form as callback scopes — the async TS idiom):
 *
 *     // bind the inbound request so the whole tree roots under agent.invoke
 *     await client.trace.requestContext(request.headers, async () => {
 *       await client.trace.step("plan", async (step) => {       // CHAIN span
 *         step.setInput(userPrompt);
 *         const plan = await client.model.chat(model, messages); // nested LLM span
 *         await client.trace.tool("web_search", args, async (t) => { // TOOL span (child)
 *           const results = await client.tools.call("web_search", args);
 *           t.setOutput(results);
 *         });
 *         step.setOutput(plan);
 *       });
 *     });
 *
 * **The invariant (must root under `agent.invoke`).** The launcher proxy starts the
 * `agent.invoke` server span and injects its W3C `traceparent` on the inbound `/invoke`
 * request. `requestContext` (or `loop(name, headers)`) extracts that context and activates
 * it, so every step/tool/llm span created inside is a **child of `agent.invoke`** — same
 * trace id, correct parent span id — not a detached root. This is the crux the M10 e2e
 * asserts.
 *
 * Nesting is by ordinary OTel context: `startActiveSpan` makes a span the active span for
 * the duration of its callback, so a `tool`/`llm` opened inside a `step` becomes its child;
 * leaving restores the parent. Works across `await` (OTel-JS uses AsyncLocalStorage-backed
 * context under Node).
 */

import {
  context as otelContext,
  SpanStatusCode,
  type Context,
  type Span,
  type Tracer,
} from "@opentelemetry/api";

import * as semconv from "./_semconv.js";
import {
  buildProvider,
  extractContext,
  installPropagator,
  TRACER_NAME,
} from "./_tracing.js";
import type { PlaneConfig } from "./config.js";

/**
 * Render an input/output payload for an `input.value`/`output.value` attribute.
 *
 * OpenInference stores these as a single string attribute. A string passes through verbatim
 * (so a plain prompt/answer reads naturally in Langfuse); anything else is compact-JSON
 * encoded, falling back to `String()` for a non-serialisable object so a rich payload never
 * crashes the trace. Mirrors Python's `_to_value`.
 */
export function toValue(value: unknown): string {
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

/**
 * A thin, safe handle over an active span with `setInput`/`setOutput`/`setAttribute`.
 *
 * Setting an attribute on an ended or non-recording span is a no-op (never an error) so a
 * telemetry blip cannot crash the loop. Mirrors Python's `SpanHandle`.
 */
export class SpanHandle {
  private readonly _span: Span;

  constructor(span: Span) {
    this._span = span;
  }

  get span(): Span {
    return this._span;
  }

  /** Set the OpenInference `input.value` for this span. */
  setInput(value: unknown): this {
    if (this._span.isRecording()) {
      this._span.setAttribute(semconv.INPUT_VALUE, toValue(value));
    }
    return this;
  }

  /** Set the OpenInference `output.value` for this span. */
  setOutput(value: unknown): this {
    if (this._span.isRecording()) {
      this._span.setAttribute(semconv.OUTPUT_VALUE, toValue(value));
    }
    return this;
  }

  /** Set an arbitrary attribute (escape hatch for extra OpenInference keys). */
  setAttribute(key: string, value: string | number | boolean): this {
    if (this._span.isRecording()) {
      this._span.setAttribute(key, value);
    }
    return this;
  }
}

/** A scope callback receiving the active span's handle. */
export type SpanScope<T> = (handle: SpanHandle) => T;

/**
 * Emits the custom-loop `step → tool → model` tree (`client.trace`).
 *
 * Construction installs the W3C propagator and builds the SDK provider. Tests pass a
 * `spanProcessor` (an in-memory exporter processor) so the emitted spans can be captured
 * and asserted; in-pod the provider exports over OTLP/gRPC to `$OTEL_EXPORTER_OTLP_ENDPOINT`.
 */
export class TraceClient {
  private readonly tracer: Tracer;

  constructor(
    config: PlaneConfig,
    opts: {
      spanProcessor?: import("@opentelemetry/sdk-trace-base").SpanProcessor;
      endpoint?: string;
    } = {},
  ) {
    installPropagator();
    const provider = buildProvider({
      spanProcessor: opts.spanProcessor,
      endpoint: opts.endpoint,
      serviceName: config.run.agentName,
      serviceVersion: config.run.agentVersion,
    });
    this.tracer = provider.getTracer(TRACER_NAME);
  }

  // ── binding the inbound request (the invariant) ───────────────────────────
  /**
   * Bind the inbound `/invoke` request so the tree roots under `agent.invoke`, for the
   * duration of `fn`.
   *
   * Pass the request's headers (the launcher injected the W3C `traceparent`); everything
   * traced inside `fn` becomes a child of the launcher's `agent.invoke` span. Without this
   * bind, the SDK's spans would start a new, detached trace — a FAIL for the M10 invariant.
   * An absent `traceparent` (e.g. a locally-run loop) is fine: `fn` simply starts a fresh
   * root trace. Async-aware — the bound context is preserved across `await` inside `fn`.
   */
  requestContext<T>(
    headers: Record<string, string> | Headers | undefined,
    fn: () => T,
  ): T {
    const ctx = extractContext(headers);
    return otelContext.with(ctx, fn);
  }

  // ── span-emitting scopes ──────────────────────────────────────────────────
  /**
   * The loop-root span — an OpenInference `AGENT` span, active for `fn`.
   *
   * Convenience over `requestContext` + a top `AGENT` span: passing *headers* binds the
   * inbound request context (rooting under `agent.invoke`) for the duration of the loop, so
   * a custom loop can write:
   *
   *     await client.trace.loop("research", req.headers, async (root) => { ... });
   */
  loop<T>(
    name: string,
    headers: Record<string, string> | Headers | undefined,
    fn: SpanScope<T>,
  ): T {
    if (headers !== undefined) {
      return this.requestContext(headers, () =>
        this.span(name, semconv.KIND_AGENT, {}, undefined, fn),
      );
    }
    return this.span(name, semconv.KIND_AGENT, {}, undefined, fn);
  }

  /**
   * A reasoning step — an OpenInference `CHAIN` span, active for `fn`.
   *
   * The unit of a custom loop's plan. `tool`/`llm` spans opened inside `fn` nest beneath it
   * via OTel context.
   */
  step<T>(name: string, fn: SpanScope<T>): T {
    return this.span(name, semconv.KIND_CHAIN, {}, undefined, fn);
  }

  /**
   * A tool call — an OpenInference `TOOL` span with `tool.name` + input, active for `fn`.
   *
   * Wraps a `client.tools.call` (or any tool invocation) so it appears as a `TOOL` node
   * under the current step, exactly like an auto-instrumented framework tool span.
   */
  tool<T>(name: string, input: unknown, fn: SpanScope<T>): T {
    return this.span(name, semconv.KIND_TOOL, { [semconv.TOOL_NAME]: name }, input, fn);
  }

  /**
   * A model call — an OpenInference `LLM` span, active for `fn`.
   *
   * Custom loops that call the gateway directly can wrap it here to get the same node.
   * `model` sets `llm.model_name`; token counts can be added on the handle via
   * `setAttribute` after the call.
   */
  llm<T>(
    name: string,
    model: string | undefined,
    input: unknown,
    fn: SpanScope<T>,
  ): T {
    const attrs: Record<string, string> = {};
    if (model) attrs[semconv.LLM_MODEL_NAME] = model;
    return this.span(name, semconv.KIND_LLM, attrs, input, fn);
  }

  // ── internal span factory ──────────────────────────────────────────────────
  /**
   * Start a recording OpenInference span of *kind*, active for `fn`.
   *
   * The span is started with `startActiveSpan` so it becomes the active span — nesting (a
   * tool/llm under a step) is automatic. On a thrown error (or a rejected Promise) the span
   * is marked ERROR and the exception recorded, then re-raised (errors are never swallowed);
   * the span is always ended. Handles both sync and async `fn` (a Promise is awaited so the
   * span outlives the async work and ends after it settles).
   */
  private span<T>(
    name: string,
    kind: string,
    attributes: Record<string, string>,
    inputValue: unknown,
    fn: SpanScope<T>,
  ): T {
    const attrs: Record<string, string> = { [semconv.SPAN_KIND]: kind, ...attributes };
    if (inputValue !== undefined && inputValue !== null) {
      attrs[semconv.INPUT_VALUE] = toValue(inputValue);
    }
    return this.tracer.startActiveSpan(name, { attributes: attrs }, (span) => {
      const handle = new SpanHandle(span);
      let result: T;
      try {
        result = fn(handle);
      } catch (err) {
        recordError(span, err);
        span.end();
        throw err;
      }
      // Async path: keep the span open until the promise settles, then end it.
      if (isPromise(result)) {
        return result.then(
          (value) => {
            span.end();
            return value;
          },
          (err: unknown) => {
            recordError(span, err);
            span.end();
            throw err;
          },
        ) as T;
      }
      span.end();
      return result;
    });
  }
}

// ── helpers ────────────────────────────────────────────────────────────────

/** Mark a span ERROR + record the exception (never swallowing it). */
function recordError(span: Span, err: unknown): void {
  const message = err instanceof Error ? err.message : String(err);
  span.setStatus({ code: SpanStatusCode.ERROR, message });
  if (err instanceof Error) {
    span.recordException(err);
  } else {
    span.recordException(message);
  }
}

/** Narrow an unknown value to a thenable (so we await async span scopes). */
function isPromise(value: unknown): value is Promise<unknown> {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as { then?: unknown }).then === "function"
  );
}

/** Re-export the extracted-context helper for callers that need the raw OTel context. */
export function extractRequestContext(
  headers: Record<string, string> | Headers | undefined,
): Context {
  return extractContext(headers);
}
