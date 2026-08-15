/**
 * serve() + the managed loop — the behavioural heart of the SDK (M77.5, ADR 0070 §4).
 * Mirrors the Python `tests/test_serve.py` + `tests/test_managed.py` cases.
 *
 * Drives `serve`/`processInvoke`/`runManagedLoop` against the mock plane (`test/plane.ts`):
 *   - the /invoke envelope + parseBody tolerance + conversation-id mint/precedence
 *   - the health probes + 404s + SSE streaming + the 502-not-a-crash error path
 *   - the DX-2 fold-in: the handler runs inside requestScope (capability bound)
 *   - the stock loop dispatches a tool + emits the AGENT→step→tool→llm tree
 *   - the maxSteps runaway guard trips
 *   - approval-required / consent-required surfacing
 *   - conversation memory persisted + replayed
 *   - ManagedConfig.fromEnv reads the environment (+ AGENT_RUNTIME)
 */

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { CAPABILITY_HEADER, currentCapability } from "../src/_capability.js";
import { Client } from "../src/client.js";
import { PlaneConfig, makeRunContext } from "../src/config.js";
import { ConfigError, ConsentRequiredError } from "../src/errors.js";
import { ManagedConfig, runManagedLoop, mintConversationId } from "../src/managed.js";
import { pauseForApproval } from "../src/_approval.js";
import {
  envelope,
  makeRequestHandler,
  parseBody,
  processInvoke,
  type InvokeRequest,
} from "../src/serve.js";
import {
  DiscoveryStub,
  InMemorySpanCollector,
  MemoryStub,
} from "./plane.js";

// ── the harness m14.2 two-turn tool-call contract (mirror of tool-call-mock.py) ──
const TOOL_NAME = DiscoveryStub.CATALOG_NAME; // "word-count"; MCP name is "word_count"
const TOOL_CALL_ID = "call_mock_0";
const FINAL_MARKER = "MOCK_TOOL_OK";
const USAGE = { prompt_tokens: 11, completion_tokens: 7, total_tokens: 18 };

function hasToolResult(messages: Array<Record<string, unknown>>): boolean {
  return messages.some((m) => m["role"] === "tool");
}

function toolResultText(messages: Array<Record<string, unknown>>): string {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    if (messages[i]!["role"] === "tool") {
      const c = messages[i]!["content"];
      return typeof c === "string" ? c : JSON.stringify(c);
    }
  }
  return "";
}

function turn1ToolCallBody(): Record<string, unknown> {
  return {
    id: "chatcmpl-mock-tool",
    model: "tool-call-mock",
    choices: [
      {
        index: 0,
        finish_reason: "tool_calls",
        message: {
          role: "assistant",
          content: null,
          tool_calls: [
            {
              id: TOOL_CALL_ID,
              type: "function",
              function: { name: TOOL_NAME, arguments: JSON.stringify({ text: "ping" }) },
            },
          ],
        },
      },
    ],
    usage: USAGE,
  };
}

function turn2FinalBody(messages: Array<Record<string, unknown>>): Record<string, unknown> {
  const text = `${FINAL_MARKER} the tool returned: ${toolResultText(messages)}`.trimEnd();
  return {
    id: "chatcmpl-mock-final",
    model: "tool-call-mock",
    choices: [{ index: 0, finish_reason: "stop", message: { role: "assistant", content: text } }],
    usage: USAGE,
  };
}

/**
 * A model gateway that reproduces the m14.2 two-turn tool-call contract. Stateless like the
 * harness mock: the turn is decided from the request `messages` — a role:"tool" result present
 * → the final answer, else → the tool call. `alwaysToolCall` exercises the maxSteps guard.
 */
class ToolCallGatewayServer {
  private server?: Server;
  private port = 0;
  readonly requests: Array<Record<string, unknown>> = [];

  constructor(
    private readonly opts: { alwaysToolCall?: boolean; plainContent?: string } = {},
  ) {}

  get baseUrl(): string {
    return `http://127.0.0.1:${this.port}`;
  }

