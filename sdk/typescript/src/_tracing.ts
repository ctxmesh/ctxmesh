/**
 * OpenTelemetry-JS wiring for the step-tracing helpers (the M10 core).
 * Parity with `sdk/python/src/ctxmesh/_tracing.py`.
 *
 * This module owns the SDK's `NodeTracerProvider` and the W3C `traceparent` propagator. It
 * mirrors the base-image bootstrap so a custom-loop tree emitted by the SDK exports the same
 * way an auto-instrumented framework tree does:
 *
 * - **OTLP/gRPC to `$OTEL_EXPORTER_OTLP_ENDPOINT`** (`:4317`) — the same path
 *   auto-instrumentation uses, so SDK spans land in Langfuse alongside them.
 * - **W3C TraceContext propagator** — so the launcher-injected `traceparent` on the inbound
 *   `/invoke` request can be *extracted* and the SDK's step spans become children of the
 *   launcher's `agent.invoke` root (the M10 invariant), not a detached new trace.
 * - **Best-effort, non-blocking.** A telemetry blip must never crash the loop: exporter
 *   setup failure degrades to a no-op (no exporter) rather than throwing into agent code.
 *
 * **Offline / test mode:** when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset the SDK does not open
 * a gRPC exporter at all — it installs a no-op provider (spans are still CREATED, so context
 * propagation + nesting work and can be asserted, but exported nowhere). Tests pass an
 * in-memory `SpanProcessor` to capture the emitted spans without a live collector — the
 * analogue of the Python `InMemorySpanExporter` fixture.
 *
 * OTel-JS 2.x API notes (feasibility-gate findings, m77.4):
 * - a `Resource` is built via `resourceFromAttributes()` (not `new Resource()`);
 * - span processors are passed in the `NodeTracerProvider` constructor `spanProcessors`
 *   (the 2.x `addSpanProcessor` was removed);
 * - `ReadableSpan.parentSpanId` was replaced by `ReadableSpan.parentSpanContext` (a
 *   `SpanContext`); the mock-plane capture + tests read `.parentSpanContext.spanId`.
 */

import {
  context as otelContext,
  propagation,
  ROOT_CONTEXT,
  type Context,
} from "@opentelemetry/api";
import { W3CTraceContextPropagator } from "@opentelemetry/core";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-grpc";
import { resourceFromAttributes } from "@opentelemetry/resources";
import { NodeTracerProvider } from "@opentelemetry/sdk-trace-node";
import { BatchSpanProcessor, type SpanProcessor } from "@opentelemetry/sdk-trace-base";

/**
 * The tracer/instrumentation-scope name. Distinct from the launcher's
 * `agent-engine/launcher` scope so a trace shows which producer emitted a span, while both
 * still share the trace id via propagation.
 */
export const TRACER_NAME = "ctxmesh";

/** Env var naming the OTLP/gRPC collector (base-node default `localhost:4317`). */
export const OTLP_ENDPOINT_ENV = "OTEL_EXPORTER_OTLP_ENDPOINT";

/** The single W3C propagator used for extract/inject (matches the base-image bootstrap). */
const PROPAGATOR = new W3CTraceContextPropagator();

/**
 * Build the SDK's `NodeTracerProvider`.
 *
 * Precedence for where spans go:
 *   1. an explicit *spanProcessor* (tests inject an in-memory exporter processor);
 *   2. else an OTLP/gRPC exporter to *endpoint* if one is set;
 *   3. else no processor at all — spans are created (so context propagation and nesting
 *      still work and can be asserted) but exported nowhere. The documented offline mode.
 *
 * The built provider is `register()`ed, which installs the async-hooks context manager +
 * the W3C propagator globally. **This is REQUIRED for nesting:** without a registered
 * context manager, OTel-JS `context.with(...)` / `startActiveSpan(...)` are no-ops (the
 * active context is always ROOT), so a `tool` opened inside a `step` would NOT become its
 * child — the M10 tree would be flat. `register()` is idempotent-in-kind (re-registering
 * installs the same async-hooks manager); spans are emitted via each provider's OWN tracer,
 * so a later provider's registration does not steal an earlier client's spans (verified in
 * the m77.4 feasibility spike). The instance tracer keeps per-client span-processor isolation.
 */
