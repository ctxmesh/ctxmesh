/**
 * The mock launcher plane — `node:http` stubs of the localhost plane, at parity
 * with `sdk/python/src/ctxmesh/testing.py` (+ the `conftest.py` fixtures).
 *
 * Test a ctxmesh agent with NO cluster and NO launcher: these tiny http-server
 * stubs stand in for the real localhost plane, so a handler or a custom loop can be
 * unit-tested offline. Pair them with `PlaneConfig.forTest(...)`.
 *
 * Each stub binds an ephemeral localhost port (`baseUrl`) and records every request
 * it received (method, path, query, headers, body) so a test can assert the client
 * hit the right endpoint with the right run context. The plane the stubs fake:
 *
 *     MemoryStub    -> the :2998 M5 contract (get/put/append/search)
 *     DiscoveryStub -> the :2999 M4 contract (/tools) + a faithful inline MCP endpoint
 *     GatewayStub   -> the OpenAI-compatible model gateway (POST /chat/completions)
 *     FeedbackStub  -> the :2995 M9 contract (POST /feedback, 202/400/502)
 *
 * `startPlane()` starts the memory/discovery/feedback/gateway stubs together and
 * returns a `PlaneConfig.forTest(...)` pointed at them (the Python `plane` fixture),
 * with `stop()` to tear everything down. `InMemorySpanCollector` is a stub for the
 * later trace tests (m77.4 fills in the OTel wiring); it captures spans in-process.
 */

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

import { PlaneConfig, makeRunContext, type RunContext } from "../src/config.js";

/** A captured inbound request the stub recorded, for assertions. */
export interface RecordedRequest {
  method: string;
  path: string;
  query: URLSearchParams;
  headers: Record<string, string>;
  body: Buffer;
  json(): unknown;
}

interface StubResponse {
  status: number;
  headers?: Record<string, string>;
  body?: Buffer | string;
}

/** A route handler: (stub, request) -> the response to send. */
type Route<S extends BaseStub> = (stub: S, req: RecordedRequest) => StubResponse;

/**
 * Collapse a concrete memory path to its route template.
 *   /memory/conv-42          -> /memory/{id}
 *   /memory/conv-42/append   -> /memory/{id}/append
 *   /memory/conv-42/search   -> /memory/{id}/search
 */
export function normalisePath(path: string): string {
  const parts = path.replace(/^\/+|\/+$/g, "").split("/");
  if (parts.length >= 2 && parts[0] === "memory") {
    const tail = parts.length > 2 ? "/" + parts.slice(2).join("/") : "";
    return "/memory/{id}" + tail;
  }
  return path;
}

/** A threaded HTTP server on an ephemeral localhost port. */
export abstract class BaseStub {
  readonly requests: RecordedRequest[] = [];
  protected readonly routes = new Map<string, Route<this>>();
  private server?: Server;
  private port = 0;

  constructor() {
    this.installRoutes();
  }

  /** Subclasses register their routes here (keyed "METHOD <path-template>"). */
  protected abstract installRoutes(): void;

  get baseUrl(): string {
    return `http://127.0.0.1:${this.port}`;
  }

  async start(): Promise<this> {
    this.server = createServer((req, res) => this.dispatch(req, res));
    await new Promise<void>((resolve) => {
      this.server!.listen(0, "127.0.0.1", () => {
        this.port = (this.server!.address() as AddressInfo).port;
        resolve();
      });
    });
    return this;
  }

  async stop(): Promise<void> {
    if (!this.server) return;
    await new Promise<void>((resolve, reject) => {
      this.server!.close((err) => (err ? reject(err) : resolve()));
    });
    this.server = undefined;
  }

  private dispatch(req: IncomingMessage, res: ServerResponse): void {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const body = Buffer.concat(chunks);
      const url = new URL(req.url ?? "/", this.baseUrl);
      const headers: Record<string, string> = {};
      for (const [k, v] of Object.entries(req.headers)) {
        if (typeof v === "string") headers[k.toLowerCase()] = v;
        else if (Array.isArray(v)) headers[k.toLowerCase()] = v.join(", ");
      }
      const recorded: RecordedRequest = {
        method: req.method ?? "GET",
        path: url.pathname,
        query: url.searchParams,
        headers,
        body,
        json: () => (body.length ? JSON.parse(body.toString("utf8")) : null),
      };
      this.requests.push(recorded);

      const key = `${recorded.method} ${normalisePath(recorded.path)}`;
      const route = this.routes.get(key);
      if (!route) {
        this.respond(res, { status: 404, body: '{"error":"no stub route"}' });
        return;
      }
      this.respond(res, route(this, recorded));
    });
  }

  private respond(res: ServerResponse, r: StubResponse): void {
    const body = typeof r.body === "string" ? Buffer.from(r.body) : (r.body ?? Buffer.alloc(0));
    for (const [k, v] of Object.entries(r.headers ?? {})) res.setHeader(k, v);
    res.setHeader("Content-Length", String(body.length));
    res.writeHead(r.status);
    res.end(body);
  }
}