  async start(): Promise<this> {
    this.server = createServer((req: IncomingMessage, res: ServerResponse) => {
      const chunks: Buffer[] = [];
      req.on("data", (c: Buffer) => chunks.push(c));
      req.on("end", () => {
        const bodyText = Buffer.concat(chunks).toString("utf8");
        const body = bodyText ? (JSON.parse(bodyText) as Record<string, unknown>) : {};
        this.requests.push(body);
        const messages = (body["messages"] as Array<Record<string, unknown>>) ?? [];
        let resp: Record<string, unknown>;
        if (this.opts.plainContent !== undefined) {
          resp = {
            id: "chatcmpl-plain",
            model: "m",
            choices: [
              { index: 0, finish_reason: "stop", message: { role: "assistant", content: this.opts.plainContent } },
            ],
            usage: USAGE,
          };
        } else if (this.opts.alwaysToolCall || !hasToolResult(messages)) {
          resp = turn1ToolCallBody();
        } else {
          resp = turn2FinalBody(messages);
        }
        const payload = Buffer.from(JSON.stringify(resp));
        res.writeHead(200, { "Content-Type": "application/json", "Content-Length": String(payload.length) });
        res.end(payload);
      });
    });
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
    await new Promise<void>((resolve, reject) => this.server!.close((e) => (e ? reject(e) : resolve())));
    this.server = undefined;
  }
}

// ── HTTP helpers (drive a real serve() server on an ephemeral port) ─────────────

function startServer(handlerFn: (req: IncomingMessage, res: ServerResponse) => void): Promise<{ server: Server; addr: string }> {
  return new Promise((resolve) => {
    const server = createServer(handlerFn);
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address() as AddressInfo;
      resolve({ server, addr: `http://127.0.0.1:${port}` });
    });
  });
}

async function post(addr: string, path: string, body: string, headers: Record<string, string> = {}) {
  const resp = await fetch(`${addr}${path}`, { method: "POST", body, headers });
  return { status: resp.status, text: await resp.text(), contentType: resp.headers.get("content-type") ?? "" };
}

async function get(addr: string, path: string) {
  const resp = await fetch(`${addr}${path}`);
  return { status: resp.status, text: await resp.text() };
}

// ── shared plane wiring (discovery + a swappable gateway + optional memory) ─────

interface Fixture {
  gateway: ToolCallGatewayServer;
  discovery: DiscoveryStub;
  memory?: MemoryStub;
  spans: InMemorySpanCollector;
  client: Client;
  stop(): Promise<void>;
}

async function makeFixture(opts: {
  alwaysToolCall?: boolean;
  plainContent?: string;
  withMemory?: boolean;
  toolResult?: Record<string, unknown>;
  knowledgeEnabled?: boolean;
} = {}): Promise<Fixture> {
  const gateway = new ToolCallGatewayServer({ alwaysToolCall: opts.alwaysToolCall, plainContent: opts.plainContent });
  const discovery = new DiscoveryStub(opts.toolResult ?? { echo: "ping" });
  const memory = opts.withMemory ? new MemoryStub() : undefined;
  await Promise.all([gateway.start(), discovery.start(), ...(memory ? [memory.start()] : [])]);

  const spans = new InMemorySpanCollector();
  const config = PlaneConfig.forTest({
    modelGatewayUrl: gateway.baseUrl,
    discoveryBaseUrl: discovery.baseUrl,
    memoryBaseUrl: memory ? memory.baseUrl : "http://127.0.0.1:1",
    memoryWired: Boolean(memory),
    knowledgeEnabled: opts.knowledgeEnabled ?? false,
    run: makeRunContext({ agentName: "managed-test" }),
  });
  const client = new Client(config, { spanProcessor: spans.processor });

  return {
    gateway,
    discovery,
    memory,
    spans,
    client,
    async stop() {
      spans.reset();
      await Promise.all([gateway.stop(), discovery.stop(), ...(memory ? [memory.stop()] : [])]);
    },
  };
}

// ── parseBody / envelope (pure) ─────────────────────────────────────────────────

describe("parseBody — tolerant like the reference entrypoint", () => {
  it("parses input + approvals; empty/raw/non-object degrade safely", () => {
    expect(parseBody('{"input":"hello","approvals":["k1","k2"]}')).toEqual({
      input: "hello",
      approvals: ["k1", "k2"],
    });
    expect(parseBody("")).toEqual({ input: "", approvals: [] });
    expect(parseBody("raw text")).toEqual({ input: "raw text", approvals: [] });
    expect(parseBody('"just a string"')).toEqual({ input: "just a string", approvals: [] });
  });
});

