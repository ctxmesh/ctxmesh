/**
 * ts-custom-agent example test — parity with the Python `sdk-custom-agent` e2e intent.
 *
 * Drives the example's `handle`/`runLoop` against the mock launcher plane (test/plane.ts), and
 * drives its `serve`-wired HTTP contract over an ephemeral port, asserting:
 *   - POST /invoke returns the `{agent, output, ...}` envelope with the model answer;
 *   - the explicit trace tree is emitted (AGENT → step(CHAIN) → tool(TOOL) → llm(LLM)) and nested;
 *   - the word-count TOOL span reflects the mock MCP tool result.
 *
 * This is the TS analogue of the M10 invariant check: a hand-rolled, no-framework loop produces the
 * same OpenInference span tree a framework agent would, rooted under the launcher's `agent.invoke`.
 */

import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { Client } from "../src/client.js";
import * as semconv from "../src/_semconv.js";
import { makeRequestHandler } from "../src/serve.js";
import { handle, runLoop } from "../examples/ts-custom-agent/agent.js";
import { startPlane, type MockPlane } from "./plane.js";

let plane: MockPlane;

beforeEach(async () => {
  // The mock gateway answers /chat/completions with a canned completion; the discovery stub
  // advertises a `word-count` catalog tool whose MCP name is `word_count` and returns {count:3}.
  plane = await startPlane({ gateway: { content: "It is a concise answer." } });
});

afterEach(async () => {
  await plane.stop();
});

describe("ts-custom-agent — the no-framework loop", () => {
  it("produces an answer and the mock tool's word count", async () => {
    const client = new Client(plane.config, { spanProcessor: plane.spans.processor });
    const req = {
      input: "count these words please",
      headers: {},
      approvals: [],
      client,
      emitToken: () => undefined,
    };
    const result = await runLoop(client, req.input, req);

    expect(result.output).toBe("It is a concise answer.");
    // The mock DiscoveryStub's word_count tool returns {count: 3}.
    expect(result.wordCount).toBe(3);
    // Two model turns (plan + answer) + one MCP tool call hit the plane.
    expect(plane.gateway.requests).toHaveLength(2);
    expect(plane.discovery.mcpCalls).toHaveLength(1);
  });

  it("emits the AGENT → step(CHAIN) → tool(TOOL) → llm(LLM) trace tree, nested", async () => {
    const client = new Client(plane.config, { spanProcessor: plane.spans.processor });
    await runLoop(client, "hello world", {
      input: "hello world",
      headers: {},
      approvals: [],
      client,
      emitToken: () => undefined,
    });

    const spans = plane.spans.finishedSpans();
    const byName = (name: string) => spans.find((s) => s.name === name);

    const agent = byName("ts-custom-agent");
    const plan = byName("plan");
    const tool = byName(WORD_COUNT_SPAN);
    const answer = byName("answer");
    expect(agent).toBeDefined();
    expect(plan).toBeDefined();
    expect(tool).toBeDefined();
    expect(answer).toBeDefined();

    // Kinds match the OpenInference vocabulary.
    expect(agent!.attributes[semconv.SPAN_KIND]).toBe(semconv.KIND_AGENT);
    expect(plan!.attributes[semconv.SPAN_KIND]).toBe(semconv.KIND_CHAIN);
    expect(tool!.attributes[semconv.SPAN_KIND]).toBe(semconv.KIND_TOOL);
    expect(tool!.attributes[semconv.TOOL_NAME]).toBe(WORD_COUNT_SPAN);

    // Nesting: plan/tool/answer are children of the AGENT span (same trace, parent = agent span id).
    const agentSpanId = agent!.spanContext().spanId;
    expect(plan!.parentSpanContext?.spanId).toBe(agentSpanId);
    expect(tool!.parentSpanContext?.spanId).toBe(agentSpanId);
    expect(answer!.parentSpanContext?.spanId).toBe(agentSpanId);

    // At least one LLM span nests under a step (the plan chat).
    const llm = spans.find((s) => s.attributes[semconv.SPAN_KIND] === semconv.KIND_LLM);
    expect(llm).toBeDefined();
    expect(llm!.parentSpanContext?.spanId).toBe(plan!.spanContext().spanId);
  });
});

describe("ts-custom-agent — the served /invoke contract", () => {
  let server: Server;
  let baseUrl: string;

  beforeEach(async () => {
    const client = new Client(plane.config, { spanProcessor: plane.spans.processor });
    server = createServer(makeRequestHandler(client, handle, "ts-custom-agent"));
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  });

  afterEach(async () => {
    await new Promise<void>((resolve, reject) =>
      server.close((err) => (err ? reject(err) : resolve())),
    );
  });

  it("answers POST /invoke with the serve envelope", async () => {
    const resp = await fetch(`${baseUrl}/invoke`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ input: "hi there" }),
    });
    expect(resp.status).toBe(200);
    const body = (await resp.json()) as Record<string, unknown>;
    expect(body.agent).toBe("ts-custom-agent");
    expect(body.output).toBe("It is a concise answer.");
  });

  it("serves GET /healthz + /readyz", async () => {
    for (const path of ["/healthz", "/readyz"]) {
      const resp = await fetch(`${baseUrl}${path}`);
      expect(resp.status).toBe(200);
      expect((await resp.json()) as Record<string, unknown>).toEqual({ status: "ok" });
    }
  });
});

/** The tool span name (the catalog name the sample uses). */
const WORD_COUNT_SPAN = "word-count";