function jsonResponse(status: number, value: unknown): StubResponse {
  return {
    status,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(value),
  };
}

// ── memory (:2998) ──────────────────────────────────────────────────────────
/** In-memory fake of the M5 :2998 contract (per-conversationId list store). */
export class MemoryStub extends BaseStub {
  readonly store = new Map<string, unknown[]>();

  private convId(req: RecordedRequest): string {
    return req.path.replace(/^\/+|\/+$/g, "").split("/")[1] ?? "";
  }

  protected installRoutes(): void {
    this.routes.set("GET /memory/{id}", (_s, req) =>
      jsonResponse(200, this.store.get(this.convId(req)) ?? []),
    );
    this.routes.set("PUT /memory/{id}", (_s, req) => {
      this.store.set(this.convId(req), req.json() as unknown[]);
      return { status: 204 };
    });
    this.routes.set("POST /memory/{id}/append", (_s, req) => {
      const list = this.store.get(this.convId(req)) ?? [];
      list.push(req.json());
      this.store.set(this.convId(req), list);
      return { status: 204 };
    });
    this.routes.set("GET /memory/{id}/search", (_s, req) => {
      const q = req.query.get("q") ?? "";
      const entries = this.store.get(this.convId(req)) ?? [];
      const matches = entries.filter((e) => q === "" || JSON.stringify(e).includes(q));
      return jsonResponse(200, matches);
    });
  }
}

// ── discovery (:2999) + fake MCP tool endpoint ──────────────────────────────
/**
 * Fake of the M4 :2999 /tools manifest and an inline MCP tool endpoint.
 *
 * The MCP endpoint answers `initialize` (with an Mcp-Session-Id header), the
 * `initialized` notification, `tools/list` (advertising the server's REAL tool
 * names), and `tools/call` — which VALIDATES `params.name` against the advertised
 * tools and returns a JSON-RPC error for an unknown name. The catalog name
 * (`word-count`, hyphen) deliberately differs from the MCP tool name (`word_count`,
 * underscore), so a client that fails to resolve the real name is rejected — the
 * guard against a false-green (mirrors the Python DiscoveryStub).
 */
export class DiscoveryStub extends BaseStub {
  static readonly CATALOG_NAME = "word-count";
  static readonly MCP_TOOL_NAME = "word_count";

  readonly mcpCalls: Array<Record<string, unknown>> = [];
  listCalls = 0;
  private sessionCounter = 0;

  constructor(
    private readonly toolResult: Record<string, unknown> = { count: 3, server_version: "v1" },
    private readonly manifestInputSchema: Record<string, unknown> | null = null,
  ) {
    super();
  }

  get mcpEndpoint(): string {
    return `${this.baseUrl}/mcp`;
  }

  private manifest(): Record<string, unknown> {
    const tool: Record<string, unknown> = {
      name: DiscoveryStub.CATALOG_NAME,
      mode: "remote",
      endpoint: this.mcpEndpoint,
      transport: "streamable-http",
    };
    if (this.manifestInputSchema !== null) tool.inputSchema = this.manifestInputSchema;
    return { version: "stub0001", tools: [tool] };
  }

  private serverTools(): Array<Record<string, unknown>> {
    return [
      {
        name: DiscoveryStub.MCP_TOOL_NAME,
        description: "Count whitespace-separated words.",
        inputSchema: {
          type: "object",
          properties: { text: { type: "string" } },
          required: ["text"],
        },
      },
    ];
  }

  protected installRoutes(): void {
    this.routes.set("GET /tools", () => jsonResponse(200, this.manifest()));

    // POST /mcp (no trailing slash) → 307 /mcp/, mirroring FastMCP/Starlette.
    this.routes.set("POST /mcp", () => ({
      status: 307,
      headers: { Location: `${this.baseUrl}/mcp/` },
    }));

    this.routes.set("POST /mcp/", (_s, req) => {
      const msg = req.json() as { method?: string; id?: unknown; params?: Record<string, unknown> };
      switch (msg.method) {
        case "initialize": {
          this.sessionCounter += 1;
          return {
            status: 200,
            headers: {
              "Content-Type": "application/json",
              "Mcp-Session-Id": `sess-${this.sessionCounter}`,
            },
            body: JSON.stringify({
              jsonrpc: "2.0",
              id: msg.id,
              result: { protocolVersion: "2025-03-26", capabilities: {} },
            }),
          };
        }
        case "notifications/initialized":
          return { status: 202 };
        case "tools/list": {
          this.listCalls += 1;
          return jsonResponse(200, {
            jsonrpc: "2.0",
            id: msg.id,
            result: { tools: this.serverTools() },
          });
        }
        case "tools/call": {
          const params = msg.params ?? {};
          const advertised = new Set(this.serverTools().map((t) => t.name));
          if (!advertised.has(params.name)) {
            return jsonResponse(200, {
              jsonrpc: "2.0",
              id: msg.id,
              error: { code: -32602, message: `Unknown tool: ${JSON.stringify(params.name)}` },
            });
          }
          this.mcpCalls.push(params);
          // Exercise the SSE branch: reply as text/event-stream.
          const result = {
            jsonrpc: "2.0",
            id: msg.id,
            result: { content: [{ type: "text", text: JSON.stringify(this.toolResult) }] },
          };
          return {
            status: 200,
            headers: { "Content-Type": "text/event-stream" },
            body: `event: message\ndata: ${JSON.stringify(result)}\n\n`,
          };
        }
        default:
          return { status: 400, body: '{"error":"unknown method"}' };
      }
    });
  }
}