describe("envelope — the /invoke response shape", () => {
  it("wraps a bare string as a single-step answer", () => {
    expect(envelope("agent-x", "answer")).toEqual({
      agent: "agent-x",
      output: "answer",
      steps: 1,
      tools_called: [],
      consent_required: [],
    });
  });

  it("carries steps/tools/consent/approval from a ManagedResult", () => {
    const body = envelope("agent-x", {
      output: "paused",
      steps: 3,
      toolsCalled: ["search", "echo"],
      consentRequired: ["scalekit"],
      approvalRequired: { key: "wire-money", summary: "send $5" },
    });
    expect(body["steps"]).toBe(3);
    expect(body["tools_called"]).toEqual(["search", "echo"]);
    expect(body["consent_required"]).toEqual(["scalekit"]);
    expect(body["approval_required"]).toEqual({ key: "wire-money", summary: "send $5" });
  });
});

// ── processInvoke: the DX-2 fold-in + conversation-id resolution ─────────────────

describe("processInvoke — request scope + conversation id", () => {
  let fx: Fixture;
  beforeEach(async () => {
    fx = await makeFixture();
  });
  afterEach(async () => {
    await fx.stop();
  });

  it("binds the invoking user's capability for the handler, then resets it", async () => {
    let seen: string | undefined = "unset";
    expect(currentCapability()).toBeUndefined();
    const handler = (req: InvokeRequest): string => {
      seen = currentCapability();
      return `echo:${req.input}`;
    };
    const body = await processInvoke(fx.client, handler, "agent-x", '{"input":"hi"}', {
      [CAPABILITY_HEADER.toLowerCase()]: "cap-serve-1",
    });
    expect(seen).toBe("cap-serve-1");
    expect(currentCapability()).toBeUndefined(); // reset on exit
    expect(body["output"]).toBe("echo:hi");
  });

  it("mints a per-run conversation id for an autonomous run", async () => {
    let cid: string | undefined;
    await processInvoke(fx.client, (req) => {
      cid = req.conversationId;
      return "";
    }, "agent-x", '{"input":"q"}', {});
    expect(cid).toMatch(/^run-/);
  });

  it("lets an inbound X-Conversation-Id take precedence (no mint)", async () => {
    let cid: string | undefined = "unset";
    await processInvoke(fx.client, (req) => {
      cid = req.conversationId;
      return "";
    }, "agent-x", '{"input":"q"}', { "x-conversation-id": "conv-42" });
    expect(cid).toBeUndefined();
  });

  it("streams tokens via emitToken when on-token is supplied", async () => {
    const frames: string[] = [];
    const handler = (req: InvokeRequest): string => {
      req.emitToken("hel");
      req.emitToken("lo");
      return "hello";
    };
    const body = await processInvoke(fx.client, handler, "agent-x", '{"input":"q"}', {}, {
      onToken: (t) => frames.push(t),
    });
    expect(frames).toEqual(["hel", "lo"]);
    expect(body["output"]).toBe("hello");
  });

  it("emitToken is a no-op without streaming", async () => {
    const body = await processInvoke(fx.client, (req) => {
      req.emitToken("x");
      return "done";
    }, "agent-x", '{"input":"q"}', {});
    expect(body["output"]).toBe("done");
  });

  it("streams step frames via emitStep when on-step is supplied (M78)", async () => {
    const frames: import("../src/managed.js").StepFrame[] = [];
    const handler = (req: InvokeRequest): string => {
      req.emitStep({ step: 1, kind: "model", tokens: { prompt: 3, completion: 2 }, ref: null });
      return "ok";
    };
    const body = await processInvoke(fx.client, handler, "agent-x", '{"input":"q"}', {}, {
      onStep: (f) => frames.push(f),
    });
    expect(frames).toEqual([
      { step: 1, kind: "model", tokens: { prompt: 3, completion: 2 }, ref: null },
    ]);
    expect(body["output"]).toBe("ok");
  });

  it("emitStep is a no-op without streaming (M78)", async () => {
    const body = await processInvoke(fx.client, (req) => {
      req.emitStep({ step: 1, kind: "model", tokens: { prompt: 0, completion: 0 }, ref: null });
      return "done";
    }, "agent-x", '{"input":"q"}', {});
    expect(body["output"]).toBe("done");
  });
});

// ── the full HTTP contract (a real serve() server) ──────────────────────────────