export function buildProvider(opts: {
  spanProcessor?: SpanProcessor;
  endpoint?: string;
  serviceName?: string;
  serviceVersion?: string;
}): NodeTracerProvider {
  const resource = resourceFromAttributes({
    "service.name": opts.serviceName || "node-agent",
    "service.version": opts.serviceVersion || "unknown",
  });

  const processors: SpanProcessor[] = [];
  if (opts.spanProcessor) {
    processors.push(opts.spanProcessor);
  } else if (opts.endpoint) {
    const processor = buildOtlpProcessor(opts.endpoint);
    if (processor) processors.push(processor);
  }

  const provider = new NodeTracerProvider({ resource, spanProcessors: processors });
  // Install the async-hooks context manager + W3C propagator globally so `context.with` /
  // `startActiveSpan` nesting actually works (see the doc note above). The W3C propagator is
  // the same one `installPropagator()` sets — registering here keeps them consistent.
  provider.register({ propagator: new W3CTraceContextPropagator() });
  return provider;
}

/**
 * Build an OTLP/gRPC BatchSpanProcessor to *endpoint*, best-effort.
 *
 * NEVER throws — a setup failure logs and degrades to no exporter, so a telemetry problem
 * cannot crash the loop (distinct from the plane clients, which surface endpoint errors).
 * The exporter's gRPC channel is lazy (no blocking dial at construction); a down collector
 * drops spans asynchronously via the BatchSpanProcessor, never blocking the agent loop.
 */
function buildOtlpProcessor(endpoint: string): SpanProcessor | undefined {
  try {
    // The base-node collector sidecar takes plaintext gRPC; the endpoint is a host:port
    // (e.g. localhost:4317) — normalise to the URL the exporter expects.
    const url = endpoint.includes("://") ? endpoint : `http://${endpoint}`;
    const exporter = new OTLPTraceExporter({ url });
    return new BatchSpanProcessor(exporter);
  } catch (err) {
    // Telemetry setup must never crash the loop — degrade to no exporter.
    console.warn(
      `ctxmesh tracing: OTLP exporter to ${endpoint} could not be created; ` +
        `step spans will not export (tracing disabled, agent continues): ${(err as Error).message}`,
    );
    return undefined;
  }
}

/**
 * Extract an OTel `Context` from inbound request headers.
 *
 * Reads the W3C `traceparent`/`tracestate` the launcher proxy injects on every `/invoke`.
 * Binding this context under the step-tracing root is what roots the SDK's tree beneath
 * `agent.invoke` rather than starting a detached trace. Header lookup is case-insensitive;
 * an absent/empty `traceparent` yields the ROOT context, so the loop simply starts a new
 * root — never an error.
 */
export function extractContext(
  headers: Record<string, string> | Headers | undefined,
): Context {
  const carrier: Record<string, string> = {};
  if (headers) {
    if (typeof (headers as Headers).forEach === "function" && "get" in (headers as Headers)) {
      (headers as Headers).forEach((value, key) => {
        carrier[key.toLowerCase()] = value;
      });
    } else {
      for (const [key, value] of Object.entries(headers as Record<string, string>)) {
        carrier[key.toLowerCase()] = value;
      }
    }
  }
  return PROPAGATOR.extract(ROOT_CONTEXT, carrier, {
    get: (c, k) => c[k],
    keys: (c) => Object.keys(c),
  });
}

/**
 * The active span's W3C `traceparent` (for correlating out-of-band calls), or `undefined`
 * when there is no recording span in context. Used by callers that need the trace id for
 * feedback scoring or a correlated gateway call.
 */
export function currentTraceparent(): string | undefined {
  const carrier: Record<string, string> = {};
  PROPAGATOR.inject(otelContext.active(), carrier, {
    set: (c, k, v) => {
      c[k] = v;
    },
  });
  return carrier.traceparent;
}

/**
 * Install the W3C TraceContext propagator globally (idempotent).
 *
 * Mirrors the base-image bootstrap so `propagation.extract/inject` and any downstream
 * auto-instrumentation share the same wire format. Called once when a `TraceClient` is
 * constructed.
 */
export function installPropagator(): void {
  propagation.setGlobalPropagator(new W3CTraceContextPropagator());
}
