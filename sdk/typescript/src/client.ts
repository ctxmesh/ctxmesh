/**
 * The Client facade: one object holding the localhost-plane data-plane clients.
 * Parity with `sdk/python/src/ctxmesh/client.py`.
 *
 * Constructed by `agent.fromEnv()` (in-pod) or `agent.fromConfig()` (tests/offline).
 * Each attribute wraps one raw launcher endpoint:
 *
 *     client.memory     -> :2998               (M5)
 *     client.knowledge  -> :2998               (M68) — knowledge-base retrieval
 *     client.feedback   -> :2995               (M9)
 *     client.model      -> $MODEL_GATEWAY_URL   (M2/M8)
 *     client.tools      -> :2999 (discovery) + MCP endpoints (M4/M77.3)
 *     client.trace      -> OTLP :4317           (M10/M77.4) — step-tracing helpers
 *
 * serve/managed-loop lands in M77.5.
 *
 * `withConversation(conversationId)` returns a new Client whose memory ops default
 * to that conversationId — bind it once per request so `client.memory.get()` needs
 * no repeated id argument through the turn.
 *
 * `requestScope(headers, approvals, fn)` binds the invoking user's run capability + any
 * granted approvals + the inbound trace context for the duration of `fn` — the DX-2 mandate
 * a custom loop MUST enter or its tool calls silently downgrade auth.
 */

import { approvalScope } from "./_approval.js";
import { capabilityScope } from "./_capability.js";
import { PlaneConfig } from "./config.js";
import { FeedbackClient } from "./feedback.js";
import { KnowledgeClient } from "./knowledge.js";
import { MemoryClient } from "./memory.js";
import { ModelClient } from "./model.js";
import { ToolsClient } from "./tools.js";
import { TraceClient } from "./trace.js";
import type { SpanProcessor } from "@opentelemetry/sdk-trace-base";

export class Client {
  readonly memory: MemoryClient;
  readonly knowledge: KnowledgeClient;
  readonly feedback: FeedbackClient;
  readonly model: ModelClient;
  readonly tools: ToolsClient;
  readonly trace: TraceClient;

  private readonly _config: PlaneConfig;

  constructor(config: PlaneConfig, opts: { spanProcessor?: SpanProcessor } = {}) {
    this._config = config;
    this.memory = new MemoryClient(config);
    this.knowledge = new KnowledgeClient(config);
    this.feedback = new FeedbackClient(config);
    this.model = new ModelClient(config);
    this.tools = new ToolsClient(config, this.knowledge);
    // trace exports over OTLP/gRPC to $OTEL_EXPORTER_OTLP_ENDPOINT in-pod; tests pass a
    // span processor (an in-memory exporter) to capture spans instead of exporting.
    this.trace = new TraceClient(config, {
      spanProcessor: opts.spanProcessor,
      endpoint: config.otlpEndpoint || undefined,
    });
  }

  /**
   * Bind the invoking user's run capability (+ any granted approvals) + the inbound trace
   * context from the `/invoke` headers for the duration of `fn` (DX-2).
   *
   * A CUSTOM agent loop MUST enter this — otherwise the capability store is unset, so every
   * tool-call egress relays NO run capability and silently resolves ORG/PUBLIC creds instead
   * of the user's OBO grant (a silent auth downgrade), and `pauseForApproval` can never
   * resume. The managed loop (M77.5 `serve`) enters it for you; this makes the same binding
   * public for hand-rolled loops. It also roots the trace under the launcher's `agent.invoke`
   * span (so a hand-rolled loop needs no separate `trace.requestContext` call).
   *
   *     await client.requestScope(request.headers, request.approvals, async () => {
   *       await client.tools.call("search", {...});   // relays the user's capability
   *     });
   */
  requestScope<T>(
    headers: Record<string, string> | Headers | undefined,
    approvals: Iterable<string> | undefined,
    fn: () => T,
  ): T {
    return capabilityScope(headers, () =>
      approvalScope(approvals, () => this.trace.requestContext(headers, fn)),
    );
  }

  get config(): PlaneConfig {
    return this._config;
  }

  get run() {
    return this._config.run;
  }

  /**
   * Return a client whose memory ops default to `conversationId`.
   *
   * conversationId is per-request (the agent reads it from its inbound payload).
   * Bind it once here so `client.memory.get()` needs no repeated id argument through
   * the turn. The knowledge/feedback/model/trace clients are shared (they are not
   * conversation-scoped) — crucially, the SAME trace client is reused so a
   * conversation-scoped client still writes to the caller's span processor (tests) /
   * OTLP provider, rather than building a second detached provider.
   */
  withConversation(conversationId: string): Client {
    const bound = new Client(this._config);
    // Replace only the memory client with a conversation-scoped one; share the rest.
    (bound as { memory: MemoryClient }).memory = new MemoryClient(
      this._config,
      conversationId,
    );
    (bound as { trace: TraceClient }).trace = this.trace;
    return bound;
  }
}