describe("makeRequestHandler — the HTTP contract", () => {
  let fx: Fixture;
  let server: Server;
  let addr: string;

  afterEach(async () => {
    await new Promise<void>((resolve) => server.close(() => resolve()));
    await fx.stop();
  });

  async function bringUp(handler: (req: InvokeRequest) => string | Promise<string>) {
    fx = await makeFixture();
    const started = await startServer(makeRequestHandler(fx.client, handler, "srv-agent"));
    server = started.server;
    addr = started.addr;
  }

  it("answers /healthz + /readyz with 200 {status:ok}", async () => {
    await bringUp((req) => `echo:${req.input}`);
    for (const path of ["/healthz", "/readyz"]) {
      const r = await get(addr, path);
      expect(r.status).toBe(200);
      expect(JSON.parse(r.text)).toEqual({ status: "ok" });
    }
  });

  it("404s unknown routes (GET + POST)", async () => {
    await bringUp((req) => req.input);
    expect((await get(addr, "/nope")).status).toBe(404);
    expect((await post(addr, "/nope", "{}")).status).toBe(404);
  });

  it("returns the envelope on /invoke", async () => {
    await bringUp((req) => `echo:${req.input}`);
    const r = await post(addr, "/invoke", '{"input":"ping"}');
    expect(r.status).toBe(200);
    const body = JSON.parse(r.text);
    expect(body.agent).toBe("srv-agent");
    expect(body.output).toBe("echo:ping");
  });

  it("streams SSE when Accept: text/event-stream", async () => {
    fx = await makeFixture();
    const handler = (req: InvokeRequest): string => {
      req.emitToken("a");
      req.emitStep({ step: 1, kind: "model", tokens: { prompt: 3, completion: 2 }, ref: null });
      req.emitToken("b");
      return "ab";
    };
    const started = await startServer(makeRequestHandler(fx.client, handler, "srv-agent"));
    server = started.server;
    addr = started.addr;

    const r = await post(addr, "/invoke", '{"input":"q"}', { Accept: "text/event-stream" });
    expect(r.contentType).toContain("text/event-stream");
    const frames = r.text
      .split("\n")
      .filter((l) => l.startsWith("data: "))
      .map((l) => JSON.parse(l.slice("data: ".length)));
    const tokens = frames.filter((f) => f.type === "token");
    expect(tokens).toEqual([
      { type: "token", text: "a" },
      { type: "token", text: "b" },
    ]);
    // The step frame streamed as an SSE `step` event (M78, ADR 0071 §4 — live step-visibility).
    const steps = frames.filter((f) => f.type === "step");
    expect(steps).toEqual([
      { type: "step", step: 1, kind: "model", tokens: { prompt: 3, completion: 2 }, ref: null },
    ]);
    const done = frames.filter((f) => f.type === "done");
    expect(done.length).toBe(1);
    expect(done[0].output).toBe("ab");
  });

  it("maps a handler error to a 502, not a crash", async () => {
    await bringUp(() => {
      throw new Error("kaboom");
    });
    const r = await post(addr, "/invoke", '{"input":"q"}');
    expect(r.status).toBe(502);
    expect(JSON.parse(r.text).error).toContain("kaboom");
  });
});

// ── the stock managed loop ───────────────────────────────────────────────────────