// ── model gateway ($MODEL_GATEWAY_URL) ──────────────────────────────────────
/**
 * Fake of the OpenAI-compatible model gateway (POST /chat/completions).
 *
 * Returns a canned OpenAI-style completion with a `usage` block. `forceStatus`
 * drives the error paths (a budget 402 / upstream 502). Records each request so a
 * test can assert the body and the Authorization header.
 */
export class GatewayStub extends BaseStub {
  constructor(
    private readonly opts: {
      content?: string;
      usage?: Record<string, number>;
      model?: string;
      forceStatus?: number;
    } = {},
  ) {
    super();
  }

  protected installRoutes(): void {
    this.routes.set("POST /chat/completions", () => {
      if (this.opts.forceStatus !== undefined) {
        return { status: this.opts.forceStatus, body: "gateway error\n" };
      }
      return jsonResponse(200, {
        id: "chatcmpl-stub",
        model: this.opts.model ?? "gpt-4o-mini",
        choices: [
          {
            index: 0,
            message: { role: "assistant", content: this.opts.content ?? "the answer is 42" },
          },
        ],
        usage: this.opts.usage ?? {
          prompt_tokens: 11,
          completion_tokens: 5,
          total_tokens: 16,
        },
      });
    });
  }
}

// ── feedback (:2995) ────────────────────────────────────────────────────────
/** Fake of the M9 :2995 feedback hook (202 on valid, 400/502 configurable). */
export class FeedbackStub extends BaseStub {
  constructor(private readonly opts: { forceStatus?: number } = {}) {
    super();
  }

  protected installRoutes(): void {
    this.routes.set("POST /feedback", (_s, req) => {
      if (this.opts.forceStatus !== undefined) {
        return { status: this.opts.forceStatus, body: "forced error\n" };
      }
      let payload: { traceId?: unknown };
      try {
        payload = req.json() as { traceId?: unknown };
      } catch {
        return { status: 400, body: "malformed JSON\n" };
      }
      if (!payload?.traceId) return { status: 400, body: "traceId is required\n" };
      return { status: 202 };
    });
  }
}

// ── in-memory span capture (trace tests, m77.4 fills the OTel side) ─────────
/** A minimal captured span shape the later trace tests assert on. */
export interface CapturedSpan {
  name: string;
  attributes: Record<string, unknown>;
}

/**
 * A stub in-memory span capture — the analogue of the Python `InMemorySpanExporter`
 * fixture. m77.4 wires the OTel-JS SDK to feed real spans in here; for now it is a
 * plain in-process buffer so the trace-test scaffolding compiles + resets cleanly.
 */
export class InMemorySpanCollector {
  private spans: CapturedSpan[] = [];

  record(span: CapturedSpan): void {
    this.spans.push(span);
  }

  finishedSpans(): readonly CapturedSpan[] {
    return this.spans;
  }

  reset(): void {
    this.spans = [];
  }
}

/** A started mock plane: the four stubs + a `PlaneConfig` pointed at them. */
export interface MockPlane {
  memory: MemoryStub;
  discovery: DiscoveryStub;
  feedback: FeedbackStub;
  gateway: GatewayStub;
  spans: InMemorySpanCollector;
  config: PlaneConfig;
  stop(): Promise<void>;
}

/**
 * Start the memory/discovery/feedback/gateway stubs together and return a
 * `PlaneConfig.forTest(...)` pointed at them (the Python `plane` + `client` fixture
 * shape). Call `stop()` to tear everything down.
 */
export async function startPlane(
  opts: { run?: RunContext; gateway?: ConstructorParameters<typeof GatewayStub>[0] } = {},
): Promise<MockPlane> {
  const memory = new MemoryStub();
  const discovery = new DiscoveryStub();
  const feedback = new FeedbackStub();
  const gateway = new GatewayStub(opts.gateway);
  await Promise.all([memory.start(), discovery.start(), feedback.start(), gateway.start()]);

  const config = PlaneConfig.forTest({
    memoryBaseUrl: memory.baseUrl,
    discoveryBaseUrl: discovery.baseUrl,
    feedbackBaseUrl: feedback.baseUrl,
    modelGatewayUrl: gateway.baseUrl,
    run: opts.run ?? makeRunContext({ agentName: "test-agent" }),
  });

  return {
    memory,
    discovery,
    feedback,
    gateway,
    spans: new InMemorySpanCollector(),
    config,
    async stop() {
      await Promise.all([memory.stop(), discovery.stop(), feedback.stop(), gateway.stop()]);
    },
  };
}