describe("runManagedLoop — the stock tool-calling loop", () => {
  it("dispatches a tool and returns the final answer (two turns)", async () => {
    const fx = await makeFixture();
    try {
      const config = new ManagedConfig({ systemPrompt: "You are a helpful assistant.", modelRoute: "tool-mock" });
      const result = await runManagedLoop(fx.client, config, "please echo ping");

      expect(result.steps).toBe(2);
      expect(result.toolsCalled).toEqual([TOOL_NAME]);
      expect(result.output.startsWith(FINAL_MARKER)).toBe(true);

      // The follow-up turn carried a role:"tool" result with the matching tool_call_id, and
      // the assistant tool-call message preceded it (OpenAI ordering).
      const followUp = fx.gateway.requests[fx.gateway.requests.length - 1]!;
      const msgs = followUp["messages"] as Array<Record<string, unknown>>;
      const toolMsgs = msgs.filter((m) => m["role"] === "tool");
      expect(toolMsgs.length).toBe(1);
      expect(toolMsgs[0]!["tool_call_id"]).toBe(TOOL_CALL_ID);
      expect(msgs.map((m) => m["role"])).toEqual(["system", "user", "assistant", "tool"]);

      // Turn 1: the config system prompt + the bound tool's schema advertised.
      const first = fx.gateway.requests[0]!;
      const firstMsgs = first["messages"] as Array<Record<string, unknown>>;
      expect(firstMsgs[0]).toEqual({ role: "system", content: "You are a helpful assistant." });
      const advertised = (first["tools"] as Array<Record<string, unknown>>).map(
        (t) => (t["function"] as Record<string, unknown>)["name"],
      );
      expect(advertised).toEqual([TOOL_NAME]);

      // The tool was actually dispatched over the MCP plane (name resolved word-count→word_count).
      expect(fx.discovery.mcpCalls.length).toBe(1);
      expect(fx.discovery.mcpCalls[0]!["name"]).toBe(DiscoveryStub.MCP_TOOL_NAME);
    } finally {
      await fx.stop();
    }
  });

  it("emits the AGENT→turn(CHAIN)→tool(TOOL)+llm(LLM) trace tree", async () => {
    const fx = await makeFixture();
    try {
      const config = new ManagedConfig({ systemPrompt: "sys", modelRoute: "tool-mock" });
      await runManagedLoop(fx.client, config, "echo please");

      const spans = fx.spans.finishedSpans();
      const kind = (name: string) => fx.spans.byName(name)?.attributes["openinference.span.kind"];
      expect(kind("managed-agent")).toBe("AGENT");

      const turns = spans.filter((s) => s.name.startsWith("turn-"));
      expect(turns.length).toBe(2);
      expect(turns.every((s) => s.attributes["openinference.span.kind"] === "CHAIN")).toBe(true);

      const toolSpans = spans.filter((s) => s.attributes["openinference.span.kind"] === "TOOL");
      expect(toolSpans.length).toBe(1);
      expect(toolSpans[0]!.attributes["tool.name"]).toBe(TOOL_NAME);

      const llmSpans = spans.filter((s) => s.attributes["openinference.span.kind"] === "LLM");
      expect(llmSpans.length).toBe(2);
    } finally {
      await fx.stop();
    }
  });

  it("emits a `step` metadata frame per boundary; ref is null when not recording (M78)", async () => {
    // Step-visibility (ADR 0071 §4/§C3): a `model` frame after each model call (with token counts)
    // and a `tool` frame per tool dispatch (with the tool name). Two-turn fixture → model, tool,
    // model. ref is null without an X-Ctxmesh-Record header (best-effort, §C3).
    const fx = await makeFixture();
    try {
      const config = new ManagedConfig({ systemPrompt: "sys", modelRoute: "tool-mock" });
      const frames: import("../src/managed.js").StepFrame[] = [];
      const result = await runManagedLoop(fx.client, config, "please echo ping", {
        onStep: (f) => frames.push(f),
      });
      expect(result.toolsCalled).toEqual([TOOL_NAME]);

      expect(frames.map((f) => [f.step, f.kind])).toEqual([
        [1, "model"],
        [1, "tool"],
        [2, "model"],
      ]);
      // Every frame carries the pinned contract shape (step int, kind, tokens block, ref).
      for (const f of frames) {
        expect(typeof f.step).toBe("number");
        expect(["model", "tool"]).toContain(f.kind);
        expect(Object.keys(f.tokens).sort()).toEqual(["completion", "prompt"]);
        expect(f.ref).toBeNull();
      }
      const modelFrames = frames.filter((f) => f.kind === "model");
      const toolFrames = frames.filter((f) => f.kind === "tool");
      // A model frame carries the response usage token counts; no `tool`.
      expect(modelFrames[0]!.tokens).toEqual({
        prompt: USAGE.prompt_tokens,
        completion: USAGE.completion_tokens,
      });
      expect(modelFrames[0]!.tool).toBeUndefined();
      // A tool frame names the dispatched tool; zero token counts.
      expect(toolFrames[0]!.tool).toBe(TOOL_NAME);
      expect(toolFrames[0]!.tokens).toEqual({ prompt: 0, completion: 0 });
    } finally {
      await fx.stop();
    }
  });

  it("populates the step frame's fixture ref when recording (M78)", async () => {
    // In record mode (X-Ctxmesh-Record stamped) the ref is a lightweight logical coordinate:
    // channel (model/tool) + the 0-based per-channel index the deferred stepper resolves against.
    const fx = await makeFixture();
    try {
      const config = new ManagedConfig({ systemPrompt: "sys", modelRoute: "tool-mock" });
      const frames: import("../src/managed.js").StepFrame[] = [];
      await runManagedLoop(fx.client, config, "please echo ping", {
        onStep: (f) => frames.push(f),
        headers: { "X-Ctxmesh-Record": "run-rec-1" },
      });
      expect(frames.map((f) => [f.kind, f.ref])).toEqual([
        ["model", { channel: "model", index: 0 }],
        ["tool", { channel: "tool", index: 0 }],
        ["model", { channel: "model", index: 1 }],
      ]);
    } finally {
      await fx.stop();
    }
  });

  it("trips the maxSteps runaway guard when the model always tool-calls", async () => {
    const fx = await makeFixture({ alwaysToolCall: true });
    try {
      const config = new ManagedConfig({ systemPrompt: "loop forever", modelRoute: "tool-mock", maxSteps: 3 });
      await expect(runManagedLoop(fx.client, config, "go")).rejects.toThrow(ConfigError);
      await expect(runManagedLoop(fx.client, config, "go")).rejects.toThrow(/maxSteps=3/);
    } finally {
      await fx.stop();
    }
  });

  it("is a plain one-turn chat agent with no tools discovered", async () => {
    const fx = await makeFixture({ plainContent: "the answer is 42" });
    try {
      // Empty discovery manifest → no tools; the loop answers in one turn.
      // (DiscoveryStub always advertises word-count, so use a plainContent gateway that never
      // tool-calls; the loop still advertises the tool but the model answers directly.)
      const config = new ManagedConfig({ systemPrompt: "sys", modelRoute: "m" });
      const result = await runManagedLoop(fx.client, config, "hello");
      expect(result.steps).toBe(1);
      expect(result.output).toBe("the answer is 42");
      expect(result.toolsCalled).toEqual([]);
    } finally {
      await fx.stop();
    }
  });
});

// ── approval-required / consent-required surfacing ───────────────────────────────

describe("runManagedLoop — HITL + consent surfacing", () => {
  it("surfaces approvalRequired for a gated tool, then resumes when granted", async () => {
    const fx = await makeFixture();
    try {
      const config = new ManagedConfig({ systemPrompt: "You are a helpful assistant.", modelRoute: "tool-mock" });
      // Model the bound tool as a sensitive action gated on approval.
      const originalCall = fx.client.tools.call.bind(fx.client.tools);
      (fx.client.tools as { call: unknown }).call = async (name: string, args?: Record<string, unknown>, opts?: unknown) => {
        pauseForApproval("echo-tool", "Run echo_tool with the given text?");
        return originalCall(name, args, opts as { timeout?: number } | undefined);
      };

      const paused = await runManagedLoop(fx.client, config, "please echo ping");
      expect(paused.approvalRequired).toEqual({ key: "echo-tool", summary: "Run echo_tool with the given text?" });
      expect(paused.toolsCalled).toEqual([]); // the gated tool did not execute

      const resumed = await runManagedLoop(fx.client, config, "please echo ping", { approvals: ["echo-tool"] });
      expect(resumed.approvalRequired).toBeUndefined();
      expect(resumed.output.startsWith(FINAL_MARKER)).toBe(true);
    } finally {
      await fx.stop();
    }
  });

  it("records consentRequired when a tool hits an unconnected MCP server", async () => {
    const fx = await makeFixture();
    try {
      const config = new ManagedConfig({ systemPrompt: "sys", modelRoute: "tool-mock" });
      (fx.client.tools as { call: unknown }).call = async () => {
        throw new ConsentRequiredError("consent required", { server: "scalekit", status: 403, body: "{}" });
      };
      const result = await runManagedLoop(fx.client, config, "please echo ping");
      // The loop threaded a consent_required tool result and continued to the mock final answer.
      expect(result.consentRequired).toEqual(["scalekit"]);
      expect(result.output.startsWith(FINAL_MARKER)).toBe(true);
      expect(result.toolsCalled).toEqual([]); // the tool never succeeded
    } finally {
      await fx.stop();
    }
  });
});

// ── conversation memory ──────────────────────────────────────────────────────────

describe("runManagedLoop — conversation memory", () => {
  it("persists the turn and replays it on the next turn", async () => {
    const fx = await makeFixture({ withMemory: true, plainContent: "the answer is 42" });
    try {
      const config = new ManagedConfig({ systemPrompt: "sys", modelRoute: "m" });
      const headers = { "x-conversation-id": "chat-1" };

      await runManagedLoop(fx.client, config, "my name is Zed", { headers });
      const turn1 = (fx.gateway.requests[0]!["messages"] as Array<Record<string, unknown>>).map((m) => m["role"]);
      expect(turn1).toEqual(["system", "user"]);
      expect(fx.memory!.store.get("chat-1")).toEqual([
        { role: "user", content: "my name is Zed" },
        { role: "assistant", content: "the answer is 42" },
      ]);

      await runManagedLoop(fx.client, config, "what is my name", { headers });
      const turn2 = fx.gateway.requests[fx.gateway.requests.length - 1]!["messages"];
      expect(turn2).toEqual([
        { role: "system", content: "sys" },
        { role: "user", content: "my name is Zed" },
        { role: "assistant", content: "the answer is 42" },
        { role: "user", content: "what is my name" },
      ]);
      expect(fx.memory!.store.get("chat-1")!.length).toBe(4);
    } finally {
      await fx.stop();
    }
  });

  it("is stateless with no conversation id (memory untouched)", async () => {
    const fx = await makeFixture({ withMemory: true, plainContent: "hi" });
    try {
      const config = new ManagedConfig({ systemPrompt: "sys", modelRoute: "m" });
      await runManagedLoop(fx.client, config, "hello"); // no headers → no conversation id
      expect(fx.memory!.store.size).toBe(0);
      const msgs = (fx.gateway.requests[0]!["messages"] as Array<Record<string, unknown>>).map((m) => m["role"]);
      expect(msgs).toEqual(["system", "user"]);
    } finally {
      await fx.stop();
    }
  });
});

// ── ManagedConfig.fromEnv ────────────────────────────────────────────────────────

describe("ManagedConfig.fromEnv — the moved-into-SDK env resolution", () => {
  it("reads SYSTEM_PROMPT / MODEL_ROUTE / MAX_STEPS", () => {
    const cfg = ManagedConfig.fromEnv({ SYSTEM_PROMPT: "be terse", MODEL_ROUTE: "gpt-4o-mini", MAX_STEPS: "5" });
    expect(cfg.systemPrompt).toBe("be terse");
    expect(cfg.modelRoute).toBe("gpt-4o-mini");
    expect(cfg.maxSteps).toBe(5);
  });

  it("has safe defaults (minimal prompt, empty route, guard-safe max steps)", () => {
    const cfg = ManagedConfig.fromEnv({});
    expect(cfg.systemPrompt).toBe("You are a helpful assistant.");
    expect(cfg.modelRoute).toBe("");
    expect(ManagedConfig.fromEnv({ MAX_STEPS: "not-a-number" }).maxSteps).toBeGreaterThanOrEqual(1);
    expect(ManagedConfig.fromEnv({ MAX_STEPS: "0" }).maxSteps).toBeGreaterThanOrEqual(1);
  });

  it("reads outputSchema / toolPolicy / resilience from AGENT_RUNTIME", () => {
    const schema = { type: "object", properties: { answer: { type: "string" } }, required: ["answer"] };
    const policy = { default: "require-approval", overrides: [{ name: "search", rule: "allow", retryable: true }] };
    const resilience = { modelCall: { timeoutSeconds: 20, maxRetries: 2 } };
    const cfg = ManagedConfig.fromEnv({
      AGENT_RUNTIME: JSON.stringify({ outputSchema: schema, toolPolicy: policy, resilience }),
    });
    expect(cfg.outputSchema).toEqual(schema);
    expect(cfg.toolPolicy).toEqual(policy);
    expect(cfg.resilience).toEqual(resilience);
  });

  it("leaves outputSchema/toolPolicy/resilience null with no AGENT_RUNTIME", () => {
    const cfg = ManagedConfig.fromEnv({});
    expect(cfg.outputSchema).toBeNull();
    expect(cfg.toolPolicy).toBeNull();
    expect(cfg.resilience).toBeNull();
  });
});

// ── knowledge auto-inject (ADR 0061 governance #5, M10) ──────────────────────────
//
// A KB whose binding set autoInject prepends an ephemeral <retrieved_context> block (with
// citations) to the system prompt each turn, RAG-style; a KB WITHOUT the flag stays TOOL-ONLY
// (byte-for-byte unchanged). Parity with the Python tests in test_managed.py. Retrieval is
// best-effort (swallowed) and NEVER persisted to session history.

describe("runManagedLoop — knowledge auto-inject", () => {
  it("prepends an ephemeral cited <retrieved_context> that is NOT persisted to history", async () => {
    const fx = await makeFixture({
      withMemory: true,
      plainContent: "the answer is 42",
      knowledgeEnabled: true,
    });
    try {
      // Script the KB retrieval — no live launcher /knowledge/search needed.
      fx.client.knowledge.search = async () =>
        [{ content: "Company PTO is 25 days.", documentRef: "hr.md", chunkIndex: 2 }] as never;

      const config = new ManagedConfig({
        systemPrompt: "sys",
        modelRoute: "m",
        knowledgeAutoInject: ["hr-kb"],
      });
      const headers = { "x-conversation-id": "chat-k" };
      await runManagedLoop(fx.client, config, "how much PTO?", { headers });

      // The gateway's turn-1 SYSTEM message carries the injected, cited block.
      const sent = fx.gateway.requests[0]!["messages"] as Array<Record<string, unknown>>;
      const systemMsg = sent[0]!;
      expect(systemMsg["role"]).toBe("system");
      expect(String(systemMsg["content"])).toContain("<retrieved_context>");
      expect(String(systemMsg["content"])).toContain("Company PTO is 25 days. [source: hr.md#2]");

      // The STORED history is the clean user↔assistant exchange — NO <retrieved_context>.
      const stored = fx.memory!.store.get("chat-k") as Array<Record<string, unknown>>;
      expect(stored).toEqual([
        { role: "user", content: "how much PTO?" },
        { role: "assistant", content: "the answer is 42" },
      ]);
      expect(stored.every((m) => !String(m["content"]).includes("<retrieved_context>"))).toBe(true);
    } finally {
      await fx.stop();
    }
  });

  it("is best-effort: a retrieval failure is swallowed and the turn proceeds", async () => {
    const fx = await makeFixture({ plainContent: "the answer is 42", knowledgeEnabled: true });
    try {
      fx.client.knowledge.search = async () => {
        throw new Error("boom — retrieval hiccup");
      };
      const config = new ManagedConfig({
        systemPrompt: "sys",
        modelRoute: "m",
        knowledgeAutoInject: ["hr-kb"],
      });
      const result = await runManagedLoop(fx.client, config, "how much PTO?");
      // The turn still produced its answer, and the system prompt was left untouched.
      expect(result.output).toBe("the answer is 42");
      const sent = fx.gateway.requests[0]!["messages"] as Array<Record<string, unknown>>;
      expect(sent[0]!["content"]).toBe("sys");
    } finally {
      await fx.stop();
    }
  });

  it("no auto-inject KB is byte-for-byte unchanged (knowledge.search never called)", async () => {
    const fx = await makeFixture({ plainContent: "the answer is 42", knowledgeEnabled: true });
    try {
      let searched = false;
      fx.client.knowledge.search = async () => {
        searched = true;
        throw new Error("knowledge.search must NOT be called without auto-inject");
      };
      const config = new ManagedConfig({ systemPrompt: "sys", modelRoute: "m" }); // no knowledgeAutoInject
      await runManagedLoop(fx.client, config, "hi");
      expect(searched).toBe(false);
      const sent = fx.gateway.requests[0]!["messages"] as Array<Record<string, unknown>>;
      expect(sent.map((m) => m["role"])).toEqual(["system", "user"]);
      expect(sent[0]!["content"]).toBe("sys");
    } finally {
      await fx.stop();
    }
  });
});

describe("ManagedConfig.fromEnv — knowledge auto-inject roster + knobs", () => {
  const saved: Record<string, string | undefined> = {};
  const KEYS = ["KNOWLEDGE_BASES", "KNOWLEDGE_TOP_K", "KNOWLEDGE_THRESHOLD"];
  beforeEach(() => {
    for (const k of KEYS) saved[k] = process.env[k];
  });
  afterEach(() => {
    for (const k of KEYS) {
      if (saved[k] === undefined) delete process.env[k];
      else process.env[k] = saved[k];
    }
  });

  it("derives knowledgeAutoInject from the roster (only autoInject entries) + reads knobs", () => {
    process.env.KNOWLEDGE_BASES = JSON.stringify([
      { name: "auto-kb", namespace: "ns", embeddingRoute: "e", autoInject: true },
      { name: "tool-kb", namespace: "ns", embeddingRoute: "e" },
    ]);
    process.env.KNOWLEDGE_TOP_K = "8";
    process.env.KNOWLEDGE_THRESHOLD = "0.4";
    const cfg = ManagedConfig.fromEnv(process.env);
    expect(cfg.knowledgeAutoInject).toEqual(["auto-kb"]);
    expect(cfg.knowledgeTopK).toBe(8);
    expect(cfg.knowledgeThreshold).toBe(0.4);
  });

  it("no roster ⇒ empty auto-inject list + default knobs (knowledge-free config unchanged)", () => {
    delete process.env.KNOWLEDGE_BASES;
    delete process.env.KNOWLEDGE_TOP_K;
    delete process.env.KNOWLEDGE_THRESHOLD;
    const cfg = ManagedConfig.fromEnv(process.env);
    expect(cfg.knowledgeAutoInject).toEqual([]);
    expect(cfg.knowledgeTopK).toBe(5);
    expect(cfg.knowledgeThreshold).toBe(0.5);
  });
});

describe("mintConversationId", () => {
  it("is unique and run-prefixed", () => {
    const a = mintConversationId();
    const b = mintConversationId();
    expect(a).toMatch(/^run-/);
    expect(a).not.toBe(b);
  });
});
